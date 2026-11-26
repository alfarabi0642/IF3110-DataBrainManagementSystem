package head

import (
	"errors"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/google/uuid"
)

func TestCommittedHeadTransitions(t *testing.T) {
	target, actor := uuid.New(), uuid.New()
	head := HeadState{TargetID: target, Generation: 1, Initialized: true}
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	plan := func(name string) string { return name + string(make([]byte, 64-len(name))) }

	eventA, head, err := PlanApply(head, uuid.New(), uuid.New(), actor, 1, plan("a"), now, map[uuid.UUID]bool{})
	if err != nil || eventA.ParentExecutionID != nil || head.ActiveExecutionID == nil || *head.ActiveExecutionID != eventA.ExecutionID || head.Generation != 2 {
		t.Fatalf("apply A failed: %+v %+v %v", eventA, head, err)
	}
	eventB, head, err := PlanApply(head, uuid.New(), uuid.New(), actor, 1, plan("b"), now, map[uuid.UUID]bool{})
	if err != nil || eventB.ParentExecutionID == nil || *eventB.ParentExecutionID != eventA.ExecutionID {
		t.Fatalf("apply B parent wrong: %+v %v", eventB, err)
	}
	eventC, head, err := PlanApply(head, uuid.New(), uuid.New(), actor, 1, plan("c"), now, map[uuid.UUID]bool{})
	if err != nil || head.ActiveExecutionID == nil || *head.ActiveExecutionID != eventC.ExecutionID || head.Generation != 4 {
		t.Fatalf("head is not C: %+v %v", head, err)
	}
	// B cannot roll back while C is active.
	if _, _, _, err := PlanRollback(head, head.Generation, eventB, uuid.New(), uuid.New(), actor, 1, plan("r"), true, 1, now); !errors.Is(err, ErrNotActiveExecution) {
		t.Fatalf("non-active rollback error = %v", err)
	}
	// C, B, A roll back in order; each exact active event moves to its parent and pushes redo.
	rollbackC, head, redoC, err := PlanRollback(head, head.Generation, eventC, uuid.New(), uuid.New(), actor, 1, plan("rc"), true, 1, now)
	if err != nil || rollbackC.CanceledExecutionID == nil || *rollbackC.CanceledExecutionID != eventC.ExecutionID ||
		head.ActiveExecutionID == nil || *head.ActiveExecutionID != eventB.ExecutionID || redoC.SourceExecutionID != eventC.ExecutionID {
		t.Fatalf("rollback C wrong: %+v %+v %+v %v", rollbackC, head, redoC, err)
	}
	rollbackB, head, redoB, err := PlanRollback(head, head.Generation, eventB, uuid.New(), uuid.New(), actor, 1, plan("rb"), true, 2, now)
	if err != nil || *rollbackB.ParentExecutionID != eventA.ExecutionID || *head.ActiveExecutionID != eventA.ExecutionID {
		t.Fatalf("rollback B wrong: %+v %+v %v", rollbackB, head, err)
	}
	_, head, redoA, err := PlanRollback(head, head.Generation, eventA, uuid.New(), uuid.New(), actor, 1, plan("ra"), true, 3, now)
	if err != nil || head.ActiveExecutionID != nil || redoA.SourceExecutionID != eventA.ExecutionID {
		t.Fatalf("rollback A wrong: %+v %+v %v", head, redoA, err)
	}
	// Forward consumes the top entry; A→B→C forward parents chain to the newly active events.
	forwardA, head, err := PlanForward(head, head.Generation, redoA, eventA, uuid.New(), actor, plan("fa"), now)
	if err != nil || forwardA.ParentExecutionID != nil || head.ActiveExecutionID == nil || *head.ActiveExecutionID != forwardA.ExecutionID {
		t.Fatalf("forward A wrong: %+v %+v %v", forwardA, head, err)
	}
	forwardB, head, err := PlanForward(head, head.Generation, redoB, eventB, uuid.New(), actor, plan("fb"), now)
	if err != nil || forwardB.ParentExecutionID == nil || *forwardB.ParentExecutionID != forwardA.ExecutionID {
		t.Fatalf("forward B parent wrong: %+v %v", forwardB, err)
	}
	forwardC, _, err := PlanForward(head, head.Generation, redoC, eventC, uuid.New(), actor, plan("fc"), now)
	if err != nil || forwardC.ParentExecutionID == nil || *forwardC.ParentExecutionID != forwardB.ExecutionID {
		t.Fatalf("forward C parent wrong: %+v %v", forwardC, err)
	}
	if forwardA.Direction != execstore.DirectionForward || forwardC.Direction != execstore.DirectionForward {
		t.Fatal("forward direction missing")
	}
	// Rollback rejection and repeated-apply guard.
	if _, _, _, err := PlanRollback(HeadState{TargetID: target}, 1, eventA, uuid.New(), uuid.New(), actor, 1, plan("x"), true, 1, now); !errors.Is(err, ErrHeadNotInitialized) {
		t.Fatalf("uninitialized rollback error = %v", err)
	}
	if _, _, _, err := PlanRollback(HeadState{TargetID: target, ActiveExecutionID: &eventA.ExecutionID,
		Generation: eventA.Generation, Initialized: true}, eventA.Generation, eventA, uuid.New(), uuid.New(), actor, 1, plan("x"), false, 1, now); !errors.Is(err, ErrIrreversibleHead) {
		t.Fatalf("irreversible rollback error = %v", err)
	}
	applied := map[uuid.UUID]bool{eventA.DefinitionID: true}
	if _, _, err := PlanApply(HeadState{TargetID: target, Generation: 1, Initialized: true}, uuid.New(), eventA.DefinitionID,
		actor, 1, plan("x"), now, applied); !errors.Is(err, ErrDefinitionApplied) {
		t.Fatalf("repeated apply error = %v", err)
	}
	if _, _, err := PlanForward(HeadState{TargetID: target, Generation: 1, Initialized: true}, 1, redoA, eventB,
		uuid.New(), actor, plan("x"), now); !errors.Is(err, ErrRedoSourceMismatch) {
		t.Fatalf("redo mismatch error = %v", err)
	}
}
