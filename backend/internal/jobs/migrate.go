package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

var (
	// ErrQueueUnavailable menyembunyikan detail koneksi yang dapat memuat credential.
	ErrQueueUnavailable = errors.New("queue database unavailable")
	// ErrQueueSchema berarti schema antrean tidak dapat diterapkan atau diverifikasi dengan aman.
	ErrQueueSchema = errors.New("queue schema migration failed; run manage migrate up with the migrator credential")
)

// runtimeGrants memberi role runtime hak minimum pada objek River. Tabel river_migration tetap milik
// migrator; daftar ini mengikuti schema River yang dipin build ini. Identifier ditulis eksplisit
// karena GRANT tidak menerima parameter; ubah bersama konstanta schema dan migration 000001.
var runtimeGrants = []string{
	`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE databrain.river_job, databrain.river_queue, databrain.river_leader TO databrain_runtime`,
	`GRANT SELECT, INSERT, DELETE ON TABLE databrain.river_notification TO databrain_runtime`,
	`GRANT USAGE, SELECT ON SEQUENCE databrain.river_job_id_seq, databrain.river_notification_id_seq TO databrain_runtime`,
	`GRANT EXECUTE ON FUNCTION databrain.river_job_state_in_bitmask(bit, databrain.river_job_state) TO databrain_runtime`,
}

// MigrateResult melaporkan versi schema River yang aktif setelah migrasi.
type MigrateResult struct {
	Version  int
	NoChange bool
}

// Migrate menerapkan schema River dan grant runtime memakai DSN migrator. Pemanggilan berulang tidak
// mengubah schema, dan runtime tidak pernah menjalankan DDL.
func Migrate(databaseURL string) (MigrateResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return MigrateResult{}, ErrQueueUnavailable
	}
	cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns = 1, 0, 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return MigrateResult{}, ErrQueueUnavailable
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return MigrateResult{}, ErrQueueUnavailable
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema})
	if err != nil {
		return MigrateResult{}, ErrQueueSchema
	}
	result, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("%w: %w", ErrQueueSchema, err)
	}
	version, err := appliedVersion(ctx, migrator)
	if err != nil {
		return MigrateResult{}, err
	}
	for _, statement := range runtimeGrants {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return MigrateResult{}, fmt.Errorf("%w: %w", ErrQueueSchema, err)
		}
	}
	return MigrateResult{Version: version, NoChange: len(result.Versions) == 0}, nil
}

func appliedVersion(ctx context.Context, migrator *rivermigrate.Migrator[pgx.Tx]) (int, error) {
	existing, err := migrator.ExistingVersions(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrQueueSchema, err)
	}
	latest, applied := 0, 0
	for _, migration := range migrator.AllVersions() {
		latest = max(latest, migration.Version)
	}
	for _, migration := range existing {
		applied = max(applied, migration.Version)
	}
	if latest == 0 || applied != latest {
		return 0, fmt.Errorf("%w: schema is at version %d, build expects %d", ErrQueueSchema, applied, latest)
	}
	return applied, nil
}
