package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/jackc/pgx/v5"
)

// ExecutionFailure hanya berisi pesan publik tetap dan diagnostic terbatas. Error driver dan body SQL
// tidak boleh disimpan di sini.
type ExecutionFailure struct {
	Code, Message, SQLState string
	StatementIndex          int
}

type attemptOutcome struct {
	state, evidence string
	failure         *ExecutionFailure
}

func notStarted(code, message string) attemptOutcome {
	return attemptOutcome{"failed", "not_started", &ExecutionFailure{Code: code, Message: message}}
}

func notStartedExecutionError(ctx context.Context, code, message string) attemptOutcome {
	if ctx.Err() != nil {
		f := executionCanceled(ctx)
		return notStarted(f.Code, f.Message)
	}
	return notStarted(code, message)
}

func uncertain(code, message string) attemptOutcome {
	return attemptOutcome{state: "unknown", failure: &ExecutionFailure{Code: code, Message: message}}
}

type executionSnapshot struct {
	sql            string
	configRevision int64
}

func (s *AttemptStore) executionSnapshot(ctx context.Context, r AttemptReceipt) (executionSnapshot, error) {
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return executionSnapshot{}, attemptFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	var snapshot executionSnapshot
	var hash []byte
	err = lease.QueryRow(ctx, `SELECT a.sql_snapshot,a.sql_sha256,a.target_config_revision
	    FROM databrain.migration_attempts a JOIN databrain.migration_streams s ON s.target_id=a.target_id AND s.active_attempt_id=a.id
	    WHERE a.target_id=$1 AND a.id=$2 AND a.state='running'`, r.TargetID, r.ID).Scan(&snapshot.sql, &hash, &snapshot.configRevision)
	actual := sha256.Sum256([]byte(snapshot.sql))
	if err != nil || !bytes.Equal(hash, actual[:]) {
		return executionSnapshot{}, attemptFailure(problem.PlatformUnavailable)
	}
	return snapshot, nil
}

func (s *AttemptStore) markCommit(ctx context.Context, r AttemptReceipt) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return attemptFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tag, err := lease.Exec(ctx, `UPDATE databrain.migration_attempts a SET commit_started_at=clock_timestamp()
	    WHERE target_id=$1 AND id=$2 AND state='running' AND commit_started_at IS NULL
	    AND EXISTS (SELECT 1 FROM databrain.migration_streams s WHERE s.target_id=a.target_id AND s.active_attempt_id=a.id)`, r.TargetID, r.ID)
	if err != nil || tag.RowsAffected() != 1 {
		return attemptFailure(problem.PlatformUnavailable)
	}
	return nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// persistOutcome mengunci stream sebelum attempt, sama seperti reservasi. Fungsi ini tidak
// menyelesaikan unknown atau mengubah hasil terminal yang sudah ada.
func (s *AttemptStore) persistOutcome(ctx context.Context, r AttemptReceipt, outcome attemptOutcome) (AttemptReceipt, error) {
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return r, attemptFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return r, attemptFailure(problem.PlatformUnavailable)
	}
	defer rollbackAttempt(tx)
	var active bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(active_attempt_id=$2,false) FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, r.TargetID, r.ID).Scan(&active)
	if err != nil {
		return r, attemptFailure(problem.PlatformUnavailable)
	}
	stored, err := scanReceipt(tx.QueryRow(ctx, `SELECT `+receiptColumns+` FROM databrain.migration_attempts WHERE target_id=$1 AND id=$2 FOR UPDATE`, r.TargetID, r.ID))
	if err != nil || stored == nil {
		return r, attemptFailure(problem.PlatformUnavailable)
	}
	if stored.State == "succeeded" || stored.State == "failed" || stored.State == "unknown" {
		return *stored, nil // Juga membuktikan hasil setelah acknowledgement hilang.
	}
	if !active {
		return r, attemptFailure(problem.PlatformUnavailable)
	}
	var code, message, sqlstate, index any
	if f := outcome.failure; f != nil {
		code, message, sqlstate = f.Code, f.Message, nullableText(f.SQLState)
		if f.StatementIndex > 0 {
			index = f.StatementIndex
		}
	}
	_, err = tx.Exec(ctx, `UPDATE databrain.migration_attempts SET state=$3,
	    finished_at=CASE WHEN $3 IN ('succeeded','failed') THEN clock_timestamp() ELSE NULL END,
	    outcome_evidence=$4,failure_code=$5,failure_message=$6,failure_sqlstate=$7,failure_statement_index=$8
	    WHERE target_id=$1 AND id=$2`, r.TargetID, r.ID, outcome.state, nullableText(outcome.evidence), code, message, sqlstate, index)
	if err != nil {
		return r, attemptFailure(problem.PlatformUnavailable)
	}
	if outcome.state != "unknown" {
		_, err = tx.Exec(ctx, `UPDATE databrain.migration_streams SET active_attempt_id=NULL WHERE target_id=$1 AND active_attempt_id=$2`, r.TargetID, r.ID)
		if err != nil {
			return r, attemptFailure(problem.PlatformUnavailable)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		// Jangan memakai ulang koneksi dengan COMMIT yang belum pasti. Pembacaan atau penyimpanan berikutnya
		// dapat menemukan hasil lewat koneksi baru.
		_ = lease.Hijack().Close(ctx)
		return r, attemptFailure(problem.PlatformUnavailable)
	}
	r.State = outcome.state
	return r, nil
}

// Cleanup tetap terbatas pada rollback, penutupan koneksi, dan persistence setelah caller batal.
func cleanupExecutionContext(end time.Time, maximum time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(maximum)
	if end.Before(deadline) {
		deadline = end
	}
	return context.WithDeadline(context.Background(), deadline)
}

func (s *AttemptStore) finishExecution(r AttemptReceipt, outcome attemptOutcome, end time.Time) (AttemptReceipt, error) {
	ctx, cancel := cleanupExecutionContext(end, 2*time.Second)
	defer cancel()
	stored, err := s.persistOutcome(ctx, r, outcome)
	if err == nil {
		return stored, nil
	}
	// Acknowledgement COMMIT target belum cukup jika hasilnya belum tersimpan permanen. Stream tetap
	// diblokir sampai hasil terminal terlihat; SQL dan mutasi terminal tidak diulang di sini.
	stored, err = s.persistOutcome(ctx, r, uncertain("OUTCOME_PERSISTENCE_FAILED", "The execution outcome could not be recorded."))
	if err == nil {
		return stored, nil
	}
	if receipt, readErr := s.Receipt(ctx, r.TargetID, r.ActorID, r.RequestKey); readErr == nil && receipt != nil {
		return *receipt, attemptFailure(problem.PlatformUnavailable)
	}
	return r, attemptFailure(problem.PlatformUnavailable)
}

func rollbackExecution(tx pgx.Tx, failure *ExecutionFailure, end time.Time) attemptOutcome {
	ctx, cancel := cleanupExecutionContext(end, 3*time.Second)
	defer cancel()
	if err := tx.Rollback(ctx); err == nil {
		return attemptOutcome{"failed", "rollback_ack", failure}
	}
	return uncertain("COMMIT_UNCERTAIN", "The transaction outcome could not be confirmed.")
}

func executionCanceled(ctx context.Context) *ExecutionFailure {
	if errors.Is(ctx.Err(), context.Canceled) {
		return &ExecutionFailure{Code: "CANCELED", Message: "Execution was canceled."}
	}
	return &ExecutionFailure{Code: "SQL_TIMEOUT", Message: "Execution exceeded its time limit."}
}

func (runner *Runner) runTarget(ctx context.Context, p access.Principal, r AttemptReceipt, end time.Time) attemptOutcome {
	snapshot, err := runner.store.executionSnapshot(ctx, r)
	if err != nil {
		return notStartedExecutionError(ctx, "OUTCOME_PERSISTENCE_FAILED", "The execution snapshot could not be read.")
	}
	statements, err := ValidateSQL(snapshot.sql)
	if err != nil {
		return notStarted("OUTCOME_PERSISTENCE_FAILED", "The execution snapshot could not be validated.")
	}
	conn, err := runner.provider.OpenMigration(ctx, r.TargetID, snapshot.configRevision, r.ID)
	if err != nil {
		return notStartedExecutionError(ctx, "CONNECTION_FAILED", "The target connection could not be opened.")
	}
	defer func() {
		cleanup, cancel := cleanupExecutionContext(end, 3*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}()
	// Periksa otorisasi tepat sebelum BEGIN setelah menunggu koneksi. Pencabutan yang sudah commit sebelum
	// pemeriksaan ini mencegah SQL berjalan.
	if _, err := runner.authorizer.RequireDatabase(ctx, p, r.TargetID, access.MigrationExecute); err != nil {
		var denied *problem.Error
		if errors.As(err, &denied) && denied.Code == problem.PlatformUnavailable {
			return notStartedExecutionError(ctx, "OUTCOME_PERSISTENCE_FAILED", "Access could not be checked before execution.")
		}
		return notStarted("ACCESS_REVOKED", "Access was revoked before execution.")
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return notStartedExecutionError(ctx, "CONNECTION_FAILED", "The target transaction could not be started.")
	}
	// Statement kontrol backend tetap dipisahkan dari SQL pengguna.
	_, err = tx.Exec(ctx, `SET LOCAL statement_timeout='30s'; SET LOCAL lock_timeout='3s'; SET LOCAL search_path=public`, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return rollbackExecution(tx, statementFailure(ctx, err, 0), end)
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.SQL, pgx.QueryExecModeExec); err != nil {
			return rollbackExecution(tx, statementFailure(ctx, err, statement.Index), end)
		}
	}
	deadline, _ := ctx.Deadline()
	if ctx.Err() != nil || time.Until(deadline) < 2*time.Second {
		return rollbackExecution(tx, executionCanceled(ctx), end)
	}
	if err := runner.store.markCommit(ctx, r); err != nil {
		return rollbackExecution(tx, &ExecutionFailure{Code: "OUTCOME_PERSISTENCE_FAILED", Message: "The commit phase could not be recorded."}, end)
	}
	if ctx.Err() != nil || time.Until(deadline) < time.Second {
		return rollbackExecution(tx, executionCanceled(ctx), end)
	}
	// Jangan mengulang COMMIT meski acknowledgement hilang. pgx.Tx yang tertutup tidak membuktikan
	// rollback; recovery memerlukan bukti operator.
	err = tx.Commit(ctx)
	return commitOutcome(conn, err)
}
