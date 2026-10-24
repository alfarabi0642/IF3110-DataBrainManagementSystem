package admin

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type testAuthorizer struct {
	access.Authorizer
	err error
}

func (a testAuthorizer) RequireAdmin(context.Context, access.Principal) error { return a.err }

type testLifecycle struct{}

func (testLifecycle) RevokeUsers(context.Context, pgx.Tx, []uuid.UUID) error { return nil }
func (testLifecycle) ReauthenticateUsers(context.Context, pgx.Tx, []uuid.UUID) error {
	return nil
}

func TestServiceRejectsBeforeDatabase(t *testing.T) {
	if service, err := NewService(nil, nil); err == nil || service != nil {
		t.Fatal("nil pool accepted")
	}
	ctx := context.Background()
	actor := access.Principal{UserID: uuid.New(), SessionID: uuid.New(), SecurityVersion: 1}
	operations := []func(*Service) error{
		func(s *Service) error { _, err := s.CreateUser(ctx, actor, CreateUserInput{}); return err },
		func(s *Service) error { _, err := s.CreateGroup(ctx, actor, CreateGroupInput{}); return err },
		func(s *Service) error { _, _, err := s.ListUsers(ctx, actor, 0, 0); return err },
		func(s *Service) error { _, _, err := s.ListGroups(ctx, actor, 0, 0); return err },
		func(s *Service) error { _, _, err := s.User(ctx, actor, uuid.Nil); return err },
		func(s *Service) error {
			_, err := s.ResetPassword(ctx, actor, uuid.New(), ResetPasswordInput{})
			return err
		},
		func(s *Service) error { _, err := s.Group(ctx, actor, uuid.Nil); return err },
		func(s *Service) error { _, err := s.EditGroup(ctx, actor, uuid.Nil, EditGroupInput{}); return err },
		func(s *Service) error { _, err := s.AddMember(ctx, actor, uuid.Nil, uuid.New()); return err },
		func(s *Service) error { _, err := s.RemoveMember(ctx, actor, uuid.New(), uuid.Nil); return err },
		func(s *Service) error { _, err := s.RemoveGrant(ctx, actor, uuid.Nil, uuid.New()); return err },
	}
	for _, code := range []problem.Code{problem.SessionInvalid, problem.AdminRequired, problem.PlatformUnavailable} {
		service := &Service{authorizer: testAuthorizer{err: failure(code)}, sessions: testLifecycle{}}
		for _, operation := range operations {
			requireAdminCode(t, operation(service), code)
		}
		_, err := service.SetGrant(ctx, actor, uuid.New(), uuid.New(), "owner")
		requireAdminCode(t, err, code)
	}
	service := &Service{authorizer: testAuthorizer{}, sessions: testLifecycle{}}
	for i, operation := range operations {
		want := problem.ValidationFailed
		if i >= 2 {
			want = problem.InvalidRequest
		}
		requireAdminCode(t, operation(service), want)
	}
	requireAdminCode(t, func() error {
		_, err := service.SetGrant(ctx, actor, uuid.New(), uuid.New(), "owner")
		return err
	}(), problem.ValidationFailed)
	pool, err := pgxpool.New(ctx, "postgres://unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	authorizer, err := access.NewAuthorizer(pool)
	if err != nil {
		t.Fatal(err)
	}
	service = &Service{pool: pool, authorizer: authorizer, sessions: testLifecycle{}}
	for _, operation := range operations {
		requireAdminCode(t, operation(service), problem.PlatformUnavailable)
	}
}

func TestCreateErrorsDoNotExposeDriverDetails(t *testing.T) {
	for _, test := range []struct {
		table, constraint, code string
		want                    error
	}{
		{"users", "users_username_key", "23505", ErrUsernameTaken},
		{"groups", "groups_name_lower_unique", "23505", ErrGroupNameTaken},
		{"users", "users_pkey", "23505", nil},
		{"groups", "groups_name_lower_unique", "23503", nil},
	} {
		err := createError(fmt.Errorf("wrapped: %w", &pgconn.PgError{SchemaName: "databrain", TableName: test.table,
			ConstraintName: test.constraint, Code: test.code, Message: "PRIVATE-driver-detail"}))
		if test.want != nil {
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v; want %v", err, test.want)
			}
		} else {
			requireAdminCode(t, err, problem.PlatformUnavailable)
		}
	}
}
