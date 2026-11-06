package migration

import (
	"bytes"
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
)

func newWorkflowFixture(t *testing.T, origin string) (runnerFixture, draftHTTPFixture) {
	t.Helper()
	f := newRunnerFixture(t)
	secrets, err := secret.New(bytes.Repeat([]byte{0x82}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(f.store.pool, secrets, auth.Policy{AppEnv: "test", AppOrigin: origin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := auth.NewMiddleware(service)
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(f.store, f.runner, f.guard, renderer)
	if err != nil {
		t.Fatal(err)
	}
	mux := httpx.NewRouter(f.db.Ready)
	handler.Register(mux, middleware)
	handler.RegisterAPI(mux, middleware)
	assets, err := httpx.MountStatic(mux, "../../web/static")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assets.Close() })
	seed := func(user uuid.UUID) draftSession {
		state, token, err := service.CreateAnonymous(context.Background())
		if err != nil {
			t.Fatal("cannot initialize HTTP workflow session")
		}
		if user != uuid.Nil {
			runnerSQL(t, f.store.pool, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, user)
		}
		return draftSession{state.ID, token, state.CSRF}
	}
	return f, draftHTTPFixture{draftFixture: f.draftFixture,
		app: httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), mux), service: service, origin: origin,
		path: "/databases/" + f.a.String() + "/drafts", otherPath: "/databases/" + f.b.String() + "/drafts",
		editor: seed(f.actor), otherEditor: seed(f.otherActor), viewer: seed(f.viewer.UserID), none: seed(f.none.UserID), admin: seed(f.admin.UserID), anonymous: seed(uuid.Nil), editGroup: f.editGroup}
}

func executeValues(session draftSession, input ReserveInput) url.Values {
	return url.Values{"_csrf": {session.csrf}, "expected_revision": {strconv.FormatInt(input.ExpectedRevision, 10)}, "request_key": {input.RequestKey.String()}}
}

func workflowHas(t *testing.T, w *httptest.ResponseRecorder, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(w.Body.String(), part) {
			t.Fatalf("workflow HTML missing %q", part)
		}
	}
}

func TestWorkflowPostgres(t *testing.T) {
	f, web := newWorkflowFixture(t, "http://localhost:18085")
	ctx := context.Background()
	history := "/databases/" + f.a.String() + "/migrations"
	sql := "\r\n-- 猫; snapshot\r\nCREATE TABLE public.workflow_once(id int PRIMARY KEY,value text); INSERT INTO public.workflow_once VALUES(1,'<script>猫;a</script>');"
	d := attemptDraft(t, f.draftFixture, f.a, sql)
	detail := web.path + "/" + d.ID.String()
	input := ReserveInput{1, uuid.New()}
	canonical := reviewPath(f.a, d.ID, input)
	var location string
	t.Run("canonical review is read only and preserves key", func(t *testing.T) {
		w := web.request(t, "GET", detail+"/review", web.editor, nil, 303)
		u, err := url.Parse(w.Header().Get("Location"))
		if err != nil || u.Path != detail+"/review" || u.Query().Get("expected_revision") != "1" {
			t.Fatal("review redirect is not canonical")
		}
		if _, err := uuid.Parse(u.Query().Get("request_key")); err != nil {
			t.Fatal("review did not generate a UUID")
		}
		for range 2 {
			w := web.request(t, "GET", canonical, web.editor, nil, 200)
			workflowHas(t, w, html.EscapeString(sql), input.RequestKey.String(), `name="expected_revision" value="1"`, `name="_csrf" value="`+web.editor.csrf+`"`)
		}
		var count int
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM databrain.migration_attempts`).Scan(&count); err != nil || count != 0 {
			t.Fatal("GET review reserved an attempt")
		}
		for _, query := range []string{"expected_revision=1", "request_key=" + input.RequestKey.String(), "expected_revision=0&request_key=" + input.RequestKey.String(), "expected_revision=1&request_key=BAD", "expected_revision=1&expected_revision=2&request_key=" + input.RequestKey.String(), "actor=forged", "request_key=&expected_revision=1"} {
			web.request(t, "GET", detail+"/review?"+query, web.editor, nil, 400)
		}
	})
	t.Run("session grants CSRF and strict input", func(t *testing.T) {
		for _, path := range []string{canonical, history, history + "/" + uuid.NewString()} {
			for _, session := range []draftSession{{}, {token: "invalid"}, web.anonymous} {
				w := web.request(t, "GET", path, session, nil, 303)
				if w.Header().Get("Location") != "/login" {
					t.Fatal("missing session did not redirect")
				}
			}
			for _, session := range []draftSession{web.none, web.admin} {
				web.request(t, "GET", path, session, nil, 404)
			}
		}
		web.request(t, "GET", canonical, web.viewer, nil, 403)
		web.request(t, "GET", history, web.viewer, nil, 200)
		for _, tc := range []struct {
			s      draftSession
			status int
		}{{web.viewer, 403}, {web.none, 404}, {web.admin, 404}, {web.anonymous, 303}, {draftSession{}, 303}} {
			web.request(t, "POST", detail+"/execute", tc.s, executeValues(tc.s, input), tc.status)
		}
		v := executeValues(web.editor, input)
		v.Set("_csrf", "wrong")
		web.request(t, "POST", detail+"/execute", web.editor, v, 403)
		for _, change := range []func(url.Values){func(v url.Values) { v.Set("sql", "forged") }, func(v url.Values) { v.Set("actor", f.otherActor.String()) }, func(v url.Values) { v["request_key"] = []string{input.RequestKey.String(), uuid.NewString()} }, func(v url.Values) { v.Del("expected_revision") }, func(v url.Values) { v.Set("expected_revision", "0") }} {
			v := executeValues(web.editor, input)
			change(v)
			web.request(t, "POST", detail+"/execute", web.editor, v, 400)
		}
		for _, origin := range []string{"", "null", "http://foreign.example"} {
			r := httptest.NewRequest("POST", detail+"/execute", strings.NewReader(executeValues(web.editor, input).Encode()))
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: web.editor.token})
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			web.app.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal("foreign Origin accepted")
			}
		}
		web.request(t, "GET", web.otherPath+"/"+d.ID.String()+"/review", web.editor, nil, 404)
		web.request(t, "POST", web.otherPath+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, input), 404)
	})
	t.Run("commit PRG snapshot replay after lost response and edited draft", func(t *testing.T) {
		w := web.request(t, "POST", detail+"/execute", web.editor, executeValues(web.editor, input), 303)
		location = w.Header().Get("Location")
		if !strings.HasPrefix(location, history+"/") {
			t.Fatal("execute did not redirect to receipt")
		}
		for range 2 {
			workflowHas(t, web.request(t, "GET", location, web.viewer, nil, 200), "succeeded", html.EscapeString(sql), "draft_editor_a", "UTC")
		}
		_, err := f.store.Save(ctx, f.a, d.ID, f.otherActor, SaveDraftInput{DraftInput{"changed latest", "INVALID latest SQL"}, 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{canonical, receiptPath(f.a, input.RequestKey)} {
			if w := web.request(t, "GET", path, web.editor, nil, 303); w.Header().Get("Location") != location {
				t.Fatal("old key did not recover receipt")
			}
		}
		if w := web.request(t, "POST", detail+"/execute", web.editor, executeValues(web.editor, input), 303); w.Header().Get("Location") != location {
			t.Fatal("POST replay changed attempt")
		}
		workflowHas(t, web.request(t, "GET", receiptPath(f.a, input.RequestKey), web.otherEditor, nil, 200), "None of your attempts")
		workflowHas(t, web.request(t, "GET", receiptPath(f.b, input.RequestKey), web.editor, nil, 200), "None of your attempts")
		web.request(t, "GET", "/databases/"+f.b.String()+"/migrations/"+strings.TrimPrefix(location, history+"/"), web.editor, nil, 404)
		w = web.request(t, "GET", reviewPath(f.a, d.ID, ReserveInput{2, input.RequestKey}), web.editor, nil, 409)
		workflowHas(t, w, "IDEMPOTENCY_CONFLICT", input.RequestKey.String())
		w = web.request(t, "GET", reviewPath(f.a, d.ID, ReserveInput{1, uuid.New()}), web.editor, nil, 409)
		workflowHas(t, w, "REVISION_CONFLICT")
		var n int
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.workflow_once`).Scan(&n); err != nil || n != 1 {
			t.Fatal("refresh/replay changed target")
		}
	})
	t.Run("invalid SQL and successful revision conflicts retain recovery links", func(t *testing.T) {
		for _, sql := range []string{"CREAT wrong", "BEGIN"} {
			d := attemptDraft(t, f.draftFixture, f.a, sql)
			in := ReserveInput{1, uuid.New()}
			w := web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 422)
			workflowHas(t, w, in.RequestKey.String(), html.EscapeString(receiptPath(f.a, in.RequestKey)), html.EscapeString(reviewPath(f.a, d.ID, in)))
			if strings.Contains(w.Body.String(), "Execute saved revision") {
				t.Fatal("error automatically offered another submit")
			}
		}
		d := attemptDraft(t, f.draftFixture, f.a, `INSERT INTO public.workflow_once VALUES(2,'second')`)
		in := ReserveInput{1, uuid.New()}
		web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 303)
		in.RequestKey = uuid.New()
		workflowHas(t, web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 409), "DRAFT_ALREADY_EXECUTED", in.RequestKey.String())
	})
	t.Run("failed outcome redirects with actual rollback", func(t *testing.T) {
		d := attemptDraft(t, f.draftFixture, f.a, `CREATE TABLE public.workflow_rollback(id int); INSERT INTO public.workflow_missing VALUES(9)`)
		in := ReserveInput{1, uuid.New()}
		w := web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 303)
		workflowHas(t, web.request(t, "GET", w.Header().Get("Location"), web.viewer, nil, 200), "failed", "SQL_ERROR", "42P01", "Nothing was committed")
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.workflow_rollback') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("HTTP failure left DDL effects")
		}
	})
	t.Run("history pagination query and snapshot escaping", func(t *testing.T) {
		workflowHas(t, web.request(t, "GET", history+"?limit=1", web.viewer, nil, 200), `rel="next"`)
		workflowHas(t, web.request(t, "GET", history+"?limit=1&offset=1", web.viewer, nil, 200), `rel="prev"`)
		runnerSQL(t, f.store.pool, `UPDATE databrain.database_targets SET name='renamed current' WHERE id=$1`, f.a)
		workflowHas(t, web.request(t, "GET", location, web.viewer, nil, 200), "Target A", html.EscapeString(sql))
		for _, query := range []string{"request_key=", "request_key=BAD", "request_key=" + input.RequestKey.String() + "&request_key=" + input.RequestKey.String(), "limit=0", "offset=100001", "sql=x"} {
			web.request(t, "GET", history+"?"+query, web.viewer, nil, 400)
		}
		web.request(t, "GET", location+"?request_key="+input.RequestKey.String(), web.viewer, nil, 400)
		web.request(t, "GET", history+"/not-uuid", web.viewer, nil, 400)
	})
	t.Run("unreadable receipt after claim preserves original recovery URLs", func(t *testing.T) {
		// Gangguan platform nyata dibatasi setelah claim permanen dan sebelum SQL target.
		runnerSQL(t, f.schema, `CREATE FUNCTION databrain.workflow_lose_read() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$ BEGIN REVOKE SELECT ON databrain.migration_attempts FROM databrain_runtime; RETURN NEW; END $$; CREATE TRIGGER workflow_lose_read AFTER UPDATE ON databrain.migration_attempts FOR EACH ROW WHEN (NEW.state='running') EXECUTE FUNCTION databrain.workflow_lose_read()`)
		d := attemptDraft(t, f.draftFixture, f.a, `CREATE TABLE public.workflow_must_not_start(id int)`)
		in := ReserveInput{1, uuid.New()}
		w := web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 503)
		runnerSQL(t, f.schema, `DROP TRIGGER workflow_lose_read ON databrain.migration_attempts; DROP FUNCTION databrain.workflow_lose_read(); GRANT SELECT ON databrain.migration_attempts TO databrain_runtime`)
		workflowHas(t, w, "PLATFORM_UNAVAILABLE", in.RequestKey.String(), "Revision</dt><dd>1", html.EscapeString(receiptPath(f.a, in.RequestKey)), html.EscapeString(reviewPath(f.a, d.ID, in)))
		if strings.Contains(w.Body.String(), "Execute saved revision") || strings.Contains(w.Body.String(), "workflow_must_not_start") {
			t.Fatal("uncertain response offers submit or loads latest SQL")
		}
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.workflow_must_not_start') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("unreadable snapshot was executed")
		}
		receipt, err := f.runner.store.Receipt(ctx, f.a, f.actor, in.RequestKey)
		if err != nil || receipt == nil || receipt.State != "running" {
			t.Fatal("durable claim lost after read failure")
		}
		for _, path := range []string{reviewPath(f.a, d.ID, in), receiptPath(f.a, in.RequestKey)} {
			if w := web.request(t, "GET", path, web.editor, nil, 303); w.Header().Get("Location") != attemptPath(f.a, receipt.ID) {
				t.Fatal("same key did not recover claim")
			}
		}
		workflowHas(t, web.request(t, "GET", attemptPath(f.a, receipt.ID), web.viewer, nil, 200), "running", "Refresh status")
		if _, err := RecoverInterrupted(ctx, f.db); err != nil {
			t.Fatal(err)
		}
		if err := ResolveAttempt(ctx, f.db, receipt.ID, ResolutionInput{"failed", "test operator", "Snapshot read was denied before any target connection; target relation is absent."}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown stays blocked and operator evidence remains private", func(t *testing.T) {
		runnerSQL(t, f.schema, `CREATE FUNCTION databrain.workflow_reject_success() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='succeeded' THEN RAISE EXCEPTION 'PRIVATE_OUTCOME'; END IF; RETURN NEW; END $$; CREATE TRIGGER workflow_reject_success BEFORE UPDATE ON databrain.migration_attempts FOR EACH ROW EXECUTE FUNCTION databrain.workflow_reject_success()`)
		d := attemptDraft(t, f.draftFixture, f.a, `INSERT INTO public.workflow_once VALUES(3,'unknown commit')`)
		in := ReserveInput{1, uuid.New()}
		w := web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 303)
		result := w.Header().Get("Location")
		runnerSQL(t, f.schema, `DROP TRIGGER workflow_reject_success ON databrain.migration_attempts; DROP FUNCTION databrain.workflow_reject_success()`)
		page := web.request(t, "GET", result, web.viewer, nil, 200)
		workflowHas(t, page, "unknown", "stay blocked", "Marked unknown")
		if strings.Contains(page.Body.String(), "/execute") || strings.Contains(page.Body.String(), "Refresh status") || strings.Contains(page.Body.String(), "PRIVATE_OUTCOME") {
			t.Fatal("unknown offers retry or exposes private data")
		}
		if w := web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 303); w.Header().Get("Location") != result {
			t.Fatal("unknown replay changed receipt")
		}
		other := in
		other.RequestKey = uuid.New()
		workflowHas(t, web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, other), 409), "TARGET_BUSY")
		receipt, err := f.runner.store.Receipt(ctx, f.a, f.actor, in.RequestKey)
		if err != nil || receipt == nil {
			t.Fatal("missing unknown receipt")
		}
		if err := ResolveAttempt(ctx, f.db, receipt.ID, ResolutionInput{"succeeded", "PRIVATE_OPERATOR", "PRIVATE_EVIDENCE actual unique row verified"}); err != nil {
			t.Fatal(err)
		}
		page = web.request(t, "GET", result, web.viewer, nil, 200)
		workflowHas(t, page, "succeeded", "An operator verified", "Marked unknown", "Resolved by operator")
		if strings.Contains(page.Body.String(), "PRIVATE_") {
			t.Fatal("operator evidence leaked")
		}
		var n int
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.workflow_once WHERE id=3`).Scan(&n); err != nil || n != 1 {
			t.Fatal("unknown replay duplicated effects")
		}
	})
	t.Run("readable receipt redirects even when outcome persistence returns an error", func(t *testing.T) {
		runnerSQL(t, f.schema, `CREATE FUNCTION databrain.workflow_reject_outcome() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state IN ('succeeded','unknown') THEN RAISE EXCEPTION 'PRIVATE_STORE_FAILURE'; END IF; RETURN NEW; END $$; CREATE TRIGGER workflow_reject_outcome BEFORE UPDATE ON databrain.migration_attempts FOR EACH ROW EXECUTE FUNCTION databrain.workflow_reject_outcome()`)
		d := attemptDraft(t, f.draftFixture, f.a, `INSERT INTO public.workflow_once VALUES(4,'durable effect')`)
		in := ReserveInput{1, uuid.New()}
		w := web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 303)
		runnerSQL(t, f.schema, `DROP TRIGGER workflow_reject_outcome ON databrain.migration_attempts; DROP FUNCTION databrain.workflow_reject_outcome()`)
		receipt, err := f.runner.store.Receipt(ctx, f.a, f.actor, in.RequestKey)
		if err != nil || receipt == nil || receipt.State != "running" || w.Header().Get("Location") != attemptPath(f.a, receipt.ID) {
			t.Fatal("readable receipt became an HTTP persistence error")
		}
		var n int
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.workflow_once WHERE id=4`).Scan(&n); err != nil || n != 1 {
			t.Fatal("target commit was not actually persisted")
		}
		if _, err := RecoverInterrupted(ctx, f.db); err != nil {
			t.Fatal(err)
		}
		if err := ResolveAttempt(ctx, f.db, receipt.ID, ResolutionInput{"succeeded", "test operator", "Target contains the unique id=4 row after acknowledged COMMIT."}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("revocation inactive group and target deny existing receipts", func(t *testing.T) {
		runnerSQL(t, f.store.pool, `UPDATE databrain.groups SET is_active=false WHERE id=$1`, f.editGroup)
		web.request(t, "GET", canonical, web.otherEditor, nil, 404)
		runnerSQL(t, f.store.pool, `UPDATE databrain.groups SET is_active=true WHERE id=$1`, f.editGroup)
		runnerSQL(t, f.store.pool, `UPDATE databrain.database_targets SET is_active=false WHERE id=$1`, f.a)
		web.request(t, "GET", location, web.viewer, nil, 404)
		web.request(t, "POST", detail+"/execute", web.editor, executeValues(web.editor, input), 404)
		runnerSQL(t, f.store.pool, `UPDATE databrain.database_targets SET is_active=true WHERE id=$1`, f.a)
		runnerSQL(t, f.store.pool, `UPDATE databrain.sessions SET revoked_at=now() WHERE id=$1`, web.editor.id)
		web.request(t, "GET", canonical, web.editor, nil, 303)
		web.request(t, "POST", detail+"/execute", web.editor, executeValues(web.editor, input), 303)
	})
}

func TestWorkflowConcurrentPostgres(t *testing.T) {
	f, web := newWorkflowFixture(t, "http://localhost:18085")
	ctx := context.Background()
	d := attemptDraft(t, f.draftFixture, f.a, `UPDATE public.draft_marker SET value=value||';once'`)
	in := ReserveInput{1, uuid.New()}
	lock, err := f.target.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackAttempt(lock)
	runnerSQL(t, lock, `UPDATE public.draft_marker SET value=value`)
	r := httptest.NewRequest("POST", web.path+"/"+d.ID.String()+"/execute", strings.NewReader(executeValues(web.editor, in).Encode()))
	r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: web.editor.token})
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", web.origin)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); web.app.ServeHTTP(w, r); result <- w }()
	done := false
	defer func() {
		rollbackAttempt(lock)
		if !done {
			<-result
		}
	}()
	active := waitRunnerTarget(t, f, f.a, in.RequestKey, "Lock")
	expected := attemptPath(f.a, active.ID)
	for _, path := range []string{reviewPath(f.a, d.ID, in), receiptPath(f.a, in.RequestKey)} {
		if w := web.request(t, "GET", path, web.editor, nil, 303); w.Header().Get("Location") != expected {
			t.Fatal("running receipt lookup changed key")
		}
	}
	w := web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, in), 303)
	if w.Header().Get("Location") != expected {
		t.Fatal("simultaneous replay created another attempt")
	}
	workflowHas(t, web.request(t, "GET", expected, web.viewer, nil, 200), "running", "Refresh status")
	busy := ReserveInput{1, uuid.New()}
	workflowHas(t, web.request(t, "POST", web.path+"/"+d.ID.String()+"/execute", web.editor, executeValues(web.editor, busy), 409), "TARGET_BUSY", busy.RequestKey.String())
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	completed := <-result
	done = true
	if completed.Code != 303 || completed.Header().Get("Location") != expected {
		t.Fatal("owner did not return original receipt")
	}
	workflowHas(t, web.request(t, "GET", expected, web.viewer, nil, 200), "succeeded")
	var value string
	if err := f.target.QueryRow(ctx, `SELECT value FROM public.draft_marker`).Scan(&value); err != nil || value != "unchanged 猫;once" {
		t.Fatal("concurrent HTTP replay repeated SQL")
	}
}
