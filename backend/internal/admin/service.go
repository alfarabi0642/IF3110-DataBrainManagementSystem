package admin

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUsernameTaken        = errors.New("username already exists")
	ErrGroupNameTaken       = errors.New("group name already exists")
	ErrPlatformLimitReached = errors.New("platform account or group limit reached")
	ErrUserNotFound         = errors.New("user not found")
	ErrGroupNotFound        = errors.New("group not found")
	ErrRevisionConflict     = &problem.Error{Code: problem.RevisionConflict}
)

type sessionLifecycle interface {
	RevokeUsers(context.Context, pgx.Tx, []uuid.UUID) error
	ReauthenticateUsers(context.Context, pgx.Tx, []uuid.UUID) error
}

type Service struct {
	pool       *pgxpool.Pool
	authorizer access.Authorizer
	sessions   sessionLifecycle
}

func NewService(pool *pgxpool.Pool, sessions *auth.Service) (*Service, error) {
	if sessions == nil {
		return nil, errors.New("admin: session lifecycle is required")
	}
	authorizer, err := access.NewAuthorizer(pool)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, authorizer: authorizer, sessions: sessions}, nil
}

func failure(code problem.Code) error { return &problem.Error{Code: code} }

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// CreateUser hashes outside the transaction, then rechecks the actor under
// locks. No existing user's privileges or sessions change when an account is created.
func (s *Service) CreateUser(ctx context.Context, actor access.Principal, in CreateUserInput) (uuid.UUID, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return uuid.Nil, err
	}
	in, err := validateCreateUser(in)
	if err != nil {
		return uuid.Nil, err
	}
	hash, err := auth.HashPassword(ctx, in.Password)
	if err != nil {
		return uuid.Nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return uuid.Nil, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return uuid.Nil, failure(problem.PlatformUnavailable)
	}
	defer rollback(tx)
	if err := lockAdmin(ctx, tx, actor); err != nil {
		return uuid.Nil, err
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM databrain.users").Scan(&count); err != nil {
		return uuid.Nil, failure(problem.PlatformUnavailable)
	}
	if count >= 1000 {
		return uuid.Nil, ErrPlatformLimitReached
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return uuid.Nil, failure(problem.InternalError)
	}
	_, err = tx.Exec(ctx, `INSERT INTO databrain.users(id,username,display_name,password_hash,is_platform_admin)
	    VALUES ($1,$2,$3,$4,$5)`, id, in.Username, in.DisplayName, hash, in.IsPlatformAdmin)
	if err != nil {
		return uuid.Nil, createError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, failure(problem.PlatformUnavailable)
	}
	return id, nil
}

func (s *Service) CreateGroup(ctx context.Context, actor access.Principal, in CreateGroupInput) (uuid.UUID, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return uuid.Nil, err
	}
	name, err := normalizeGroupName(in.Name)
	if err != nil {
		return uuid.Nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return uuid.Nil, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return uuid.Nil, failure(problem.PlatformUnavailable)
	}
	defer rollback(tx)
	if err := lockAdmin(ctx, tx, actor); err != nil {
		return uuid.Nil, err
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM databrain.groups").Scan(&count); err != nil {
		return uuid.Nil, failure(problem.PlatformUnavailable)
	}
	if count >= 100 {
		return uuid.Nil, ErrPlatformLimitReached
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return uuid.Nil, failure(problem.InternalError)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO databrain.groups(id,name) VALUES ($1,$2)", id, name); err != nil {
		return uuid.Nil, createError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, failure(problem.PlatformUnavailable)
	}
	return id, nil
}

func lockAdmin(ctx context.Context, tx pgx.Tx, actor access.Principal) error {
	if err := lockGuard(ctx, tx); err != nil {
		return err
	}
	return lockUsersAndSessions(ctx, tx, actor, nil)
}

func lockGuard(ctx context.Context, tx pgx.Tx) error {
	var guard bool
	if err := tx.QueryRow(ctx, "SELECT singleton FROM databrain.security_guard WHERE singleton FOR UPDATE").Scan(&guard); err != nil || !guard {
		return failure(problem.PlatformUnavailable)
	}
	return nil
}

func lockUsersAndSessions(ctx context.Context, tx pgx.Tx, actor access.Principal, affected []uuid.UUID) error {
	ids := append(slices.Clone(affected), actor.UserID)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)
	rows, err := tx.Query(ctx, "SELECT id FROM databrain.users WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE", ids)
	if err != nil {
		return failure(problem.PlatformUnavailable)
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return failure(problem.PlatformUnavailable)
	}
	if !slices.Contains(users, actor.UserID) {
		return failure(problem.SessionInvalid)
	}
	rows, err = tx.Query(ctx, `SELECT id FROM databrain.sessions
	    WHERE (user_id=ANY($1::uuid[]) AND kind='authenticated' AND revoked_at IS NULL) OR id=$2
	    ORDER BY id FOR UPDATE`, ids, actor.SessionID)
	if err != nil {
		return failure(problem.PlatformUnavailable)
	}
	sessions, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return failure(problem.PlatformUnavailable)
	}
	if !slices.Contains(sessions, actor.SessionID) {
		return failure(problem.SessionInvalid)
	}
	if err := access.RequireAdminIn(ctx, tx, actor); err != nil {
		return err
	}
	if len(users) != len(ids) {
		return ErrUserNotFound
	}
	return nil
}

func createError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" && pgerr.SchemaName == "databrain" {
		switch {
		case pgerr.TableName == "users" && pgerr.ConstraintName == "users_username_key":
			return ErrUsernameTaken
		case pgerr.TableName == "groups" && pgerr.ConstraintName == "groups_name_lower_unique":
			return ErrGroupNameTaken
		}
	}
	return failure(problem.PlatformUnavailable)
}
