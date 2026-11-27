package migration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	headstore "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/head"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/httpapi"
	migrationworker "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/worker"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
)

func TestIsTransientSQLState(t *testing.T) {
	for _, code := range []string{"40001", "40P01", "55P03", "53300", "08000", "08006"} {
		if !migrationworker.IsTransientSQLState(code) {
			t.Fatalf("expected %s to be transient", code)
		}
	}
	for _, code := range []string{"", "23505", "42601", "42501", "22P02", "P0001"} {
		if migrationworker.IsTransientSQLState(code) {
			t.Fatalf("expected %s to be permanent, not transient", code)
		}
	}
}

func TestReconcileExecutionPostgres(t *testing.T) {
	t.Run("committed in journal reconciles to finished/succeeded without SQL rerun", func(t *testing.T) {
		h := newWorkerTestHarness(t, true)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		adm, _ := h.admitNormalDraft(t, ctx, "Reconcile committed",
			`CREATE TABLE public.rec_ok (id int PRIMARY KEY); INSERT INTO public.rec_ok VALUES (1)`,
			`DROP TABLE public.rec_ok`)

		exec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		exec.AfterTargetCommit = func() error { return errors.New("crash before platform finalize") }
		if err := exec.Execute(ctx, adm.ExecutionID, adm.RequestID, adm.JobID); err == nil {
			t.Fatal("expected injected fault")
		}

		res, err := migrationworker.ReconcileExecution(ctx, h.rf.db.Pool, h.provider, h.appSecret, adm.ExecutionID)
		if err != nil {
			t.Fatalf("ReconcileExecution failed: %v", err)
		}
		if res.Evidence != migrationworker.EvidenceCommitted || res.FinalPhase != "finished" || res.Outcome != "succeeded" {
			t.Fatalf("unexpected reconcile result: %+v", res)
		}
		// Second reconcile is idempotent.
		res2, err := migrationworker.ReconcileExecution(ctx, h.rf.db.Pool, h.provider, h.appSecret, adm.ExecutionID)
		if err != nil || !res2.AlreadyDone || res2.Outcome != "succeeded" {
			t.Fatalf("unexpected idempotent reconcile result: %+v, err=%v", res2, err)
		}
	})

	t.Run("blocked unknown with zero journal commits reconciles to finished/failed", func(t *testing.T) {
		h := newWorkerTestHarness(t, true)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		adm, _ := h.admitNormalDraft(t, ctx, "Reconcile rolled back",
			`CREATE TABLE public.rec_none (id int)`, `DROP TABLE public.rec_none`)

		// Simulate an execution stuck in blocked/unknown with zero committed journal rows.
		if _, err := h.rf.db.Pool.Exec(ctx, `UPDATE databrain.migration_executions
			SET phase='blocked', outcome='unknown', reconciliation_pending=true, failure_code='COMMIT_UNCERTAIN'
			WHERE id=$1`, adm.ExecutionID); err != nil {
			t.Fatal(err)
		}

		res, err := migrationworker.ReconcileExecution(ctx, h.rf.db.Pool, h.provider, h.appSecret, adm.ExecutionID)
		if err != nil {
			t.Fatalf("ReconcileExecution failed: %v", err)
		}
		if res.Evidence != migrationworker.EvidenceProvenRollback || res.FinalPhase != "finished" || res.Outcome != "failed" {
			t.Fatalf("unexpected reconcile result: %+v", res)
		}
		var activeExec *string
		if err := h.rf.db.Pool.QueryRow(ctx, `SELECT active_execution_id::text FROM databrain.migration_streams WHERE target_id=$1`, h.rf.a).Scan(&activeExec); err != nil || activeExec != nil {
			t.Fatalf("stream gate not released: %v, err=%v", activeExec, err)
		}
	})

	t.Run("missing journal refuses reconciliation and retains gate", func(t *testing.T) {
		h := newWorkerTestHarness(t, false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		adm, _ := h.admitNormalDraft(t, ctx, "Reconcile unprovable",
			`CREATE TABLE public.rec_unprov (id int)`, `DROP TABLE public.rec_unprov`)

		res, err := migrationworker.ReconcileExecution(ctx, h.rf.db.Pool, h.provider, h.appSecret, adm.ExecutionID)
		if !errors.Is(err, migrationworker.ErrReconcileUnprovable) || res.Evidence != migrationworker.EvidenceUnprovable {
			t.Fatalf("expected ErrReconcileUnprovable, got res=%+v err=%v", res, err)
		}
		var activeExec *string
		if err := h.rf.db.Pool.QueryRow(ctx, `SELECT active_execution_id::text FROM databrain.migration_streams WHERE target_id=$1`, h.rf.a).Scan(&activeExec); err != nil || activeExec == nil {
			t.Fatalf("gate must stay held when unprovable: %v, err=%v", activeExec, err)
		}
	})
}

func TestRollbackAndForwardEndToEndPostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exec, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Apply migration A.
	admApply, reqApply := h.admitNormalDraft(t, ctx, "Create rf_items",
		`CREATE TABLE public.rf_items (id int PRIMARY KEY); INSERT INTO public.rf_items VALUES (7)`,
		`DROP TABLE public.rf_items`)
	if err := exec.Execute(ctx, admApply.ExecutionID, admApply.RequestID, admApply.JobID); err != nil {
		t.Fatalf("apply execute failed: %v", err)
	}
	var count int
	if err := h.targetDB.QueryRow(ctx, `SELECT count(*) FROM public.rf_items`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected 1 row after apply, got %d err=%v", count, err)
	}

	// 2. Submit rollback request for active head E1 (immutable down SQL, no replacement).
	head, err := headstore.LoadHead(ctx, h.rf.db.Pool, h.rf.a)
	if err != nil || head.ActiveExecutionID == nil || *head.ActiveExecutionID != admApply.ExecutionID {
		t.Fatalf("unexpected head after apply: %+v err=%v", head, err)
	}
	rbReq, err := h.svc.SubmitRollbackRequest(ctx, h.editor, h.rf.a, reqApply.DefinitionID, admApply.ExecutionID, head.Generation, uuid.New())
	if err != nil {
		t.Fatalf("SubmitRollbackRequest failed: %v", err)
	}
	for _, g := range rbReq.Groups {
		p := h.principalForUser(t, ctx, g.LeaderID)
		rbReq, err = h.svc.ApproveRequest(ctx, p, h.rf.a, rbReq.ID, g.GroupID)
		if err != nil {
			t.Fatalf("rollback approve failed: %v", err)
		}
	}
	admRB, err := h.svc.AcceptRequest(ctx, h.editor, h.rf.a, rbReq.ID, uuid.New())
	if err != nil {
		t.Fatalf("rollback accept failed: %v", err)
	}
	if err := exec.Execute(ctx, admRB.ExecutionID, admRB.RequestID, admRB.JobID); err != nil {
		t.Fatalf("rollback execute failed: %v", err)
	}
	var exists bool
	if err := h.targetDB.QueryRow(ctx, `SELECT to_regclass('public.rf_items') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("table should be dropped after rollback: exists=%v err=%v", exists, err)
	}

	// 3. Divergent new apply while redo stack is non-empty must be rejected.
	d2, err := h.svc.CreateDraft(ctx, h.editor, h.rf.a, DraftInput{
		Title: "Divergent B", SQL: `CREATE TABLE public.div_b (id int)`, DownSQL: `DROP TABLE public.div_b`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SubmitRequest(ctx, h.editor, h.rf.a, d2.ID, d2.Revision, uuid.New()); err == nil {
		t.Fatal("expected divergent apply to be rejected while redo stack is non-empty")
	} else {
		var pe *problem.Error
		if !errors.As(err, &pe) || pe.Code != problem.ReviewHeadUnavailable {
			t.Fatalf("expected REVIEW_HEAD_UNAVAILABLE, got %v", err)
		}
	}

	// 4. Submit forward request for A (immutable up SQL).
	head, err = headstore.LoadHead(ctx, h.rf.db.Pool, h.rf.a)
	if err != nil || head.ActiveExecutionID != nil || head.Generation != 3 {
		t.Fatalf("unexpected head after rollback: %+v err=%v", head, err)
	}
	fwdReq, err := h.svc.SubmitForwardRequest(ctx, h.editor, h.rf.a, reqApply.DefinitionID, admApply.ExecutionID, head.Generation, uuid.New())
	if err != nil {
		t.Fatalf("SubmitForwardRequest failed: %v", err)
	}
	for _, g := range fwdReq.Groups {
		p := h.principalForUser(t, ctx, g.LeaderID)
		fwdReq, err = h.svc.ApproveRequest(ctx, p, h.rf.a, fwdReq.ID, g.GroupID)
		if err != nil {
			t.Fatalf("forward approve failed: %v", err)
		}
	}
	admFwd, err := h.svc.AcceptRequest(ctx, h.editor, h.rf.a, fwdReq.ID, uuid.New())
	if err != nil {
		t.Fatalf("forward accept failed: %v", err)
	}
	if err := exec.Execute(ctx, admFwd.ExecutionID, admFwd.RequestID, admFwd.JobID); err != nil {
		t.Fatalf("forward execute failed: %v", err)
	}
	if err := h.targetDB.QueryRow(ctx, `SELECT count(*) FROM public.rf_items`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected 1 row after forward, got %d err=%v", count, err)
	}

	// 5. Committed history: Hide rollback hides E1+E2 and keeps E3; full history shows all 3.
	fullEvents, _, err := h.svc.CommittedHistory(ctx, h.editor, h.rf.a, 50, 0, false)
	if err != nil || len(fullEvents) != 3 {
		t.Fatalf("expected 3 full committed events, got %d err=%v", len(fullEvents), err)
	}
	hiddenEvents, _, err := h.svc.CommittedHistory(ctx, h.editor, h.rf.a, 50, 0, true)
	if err != nil || len(hiddenEvents) != 1 || hiddenEvents[0].ExecutionID != admFwd.ExecutionID {
		t.Fatalf("expected only forward event when hide_rollback=true, got %+v err=%v", hiddenEvents, err)
	}
}

func TestLegacyExecuteRetiredReturns410Postgres(t *testing.T) {
	f := newRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	secrets, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := auth.NewService(f.store.Pool(), secrets, auth.Policy{AppEnv: "test", AppOrigin: "http://127.0.0.1:8080"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mw, err := auth.NewMiddleware(authSvc)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(f.store, f.runner, f.guard)
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.NewAPIHandler(svc)
	if err != nil {
		t.Fatal(err)
	}
	api.RetireLegacyExecute()
	mux := httpx.NewRouter(f.db.Ready)
	api.RegisterAPI(mux, mw)
	handler := httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), httpx.APIHandler(mux))

	state, token, err := authSvc.CreateAnonymous(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, f.actor)

	d, err := svc.CreateDraft(ctx, f.editor, f.a, DraftInput{Title: "Bypass attempt", SQL: "CREATE TABLE public.bypass(id int)"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/databases/"+f.a.String()+"/drafts/"+d.ID.String()+"/execute",
		strings.NewReader(`{"expected_revision":"1","request_key":"`+uuid.NewString()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Header.Set("X-CSRF-Token", state.CSRF)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "LEGACY_EXECUTE_RETIRED") {
		t.Fatalf("expected 410 LEGACY_EXECUTE_RETIRED, got %d: %s", rec.Code, rec.Body.String())
	}
	var exists bool
	if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.bypass') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("retired execute endpoint ran target SQL: exists=%v err=%v", exists, err)
	}
}
