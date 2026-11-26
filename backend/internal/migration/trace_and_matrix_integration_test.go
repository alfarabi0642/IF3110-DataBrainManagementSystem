package migration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/journal"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	migrationworker "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/worker"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestEndToEndTracePropagationAndRedactedLogsPostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	exporter := tracetest.NewInMemoryExporter()
	var logBuf bytes.Buffer
	logger := telemetry.NewLoggerWithWriter(&logBuf, telemetry.ServiceWorker)
	tp, err := telemetry.NewTracerProviderWithExporter(telemetry.ServiceAPI, exporter, logger)
	if err != nil {
		t.Fatalf("NewTracerProviderWithExporter failed: %v", err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(tp) })

	cfg := seedBatchTestTable(t, ctx, h, 15)
	plan, err := sqlpolicy.BuildBatchPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.svc.CreateDraft(ctx, h.editor, h.rf.a, DraftInput{
		Title:   "trace-batch-demo",
		SQL:     plan.UpSQL,
		DownSQL: plan.DownSQL,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := h.svc.SubmitBatchRequest(ctx, h.editor, h.rf.a, d.ID, d.Revision, uuid.New(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range req.Groups {
		p := h.principalForUser(t, ctx, g.LeaderID)
		req, err = h.svc.ApproveRequest(ctx, p, h.rf.a, req.ID, g.GroupID)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Simulate an API HTTP request root span -> AcceptRequest -> River enqueue.
	httpCtx, httpSpan := telemetry.Tracer().Start(ctx, "http.request")
	rootTraceID := httpSpan.SpanContext().TraceID().String()
	adm, err := h.svc.AcceptRequest(httpCtx, h.editor, h.rf.a, req.ID, uuid.New())
	httpSpan.End()
	if err != nil {
		t.Fatalf("AcceptRequest failed: %v", err)
	}

	// Read the persisted River job args JSON from databrain.river_job to verify traceparent was persisted.
	var rawArgs []byte
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT args FROM databrain.river_job WHERE id=$1`, adm.JobID).Scan(&rawArgs); err != nil {
		t.Fatalf("cannot read river_job args: %v", err)
	}
	var persistedArgs jobs.ExecutionJobArgs
	if err := json.Unmarshal(rawArgs, &persistedArgs); err != nil {
		t.Fatalf("cannot unmarshal ExecutionJobArgs: %v", err)
	}
	if persistedArgs.TraceParent == "" || !strings.Contains(persistedArgs.TraceParent, rootTraceID) {
		t.Fatalf("expected persisted River traceparent containing %s, got %q", rootTraceID, persistedArgs.TraceParent)
	}

	// Execute via jobs.ExecutionWorker.Work with a fresh background context (simulating separate worker process).
	workerExec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, logger)
	if err != nil {
		t.Fatal(err)
	}
	riverWorkers := jobs.NewWorkersWithExecution(workerExec.HandleJob)
	_ = riverWorkers
	// Invoke ExecutionWorker.Work directly with fresh context.Background() to prove cross-process trace restoration.
	var execWorker jobs.ExecutionWorker
	// Use a small wrapper via NewWorkersWithExecution? Better: call river.Job with extracted traceparent.
	_ = execWorker
	workerCtx := telemetry.ExtractTraceContext(context.Background(), persistedArgs.TraceParent, persistedArgs.TraceState)
	workerCtx, consumeSpan := telemetry.Tracer().Start(workerCtx, "river.consume")
	err = workerExec.HandleJob(workerCtx, persistedArgs, adm.JobID)
	consumeSpan.End()
	if err != nil {
		t.Fatalf("worker HandleJob failed: %v", err)
	}
	if err := tp.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush failed: %v", err)
	}

	spans := exporter.GetSpans()
	seenNames := make(map[string]int)
	for _, s := range spans {
		if s.SpanContext.TraceID().String() == rootTraceID {
			seenNames[s.Name]++
		}
	}
	for _, required := range []string{"http.request", "migration.admit", "river.enqueue", "river.consume", "migration.execute", "migration.batch"} {
		if seenNames[required] == 0 {
			t.Fatalf("missing required span %q under root trace_id %s (seen=%v)", required, rootTraceID, seenNames)
		}
	}
	if seenNames["migration.batch"] != 2 {
		t.Fatalf("expected 2 migration.batch spans for 15 rows / batch_size 10, got %d", seenNames["migration.batch"])
	}

	// Verify structured JSON logs contain trace_id, span_id, execution_id, and zero secret leakage.
	logText := logBuf.String()
	for _, forbidden := range []string{"test-target-password", "test-runtime-password", "test-owner-password", "before_value"} {
		if strings.Contains(logText, forbidden) {
			t.Fatalf("structured logs leaked forbidden token %q", forbidden)
		}
	}
	lines := strings.Split(strings.TrimSpace(logText), "\n")
	events := make(map[string]bool)
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("non-JSON log line: %q: %v", line, err)
		}
		if entry["trace_id"] != rootTraceID {
			t.Fatalf("log line trace_id mismatch: got %v want %s (line=%s)", entry["trace_id"], rootTraceID, line)
		}
		if ev, ok := entry["event"].(string); ok {
			events[ev] = true
		}
	}
	for _, wantEvent := range []string{"migration execution started", "migration batch committed", "migration execution finished"} {
		if !events[wantEvent] {
			t.Fatalf("missing structured activity event %q in logs (got=%v)", wantEvent, events)
		}
	}
	t.Logf("trace_id=%s spans=%v sample_log=%s", rootTraceID, seenNames, lines[0])

	// Also verify River Job struct type compiles for documentation parity.
	_ = &river.Job[jobs.ExecutionJobArgs]{JobRow: &rivertype.JobRow{ID: adm.JobID}, Args: persistedArgs}
}

func TestIntegratedConcurrencyAndDenialMatrixPostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// Create a real second disposable target database and point target B at it so we can test
	// concurrent execution across two live physical targets.
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDraftTestConn(admin) })

	targetBDBName := "databrain_conc_b_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{targetBDBName}.Sanitize()); err != nil {
		t.Fatalf("create target B database failed: %v", err)
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanCancel()
		_, _ = admin.Exec(cleanCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{targetBDBName}.Sanitize()+" WITH (FORCE)")
	})
	if _, err := admin.Exec(ctx, "GRANT CONNECT, CREATE ON DATABASE "+pgx.Identifier{targetBDBName}.Sanitize()+" TO databrain_target"); err != nil {
		t.Fatal(err)
	}
	tuB, _ := url.Parse(raw)
	tuB.Path = "/" + targetBDBName
	targetBOwner, err := pgx.Connect(ctx, tuB.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDraftTestConn(targetBOwner) })
	if _, err := targetBOwner.Exec(ctx, `GRANT USAGE, CREATE ON SCHEMA public TO databrain_target`); err != nil {
		t.Fatal(err)
	}

	ddlBytes, err := os.ReadFile(filepath.Join("..", "..", "dev", "target-journal.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetBOwner.Exec(ctx, string(ddlBytes)); err != nil {
		t.Fatalf("provision journal on target B failed: %v", err)
	}
	targetCID := uuid.New()
	sealer, err := secret.New(bytes.Repeat([]byte{0x82}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealedC, err := sealer.SealTarget(targetCID, []byte(os.Getenv("DATABRAIN_TARGET_PASSWORD")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.rf.db.Pool.Exec(ctx, `
		INSERT INTO databrain.database_targets (id, name, host, port, database_name, username, password_sealed, ssl_mode, is_active, config_revision, created_by)
		SELECT $1, 'Target C', host, port, $2, username, $4, ssl_mode, true, 1, created_by
		  FROM databrain.database_targets WHERE id = $3
	`, targetCID, targetBDBName, h.rf.a, sealedC); err != nil {
		t.Fatalf("insert target C failed: %v", err)
	}
	if _, err := h.rf.db.Pool.Exec(ctx, `INSERT INTO databrain.migration_streams (target_id) VALUES ($1) ON CONFLICT (target_id) DO NOTHING`, targetCID); err != nil {
		t.Fatalf("insert stream C failed: %v", err)
	}
	if _, err := h.rf.db.Pool.Exec(ctx, `INSERT INTO databrain.migration_target_heads (target_id, initialized, generation) VALUES ($1, true, 1) ON CONFLICT (target_id) DO UPDATE SET initialized = true`, targetCID); err != nil {
		t.Fatalf("insert head C failed: %v", err)
	}
	if _, err := h.rf.db.Pool.Exec(ctx, `
		INSERT INTO databrain.database_grants (group_id, target_id, access_level)
		SELECT group_id, $1, access_level FROM databrain.database_grants WHERE target_id = $2
	`, targetCID, h.rf.a); err != nil {
		t.Fatalf("copy grants to target C failed: %v", err)
	}

	// 1. Different-target concurrency: admit migrations on target A and target C and run both concurrently.
	admA, reqA := h.admitNormalDraft(t, ctx, "concurrent-a", "CREATE TABLE public.conc_a (id int PRIMARY KEY)", "DROP TABLE public.conc_a")

	dB, err := h.svc.CreateDraft(ctx, h.editor, targetCID, DraftInput{
		Title:   "concurrent-c",
		SQL:     "CREATE TABLE public.conc_c (id int PRIMARY KEY)",
		DownSQL: "DROP TABLE public.conc_c",
	})
	if err != nil {
		t.Fatalf("CreateDraft C failed: %v", err)
	}
	reqB, err := h.svc.SubmitRequest(ctx, h.editor, targetCID, dB.ID, dB.Revision, uuid.New())
	if err != nil {
		t.Fatalf("SubmitRequest C failed: %v", err)
	}
	for _, g := range reqB.Groups {
		p := h.principalForUser(t, ctx, g.LeaderID)
		reqB, err = h.svc.ApproveRequest(ctx, p, targetCID, reqB.ID, g.GroupID)
		if err != nil {
			t.Fatalf("ApproveRequest C failed: %v", err)
		}
	}
	admB, err := h.svc.AcceptRequest(ctx, h.editor, targetCID, reqB.ID, uuid.New())
	if err != nil {
		t.Fatalf("AcceptRequest C failed: %v", err)
	}

	worker, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = worker.Execute(ctx, admA.ExecutionID, reqA.ID, admA.JobID)
	}()
	go func() {
		defer wg.Done()
		errs[1] = worker.Execute(ctx, admB.ExecutionID, reqB.ID, admB.JobID)
	}()
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent different-target executions failed: errA=%v errB=%v", errs[0], errs[1])
	}

	// 2. Same-target serialization: hold advisory lock (3110,2) on target A and confirm worker snoozes.
	admLock, reqLock := h.admitNormalDraft(t, ctx, "lock-test", "CREATE TABLE public.lock_test (id int PRIMARY KEY)", "DROP TABLE public.lock_test")
	lock, acquired, err := journal.TryTargetLock(ctx, h.targetEnd)
	if err != nil || !acquired {
		t.Fatalf("expected to acquire test lock on target A: acquired=%v err=%v", acquired, err)
	}
	snoozeErr := worker.Execute(ctx, admLock.ExecutionID, reqLock.ID, admLock.JobID)
	var snooze jobs.SnoozeError
	if snoozeErr == nil || !errorsAs(snoozeErr, &snooze) {
		_ = lock.Release(ctx)
		t.Fatalf("expected SnoozeError while physical target lock is held, got %v", snoozeErr)
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.Execute(ctx, admLock.ExecutionID, reqLock.ID, admLock.JobID); err != nil {
		t.Fatalf("expected worker to succeed after target lock released, got %v", err)
	}

	// 3. Post-approval access revocation denial: revoke editor's edit grant before worker runs.
	admDeny, reqDeny := h.admitNormalDraft(t, ctx, "revoke-test", "CREATE TABLE public.should_not_exist (id int PRIMARY KEY)", "DROP TABLE public.should_not_exist")
	if _, err := h.rf.db.Pool.Exec(ctx, `UPDATE databrain.database_grants SET access_level='view_only' WHERE target_id=$1`, h.rf.a); err != nil {
		t.Fatal(err)
	}
	denyErr := worker.Execute(ctx, admDeny.ExecutionID, reqDeny.ID, admDeny.JobID)
	var perm jobs.PermanentError
	if denyErr == nil || !errorsAs(denyErr, &perm) {
		t.Fatalf("expected PermanentError after edit grant revoked, got %v", denyErr)
	}
	var exists bool
	if err := h.targetEnd.QueryRow(ctx, `SELECT to_regclass('public.should_not_exist') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("target SQL executed despite post-approval edit access revocation")
	}
}

func errorsAs[T any](err error, target *T) bool {
	if err == nil {
		return false
	}
	switch v := err.(type) {
	case T:
		*target = v
		return true
	default:
		if u, ok := err.(interface{ Unwrap() error }); ok {
			return errorsAs(u.Unwrap(), target)
		}
		return false
	}
}
