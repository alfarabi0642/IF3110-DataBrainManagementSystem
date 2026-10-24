package auth

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

type loginUser struct {
	id              uuid.UUID
	passwordHash    string
	active          bool
	securityVersion int64
}

// Login verifies outside locks, then rechecks the user and old family before
// revoking it and inserting the authenticated family in one transaction.
func (s *Service) Login(ctx context.Context, previous Session, username, password string) (Session, string, error) {
	if previous.ID == uuid.Nil || previous.Kind != AnonymousSession && previous.Kind != AuthenticatedSession {
		return Session{}, "", sessionFailure(problem.SessionInvalid)
	}
	if previous.Kind == AuthenticatedSession && !previous.ReauthRequired {
		return Session{}, "", ErrAlreadyAuthenticated
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	var user loginUser
	err = lease.QueryRow(ctx, `SELECT id,password_hash,is_active,security_version
	    FROM databrain.users WHERE username=$1`, username).
		Scan(&user.id, &user.passwordHash, &user.active, &user.securityVersion)
	lease.Release()
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !user.active {
		if dummyErr := CheckDummyPassword(ctx, password); dummyErr != nil && !errors.Is(dummyErr, ErrInvalidPassword) {
			return Session{}, "", loginHashError(dummyErr)
		}
		return Session{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	match, err := VerifyPassword(ctx, password, user.passwordHash)
	if err != nil {
		if errors.Is(err, ErrInvalidPassword) {
			return Session{}, "", ErrInvalidCredentials
		}
		return Session{}, "", loginHashError(err)
	}
	if !match {
		return Session{}, "", ErrInvalidCredentials
	}

	lease, err = platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	defer rollbackAuth(tx)
	var current loginUser
	current.id = user.id
	err = tx.QueryRow(ctx, `SELECT password_hash,is_active,security_version
	    FROM databrain.users WHERE id=$1 FOR UPDATE`, user.id).
		Scan(&current.passwordHash, &current.active, &current.securityVersion)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!current.active || current.passwordHash != user.passwordHash || current.securityVersion != user.securityVersion) {
		return Session{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	if err := recheckLoginFamily(ctx, tx, previous, user); err != nil {
		return Session{}, "", err
	}
	if err := s.RevokeSession(ctx, tx, previous.ID); err != nil {
		return Session{}, "", err
	}
	state, token, err := s.insertSession(ctx, tx, AuthenticatedSession, user.id, user.securityVersion)
	if err != nil {
		return Session{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	return state, token, nil
}

func loginHashError(err error) error {
	if errors.Is(err, ErrPasswordHashBusy) {
		return sessionFailure(problem.AuthBusy)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return sessionFailure(problem.InternalError)
}

func recheckLoginFamily(ctx context.Context, tx pgx.Tx, previous Session, user loginUser) error {
	var kind SessionKind
	var oldUser pgtype.UUID
	var version pgtype.Int8
	var lastSeen, absolute, now time.Time
	var revoked pgtype.Timestamptz
	var reauth bool
	err := tx.QueryRow(ctx, `SELECT kind,user_id,security_version,last_seen_at,absolute_expires_at,
	    revoked_at,reauth_required,clock_timestamp() FROM databrain.sessions WHERE id=$1 FOR UPDATE`, previous.ID).
		Scan(&kind, &oldUser, &version, &lastSeen, &absolute, &revoked, &reauth, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionFailure(problem.SessionInvalid)
	}
	if err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	if revoked.Valid || !now.Before(absolute) || kind != previous.Kind {
		return sessionFailure(problem.SessionInvalid)
	}
	if kind == AnonymousSession {
		if oldUser.Valid || version.Valid || reauth {
			return sessionFailure(problem.SessionInvalid)
		}
		return nil
	}
	if !reauth || !oldUser.Valid || uuid.UUID(oldUser.Bytes) != user.id || !version.Valid ||
		version.Int64 != user.securityVersion || !now.Before(lastSeen.Add(authenticatedIdle)) {
		return sessionFailure(problem.SessionInvalid)
	}
	return nil
}
