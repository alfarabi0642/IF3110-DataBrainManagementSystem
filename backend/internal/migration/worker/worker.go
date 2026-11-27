package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/head"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/journal"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/legacy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	workerLockSnooze      = 5 * time.Second
	workerCleanupTimeout  = 5 * time.Second
	workerPlatformTimeout = 10 * time.Second
)

type targetOpener interface {
	OpenMigration(ctx context.Context, targetID uuid.UUID, configRevision int64, attemptID uuid.UUID) (*pgx.Conn, error)
}

// WorkerExecutor menjalankan eksekusi migration normal secara asinkron di proses worker dengan
// lock kepemilikan fisik target, verifikasi target journal fail-closed, revalidasi kebijakan, bukti
// commit atomik, dan finalisasi history idempoten.
type WorkerExecutor struct {
	pool      *pgxpool.Pool
	provider  targetOpener
	appSecret []byte
	guard     migration.WorkerGuard
	finalizer head.FinalizeFunc
	logger    *slog.Logger

	// Hook pengujian deterministik (nil pada produksi).
	beforeCommit          func() error
	AfterTargetCommit     func() error
	afterPlatformFinalize func() error
	BeforeBatch           func(ordinal int) error
}

// NewWorkerExecutor membangun executor worker produksi dengan guard dan finalizer domain nyata.
func NewWorkerExecutor(pool *pgxpool.Pool, provider *targets.Provider, appSecret []byte, logger *slog.Logger) (*WorkerExecutor, error) {
	if pool == nil || provider == nil || len(appSecret) != 32 {
		return nil, errors.New("worker executor requires pool, target provider and 32-byte app secret")
	}
	if logger == nil {
		logger = slog.Default()
	}
	secretCopy := make([]byte, len(appSecret))
	copy(secretCopy, appSecret)
	return &WorkerExecutor{
		pool:      pool,
		provider:  provider,
		appSecret: secretCopy,
		guard: func(ctx context.Context, op execstore.ApprovedOperation) error {
			return migration.RevalidateOperation(ctx, pool, op)
		},
		finalizer: head.FinalizeCommitted,
		logger:    logger,
	}, nil
}

type storedExecution struct {
	op          execstore.ApprovedOperation
	phase       string
	outcome     string
	reconciling bool
}

// HandleJob adalah adaptor jobs.ExecutionHandler untuk River ExecutionWorker.
func (w *WorkerExecutor) HandleJob(ctx context.Context, args jobs.ExecutionJobArgs, jobID int64) error {
	return w.Execute(ctx, args.ExecutionID, args.RequestID, jobID)
}

// Execute menjalankan atau merekonsiliasi satu eksekusi normal atau batch. Duplicate delivery aman
// karena status finished langsung selesai dan bukti target journal mencegah eksekusi SQL ulang.
func (w *WorkerExecutor) Execute(ctx context.Context, executionID, requestID uuid.UUID, jobID int64) (retErr error) {
	if executionID == uuid.Nil || requestID == uuid.Nil || jobID < 1 {
		return jobs.PermanentError{Err: errors.New("invalid execution job reference")}
	}
	started := time.Now()
	riverAttempt := jobs.Attempt(ctx)
	ctx, span := telemetry.Tracer().Start(ctx, "migration.execute")
	span.SetAttributes(
		attribute.String(telemetry.FieldExecutionID, executionID.String()),
		attribute.String(telemetry.FieldRequestID, requestID.String()),
		attribute.Int64(telemetry.FieldJobID, jobID),
		attribute.Int(telemetry.FieldAttempt, riverAttempt),
	)
	ctx = telemetry.WithLogAttrs(ctx,
		telemetry.FieldExecutionID, executionID.String(),
		telemetry.FieldRequestID, requestID.String(),
		telemetry.FieldJobID, jobID,
		telemetry.FieldAttempt, riverAttempt,
	)
	finalStatus := ""
	defer func() {
		status := finalStatus
		if status == "" {
			status = "succeeded"
			if retErr != nil {
				var snooze jobs.SnoozeError
				if errors.As(retErr, &snooze) {
					status = "snoozed"
				} else {
					status = "error"
					span.SetStatus(codes.Error, telemetry.SanitizeError(retErr).Code)
				}
			}
		}
		dur := time.Since(started).Milliseconds()
		span.SetAttributes(
			attribute.String(telemetry.FieldStatus, status),
			attribute.Int64(telemetry.FieldDuration, dur),
		)
		attrs := append([]any{
			telemetry.FieldExecutionID, executionID.String(),
			telemetry.FieldRequestID, requestID.String(),
			telemetry.FieldJobID, jobID,
			telemetry.FieldAttempt, riverAttempt,
			telemetry.FieldStatus, status,
			telemetry.FieldDuration, dur,
		}, telemetry.TraceAttrs(ctx)...)
		w.logger.Info("migration execution finished", attrs...)
		span.End()
	}()

	execRec, err := w.loadExecution(ctx, executionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return jobs.PermanentError{Err: errors.New("execution record not found")}
		}
		return err
	}
	op := execRec.op
	ctx = telemetry.WithLogAttrs(ctx,
		telemetry.FieldDatabase, op.TargetID.String(),
		telemetry.FieldActor, op.ActorID.String(),
		"migration_id", op.DefinitionID.String(),
	)
	span.SetAttributes(
		attribute.String(telemetry.FieldDatabase, op.TargetID.String()),
		attribute.String(telemetry.FieldActor, op.ActorID.String()),
		attribute.String("migration_id", op.DefinitionID.String()),
		attribute.String("direction", op.Direction),
		attribute.String("mode", op.Mode),
	)
	startAttrs := append([]any{
		telemetry.FieldExecutionID, executionID.String(),
		telemetry.FieldRequestID, requestID.String(),
		telemetry.FieldDatabase, op.TargetID.String(),
		telemetry.FieldActor, op.ActorID.String(),
		"migration_id", op.DefinitionID.String(),
		telemetry.FieldJobID, jobID,
		telemetry.FieldAttempt, riverAttempt,
		"direction", op.Direction,
		"mode", op.Mode,
	}, telemetry.TraceAttrs(ctx)...)
	w.logger.Info("migration execution started", startAttrs...)

	if op.RequestID != requestID {
		return jobs.PermanentError{Err: errors.New("execution request ID mismatch")}
	}
	if execstore.PlanHash(op) != op.PlanHash {
		_ = w.markBlockedUnknown(executionID, 0, "PLAN_HASH_MISMATCH", "Stored execution plan failed integrity verification.", "")
		return jobs.PermanentError{Err: errors.New("stored execution plan hash mismatch")}
	}
	if execRec.phase == "finished" || execRec.phase == "blocked" {
		if execRec.outcome != "" {
			finalStatus = execRec.outcome
		} else {
			finalStatus = execRec.phase
		}
		return nil
	}

	// Jika percobaan River terakhir mengembalikan error retryable (bukan snooze/permanent),
	// selaraskan status eksekusi dan gate agar tidak tertinggal menggantung.
	defer func() {
		if retErr == nil || !jobs.IsFinalAttempt(ctx) {
			return
		}
		var snooze jobs.SnoozeError
		var perm jobs.PermanentError
		if errors.As(retErr, &snooze) || errors.As(retErr, &perm) {
			return
		}
		latest, loadErr := w.loadExecution(context.WithoutCancel(ctx), executionID)
		if loadErr != nil || latest.phase == "finished" || latest.phase == "blocked" {
			return
		}
		if latest.phase == "queued" {
			_ = w.markFailedAndRelease(executionID, 0, "RETRIES_EXHAUSTED",
				"Execution failed after exhausting automatic retries before starting.", "")
		} else {
			_ = w.markBlockedUnknown(executionID, 0, "RETRIES_EXHAUSTED",
				"Execution exhausted automatic retries after starting; reconcile before retrying.", "")
		}
	}()

	conn, err := w.provider.OpenMigration(ctx, op.TargetID, op.TargetConfigRevision, executionID)
	if err != nil {
		var pe *problem.Error
		if errors.As(err, &pe) && (pe.Code == problem.RevisionConflict || pe.Code == problem.DatabaseNotFound) {
			if finErr := w.markFailedAndRelease(executionID, 0, string(pe.Code), "Target configuration changed or target is inactive before execution.", ""); finErr != nil {
				return finErr
			}
			return jobs.PermanentError{Err: pe}
		}
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	lockCtx, lockSpan := telemetry.Tracer().Start(ctx, "migration.lock")
	lockSpan.SetAttributes(
		attribute.String(telemetry.FieldDatabase, op.TargetID.String()),
		attribute.String(telemetry.FieldExecutionID, executionID.String()),
	)
	lock, acquired, err := journal.TryTargetLock(lockCtx, conn)
	if err != nil {
		lockSpan.SetAttributes(attribute.Bool("lock.acquired", false), attribute.String(telemetry.FieldStatus, "error"))
		lockSpan.SetStatus(codes.Error, telemetry.SanitizeError(err).Code)
		lockSpan.End()
		return err
	}
	if !acquired {
		lockSpan.SetAttributes(attribute.Bool("lock.acquired", false), attribute.String(telemetry.FieldStatus, "contended"))
		lockSpan.End()
		return jobs.SnoozeError{Duration: workerLockSnooze, Reason: "target ownership lock held by another worker"}
	}
	lockSpan.SetAttributes(attribute.Bool("lock.acquired", true), attribute.String(telemetry.FieldStatus, "acquired"))
	lockSpan.End()
	defer func() {
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
		defer cancel()
		_ = lock.Release(relCtx)
	}()

	// Fail-closed: verifikasi proteksi schema/role databrain_runtime sebelum inspeksi atau SQL apa pun.
	if err := journal.VerifyTargetJournal(ctx, conn); err != nil {
		code := "TARGET_JOURNAL_UNSAFE"
		msg := "Target database journal failed privilege or integrity verification."
		if errors.Is(err, journal.ErrJournalMissing) {
			code = "TARGET_JOURNAL_MISSING"
			msg = "Target database journal schema databrain_runtime is not provisioned."
		} else if errors.Is(err, journal.ErrJournalVersion) {
			code = "TARGET_JOURNAL_VERSION"
			msg = "Target database journal schema version is not supported."
		}
		if execRec.phase != "queued" {
			if blkErr := w.markBlockedUnknown(executionID, 0, code, msg, ""); blkErr != nil {
				return blkErr
			}
			return jobs.PermanentError{Err: err}
		}
		if finErr := w.markFailedAndRelease(executionID, 0, code, msg, ""); finErr != nil {
			return finErr
		}
		return jobs.PermanentError{Err: err}
	}

	if op.Mode == execstore.ModeBatch {
		return w.executeBatch(ctx, conn, executionID, jobID, op)
	}

	ownerSecret, err := journal.DeriveOwnerSecret(w.appSecret, executionID)
	if err != nil {
		return jobs.PermanentError{Err: err}
	}

	// Cek pemilik unresolved lintas-alias sebelum revalidasi atau SQL.
	owner, err := journal.ReadJournalOwner(ctx, conn)
	if err != nil {
		return err
	}
	if owner != nil && owner.RootExecutionID != executionID {
		if cleared, clearErr := w.clearFinishedForeignOwner(ctx, conn, owner); clearErr != nil {
			return clearErr
		} else if !cleared {
			return jobs.SnoozeError{Duration: workerLockSnooze, Reason: "physical target owned by another unresolved execution"}
		}
	}

	// Cek bukti commit yang mungkin sudah ada dari percobaan sebelum crash/lost-ack.
	commits, err := journal.ReadJournalCommits(ctx, conn, executionID)
	if err != nil {
		return err
	}
	for _, c := range commits {
		if c.BatchOrdinal == 0 && c.Direction == op.Direction {
			if c.PlanHash != op.PlanHash {
				_ = w.markBlockedUnknown(executionID, 0, "JOURNAL_HASH_MISMATCH", "Target journal commit hash conflicts with execution plan.", "")
				return jobs.PermanentError{Err: head.ErrReconciliationConflict}
			}
			return w.finalizeAndClearOwner(ctx, conn, executionID, 0, op, c, ownerSecret)
		}
	}

	// Revalidasi kebijakan approval setelah memegang lock target, tanpa menahan lock platform saat SQL.
	if err := w.guard(ctx, op); err != nil {
		var pe *problem.Error
		if errors.As(err, &pe) && pe.Code == problem.PlatformUnavailable {
			return err
		}
		code := "ELIGIBILITY_DENIED"
		msg := "Migration is no longer eligible to execute."
		if errors.As(err, &pe) {
			code = string(pe.Code)
			if pe.Code == problem.DatabaseAccessDenied {
				msg = "Actor edit access was revoked before execution."
			} else if pe.Code == problem.ReviewRequestStale {
				msg = "Approval policy, target configuration, or head changed before execution."
			}
		}
		if owner != nil && owner.RootExecutionID == executionID {
			clearJournalOwnerBestEffort(conn, executionID, ownerSecret)
		}
		if finErr := w.markFailedAndRelease(executionID, 0, code, msg, ""); finErr != nil {
			return finErr
		}
		return jobs.PermanentError{Err: err}
	}

	sqlText, err := sqlForDirection(op)
	if err != nil {
		if finErr := w.markFailedAndRelease(executionID, 0, "SQL_INVALID", "Execution SQL snapshot is empty or invalid.", ""); finErr != nil {
			return finErr
		}
		return jobs.PermanentError{Err: err}
	}
	statements, err := sqlpolicy.ValidateSQL(sqlText)
	if err != nil {
		if finErr := w.markFailedAndRelease(executionID, 0, string(problem.SQLUnsupported), "Execution SQL snapshot failed policy validation.", ""); finErr != nil {
			return finErr
		}
		return jobs.PermanentError{Err: err}
	}

	attemptNumber, err := w.beginAttempt(ctx, executionID, jobID)
	if err != nil {
		return err
	}

	if err := journal.AcquireJournalOwner(ctx, conn, executionID, executionID, op.PlanHash, ownerSecret); err != nil {
		if errors.Is(err, journal.ErrJournalOwned) {
			_ = w.finishAttempt(executionID, attemptNumber, "failed", "TARGET_BUSY", "Target owned by another unresolved execution.", "")
			return jobs.SnoozeError{Duration: workerLockSnooze, Reason: "target journal owned by another execution"}
		}
		_ = w.finishAttempt(executionID, attemptNumber, "failed", "TARGET_JOURNAL_UNSAFE", "Could not acquire durable target journal ownership.", "")
		return err
	}

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		clearJournalOwnerBestEffort(conn, executionID, ownerSecret)
		_ = w.finishAttempt(executionID, attemptNumber, "failed", "CONNECTION_FAILED", "Could not begin target transaction.", "")
		return err
	}

	txGuard, err := journal.ArmJournalTx(ctx, tx, executionID, executionID, op.PlanHash)
	if err != nil {
		w.rollbackTargetTx(tx)
		clearJournalOwnerBestEffort(conn, executionID, ownerSecret)
		_ = w.finishAttempt(executionID, attemptNumber, "failed", "TARGET_JOURNAL_UNSAFE", "Could not arm target journal transaction guard.", "")
		return err
	}

	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout='300s'; SET LOCAL lock_timeout='3s'; SET LOCAL search_path=public`, pgx.QueryExecModeSimpleProtocol); err != nil {
		return w.handleTargetSQLFailure(ctx, conn, tx, executionID, attemptNumber, err, 0, ownerSecret)
	}

	var totalRows int64
	for _, stmt := range statements {
		tag, execErr := tx.Exec(ctx, stmt.SQL, pgx.QueryExecModeExec)
		if execErr != nil {
			return w.handleTargetSQLFailure(ctx, conn, tx, executionID, attemptNumber, execErr, stmt.Index, ownerSecret)
		}
		totalRows += tag.RowsAffected()
	}

	if err := txGuard.RecordCommit(ctx, tx, 0, op.Direction, totalRows, nil); err != nil {
		return w.handleTargetSQLFailure(ctx, conn, tx, executionID, attemptNumber, err, 0, ownerSecret)
	}

	if w.beforeCommit != nil {
		if hookErr := w.beforeCommit(); hookErr != nil {
			w.rollbackTargetTx(tx)
			clearJournalOwnerBestEffort(conn, executionID, ownerSecret)
			if finErr := w.markFailedAndRelease(executionID, attemptNumber, "BEFORE_COMMIT_FAULT", "Injected fault aborted transaction before commit.", ""); finErr != nil {
				return finErr
			}
			return jobs.PermanentError{Err: hookErr}
		}
	}

	commitErr := tx.Commit(ctx)
	if commitErr != nil {
		return w.handleCommitError(ctx, conn, executionID, attemptNumber, op, commitErr, ownerSecret)
	}

	if w.AfterTargetCommit != nil {
		if hookErr := w.AfterTargetCommit(); hookErr != nil {
			_ = w.markReconciling(executionID, attemptNumber, "FINALIZE_INTERRUPTED", "Target commit succeeded; platform finalization interrupted.")
			return hookErr
		}
	}

	// Baca kembali bukti commit aktual dari target journal untuk finalisasi platform.
	readCtx, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
	postCommits, readErr := journal.ReadJournalCommits(readCtx, conn, executionID)
	cancelRead()
	var committed journal.JournalCommit
	if readErr == nil {
		for _, c := range postCommits {
			if c.BatchOrdinal == 0 && c.Direction == op.Direction {
				committed = c
				break
			}
		}
	}
	if committed.RootExecutionID == uuid.Nil {
		committed = journal.JournalCommit{
			RootExecutionID:   executionID,
			BatchOrdinal:      0,
			Direction:         op.Direction,
			ActionExecutionID: executionID,
			PlanHash:          op.PlanHash,
			RowsAffected:      totalRows,
			CommittedAt:       time.Now().UTC(),
		}
	}

	if err := w.finalizeAndClearOwner(ctx, conn, executionID, attemptNumber, op, committed, ownerSecret); err != nil {
		return err
	}
	if w.afterPlatformFinalize != nil {
		return w.afterPlatformFinalize()
	}
	return nil
}

func sqlForDirection(op execstore.ApprovedOperation) (string, error) {
	switch op.Direction {
	case execstore.DirectionApply, execstore.DirectionForward:
		if op.SQLSnapshot == "" {
			return "", errors.New("missing up SQL snapshot")
		}
		return op.SQLSnapshot, nil
	case execstore.DirectionRollback:
		if op.DownSnapshot == "" {
			return "", errors.New("missing down SQL snapshot")
		}
		return op.DownSnapshot, nil
	default:
		return "", fmt.Errorf("unsupported normal direction %q", op.Direction)
	}
}

func (w *WorkerExecutor) handleTargetSQLFailure(ctx context.Context, conn *pgx.Conn, tx pgx.Tx,
	executionID uuid.UUID, attemptNumber int, execErr error, stmtIndex int, ownerSecret [32]byte) error {
	f := legacy.StatementFailure(ctx, execErr, stmtIndex)
	rbCtx, cancelRB := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
	rbErr := tx.Rollback(rbCtx)
	cancelRB()
	if rbErr != nil {
		// Rollback tidak terkonfirmasi: tahan gate platform dan target unresolved_owner.
		_ = w.markBlockedUnknown(executionID, attemptNumber, "COMMIT_UNCERTAIN", "Rollback could not be confirmed after SQL failure.", f.SQLState)
		return jobs.PermanentError{Err: rbErr}
	}
	clearJournalOwnerBestEffort(conn, executionID, ownerSecret)
	if IsTransientSQLState(f.SQLState) {
		// Kegagalan sementara dengan rollback terkonfirmasi: catat attempt gagal dan kembalikan
		// error retryable agar River menjadwalkan ulang dengan backoff terbatas.
		_ = w.finishAttempt(executionID, attemptNumber, "failed", f.Code, f.Message, f.SQLState)
		return execErr
	}
	if finErr := w.markFailedAndRelease(executionID, attemptNumber, f.Code, f.Message, f.SQLState); finErr != nil {
		return finErr
	}
	return jobs.PermanentError{Err: execErr}
}

func (w *WorkerExecutor) handleCommitError(ctx context.Context, conn *pgx.Conn, executionID uuid.UUID, attemptNumber int,
	op execstore.ApprovedOperation, commitErr error, ownerSecret [32]byte) error {
	// Periksa apakah target journal mencatat commit meskipun COMMIT error/putus.
	checkCtx, cancelCheck := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
	commits, readErr := journal.ReadJournalCommits(checkCtx, conn, executionID)
	cancelCheck()
	if readErr == nil {
		for _, c := range commits {
			if c.BatchOrdinal == 0 && c.Direction == op.Direction {
				if c.PlanHash != op.PlanHash {
					_ = w.markBlockedUnknown(executionID, attemptNumber, "JOURNAL_HASH_MISMATCH", "Target journal commit hash conflicts with execution plan.", "")
					return jobs.PermanentError{Err: head.ErrReconciliationConflict}
				}
				return w.finalizeAndClearOwner(ctx, conn, executionID, attemptNumber, op, c, ownerSecret)
			}
		}
		// Koneksi masih sehat dan journal membuktikan 0 commit: hanya PgError definitif (mis. deferred constraint
		// atau serialization failure) yang membuktikan rollback; error lain tetap unknown.
		var pgErr *pgconn.PgError
		if errors.As(commitErr, &pgErr) && pgErr.Code != "" {
			clearJournalOwnerBestEffort(conn, executionID, ownerSecret)
			if IsTransientSQLState(pgErr.Code) {
				_ = w.finishAttempt(executionID, attemptNumber, "failed", "SQL_TRANSIENT_FAILURE", "Transient target error rolled back on commit.", pgErr.Code)
				return commitErr
			}
			if finErr := w.markFailedAndRelease(executionID, attemptNumber, "SQL_EXECUTION_FAILED", "Target transaction rolled back on commit.", pgErr.Code); finErr != nil {
				return finErr
			}
			return jobs.PermanentError{Err: commitErr}
		}
	}
	_ = w.markBlockedUnknown(executionID, attemptNumber, "COMMIT_UNCERTAIN", "The target transaction outcome could not be confirmed.", "")
	return jobs.PermanentError{Err: commitErr}
}

func (w *WorkerExecutor) finalizeAndClearOwner(ctx context.Context, conn *pgx.Conn, executionID uuid.UUID, attemptNumber int,
	op execstore.ApprovedOperation, committed journal.JournalCommit, ownerSecret [32]byte) error {
	finCtx, cancelFin := context.WithTimeout(context.WithoutCancel(ctx), workerPlatformTimeout)
	defer cancelFin()
	lease, err := platformdb.Acquire(finCtx, w.pool)
	if err != nil {
		_ = w.markReconciling(executionID, attemptNumber, "FINALIZE_PENDING", "Target committed; waiting to finalize platform history.")
		return err
	}
	defer lease.Release()
	tx, err := lease.BeginTx(finCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		_ = w.markReconciling(executionID, attemptNumber, "FINALIZE_PENDING", "Target committed; waiting to finalize platform history.")
		return err
	}
	defer func() { _ = tx.Rollback(finCtx) }()

	if err := tx.QueryRow(finCtx, `SELECT 1 FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, op.TargetID).Scan(new(int)); err != nil {
		return err
	}
	var currentPhase string
	if err := tx.QueryRow(finCtx, `SELECT phase FROM databrain.migration_executions WHERE id=$1 FOR UPDATE`, executionID).Scan(&currentPhase); err != nil {
		return err
	}

	ev := head.CommitEvidence{
		RootExecutionID:   committed.RootExecutionID,
		ActionExecutionID: committed.ActionExecutionID,
		PlanHash:          committed.PlanHash,
		Direction:         committed.Direction,
		Mode:              op.Mode,
		RowsAffected:      committed.RowsAffected,
		CommittedAt:       committed.CommittedAt,
		Evidence:          fmt.Sprintf("target_journal:v%d:batch=%d", journal.TargetJournalVersion, committed.BatchOrdinal),
	}
	if err := w.finalizer(finCtx, tx, executionID, ev); err != nil {
		if errors.Is(err, head.ErrReconciliationConflict) {
			_ = tx.Rollback(finCtx)
			_ = w.markBlockedUnknown(executionID, attemptNumber, "RECONCILIATION_CONFLICT", "Target commit evidence conflicts with platform history.", "")
			return jobs.PermanentError{Err: err}
		}
		return err
	}

	if currentPhase != "finished" {
		if _, err := tx.Exec(finCtx, `UPDATE databrain.migration_executions
			SET phase='finished', outcome='succeeded',
			    committed_rows=GREATEST(committed_rows, $2),
			    committed_batches=GREATEST(committed_batches, 1),
			    reconciliation_pending=false,
			    failure_code=NULL, failure_message=NULL, failure_sqlstate=NULL,
			    finished_at=coalesce(finished_at, clock_timestamp())
			WHERE id=$1`, executionID, committed.RowsAffected); err != nil {
			return err
		}
	}
	if attemptNumber > 0 {
		if _, err := tx.Exec(finCtx, `UPDATE databrain.migration_worker_attempts
			SET state='succeeded', finished_at=clock_timestamp()
			WHERE execution_id=$1 AND attempt_number=$2 AND state='running'`, executionID, attemptNumber); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(finCtx, `UPDATE databrain.migration_streams
		SET active_execution_id=NULL WHERE target_id=$1 AND active_execution_id=$2`, op.TargetID, executionID); err != nil {
		return err
	}
	if err := tx.Commit(finCtx); err != nil {
		return err
	}

	clearCtx, cancelClear := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
	defer cancelClear()
	if err := journal.ClearJournalOwner(clearCtx, conn, executionID, ownerSecret); err != nil {
		// Platform sudah final; pembersihan owner target akan ditangani ulang pada rekonsiliasi.
		w.logger.Warn("failed to clear target journal owner after platform finalization", "execution_id", executionID.String())
	}
	return nil
}

func (w *WorkerExecutor) rollbackTargetTx(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), workerCleanupTimeout)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// clearFinishedForeignOwner membersihkan unresolved_owner yang tertinggal di target apabila
// worker sebelumnya crash setelah finalisasi platform (phase='finished') dan gate stream sudah lepas.
func (w *WorkerExecutor) clearFinishedForeignOwner(ctx context.Context, conn *pgx.Conn, owner *journal.JournalOwner) (bool, error) {
	if owner == nil || owner.RootExecutionID == uuid.Nil {
		return false, nil
	}
	checkID := owner.ActionExecutionID
	if checkID == uuid.Nil {
		checkID = owner.RootExecutionID
	}
	lease, err := platformdb.Acquire(ctx, w.pool)
	if err != nil {
		return false, err
	}
	defer lease.Release()

	var phase string
	var stillActive bool
	err = lease.QueryRow(ctx, `SELECT e.phase,
		EXISTS (SELECT 1 FROM databrain.migration_streams s
		        WHERE s.target_id = e.target_id AND s.active_execution_id IN ($1, $2))
		FROM databrain.migration_executions e WHERE e.id = $2`,
		owner.RootExecutionID, checkID).Scan(&phase, &stillActive)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if phase != "finished" || stillActive {
		return false, nil
	}
	staleSecret, err := journal.DeriveOwnerSecret(w.appSecret, owner.RootExecutionID)
	if err != nil {
		return false, err
	}
	if err := journal.ClearJournalOwner(ctx, conn, owner.RootExecutionID, staleSecret); err != nil {
		return false, err
	}
	w.logger.Info("cleared stale target journal owner from already-finished execution",
		"root_execution_id", owner.RootExecutionID.String(),
		"action_execution_id", checkID.String(),
	)
	return true, nil
}

func (w *WorkerExecutor) loadExecution(ctx context.Context, executionID uuid.UUID) (storedExecution, error) {
	ctx, cancel := context.WithTimeout(ctx, workerPlatformTimeout)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, w.pool)
	if err != nil {
		return storedExecution{}, err
	}
	defer lease.Release()
	var rec storedExecution
	var expectedHead, sourceExec, outcome *string
	var batchRaw []byte
	err = lease.QueryRow(ctx, `SELECT request_id, definition_id, target_id, actor_id, revision,
		target_config_revision, direction, mode, plan_hash, sql_snapshot, down_snapshot,
		expected_head_id::text, source_execution_id::text, phase, outcome, reconciliation_pending, batch_config
		FROM databrain.migration_executions WHERE id=$1`, executionID).Scan(
		&rec.op.RequestID, &rec.op.DefinitionID, &rec.op.TargetID, &rec.op.ActorID, &rec.op.Revision,
		&rec.op.TargetConfigRevision, &rec.op.Direction, &rec.op.Mode, &rec.op.PlanHash,
		&rec.op.SQLSnapshot, &rec.op.DownSnapshot, &expectedHead, &sourceExec,
		&rec.phase, &outcome, &rec.reconciling, &batchRaw,
	)
	if err != nil {
		return storedExecution{}, err
	}
	if len(batchRaw) > 0 {
		cfg, err := execstore.UnmarshalBatchConfig(batchRaw)
		if err != nil {
			return storedExecution{}, err
		}
		rec.op.Batch = cfg
	}
	if expectedHead != nil && *expectedHead != "" {
		id, err := uuid.Parse(*expectedHead)
		if err != nil {
			return storedExecution{}, err
		}
		rec.op.ExpectedHeadID = &id
	}
	if sourceExec != nil && *sourceExec != "" {
		id, err := uuid.Parse(*sourceExec)
		if err != nil {
			return storedExecution{}, err
		}
		rec.op.SourceExecutionID = &id
	}
	if outcome != nil {
		rec.outcome = *outcome
	}
	return rec, nil
}

func (w *WorkerExecutor) withPlatformTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	txCtx, cancel := context.WithTimeout(ctx, workerPlatformTimeout)
	defer cancel()
	lease, err := platformdb.Acquire(txCtx, w.pool)
	if err != nil {
		return err
	}
	defer lease.Release()
	tx, err := lease.BeginTx(txCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(txCtx) }()
	if err := fn(txCtx, tx); err != nil {
		return err
	}
	return tx.Commit(txCtx)
}

func clearJournalOwnerBestEffort(conn *pgx.Conn, rootID uuid.UUID, ownerSecret [32]byte) {
	ctx, cancel := context.WithTimeout(context.Background(), workerCleanupTimeout)
	defer cancel()
	_ = journal.ClearJournalOwner(ctx, conn, rootID, ownerSecret)
}

func (w *WorkerExecutor) beginAttempt(ctx context.Context, executionID uuid.UUID, jobID int64) (int, error) {
	var attemptNum int
	err := w.withPlatformTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var phase string
		if err := tx.QueryRow(ctx, `SELECT phase FROM databrain.migration_executions WHERE id=$1 FOR UPDATE`, executionID).Scan(&phase); err != nil {
			return err
		}
		if phase == "finished" {
			return errors.New("execution already finished")
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(attempt_number), 0) + 1 FROM databrain.migration_worker_attempts WHERE execution_id=$1`, executionID).Scan(&attemptNum); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_worker_attempts (execution_id, attempt_number, job_id, state)
			VALUES ($1, $2, $3, 'running')`, executionID, attemptNum, jobID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE databrain.migration_executions
			SET phase='running', outcome=NULL, reconciliation_pending=false,
			    started_at=coalesce(started_at, clock_timestamp())
			WHERE id=$1 AND phase IN ('queued','blocked','running')`, executionID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return attemptNum, nil
}

func (w *WorkerExecutor) finishAttempt(executionID uuid.UUID, attemptNumber int, state, code, message, sqlstate string) error {
	if attemptNumber <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), workerPlatformTimeout)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, w.pool)
	if err != nil {
		return err
	}
	defer lease.Release()
	_, err = lease.Exec(ctx, `UPDATE databrain.migration_worker_attempts
		SET state=$3, finished_at=clock_timestamp(),
		    failure_code=$4, failure_message=$5, failure_sqlstate=$6
		WHERE execution_id=$1 AND attempt_number=$2 AND state='running'`,
		executionID, attemptNumber, state, legacy.NullableText(code), legacy.NullableText(truncateMessage(message)), legacy.NullableText(sqlstate))
	return err
}

func (w *WorkerExecutor) markFailedAndRelease(executionID uuid.UUID, attemptNumber int, code, message, sqlstate string) error {
	return w.withPlatformTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var targetID uuid.UUID
		var phase string
		if err := tx.QueryRow(ctx, `SELECT target_id, phase FROM databrain.migration_executions WHERE id=$1 FOR UPDATE`, executionID).Scan(&targetID, &phase); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(new(int)); err != nil {
			return err
		}
		if phase != "finished" {
			if _, err := tx.Exec(ctx, `UPDATE databrain.migration_executions
				SET phase='finished', outcome='failed', reconciliation_pending=false,
				    failure_code=$2, failure_message=$3, failure_sqlstate=$4,
				    finished_at=coalesce(finished_at, clock_timestamp())
				WHERE id=$1`, executionID, legacy.NullableText(code), legacy.NullableText(truncateMessage(message)), legacy.NullableText(sqlstate)); err != nil {
				return err
			}
		}
		if attemptNumber > 0 {
			if _, err := tx.Exec(ctx, `UPDATE databrain.migration_worker_attempts
				SET state='failed', finished_at=clock_timestamp(),
				    failure_code=$3, failure_message=$4, failure_sqlstate=$5
				WHERE execution_id=$1 AND attempt_number=$2 AND state='running'`,
				executionID, attemptNumber, legacy.NullableText(code), legacy.NullableText(truncateMessage(message)), legacy.NullableText(sqlstate)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE databrain.migration_streams
			SET active_execution_id=NULL WHERE target_id=$1 AND active_execution_id=$2`, targetID, executionID)
		return err
	})
}

func (w *WorkerExecutor) markBlockedUnknown(executionID uuid.UUID, attemptNumber int, code, message, sqlstate string) error {
	return w.withPlatformTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var phase string
		if err := tx.QueryRow(ctx, `SELECT phase FROM databrain.migration_executions WHERE id=$1 FOR UPDATE`, executionID).Scan(&phase); err != nil {
			return err
		}
		if phase != "finished" {
			if _, err := tx.Exec(ctx, `UPDATE databrain.migration_executions
				SET phase='blocked', outcome='unknown', reconciliation_pending=true,
				    failure_code=$2, failure_message=$3, failure_sqlstate=$4
				WHERE id=$1`, executionID, legacy.NullableText(code), legacy.NullableText(truncateMessage(message)), legacy.NullableText(sqlstate)); err != nil {
				return err
			}
		}
		if attemptNumber > 0 {
			if _, err := tx.Exec(ctx, `UPDATE databrain.migration_worker_attempts
				SET state='unknown', finished_at=clock_timestamp(),
				    failure_code=$3, failure_message=$4, failure_sqlstate=$5
				WHERE execution_id=$1 AND attempt_number=$2 AND state='running'`,
				executionID, attemptNumber, legacy.NullableText(code), legacy.NullableText(truncateMessage(message)), legacy.NullableText(sqlstate)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (w *WorkerExecutor) markReconciling(executionID uuid.UUID, attemptNumber int, code, message string) error {
	return w.markBlockedUnknown(executionID, attemptNumber, code, message, "")
}

func truncateMessage(msg string) string {
	if len(msg) > 240 {
		return msg[:240]
	}
	return msg
}
