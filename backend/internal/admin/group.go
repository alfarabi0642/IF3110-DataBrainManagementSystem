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
	var group GroupItem
	err = lease.QueryRow(ctx, `SELECT id::text,name,is_active,revision FROM databrain.groups WHERE id=$1`, groupID).
		Scan(&group.ID, &group.Name, &group.IsActive, &group.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupItem{}, ErrGroupNotFound
	}
	if err != nil {
		return GroupItem{}, failure(problem.PlatformUnavailable)
	}
	return group, nil
}

// GroupDetails returns the complete bounded aggregate used by the group page.
// M1 caps the platform at 1000 users and 100 groups, so detail routes do not
// silently paginate security relationships that the administrator must see.
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

// EditGroup returns true when changing the group's status also invalidated the
// acting admin's family. Name-only and identical edits do not touch sessions.
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
	var currentName string
	var active bool
	var revision int64
	err = tx.QueryRow(ctx, `SELECT name,is_active,revision FROM databrain.groups WHERE id=$1 FOR UPDATE`, groupID).
		Scan(&currentName, &active, &revision)
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return false, failure(problem.PlatformUnavailable)
	}
	statusChanged := !missing && active != in.IsActive
	members := []uuid.UUID(nil)
	if statusChanged {
		members, err = groupMemberIDs(ctx, tx, groupID)
		if err != nil {
			return false, err
		}
	}
	if err := lockUsersAndSessions(ctx, tx, actor, members); err != nil {
		return false, err
	}
	if missing {
		return false, ErrGroupNotFound
	}
	if revision != in.ExpectedRevision {
		return false, ErrRevisionConflict
	}
	if currentName == name && !statusChanged {
		if err := tx.Commit(ctx); err != nil {
			return false, failure(problem.PlatformUnavailable)
		}
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.groups SET name=$2,is_active=$3,
	    revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, groupID, name, in.IsActive); err != nil {
		return false, createError(err)
	}
	if statusChanged && in.IsActive {
		err = s.sessions.ReauthenticateUsers(ctx, tx, members)
	} else if statusChanged {
		err = s.sessions.RevokeUsers(ctx, tx, members)
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, failure(problem.PlatformUnavailable)
	}
	return statusChanged && slices.Contains(members, actor.UserID), nil
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
