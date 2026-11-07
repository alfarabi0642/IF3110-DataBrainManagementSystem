package migration

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Runner membatasi dua eksekusi lokal sebelum reservasi; AttemptStore menjaga urutan permanen.
type Runner struct {
	store      *AttemptStore
	provider   *targets.Provider
	authorizer access.Authorizer
	slots      chan struct{}
}

func NewRunner(store *AttemptStore, provider *targets.Provider, authorizer access.Authorizer) (*Runner, error) {
	if store == nil || store.pool == nil || provider == nil || authorizer == nil {
		return nil, attemptFailure(problem.ValidationFailed)
	}
	return &Runner{store: store, provider: provider, authorizer: authorizer, slots: make(chan struct{}, 2)}, nil
}

func (runner *Runner) replay(ctx context.Context, target, draft, actor uuid.UUID, input ReserveInput) (*AttemptReceipt, error) {
	r, err := runner.store.Receipt(ctx, target, actor, input.RequestKey)
	if err != nil || r == nil {
		return nil, err
	}
	matched, err := matchReservation(*r, draft, input.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	return &matched.Receipt, nil
}

// Execute menerima principal tervalidasi, tanpa actor dari form. State receipt menunjukkan hasil
// termasuk failed/unknown. Jika error terjadi setelah reservasi, receipt tetap dapat dipakai untuk
// memeriksa attempt.
func (runner *Runner) Execute(ctx context.Context, p access.Principal, targetID, draftID uuid.UUID, input ReserveInput) (AttemptReceipt, error) {
	if targetID == uuid.Nil || draftID == uuid.Nil || input.RequestKey == uuid.Nil || input.ExpectedRevision < 1 {
		return AttemptReceipt{}, attemptFailure(problem.InvalidRequest)
	}
	end := time.Now().Add(65 * time.Second)
	// Sisihkan lima detik untuk rollback, penutupan, dan penyimpanan hasil dalam batas total. Pembatalan
	// parent tetap langsung menghentikan pekerjaan aktif.
	ctx, cancel := context.WithDeadline(ctx, end.Add(-5*time.Second))
	defer cancel()
	if _, err := runner.authorizer.RequireDatabase(ctx, p, targetID, access.MigrationExecute); err != nil {
		return AttemptReceipt{}, err
	}
	if receipt, err := runner.replay(ctx, targetID, draftID, p.UserID, input); err != nil || receipt != nil {
		if receipt != nil {
			return *receipt, nil
		}
		return AttemptReceipt{}, err
	}
	select {
	case runner.slots <- struct{}{}:
		defer func() { <-runner.slots }()
	default:
		if receipt, err := runner.replay(ctx, targetID, draftID, p.UserID, input); err != nil || receipt != nil {
			if receipt != nil {
				return *receipt, nil
			}
			return AttemptReceipt{}, err
		}
		return AttemptReceipt{}, attemptFailure(problem.ExecutionCapacity)
	}
	reservation, err := runner.store.Reserve(ctx, targetID, draftID, p.UserID, input)
	if err != nil {
		return AttemptReceipt{}, err
	}
	r := reservation.Receipt
	if !reservation.Created() {
		return r, nil
	}
	claimed, err := runner.store.Claim(ctx, reservation)
	if err != nil {
		return runner.store.finishExecution(r, notStartedExecutionError(ctx, "OUTCOME_PERSISTENCE_FAILED", "Execution could not be started."), end)
	}
	if !claimed {
		if stored, err := runner.store.Receipt(ctx, targetID, p.UserID, input.RequestKey); err == nil && stored != nil {
			return *stored, nil
		}
		return r, attemptFailure(problem.PlatformUnavailable)
	}
	r.State = "running"
	outcome := runner.runTarget(ctx, p, r, end)
	return runner.store.finishExecution(r, outcome, end)
}

func safeSQLState(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) != 5 {
		return ""
	}
	for _, c := range pgErr.Code {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
			return ""
		}
	}
	return pgErr.Code
}

func statementFailure(ctx context.Context, err error, index int) *ExecutionFailure {
	f := &ExecutionFailure{Code: "SQL_ERROR", Message: "A migration statement failed.", SQLState: safeSQLState(err), StatementIndex: index}
	if ctx.Err() != nil {
		canceled := executionCanceled(ctx)
		f.Code, f.Message = canceled.Code, canceled.Message
	} else if f.SQLState == "55P03" {
		f.Code, f.Message = "LOCK_TIMEOUT", "A migration statement exceeded its lock wait limit."
	} else if f.SQLState == "57014" {
		f.Code, f.Message = "SQL_TIMEOUT", "A migration statement exceeded its time limit."
	}
	return f
}

func commitOutcome(conn *pgx.Conn, err error) attemptOutcome {
	if err == nil {
		return attemptOutcome{state: "succeeded", evidence: "commit_ack"}
	}
	code := safeSQLState(err)
	var pgErr *pgconn.PgError
	// Hanya rollback definitif yang diakui dapat disebut gagal. Error koneksi,
	// statement_completion_unknown (40003), serta error fatal/internal tetap unknown.
	definitive := errors.Is(err, pgx.ErrTxCommitRollback) ||
		errors.As(err, &pgErr) && pgErr.Severity == "ERROR" && conn != nil && conn.PgConn().TxStatus() == 'I' &&
			(strings.HasPrefix(code, "23") || code == "40001" || code == "40P01" || code == "P0001")
	if definitive {
		return attemptOutcome{"failed", "server_rollback", &ExecutionFailure{Code: "SQL_ERROR", Message: "The target rejected the transaction commit.", SQLState: code}}
	}
	return uncertain("COMMIT_UNCERTAIN", "The transaction commit could not be confirmed.")
}
