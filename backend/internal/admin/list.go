package admin

import (
	"context"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

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
	rows, err := lease.Query(ctx, `SELECT id::text,name,is_active,revision
	    FROM databrain.groups ORDER BY name,id LIMIT $1 OFFSET $2`, limit+1, offset)
	if err != nil {
		return nil, false, failure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]GroupItem, 0, limit)
	for rows.Next() {
		var item GroupItem
		if err := rows.Scan(&item.ID, &item.Name, &item.IsActive, &item.Revision); err != nil {
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
