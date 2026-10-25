package targets

import (
	"context"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

// UserDatabase hanya memuat data yang boleh muncul dalam daftar database user.
type UserDatabase struct {
	ID                   uuid.UUID
	Name                 string
	EffectiveAccess      access.Level
	ActiveAttemptID      *uuid.UUID
	ActiveAttemptState   string
	ActiveAttemptVersion string
}

// ListForUser membaca grant group terkini untuk user session tervalidasi. Query ini tidak menggantikan
// validasi session atau Authorizer. Nilai bool menandai halaman berikutnya; handler menentukan limit
// default 50.
func (p *Provider) ListForUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]UserDatabase, bool, error) {
	if userID == uuid.Nil || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, failure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, p.pool)
	if err != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	rows, err := lease.Query(ctx, `WITH effective AS (
	    SELECT dg.target_id,
	        CASE WHEN bool_or(dg.access_level='view_edit') THEN 'view_edit' ELSE 'view_only' END AS level
	    FROM databrain.database_grants dg
	    JOIN databrain.groups g ON g.id=dg.group_id AND g.is_active
	    JOIN databrain.memberships m ON m.group_id=g.id
	    JOIN databrain.users u ON u.id=m.user_id AND u.is_active
	    WHERE u.id=$1 GROUP BY dg.target_id
	) SELECT t.id,t.name,e.level,s.active_attempt_id,coalesce(a.state,''),coalesce(a.version::text,'')
	    FROM databrain.database_targets t JOIN effective e ON e.target_id=t.id
	    LEFT JOIN databrain.migration_streams s ON s.target_id=t.id
	    LEFT JOIN databrain.migration_attempts a ON a.target_id=t.id AND a.id=s.active_attempt_id
	    WHERE t.is_active ORDER BY t.name,t.id LIMIT $2 OFFSET $3`, userID, limit+1, offset)
	if err != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]UserDatabase, 0, limit)
	for rows.Next() {
		var item UserDatabase
		if err := rows.Scan(&item.ID, &item.Name, &item.EffectiveAccess, &item.ActiveAttemptID, &item.ActiveAttemptState, &item.ActiveAttemptVersion); err != nil {
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

// ListAdmin memuat metadata koneksi aktif/nonaktif tanpa credential. Handler wajib memanggil
// RequireAdmin sebelum query atau selector admin.
func (p *Provider) ListAdmin(ctx context.Context, limit, offset int) ([]Target, bool, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, failure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, p.pool)
	if err != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	rows, err := lease.Query(ctx, `SELECT id,name,host,port,database_name,username,ssl_mode,is_active,
	    config_revision,created_by,created_at,updated_at,octet_length(password_sealed)>0
	    FROM databrain.database_targets ORDER BY name,id LIMIT $1 OFFSET $2`, limit+1, offset)
	if err != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]Target, 0, limit)
	for rows.Next() {
		var item Target
		if err := rows.Scan(&item.ID, &item.Name, &item.Host, &item.Port, &item.DatabaseName, &item.Username,
			&item.SSLMode, &item.IsActive, &item.ConfigRevision, &item.CreatedBy, &item.CreatedAt, &item.UpdatedAt, &item.PasswordStored); err != nil {
			return nil, false, failure(problem.PlatformUnavailable)
		}
		item.CreatedAt, item.UpdatedAt = item.CreatedAt.UTC(), item.UpdatedAt.UTC()
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
