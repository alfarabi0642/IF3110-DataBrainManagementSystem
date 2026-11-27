package migration

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerGuard adalah kontrak revalidasi domain sebelum worker menjalankan SQL target.
type WorkerGuard func(context.Context, execstore.ApprovedOperation) error

// RevalidateOperation memeriksa ulang izin aktor terkini (tanpa sesi browser), target aktif,
// konfigurasi target, generasi policy/approval, posisi head, dan integritas plan hash setelah
// worker memegang lock target. Transaksi platform singkat ini selalu dilepas sebelum SQL target.
func RevalidateOperation(ctx context.Context, pool *pgxpool.Pool, op execstore.ApprovedOperation) error {
	if pool == nil {
		return draftFailure(problem.PlatformUnavailable)
	}
	if op.TargetID == uuid.Nil || op.RequestID == uuid.Nil || op.DefinitionID == uuid.Nil ||
		op.ActorID == uuid.Nil || op.Revision < 1 || op.TargetConfigRevision < 1 {
		return draftFailure(problem.ValidationFailed)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, pool)
	if err != nil {
		return draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return draftFailure(problem.PlatformUnavailable)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSecurityGuard(ctx, tx); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.migration_streams WHERE target_id=$1 FOR SHARE`, op.TargetID).Scan(new(int)); err != nil {
		return draftFailure(problem.PlatformUnavailable)
	}
	var targetLocked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM databrain.database_targets WHERE id=$1 FOR SHARE`, op.TargetID).Scan(&targetLocked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return draftFailure(problem.DatabaseNotFound)
		}
		return draftFailure(problem.PlatformUnavailable)
	}
	if _, err := access.RequireActorDatabaseIn(ctx, tx, op.ActorID, op.TargetID, access.MigrationExecute); err != nil {
		return err
	}
	request, err := loadRequest(ctx, tx, op.TargetID, op.RequestID)
	if err != nil {
		return err
	}
	if request.State != "consumed" || request.ExecutionID == nil || request.JobID == nil {
		return draftFailure(problem.ReviewRequestNotApproved)
	}
	if request.ActorID != op.ActorID || request.DefinitionID != op.DefinitionID ||
		request.Revision != op.Revision || int64(request.ConfigRevision) != op.TargetConfigRevision ||
		request.Direction != op.Direction || request.Mode != op.Mode ||
		!uuidPtrEqual(request.ExpectedHeadID, op.ExpectedHeadID) {
		return draftFailure(problem.ValidationFailed)
	}
	if len(request.Groups) == 0 {
		return draftFailure(problem.ApprovalConfigurationIncomplete)
	}
	for _, group := range request.Groups {
		if group.Decision == nil {
			return draftFailure(problem.ReviewRequestNotApproved)
		}
	}
	rebuilt := execstore.ApprovedOperation{
		RequestID:            request.ID,
		DefinitionID:         request.DefinitionID,
		TargetID:             op.TargetID,
		ActorID:              request.ActorID,
		Revision:             request.Revision,
		TargetConfigRevision: int64(request.ConfigRevision),
		Direction:            request.Direction,
		Mode:                 request.Mode,
		SQLSnapshot:          request.SQL,
		DownSnapshot:         request.DownSQL,
		ExpectedHeadID:       request.ExpectedHeadID,
		SourceExecutionID:    op.SourceExecutionID,
		Batch:                request.Batch,
	}
	if execstore.PlanHash(rebuilt) != op.PlanHash {
		return draftFailure(problem.ValidationFailed)
	}

	state, err := readTargetReviewState(ctx, tx, op.TargetID)
	if err != nil {
		return err
	}
	if !state.active {
		return draftFailure(problem.TargetConflict)
	}
	if state.configRevision != request.ConfigRevision || state.policyGeneration != request.PolicyGeneration {
		return draftFailure(problem.ReviewRequestStale)
	}
	if !state.initialized || state.headGeneration != request.HeadGeneration || !uuidPtrEqual(state.headID, request.ExpectedHeadID) {
		return draftFailure(problem.ReviewRequestStale)
	}
	return nil
}

func uuidPtrEqual(a, b *uuid.UUID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
