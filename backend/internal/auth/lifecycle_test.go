package auth

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func unitSessionService(t *testing.T) *Service {
	t.Helper()
	secrets, err := secret.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{secrets: secrets}
}

func TestSessionStateExpiryAndIdentity(t *testing.T) {
	service := unitSessionService(t)
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	base := sessionRow{id: uuid.New(), kind: AuthenticatedSession, now: now,
		absolute: now.Add(time.Hour), validTo: now.Add(time.Minute), lastSeen: now,
		userID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, userActive: true,
		sessionVersion: pgtype.Int8{Int64: 1, Valid: true}, userVersion: pgtype.Int8{Int64: 1, Valid: true}}
	for _, tt := range []struct {
		name        string
		change      func(*sessionRow)
		allowReauth bool
		want        error
	}{
		{"valid", func(*sessionRow) {}, false, nil},
		{"absolute boundary", func(v *sessionRow) { v.absolute = now }, false, sessionFailure(problem.SessionInvalid)},
		{"alias boundary", func(v *sessionRow) { v.validTo = now }, false, sessionFailure(problem.SessionInvalid)},
		{"idle boundary", func(v *sessionRow) { v.lastSeen = now.Add(-authenticatedIdle) }, false, sessionFailure(problem.SessionInvalid)},
		{"before idle boundary", func(v *sessionRow) { v.lastSeen = now.Add(-authenticatedIdle + time.Nanosecond) }, false, nil},
		{"revoked", func(v *sessionRow) { v.revoked.Valid = true }, true, sessionFailure(problem.SessionInvalid)},
		{"inactive user", func(v *sessionRow) { v.userActive = false }, true, sessionFailure(problem.SessionInvalid)},
		{"changed version", func(v *sessionRow) { v.userVersion.Int64++ }, true, sessionFailure(problem.SessionInvalid)},
		{"missing user", func(v *sessionRow) { v.userID.Valid = false }, false, sessionFailure(problem.SessionInvalid)},
		{"reauth protected", func(v *sessionRow) { v.reauth = true }, false, ErrReauthRequired},
		{"reauth login", func(v *sessionRow) { v.reauth = true }, true, nil},
		{"anonymous", func(v *sessionRow) { v.kind = AnonymousSession; v.userID.Valid = false; v.userActive = false }, false, nil},
		{"unknown kind", func(v *sessionRow) { v.kind = "unknown" }, false, sessionFailure(problem.SessionInvalid)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			value := base
			tt.change(&value)
			state, err := service.sessionState(value, tt.allowReauth)
			if tt.want == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if tt.want == ErrReauthRequired {
				if !errors.Is(err, tt.want) || !state.ReauthRequired {
					t.Fatal("reauth state was lost")
				}
			} else {
				requireProblem(t, err, problem.SessionInvalid)
				if state.ID != uuid.Nil {
					t.Fatal("invalid identity returned")
				}
				return
			}
			if state.ID != base.id || len(state.CSRF) != 43 {
				t.Fatal("valid session lost its family")
			}
			if value.kind == AnonymousSession && state.Principal.UserID != uuid.Nil {
				t.Fatal("anonymous has a principal")
			}
		})
	}
}

// Only Exec is implemented: rotation must not commit or roll back this transaction.
type rotationTx struct {
	pgx.Tx
	failAt int
	calls  [][]any
}

func (tx *rotationTx) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	tx.calls = append(tx.calls, args)
	if len(tx.calls) == tx.failAt {
		return pgconn.CommandTag{}, errors.New("SENSITIVE-driver-error")
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func TestRotationPreservesAbsoluteAndFailsClosed(t *testing.T) {
	service := unitSessionService(t)
	now := time.Now().UTC()
	for _, lifetime := range []time.Duration{time.Minute, time.Hour} {
		value := sessionRow{id: uuid.New(), currentHash: bytes.Repeat([]byte{1}, 32), now: now, absolute: now.Add(lifetime)}
		tx := &rotationTx{}
		token, err := service.rotateSession(context.Background(), tx, value)
		if err != nil || len(tx.calls) != 3 {
			t.Fatalf("rotation calls=%d error=%v", len(tx.calls), err)
		}
		raw, err := decodeToken(token)
		if err != nil {
			t.Fatal(err)
		}
		hash := tokenHash(raw)
		if !bytes.Equal(tx.calls[1][0].([]byte), hash[:]) || tx.calls[1][2] != value.absolute {
			t.Fatal("successor hash or expiry changed")
		}
		aliasUntil := now.Add(min(lifetime, aliasGrace))
		if tx.calls[0][1] != aliasUntil {
			t.Fatal("alias exceeded grace or absolute expiry")
		}
		opened, err := service.secrets.OpenSession(value.id, tx.calls[2][2].([]byte))
		if err != nil || !bytes.Equal(opened, raw) {
			t.Fatal("successor cannot be repaired")
		}
		for failAt := 1; failAt <= 3; failAt++ {
			failed := &rotationTx{failAt: failAt}
			token, err := service.rotateSession(context.Background(), failed, value)
			requireProblem(t, err, problem.PlatformUnavailable)
			if token != "" || len(failed.calls) != failAt {
				t.Fatal("rotation continued or published token after failure")
			}
		}
	}
}

func TestLifecycleRejectsInvalidInputBeforeSQL(t *testing.T) {
	service := &Service{}
	ctx := context.Background()
	requireProblem(t, service.RevokeSession(ctx, nil, uuid.New()), problem.InvalidRequest)
	requireProblem(t, service.RevokeSession(ctx, &rotationTx{}, uuid.Nil), problem.InvalidRequest)
	for _, operation := range []func(context.Context, pgx.Tx, []uuid.UUID) error{service.RevokeUsers, service.ReauthenticateUsers} {
		requireProblem(t, operation(ctx, nil, []uuid.UUID{uuid.New()}), problem.InvalidRequest)
		requireProblem(t, operation(ctx, &rotationTx{}, []uuid.UUID{uuid.Nil}), problem.InvalidRequest)
		if err := operation(ctx, &rotationTx{}, nil); err != nil {
			t.Fatal("empty affected set should do nothing")
		}
	}
}
