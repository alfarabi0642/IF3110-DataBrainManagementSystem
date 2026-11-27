package admin

import (
	"context"
	"slices"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// groupGrantTargetIDs mengembalikan target yang grant-nya dimiliki group, terurut stabil.
func groupGrantTargetIDs(ctx context.Context, tx pgx.Tx, groupID uuid.UUID) ([]uuid.UUID, error) {
	return targetIDsForGroups(ctx, tx, []uuid.UUID{groupID})
}

func lockGroupsLedBy(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM databrain.groups WHERE leader_id=$1 ORDER BY id FOR UPDATE`, userID)
	if err != nil {
		return nil, failure(problem.PlatformUnavailable)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, failure(problem.PlatformUnavailable)
	}
	return ids, nil
}

func targetIDsForGroups(ctx context.Context, tx pgx.Tx, groupIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT target_id FROM databrain.database_grants
	    WHERE group_id=ANY($1::uuid[]) ORDER BY target_id`, groupIDs)
	if err != nil {
		return nil, failure(problem.PlatformUnavailable)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, failure(problem.PlatformUnavailable)
	}
	return ids, nil
}

// advancePolicyGenerations mengunci target dalam urutan ID stabil dan menaikkan generasi review
// sekali untuk setiap target. No-op bila himpunan kosong.
func advancePolicyGenerations(ctx context.Context, tx pgx.Tx, targetIDs []uuid.UUID) error {
	ids := slices.Clone(targetIDs)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM databrain.database_targets
	    WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return failure(problem.PlatformUnavailable)
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(locked) != len(ids) {
		return failure(problem.PlatformUnavailable)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.migration_policy_generations(target_id)
	    SELECT unnest($1::uuid[]) ON CONFLICT (target_id) DO NOTHING`, ids); err != nil {
		return failure(problem.PlatformUnavailable)
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.migration_policy_generations
	    SET generation=generation+1,updated_at=clock_timestamp()
	    WHERE target_id=ANY($1::uuid[])`, ids); err != nil {
		return failure(problem.PlatformUnavailable)
	}
	return nil
}
