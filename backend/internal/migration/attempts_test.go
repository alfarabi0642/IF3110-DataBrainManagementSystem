package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAttemptArgumentsAndCreator(t *testing.T) {
	s := NewAttemptStore(nil)
	ctx := context.Background()
	id, key := uuid.New(), uuid.New()
	for _, ids := range [][3]uuid.UUID{{uuid.Nil, id, id}, {id, uuid.Nil, id}, {id, id, uuid.Nil}} {
		_, err := s.Reserve(ctx, ids[0], ids[1], ids[2], ReserveInput{1, key})
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	for _, input := range []ReserveInput{{0, key}, {-1, key}, {1, uuid.Nil}} {
		_, err := s.Reserve(ctx, id, id, id, input)
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	for _, ids := range [][3]uuid.UUID{{uuid.Nil, id, key}, {id, uuid.Nil, key}, {id, id, uuid.Nil}} {
		_, err := s.Receipt(ctx, ids[0], ids[1], ids[2])
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	r := AttemptReceipt{ID: id, DraftID: uuid.New(), DraftRevision: 3}
	replay, err := matchReservation(r, r.DraftID, 3)
	if err != nil || replay.Created() || replay.Receipt != r {
		t.Fatal("matching receipt must be replay-only")
	}
	for _, request := range []struct {
		draft uuid.UUID
		rev   int64
	}{{uuid.New(), 3}, {r.DraftID, 4}} {
		_, err := matchReservation(r, request.draft, request.rev)
		requireDraftCode(t, err, problem.IdempotencyConflict)
	}
	for _, reservation := range []Reservation{{}, replay, NewAttemptStore(nil).created(r)} {
		claimed, err := s.Claim(ctx, reservation)
		if err != nil || claimed {
			t.Fatal("non-creator must not reach the database or claim")
		}
	}
}

func TestAttemptErrorsAreSafe(t *testing.T) {
	for _, tc := range []struct {
		constraint string
		want       problem.Code
	}{{"one_unresolved_attempt_per_target", problem.TargetBusy}, {"one_success_per_draft_revision", problem.DraftAlreadyExecuted}, {"other", problem.PlatformUnavailable}} {
		err := attemptStoreError(&pgconn.PgError{Code: "23505", ConstraintName: tc.constraint, Message: "private SQL", Detail: "private DSN"})
		requireDraftCode(t, err, tc.want)
	}
	requireDraftCode(t, attemptStoreError(errors.New("private credentials")), problem.PlatformUnavailable)
}
