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

// The aggregate GroupPage query stays pending until member/grant/choice
// pagination is fixed; returning a silent subset would misrepresent access.

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
