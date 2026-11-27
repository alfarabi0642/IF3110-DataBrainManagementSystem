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
	"github.com/jackc/pgx/v5/pgtype"
)

type RequestSummary struct {
	ID, TargetID, DraftID, DefinitionID uuid.UUID
	ActorID                             uuid.UUID
	Title, ActorUsername                string
	Direction, Mode, State, StaleReason string
	Revision                            int64
	CreatedAt, UpdatedAt                time.Time
}

type RequestDecision struct {
	ActorID, LeaderID uuid.UUID
	ActorUsername     string
	CreatedAt         time.Time
}

type RequestGroup struct {
	GroupID        uuid.UUID
	GroupName      string
	Access         access.Level
	LeaderID       uuid.UUID
	LeaderUsername string
	Decision       *RequestDecision
	Eligible       bool
}

type ReviewRequest struct {
	RequestSummary
	ConfigRevision     int
	PolicyGeneration   int64
	HeadGeneration     int64
	ExpectedHeadID     *uuid.UUID
	SourceExecutionID  *uuid.UUID
	SQL, DownSQL       string
	Irreversible       bool
	IrreversibleReason string
	Groups             []RequestGroup
	EligibleGroupIDs   []uuid.UUID
	ExecutionID        *uuid.UUID
	JobID              *int64
	ConsumedRequestKey *uuid.UUID
	ConsumedAt         *time.Time
	Batch              *sqlpolicy.BatchConfig
}

type targetReviewState struct {
	configRevision   int
	active           bool
	policyGeneration int64
	initialized      bool
	headID           *uuid.UUID
	headGeneration   int64
}

func readTargetReviewState(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, targetID uuid.UUID) (targetReviewState, error) {
	var state targetReviewState
	var head string
	err := q.QueryRow(ctx, `SELECT t.config_revision,t.is_active,
	    coalesce((SELECT generation FROM databrain.migration_policy_generations WHERE target_id=t.id),1),
	    coalesce(h.initialized,false),coalesce(h.active_execution_id::text,''),coalesce(h.generation,1)
	    FROM databrain.database_targets t
	    LEFT JOIN databrain.migration_target_heads h ON h.target_id=t.id
	    WHERE t.id=$1`, targetID).
		Scan(&state.configRevision, &state.active, &state.policyGeneration, &state.initialized, &head, &state.headGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return targetReviewState{}, draftFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return targetReviewState{}, draftFailure(problem.PlatformUnavailable)
	}
	if head != "" {
		id, err := uuid.Parse(head)
		if err != nil {
			return targetReviewState{}, draftFailure(problem.PlatformUnavailable)
		}
		state.headID = &id
	}
	return state, nil
}

func (s targetReviewState) staleReason(configRevision int, policyGeneration int64, expectedHeadID *uuid.UUID, headGeneration int64) string {
	switch {
	case !s.active:
		return "target_inactive"
	case s.configRevision != configRevision:
		return "configuration_changed"
	case s.policyGeneration != policyGeneration:
		return "policy_changed"
	case !s.initialized || s.headGeneration != headGeneration || !uuidPtrEqual(s.headID, expectedHeadID):
		return "head_changed"
	default:
		return ""
	}
}

func requestStoreError(err error) error {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		switch pgErr.SQLState() {
		case "23505":
			return draftFailure(problem.IdempotencyConflict)
		case "23503", "23514", "22001", "22021", "22003":
			return draftFailure(problem.ValidationFailed)
		}
	}
	return draftFailure(problem.PlatformUnavailable)
}

func (s *Service) SubmitRequest(ctx context.Context, p access.Principal, targetID, draftID uuid.UUID, expectedRevision int64, requestKey uuid.UUID) (ReviewRequest, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.MigrationWrite); err != nil {
		return ReviewRequest{}, err
	}
	if targetID == uuid.Nil || draftID == uuid.Nil || expectedRevision < 1 || requestKey == uuid.Nil {
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
		if request.DraftID != draftID || request.Revision != expectedRevision {
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
	err = tx.QueryRow(ctx, `SELECT active_attempt_id IS NOT NULL OR active_execution_id IS NOT NULL
	    FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(&busy)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if err != nil {
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
	definition, err := snapshotDefinitionTx(ctx, tx, targetID, draftID, expectedRevision)
	if err != nil {
		return ReviewRequest{}, err
	}
	var actorName string
	if err := tx.QueryRow(ctx, `SELECT username FROM databrain.users WHERE id=$1`, p.UserID).Scan(&actorName); err != nil {
		return ReviewRequest{}, draftFailure(problem.InvalidRequest)
	}
	requestID := uuid.New()
	var inserted uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO databrain.migration_requests
	    (id,target_id,draft_id,definition_id,actor_id,actor_username,revision,target_config_revision,
	     direction,mode,expected_head_id,head_generation,policy_generation,request_key,state)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'apply','normal',$9,$10,$11,$12,'pending_review')
	    ON CONFLICT (target_id,actor_id,request_key) DO NOTHING RETURNING id`,
		requestID, targetID, draftID, definition.ID, p.UserID, actorName, definition.Revision,
		state.configRevision, state.headID, state.headGeneration, state.policyGeneration, requestKey).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		var racedID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM databrain.migration_requests
		    WHERE target_id=$1 AND actor_id=$2 AND request_key=$3`, targetID, p.UserID, requestKey).Scan(&racedID); err != nil {
			return ReviewRequest{}, draftFailure(problem.IdempotencyConflict)
		}
		request, err := loadRequest(ctx, tx, targetID, racedID)
		if err != nil {
			return ReviewRequest{}, err
		}
		if request.DraftID != draftID || request.Revision != expectedRevision {
			return ReviewRequest{}, draftFailure(problem.IdempotencyConflict)
		}
		return request, nil
	}
	if err != nil {
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

func (s *Service) ListRequests(ctx context.Context, p access.Principal, targetID uuid.UUID, limit, offset int) ([]RequestSummary, bool, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.DatabaseView); err != nil {
		return nil, false, err
	}
	if targetID == uuid.Nil || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	state, err := readTargetReviewState(ctx, s.store.pool, targetID)
	if err != nil {
		return nil, false, err
	}
	rows, err := s.store.pool.Query(ctx, `SELECT r.id::text,r.target_id::text,r.draft_id::text,r.definition_id::text,
	    r.actor_id::text,r.actor_username,d.title,r.revision,r.direction,r.mode,r.state,coalesce(r.stale_reason,''),
	    r.created_at,r.updated_at,r.target_config_revision,r.policy_generation,
	    coalesce(r.expected_head_id::text,''),r.head_generation,
	    (SELECT count(*) FROM databrain.migration_request_groups g
	     WHERE g.target_id=r.target_id AND g.request_id=r.id),
	    (SELECT count(*) FROM databrain.migration_request_decisions x
	     WHERE x.target_id=r.target_id AND x.request_id=r.id)
	    FROM databrain.migration_requests r
	    JOIN databrain.migration_drafts d ON d.target_id=r.target_id AND d.id=r.draft_id
	    WHERE r.target_id=$1 ORDER BY r.created_at DESC,r.id LIMIT $2 OFFSET $3`, targetID, limit+1, offset)
	if err != nil {
		return nil, false, draftFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]RequestSummary, 0, limit)
	for rows.Next() {
		var item RequestSummary
		var reviewState, stale, expectedHeadStr string
		var configRevision int
		var policyGeneration, headGeneration int64
		var required, decided int
		if err := rows.Scan(&item.ID, &item.TargetID, &item.DraftID, &item.DefinitionID, &item.ActorID,
			&item.ActorUsername, &item.Title, &item.Revision, &item.Direction, &item.Mode, &reviewState, &stale,
			&item.CreatedAt, &item.UpdatedAt, &configRevision, &policyGeneration,
			&expectedHeadStr, &headGeneration, &required, &decided); err != nil {
			return nil, false, draftFailure(problem.PlatformUnavailable)
		}
		var expectedHeadID *uuid.UUID
		if expectedHeadStr != "" {
			id, err := uuid.Parse(expectedHeadStr)
			if err != nil {
				return nil, false, draftFailure(problem.PlatformUnavailable)
			}
			expectedHeadID = &id
		}
		item.CreatedAt, item.UpdatedAt = item.CreatedAt.UTC(), item.UpdatedAt.UTC()
		item.StaleReason = stale
		switch {
		case reviewState == "consumed":
			item.State = "consumed"
		case reviewState == "stale" || state.staleReason(configRevision, policyGeneration, expectedHeadID, headGeneration) != "":
			item.State = "stale"
			if reason := state.staleReason(configRevision, policyGeneration, expectedHeadID, headGeneration); reason != "" {
				item.StaleReason = reason
			}
		case decided < required:
			item.State = "pending_review"
			item.StaleReason = ""
		default:
			item.State = "approved"
			item.StaleReason = ""
		}
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

func (s *Service) GetRequest(ctx context.Context, p access.Principal, targetID, requestID uuid.UUID) (ReviewRequest, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.DatabaseView); err != nil {
		return ReviewRequest{}, err
	}
	if targetID == uuid.Nil || requestID == uuid.Nil {
		return ReviewRequest{}, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := loadRequest(ctx, s.store.pool, targetID, requestID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if err := s.markEligible(ctx, s.store.pool, p, &request); err != nil {
		return ReviewRequest{}, err
	}
	return request, nil
}

func (s *Service) ApproveRequest(ctx context.Context, p access.Principal, targetID, requestID, groupID uuid.UUID) (ReviewRequest, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.DatabaseView); err != nil {
		return ReviewRequest{}, err
	}
	if targetID == uuid.Nil || requestID == uuid.Nil || groupID == uuid.Nil {
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
	if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(new(int)); err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	var targetLocked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM databrain.database_targets WHERE id=$1 FOR SHARE`, targetID).Scan(&targetLocked); err != nil {
		return ReviewRequest{}, draftFailure(problem.DatabaseNotFound)
	}
	var lockedID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM databrain.migration_requests WHERE target_id=$1 AND id=$2 FOR UPDATE`,
		targetID, requestID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.ReviewRequestNotFound)
	}
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if _, err := access.RequireDatabaseIn(ctx, tx, p, targetID, access.DatabaseView); err != nil {
		return ReviewRequest{}, err
	}
	if err := lockPrincipal(ctx, tx, p); err != nil {
		return ReviewRequest{}, err
	}
	request, err := loadRequest(ctx, tx, targetID, requestID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if request.State == "consumed" {
		return ReviewRequest{}, draftFailure(problem.ReviewRequestStale)
	}
	if request.StaleReason != "" || request.State == "stale" {
		reason := request.StaleReason
		if reason == "" {
			reason = "policy_changed"
		}
		if _, err := tx.Exec(ctx, `UPDATE databrain.migration_requests SET state='stale',stale_reason=$3,
		    updated_at=now() WHERE target_id=$1 AND id=$2`, targetID, requestID, reason); err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		if err := tx.Commit(ctx); err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		return ReviewRequest{}, draftFailure(problem.ReviewRequestStale)
	}
	var leader pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT leader_id FROM databrain.migration_request_groups
	    WHERE target_id=$1 AND request_id=$2 AND group_id=$3`, targetID, requestID, groupID).Scan(&leader)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.DatabaseAccessDenied)
	}
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	var currentLeader pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT leader_id FROM databrain.groups WHERE id=$1 AND is_active`, groupID).Scan(&currentLeader)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!currentLeader.Valid || uuid.UUID(currentLeader.Bytes) != p.UserID) {
		return ReviewRequest{}, draftFailure(problem.DatabaseAccessDenied)
	}
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	var existingActor pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT actor_id FROM databrain.migration_request_decisions
	    WHERE target_id=$1 AND request_id=$2 AND group_id=$3`, targetID, requestID, groupID).Scan(&existingActor)
	if err == nil {
		if uuid.UUID(existingActor.Bytes) != p.UserID {
			return ReviewRequest{}, draftFailure(problem.DatabaseAccessDenied)
		}
		if err := tx.Commit(ctx); err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		request, err := loadRequest(ctx, s.store.pool, targetID, requestID)
		if err != nil {
			return ReviewRequest{}, err
		}
		if err := s.markEligible(ctx, s.store.pool, p, &request); err != nil {
			return ReviewRequest{}, err
		}
		return request, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_request_decisions
	    (target_id,request_id,group_id,actor_id,leader_id) VALUES ($1,$2,$3,$4,$4)`,
		targetID, requestID, groupID, p.UserID); err != nil {
		return ReviewRequest{}, requestStoreError(err)
	}
	var required, decided int
	if err := tx.QueryRow(ctx, `SELECT
	    (SELECT count(*) FROM databrain.migration_request_groups WHERE target_id=$1 AND request_id=$2),
	    (SELECT count(*) FROM databrain.migration_request_decisions WHERE target_id=$1 AND request_id=$2)`,
		targetID, requestID).Scan(&required, &decided); err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	nextState := "pending_review"
	if decided >= required {
		nextState = "approved"
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.migration_requests SET state=$3,stale_reason=NULL,
	    updated_at=now() WHERE target_id=$1 AND id=$2`, targetID, requestID, nextState); err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	request, err = loadRequest(ctx, s.store.pool, targetID, requestID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if err := s.markEligible(ctx, s.store.pool, p, &request); err != nil {
		return ReviewRequest{}, err
	}
	return request, nil
}

func (s *Service) markEligible(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, p access.Principal, request *ReviewRequest) error {
	if request.State == "stale" || request.State == "consumed" {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(request.Groups))
	for _, group := range request.Groups {
		ids = append(ids, group.GroupID)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT id::text,coalesce(leader_id::text,'') FROM databrain.groups
	    WHERE id=ANY($1::uuid[]) AND is_active`, ids)
	if err != nil {
		return draftFailure(problem.PlatformUnavailable)
	}
	leaders := map[uuid.UUID]uuid.UUID{}
	for rows.Next() {
		var group, leader string
		if err := rows.Scan(&group, &leader); err != nil {
			rows.Close()
			return draftFailure(problem.PlatformUnavailable)
		}
		groupID, err := uuid.Parse(group)
		if err != nil {
			rows.Close()
			return draftFailure(problem.PlatformUnavailable)
		}
		if leader != "" {
			leaderID, err := uuid.Parse(leader)
			if err != nil {
				rows.Close()
				return draftFailure(problem.PlatformUnavailable)
			}
			leaders[groupID] = leaderID
		}
	}
	rows.Close()
	for index := range request.Groups {
		group := &request.Groups[index]
		if group.Decision == nil && leaders[group.GroupID] == p.UserID {
			group.Eligible = true
			request.EligibleGroupIDs = append(request.EligibleGroupIDs, group.GroupID)
		}
	}
	return nil
}

func loadRequest(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, targetID, requestID uuid.UUID) (ReviewRequest, error) {
	var request ReviewRequest
	var source, head, execution, consumedKey string
	var jobID pgtype.Int8
	var consumedAt pgtype.Timestamptz
	var batchRaw []byte
	err := q.QueryRow(ctx, `SELECT r.id::text,r.target_id::text,r.draft_id::text,r.definition_id::text,
	    r.actor_id::text,r.actor_username,d.title,r.revision,r.direction,r.mode,r.state,coalesce(r.stale_reason,''),
	    r.created_at,r.updated_at,r.target_config_revision,r.policy_generation,r.head_generation,
	    coalesce(r.expected_head_id::text,''),coalesce(r.source_execution_id::text,''),
	    coalesce(r.execution_id::text,''),r.job_id,coalesce(r.consumed_request_key::text,''),r.consumed_at,
	    def.up_sql,def.down_sql,def.irreversible,coalesce(def.irreversible_reason,''),r.batch_config
	    FROM databrain.migration_requests r
	    JOIN databrain.migration_drafts d ON d.target_id=r.target_id AND d.id=r.draft_id
	    JOIN databrain.migration_definitions def ON def.target_id=r.target_id AND def.id=r.definition_id
	    WHERE r.target_id=$1 AND r.id=$2`, targetID, requestID).
		Scan(&request.ID, &request.TargetID, &request.DraftID, &request.DefinitionID, &request.ActorID,
			&request.ActorUsername, &request.Title, &request.Revision, &request.Direction, &request.Mode, &request.State,
			&request.StaleReason, &request.CreatedAt, &request.UpdatedAt, &request.ConfigRevision,
			&request.PolicyGeneration, &request.HeadGeneration, &head, &source,
			&execution, &jobID, &consumedKey, &consumedAt, &request.SQL,
			&request.DownSQL, &request.Irreversible, &request.IrreversibleReason, &batchRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, draftFailure(problem.ReviewRequestNotFound)
	}
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	if len(batchRaw) > 0 {
		cfg, err := execstore.UnmarshalBatchConfig(batchRaw)
		if err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		request.Batch = cfg
	}
	request.CreatedAt, request.UpdatedAt = request.CreatedAt.UTC(), request.UpdatedAt.UTC()
	if head != "" {
		id, err := uuid.Parse(head)
		if err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		request.ExpectedHeadID = &id
	}
	if source != "" {
		id, err := uuid.Parse(source)
		if err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		request.SourceExecutionID = &id
	}
	if execution != "" {
		id, err := uuid.Parse(execution)
		if err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		request.ExecutionID = &id
	}
	if jobID.Valid {
		value := jobID.Int64
		request.JobID = &value
	}
	if consumedKey != "" {
		id, err := uuid.Parse(consumedKey)
		if err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		request.ConsumedRequestKey = &id
	}
	if consumedAt.Valid {
		value := consumedAt.Time.UTC()
		request.ConsumedAt = &value
	}
	rows, err := q.Query(ctx, `SELECT g.group_id::text,g.group_name,g.access_level,g.leader_id::text,
	    coalesce(u.username,''),coalesce(d.actor_id::text,''),coalesce(du.username,''),d.created_at
	    FROM databrain.migration_request_groups g
	    LEFT JOIN databrain.migration_request_decisions d
	      ON d.target_id=g.target_id AND d.request_id=g.request_id AND d.group_id=g.group_id
	    LEFT JOIN databrain.users u ON u.id=g.leader_id
	    LEFT JOIN databrain.users du ON du.id=d.actor_id
	    WHERE g.target_id=$1 AND g.request_id=$2 ORDER BY g.group_name,g.group_id`, targetID, requestID)
	if err != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	request.Groups = make([]RequestGroup, 0)
	for rows.Next() {
		var group RequestGroup
		var groupID, accessLevel, leaderID, actorID string
		var actorName string
		var decidedAt pgtype.Timestamptz
		if err := rows.Scan(&groupID, &group.GroupName, &accessLevel, &leaderID, &group.LeaderUsername,
			&actorID, &actorName, &decidedAt); err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		gid, err := uuid.Parse(groupID)
		if err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		lid, err := uuid.Parse(leaderID)
		if err != nil {
			return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
		}
		group.GroupID, group.LeaderID, group.Access = gid, lid, access.Level(accessLevel)
		if actorID != "" && decidedAt.Valid {
			actor, err := uuid.Parse(actorID)
			if err != nil {
				return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
			}
			group.Decision = &RequestDecision{ActorID: actor, LeaderID: lid, ActorUsername: actorName, CreatedAt: decidedAt.Time.UTC()}
		}
		request.Groups = append(request.Groups, group)
	}
	if rows.Err() != nil {
		return ReviewRequest{}, draftFailure(problem.PlatformUnavailable)
	}
	state, err := readTargetReviewState(ctx, q, targetID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if request.State != "consumed" {
		if reason := state.staleReason(request.ConfigRevision, request.PolicyGeneration, request.ExpectedHeadID, request.HeadGeneration); reason != "" {
			request.State, request.StaleReason = "stale", reason
		} else if request.State == "stale" {
			request.State = "stale"
		} else if len(request.Groups) > 0 {
			decided := 0
			for _, group := range request.Groups {
				if group.Decision != nil {
					decided++
				}
			}
			if decided < len(request.Groups) {
				request.State = "pending_review"
			} else {
				request.State = "approved"
			}
		}
	}
	return request, nil
}

func lockPrincipal(ctx context.Context, tx pgx.Tx, p access.Principal) error {
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.users WHERE id=$1 AND is_active FOR SHARE`, p.UserID).Scan(&one); err != nil {
		return draftFailure(problem.SessionInvalid)
	}
	if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.sessions WHERE id=$1 AND user_id=$2
	    AND revoked_at IS NULL AND NOT reauth_required FOR SHARE`, p.SessionID, p.UserID).Scan(&one); err != nil {
		return draftFailure(problem.SessionInvalid)
	}
	return nil
}

func lockSecurityGuard(ctx context.Context, tx pgx.Tx) error {
	var singleton bool
	if err := tx.QueryRow(ctx, `SELECT singleton FROM databrain.security_guard WHERE singleton FOR UPDATE`).Scan(&singleton); err != nil || !singleton {
		return draftFailure(problem.PlatformUnavailable)
	}
	return nil
}
