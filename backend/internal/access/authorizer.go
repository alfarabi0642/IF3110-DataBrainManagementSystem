package access

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type authorizer struct {
	pool *pgxpool.Pool
}

func NewAuthorizer(pool *pgxpool.Pool) (Authorizer, error) {
	if pool == nil {
		return nil, errors.New("access: platform pool is required")
	}
	return &authorizer{pool: pool}, nil
}

func accessFailure(code problem.Code) error { return &problem.Error{Code: code} }

func validPrincipal(p Principal) bool {
	return p.UserID != uuid.Nil && p.SessionID != uuid.Nil && p.SecurityVersion > 0
}

func (a *authorizer) RequireAdmin(ctx context.Context, p Principal) error {
	if !validPrincipal(p) {
		return accessFailure(problem.SessionInvalid)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(bounded, a.pool)
	if err != nil {
		return accessFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	return RequireAdminIn(bounded, lease, p)
}

// RequireAdminIn reuses the live admin check on the caller's connection or
// transaction. Mutations must lock security_guard, actor user, then actor
// session before this check, using READ COMMITTED to recheck after lock waits.
func RequireAdminIn(ctx context.Context, db platformdb.DBTX, p Principal) error {
	if !validPrincipal(p) {
		return accessFailure(problem.SessionInvalid)
	}
	if db == nil {
		return accessFailure(problem.InternalError)
	}
	var valid, admin bool
	err := db.QueryRow(ctx, `WITH checked_at AS (SELECT clock_timestamp() AS value), actor AS (
	    SELECT u.is_platform_admin FROM databrain.sessions s JOIN databrain.users u ON u.id=s.user_id
	    CROSS JOIN checked_at
	    WHERE s.id=$1 AND s.user_id=$2 AND s.security_version=$3 AND u.security_version=$3
	      AND s.kind='authenticated' AND s.revoked_at IS NULL AND NOT s.reauth_required
	      AND u.is_active AND s.absolute_expires_at>checked_at.value
	      AND s.last_seen_at+interval '30 minutes'>checked_at.value
	) SELECT EXISTS(SELECT 1 FROM actor),COALESCE((SELECT is_platform_admin FROM actor),false)`,
		p.SessionID, p.UserID, p.SecurityVersion).Scan(&valid, &admin)
	if err != nil {
		return accessFailure(problem.PlatformUnavailable)
	}
	if !valid {
		return accessFailure(problem.SessionInvalid)
	}
	if !admin {
		return accessFailure(problem.AdminRequired)
	}
	return nil
}

func (a *authorizer) RequireDatabase(ctx context.Context, p Principal, targetID uuid.UUID, action Action) (Level, error) {
	if !validPrincipal(p) {
		return "", accessFailure(problem.SessionInvalid)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(bounded, a.pool)
	if err != nil {
		return "", accessFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	var valid bool
	var granted pgtype.Text
	err = lease.QueryRow(bounded, `WITH checked_at AS (SELECT clock_timestamp() AS value), actor AS (
	    SELECT u.id FROM databrain.sessions s JOIN databrain.users u ON u.id=s.user_id
	    CROSS JOIN checked_at
	    WHERE s.id=$1 AND s.user_id=$2 AND s.security_version=$3 AND u.security_version=$3
	      AND s.kind='authenticated' AND s.revoked_at IS NULL AND NOT s.reauth_required
	      AND u.is_active AND s.absolute_expires_at>checked_at.value
	      AND s.last_seen_at+interval '30 minutes'>checked_at.value
	), effective AS (
	    SELECT CASE WHEN bool_or(dg.access_level='view_edit') THEN 'view_edit' ELSE 'view_only' END AS level
	    FROM actor JOIN databrain.memberships m ON m.user_id=actor.id
	    JOIN databrain.groups g ON g.id=m.group_id AND g.is_active
	    JOIN databrain.database_grants dg ON dg.group_id=g.id
	    JOIN databrain.database_targets t ON t.id=dg.target_id AND t.is_active
	    WHERE t.id=$4 GROUP BY t.id
	) SELECT EXISTS(SELECT 1 FROM actor),(SELECT level FROM effective)`,
		p.SessionID, p.UserID, p.SecurityVersion, targetID).Scan(&valid, &granted)
	if err != nil {
		return "", accessFailure(problem.PlatformUnavailable)
	}
	if !valid {
		return "", accessFailure(problem.SessionInvalid)
	}
	if !granted.Valid {
		return "", accessFailure(problem.DatabaseNotFound)
	}
	level := Level(granted.String)
	switch action {
	case DatabaseView:
		if level != ViewOnly && level != ViewEdit {
			return "", accessFailure(problem.DatabaseAccessDenied)
		}
	case MigrationWrite, MigrationExecute:
		if level != ViewEdit {
			return "", accessFailure(problem.DatabaseAccessDenied)
		}
	default:
		return "", accessFailure(problem.DatabaseAccessDenied)
	}
	return level, nil
}
