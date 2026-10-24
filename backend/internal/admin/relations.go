package admin

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AddMember is idempotent. It requires active resources only when it actually
// creates the relationship and then requires every live family to reauthenticate.
func (s *Service) AddMember(ctx context.Context, actor access.Principal, groupID, userID uuid.UUID) (bool, error) {
	return s.changeMember(ctx, actor, groupID, userID, true)
}

// RemoveMember is idempotent and revokes every family only after an existing
// relationship is removed. The bool tells an HTTP caller to redirect the actor.
func (s *Service) RemoveMember(ctx context.Context, actor access.Principal, groupID, userID uuid.UUID) (bool, error) {
	return s.changeMember(ctx, actor, groupID, userID, false)
}

func (s *Service) changeMember(ctx context.Context, actor access.Principal, groupID, userID uuid.UUID, add bool) (bool, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return false, err
	}
	if groupID == uuid.Nil || userID == uuid.Nil {
		return false, failure(problem.InvalidRequest)
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
	var groupActive bool
	err = tx.QueryRow(ctx, "SELECT is_active FROM databrain.groups WHERE id=$1 FOR UPDATE", groupID).Scan(&groupActive)
	groupMissing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !groupMissing {
		return false, failure(problem.PlatformUnavailable)
	}
	if err := lockUsersAndSessions(ctx, tx, actor, []uuid.UUID{userID}); err != nil {
		return false, err
	}
	if groupMissing {
		return false, ErrGroupNotFound
	}
	var userActive bool
	if err := tx.QueryRow(ctx, "SELECT is_active FROM databrain.users WHERE id=$1", userID).Scan(&userActive); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM databrain.memberships WHERE group_id=$1 AND user_id=$2)`, groupID, userID).Scan(&exists); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	if exists == add {
		if err := tx.Commit(ctx); err != nil {
			return false, failure(problem.PlatformUnavailable)
		}
		return false, nil
	}
	if add {
		if !groupActive || !userActive {
			return false, validationFailed()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$2)`, groupID, userID); err != nil {
			return false, failure(problem.PlatformUnavailable)
		}
		err = s.sessions.ReauthenticateUsers(ctx, tx, []uuid.UUID{userID})
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM databrain.memberships WHERE group_id=$1 AND user_id=$2`, groupID, userID); err != nil {
			return false, failure(problem.PlatformUnavailable)
		}
		err = s.sessions.RevokeUsers(ctx, tx, []uuid.UUID{userID})
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	return actor.UserID == userID, nil
}

// SetGrant is idempotent. Adding/upgrading access requires active resources;
// downgrade remains available so an admin can reduce access while inactive.
func (s *Service) SetGrant(ctx context.Context, actor access.Principal, groupID, targetID uuid.UUID, level access.Level) (bool, error) {
	return s.changeGrant(ctx, actor, groupID, targetID, level, false)
}

// RemoveGrant is idempotent for a missing relationship, not for missing parent
// resources. An actual removal revokes all current group members.
func (s *Service) RemoveGrant(ctx context.Context, actor access.Principal, groupID, targetID uuid.UUID) (bool, error) {
	return s.changeGrant(ctx, actor, groupID, targetID, "", true)
}

func (s *Service) changeGrant(ctx context.Context, actor access.Principal, groupID, targetID uuid.UUID, level access.Level, remove bool) (bool, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return false, err
	}
	if groupID == uuid.Nil || targetID == uuid.Nil {
		return false, failure(problem.InvalidRequest)
	}
	if !remove && level != access.ViewOnly && level != access.ViewEdit {
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
	var groupActive bool
	err = tx.QueryRow(ctx, "SELECT is_active FROM databrain.groups WHERE id=$1 FOR UPDATE", groupID).Scan(&groupActive)
	groupMissing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !groupMissing {
		return false, failure(problem.PlatformUnavailable)
	}
	var targetActive bool
	targetMissing := false
	if !groupMissing {
		err = tx.QueryRow(ctx, "SELECT is_active FROM databrain.database_targets WHERE id=$1 FOR UPDATE", targetID).Scan(&targetActive)
		targetMissing = errors.Is(err, pgx.ErrNoRows)
		if err != nil && !targetMissing {
			return false, failure(problem.PlatformUnavailable)
		}
	}
	var members []uuid.UUID
	if !groupMissing {
		members, err = groupMemberIDs(ctx, tx, groupID)
		if err != nil {
			return false, err
		}
	}
	if err := lockUsersAndSessions(ctx, tx, actor, members); err != nil {
		return false, err
	}
	if groupMissing {
		return false, ErrGroupNotFound
	}
	if targetMissing {
		return false, failure(problem.DatabaseNotFound)
	}
	var current access.Level
	err = tx.QueryRow(ctx, `SELECT access_level FROM databrain.database_grants WHERE group_id=$1 AND target_id=$2`, groupID, targetID).Scan(&current)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, failure(problem.PlatformUnavailable)
	}
	if (remove && !exists) || (!remove && exists && current == level) {
		if err := tx.Commit(ctx); err != nil {
			return false, failure(problem.PlatformUnavailable)
		}
		return false, nil
	}
	increase := !remove && (!exists || current == access.ViewOnly && level == access.ViewEdit)
	if increase && (!groupActive || !targetActive) {
		return false, validationFailed()
	}
	if remove {
		_, err = tx.Exec(ctx, `DELETE FROM databrain.database_grants WHERE group_id=$1 AND target_id=$2`, groupID, targetID)
	} else if exists {
		_, err = tx.Exec(ctx, `UPDATE databrain.database_grants SET access_level=$3,updated_at=clock_timestamp()
		    WHERE group_id=$1 AND target_id=$2`, groupID, targetID, level)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO databrain.database_grants(group_id,target_id,access_level) VALUES ($1,$2,$3)`, groupID, targetID, level)
	}
	if err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	if increase {
		err = s.sessions.ReauthenticateUsers(ctx, tx, members)
	} else {
		err = s.sessions.RevokeUsers(ctx, tx, members)
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	return slices.Contains(members, actor.UserID), nil
}
