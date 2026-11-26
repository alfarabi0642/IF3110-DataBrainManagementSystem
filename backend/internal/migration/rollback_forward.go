package migration

import (
	"context"
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

// SubmitRollbackRequest membuat review request baru untuk membatalkan active head terakhir
// menggunakan SQL down asli yang immutable. Tidak pernah menerima SQL pengganti.
func (s *Service) SubmitRollbackRequest(ctx context.Context, p access.Principal, targetID, definitionID, sourceExecutionID uuid.UUID,
	expectedHeadGeneration int64, requestKey uuid.UUID) (ReviewRequest, error) {
	return s.submitHeadMotionRequest(ctx, p, targetID, definitionID, sourceExecutionID, expectedHeadGeneration, requestKey, execstore.DirectionRollback)
}

// SubmitForwardRequest membuat review request baru untuk menjalankan ulang SQL up asli dari entri
// teratas migration_redo_entries secara linear. Tidak pernah menerima SQL pengganti.
func (s *Service) SubmitForwardRequest(ctx context.Context, p access.Principal, targetID, definitionID, sourceExecutionID uuid.UUID,
	expectedHeadGeneration int64, requestKey uuid.UUID) (ReviewRequest, error) {
	return s.submitHeadMotionRequest(ctx, p, targetID, definitionID, sourceExecutionID, expectedHeadGeneration, requestKey, execstore.DirectionForward)
}

func (s *Service) submitHeadMotionRequest(ctx context.Context, p access.Principal, targetID, definitionID, sourceExecutionID uuid.UUID,
	expectedHeadGeneration int64, requestKey uuid.UUID, direction string) (ReviewRequest, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.MigrationWrite); err != nil {
		return ReviewRequest{}, err
	}
	if targetID == uuid.Nil || definitionID == uuid.Nil || sourceExecutionID == uuid.Nil ||
		expectedHeadGeneration < 1 || requestKey == uuid.Nil {
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
		if request.Direction != direction || request.DefinitionID != definitionID ||
			!uuidPtrEqual(request.SourceExecutionID, &sourceExecutionID) {
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
	if _, err := access.RequireDatabaseIn(ctx, tx, p, targetID, access.MigrationWrite); err != nil {
		return ReviewRequest{}, err
	}
	if err := lockPrincipal(ctx, tx, p); err != nil {
		return ReviewRequest{}, err
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
	if state.headGeneration != expectedHeadGeneration {
		return ReviewRequest{}, draftFailure(problem.RevisionConflict)
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

	// Muat definisi asli yang immutable; SQL tidak pernah diganti oleh client.
	var (
		draftID      uuid.UUID
		revision     int64
		upSQL        string
		downSQL      string
		irreversible bool
	)
	err = tx.QueryRow(ctx, `SELECT draft_id, revision, up_sql, down_sql, irreversible
	    FROM databrain.migration_definitions WHERE target_id=$1 AND id=$2`, targetID, definitionID).Scan(
		&draftID, &revision, &upSQL, &downSQL, &irreversible,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.DraftNotFound)
	}
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}

	switch direction {
	case execstore.DirectionRollback:
		if state.headID == nil || *state.headID != sourceExecutionID {
			return ReviewRequest{}, draftFailure(problem.RevisionConflict)
		}
		var activeDefID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT definition_id FROM databrain.migration_committed_events
		    WHERE target_id=$1 AND execution_id=$2`, targetID, sourceExecutionID).Scan(&activeDefID); err != nil {
			return ReviewRequest{}, draftFailure(problem.RevisionConflict)
		}
		if activeDefID != definitionID {
			return ReviewRequest{}, draftFailure(problem.ValidationFailed)
		}
		if irreversible || downSQL == "" {
			return ReviewRequest{}, draftFailure(problem.ValidationFailed)
		}
		if _, err := sqlpolicy.ValidateSQL(downSQL); err != nil {
			return ReviewRequest{}, err
		}

	case execstore.DirectionForward:
		var topSource, topDef uuid.UUID
		err := tx.QueryRow(ctx, `SELECT source_execution_id, definition_id
		    FROM databrain.migration_redo_entries WHERE target_id=$1 ORDER BY position DESC LIMIT 1`, targetID).Scan(&topSource, &topDef)
		if errors.Is(err, pgx.ErrNoRows) || topSource != sourceExecutionID || topDef != definitionID {
			return ReviewRequest{}, draftFailure(problem.RevisionConflict)
		}
		if err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		if _, err := sqlpolicy.ValidateSQL(upSQL); err != nil {
			return ReviewRequest{}, err
		}

	default:
		return ReviewRequest{}, draftFailure(problem.InvalidRequest)
	}

	var actorName string
	if err := tx.QueryRow(ctx, `SELECT username FROM databrain.users WHERE id=$1`, p.UserID).Scan(&actorName); err != nil {
		return ReviewRequest{}, draftFailure(problem.InvalidRequest)
	}
	requestID := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_requests
	    (id,target_id,draft_id,definition_id,actor_id,actor_username,revision,target_config_revision,
	     direction,mode,source_execution_id,expected_head_id,head_generation,policy_generation,request_key,state)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'normal',$10,$11,$12,$13,$14,'pending_review')`,
		requestID, targetID, draftID, definitionID, p.UserID, actorName, revision,
		state.configRevision, direction, sourceExecutionID, state.headID, state.headGeneration,
		state.policyGeneration, requestKey); err != nil {
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
