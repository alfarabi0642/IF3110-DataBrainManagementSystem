package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/migrations"
)

// Fixture hanya membuat dan menghapus database bernama unik pada cluster disposable tervalidasi.
// Database aplikasi dan target yang ada tidak direset.
func jobsFixture(t *testing.T) (string, string) {
	t.Helper()
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	if raw == "" {
		t.Skip("set DATABRAIN_TEST_ADMIN_URL for the disposable PostgreSQL integration tests")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.User == nil ||
		u.User.Username() != "databrain_fixture_owner" || u.Path != "/postgres" {
		t.Fatal("integration tests require the fixture owner on 127.0.0.1:55432/postgres")
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
	name := "databrain_jobs_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER databrain_migrator"); err != nil {
		t.Fatal("cannot create isolated queue test database")
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
			t.Error("could not drop isolated queue test database")
		}
	})
	if _, err := owner.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC; GRANT CONNECT ON DATABASE "+quoted+" TO databrain_runtime"); err != nil {
		t.Fatal("cannot isolate queue test database")
	}
	u.Path = "/" + name
	u.User = url.UserPassword("databrain_migrator", os.Getenv("DATABRAIN_MIGRATOR_PASSWORD"))
	migratorURL := u.String()
	u.User = url.UserPassword("databrain_runtime", os.Getenv("DATABRAIN_RUNTIME_PASSWORD"))
	return migratorURL, u.String()
}

func prepareQueue(t *testing.T, migratorURL string) {
	t.Helper()
	if _, err := migrations.Up(migratorURL); err != nil {
		t.Fatal("fixture platform migration failed")
	}
	if _, err := Migrate(migratorURL); err != nil {
		t.Fatal("fixture queue migration failed")
	}
}

func openRuntimePool(t *testing.T, runtimeURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), runtimeURL)
	if err != nil {
		t.Fatal("cannot create runtime pool")
	}
	t.Cleanup(pool.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("cannot connect runtime pool")
	}
	return pool
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMigrateRejectsInvalidDSNWithoutSecrets(t *testing.T) {
	result, err := Migrate("postgres://SENSITIVE:%zz@localhost/platform")
	if !errors.Is(err, ErrQueueUnavailable) || result != (MigrateResult{}) || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatalf("invalid queue DSN was accepted or disclosed: %+v, %v", result, err)
	}
}

func TestQueueMigrationIsIdempotentAndGrantsRuntime(t *testing.T) {
	migratorURL, runtimeURL := jobsFixture(t)
	if result, err := Migrate(migratorURL); !errors.Is(err, ErrQueueSchema) || result != (MigrateResult{}) {
		t.Fatalf("queue migration before platform schema was accepted: %+v, %v", result, err)
	}
	if _, err := migrations.Up(migratorURL); err != nil {
		t.Fatal("platform migration failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openRuntimePool(t, runtimeURL)
	if Ready(ctx, pool) {
		t.Fatal("queue schema reported ready before River migration")
	}
	first, err := Migrate(migratorURL)
	if err != nil || first.Version < 1 || first.NoChange {
		t.Fatalf("fresh queue migration failed: %+v, %v", first, err)
	}
	second, err := Migrate(migratorURL)
	if err != nil || second.Version != first.Version || !second.NoChange {
		t.Fatalf("queue migration rerun was not a no-op: %+v, %v", second, err)
	}
	if !Ready(ctx, pool) {
		t.Fatal("queue schema not ready after migration")
	}
	var insert, migrationRead, schemaCreate bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege(current_user,'databrain.river_job','INSERT'),
	    has_table_privilege(current_user,'databrain.river_migration','SELECT'),
	    has_schema_privilege(current_user,'databrain','CREATE')`).Scan(&insert, &migrationRead, &schemaCreate); err != nil {
		t.Fatal(err)
	}
	if !insert || migrationRead || schemaCreate {
		t.Fatalf("runtime grants are not least-privilege: insert=%t migration_read=%t create=%t", insert, migrationRead, schemaCreate)
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE databrain.forbidden_queue_ddl(id int)"); err == nil {
		t.Fatal("runtime role performed queue DDL")
	}
}

func TestEnqueueUsesCallerTransaction(t *testing.T) {
	migratorURL, runtimeURL := jobsFixture(t)
	prepareQueue(t, migratorURL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openRuntimePool(t, runtimeURL)
	enqueuer, err := NewEnqueuer()
	if err != nil {
		t.Fatal(err)
	}
	args := ExecutionJobArgs{Version: PayloadVersion, ExecutionID: uuid.New(), RequestID: uuid.New()}
	tx := beginTx(t, ctx, pool)
	if _, err := enqueuer.Enqueue(ctx, tx, args); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM databrain.river_job").Scan(&count); err != nil || count != 0 {
		t.Fatal("rolled-back transaction left a job")
	}
	tx = beginTx(t, ctx, pool)
	jobID, err := enqueuer.Enqueue(ctx, tx, args)
	if err != nil || jobID < 1 {
		t.Fatalf("committed enqueue failed: %d, %v", jobID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var kind string
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT kind,args FROM databrain.river_job WHERE id=$1`, jobID).Scan(&kind, &stored); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(args)
	if err != nil || kind != executionKind {
		t.Fatalf("stored job differs from execution references: kind=%q args=%s", kind, stored)
	}
	var wantFields, storedFields map[string]any
	if err := json.Unmarshal(want, &wantFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stored, &storedFields); err != nil || !reflect.DeepEqual(storedFields, wantFields) {
		t.Fatalf("stored args = %s; want only %s", stored, want)
	}
	tx = beginTx(t, ctx, pool)
	invalid := ExecutionJobArgs{Version: PayloadVersion + 1, ExecutionID: uuid.New(), RequestID: uuid.New()}
	if _, err := enqueuer.Enqueue(ctx, tx, invalid); !errors.Is(err, ErrPayload) {
		t.Fatalf("invalid payload was not rejected: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM databrain.river_job").Scan(&count); err != nil || count != 1 {
		t.Fatal("invalid payload inserted a job")
	}
}

func TestWorkerConsumesKnownJobOnceAcrossRestart(t *testing.T) {
	migratorURL, runtimeURL := jobsFixture(t)
	prepareQueue(t, migratorURL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := openRuntimePool(t, runtimeURL)
	enqueuer, err := NewEnqueuer()
	if err != nil {
		t.Fatal(err)
	}
	executionID := insertJob(t, ctx, pool, enqueuer, ExecutionJobArgs{Version: PayloadVersion, ExecutionID: uuid.New(), RequestID: uuid.New()})
	referenceID := insertJob(t, ctx, pool, enqueuer, ReferenceArgs{Version: PayloadVersion, ReferenceID: uuid.New()})
	settings := config.WorkerSettings{Concurrency: 1, JobTimeout: 10 * time.Second, DrainTimeout: time.Second}

	client := startWorker(t, pool, settings, NewWorkers())
	waitJobState(t, ctx, pool, referenceID, "completed")
	var attempt int
	if err := pool.QueryRow(ctx, `SELECT attempt FROM databrain.river_job WHERE id=$1`, referenceID).Scan(&attempt); err != nil || attempt != 1 {
		t.Fatalf("reference job was attempted %d times", attempt)
	}
	var executionState string
	var executionAttempt int
	if err := pool.QueryRow(ctx, `SELECT state,attempt FROM databrain.river_job WHERE id=$1`, executionID).Scan(&executionState, &executionAttempt); err != nil {
		t.Fatal(err)
	}
	if executionState != "available" || executionAttempt != 0 {
		t.Fatalf("worker consumed a job without a registered handler: state=%q attempt=%d", executionState, executionAttempt)
	}
	stopWorker(t, client)

	restarted := startWorker(t, pool, settings, NewWorkers())
	if err := pool.QueryRow(ctx, `SELECT state,attempt FROM databrain.river_job WHERE id=$1`, referenceID).Scan(&executionState, &executionAttempt); err != nil || executionState != "completed" || executionAttempt != 1 {
		t.Fatalf("restart consumed the completed job again: state=%q attempt=%d", executionState, executionAttempt)
	}
	stopWorker(t, restarted)
}

func TestWorkerDrainCancelsRunningJob(t *testing.T) {
	migratorURL, runtimeURL := jobsFixture(t)
	prepareQueue(t, migratorURL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := openRuntimePool(t, runtimeURL)
	enqueuer, err := NewEnqueuer()
	if err != nil {
		t.Fatal(err)
	}
	jobID := insertJob(t, ctx, pool, enqueuer, blockingTestArgs{Version: PayloadVersion})
	worker := &blockingTestWorker{started: make(chan struct{}), canceled: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	settings := config.WorkerSettings{Concurrency: 1, JobTimeout: 10 * time.Second, DrainTimeout: 100 * time.Millisecond}
	client, err := NewWorker(pool, settings, workers, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	startCtx, stopStart := context.WithCancel(context.Background())
	if err := client.Start(startCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopStart()
		stopWorker(t, client)
	})
	select {
	case <-worker.started:
	case <-time.After(15 * time.Second):
		t.Fatal("blocking job did not start")
	}
	stopStart()
	select {
	case <-client.Stopped():
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop after the drain deadline")
	}
	select {
	case <-worker.canceled:
	default:
		t.Fatal("running job context was not canceled after the drain deadline")
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM databrain.river_job WHERE id=$1`, jobID).Scan(&state); err != nil || state != "available" {
		t.Fatalf("cancelled job was lost or left running: state=%q", state)
	}
}

func beginTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func insertJob[T river.JobArgs](t *testing.T, ctx context.Context, pool *pgxpool.Pool, enqueuer *Enqueuer, args T) int64 {
	t.Helper()
	tx := beginTx(t, ctx, pool)
	result, err := enqueuer.client.InsertTx(ctx, tx, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return result.Job.ID
}

func startWorker(t *testing.T, pool *pgxpool.Pool, settings config.WorkerSettings, workers *river.Workers) *river.Client[pgx.Tx] {
	t.Helper()
	client, err := NewWorker(pool, settings, workers, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopWorker(t, client) })
	return client
}

func stopWorker(t *testing.T, client *river.Client[pgx.Tx]) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Stop(ctx); err != nil {
		t.Fatal("worker did not stop within its deadline")
	}
}

func waitJobState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID int64, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM databrain.river_job WHERE id=$1`, jobID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d state = %q; want %q", jobID, state, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type blockingTestArgs struct {
	Version int `json:"version"`
}

func (blockingTestArgs) Kind() string { return "jobs_blocking_test" }

type blockingTestWorker struct {
	river.WorkerDefaults[blockingTestArgs]
	started  chan struct{}
	canceled chan struct{}
}

func (w *blockingTestWorker) Work(ctx context.Context, job *river.Job[blockingTestArgs]) error {
	close(w.started)
	<-ctx.Done()
	close(w.canceled)
	return ctx.Err()
}
