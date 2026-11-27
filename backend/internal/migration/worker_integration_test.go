package migration_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/head"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/journal"
	migrationworker "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/worker"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type workerTestHarness struct {
	rf        runnerFixture
	svc       *Service
	provider  *targets.Provider
	appSecret []byte
	targetDB  *pgx.Conn // owner connection to target A
	targetEnd *pgx.Conn // databrain_target connection to target A
	leaderA   access.Principal
	editor    access.Principal
}

func newWorkerTestHarness(t *testing.T, provisionJournal bool) workerTestHarness {
	t.Helper()
	rf := newRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Runner fixture already creates target A owned by databrain_fixture_owner with databrain_target
	// granted CONNECT and USAGE,CREATE on public. Also grant CREATE on database so CREATE SCHEMA works.
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDraftTestConn(admin) })

	var targetDBName string
	if err := rf.db.Pool.QueryRow(ctx, `SELECT database_name FROM databrain.database_targets WHERE id=$1`, rf.a).Scan(&targetDBName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{targetDBName}.Sanitize()+" TO databrain_target"); err != nil {
		t.Fatal(err)
	}

	if provisionJournal {
		ddlBytes, err := os.ReadFile(filepath.Join("..", "..", "dev", "target-journal.sql"))
		if err != nil {
			t.Fatalf("cannot read target-journal.sql: %v", err)
		}
		if _, err := rf.target.Exec(ctx, string(ddlBytes)); err != nil {
			t.Fatalf("cannot provision target journal: %v", err)
		}
	}

	// Open a databrain_target connection to target A for verification/lock tests.
	tu, _ := url.Parse(raw)
	tu.Path = "/" + targetDBName
	tu.User = url.UserPassword("databrain_target", os.Getenv("DATABRAIN_TARGET_PASSWORD"))
	targetEnd, err := pgx.Connect(ctx, tu.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDraftTestConn(targetEnd) })

	// Assign group leaders for all active groups granted on target A so submission/approval works.
	if _, err := rf.db.Pool.Exec(ctx, `
		UPDATE databrain.groups g
		   SET leader_id = (SELECT m.user_id FROM databrain.memberships m WHERE m.group_id = g.id ORDER BY m.user_id LIMIT 1)
		 WHERE g.leader_id IS NULL
		   AND EXISTS (SELECT 1 FROM databrain.database_grants dg WHERE dg.group_id = g.id AND dg.target_id = $1)
	`, rf.a); err != nil {
		t.Fatal(err)
	}

	appSecret := bytes.Repeat([]byte{0x71}, 32)
	provider := rf.runner.Provider()
	svc, err := NewService(rf.store, rf.runner, rf.guard)
	if err != nil {
		t.Fatal(err)
	}

	return workerTestHarness{
		rf:        rf,
		svc:       svc,
		provider:  provider,
		appSecret: appSecret,
		targetDB:  rf.target,
		targetEnd: targetEnd,
		editor:    rf.editor,
	}
}

// admitNormalDraft creates a draft, submits it, approves it with every required group leader, and
// consumes it into a queued execution + River job via AcceptRequest.
func (h workerTestHarness) admitNormalDraft(t *testing.T, ctx context.Context, title, upSQL, downSQL string) (Admission, ReviewRequest) {
	t.Helper()
	d, err := h.svc.CreateDraft(ctx, h.editor, h.rf.a, DraftInput{
		Title:   title,
		SQL:     upSQL,
		DownSQL: downSQL,
	})
	if err != nil {
		t.Fatalf("CreateDraft failed: %v", err)
	}
	req, err := h.svc.SubmitRequest(ctx, h.editor, h.rf.a, d.ID, d.Revision, uuid.New())
	if err != nil {
		t.Fatalf("SubmitRequest failed: %v", err)
	}
	for _, g := range req.Groups {
		// Create a live authenticated session principal for each required group leader to cast their vote.
		p := h.principalForUser(t, ctx, g.LeaderID)
		req, err = h.svc.ApproveRequest(ctx, p, h.rf.a, req.ID, g.GroupID)
		if err != nil {
			t.Fatalf("ApproveRequest for group %s failed: %v", g.GroupName, err)
		}
	}
	adm, err := h.svc.AcceptRequest(ctx, h.editor, h.rf.a, req.ID, uuid.New())
	if err != nil {
		t.Fatalf("AcceptRequest failed: %v", err)
	}
	return adm, req
}

func (h workerTestHarness) principalForUser(t *testing.T, ctx context.Context, userID uuid.UUID) access.Principal {
	t.Helper()
	var sessID uuid.UUID
	var secVer int64
	err := h.rf.db.Pool.QueryRow(ctx, `SELECT s.id, u.security_version
		FROM databrain.sessions s JOIN databrain.users u ON u.id=s.user_id
		WHERE s.user_id=$1 AND s.kind='authenticated' AND s.revoked_at IS NULL LIMIT 1`, userID).Scan(&sessID, &secVer)
	if err == nil {
		return access.Principal{UserID: userID, SessionID: sessID, SecurityVersion: secVer}
	}
	sessID = uuid.New()
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT security_version FROM databrain.users WHERE id=$1`, userID).Scan(&secVer); err != nil {
		t.Fatal(err)
	}
	if _, err := h.rf.db.Pool.Exec(ctx, `INSERT INTO databrain.sessions
		(id, kind, user_id, security_version, csrf_secret, absolute_expires_at, last_seen_at)
		VALUES ($1, 'authenticated', $2, $3, 'test-csrf', now()+interval '1 hour', now())`,
		sessID, userID, secVer); err != nil {
		t.Fatal(err)
	}
	return access.Principal{UserID: userID, SessionID: sessID, SecurityVersion: secVer}
}

func TestWorkerExecutorNormalEndToEndPostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adm, _ := h.admitNormalDraft(t, ctx, "Create orders table",
		`CREATE TABLE public.orders (id int PRIMARY KEY, note text); INSERT INTO public.orders VALUES (1, 'first'), (2, 'second')`,
		`DROP TABLE public.orders`)

	// Revoke the editor's browser session AFTER admission: worker must still succeed because
	// RevalidateOperation checks active user & effective edit grant without requiring a live session.
	if _, err := h.rf.db.Pool.Exec(ctx, `UPDATE databrain.sessions SET revoked_at=now() WHERE user_id=$1`, h.editor.UserID); err != nil {
		t.Fatal(err)
	}

	exec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID); err != nil {
		t.Fatalf("WorkerExecutor.Execute failed: %v", err)
	}

	// Verify target data committed.
	var rowCount int
	if err := h.targetDB.QueryRow(ctx, `SELECT count(*) FROM public.orders`).Scan(&rowCount); err != nil || rowCount != 2 {
		t.Fatalf("expected 2 target rows, got %d, err=%v", rowCount, err)
	}

	// Verify target journal has 1 commit and cleared unresolved_owner.
	commits, err := journal.ReadJournalCommits(ctx, h.targetEnd, adm.ExecutionID)
	if err != nil || len(commits) != 1 || commits[0].RowsAffected != 2 {
		t.Fatalf("unexpected journal commits: %+v, err=%v", commits, err)
	}
	owner, err := journal.ReadJournalOwner(ctx, h.targetEnd)
	if err != nil || owner != nil {
		t.Fatalf("expected cleared target owner, got %+v, err=%v", owner, err)
	}

	// Verify platform execution, worker attempt, stream gate, committed event, and target head.
	var phase, outcome string
	var committedRows, committedBatches int64
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT phase, outcome, committed_rows, committed_batches
		FROM databrain.migration_executions WHERE id=$1`, adm.ExecutionID).Scan(&phase, &outcome, &committedRows, &committedBatches); err != nil {
		t.Fatal(err)
	}
	if phase != "finished" || outcome != "succeeded" || committedRows != 2 || committedBatches != 1 {
		t.Fatalf("unexpected execution state: phase=%s outcome=%s rows=%d batches=%d", phase, outcome, committedRows, committedBatches)
	}
	var activeExec *string
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT active_execution_id::text FROM databrain.migration_streams WHERE target_id=$1`, h.rf.a).Scan(&activeExec); err != nil || activeExec != nil {
		t.Fatalf("expected cleared stream gate, got %v, err=%v", activeExec, err)
	}
	head, err := head.LoadHead(ctx, h.rf.db.Pool, h.rf.a)
	if err != nil || !head.Initialized || head.ActiveExecutionID == nil || *head.ActiveExecutionID != adm.ExecutionID || head.Generation != 2 {
		t.Fatalf("unexpected head after apply: %+v, err=%v", head, err)
	}

	// Duplicate delivery after finalization must be a safe no-op (no SQL rerun, 1 event, 2 rows).
	if err := exec.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID); err != nil {
		t.Fatalf("duplicate delivery failed: %v", err)
	}
	var eventCount int
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT count(*) FROM databrain.migration_committed_events WHERE target_id=$1`, h.rf.a).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("expected 1 committed event, got %d, err=%v", eventCount, err)
	}
}

func TestWorkerExecutorFaultAfterTargetCommitReconcilesWithoutRerunPostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Non-idempotent CREATE TABLE proves SQL is never executed a second time on replay.
	adm, _ := h.admitNormalDraft(t, ctx, "Once-only table",
		`CREATE TABLE public.once_only (id int PRIMARY KEY); INSERT INTO public.once_only VALUES (42)`,
		`DROP TABLE public.once_only`)

	exec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	exec.AfterTargetCommit = func() error { return errors.New("simulated crash before platform finalize") }

	if err := exec.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID); err == nil {
		t.Fatal("expected injected afterTargetCommit fault")
	}

	// Verify platform is blocked/unknown (reconciling) and target owner is still held.
	var phase, outcome string
	var reconciling bool
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT phase, outcome, reconciliation_pending FROM databrain.migration_executions WHERE id=$1`, adm.ExecutionID).Scan(&phase, &outcome, &reconciling); err != nil {
		t.Fatal(err)
	}
	if phase != "blocked" || outcome != "unknown" || !reconciling {
		t.Fatalf("expected blocked/unknown/reconciling, got %s/%s/%v", phase, outcome, reconciling)
	}

	// Even if actor's edit grant is revoked AFTER target commit, reconciliation must still record
	// the proven commit rather than suppressing history.
	if _, err := h.rf.db.Pool.Exec(ctx, `UPDATE databrain.users SET is_active=false WHERE id=$1`, h.editor.UserID); err != nil {
		t.Fatal(err)
	}

	// Replay on a healthy replacement worker without the fault hook.
	replacement, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID); err != nil {
		t.Fatalf("reconciliation replay failed: %v", err)
	}

	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT phase, outcome, reconciliation_pending FROM databrain.migration_executions WHERE id=$1`, adm.ExecutionID).Scan(&phase, &outcome, &reconciling); err != nil {
		t.Fatal(err)
	}
	if phase != "finished" || outcome != "succeeded" || reconciling {
		t.Fatalf("expected finished/succeeded after reconciliation, got %s/%s/%v", phase, outcome, reconciling)
	}
	head, err := head.LoadHead(ctx, h.rf.db.Pool, h.rf.a)
	if err != nil || head.ActiveExecutionID == nil || *head.ActiveExecutionID != adm.ExecutionID {
		t.Fatalf("head not finalized on reconciliation: %+v, err=%v", head, err)
	}
	owner, err := journal.ReadJournalOwner(ctx, h.targetEnd)
	if err != nil || owner != nil {
		t.Fatalf("target owner not cleared after reconciliation: %+v, err=%v", owner, err)
	}
}

func TestWorkerExecutorDenialsFaultsAndContentionPostgres(t *testing.T) {
	t.Run("revoked actor access blocks SQL and releases gate", func(t *testing.T) {
		h := newWorkerTestHarness(t, true)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		adm, _ := h.admitNormalDraft(t, ctx, "Revoked actor", `CREATE TABLE public.should_not_exist (id int)`, `DROP TABLE public.should_not_exist`)

		// Deactivate actor before worker runs.
		if _, err := h.rf.db.Pool.Exec(ctx, `UPDATE databrain.users SET is_active=false WHERE id=$1`, h.editor.UserID); err != nil {
			t.Fatal(err)
		}
		exec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		err = exec.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID)
		var perm jobs.PermanentError
		if !errors.As(err, &perm) {
			t.Fatalf("expected PermanentError on revoked actor, got %v", err)
		}
		var exists bool
		if err := h.targetDB.QueryRow(ctx, `SELECT to_regclass('public.should_not_exist') IS NOT NULL`).Scan(&exists); err != nil || exists {
			t.Fatalf("target SQL ran despite revoked actor: exists=%v err=%v", exists, err)
		}
		var phase, outcome string
		if err := h.rf.db.Pool.QueryRow(ctx, `SELECT phase, outcome FROM databrain.migration_executions WHERE id=$1`, adm.ExecutionID).Scan(&phase, &outcome); err != nil || phase != "finished" || outcome != "failed" {
			t.Fatalf("expected finished/failed, got %s/%s err=%v", phase, outcome, err)
		}
	})

	t.Run("SQL failure rolls back all statements and releases gate", func(t *testing.T) {
		h := newWorkerTestHarness(t, true)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		adm, _ := h.admitNormalDraft(t, ctx, "Mid-tx failure",
			`CREATE TABLE public.partial_rolled_back (id int PRIMARY KEY); INSERT INTO public.partial_rolled_back VALUES (1); INSERT INTO public.partial_rolled_back VALUES (1)`,
			`DROP TABLE public.partial_rolled_back`)

		exec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		err = exec.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID)
		var perm jobs.PermanentError
		if !errors.As(err, &perm) {
			t.Fatalf("expected PermanentError on duplicate key SQL failure, got %v", err)
		}
		var exists bool
		if err := h.targetDB.QueryRow(ctx, `SELECT to_regclass('public.partial_rolled_back') IS NOT NULL`).Scan(&exists); err != nil || exists {
			t.Fatalf("first statement was not rolled back: exists=%v err=%v", exists, err)
		}
		var phase, outcome, sqlstate string
		if err := h.rf.db.Pool.QueryRow(ctx, `SELECT phase, outcome, coalesce(failure_sqlstate,'') FROM databrain.migration_executions WHERE id=$1`, adm.ExecutionID).Scan(&phase, &outcome, &sqlstate); err != nil {
			t.Fatal(err)
		}
		if phase != "finished" || outcome != "failed" || sqlstate != "23505" {
			t.Fatalf("expected finished/failed/23505, got %s/%s/%s", phase, outcome, sqlstate)
		}
		head, err := head.LoadHead(ctx, h.rf.db.Pool, h.rf.a)
		if err != nil || head.ActiveExecutionID != nil || head.Generation != 1 {
			t.Fatalf("failed SQL must not move head: %+v, err=%v", head, err)
		}
	})

	t.Run("lock contention returns SnoozeError without running SQL", func(t *testing.T) {
		h := newWorkerTestHarness(t, true)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		adm, _ := h.admitNormalDraft(t, ctx, "Lock contention", `CREATE TABLE public.contended (id int)`, `DROP TABLE public.contended`)

		// Hold the physical target lock from a second connection.
		lock, acquired, err := journal.TryTargetLock(ctx, h.targetEnd)
		if err != nil || !acquired {
			t.Fatalf("failed to hold lock: %v", err)
		}
		defer func() { _ = lock.Release(ctx) }()

		exec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		err = exec.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID)
		var snooze jobs.SnoozeError
		if !errors.As(err, &snooze) {
			t.Fatalf("expected SnoozeError under lock contention, got %v", err)
		}
		var phase string
		if err := h.rf.db.Pool.QueryRow(ctx, `SELECT phase FROM databrain.migration_executions WHERE id=$1`, adm.ExecutionID).Scan(&phase); err != nil || phase != "queued" {
			t.Fatalf("contended execution should remain queued, got %s err=%v", phase, err)
		}
	})

	t.Run("missing target journal fails closed before SQL", func(t *testing.T) {
		h := newWorkerTestHarness(t, false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		adm, _ := h.admitNormalDraft(t, ctx, "Unprovisioned target", `CREATE TABLE public.no_journal (id int)`, `DROP TABLE public.no_journal`)

		exec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		err = exec.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID)
		var perm jobs.PermanentError
		if !errors.As(err, &perm) {
			t.Fatalf("expected PermanentError on missing journal, got %v", err)
		}
		var code string
		if err := h.rf.db.Pool.QueryRow(ctx, `SELECT coalesce(failure_code,'') FROM databrain.migration_executions WHERE id=$1`, adm.ExecutionID).Scan(&code); err != nil || code != "TARGET_JOURNAL_MISSING" {
			t.Fatalf("expected TARGET_JOURNAL_MISSING, got %q err=%v", code, err)
		}
	})
}

func TestFinalizeCommittedApplyRollbackForwardAndIdempotencyPostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed a draft + definition and 3 executions (apply, rollback, forward).
	d, err := h.svc.CreateDraft(ctx, h.editor, h.rf.a, DraftInput{
		Title: "Linear head test", SQL: "CREATE TABLE public.lin(id int)", DownSQL: "DROP TABLE public.lin",
	})
	if err != nil {
		t.Fatal(err)
	}
	def, err := h.rf.store.SnapshotDefinition(ctx, h.rf.a, d.ID, d.Revision)
	if err != nil {
		t.Fatal(err)
	}

	insertExec := func(dir string) (uuid.UUID, string) {
		id := uuid.New()
		hash := strings.Repeat("d", 64)
		if dir == execstore.DirectionRollback {
			hash = strings.Repeat("e", 64)
		} else if dir == execstore.DirectionForward {
			hash = strings.Repeat("f", 64)
		}
		if _, err := h.rf.db.Pool.Exec(ctx, `INSERT INTO databrain.migration_executions
			(id, target_id, actor_id, request_id, definition_id, revision, target_config_revision,
			 direction, mode, plan_hash, sql_snapshot, down_snapshot)
			VALUES ($1,$2,$3,$4,$5,$6,1,$7,'normal',$8,$9,$10)`,
			id, h.rf.a, h.editor.UserID, uuid.New(), def.ID, def.Revision, dir, hash, def.UpSQL, def.DownSQL); err != nil {
			t.Fatal(err)
		}
		return id, hash
	}

	eApply, hApply := insertExec(execstore.DirectionApply)
	eRollback, hRollback := insertExec(execstore.DirectionRollback)
	eForward, hForward := insertExec(execstore.DirectionForward)

	runFinalize := func(execID uuid.UUID, dir, hash string) error {
		tx, err := h.rf.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		err = head.FinalizeCommitted(ctx, tx, execID, head.CommitEvidence{
			RootExecutionID: execID, ActionExecutionID: execID, PlanHash: hash,
			Direction: dir, Mode: execstore.ModeNormal, CommittedAt: time.Now().UTC(), Evidence: "target_journal:v1:batch=0",
		})
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	if err := runFinalize(eApply, execstore.DirectionApply, hApply); err != nil {
		t.Fatalf("apply finalize failed: %v", err)
	}
	// Duplicate replay must succeed idempotently.
	if err := runFinalize(eApply, execstore.DirectionApply, hApply); err != nil {
		t.Fatalf("duplicate apply finalize failed: %v", err)
	}
	if err := runFinalize(eRollback, execstore.DirectionRollback, hRollback); err != nil {
		t.Fatalf("rollback finalize failed: %v", err)
	}
	// Divergent apply while redo stack is non-empty must be rejected.
	eDivergent, hDivergent := insertExec(execstore.DirectionApply)
	if err := runFinalize(eDivergent, execstore.DirectionApply, hDivergent); !errors.Is(err, head.ErrRedoStackNotEmpty) {
		t.Fatalf("expected ErrRedoStackNotEmpty, got %v", err)
	}
	if err := runFinalize(eForward, execstore.DirectionForward, hForward); err != nil {
		t.Fatalf("forward finalize failed: %v", err)
	}
	head, err := head.LoadHead(ctx, h.rf.db.Pool, h.rf.a)
	if err != nil || head.ActiveExecutionID == nil || *head.ActiveExecutionID != eForward || head.Generation != 4 {
		t.Fatalf("unexpected head after apply->rollback->forward: %+v, err=%v", head, err)
	}
}
