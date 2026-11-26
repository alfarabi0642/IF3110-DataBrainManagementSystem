package head

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrHeadNotInitialized = errors.New("migration head is not initialized")
	ErrNotActiveExecution = errors.New("execution is not the active head")
	ErrIrreversibleHead   = errors.New("migration head is irreversible")
	ErrDefinitionApplied  = errors.New("definition is already applied")
	ErrRedoSourceMismatch = errors.New("redo entry does not match its canceled event")
)

type HeadState struct {
	TargetID          uuid.UUID
	ActiveExecutionID *uuid.UUID
	Generation        int64
	Initialized       bool
}

type CommittedEvent struct {
	ID, TargetID, ExecutionID, DefinitionID, ActorID uuid.UUID
	Revision, Generation                             int64
	Direction, Mode, PlanHash, CommitEvidence        string
	ParentExecutionID, CanceledExecutionID           *uuid.UUID
	CommittedAt                                      time.Time
}

type RedoEntry struct {
	TargetID          uuid.UUID
	Position          int64
	SourceExecutionID uuid.UUID
	DefinitionID      uuid.UUID
}

// LoadHead membaca posisi aktif target. Baris tanpa head berarti cutover belum diinisialisasi.
func LoadHead(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, targetID uuid.UUID) (HeadState, error) {
	var head HeadState
	var active string
	err := q.QueryRow(ctx, `SELECT target_id::text,coalesce(active_execution_id::text,''),generation,initialized
	    FROM databrain.migration_target_heads WHERE target_id=$1`, targetID).
		Scan(&head.TargetID, &active, &head.Generation, &head.Initialized)
	if errors.Is(err, pgx.ErrNoRows) {
		return HeadState{}, ErrHeadNotInitialized
	}
	if err != nil {
		return HeadState{}, err
	}
	if active != "" {
		id, err := uuid.Parse(active)
		if err != nil {
			return HeadState{}, err
		}
		head.ActiveExecutionID = &id
	}
	return head, nil
}

// PlanApply appends an ordinary apply whose parent is today's active head.
func PlanApply(head HeadState, executionID, definitionID, actorID uuid.UUID, revision int64,
	planHash string, committedAt time.Time, applied map[uuid.UUID]bool) (CommittedEvent, HeadState, error) {
	if !head.Initialized {
		return CommittedEvent{}, HeadState{}, ErrHeadNotInitialized
	}
	if executionID == uuid.Nil || definitionID == uuid.Nil || actorID == uuid.Nil || revision < 1 {
		return CommittedEvent{}, HeadState{}, errors.New("invalid apply event")
	}
	if applied[definitionID] {
		return CommittedEvent{}, HeadState{}, ErrDefinitionApplied
	}
	generation := head.Generation + 1
	event := CommittedEvent{
		ID: executionID, TargetID: head.TargetID, ExecutionID: executionID, DefinitionID: definitionID,
		ActorID: actorID, Revision: revision, Direction: execstore.DirectionApply, Mode: execstore.ModeNormal,
		PlanHash: planHash, ParentExecutionID: head.ActiveExecutionID, Generation: generation,
		CommittedAt: committedAt,
	}
	head.ActiveExecutionID, head.Generation = &event.ExecutionID, generation
	return event, head, nil
}

// PlanRollback cancels the exact active event, moves the head to its parent and appends a redo entry.
func PlanRollback(head HeadState, expectedHeadGeneration int64, active CommittedEvent, executionID, definitionID, actorID uuid.UUID,
	revision int64, planHash string, reversible bool, redoPosition int64, committedAt time.Time) (CommittedEvent, HeadState, RedoEntry, error) {
	if !head.Initialized {
		return CommittedEvent{}, HeadState{}, RedoEntry{}, ErrHeadNotInitialized
	}
	if head.Generation != expectedHeadGeneration || head.ActiveExecutionID == nil ||
		*head.ActiveExecutionID != active.ExecutionID {
		return CommittedEvent{}, HeadState{}, RedoEntry{}, ErrNotActiveExecution
	}
	if !reversible {
		return CommittedEvent{}, HeadState{}, RedoEntry{}, ErrIrreversibleHead
	}
	if executionID == uuid.Nil || definitionID == uuid.Nil || actorID == uuid.Nil || revision < 1 || redoPosition < 1 {
		return CommittedEvent{}, HeadState{}, RedoEntry{}, errors.New("invalid rollback event")
	}
	canceled := active.ExecutionID
	generation := head.Generation + 1
	event := CommittedEvent{
		ID: executionID, TargetID: head.TargetID, ExecutionID: executionID, DefinitionID: definitionID,
		ActorID: actorID, Revision: revision, Direction: execstore.DirectionRollback, Mode: execstore.ModeNormal,
		PlanHash: planHash, ParentExecutionID: active.ParentExecutionID, CanceledExecutionID: &canceled,
		Generation: generation, CommittedAt: committedAt,
	}
	head.ActiveExecutionID, head.Generation = active.ParentExecutionID, generation
	redo := RedoEntry{TargetID: head.TargetID, Position: redoPosition, SourceExecutionID: active.ExecutionID,
		DefinitionID: active.DefinitionID}
	return event, head, redo, nil
}

// PlanForward consumes one redo entry and applies the original up as a fresh active event parented to
// today's active head.
func PlanForward(head HeadState, expectedHeadGeneration int64, redo RedoEntry, source CommittedEvent, executionID, actorID uuid.UUID,
	planHash string, committedAt time.Time) (CommittedEvent, HeadState, error) {
	if !head.Initialized {
		return CommittedEvent{}, HeadState{}, ErrHeadNotInitialized
	}
	if head.Generation != expectedHeadGeneration {
		return CommittedEvent{}, HeadState{}, ErrNotActiveExecution
	}
	if redo.Position < 1 || redo.SourceExecutionID != source.ExecutionID || redo.DefinitionID != source.DefinitionID {
		return CommittedEvent{}, HeadState{}, ErrRedoSourceMismatch
	}
	if executionID == uuid.Nil || actorID == uuid.Nil {
		return CommittedEvent{}, HeadState{}, errors.New("invalid forward event")
	}
	generation := head.Generation + 1
	event := CommittedEvent{
		ID: executionID, TargetID: head.TargetID, ExecutionID: executionID, DefinitionID: source.DefinitionID,
		ActorID: actorID, Revision: source.Revision, Direction: execstore.DirectionForward, Mode: execstore.ModeNormal,
		PlanHash: planHash, ParentExecutionID: head.ActiveExecutionID, Generation: generation,
		CommittedAt: committedAt,
	}
	head.ActiveExecutionID, head.Generation = &event.ExecutionID, generation
	return event, head, nil
}
