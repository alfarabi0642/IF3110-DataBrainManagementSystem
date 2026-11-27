package migration

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func batchSHA256Bytes(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ExecutionBatchSummary adalah proyeksi aman satu batch ter-commit (tanpa before-image).
type ExecutionBatchSummary struct {
	BatchOrdinal      int
	Direction         string
	ActionExecutionID uuid.UUID
	RowsAffected      int64
	CursorEnd         *string
	CommittedAt       time.Time
}

// SubmitBatchRequest membuat review request mode batch (v1 recipe) terikat ke konfigurasi batch
// tervalidasi. SQL up/down harus persis sama dengan template yang dihasilkan BuildBatchPlan.
func (s *Service) SubmitBatchRequest(ctx context.Context, p access.Principal, targetID, draftID uuid.UUID,
	expectedRevision int64, requestKey uuid.UUID, cfg sqlpolicy.BatchConfig) (ReviewRequest, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.MigrationWrite); err != nil {
		return ReviewRequest{}, err
	}
	if targetID == uuid.Nil || draftID == uuid.Nil || expectedRevision < 1 || requestKey == uuid.Nil {
		return ReviewRequest{}, draftFailure(problem.InvalidRequest)
	}
	plan, err := sqlpolicy.BuildBatchPlan(cfg)
	if err != nil {
		return ReviewRequest{}, err
	}
	batchJSON, err := execstore.MarshalBatchConfig(&plan.Config)
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.ValidationFailed)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.store.pool)
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSecurityGuard(ctx, tx); err != nil {
		return ReviewRequest{}, err
	}
	var existingID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM databrain.migration_requests
	    WHERE target_id=$1 AND actor_id=$2 AND request_key=$3`, targetID, p.UserID, requestKey).Scan(&existingID)
	if err == nil {
		request, err := loadRequest(ctx, tx, targetID, existingID)
		if err != nil {
			return ReviewRequest{}, err
		}
		if request.DraftID != draftID || request.Revision != expectedRevision || request.Mode != execstore.ModeBatch {
			return ReviewRequest{}, draftFailure(problem.IdempotencyConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		return request, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}

	var busy bool
	if err := tx.QueryRow(ctx, `SELECT active_attempt_id IS NOT NULL OR active_execution_id IS NOT NULL
	    FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(&busy); err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if busy {
		return ReviewRequest{}, draftFailure(problem.TargetBusy)
	}
	var targetLocked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM databrain.database_targets WHERE id=$1 FOR SHARE`, targetID).Scan(&targetLocked); err != nil {
		return ReviewRequest{}, draftFailure(problem.DatabaseNotFound)
	}
	state, err := readTargetReviewState(ctx, tx, targetID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if !state.active {
		return ReviewRequest{}, draftFailure(problem.TargetConflict)
	}
	if !state.initialized {
		return ReviewRequest{}, draftFailure(problem.ReviewHeadUnavailable)
	}
	groups, err := access.RequiredGroups(ctx, tx, targetID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if len(groups) == 0 {
		return ReviewRequest{}, draftFailure(problem.ApprovalConfigurationIncomplete)
	}
	for _, group := range groups {
		if group.IncompleteReason != "" {
			return ReviewRequest{}, draftFailure(problem.ApprovalConfigurationIncomplete)
		}
	}

	// Buat definition immutable dari draft dan pastikan draft membawa template SQL batch yang sah.
	var draftTitle, draftSQL, draftDownSQL string
	var draftIrreversible bool
	if err := tx.QueryRow(ctx, `SELECT title, sql, down_sql, irreversible FROM databrain.migration_drafts
	    WHERE target_id=$1 AND id=$2 AND revision=$3 FOR UPDATE`, targetID, draftID, expectedRevision).
		Scan(&draftTitle, &draftSQL, &draftDownSQL, &draftIrreversible); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewRequest{}, draftFailure(problem.RevisionConflict)
		}
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if draftIrreversible || draftSQL != plan.UpSQL || draftDownSQL != plan.DownSQL {
		return ReviewRequest{}, draftFailure(problem.SQLUnsupported)
	}
	// Simpan definisi batch immutable dengan template up/down resmi (VerifyBatchPlan).
	defID := uuid.New()
	upHash := batchSHA256Bytes(plan.UpSQL)
	downHash := batchSHA256Bytes(plan.DownSQL)
	var existingDefID uuid.UUID
	var existingUp, existingDown string
	err = tx.QueryRow(ctx, `SELECT id, up_sql, down_sql FROM databrain.migration_definitions
	    WHERE target_id=$1 AND draft_id=$2 AND revision=$3`, targetID, draftID, expectedRevision).Scan(&existingDefID, &existingUp, &existingDown)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_definitions
		    (id,target_id,draft_id,revision,up_sql,down_sql,irreversible,irreversible_reason,up_sha256,down_sha256,source)
		    VALUES ($1,$2,$3,$4,$5,$6,false,NULL,$7,$8,'submission')`,
			defID, targetID, draftID, expectedRevision, plan.UpSQL, plan.DownSQL, upHash, downHash); err != nil {
			return ReviewRequest{}, requestStoreError(err)
		}
	} else if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	} else {
		if existingUp != plan.UpSQL || existingDown != plan.DownSQL {
			return ReviewRequest{}, draftFailure(problem.SQLUnsupported)
		}
		defID = existingDefID
	}

	var actorName string
	if err := tx.QueryRow(ctx, `SELECT username FROM databrain.users WHERE id=$1`, p.UserID).Scan(&actorName); err != nil {
		return ReviewRequest{}, draftFailure(problem.InvalidRequest)
	}
	requestID := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_requests
	    (id,target_id,draft_id,definition_id,actor_id,actor_username,revision,target_config_revision,
	     direction,mode,expected_head_id,head_generation,policy_generation,request_key,state,batch_config)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'apply','batch',$9,$10,$11,$12,'pending_review',$13)`,
		requestID, targetID, draftID, defID, p.UserID, actorName, expectedRevision,
		state.configRevision, state.headID, state.headGeneration, state.policyGeneration, requestKey, batchJSON); err != nil {
		return ReviewRequest{}, requestStoreError(err)
	}
	for _, group := range groups {
		if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_request_groups
		    (target_id,request_id,group_id,group_name,access_level,leader_id)
		    VALUES ($1,$2,$3,$4,$5,$6)`, targetID, requestID, group.GroupID, group.GroupName,
			group.Access, group.LeaderID); err != nil {
			return ReviewRequest{}, requestStoreError(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	return loadRequest(ctx, s.store.pool, targetID, requestID)
}

// SubmitRecoveryRequest membuat review request (resume atau compensate) yang mengikat eksekusi
// sumber partial/partially_reverted dan tetap melalui approval penuh semua leader grup.
func (s *Service) SubmitRecoveryRequest(ctx context.Context, p access.Principal, targetID, sourceExecutionID, requestKey uuid.UUID, direction string) (ReviewRequest, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.MigrationWrite); err != nil {
		return ReviewRequest{}, err
	}
	if targetID == uuid.Nil || sourceExecutionID == uuid.Nil || requestKey == uuid.Nil ||
		(direction != execstore.DirectionResume && direction != execstore.DirectionCompensate) {
		return ReviewRequest{}, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.store.pool)
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSecurityGuard(ctx, tx); err != nil {
		return ReviewRequest{}, err
	}
	var existingID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM databrain.migration_requests
	    WHERE target_id=$1 AND actor_id=$2 AND request_key=$3`, targetID, p.UserID, requestKey).Scan(&existingID)
	if err == nil {
		request, err := loadRequest(ctx, tx, targetID, existingID)
		if err != nil {
			return ReviewRequest{}, err
		}
		if request.Direction != direction || !uuidPtrEqual(request.SourceExecutionID, &sourceExecutionID) {
			return ReviewRequest{}, draftFailure(problem.IdempotencyConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		return request, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}

	// Pastikan gate target masih dipegang persis oleh eksekusi sumber (tidak ada attempt aktif lain
	// dan sumber belum dilanjutkan/diselesaikan oleh eksekusi lain).
	var activeAttemptID, activeExecutionID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT active_attempt_id, active_execution_id
	    FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).
		Scan(&activeAttemptID, &activeExecutionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewRequest{}, draftFailure(problem.DatabaseNotFound)
		}
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if activeAttemptID != nil || activeExecutionID == nil || *activeExecutionID != sourceExecutionID {
		return ReviewRequest{}, draftFailure(problem.TargetConflict)
	}

	// Muat eksekusi sumber immutable dan pastikan phase/outcome masih blocked partial/partially_reverted.
	var (
		srcDefID    uuid.UUID
		srcRevision int64
		srcMode     string
		srcPhase    string
		srcOutcome  string
		srcBatchRaw []byte
		srcDownSQL  string
	)
	err = tx.QueryRow(ctx, `SELECT definition_id, revision, mode, phase, coalesce(outcome,''), batch_config, down_snapshot
	    FROM databrain.migration_executions WHERE target_id=$1 AND id=$2 FOR UPDATE`,
		targetID, sourceExecutionID).Scan(&srcDefID, &srcRevision, &srcMode, &srcPhase, &srcOutcome, &srcBatchRaw, &srcDownSQL)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.ExecutionNotFound)
	}
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if srcMode != execstore.ModeBatch || len(srcBatchRaw) == 0 || srcPhase != "blocked" {
		return ReviewRequest{}, draftFailure(problem.TargetConflict)
	}
	if direction == execstore.DirectionResume && srcOutcome != "partial" {
		return ReviewRequest{}, draftFailure(problem.TargetConflict)
	}
	if direction == execstore.DirectionCompensate && srcOutcome != "partial" && srcOutcome != "partially_reverted" {
		return ReviewRequest{}, draftFailure(problem.TargetConflict)
	}
	if direction == execstore.DirectionCompensate && srcDownSQL == "" {
		return ReviewRequest{}, draftFailure(problem.ValidationFailed)
	}

	var draftID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT draft_id FROM databrain.migration_definitions WHERE target_id=$1 AND id=$2`,
		targetID, srcDefID).Scan(&draftID); err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}

	state, err := readTargetReviewState(ctx, tx, targetID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if !state.active || !state.initialized {
		return ReviewRequest{}, draftFailure(problem.TargetConflict)
	}
	groups, err := access.RequiredGroups(ctx, tx, targetID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if len(groups) == 0 {
		return ReviewRequest{}, draftFailure(problem.ApprovalConfigurationIncomplete)
	}
	for _, group := range groups {
		if group.IncompleteReason != "" {
			return ReviewRequest{}, draftFailure(problem.ApprovalConfigurationIncomplete)
		}
	}

	var actorName string
	if err := tx.QueryRow(ctx, `SELECT username FROM databrain.users WHERE id=$1`, p.UserID).Scan(&actorName); err != nil {
		return ReviewRequest{}, draftFailure(problem.InvalidRequest)
	}
	requestID := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_requests
	    (id,target_id,draft_id,definition_id,actor_id,actor_username,revision,target_config_revision,
	     direction,mode,source_execution_id,expected_head_id,head_generation,policy_generation,request_key,state,batch_config)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'batch',$10,$11,$12,$13,$14,'pending_review',$15)`,
		requestID, targetID, draftID, srcDefID, p.UserID, actorName, srcRevision,
		state.configRevision, direction, sourceExecutionID, state.headID, state.headGeneration,
		state.policyGeneration, requestKey, srcBatchRaw); err != nil {
		return ReviewRequest{}, requestStoreError(err)
	}
	for _, group := range groups {
		if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_request_groups
		    (target_id,request_id,group_id,group_name,access_level,leader_id)
		    VALUES ($1,$2,$3,$4,$5,$6)`, targetID, requestID, group.GroupID, group.GroupName,
			group.Access, group.LeaderID); err != nil {
			return ReviewRequest{}, requestStoreError(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	return loadRequest(ctx, s.store.pool, targetID, requestID)
}

// ListExecutionBatches membaca ringkasan batch ter-commit untuk satu eksekusi (tanpa before-image).
func (s *Service) ListExecutionBatches(ctx context.Context, p access.Principal, targetID, executionID uuid.UUID, limit, offset int) ([]ExecutionBatchSummary, bool, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.DatabaseView); err != nil {
		return nil, false, err
	}
	if targetID == uuid.Nil || executionID == uuid.Nil || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var exists bool
	if err := s.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM databrain.migration_executions WHERE target_id=$1 AND id=$2)`, targetID, executionID).Scan(&exists); err != nil {
		return nil, false, draftFailure(problem.PlatformUnavailable)
	}
	if !exists {
		return nil, false, draftFailure(problem.ExecutionNotFound)
	}
	rows, err := s.store.pool.Query(ctx, `SELECT batch_ordinal, direction, action_execution_id, rows_affected, cursor_end, committed_at
	    FROM databrain.migration_execution_batches
	    WHERE target_id=$1 AND (execution_id=$2 OR action_execution_id=$2)
	    ORDER BY batch_ordinal ASC, direction ASC LIMIT $3 OFFSET $4`, targetID, executionID, limit+1, offset)
	if err != nil {
		return nil, false, draftFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]ExecutionBatchSummary, 0, limit)
	for rows.Next() {
		var item ExecutionBatchSummary
		if err := rows.Scan(&item.BatchOrdinal, &item.Direction, &item.ActionExecutionID, &item.RowsAffected, &item.CursorEnd, &item.CommittedAt); err != nil {
			return nil, false, draftFailure(problem.PlatformUnavailable)
		}
		item.CommittedAt = item.CommittedAt.UTC()
		items = append(items, item)
	}
	if rows.Err() != nil {
		return nil, false, draftFailure(problem.PlatformUnavailable)
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}
