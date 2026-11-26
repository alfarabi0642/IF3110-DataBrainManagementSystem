package execstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func requireProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var prob *problem.Error
	if !errors.As(err, &prob) {
		t.Fatalf("expected problem.Error %q, got %T (%v)", want, err, err)
	}
	if prob.Code != want {
		t.Fatalf("expected problem code %q, got %q", want, prob.Code)
	}
}

func validApprovedOperation() ApprovedOperation {
	op := ApprovedOperation{
		RequestID: uuid.New(), DefinitionID: uuid.New(), TargetID: uuid.New(), ActorID: uuid.New(),
		Revision: 1, TargetConfigRevision: 1,
		Direction: DirectionApply, Mode: ModeNormal,
		SQLSnapshot:  "CREATE TABLE public.enqueue_probe(id int)",
		DownSnapshot: "DROP TABLE public.enqueue_probe",
	}
	op.PlanHash = PlanHash(op)
	return op
}

func rehashed(op ApprovedOperation) ApprovedOperation {
	op.PlanHash = PlanHash(op)
	return op
}

func TestPlanHashBindsCanonicalFields(t *testing.T) {
	op := validApprovedOperation()
	if PlanHash(op) != op.PlanHash {
		t.Fatal("plan hash is not deterministic")
	}
	head, source := uuid.New(), uuid.New()
	for _, tt := range []struct {
		name   string
		mutate func(*ApprovedOperation)
	}{
		{"target", func(o *ApprovedOperation) { o.TargetID = uuid.New() }},
		{"actor", func(o *ApprovedOperation) { o.ActorID = uuid.New() }},
		{"request", func(o *ApprovedOperation) { o.RequestID = uuid.New() }},
		{"definition", func(o *ApprovedOperation) { o.DefinitionID = uuid.New() }},
		{"revision", func(o *ApprovedOperation) { o.Revision++ }},
		{"config revision", func(o *ApprovedOperation) { o.TargetConfigRevision++ }},
		{"direction", func(o *ApprovedOperation) { o.Direction = DirectionRollback }},
		{"mode", func(o *ApprovedOperation) { o.Mode = ModeBatch }},
		{"expected head", func(o *ApprovedOperation) { o.ExpectedHeadID = &head }},
		{"source execution", func(o *ApprovedOperation) { o.SourceExecutionID = &source }},
		{"up SQL", func(o *ApprovedOperation) { o.SQLSnapshot += " " }},
		{"down SQL", func(o *ApprovedOperation) { o.DownSnapshot += " " }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mutated := op
			tt.mutate(&mutated)
			if PlanHash(mutated) == op.PlanHash {
				t.Fatal("plan hash did not bind its canonical field")
			}
		})
	}
}

func TestApprovedOperationValidation(t *testing.T) {
	valid := validApprovedOperation()
	if err := valid.validate(uuid.New()); err != nil {
		t.Fatalf("valid operation rejected: %v", err)
	}
	for _, tt := range []struct {
		name string
		op   ApprovedOperation
		key  uuid.UUID
	}{
		{"missing request key", valid, uuid.Nil},
		{"missing target", rehashed(func() ApprovedOperation { o := valid; o.TargetID = uuid.Nil; return o }()), uuid.New()},
		{"missing actor", rehashed(func() ApprovedOperation { o := valid; o.ActorID = uuid.Nil; return o }()), uuid.New()},
		{"missing request", rehashed(func() ApprovedOperation { o := valid; o.RequestID = uuid.Nil; return o }()), uuid.New()},
		{"missing definition", rehashed(func() ApprovedOperation { o := valid; o.DefinitionID = uuid.Nil; return o }()), uuid.New()},
		{"zero revision", rehashed(func() ApprovedOperation { o := valid; o.Revision = 0; return o }()), uuid.New()},
		{"zero config revision", rehashed(func() ApprovedOperation { o := valid; o.TargetConfigRevision = 0; return o }()), uuid.New()},
		{"config revision overflow", rehashed(func() ApprovedOperation { o := valid; o.TargetConfigRevision = maxTargetConfigRevision + 1; return o }()), uuid.New()},
		{"zero expected head", rehashed(func() ApprovedOperation { o := valid; head := uuid.Nil; o.ExpectedHeadID = &head; return o }()), uuid.New()},
		{"zero source execution", rehashed(func() ApprovedOperation { o := valid; source := uuid.Nil; o.SourceExecutionID = &source; return o }()), uuid.New()},
		{"unknown direction", rehashed(func() ApprovedOperation { o := valid; o.Direction = "merge"; return o }()), uuid.New()},
		{"batch without contract", rehashed(func() ApprovedOperation { o := valid; o.Mode = ModeBatch; return o }()), uuid.New()},
		{"empty SQL", rehashed(func() ApprovedOperation { o := valid; o.SQLSnapshot = ""; return o }()), uuid.New()},
		{"oversized SQL", rehashed(func() ApprovedOperation {
			o := valid
			o.SQLSnapshot = strings.Repeat("x", maxSnapshotBytes+1)
			return o
		}()), uuid.New()},
		{"oversized down SQL", rehashed(func() ApprovedOperation {
			o := valid
			o.DownSnapshot = strings.Repeat("x", maxSnapshotBytes+1)
			return o
		}()), uuid.New()},
		{"invalid SQL text", rehashed(func() ApprovedOperation { o := valid; o.SQLSnapshot = "\xff"; return o }()), uuid.New()},
		{"NUL in SQL", rehashed(func() ApprovedOperation { o := valid; o.SQLSnapshot = "SELECT 1\x00"; return o }()), uuid.New()},
		{"stale plan hash", func() ApprovedOperation { o := valid; o.PlanHash = strings.Repeat("0", 64); return o }(), uuid.New()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requireProblemCode(t, tt.op.validate(tt.key), problem.InvalidRequest)
		})
	}
}

func TestEnqueueRequiresCallerTransaction(t *testing.T) {
	store := &EnqueueStore{}
	_, err := store.Enqueue(context.Background(), nil, validApprovedOperation(), uuid.New())
	requireProblemCode(t, err, problem.InternalError)
}
