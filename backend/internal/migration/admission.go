package migration

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type Admission struct {
	RequestID   uuid.UUID
	ExecutionID uuid.UUID
	JobID       int64
	Replayed    bool
}

// AcceptRequest mengonsumsi request yang sudah approved menjadi tepat satu execution/job pada transaksi
// yang sama. Hanya initiating editor yang dapat memanggilnya; SQL target tidak pernah dijalankan di sini.
func (s *Service) AcceptRequest(ctx context.Context, p access.Principal, targetID, requestID uuid.UUID, requestKey uuid.UUID) (adm Admission, retErr error) {
	ctx, span := telemetry.Tracer().Start(ctx, "migration.admit")
	span.SetAttributes(
		attribute.String(telemetry.FieldDatabase, targetID.String()),
		attribute.String(telemetry.FieldRequestID, requestID.String()),
		attribute.String(telemetry.FieldActor, p.UserID.String()),
	)
	ctx = telemetry.WithLogAttrs(ctx,
		telemetry.FieldDatabase, targetID.String(),
		telemetry.FieldRequestID, requestID.String(),
		telemetry.FieldActor, p.UserID.String(),
	)
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, telemetry.SanitizeError(retErr).Code)
		}
		span.End()
	}()

	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.MigrationWrite); err != nil {
		return Admission{}, err
	}
	if targetID == uuid.Nil || requestID == uuid.Nil || requestKey == uuid.Nil {
		return Admission{}, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.store.pool)
	if err != nil {
		return Admission{}, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Admission{}, draftFailure(problem.PlatformUnavailable)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockSecurityGuard(ctx, tx); err != nil {
		return Admission{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(new(int)); err != nil {
		return Admission{}, draftFailure(problem.PlatformUnavailable)
	}
	var targetLocked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM databrain.database_targets WHERE id=$1 FOR SHARE`, targetID).Scan(&targetLocked); err != nil {
		return Admission{}, draftFailure(problem.DatabaseNotFound)
	}
	var lockedID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM databrain.migration_requests WHERE target_id=$1 AND id=$2 FOR UPDATE`,
		targetID, requestID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admission{}, draftFailure(problem.ReviewRequestNotFound)
	}
	if err != nil {
		return Admission{}, draftFailure(problem.PlatformUnavailable)
	}
	if _, err := access.RequireDatabaseIn(ctx, tx, p, targetID, access.MigrationWrite); err != nil {
		return Admission{}, err
	}
	if err := lockPrincipal(ctx, tx, p); err != nil {
		return Admission{}, err
	}
	request, err := loadRequest(ctx, tx, targetID, requestID)
	if err != nil {
		return Admission{}, err
	}
	if request.ActorID != p.UserID {
		return Admission{}, draftFailure(problem.DatabaseAccessDenied)
	}
	if request.State == "consumed" {
		if request.ConsumedRequestKey != nil && *request.ConsumedRequestKey == requestKey &&
			request.ExecutionID != nil && request.JobID != nil {
			if err := tx.Commit(ctx); err != nil {
				return Admission{}, draftFailure(problem.PlatformUnavailable)
			}
			return Admission{RequestID: request.ID, ExecutionID: *request.ExecutionID, JobID: *request.JobID, Replayed: true}, nil
		}
		return Admission{}, draftFailure(problem.IdempotencyConflict)
	}
	if request.State == "stale" || request.StaleReason != "" {
		reason := request.StaleReason
		if reason == "" {
			reason = "policy_changed"
		}
		if _, err := tx.Exec(ctx, `UPDATE databrain.migration_requests SET state='stale',stale_reason=$3,
		    updated_at=now() WHERE target_id=$1 AND id=$2`, targetID, requestID, reason); err != nil {
			return Admission{}, draftFailure(problem.PlatformUnavailable)
		}
		if err := tx.Commit(ctx); err != nil {
			return Admission{}, draftFailure(problem.PlatformUnavailable)
		}
		return Admission{}, draftFailure(problem.ReviewRequestStale)
	}
	if request.State != "approved" {
		return Admission{}, draftFailure(problem.ReviewRequestNotApproved)
	}
	for _, group := range request.Groups {
		if group.Decision == nil {
			return Admission{}, draftFailure(problem.ReviewRequestNotApproved)
		}
	}
	state, err := readTargetReviewState(ctx, tx, targetID)
	if err != nil {
		return Admission{}, err
	}
	if !state.active {
		return Admission{}, draftFailure(problem.TargetConflict)
	}
	if reason := state.staleReason(request.ConfigRevision, request.PolicyGeneration, request.ExpectedHeadID, request.HeadGeneration); reason != "" {
		if _, err := tx.Exec(ctx, `UPDATE databrain.migration_requests SET state='stale',stale_reason=$3,
		    updated_at=now() WHERE target_id=$1 AND id=$2`, targetID, requestID, reason); err != nil {
			return Admission{}, draftFailure(problem.PlatformUnavailable)
		}
		if err := tx.Commit(ctx); err != nil {
			return Admission{}, draftFailure(problem.PlatformUnavailable)
		}
		return Admission{}, draftFailure(problem.ReviewRequestStale)
	}
	switch request.Direction {
	case execstore.DirectionApply, execstore.DirectionRollback, execstore.DirectionForward, execstore.DirectionResume, execstore.DirectionCompensate:
	default:
		return Admission{}, draftFailure(problem.InvalidRequest)
	}
	if request.Mode != execstore.ModeNormal && request.Mode != execstore.ModeBatch {
		return Admission{}, draftFailure(problem.InvalidRequest)
	}
	op := execstore.ApprovedOperation{
		RequestID: request.ID, DefinitionID: request.DefinitionID, TargetID: targetID, ActorID: request.ActorID,
		Revision: request.Revision, TargetConfigRevision: int64(request.ConfigRevision),
		Direction: request.Direction, Mode: request.Mode, SQLSnapshot: request.SQL, DownSnapshot: request.DownSQL,
		ExpectedHeadID: request.ExpectedHeadID, SourceExecutionID: request.SourceExecutionID,
		Batch: request.Batch,
	}
	op.PlanHash = execstore.PlanHash(op)
	enqueuer, err := jobs.NewEnqueuer()
	if err != nil {
		return Admission{}, draftFailure(problem.PlatformUnavailable)
	}
	store, err := execstore.NewEnqueueStore(s.store.pool, enqueuer)
	if err != nil {
		return Admission{}, draftFailure(problem.PlatformUnavailable)
	}
	accepted, err := store.Enqueue(ctx, tx, op, requestKey)
	if err != nil {
		return Admission{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.migration_requests SET state='consumed',
	    execution_id=$3,job_id=$4,consumed_request_key=$5,consumed_at=now(),updated_at=now()
	    WHERE target_id=$1 AND id=$2`, targetID, requestID, accepted.ExecutionID, accepted.JobID, requestKey); err != nil {
		return Admission{}, requestStoreError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		receipt, lookupErr := store.Receipt(ctx, targetID, op.ActorID, requestKey)
		if lookupErr == nil && receipt != nil {
			var storedRequest uuid.UUID
			var storedHash string
			if s.store.pool.QueryRow(ctx, `SELECT request_id,plan_hash FROM databrain.migration_executions WHERE id=$1`,
				receipt.ExecutionID).Scan(&storedRequest, &storedHash) == nil &&
				storedRequest == op.RequestID && storedHash == op.PlanHash {
				return Admission{RequestID: request.ID, ExecutionID: receipt.ExecutionID, JobID: receipt.JobID, Replayed: true}, nil
			}
		}
		return Admission{}, draftFailure(problem.PlatformUnavailable)
	}
	return Admission{RequestID: request.ID, ExecutionID: accepted.ExecutionID, JobID: accepted.JobID, Replayed: accepted.Replayed}, nil
}
