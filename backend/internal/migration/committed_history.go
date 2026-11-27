package migration

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type CommittedEventSummary struct {
	ID, TargetID, ExecutionID, DefinitionID uuid.UUID
	ActorUsername, Title, Direction, Mode   string
	Status                                  string
	PlanHash                                string
	Revision, Generation                    int64
	ParentExecutionID, CanceledExecutionID  *uuid.UUID
	CommittedAt                             time.Time
}

type CommittedEventDetail struct {
	CommittedEventSummary
	SQL, DownSQL       string
	Irreversible       bool
	IrreversibleReason string
}

type ActivityItem struct {
	SourceKind, ID, TargetID string
	ExecutionID, AttemptID   string
	AttemptNumber            *int
	Title, Actor, State      string
	Revision                 int64
	CreatedAt                time.Time
}

func committedEventColumns() string {
	return `e.id::text,e.target_id::text,e.execution_id::text,e.definition_id::text,u.username,d.title,
	    e.revision,e.direction,e.mode,e.plan_hash,e.parent_execution_id::text,e.canceled_execution_id::text,
	    e.generation,e.committed_at`
}

func scanCommittedEvent(row pgx.Row, detail *CommittedEventDetail) error {
	var parent, canceled *string
	err := row.Scan(&detail.ID, &detail.TargetID, &detail.ExecutionID, &detail.DefinitionID,
		&detail.ActorUsername, &detail.Title, &detail.Revision, &detail.Direction, &detail.Mode,
		&detail.PlanHash, &parent, &canceled, &detail.Generation, &detail.CommittedAt)
	if err != nil {
		return err
	}
	detail.CreatedAtUTC()
	if parent != nil && *parent != "" {
		id, err := uuid.Parse(*parent)
		if err != nil {
			return err
		}
		detail.ParentExecutionID = &id
	}
	if canceled != nil && *canceled != "" {
		id, err := uuid.Parse(*canceled)
		if err != nil {
			return err
		}
		detail.CanceledExecutionID = &id
	}
	detail.Status = "committed"
	return nil
}

func (s *CommittedEventSummary) CreatedAtUTC() { s.CommittedAt = s.CommittedAt.UTC() }

// CommittedHistory reads committed events in generation order. Hide rollback removes rollback events
// and their exact canceled counterparts before limit/offset; all joins and predicates are target-scoped.
func (s *Service) CommittedHistory(ctx context.Context, p access.Principal, targetID uuid.UUID, limit, offset int, hideRollback bool) ([]CommittedEventSummary, bool, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.DatabaseView); err != nil {
		return nil, false, err
	}
	if targetID == uuid.Nil || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	filter := ""
	if hideRollback {
		filter = ` AND e.direction <> 'rollback'
		    AND NOT EXISTS (SELECT 1 FROM databrain.migration_committed_events r
		        WHERE r.target_id=e.target_id AND r.direction='rollback'
		          AND r.canceled_execution_id=e.execution_id)`
	}
	rows, err := s.store.pool.Query(ctx, `SELECT `+committedEventColumns()+`
	    FROM databrain.migration_committed_events e
	    JOIN databrain.users u ON u.id=e.actor_id
	    JOIN databrain.migration_definitions def ON def.target_id=e.target_id AND def.id=e.definition_id
	    JOIN databrain.migration_drafts d ON d.target_id=def.target_id AND d.id=def.draft_id
	    WHERE e.target_id=$1`+filter+`
	    ORDER BY e.generation DESC,e.id LIMIT $2 OFFSET $3`, targetID, limit+1, offset)
	if err != nil {
		return nil, false, draftFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]CommittedEventSummary, 0, limit)
	for rows.Next() {
		var item CommittedEventDetail
		if err := scanCommittedEvent(rows, &item); err != nil {
			return nil, false, draftFailure(problem.PlatformUnavailable)
		}
		items = append(items, item.CommittedEventSummary)
	}
	if rows.Err() != nil {
		return nil, false, draftFailure(problem.PlatformUnavailable)
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}

// CommittedEvent returns exact frozen event SQL for an authorized target viewer. Missing/cross-target
// execution IDs are indistinguishable.
func (s *Service) CommittedEvent(ctx context.Context, p access.Principal, targetID, executionID uuid.UUID) (*CommittedEventDetail, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.DatabaseView); err != nil {
		return nil, err
	}
	if targetID == uuid.Nil || executionID == uuid.Nil {
		return nil, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var item CommittedEventDetail
	var parent, canceled *string
	err := s.store.pool.QueryRow(ctx, `SELECT `+committedEventColumns()+`,
	    def.up_sql,def.down_sql,def.irreversible,coalesce(def.irreversible_reason,'')
	    FROM databrain.migration_committed_events e
	    JOIN databrain.users u ON u.id=e.actor_id
	    JOIN databrain.migration_definitions def ON def.target_id=e.target_id AND def.id=e.definition_id
	    JOIN databrain.migration_drafts d ON d.target_id=def.target_id AND d.id=def.draft_id
	    WHERE e.target_id=$1 AND e.execution_id=$2`, targetID, executionID).
		Scan(&item.ID, &item.TargetID, &item.ExecutionID, &item.DefinitionID, &item.ActorUsername, &item.Title,
			&item.Revision, &item.Direction, &item.Mode, &item.PlanHash, &parent, &canceled,
			&item.Generation, &item.CommittedAt, &item.SQL, &item.DownSQL, &item.Irreversible, &item.IrreversibleReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, draftFailure(problem.PlatformUnavailable)
	}
	if parent != nil && *parent != "" {
		id, err := uuid.Parse(*parent)
		if err != nil {
			return nil, draftFailure(problem.PlatformUnavailable)
		}
		item.ParentExecutionID = &id
	}
	if canceled != nil && *canceled != "" {
		id, err := uuid.Parse(*canceled)
		if err != nil {
			return nil, draftFailure(problem.PlatformUnavailable)
		}
		item.CanceledExecutionID = &id
	}
	item.CreatedAtUTC()
	item.Status = "committed"
	return &item, nil
}

// Activity merges legacy attempts, M2 executions and worker attempts without changing committed rows.
func (s *Service) Activity(ctx context.Context, p access.Principal, targetID uuid.UUID, limit, offset int) ([]ActivityItem, bool, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.DatabaseView); err != nil {
		return nil, false, err
	}
	if targetID == uuid.Nil || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.store.pool.Query(ctx, `SELECT source_kind,id,target_id,execution_id,attempt_id,
	    attempt_number,revision,actor,title,state,created_at FROM (
	    SELECT 'legacy_attempt'::text AS source_kind,a.id::text AS id,a.target_id::text AS target_id,
	        ''::text AS execution_id,a.id::text AS attempt_id,NULL::int AS attempt_number,
	        a.draft_revision::bigint AS revision,a.executor_username_snapshot AS actor,a.title_snapshot AS title,
	        a.state AS state,a.created_at AS created_at
	    FROM databrain.migration_attempts a WHERE a.target_id=$1
	    UNION ALL
	    SELECT 'execution',e.id::text,e.target_id::text,e.id::text,'',NULL,e.revision,u.username,coalesce(d.title,''),
	        CASE WHEN e.outcome IS NULL THEN e.phase ELSE e.phase||':'||e.outcome END,e.created_at
	    FROM databrain.migration_executions e
	    JOIN databrain.users u ON u.id=e.actor_id
	    LEFT JOIN databrain.migration_definitions def ON def.target_id=e.target_id AND def.id=e.definition_id
	    LEFT JOIN databrain.migration_drafts d ON d.target_id=def.target_id AND d.id=def.draft_id
	    WHERE e.target_id=$1
	    UNION ALL
	    SELECT 'worker_attempt',e.id::text||':'||w.attempt_number,e.target_id::text,e.id::text,'',
	        w.attempt_number,e.revision,u.username,coalesce(d.title,''),w.state,w.started_at
	    FROM databrain.migration_worker_attempts w
	    JOIN databrain.migration_executions e ON e.id=w.execution_id AND e.target_id=$1
	    JOIN databrain.users u ON u.id=e.actor_id
	    LEFT JOIN databrain.migration_definitions def ON def.target_id=e.target_id AND def.id=e.definition_id
	    LEFT JOIN databrain.migration_drafts d ON d.target_id=def.target_id AND d.id=def.draft_id
	) activity ORDER BY created_at DESC,id LIMIT $2 OFFSET $3`, targetID, limit+1, offset)
	if err != nil {
		return nil, false, draftFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]ActivityItem, 0, limit)
	for rows.Next() {
		var item ActivityItem
		var attempt pgtype.Int4
		if err := rows.Scan(&item.SourceKind, &item.ID, &item.TargetID, &item.ExecutionID, &item.AttemptID,
			&attempt, &item.Revision, &item.Actor, &item.Title, &item.State, &item.CreatedAt); err != nil {
			return nil, false, draftFailure(problem.PlatformUnavailable)
		}
		item.CreatedAt = item.CreatedAt.UTC()
		if attempt.Valid {
			value := int(attempt.Int32)
			item.AttemptNumber = &value
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

type HeadExecutionSummary struct {
	ExecutionID        uuid.UUID
	DefinitionID       uuid.UUID
	Title              string
	Revision           int64
	Irreversible       bool
	IrreversibleReason string
	CanRollback        bool
}

type ForwardCandidateSummary struct {
	SourceExecutionID uuid.UUID
	DefinitionID      uuid.UUID
	Title             string
	Revision          int64
}

type TargetHeadState struct {
	Initialized    bool
	HeadGeneration int64
	RedoDepth      int
	Head           *HeadExecutionSummary
	NextForward    *ForwardCandidateSummary
}

// TargetHead membaca posisi head aktif dan puncak redo stack untuk target agar SPA dapat
// menampilkan tombol Rollback/Forward secara jujur sesuai state server.
func (s *Service) TargetHead(ctx context.Context, p access.Principal, targetID uuid.UUID) (TargetHeadState, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, targetID, access.DatabaseView); err != nil {
		return TargetHeadState{}, err
	}
	if targetID == uuid.Nil {
		return TargetHeadState{}, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var out TargetHeadState
	var activeExec *string
	err := s.store.pool.QueryRow(ctx, `SELECT coalesce(initialized, false), coalesce(generation, 1), active_execution_id::text
		FROM databrain.migration_target_heads WHERE target_id=$1`, targetID).
		Scan(&out.Initialized, &out.HeadGeneration, &activeExec)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return TargetHeadState{}, draftFailure(problem.PlatformUnavailable)
	}

	if activeExec != nil && *activeExec != "" {
		headID, parseErr := uuid.Parse(*activeExec)
		if parseErr != nil {
			return TargetHeadState{}, draftFailure(problem.PlatformUnavailable)
		}
		var h HeadExecutionSummary
		var mode string
		h.ExecutionID = headID
		err = s.store.pool.QueryRow(ctx, `SELECT e.definition_id, d.title, e.revision, e.mode, def.irreversible, coalesce(def.irreversible_reason,'')
			FROM databrain.migration_committed_events e
			JOIN databrain.migration_definitions def ON def.target_id=e.target_id AND def.id=e.definition_id
			JOIN databrain.migration_drafts d ON d.target_id=def.target_id AND d.id=def.draft_id
			WHERE e.target_id=$1 AND e.execution_id=$2`, targetID, headID).
			Scan(&h.DefinitionID, &h.Title, &h.Revision, &mode, &h.Irreversible, &h.IrreversibleReason)
		if err == nil {
			h.CanRollback = !h.Irreversible && mode != "batch"
			out.Head = &h
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return TargetHeadState{}, draftFailure(problem.PlatformUnavailable)
		}
	}

	if err := s.store.pool.QueryRow(ctx, `SELECT count(*) FROM databrain.migration_redo_entries WHERE target_id=$1`, targetID).Scan(&out.RedoDepth); err != nil {
		return TargetHeadState{}, draftFailure(problem.PlatformUnavailable)
	}
	if out.RedoDepth > 0 {
		var f ForwardCandidateSummary
		err = s.store.pool.QueryRow(ctx, `SELECT r.source_execution_id, r.definition_id, d.title, def.revision
			FROM databrain.migration_redo_entries r
			JOIN databrain.migration_definitions def ON def.target_id=r.target_id AND def.id=r.definition_id
			JOIN databrain.migration_drafts d ON d.target_id=def.target_id AND d.id=def.draft_id
			WHERE r.target_id=$1
			ORDER BY r.position DESC LIMIT 1`, targetID).
			Scan(&f.SourceExecutionID, &f.DefinitionID, &f.Title, &f.Revision)
		if err == nil {
			out.NextForward = &f
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return TargetHeadState{}, draftFailure(problem.PlatformUnavailable)
		}
	}
	return out, nil
}
