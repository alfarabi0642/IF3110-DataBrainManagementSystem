package admin

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type userSecurityEffect uint8

const (
	preserveUserSessions userSecurityEffect = iota
	revokeUserSessions
	reauthenticateUserSessions
)

func classifyUserEdit(active, admin bool, in EditUserInput) userSecurityEffect {
	statusChanged, roleChanged := active != in.IsActive, admin != in.IsPlatformAdmin
	if active && !in.IsActive {
		return revokeUserSessions
	}
	if !active && in.IsActive {
		return reauthenticateUserSessions
	}
	if !active {
		return preserveUserSessions
	}
	if roleChanged && in.IsPlatformAdmin {
		return reauthenticateUserSessions
	}
	if statusChanged || roleChanged {
		return revokeUserSessions
	}
	return preserveUserSessions
}

func (s *Service) User(ctx context.Context, actor access.Principal, userID uuid.UUID) (UserItem, []GroupItem, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return UserItem{}, nil, err
	}
	if userID == uuid.Nil {
		return UserItem{}, nil, failure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return UserItem{}, nil, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	var user UserItem
	err = lease.QueryRow(ctx, `SELECT id::text,username,display_name,is_active,is_platform_admin,revision
	    FROM databrain.users WHERE id=$1`, userID).
		Scan(&user.ID, &user.Username, &user.DisplayName, &user.IsActive, &user.IsAdmin, &user.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserItem{}, nil, ErrUserNotFound
	}
	if err != nil {
		return UserItem{}, nil, failure(problem.PlatformUnavailable)
	}
	rows, err := lease.Query(ctx, `SELECT g.id::text,g.name,g.is_active,g.revision
	    FROM databrain.groups g JOIN databrain.memberships m ON m.group_id=g.id
	    WHERE m.user_id=$1 ORDER BY g.name,g.id`, userID)
	if err != nil {
		return UserItem{}, nil, failure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	groups := make([]GroupItem, 0)
	for rows.Next() {
		var group GroupItem
		if err := rows.Scan(&group.ID, &group.Name, &group.IsActive, &group.Revision); err != nil {
			return UserItem{}, nil, failure(problem.PlatformUnavailable)
		}
		groups = append(groups, group)
	}
	if rows.Err() != nil {
		return UserItem{}, nil, failure(problem.PlatformUnavailable)
	}
	return user, groups, nil
}

// EditUser treats deactivation as a reduction even when the submitted role is
// promoted, and reactivation as an increase even when the role is demoted.
// This prevents a combined full-form edit from weakening its session effect.
func (s *Service) EditUser(ctx context.Context, actor access.Principal, userID uuid.UUID, in EditUserInput) (bool, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return false, err
	}
	displayName, err := auth.NormalizeDisplayName(in.DisplayName)
	if userID == uuid.Nil || in.ExpectedRevision < 1 {
		return false, failure(problem.InvalidRequest)
	}
	if err != nil {
		return false, validationFailed()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	defer rollback(tx)
	if err := lockGuard(ctx, tx); err != nil {
		return false, err
	}
	if err := lockUsersAndSessions(ctx, tx, actor, []uuid.UUID{userID}); err != nil {
		return false, err
	}
	var currentName string
	var active, admin bool
	var revision int64
	err = tx.QueryRow(ctx, `SELECT display_name,is_active,is_platform_admin,revision
	    FROM databrain.users WHERE id=$1`, userID).Scan(&currentName, &active, &admin, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrUserNotFound
	}
	if err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	if revision != in.ExpectedRevision {
		return false, ErrRevisionConflict
	}
	effect := classifyUserEdit(active, admin, in)
	if currentName == displayName && effect == preserveUserSessions {
		if err := tx.Commit(ctx); err != nil {
			return false, failure(problem.PlatformUnavailable)
		}
		return false, nil
	}
	if admin && effect == revokeUserSessions {
		var activeAdmins int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM databrain.users
		    WHERE is_active AND is_platform_admin`).Scan(&activeAdmins); err != nil {
			return false, failure(problem.PlatformUnavailable)
		}
		if activeAdmins <= 1 {
			return false, ErrLastAdmin
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.users SET display_name=$2,is_active=$3,
	    is_platform_admin=$4,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`,
		userID, displayName, in.IsActive, in.IsPlatformAdmin); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	if effect == reauthenticateUserSessions {
		err = s.sessions.ReauthenticateUsers(ctx, tx, []uuid.UUID{userID})
	} else if effect == revokeUserSessions {
		err = s.sessions.RevokeUsers(ctx, tx, []uuid.UUID{userID})
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	return effect != preserveUserSessions && actor.UserID == userID, nil
}

// ResetPassword returns true when the acting admin reset their own password;
// the HTTP caller must redirect that committed request to /login.
func (s *Service) ResetPassword(ctx context.Context, actor access.Principal, userID uuid.UUID, in ResetPasswordInput) (bool, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return false, err
	}
	if userID == uuid.Nil || in.ExpectedRevision < 1 {
		return false, failure(problem.InvalidRequest)
	}
	if auth.ValidatePassword(in.Password) != nil {
		return false, validationFailed()
	}
	hash, err := auth.HashPassword(ctx, in.Password)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	defer rollback(tx)
	if err := lockGuard(ctx, tx); err != nil {
		return false, err
	}
	if err := lockUsersAndSessions(ctx, tx, actor, []uuid.UUID{userID}); err != nil {
		return false, err
	}
	var revision int64
	if err := tx.QueryRow(ctx, "SELECT revision FROM databrain.users WHERE id=$1", userID).Scan(&revision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrUserNotFound
		}
		return false, failure(problem.PlatformUnavailable)
	}
	if revision != in.ExpectedRevision {
		return false, ErrRevisionConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.users SET password_hash=$2,revision=revision+1,
	    updated_at=clock_timestamp() WHERE id=$1`, userID, hash); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	if err := s.sessions.RevokeUsers(ctx, tx, []uuid.UUID{userID}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	return actor.UserID == userID, nil
}
