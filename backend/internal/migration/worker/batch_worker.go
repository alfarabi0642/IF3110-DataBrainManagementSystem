package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/head"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/journal"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/legacy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// executeBatch menjalankan mode batch (apply, resume, atau compensate) di bawah lock kepemilikan
// fisik target (3110,2) dengan transaksi + checkpoint atomik per batch dan memori terbatas satu batch
func (w *WorkerExecutor) executeBatch(ctx context.Context, conn *pgx.Conn, executionID uuid.UUID,
	jobID int64, op execstore.ApprovedOperation) error {
	if op.Batch == nil {
		_ = w.markFailedAndRelease(executionID, 0, "VALIDATION_FAILED", "Batch execution is missing batch configuration.", "")
		return jobs.PermanentError{Err: errors.New("missing batch configuration")}
	}
	plan, err := sqlpolicy.ValidateBatchRecipe(ctx, conn, *op.Batch)
	if err != nil {
		_ = w.markFailedAndRelease(executionID, 0, string(problem.SQLUnsupported), "Target table no longer satisfies the supported v1 batch recipe.", "")
		return jobs.PermanentError{Err: err}
	}
	if op.SQLSnapshot != plan.UpSQL || op.DownSnapshot != plan.DownSQL {
		_ = w.markFailedAndRelease(executionID, 0, string(problem.SQLUnsupported), "Batch SQL snapshot does not match verified template.", "")
		return jobs.PermanentError{Err: errors.New("batch template mismatch")}
	}

	rootID := executionID
	var immediateSourceID uuid.UUID
	isRecovery := op.Direction == execstore.DirectionResume || op.Direction == execstore.DirectionCompensate
	if isRecovery {
		if op.SourceExecutionID == nil || *op.SourceExecutionID == uuid.Nil {
			_ = w.markFailedAndRelease(executionID, 0, "INVALID_REQUEST", "Recovery action requires an immutable source execution.", "")
			return jobs.PermanentError{Err: errors.New("missing source execution ID")}
		}
		immediateSourceID = *op.SourceExecutionID
		resolvedRoot, err := w.resolveRootExecutionID(ctx, immediateSourceID)
		if err != nil {
			return err
		}
		rootID = resolvedRoot
	}

	ownerSecret, err := journal.DeriveOwnerSecret(w.appSecret, rootID)
	if err != nil {
		return jobs.PermanentError{Err: err}
	}

	// Cek pemilik unresolved lintas-alias. Untuk recovery, root_execution_id harus sama dengan rootID.
	owner, err := journal.ReadJournalOwner(ctx, conn)
	if err != nil {
		return err
	}
	if owner != nil && owner.RootExecutionID != rootID {
		if cleared, clearErr := w.clearFinishedForeignOwner(ctx, conn, owner); clearErr != nil {
			return clearErr
		} else if !cleared {
			return jobs.SnoozeError{Duration: workerLockSnooze, Reason: "physical target owned by another unresolved execution"}
		}
		owner = nil
	}

	// Revalidasi kebijakan approval sebelum mulai (dan ulangi sebelum setiap batch baru).
	if err := w.guard(ctx, op); err != nil {
		var pe *problem.Error
		if errors.As(err, &pe) && pe.Code == problem.PlatformUnavailable {
			return err
		}
		if !isRecovery && owner != nil && owner.RootExecutionID == rootID {
			clearJournalOwnerBestEffort(conn, rootID, ownerSecret)
		}
		if isRecovery {
			_ = w.markRecoveryFailedKeepSourceGate(executionID, immediateSourceID, 0, "ELIGIBILITY_DENIED", "Recovery action is no longer eligible.")
		} else {
			_ = w.markFailedAndRelease(executionID, 0, "ELIGIBILITY_DENIED", "Batch migration is no longer eligible.", "")
		}
		return jobs.PermanentError{Err: err}
	}

	attemptNumber, err := w.beginAttempt(ctx, executionID, jobID)
	if err != nil {
		return err
	}

	// Ambil atau pindahkan kepemilikan target journal untuk rootID.
	if isRecovery {
		if owner == nil {
			if err := journal.AcquireJournalOwner(ctx, conn, rootID, executionID, op.PlanHash, ownerSecret); err != nil {
				_ = w.finishAttempt(executionID, attemptNumber, "failed", "TARGET_JOURNAL_UNSAFE", "Could not acquire target journal ownership.", "")
				return err
			}
		} else {
			if err := journal.TransferJournalOwner(ctx, conn, rootID, executionID, op.PlanHash, ownerSecret); err != nil {
				_ = w.finishAttempt(executionID, attemptNumber, "failed", "TARGET_JOURNAL_UNSAFE", "Could not transfer target journal ownership.", "")
				return err
			}
		}
	} else {
		if err := journal.AcquireJournalOwner(ctx, conn, rootID, executionID, op.PlanHash, ownerSecret); err != nil {
			if errors.Is(err, journal.ErrJournalOwned) {
				_ = w.finishAttempt(executionID, attemptNumber, "failed", "TARGET_BUSY", "Target owned by another unresolved execution.", "")
				return jobs.SnoozeError{Duration: workerLockSnooze, Reason: "target journal owned by another execution"}
			}
			_ = w.finishAttempt(executionID, attemptNumber, "failed", "TARGET_JOURNAL_UNSAFE", "Could not acquire durable target journal ownership.", "")
			return err
		}
	}

	// Tangkap cakupan (hanya sekali per root execution) menggunakan keyset cursor terbatas 1 batch
	// di dalam satu transaksi REPEATABLE READ agar snapshot konsisten dan atomik (tidak pernah parsial).
	totalRows, totalBatches, err := journal.ReadBatchScopeMeta(ctx, conn, rootID)
	if err != nil {
		return err
	}
	if totalBatches == 0 && op.Direction == execstore.DirectionApply {
		totalRows, totalBatches, err = captureBatchScopeChunked(ctx, conn, rootID, executionID, op.PlanHash, plan.Config)
		if err != nil {
			_ = w.finishAttempt(executionID, attemptNumber, "failed", "BATCH_SCOPE_FAILED", "Could not capture bounded batch scope.", "")
			return err
		}
	}
	_ = w.updateExecutionTotalRows(executionID, totalRows)

	if op.Direction == execstore.DirectionCompensate {
		return w.runReverseCompensation(ctx, conn, executionID, rootID, attemptNumber, op, plan, ownerSecret)
	}
	return w.runForwardBatches(ctx, conn, executionID, rootID, attemptNumber, op, plan, totalRows, totalBatches, ownerSecret)
}

// captureBatchScopeChunked membaca tabel target dalam potongan keyset <= batch_size di dalam satu
// transaksi REPEATABLE READ sehingga snapshot konsisten, atomik (all-or-nothing), dan memori Go
// tidak pernah menampung seluruh dataset.
func captureBatchScopeChunked(ctx context.Context, conn *pgx.Conn, rootID, actionID uuid.UUID,
	planHash string, cfg sqlpolicy.BatchConfig) (int64, int, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()

	table := pgx.Identifier{cfg.Schema, cfg.Table}.Sanitize()
	keyCol := pgx.Identifier{cfg.KeyColumn}.Sanitize()
	valCol := pgx.Identifier{cfg.ValueColumn}.Sanitize()

	var totalRows int64
	ordinal := 0
	var lastKey int64
	hasCursor := false

	for {
		var rows pgx.Rows
		if !hasCursor {
			rows, err = tx.Query(ctx,
				"SELECT "+keyCol+", "+valCol+" FROM "+table+" ORDER BY "+keyCol+" ASC LIMIT $1",
				cfg.BatchSize)
		} else {
			rows, err = tx.Query(ctx,
				"SELECT "+keyCol+", "+valCol+" FROM "+table+" WHERE "+keyCol+" > $1 ORDER BY "+keyCol+" ASC LIMIT $2",
				lastKey, cfg.BatchSize)
		}
		if err != nil {
			return 0, 0, err
		}
		keys := make([]int64, 0, cfg.BatchSize)
		beforeVals := make([]int64, 0, cfg.BatchSize)
		afterVals := make([]int64, 0, cfg.BatchSize)
		for rows.Next() {
			var k, v int64
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return 0, 0, err
			}
			keys = append(keys, k)
			beforeVals = append(beforeVals, v)
			afterVals = append(afterVals, v+cfg.Delta)
			lastKey = k
			hasCursor = true
		}
		rowErr := rows.Err()
		rows.Close()
		if rowErr != nil {
			return 0, 0, rowErr
		}
		if len(keys) == 0 {
			break
		}
		ordinal++
		if err := journal.RecordBatchScope(ctx, tx, rootID, actionID, planHash, ordinal, keys, beforeVals, afterVals); err != nil {
			return 0, 0, err
		}
		totalRows += int64(len(keys))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return totalRows, ordinal, nil
}

func (w *WorkerExecutor) runForwardBatches(ctx context.Context, conn *pgx.Conn, executionID, rootID uuid.UUID,
	attemptNumber int, op execstore.ApprovedOperation, plan sqlpolicy.BatchPlan, totalRows int64, totalBatches int, ownerSecret [32]byte) error {
	commits, err := journal.ReadJournalCommits(ctx, conn, rootID)
	if err != nil {
		return err
	}
	committedOrds := make(map[int]journal.JournalCommit)
	var committedRowsCount int64
	for _, c := range commits {
		if c.BatchOrdinal >= 1 && (c.Direction == execstore.DirectionApply || c.Direction == execstore.DirectionResume) {
			committedOrds[c.BatchOrdinal] = c
			committedRowsCount += c.RowsAffected
		}
	}
	// Pastikan proyeksi platform mencerminkan batch yang sudah ter-commit sebelum melanjutkan.
	for ord, c := range committedOrds {
		_ = w.projectCommittedBatch(op.TargetID, rootID, executionID, ord, c.Direction, c.RowsAffected, c.CursorEnd, c.CommittedAt)
	}

	for ord := 1; ord <= totalBatches; ord++ {
		if _, done := committedOrds[ord]; done {
			continue
		}
		// Revalidasi sebelum setiap batch baru; batch yang sudah commit tidak pernah dihapus.
		if ord > 1 {
			if err := w.guard(ctx, op); err != nil {
				_ = w.markBlockedPartial(executionID, rootID, attemptNumber, int64(len(committedOrds)), committedRowsCount,
					"ELIGIBILITY_DENIED", "Eligibility revoked between batches; prior committed batches remain intact.", "")
				return jobs.PermanentError{Err: err}
			}
		}

		scopeRows, err := journal.ReadBatchScope(ctx, conn, rootID, ord)
		if err != nil {
			return err
		}
		if len(scopeRows) == 0 {
			continue
		}
		keys := make([]int64, len(scopeRows))
		for i, r := range scopeRows {
			keys[i] = r.Key
		}
		cursorEnd := strconv.FormatInt(keys[len(keys)-1], 10)

		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			_ = w.markBlockedPartial(executionID, rootID, attemptNumber, int64(len(committedOrds)), committedRowsCount,
				"CONNECTION_FAILED", "Could not start batch transaction.", "")
			return jobs.PermanentError{Err: err}
		}
		txGuard, err := journal.ArmJournalTx(ctx, tx, rootID, executionID, op.PlanHash)
		if err != nil {
			w.rollbackTargetTx(tx)
			_ = w.markBlockedPartial(executionID, rootID, attemptNumber, int64(len(committedOrds)), committedRowsCount,
				"TARGET_JOURNAL_UNSAFE", "Could not arm batch transaction guard.", "")
			return jobs.PermanentError{Err: err}
		}
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout='300s'; SET LOCAL lock_timeout='3s'; SET LOCAL search_path=public`, pgx.QueryExecModeSimpleProtocol); err != nil {
			w.rollbackTargetTx(tx)
			_ = w.markBlockedPartial(executionID, rootID, attemptNumber, int64(len(committedOrds)), committedRowsCount,
				"SQL_EXECUTION_FAILED", "Could not configure batch session parameters.", "")
			return jobs.PermanentError{Err: err}
		}

		if w.BeforeBatch != nil {
			if hookErr := w.BeforeBatch(ord); hookErr != nil {
				w.rollbackTargetTx(tx)
				_ = w.markBlockedPartial(executionID, rootID, attemptNumber, int64(len(committedOrds)), committedRowsCount,
					"BATCH_FAILED", fmt.Sprintf("Batch %d aborted before commit; prior committed batches preserved.", ord), "")
				return jobs.PermanentError{Err: hookErr}
			}
		}

		batchStart := time.Now()
		batchCtx, batchSpan := telemetry.Tracer().Start(ctx, "migration.batch")
		batchCtx = telemetry.WithLogAttrs(batchCtx, telemetry.FieldBatch, ord, "attempt_number", attemptNumber)
		batchSpan.SetAttributes(
			attribute.String(telemetry.FieldExecutionID, executionID.String()),
			attribute.String(telemetry.FieldDatabase, op.TargetID.String()),
			attribute.Int(telemetry.FieldBatch, ord),
			attribute.Int(telemetry.FieldAttempt, attemptNumber),
			attribute.String("direction", op.Direction),
		)

		tag, execErr := tx.Exec(batchCtx, plan.UpSQL, keys, plan.Config.Delta)
		if execErr != nil {
			f := legacy.StatementFailure(batchCtx, execErr, ord)
			batchSpan.SetAttributes(attribute.String(telemetry.FieldStatus, "error"), attribute.Int64(telemetry.FieldDuration, time.Since(batchStart).Milliseconds()))
			batchSpan.SetStatus(codes.Error, f.Code)
			batchSpan.End()
			w.rollbackTargetTx(tx)
			if len(committedOrds) == 0 {
				clearCtx, cancel := context.WithTimeout(context.WithoutCancel(batchCtx), workerCleanupTimeout)
				_ = journal.ClearJournalOwner(clearCtx, conn, rootID, ownerSecret)
				cancel()
				_ = w.markFailedAndRelease(executionID, attemptNumber, f.Code, f.Message, f.SQLState)
			} else {
				_ = w.markBlockedPartial(executionID, rootID, attemptNumber, int64(len(committedOrds)), committedRowsCount,
					f.Code, f.Message, f.SQLState)
			}
			return jobs.PermanentError{Err: execErr}
		}
		if tag.RowsAffected() != int64(len(keys)) {
			batchSpan.SetAttributes(attribute.String(telemetry.FieldStatus, "error"), attribute.Int64(telemetry.FieldDuration, time.Since(batchStart).Milliseconds()))
			batchSpan.SetStatus(codes.Error, "BATCH_SCOPE_MISMATCH")
			batchSpan.End()
			w.rollbackTargetTx(tx)
			if len(committedOrds) == 0 {
				clearCtx, cancel := context.WithTimeout(context.WithoutCancel(batchCtx), workerCleanupTimeout)
				_ = journal.ClearJournalOwner(clearCtx, conn, rootID, ownerSecret)
				cancel()
				_ = w.markFailedAndRelease(executionID, attemptNumber, "BATCH_SCOPE_MISMATCH", "Target rows changed concurrently during batch execution.", "")
			} else {
				_ = w.markBlockedPartial(executionID, rootID, attemptNumber, int64(len(committedOrds)), committedRowsCount,
					"BATCH_SCOPE_MISMATCH", "Target rows changed concurrently during batch execution.", "")
			}
			return jobs.PermanentError{Err: errors.New("batch affected row count mismatch")}
		}

		batchSpan.SetAttributes(attribute.Int64(telemetry.FieldRows, tag.RowsAffected()))
		if err := txGuard.RecordCommit(batchCtx, tx, ord, op.Direction, tag.RowsAffected(), &cursorEnd); err != nil {
			batchSpan.SetAttributes(attribute.String(telemetry.FieldStatus, "error"), attribute.Int64(telemetry.FieldDuration, time.Since(batchStart).Milliseconds()))
			batchSpan.SetStatus(codes.Error, "TARGET_JOURNAL_UNSAFE")
			batchSpan.End()
			w.rollbackTargetTx(tx)
			_ = w.markBlockedPartial(executionID, rootID, attemptNumber, int64(len(committedOrds)), committedRowsCount,
				"TARGET_JOURNAL_UNSAFE", "Could not record batch checkpoint.", "")
			return jobs.PermanentError{Err: err}
		}
		if err := tx.Commit(batchCtx); err != nil {
			batchSpan.SetAttributes(attribute.String(telemetry.FieldStatus, "unknown"), attribute.Int64(telemetry.FieldDuration, time.Since(batchStart).Milliseconds()))
			batchSpan.SetStatus(codes.Error, "COMMIT_UNCERTAIN")
			batchSpan.End()
			_ = w.markBlockedUnknown(executionID, attemptNumber, "COMMIT_UNCERTAIN", "Batch commit outcome could not be confirmed.", "")
			return jobs.PermanentError{Err: err}
		}
		batchDur := time.Since(batchStart).Milliseconds()
		batchSpan.SetAttributes(
			attribute.String(telemetry.FieldStatus, "committed"),
			attribute.Int64(telemetry.FieldDuration, batchDur),
		)
		batchAttrs := append([]any{
			telemetry.FieldExecutionID, executionID.String(),
			telemetry.FieldDatabase, op.TargetID.String(),
			telemetry.FieldBatch, ord,
			telemetry.FieldAttempt, attemptNumber,
			"direction", op.Direction,
			telemetry.FieldRows, tag.RowsAffected(),
			telemetry.FieldDuration, batchDur,
			telemetry.FieldStatus, "committed",
		}, telemetry.TraceAttrs(batchCtx)...)
		w.logger.Info("migration batch committed", batchAttrs...)
		batchSpan.End()

		nowUTC := time.Now().UTC()
		committedOrds[ord] = journal.JournalCommit{
			RootExecutionID: rootID, BatchOrdinal: ord, Direction: op.Direction,
			ActionExecutionID: executionID, PlanHash: op.PlanHash,
			RowsAffected: tag.RowsAffected(), CursorEnd: &cursorEnd, CommittedAt: nowUTC,
		}
		committedRowsCount += tag.RowsAffected()
		_ = w.projectCommittedBatch(op.TargetID, rootID, executionID, ord, op.Direction, tag.RowsAffected(), &cursorEnd, nowUTC)
	}

	// Semua batch selesai: finalisasi logical apply satu kali di platform
	finalCommit := journal.JournalCommit{
		RootExecutionID:   rootID,
		BatchOrdinal:      totalBatches,
		Direction:         execstore.DirectionApply,
		ActionExecutionID: executionID,
		PlanHash:          op.PlanHash,
		RowsAffected:      committedRowsCount,
		CommittedAt:       time.Now().UTC(),
	}
	return w.finalizeBatchSuccess(conn, executionID, rootID, attemptNumber, op, finalCommit, int64(totalBatches), committedRowsCount, ownerSecret)
}

func (w *WorkerExecutor) runReverseCompensation(ctx context.Context, conn *pgx.Conn, executionID, rootID uuid.UUID,
	attemptNumber int, op execstore.ApprovedOperation, plan sqlpolicy.BatchPlan, ownerSecret [32]byte) error {
	commits, err := journal.ReadJournalCommits(ctx, conn, rootID)
	if err != nil {
		return err
	}
	forwardOrds := make([]int, 0)
	reverted := make(map[int]int64)
	for _, c := range commits {
		if c.BatchOrdinal >= 1 {
			if c.Direction == execstore.DirectionApply || c.Direction == execstore.DirectionResume {
				forwardOrds = append(forwardOrds, c.BatchOrdinal)
			} else if c.Direction == execstore.DirectionCompensate {
				reverted[c.BatchOrdinal] = c.RowsAffected
			}
		}
	}
	// Urutkan descending (batch terakhir yang committed dibatalkan lebih dulu).
	slices.SortFunc(forwardOrds, func(a, b int) int { return cmp.Compare(b, a) })

	table := pgx.Identifier{plan.Config.Schema, plan.Config.Table}.Sanitize()
	keyCol := pgx.Identifier{plan.Config.KeyColumn}.Sanitize()
	valCol := pgx.Identifier{plan.Config.ValueColumn}.Sanitize()

	var revertedBatches int64
	var revertedRows int64
	processed := 0
	for _, ord := range forwardOrds {
		if priorRows, done := reverted[ord]; done {
			revertedBatches++
			revertedRows += priorRows
			continue
		}
		// Revalidasi izin dan approval sebelum setiap batch kompensasi berikutnya.
		if processed > 0 {
			if err := w.guard(ctx, op); err != nil {
				_ = w.markPartiallyReverted(executionID, rootID, attemptNumber, revertedBatches, revertedRows,
					"ELIGIBILITY_DENIED", "Approval or access changed between compensation batches.", "")
				return jobs.PermanentError{Err: err}
			}
		}
		processed++
		scopeRows, err := journal.ReadBatchScope(ctx, conn, rootID, ord)
		if err != nil {
			return err
		}
		keys := make([]int64, len(scopeRows))
		expectedAfter := make(map[int64]int64, len(scopeRows))
		for i, r := range scopeRows {
			keys[i] = r.Key
			expectedAfter[r.Key] = r.AfterValue
		}
		cursorEnd := strconv.FormatInt(keys[len(keys)-1], 10)

		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			_ = w.markPartiallyReverted(executionID, rootID, attemptNumber, revertedBatches, revertedRows, "CONNECTION_FAILED", "Could not start compensation transaction.", "")
			return jobs.PermanentError{Err: err}
		}
		txGuard, err := journal.ArmJournalTx(ctx, tx, rootID, executionID, op.PlanHash)
		if err != nil {
			w.rollbackTargetTx(tx)
			_ = w.markPartiallyReverted(executionID, rootID, attemptNumber, revertedBatches, revertedRows, "TARGET_JOURNAL_UNSAFE", "Could not arm compensation guard.", "")
			return jobs.PermanentError{Err: err}
		}

		// Verifikasi after-image sebelum menjalankan SQL down agar perubahan eksternal tidak tertimpa.
		rows, err := tx.Query(ctx, "SELECT "+keyCol+", "+valCol+" FROM "+table+" WHERE "+keyCol+" = ANY($1) FOR UPDATE", keys)
		if err != nil {
			w.rollbackTargetTx(tx)
			_ = w.markPartiallyReverted(executionID, rootID, attemptNumber, revertedBatches, revertedRows, "COMPENSATION_UNSAFE", "Could not verify target rows before compensation.", "")
			return jobs.PermanentError{Err: err}
		}
		matched := 0
		mismatch := false
		for rows.Next() {
			var k, v int64
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				w.rollbackTargetTx(tx)
				return jobs.PermanentError{Err: err}
			}
			if want, ok := expectedAfter[k]; !ok || v != want {
				mismatch = true
			}
			matched++
		}
		rows.Close()
		if mismatch || matched != len(keys) {
			w.rollbackTargetTx(tx)
			_ = w.markPartiallyReverted(executionID, rootID, attemptNumber, revertedBatches, revertedRows,
				"COMPENSATION_UNSAFE", fmt.Sprintf("Batch %d rows changed externally; reverse compensation stopped safely.", ord), "")
			return jobs.PermanentError{Err: errors.New("external modification detected before reverse compensation")}
		}

		if w.BeforeBatch != nil {
			if hookErr := w.BeforeBatch(ord); hookErr != nil {
				w.rollbackTargetTx(tx)
				_ = w.markPartiallyReverted(executionID, rootID, attemptNumber, revertedBatches, revertedRows,
					"COMPENSATION_STOPPED", fmt.Sprintf("Reverse compensation stopped at batch %d.", ord), "")
				return jobs.PermanentError{Err: hookErr}
			}
		}

		batchStart := time.Now()
		batchCtx, batchSpan := telemetry.Tracer().Start(ctx, "migration.batch")
		batchCtx = telemetry.WithLogAttrs(batchCtx, telemetry.FieldBatch, ord, "attempt_number", attemptNumber)
		batchSpan.SetAttributes(
			attribute.String(telemetry.FieldExecutionID, executionID.String()),
			attribute.String(telemetry.FieldDatabase, op.TargetID.String()),
			attribute.Int(telemetry.FieldBatch, ord),
			attribute.Int(telemetry.FieldAttempt, attemptNumber),
			attribute.String("direction", execstore.DirectionCompensate),
		)

		tag, execErr := tx.Exec(batchCtx, plan.DownSQL, keys, plan.Config.Delta)
		if execErr != nil || tag.RowsAffected() != int64(len(keys)) {
			batchSpan.SetAttributes(attribute.String(telemetry.FieldStatus, "error"), attribute.Int64(telemetry.FieldDuration, time.Since(batchStart).Milliseconds()))
			batchSpan.SetStatus(codes.Error, "COMPENSATION_FAILED")
			batchSpan.End()
			w.rollbackTargetTx(tx)
			_ = w.markPartiallyReverted(executionID, rootID, attemptNumber, revertedBatches, revertedRows,
				"COMPENSATION_FAILED", fmt.Sprintf("Reverse compensation failed on batch %d.", ord), "")
			return jobs.PermanentError{Err: errors.New("reverse compensation batch failed")}
		}
		batchSpan.SetAttributes(attribute.Int64(telemetry.FieldRows, tag.RowsAffected()))
		if err := txGuard.RecordCommit(batchCtx, tx, ord, execstore.DirectionCompensate, tag.RowsAffected(), &cursorEnd); err != nil {
			batchSpan.SetAttributes(attribute.String(telemetry.FieldStatus, "error"), attribute.Int64(telemetry.FieldDuration, time.Since(batchStart).Milliseconds()))
			batchSpan.SetStatus(codes.Error, "TARGET_JOURNAL_UNSAFE")
			batchSpan.End()
			w.rollbackTargetTx(tx)
			_ = w.markPartiallyReverted(executionID, rootID, attemptNumber, revertedBatches, revertedRows,
				"TARGET_JOURNAL_UNSAFE", "Could not record reverse compensation checkpoint.", "")
			return jobs.PermanentError{Err: err}
		}
		if err := tx.Commit(batchCtx); err != nil {
			batchSpan.SetAttributes(attribute.String(telemetry.FieldStatus, "unknown"), attribute.Int64(telemetry.FieldDuration, time.Since(batchStart).Milliseconds()))
			batchSpan.SetStatus(codes.Error, "COMMIT_UNCERTAIN")
			batchSpan.End()
			_ = w.markBlockedUnknown(executionID, attemptNumber, "COMMIT_UNCERTAIN", "Compensation commit outcome could not be confirmed.", "")
			return jobs.PermanentError{Err: err}
		}
		batchDur := time.Since(batchStart).Milliseconds()
		batchSpan.SetAttributes(
			attribute.String(telemetry.FieldStatus, "committed"),
			attribute.Int64(telemetry.FieldDuration, batchDur),
		)
		batchAttrs := append([]any{
			telemetry.FieldExecutionID, executionID.String(),
			telemetry.FieldDatabase, op.TargetID.String(),
			telemetry.FieldBatch, ord,
			telemetry.FieldAttempt, attemptNumber,
			"direction", execstore.DirectionCompensate,
			telemetry.FieldRows, tag.RowsAffected(),
			telemetry.FieldDuration, batchDur,
			telemetry.FieldStatus, "committed",
		}, telemetry.TraceAttrs(batchCtx)...)
		w.logger.Info("migration batch compensated", batchAttrs...)
		batchSpan.End()

		nowUTC := time.Now().UTC()
		reverted[ord] = tag.RowsAffected()
		revertedBatches++
		revertedRows += tag.RowsAffected()
		_ = w.projectCommittedBatch(op.TargetID, rootID, executionID, ord, execstore.DirectionCompensate, tag.RowsAffected(), &cursorEnd, nowUTC)
	}

	// Seluruh batch committed berhasil dikompensasi: prior head tetap tidak berubah
	return w.finalizeCompensationComplete(conn, executionID, rootID, attemptNumber, op.TargetID, revertedBatches, revertedRows, ownerSecret)
}

func (w *WorkerExecutor) updateExecutionTotalRows(executionID uuid.UUID, totalRows int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), workerPlatformTimeout)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, w.pool)
	if err != nil {
		return err
	}
	defer lease.Release()
	_, err = lease.Exec(ctx, `UPDATE databrain.migration_executions SET total_rows=$2 WHERE id=$1 AND phase <> 'finished'`, executionID, totalRows)
	return err
}

func (w *WorkerExecutor) projectCommittedBatch(targetID, rootID, actionID uuid.UUID, ord int, dir string,
	rows int64, cursorEnd *string, committedAt time.Time) error {
	return w.withPlatformTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_execution_batches
			(target_id, execution_id, batch_ordinal, direction, action_execution_id, rows_affected, cursor_end, committed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (target_id, execution_id, batch_ordinal, direction) DO NOTHING`,
			targetID, rootID, ord, dir, actionID, rows, cursorEnd, committedAt); err != nil {
			return err
		}
		if dir == execstore.DirectionApply || dir == execstore.DirectionResume {
			_, err := tx.Exec(ctx, `UPDATE databrain.migration_executions
				SET committed_batches=(SELECT count(*) FROM databrain.migration_execution_batches WHERE target_id=$1 AND execution_id=$2 AND direction IN ('apply','resume')),
				    committed_rows=(SELECT coalesce(sum(rows_affected),0) FROM databrain.migration_execution_batches WHERE target_id=$1 AND execution_id=$2 AND direction IN ('apply','resume')),
				    has_partial_commits=true
				WHERE id=$3 AND phase <> 'finished'`, targetID, rootID, actionID)
			return err
		}
		if dir == execstore.DirectionCompensate {
			_, err := tx.Exec(ctx, `UPDATE databrain.migration_executions
				SET committed_batches=(SELECT count(*) FROM databrain.migration_execution_batches WHERE target_id=$1 AND execution_id=$2 AND direction='compensate'),
				    committed_rows=(SELECT coalesce(sum(rows_affected),0) FROM databrain.migration_execution_batches WHERE target_id=$1 AND execution_id=$2 AND direction='compensate'),
				    has_partial_commits=true
				WHERE id=$3 AND phase <> 'finished'`, targetID, rootID, actionID)
			return err
		}
		return nil
	})
}

func (w *WorkerExecutor) markBlockedPartial(executionID, rootID uuid.UUID, attemptNumber int,
	committedBatches, committedRows int64, code, message, sqlstate string) error {
	return w.markBlockedBatchOutcome(executionID, attemptNumber, "partial", committedBatches, committedRows, code, message, sqlstate)
}

func (w *WorkerExecutor) markPartiallyReverted(executionID, rootID uuid.UUID, attemptNumber int,
	revertedBatches, revertedRows int64, code, message, sqlstate string) error {
	return w.markBlockedBatchOutcome(executionID, attemptNumber, "partially_reverted", revertedBatches, revertedRows, code, message, sqlstate)
}

func (w *WorkerExecutor) markBlockedBatchOutcome(executionID uuid.UUID, attemptNumber int, outcome string,
	batches, rows int64, code, message, sqlstate string) error {
	return w.withPlatformTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE databrain.migration_executions
			SET phase='blocked', outcome=$2,
			    committed_batches=GREATEST(committed_batches, $3),
			    committed_rows=GREATEST(committed_rows, $4),
			    has_partial_commits=true, reconciliation_pending=false,
			    failure_code=$5, failure_message=$6, failure_sqlstate=$7
			WHERE id=$1 AND phase <> 'finished'`,
			executionID, outcome, batches, rows, legacy.NullableText(code), legacy.NullableText(truncateMessage(message)), legacy.NullableText(sqlstate)); err != nil {
			return err
		}
		if attemptNumber > 0 {
			_, _ = tx.Exec(ctx, `UPDATE databrain.migration_worker_attempts
				SET state='partial', finished_at=clock_timestamp(),
				    failure_code=$3, failure_message=$4, failure_sqlstate=$5
				WHERE execution_id=$1 AND attempt_number=$2 AND state='running'`,
				executionID, attemptNumber, legacy.NullableText(code), legacy.NullableText(truncateMessage(message)), legacy.NullableText(sqlstate))
		}
		return nil
	})
}

func (w *WorkerExecutor) markRecoveryFailedKeepSourceGate(executionID, rootID uuid.UUID, attemptNumber int, code, message string) error {
	return w.withPlatformTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var targetID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT target_id FROM databrain.migration_executions WHERE id=$1 FOR UPDATE`, executionID).Scan(&targetID); err != nil {
			return err
		}
		_, _ = tx.Exec(ctx, `UPDATE databrain.migration_executions
			SET phase='finished', outcome='failed', failure_code=$2, failure_message=$3, finished_at=clock_timestamp()
			WHERE id=$1 AND phase <> 'finished'`, executionID, legacy.NullableText(code), legacy.NullableText(truncateMessage(message)))
		// Kembalikan gate stream ke source execution yang masih partial agar target tetap terlindungi.
		_, _ = tx.Exec(ctx, `UPDATE databrain.migration_streams SET active_execution_id=$2 WHERE target_id=$1 AND active_execution_id=$3`,
			targetID, rootID, executionID)
		return nil
	})
}

func (w *WorkerExecutor) finalizeBatchSuccess(conn *pgx.Conn, executionID, rootID uuid.UUID, attemptNumber int,
	op execstore.ApprovedOperation, committed journal.JournalCommit, totalBatches, totalRows int64, ownerSecret [32]byte) error {
	err := w.withPlatformTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, op.TargetID).Scan(new(int)); err != nil {
			return err
		}
		// Finalize logical apply once on rootID
		var rootPlanHash string
		if err := tx.QueryRow(ctx, `SELECT plan_hash FROM databrain.migration_executions WHERE id=$1 FOR UPDATE`, rootID).Scan(&rootPlanHash); err != nil {
			return err
		}
		ev := head.CommitEvidence{
			RootExecutionID:   rootID,
			ActionExecutionID: executionID,
			PlanHash:          rootPlanHash,
			Direction:         execstore.DirectionApply,
			Mode:              execstore.ModeBatch,
			RowsAffected:      totalRows,
			CommittedAt:       committed.CommittedAt,
			Evidence:          fmt.Sprintf("target_journal:v%d:batches=%d", journal.TargetJournalVersion, totalBatches),
		}
		if err := w.finalizer(ctx, tx, rootID, ev); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE databrain.migration_executions
			SET phase='finished', outcome='succeeded',
			    committed_batches=GREATEST(committed_batches, $2),
			    committed_rows=GREATEST(committed_rows, $3),
			    reconciliation_pending=false, failure_code=NULL, failure_message=NULL, failure_sqlstate=NULL,
			    finished_at=coalesce(finished_at, clock_timestamp())
			WHERE id=$1 AND phase <> 'finished'`, executionID, totalBatches, totalRows); err != nil {
			return err
		}
		if rootID != executionID {
			_, _ = tx.Exec(ctx, `UPDATE databrain.migration_executions
				SET phase='finished', outcome='succeeded',
				    committed_batches=GREATEST(committed_batches, $2),
				    committed_rows=GREATEST(committed_rows, $3),
				    reconciliation_pending=false, failure_code=NULL, failure_message=NULL, failure_sqlstate=NULL,
				    finished_at=coalesce(finished_at, clock_timestamp())
				WHERE id=$1 AND phase <> 'finished'`, rootID, totalBatches, totalRows)
		}
		if attemptNumber > 0 {
			_, _ = tx.Exec(ctx, `UPDATE databrain.migration_worker_attempts
				SET state='succeeded', finished_at=clock_timestamp()
				WHERE execution_id=$1 AND attempt_number=$2 AND state='running'`, executionID, attemptNumber)
		}
		_, err := tx.Exec(ctx, `UPDATE databrain.migration_streams
			SET active_execution_id=NULL WHERE target_id=$1 AND active_execution_id IN ($2, $3)`, op.TargetID, executionID, rootID)
		return err
	})
	if err != nil {
		return err
	}
	clearJournalOwnerBestEffort(conn, rootID, ownerSecret)
	return nil
}

func (w *WorkerExecutor) finalizeCompensationComplete(conn *pgx.Conn, executionID, rootID uuid.UUID,
	attemptNumber int, targetID uuid.UUID, revertedBatches, revertedRows int64, ownerSecret [32]byte) error {
	err := w.withPlatformTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(new(int)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE databrain.migration_executions
			SET phase='finished', outcome='succeeded',
			    committed_batches=GREATEST(committed_batches, $2),
			    committed_rows=GREATEST(committed_rows, $3),
			    reconciliation_pending=false, failure_code=NULL, failure_message=NULL, failure_sqlstate=NULL,
			    finished_at=coalesce(finished_at, clock_timestamp())
			WHERE id=$1 AND phase <> 'finished'`, executionID, revertedBatches, revertedRows); err != nil {
			return err
		}
		if rootID != executionID {
			_, _ = tx.Exec(ctx, `UPDATE databrain.migration_executions
				SET phase='finished', outcome='failed',
				    reconciliation_pending=false,
				    failure_code='COMPENSATED', failure_message='All committed batches were reverted by approved compensation.',
				    finished_at=coalesce(finished_at, clock_timestamp())
				WHERE id=$1 AND phase <> 'finished'`, rootID)
		}
		if attemptNumber > 0 {
			_, _ = tx.Exec(ctx, `UPDATE databrain.migration_worker_attempts
				SET state='succeeded', finished_at=clock_timestamp()
				WHERE execution_id=$1 AND attempt_number=$2 AND state='running'`, executionID, attemptNumber)
		}
		_, err := tx.Exec(ctx, `UPDATE databrain.migration_streams
			SET active_execution_id=NULL WHERE target_id=$1 AND active_execution_id IN ($2, $3)`, targetID, executionID, rootID)
		return err
	})
	if err != nil {
		return err
	}
	clearJournalOwnerBestEffort(conn, rootID, ownerSecret)
	return nil
}

func (w *WorkerExecutor) resolveRootExecutionID(ctx context.Context, startID uuid.UUID) (uuid.UUID, error) {
	qCtx, cancel := context.WithTimeout(ctx, workerPlatformTimeout)
	defer cancel()
	lease, err := platformdb.Acquire(qCtx, w.pool)
	if err != nil {
		return uuid.Nil, err
	}
	defer lease.Release()
	var rootID uuid.UUID
	err = lease.QueryRow(qCtx, `
		WITH RECURSIVE chain AS (
			SELECT id, source_execution_id, 0 AS depth
			  FROM databrain.migration_executions
			 WHERE id = $1
			UNION ALL
			SELECT e.id, e.source_execution_id, c.depth + 1
			  FROM databrain.migration_executions e
			  JOIN chain c ON e.id = c.source_execution_id
			 WHERE c.depth < 32
		)
		SELECT id FROM chain ORDER BY depth DESC LIMIT 1`, startID).Scan(&rootID)
	if err != nil {
		return uuid.Nil, err
	}
	return rootID, nil
}
