package platformdb

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func platformFixture(t *testing.T) (string, string, *pgx.Conn) {
	t.Helper()
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	if raw == "" {
		t.Skip("set DATABRAIN_TEST_ADMIN_URL for the A-03 disposable PostgreSQL integration tests")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.User == nil ||
		u.User.Username() != "databrain_fixture_owner" || u.Path != "/postgres" {
		t.Fatal("integration tests require the A-03 fixture owner on 127.0.0.1:55432/postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("cannot connect to fixture owner")
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	var directory, role, database string
	if err := owner.QueryRow(ctx, "SELECT current_setting('data_directory'),current_user,current_database()").Scan(&directory, &role, &database); err != nil {
		t.Fatal("cannot inspect fixture identity")
	}
	expected, err := filepath.Abs("../../../.tmp/postgres/data")
	if err != nil || !strings.EqualFold(filepath.Clean(directory), filepath.Clean(expected)) || role != "databrain_fixture_owner" || database != "postgres" {
		t.Fatal("fixture directory or identity does not match this repository")
	}
	name := "databrain_pool_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER databrain_migrator"); err != nil {
		t.Fatal("cannot create isolated pool test database")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var actualOwner string
		if err := owner.QueryRow(cleanup, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", name).Scan(&actualOwner); err != nil || actualOwner != "databrain_migrator" {
			t.Error("cleanup refused: test database ownership changed")
			return
		}
		if _, err := owner.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
			t.Error("could not drop isolated pool test database")
		}
	})
	if _, err := owner.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC; GRANT CONNECT ON DATABASE "+quoted+" TO databrain_runtime"); err != nil {
		t.Fatal("cannot isolate pool test database")
	}
	u.Path = "/" + name
	u.User = url.UserPassword("databrain_migrator", os.Getenv("DATABRAIN_MIGRATOR_PASSWORD"))
	migratorURL := u.String()
	u.User = url.UserPassword("databrain_runtime", os.Getenv("DATABRAIN_RUNTIME_PASSWORD"))
	return migratorURL, u.String(), owner
}

func closePlatform(t *testing.T, db *DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.Close(ctx); err != nil {
		t.Error("platform close exceeded its deadline")
	}
}

func TestPlatformPoolSchemaAndLock(t *testing.T) {
	if SchemaVersion != int64(migrations.Version) {
		t.Fatal("runtime and migration schema versions differ")
	}
	migratorURL, runtimeURL, owner := platformFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if db, err := Open(ctx, runtimeURL); !errors.Is(err, ErrSchema) || db != nil {
		t.Fatal("missing schema accepted")
	}
	if _, err := migrations.Up(migratorURL); err != nil {
		t.Fatal("fixture migration failed")
	}
	if db, err := Open(ctx, migratorURL); !errors.Is(err, ErrRuntimeRole) || db != nil {
		t.Fatal("schema owner accepted as runtime")
	}
	migrator, err := pgx.Connect(ctx, migratorURL)
	if err != nil {
		t.Fatal("cannot open migrator verification connection")
	}
	defer migrator.Close(context.Background())
	for _, state := range []string{"version=0,dirty=false", "version=2,dirty=false", "version=1,dirty=true"} {
		if _, err := migrator.Exec(ctx, "UPDATE public.schema_migrations SET "+state); err != nil {
			t.Fatal("cannot set test schema version")
		}
		if db, err := Open(ctx, runtimeURL); !errors.Is(err, ErrSchema) || db != nil {
			t.Fatal("unsupported or dirty schema accepted")
		}
	}
	if _, err := migrator.Exec(ctx, "UPDATE public.schema_migrations SET version=1,dirty=false"); err != nil {
		t.Fatal("cannot restore test schema version")
	}
	db, err := Open(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer closePlatform(t, db)
	if !db.Ready(ctx) || db.Pool.Config().MaxConns != 10 || db.Pool.Config().ConnConfig.ConnectTimeout != 5*time.Second {
		t.Fatal("pool not ready or limits differ from contract")
	}
	if second, err := Open(ctx, runtimeURL); !errors.Is(err, ErrProcessLock) || second != nil {
		t.Fatal("second independent server session acquired the process lock")
	}
	// Borrow all 10 connections; the dedicated lock remains independent of them.
	for range 10 {
		conn, err := Acquire(ctx, db.Pool)
		if err != nil {
			t.Fatal("pool could not reach its configured limit")
		}
		defer conn.Release()
	}
	waitStarted := time.Now()
	if conn, err := Acquire(ctx, db.Pool); err != ErrUnavailable || conn != nil || time.Since(waitStarted) < 1500*time.Millisecond || time.Since(waitStarted) > 3*time.Second {
		t.Fatal("saturated pool did not enforce the 2-second acquire deadline")
	}
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if conn, err := Acquire(short, db.Pool); err != ErrUnavailable || conn != nil {
		t.Fatal("acquire ignored caller cancellation")
	}
	// PostgreSQL terminates only the dedicated connection owned by this test.
	pid := db.lock.PgConn().PID()
	var terminated bool
	if err := owner.QueryRow(ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&terminated); err != nil || !terminated {
		t.Fatal("could not terminate this test's process-lock connection")
	}
	select {
	case <-db.Lost():
	case <-time.After(4 * time.Second):
		t.Fatal("process lock loss was not detected")
	}
	if db.Ready(ctx) {
		t.Fatal("readiness survived process lock loss")
	}
}

func TestPlatformReadinessAndCleanup(t *testing.T) {
	migratorURL, runtimeURL, owner := platformFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := migrations.Up(migratorURL); err != nil {
		t.Fatal("fixture migration failed")
	}
	db, err := Open(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer closePlatform(t, db)
	migrator, err := pgx.Connect(ctx, migratorURL)
	if err != nil {
		t.Fatal("cannot connect to test schema")
	}
	defer migrator.Close(context.Background())
	if _, err := migrator.Exec(ctx, "UPDATE public.schema_migrations SET dirty=true"); err != nil {
		t.Fatal("cannot set dirty test schema")
	}
	if db.Ready(ctx) {
		t.Fatal("readiness ignored dirty schema")
	}
	if _, err := migrator.Exec(ctx, "UPDATE public.schema_migrations SET dirty=false"); err != nil || !db.Ready(ctx) {
		t.Fatal("readiness did not recover after schema was restored")
	}
	conn, err := Acquire(ctx, db.Pool)
	if err != nil {
		t.Fatal(err)
	}
	queryCtx, queryCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	_, queryErr := conn.Exec(queryCtx, "SELECT pg_sleep(5)")
	queryCancel()
	conn.Release()
	if queryErr == nil {
		t.Fatal("database query ignored cancellation")
	}
	// Close has a bounded wait even if a consumer temporarily retains a lease.
	lease, err := Acquire(ctx, db.Pool)
	if err != nil {
		t.Fatal(err)
	}
	closing, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	if err := db.Close(closing); err != ErrUnavailable {
		t.Fatal("close failed to respect the caller deadline")
	}
	stop()
	lease.Release()
	closePlatform(t, db)
	if db.Ready(ctx) {
		t.Fatal("closed platform reported ready")
	}
	// A new owner can acquire after the first pool and dedicated connection close.
	reopened, err := Open(ctx, runtimeURL)
	if err != nil {
		t.Fatal("shutdown leaked the process lock")
	}
	closePlatform(t, reopened)
	var count int
	u, _ := url.Parse(runtimeURL)
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND usename='databrain_runtime'", strings.TrimPrefix(u.Path, "/")).Scan(&count); err != nil || count != 0 {
		t.Fatal("shutdown leaked runtime database connections")
	}
	u.User = url.UserPassword("databrain_runtime", "SENSITIVE_WRONG_PASSWORD")
	if db, err := Open(ctx, u.String()); db != nil || err != ErrUnavailable || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatal("connection failure was accepted or disclosed driver details")
	}
}

func TestPlatformInvalidDSNIsSafe(t *testing.T) {
	db, err := Open(context.Background(), "postgres://SENSITIVE:%zz@localhost/platform")
	if db != nil || err != ErrUnavailable || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatal("invalid DSN was accepted or disclosed")
	}
}
