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

const groupColumns = `g.id::text,g.name,g.is_active,g.revision,coalesce(g.leader_id::text,''),
    coalesce(u.username,''),coalesce(u.display_name,'')`
const groupJoin = ` FROM databrain.groups g LEFT JOIN databrain.users u ON u.id=g.leader_id`

func scanGroup(row pgx.Row) (GroupItem, error) {
	var group GroupItem
	var leader string
	if err := row.Scan(&group.ID, &group.Name, &group.IsActive, &group.Revision, &leader,
		&group.LeaderUsername, &group.LeaderDisplayName); err != nil {
		return GroupItem{}, err
	}
	if leader != "" {
		group.LeaderID = &leader
	}
	return group, nil
}

func (s *Service) ListGroups(ctx context.Context, actor access.Principal, limit, offset int) ([]GroupItem, bool, error) {
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
	rows, err := lease.Query(ctx, `SELECT `+groupColumns+groupJoin+`
	    ORDER BY g.name,g.id LIMIT $1 OFFSET $2`, limit+1, offset)
	if err != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]GroupItem, 0, limit)
	for rows.Next() {
		item, err := scanGroup(rows)
		if err != nil {
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

func (s *Service) Group(ctx context.Context, actor access.Principal, groupID uuid.UUID) (GroupItem, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return GroupItem{}, err
	}
	if groupID == uuid.Nil {
		return GroupItem{}, failure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return GroupItem{}, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	group, err := scanGroup(lease.QueryRow(ctx, `SELECT `+groupColumns+groupJoin+` WHERE g.id=$1`, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupItem{}, ErrGroupNotFound
	}
	if err != nil {
		return GroupItem{}, failure(problem.PlatformUnavailable)
	}
	return group, nil
}

// GroupDetails memuat seluruh relasi untuk halaman group. Batas 1.000 user dan 100 group memungkinkan
// admin melihat relasi keamanan tanpa pagination tersembunyi.
func (s *Service) GroupDetails(ctx context.Context, actor access.Principal, groupID uuid.UUID) (GroupItem, []MemberItem, []MemberItem, []GrantItem, []DatabaseChoice, error) {
	group, err := s.Group(ctx, actor, groupID)
	if err != nil {
		return GroupItem{}, nil, nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return GroupItem{}, nil, nil, nil, nil, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()

	members, err := collectMembers(ctx, lease, `SELECT u.id::text,u.username,u.display_name,u.is_active
	    FROM databrain.users u JOIN databrain.memberships m ON m.user_id=u.id
	    WHERE m.group_id=$1 ORDER BY u.username,u.id`, groupID)
	if err != nil {
		return GroupItem{}, nil, nil, nil, nil, err
	}
	choices, err := collectMembers(ctx, lease, `SELECT u.id::text,u.username,u.display_name,u.is_active
	    FROM databrain.users u WHERE u.is_active AND NOT EXISTS (
	      SELECT 1 FROM databrain.memberships m WHERE m.group_id=$1 AND m.user_id=u.id)
	    ORDER BY u.username,u.id`, groupID)
	if err != nil {
		return GroupItem{}, nil, nil, nil, nil, err
	}
	grantRows, err := lease.Query(ctx, `SELECT t.id::text,t.name,g.access_level
	    FROM databrain.database_grants g JOIN databrain.database_targets t ON t.id=g.target_id
	    WHERE g.group_id=$1 ORDER BY t.name,t.id`, groupID)
	if err != nil {
		return GroupItem{}, nil, nil, nil, nil, failure(problem.PlatformUnavailable)
	}
	grants := make([]GrantItem, 0)
	for grantRows.Next() {
		var grant GrantItem
		if err := grantRows.Scan(&grant.DatabaseID, &grant.DatabaseName, &grant.Access); err != nil {
			grantRows.Close()
			return GroupItem{}, nil, nil, nil, nil, failure(problem.PlatformUnavailable)
		}
		grants = append(grants, grant)
	}
	if err := grantRows.Err(); err != nil {
		grantRows.Close()
		return GroupItem{}, nil, nil, nil, nil, failure(problem.PlatformUnavailable)
	}
	grantRows.Close()

	choiceRows, err := lease.Query(ctx, `SELECT id::text,name FROM databrain.database_targets
	    WHERE is_active ORDER BY name,id`)
	if err != nil {
		return GroupItem{}, nil, nil, nil, nil, failure(problem.PlatformUnavailable)
	}
	defer choiceRows.Close()
	databases := make([]DatabaseChoice, 0)
	for choiceRows.Next() {
		var choice DatabaseChoice
		if err := choiceRows.Scan(&choice.ID, &choice.Name); err != nil {
			return GroupItem{}, nil, nil, nil, nil, failure(problem.PlatformUnavailable)
		}
		databases = append(databases, choice)
	}
	if choiceRows.Err() != nil {
		return GroupItem{}, nil, nil, nil, nil, failure(problem.PlatformUnavailable)
	}
	return group, members, choices, grants, databases, nil
}

type rowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func collectMembers(ctx context.Context, db rowQuerier, query string, args ...any) ([]MemberItem, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, failure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]MemberItem, 0)
	for rows.Next() {
		var item MemberItem
		if err := rows.Scan(&item.ID, &item.Username, &item.DisplayName, &item.IsActive); err != nil {
			return nil, failure(problem.PlatformUnavailable)
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return nil, failure(problem.PlatformUnavailable)
	}
	return items, nil
}

// EditGroup mengembalikan true jika perubahan status mencabut family admin yang bertindak. Perubahan
// nama dan nilai identik tidak mengubah session.
func (s *Service) EditGroup(ctx context.Context, actor access.Principal, groupID uuid.UUID, in EditGroupInput) (bool, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return false, err
	}
	name, err := normalizeGroupName(in.Name)
	if groupID == uuid.Nil || in.ExpectedRevision < 1 {
		return false, failure(problem.InvalidRequest)
	}
	if err != nil {
		return false, err
	}
	var selfAffected bool
	err = s.withGuardTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var currentName string
		var active bool
		var revision int64
		err := tx.QueryRow(ctx, `SELECT name,is_active,revision FROM databrain.groups WHERE id=$1 FOR UPDATE`, groupID).
			Scan(&currentName, &active, &revision)
		missing := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !missing {
			return failure(problem.PlatformUnavailable)
		}
		statusChanged := !missing && active != in.IsActive
		var members []uuid.UUID
		if statusChanged {
			members, err = groupMemberIDs(ctx, tx, groupID)
			if err != nil {
				return err
			}
		}
		if err := lockUsersAndSessions(ctx, tx, actor, members); err != nil {
			return err
		}
		if missing {
			return ErrGroupNotFound
		}
		if revision != in.ExpectedRevision {
			return ErrRevisionConflict
		}
		if currentName == name && !statusChanged {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE databrain.groups SET name=$2,is_active=$3,
		    revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, groupID, name, in.IsActive); err != nil {
			return createError(err)
		}
		if statusChanged {
			targets, err := groupGrantTargetIDs(ctx, tx, groupID)
			if err != nil {
				return err
			}
			if err := advancePolicyGenerations(ctx, tx, targets); err != nil {
				return err
			}
		}
		if statusChanged && in.IsActive {
			err = s.sessions.ReauthenticateUsers(ctx, tx, members)
		} else if statusChanged {
			err = s.sessions.RevokeUsers(ctx, tx, members)
		}
		if err != nil {
			return err
		}
		selfAffected = statusChanged && slices.Contains(members, actor.UserID)
		return nil
	})
	if err != nil {
		return false, err
	}
	return selfAffected, nil
}

func groupMemberIDs(ctx context.Context, tx pgx.Tx, groupID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT user_id FROM databrain.memberships WHERE group_id=$1 ORDER BY user_id`, groupID)
	if err != nil {
		return nil, failure(problem.PlatformUnavailable)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, failure(problem.PlatformUnavailable)
	}
	return ids, nil
}

// Assigning a leader to a group (new feature)
// AssignLeader mengganti leader hanya dengan member aktif dan revision yang diharapkan. Perubahan
// menaikkan revision group dan generasi policy seluruh target yang memakai group ini.
func (s *Service) AssignLeader(ctx context.Context, actor access.Principal, groupID, userID uuid.UUID, expectedRevision int64) (GroupItem, error) {
	if err := s.authorizer.RequireAdmin(ctx, actor); err != nil {
		return GroupItem{}, err
	}
	if groupID == uuid.Nil || userID == uuid.Nil || expectedRevision < 1 {
		return GroupItem{}, failure(problem.InvalidRequest)
	}
	var group GroupItem
	err := s.withGuardTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var revision int64
		var leader string
		err := tx.QueryRow(ctx, `SELECT revision,coalesce(leader_id::text,'') FROM databrain.groups
		    WHERE id=$1 FOR UPDATE`, groupID).Scan(&revision, &leader)
		missing := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !missing {
			return failure(problem.PlatformUnavailable)
		}
		if missing {
			return ErrGroupNotFound
		}
		if revision != expectedRevision {
			return ErrRevisionConflict
		}
		if err := lockUsersAndSessions(ctx, tx, actor, []uuid.UUID{userID}); err != nil {
			return err
		}
		if leader == userID.String() {
			group, err = scanGroup(tx.QueryRow(ctx, `SELECT `+groupColumns+groupJoin+` WHERE g.id=$1`, groupID))
			if err != nil {
				return failure(problem.PlatformUnavailable)
			}
			return nil
		}
		var userActive bool
		if err := tx.QueryRow(ctx, `SELECT is_active FROM databrain.users WHERE id=$1`, userID).Scan(&userActive); err != nil {
			return failure(problem.PlatformUnavailable)
		}
		if !userActive {
			return validationFailed()
		}
		var member bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM databrain.memberships
		    WHERE group_id=$1 AND user_id=$2)`, groupID, userID).Scan(&member); err != nil {
			return failure(problem.PlatformUnavailable)
		}
		if !member {
			return validationFailed()
		}
		targets, err := groupGrantTargetIDs(ctx, tx, groupID)
		if err != nil {
			return err
		}
		if err := advancePolicyGenerations(ctx, tx, targets); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE databrain.groups SET leader_id=$2,revision=revision+1,
		    updated_at=clock_timestamp() WHERE id=$1`, groupID, userID); err != nil {
			return failure(problem.PlatformUnavailable)
		}
		group, err = scanGroup(tx.QueryRow(ctx, `SELECT `+groupColumns+groupJoin+` WHERE g.id=$1`, groupID))
		if err != nil {
			return failure(problem.PlatformUnavailable)
		}
		return nil
	})
	if err != nil {
		return GroupItem{}, err
	}
	return group, nil
}
