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

// ListUsers memuat metadata aman akun aktif dan nonaktif. Nilai bool menandakan halaman berikutnya;
// handler menentukan limit default 50.
func (s *Service) ListUsers(ctx context.Context, actor access.Principal, limit, offset int) ([]UserItem, bool, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return nil, false, err
	}
	if limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, failure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	rows, err := lease.Query(ctx, `SELECT id::text,username,display_name,is_active,is_platform_admin,revision
	    FROM databrain.users ORDER BY username,id LIMIT $1 OFFSET $2`, limit+1, offset)
	if err != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]UserItem, 0, limit)
	for rows.Next() {
		var item UserItem
		if err := rows.Scan(&item.ID, &item.Username, &item.DisplayName, &item.IsActive, &item.IsAdmin, &item.Revision); err != nil {
			return nil, false, failure(problem.PlatformUnavailable)
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
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
	rows, err := lease.Query(ctx, `SELECT `+groupColumns+`
	    FROM databrain.groups g JOIN databrain.memberships m ON m.group_id=g.id
	    LEFT JOIN databrain.users u ON u.id=g.leader_id
	    WHERE m.user_id=$1 ORDER BY g.name,g.id`, userID)
	if err != nil {
		return UserItem{}, nil, failure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	groups := make([]GroupItem, 0)
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return UserItem{}, nil, failure(problem.PlatformUnavailable)
		}
		groups = append(groups, group)
	}
	if rows.Err() != nil {
		return UserItem{}, nil, failure(problem.PlatformUnavailable)
	}
	return user, groups, nil
}

// EditUser mendahulukan penonaktifan sebagai pengurangan akses dan reaktivasi sebagai peningkatan
// akses. Perubahan role bersamaan tidak boleh melemahkan efek session.
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
	var selfAffected bool
	err = s.withGuardTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var ledGroups []uuid.UUID
		if !in.IsActive {
			var err error
			ledGroups, err = lockGroupsLedBy(ctx, tx, userID)
			if err != nil {
				return err
			}
		}
		if err := lockUsersAndSessions(ctx, tx, actor, []uuid.UUID{userID}); err != nil {
			return err
		}
		var currentName string
		var active, admin bool
		var revision int64
		err := tx.QueryRow(ctx, `SELECT display_name,is_active,is_platform_admin,revision
		    FROM databrain.users WHERE id=$1`, userID).Scan(&currentName, &active, &admin, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return failure(problem.PlatformUnavailable)
		}
		if revision != in.ExpectedRevision {
			return ErrRevisionConflict
		}
		effect := classifyUserEdit(active, admin, in)
		if currentName == displayName && active == in.IsActive && admin == in.IsPlatformAdmin {
			return nil
		}
		if admin && effect == revokeUserSessions {
			var activeAdmins int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM databrain.users
			    WHERE is_active AND is_platform_admin`).Scan(&activeAdmins); err != nil {
				return failure(problem.PlatformUnavailable)
			}
			if activeAdmins <= 1 {
				return ErrLastAdmin
			}
		}
		if active && !in.IsActive && len(ledGroups) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE databrain.groups SET leader_id=NULL,revision=revision+1,
			    updated_at=clock_timestamp() WHERE id=ANY($1::uuid[])`, ledGroups); err != nil {
				return failure(problem.PlatformUnavailable)
			}
			targets, err := targetIDsForGroups(ctx, tx, ledGroups)
			if err != nil {
				return err
			}
			if err := advancePolicyGenerations(ctx, tx, targets); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE databrain.users SET display_name=$2,is_active=$3,
		    is_platform_admin=$4,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`,
			userID, displayName, in.IsActive, in.IsPlatformAdmin); err != nil {
			return failure(problem.PlatformUnavailable)
		}
		if effect == reauthenticateUserSessions {
			err = s.sessions.ReauthenticateUsers(ctx, tx, []uuid.UUID{userID})
		} else if effect == revokeUserSessions {
			err = s.sessions.RevokeUsers(ctx, tx, []uuid.UUID{userID})
		}
		if err != nil {
			return err
		}
		selfAffected = effect != preserveUserSessions && actor.UserID == userID
		return nil
	})
	if err != nil {
		return false, err
	}
	return selfAffected, nil
}

// ResetPassword mengembalikan true jika admin mereset password sendiri. Handler harus mengarahkan ke
// /login setelah transaksi berhasil.
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
	err = s.withGuardTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockUsersAndSessions(ctx, tx, actor, []uuid.UUID{userID}); err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, "SELECT revision FROM databrain.users WHERE id=$1", userID).Scan(&revision); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUserNotFound
			}
			return failure(problem.PlatformUnavailable)
		}
		if revision != in.ExpectedRevision {
			return ErrRevisionConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE databrain.users SET password_hash=$2,revision=revision+1,
		    updated_at=clock_timestamp() WHERE id=$1`, userID, hash); err != nil {
			return failure(problem.PlatformUnavailable)
		}
		return s.sessions.RevokeUsers(ctx, tx, []uuid.UUID{userID})
	})
	if err != nil {
		return false, err
	}
	return actor.UserID == userID, nil
}
