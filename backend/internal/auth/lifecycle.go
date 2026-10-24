package auth

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RevokeSession revokes one family, including every alias. The caller supplies
// an authorized internal ID and commits before clearing the cookie. It does not
// change the user's security version or revoke their other sessions.
func (s *Service) RevokeSession(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	if tx == nil || id == uuid.Nil {
		return sessionFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var locked uuid.UUID
	err := tx.QueryRow(ctx, "SELECT id FROM databrain.sessions WHERE id=$1 FOR UPDATE", id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionFailure(problem.SessionInvalid)
	}
	if err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	_, err = tx.Exec(ctx, `UPDATE databrain.sessions SET revoked_at=clock_timestamp()
	    WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	return nil
}

// RevokeUsers increments security_version once per distinct user and revokes
// all their families in the caller's transaction. Call only for actual access
// reductions. Do not also increment security_version in the caller.
//
// Admin callers must first lock security_guard and all involved users in UUID
// order, recheck the actor/revision, then mutate the domain and invoke this once.
// These locks are reacquired here for standalone use; sessions are locked last.
// The caller must roll back on error. This method never commits or rolls back.
func (s *Service) RevokeUsers(ctx context.Context, tx pgx.Tx, userIDs []uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ids, sessions, err := lockUserFamilies(ctx, tx, userIDs)
	if err != nil || len(ids) == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.users SET security_version=security_version+1
	    WHERE id=ANY($1::uuid[])`, ids); err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.sessions SET revoked_at=clock_timestamp()
	    WHERE id=ANY($1::uuid[]) AND revoked_at IS NULL`, sessions); err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	return nil
}

// ReauthenticateUsers rotates live families and requires password verification
// after an actual privilege increase. It preserves expiry, last_seen and user
// security_version. It shares RevokeUsers' lock/transaction caller requirements.
// No cookie tokens are returned: other users' tokens must not reach the actor.
func (s *Service) ReauthenticateUsers(ctx context.Context, tx pgx.Tx, userIDs []uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, sessions, err := lockUserFamilies(ctx, tx, userIDs)
	if err != nil {
		return err
	}
	for _, id := range sessions {
		var value sessionRow
		err := tx.QueryRow(ctx, `SELECT s.id,s.current_token_hash,s.absolute_expires_at,s.last_seen_at,clock_timestamp()
		    FROM databrain.sessions s JOIN databrain.users u ON u.id=s.user_id
		    WHERE s.id=$1 AND s.revoked_at IS NULL AND u.is_active AND u.security_version=s.security_version`, id).
			Scan(&value.id, &value.currentHash, &value.absolute, &value.lastSeen, &value.now)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return sessionFailure(problem.PlatformUnavailable)
		}
		if !value.now.Before(value.absolute) || !value.now.Before(value.lastSeen.Add(authenticatedIdle)) {
			continue
		}
		if _, err := s.rotateSession(ctx, tx, value); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE databrain.sessions SET reauth_required=true WHERE id=$1", id); err != nil {
			return sessionFailure(problem.PlatformUnavailable)
		}
	}
	return nil
}

func lockUserFamilies(ctx context.Context, tx pgx.Tx, userIDs []uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
	if tx == nil {
		return nil, nil, sessionFailure(problem.InvalidRequest)
	}
	ids := slices.Clone(userIDs)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return ids, nil, nil
	}
	if ids[0] == uuid.Nil || len(ids) > 1000 {
		return nil, nil, sessionFailure(problem.InvalidRequest)
	}
	var guard bool
	if err := tx.QueryRow(ctx, "SELECT singleton FROM databrain.security_guard WHERE singleton FOR UPDATE").Scan(&guard); err != nil || !guard {
		return nil, nil, sessionFailure(problem.PlatformUnavailable)
	}
	rows, err := tx.Query(ctx, "SELECT id FROM databrain.users WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE", ids)
	if err != nil {
		return nil, nil, sessionFailure(problem.PlatformUnavailable)
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, nil, sessionFailure(problem.PlatformUnavailable)
	}
	if len(users) != len(ids) {
		return nil, nil, sessionFailure(problem.InvalidRequest)
	}
	rows, err = tx.Query(ctx, `SELECT id FROM databrain.sessions
	    WHERE user_id=ANY($1::uuid[]) AND kind='authenticated' AND revoked_at IS NULL ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, nil, sessionFailure(problem.PlatformUnavailable)
	}
	sessions, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, nil, sessionFailure(problem.PlatformUnavailable)
	}
	return ids, sessions, nil
}
