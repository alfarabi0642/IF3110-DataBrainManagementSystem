package admin

import (
	"context"
	"errors"
	"slices"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AddMember bersifat idempoten. Relasi baru memerlukan resource aktif dan mewajibkan reauthentication
// pada seluruh family yang masih hidup.
func (s *Service) AddMember(ctx context.Context, actor access.Principal, groupID, userID uuid.UUID) (bool, error) {
	return s.changeMember(ctx, actor, groupID, userID, true)
}

// RemoveMember mencabut seluruh family setelah membership benar-benar dihapus. Nilai bool meminta
// handler mengarahkan actor ke login.
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
	var selfAffected bool
	err := s.withGuardTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var groupActive bool
		var leader string
		err := tx.QueryRow(ctx, `SELECT is_active,coalesce(leader_id::text,'') FROM databrain.groups
		    WHERE id=$1 FOR UPDATE`, groupID).Scan(&groupActive, &leader)
		groupMissing := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !groupMissing {
			return failure(problem.PlatformUnavailable)
		}
		if err := lockUsersAndSessions(ctx, tx, actor, []uuid.UUID{userID}); err != nil {
			return err
		}
		if groupMissing {
			return ErrGroupNotFound
		}
		var userActive bool
		if err := tx.QueryRow(ctx, "SELECT is_active FROM databrain.users WHERE id=$1", userID).Scan(&userActive); err != nil {
			return failure(problem.PlatformUnavailable)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM databrain.memberships WHERE group_id=$1 AND user_id=$2)`, groupID, userID).Scan(&exists); err != nil {
			return failure(problem.PlatformUnavailable)
		}
		if exists == add {
			return nil
		}
		if add {
			if !groupActive || !userActive {
				return validationFailed()
			}
			if _, err := tx.Exec(ctx, `INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$2)`, groupID, userID); err != nil {
				return failure(problem.PlatformUnavailable)
			}
			err = s.sessions.ReauthenticateUsers(ctx, tx, []uuid.UUID{userID})
		} else {
			if leader == userID.String() {
				targets, err := groupGrantTargetIDs(ctx, tx, groupID)
				if err != nil {
					return err
				}
				if err := advancePolicyGenerations(ctx, tx, targets); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE databrain.groups SET leader_id=NULL,revision=revision+1,
				    updated_at=clock_timestamp() WHERE id=$1`, groupID); err != nil {
					return failure(problem.PlatformUnavailable)
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM databrain.memberships WHERE group_id=$1 AND user_id=$2`, groupID, userID); err != nil {
				return failure(problem.PlatformUnavailable)
			}
			err = s.sessions.RevokeUsers(ctx, tx, []uuid.UUID{userID})
		}
		if err != nil {
			return err
		}
		selfAffected = actor.UserID == userID
		return nil
	})
	if err != nil {
		return false, err
	}
	return selfAffected, nil
}

// SetGrant bersifat idempoten. Penambahan atau peningkatan akses memerlukan resource aktif; penurunan
// akses tetap diperbolehkan saat nonaktif.
func (s *Service) SetGrant(ctx context.Context, actor access.Principal, groupID, targetID uuid.UUID, level access.Level) (bool, error) {
	return s.changeGrant(ctx, actor, groupID, targetID, level, false)
}

// RemoveGrant menerima relasi yang sudah tidak ada, tetapi parent harus ada. Penghapusan grant
// mencabut session seluruh anggota group.
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
	var selfAffected bool
	err := s.withGuardTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var groupActive bool
		err := tx.QueryRow(ctx, "SELECT is_active FROM databrain.groups WHERE id=$1 FOR UPDATE", groupID).Scan(&groupActive)
		groupMissing := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !groupMissing {
			return failure(problem.PlatformUnavailable)
		}
		var members []uuid.UUID
		if !groupMissing {
			members, err = groupMemberIDs(ctx, tx, groupID)
			if err != nil {
				return err
			}
		}
		if err := lockUsersAndSessions(ctx, tx, actor, members); err != nil {
			return err
		}
		var targetActive bool
		targetMissing := false
		if !groupMissing {
			err = tx.QueryRow(ctx, "SELECT is_active FROM databrain.database_targets WHERE id=$1 FOR UPDATE", targetID).Scan(&targetActive)
			targetMissing = errors.Is(err, pgx.ErrNoRows)
			if err != nil && !targetMissing {
				return failure(problem.PlatformUnavailable)
			}
		}
		if groupMissing {
			return ErrGroupNotFound
		}
		if targetMissing {
			return failure(problem.DatabaseNotFound)
		}
		var current access.Level
		err = tx.QueryRow(ctx, `SELECT access_level FROM databrain.database_grants WHERE group_id=$1 AND target_id=$2`, groupID, targetID).Scan(&current)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return failure(problem.PlatformUnavailable)
		}
		if (remove && !exists) || (!remove && exists && current == level) {
			return nil
		}
		increase := !remove && (!exists || current == access.ViewOnly && level == access.ViewEdit)
		if increase && (!groupActive || !targetActive) {
			return validationFailed()
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
			return failure(problem.PlatformUnavailable)
		}
		if err := advancePolicyGenerations(ctx, tx, []uuid.UUID{targetID}); err != nil {
			return err
		}
		if increase {
			err = s.sessions.ReauthenticateUsers(ctx, tx, members)
		} else {
			err = s.sessions.RevokeUsers(ctx, tx, members)
		}
		if err != nil {
			return err
		}
		selfAffected = slices.Contains(members, actor.UserID)
		return nil
	})
	if err != nil {
		return false, err
	}
	return selfAffected, nil
}
