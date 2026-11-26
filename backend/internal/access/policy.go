package access

import (
	"context"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RequiredGroup adalah grup efektif untuk satu target. Grup aktif dengan grant view-only tetap wajib
// menyetujui; leader yang hilang/inaktif/bukan anggota dikembalikan dengan reason, bukan dihilangkan.
type RequiredGroup struct {
	GroupID           uuid.UUID
	GroupName         string
	Access            Level
	LeaderID          uuid.UUID
	LeaderUsername    string
	LeaderDisplayName string
	IncompleteReason  string
}

const (
	ReasonNoLeader        = "no_leader"
	ReasonLeaderInactive  = "leader_inactive"
	ReasonLeaderNotMember = "leader_not_member"
)

type policyQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Requirements: all group needed for a migration to be executed in a database target
// RequiredGroups membaca himpunan approver untuk target. Left join mempertahankan grup yang
// konfigurasinya rusak agar submission bisa diblokir dengan alasan yang dapat diperbaiki.
func RequiredGroups(ctx context.Context, db policyQuerier, targetID uuid.UUID) ([]RequiredGroup, error) {
	if targetID == uuid.Nil {
		return nil, &problem.Error{Code: problem.InvalidRequest}
	}
	rows, err := db.Query(ctx, `SELECT g.id::text,g.name,gr.access_level,
	    coalesce(g.leader_id::text,''),coalesce(u.username,''),coalesce(u.display_name,''),
	    CASE
	      WHEN g.leader_id IS NULL THEN 'no_leader'
	      WHEN u.id IS NULL OR NOT u.is_active THEN 'leader_inactive'
	      WHEN m.user_id IS NULL THEN 'leader_not_member'
	      ELSE ''
	    END
	    FROM databrain.database_grants gr
	    JOIN databrain.groups g ON g.id=gr.group_id
	    LEFT JOIN databrain.users u ON u.id=g.leader_id
	    LEFT JOIN databrain.memberships m ON m.group_id=g.id AND m.user_id=g.leader_id
	    WHERE gr.target_id=$1 AND g.is_active
	    ORDER BY g.name,g.id`, targetID)
	if err != nil {
		return nil, &problem.Error{Code: problem.PlatformUnavailable}
	}
	defer rows.Close()
	groups := make([]RequiredGroup, 0)
	for rows.Next() {
		var group RequiredGroup
		var leader, accessLevel, reason string
		if err := rows.Scan(&group.GroupID, &group.GroupName, &accessLevel, &leader,
			&group.LeaderUsername, &group.LeaderDisplayName, &reason); err != nil {
			return nil, &problem.Error{Code: problem.PlatformUnavailable}
		}
		if leader != "" {
			id, err := uuid.Parse(leader)
			if err != nil {
				return nil, &problem.Error{Code: problem.PlatformUnavailable}
			}
			group.LeaderID = id
		}
		group.Access = Level(accessLevel)
		group.IncompleteReason = reason
		groups = append(groups, group)
	}
	if rows.Err() != nil {
		return nil, &problem.Error{Code: problem.PlatformUnavailable}
	}
	return groups, nil
}

// Database target version of policy
// PolicyGeneration mengembalikan generasi(version) review target (target tanpa baris dimulai dari 1).
func PolicyGeneration(ctx context.Context, db policyQuerier, targetID uuid.UUID) (int64, error) {
	if targetID == uuid.Nil {
		return 0, &problem.Error{Code: problem.InvalidRequest}
	}
	var generation int64
	if err := db.QueryRow(ctx, `SELECT coalesce((SELECT generation FROM databrain.migration_policy_generations
	    WHERE target_id=$1),1)`, targetID).Scan(&generation); err != nil {
		return 0, &problem.Error{Code: problem.PlatformUnavailable}
	}
	return generation, nil
}
