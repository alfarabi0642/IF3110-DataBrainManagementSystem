package head

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/journal"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrReconciliationConflict = errors.New("committed evidence conflicts with stored execution or history")
	ErrRedoStackNotEmpty      = errors.New("cannot apply a new migration while pending forward entries exist")
)

// CommitEvidence membawa bukti terverifikasi dari target journal untuk finalisasi history/head
// secara idempoten.
type CommitEvidence struct {
	RootExecutionID   uuid.UUID
	ActionExecutionID uuid.UUID
	PlanHash          string
	Direction         string
	Mode              string
	RowsAffected      int64
	CommittedAt       time.Time
	Evidence          string
}

// FinalizeFunc dipanggil worker di dalam transaksi platform singkat setelah commit target terbukti.
type FinalizeFunc func(context.Context, pgx.Tx, uuid.UUID, CommitEvidence) error

// FinalizeCommitted mencatat committed event secara append-only dan memajukan migration_target_heads
// serta migration_redo_entries di dalam transaksi caller. Fungsi ini idempoten menurut execution ID
// dan tidak pernah memeriksa ulang izin sesi setelah commit target terbukti.
func FinalizeCommitted(ctx context.Context, tx pgx.Tx, executionID uuid.UUID, evidence CommitEvidence) error {
	if tx == nil || executionID == uuid.Nil || !journal.IsValidPlanHash(evidence.PlanHash) ||
		evidence.Evidence == "" || len(evidence.Evidence) > 200 {
		return ErrReconciliationConflict
	}
	committedAt := evidence.CommittedAt.UTC()
	if committedAt.IsZero() {
		committedAt = time.Now().UTC()
	}

	var (
		targetID     uuid.UUID
		definitionID uuid.UUID
		actorID      uuid.UUID
		revision     int64
		direction    string
		mode         string
		planHash     string
	)
	err := tx.QueryRow(ctx, `SELECT target_id, definition_id, actor_id, revision, direction, mode, plan_hash
		FROM databrain.migration_executions WHERE id = $1`, executionID).Scan(
		&targetID, &definitionID, &actorID, &revision, &direction, &mode, &planHash,
	)
	if err != nil {
		return err
	}
	if planHash != evidence.PlanHash || (evidence.Direction != "" && evidence.Direction != direction) {
		return ErrReconciliationConflict
	}

	if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(new(int)); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT 1 FROM databrain.migration_target_heads WHERE target_id=$1 FOR UPDATE`, targetID).Scan(new(int)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrHeadNotInitialized
		}
		return err
	}

	// Deduplikasi menurut execution_id dilakukan sebelum memeriksa expected head lama agar replay
	// finalisasi setelah acknowledgement hilang selalu idempoten.
	var existingHash, existingDir string
	err = tx.QueryRow(ctx, `SELECT plan_hash, direction FROM databrain.migration_committed_events
		WHERE target_id=$1 AND execution_id=$2`, targetID, executionID).Scan(&existingHash, &existingDir)
	if err == nil {
		if existingHash == evidence.PlanHash && existingDir == direction {
			return nil
		}
		return ErrReconciliationConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	head, err := LoadHead(ctx, tx, targetID)
	if err != nil {
		return err
	}
	if !head.Initialized {
		return ErrHeadNotInitialized
	}

	switch direction {
	case execstore.DirectionApply:
		var redoCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM databrain.migration_redo_entries WHERE target_id=$1`, targetID).Scan(&redoCount); err != nil {
			return err
		}
		if redoCount > 0 {
			return ErrRedoStackNotEmpty
		}
		applied, err := loadCurrentlyAppliedDefinitions(ctx, tx, targetID)
		if err != nil {
			return err
		}
		event, nextHead, err := PlanApply(head, executionID, definitionID, actorID, revision, planHash, committedAt, applied)
		if err != nil {
			return err
		}
		event.Mode = mode
		event.CommitEvidence = evidence.Evidence
		if err := insertCommittedEvent(ctx, tx, event); err != nil {
			return err
		}
		return updateTargetHead(ctx, tx, nextHead)

	case execstore.DirectionRollback:
		if head.ActiveExecutionID == nil {
			return ErrNotActiveExecution
		}
		active, err := loadCommittedEvent(ctx, tx, targetID, *head.ActiveExecutionID)
		if err != nil {
			return err
		}
		var irreversible bool
		if err := tx.QueryRow(ctx, `SELECT irreversible FROM databrain.migration_definitions
			WHERE target_id=$1 AND id=$2`, targetID, active.DefinitionID).Scan(&irreversible); err != nil {
			return err
		}
		var nextRedoPos int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(position), 0) + 1 FROM databrain.migration_redo_entries
			WHERE target_id=$1`, targetID).Scan(&nextRedoPos); err != nil {
			return err
		}
		event, nextHead, redo, err := PlanRollback(head, head.Generation, active, executionID, definitionID, actorID,
			revision, planHash, !irreversible, nextRedoPos, committedAt)
		if err != nil {
			return err
		}
		event.Mode = mode
		event.CommitEvidence = evidence.Evidence
		if err := insertCommittedEvent(ctx, tx, event); err != nil {
			return err
		}
		if err := updateTargetHead(ctx, tx, nextHead); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO databrain.migration_redo_entries
			(target_id, position, source_execution_id, definition_id) VALUES ($1, $2, $3, $4)`,
			redo.TargetID, redo.Position, redo.SourceExecutionID, redo.DefinitionID)
		return err

	case execstore.DirectionForward:
		var topRedo RedoEntry
		err := tx.QueryRow(ctx, `SELECT target_id, position, source_execution_id, definition_id
			FROM databrain.migration_redo_entries WHERE target_id=$1 ORDER BY position DESC LIMIT 1`, targetID).Scan(
			&topRedo.TargetID, &topRedo.Position, &topRedo.SourceExecutionID, &topRedo.DefinitionID,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRedoSourceMismatch
			}
			return err
		}
		source, err := loadCommittedEvent(ctx, tx, targetID, topRedo.SourceExecutionID)
		if err != nil {
			return err
		}
		event, nextHead, err := PlanForward(head, head.Generation, topRedo, source, executionID, actorID, planHash, committedAt)
		if err != nil {
			return err
		}
		event.Mode = mode
		event.CommitEvidence = evidence.Evidence
		if err := insertCommittedEvent(ctx, tx, event); err != nil {
			return err
		}
		if err := updateTargetHead(ctx, tx, nextHead); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM databrain.migration_redo_entries WHERE target_id=$1 AND position=$2`,
			topRedo.TargetID, topRedo.Position)
		return err

	default:
		return ErrReconciliationConflict
	}
}

func loadCurrentlyAppliedDefinitions(ctx context.Context, tx pgx.Tx, targetID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := tx.Query(ctx, `SELECT e.definition_id
		FROM databrain.migration_committed_events e
		WHERE e.target_id = $1
		  AND e.direction IN ('apply', 'forward')
		  AND NOT EXISTS (
		      SELECT 1 FROM databrain.migration_committed_events r
		       WHERE r.target_id = e.target_id
		         AND r.direction = 'rollback'
		         AND r.canceled_execution_id = e.execution_id
		  )`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := make(map[uuid.UUID]bool)
	for rows.Next() {
		var defID uuid.UUID
		if err := rows.Scan(&defID); err != nil {
			return nil, err
		}
		applied[defID] = true
	}
	return applied, rows.Err()
}

func loadCommittedEvent(ctx context.Context, tx pgx.Tx, targetID, executionID uuid.UUID) (CommittedEvent, error) {
	var e CommittedEvent
	var parent, canceled *string
	err := tx.QueryRow(ctx, `SELECT id, target_id, execution_id, definition_id, actor_id, revision,
		direction, mode, plan_hash, commit_evidence, parent_execution_id::text, canceled_execution_id::text,
		generation, committed_at
		FROM databrain.migration_committed_events
		WHERE target_id=$1 AND execution_id=$2`, targetID, executionID).Scan(
		&e.ID, &e.TargetID, &e.ExecutionID, &e.DefinitionID, &e.ActorID, &e.Revision,
		&e.Direction, &e.Mode, &e.PlanHash, &e.CommitEvidence, &parent, &canceled,
		&e.Generation, &e.CommittedAt,
	)
	if err != nil {
		return CommittedEvent{}, err
	}
	if parent != nil && *parent != "" {
		id, err := uuid.Parse(*parent)
		if err != nil {
			return CommittedEvent{}, err
		}
		e.ParentExecutionID = &id
	}
	if canceled != nil && *canceled != "" {
		id, err := uuid.Parse(*canceled)
		if err != nil {
			return CommittedEvent{}, err
		}
		e.CanceledExecutionID = &id
	}
	return e, nil
}

func insertCommittedEvent(ctx context.Context, tx pgx.Tx, e CommittedEvent) error {
	_, err := tx.Exec(ctx, `INSERT INTO databrain.migration_committed_events
		(id, target_id, execution_id, definition_id, actor_id, revision, direction, mode,
		 plan_hash, commit_evidence, parent_execution_id, canceled_execution_id, generation, committed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		e.ID, e.TargetID, e.ExecutionID, e.DefinitionID, e.ActorID, e.Revision, e.Direction, e.Mode,
		e.PlanHash, e.CommitEvidence, e.ParentExecutionID, e.CanceledExecutionID, e.Generation, e.CommittedAt,
	)
	return err
}

func updateTargetHead(ctx context.Context, tx pgx.Tx, head HeadState) error {
	_, err := tx.Exec(ctx, `UPDATE databrain.migration_target_heads
		SET active_execution_id=$2, generation=$3, updated_at=now()
		WHERE target_id=$1`, head.TargetID, head.ActiveExecutionID, head.Generation)
	return err
}
