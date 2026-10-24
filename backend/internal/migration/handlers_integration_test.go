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
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
)

type draftSession struct {
	id          uuid.UUID
	token, csrf string
}
type draftHTTPFixture struct {
	draftFixture
	app                                                 http.Handler
	service                                             *auth.Service
	origin, path, otherPath                             string
	editor, otherEditor, viewer, none, admin, anonymous draftSession
	editGroup                                           uuid.UUID
}

func newDraftHTTPFixture(t *testing.T, origin string) draftHTTPFixture {
	t.Helper()
	f := newDraftFixture(t) // Owner/path/system identifier checked before mutation.
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.store.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal("cannot prepare isolated HTTP draft fixture")
		}
	}
	secrets, err := secret.New(bytes.Repeat([]byte{0x71}, 32))
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
	guard, err := access.NewAuthorizer(f.store.pool)
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil {
		t.Fatal("real draft templates unavailable")
	}
	handler, err := NewHandler(f.store, guard, renderer)
	if err != nil {
		t.Fatal(err)
	}
	mux := httpx.NewRouter(nil)
	handler.Register(mux, middleware)
	viewer, none, admin := uuid.New(), uuid.New(), uuid.New()
	for _, u := range []struct {
		id    uuid.UUID
		name  string
		admin bool
	}{{viewer, "draft_viewer", false}, {none, "draft_none", false}, {admin, "draft_admin", true}} {
		exec(`INSERT INTO databrain.users(id,username,display_name,password_hash,is_platform_admin) VALUES ($1,$2,'Display <script>identity</script>','not-a-login-hash',$3)`, u.id, u.name, u.admin)
	}
	editGroup, viewGroup := uuid.New(), uuid.New()
	exec(`INSERT INTO databrain.groups(id,name) VALUES ($1,'Draft editors'),($2,'Draft viewers')`, editGroup, viewGroup)
	exec(`INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$2),($1,$3),($4,$2),($4,$5)`, editGroup, f.actor, f.otherActor, viewGroup, viewer)
	for _, id := range []uuid.UUID{f.a, f.b} {
		exec(`INSERT INTO databrain.database_grants(group_id,target_id,access_level) VALUES ($1,$2,'view_edit'),($3,$2,'view_only')`, editGroup, id, viewGroup)
	}
	exec(`UPDATE databrain.database_targets SET name='Database <b>猫</b>' WHERE id=$1`, f.a)
	seed := func(user uuid.UUID) draftSession {
		state, token, err := service.CreateAnonymous(ctx)
		if err != nil {
			t.Fatal("cannot create test session")
		}
		if user != uuid.Nil { // Login handlers are Brain's pending work; validator/guard remain real.
			exec(`UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, user)
		}
		return draftSession{state.ID, token, state.CSRF}
	}
	return draftHTTPFixture{draftFixture: f, app: httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), mux), service: service, origin: origin, path: "/databases/" + f.a.String() + "/drafts", otherPath: "/databases/" + f.b.String() + "/drafts", editor: seed(f.actor), otherEditor: seed(f.otherActor), viewer: seed(viewer), none: seed(none), admin: seed(admin), anonymous: seed(uuid.Nil), editGroup: editGroup}
}

func (f draftHTTPFixture) request(t *testing.T, method, path string, session draftSession, values url.Values, want int) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if session.token != "" {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.token})
	}
	if method == "POST" {
		r.Header.Set("Origin", f.origin)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("draft HTTP %s: status %d, expected %d; body %.180s", method, w.Code, want, w.Body.String())
	}
	for _, private := range []string{session.token, "password_sealed", "postgres://", "127.0.0.1", "databrain_target"} {
		if private != "" && strings.Contains(w.Body.String(), private) {
			t.Fatal("draft response disclosed connection/session data")
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("draft response can be cached")
	}
	return w
}

func draftValues(session draftSession, title, sql, revision string) url.Values {
	v := url.Values{"_csrf": {session.csrf}, "title": {title}, "sql": {sql}}
	if revision != "" {
		v.Set("expected_revision", revision)
	}
	return v
}

func TestDraftHandlersPostgres(t *testing.T) {
	f := newDraftHTTPFixture(t, "http://localhost:8080")
	ctx := context.Background()
	input := DraftInput{Title: "  Draft <b>猫</b>  ", SQL: "\r\nCREAT TABLE invalid;\t\n</textarea><script>fixture</script>  "}
	d, err := f.store.Create(ctx, f.a, f.actor, input)
	if err != nil {
		t.Fatal(err)
	}
	detail := f.path + "/" + d.ID.String()
	has := func(t *testing.T, w *httptest.ResponseRecorder, parts ...string) {
		t.Helper()
		for _, part := range parts {
			if !strings.Contains(w.Body.String(), part) {
				t.Fatalf("draft HTML missing %q", part)
			}
		}
	}
	t.Run("session and Edit guard on every route", func(t *testing.T) {
		for _, path := range []string{f.path, detail} {
			for _, session := range []draftSession{{}, {token: "invalid"}, f.anonymous} {
				for _, method := range []string{"GET", "POST"} {
					w := f.request(t, method, path, session, draftValues(session, "x", "invalid sql", "1"), 303)
					if w.Header().Get("Location") != "/login" {
						t.Fatal("invalid session did not redirect")
					}
				}
			}
			for _, tc := range []struct {
				s      draftSession
				status int
			}{{f.viewer, 403}, {f.none, 404}, {f.admin, 404}} {
				f.request(t, "GET", path, tc.s, nil, tc.status)
				f.request(t, "POST", path, tc.s, draftValues(tc.s, "forbidden", "SQL", "1"), tc.status)
			}
		}
	})
	t.Run("editor read list pagination and escaped detail", func(t *testing.T) {
		w := f.request(t, "GET", detail, f.editor, nil, 200)
		has(t, w, html.EscapeString(input.Title), html.EscapeString(input.SQL), `name="expected_revision" value="1"`, "draft_editor_a", "Database &lt;b&gt;猫&lt;/b&gt;")
		if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "/review") || strings.Contains(w.Body.String(), "/execute") {
			t.Fatal("draft HTML exposes markup or unavailable execution flow")
		}
		if _, err := f.store.Create(ctx, f.a, f.actor, DraftInput{"second", "large private SQL body"}); err != nil {
			t.Fatal(err)
		}
		list := f.request(t, "GET", f.path+"?limit=1", f.editor, nil, 200)
		has(t, list, `rel="next"`)
		if strings.Contains(list.Body.String(), "large private SQL body") {
			t.Fatal("list included a full draft script")
		}
		has(t, f.request(t, "GET", f.path+"?limit=1&offset=1", f.editor, nil, 200), `rel="prev"`)
		has(t, f.request(t, "GET", f.otherPath, f.editor, nil, 200), "No drafts yet")
		f.request(t, "GET", f.path+"?offset=100000", f.editor, nil, 200)
		for _, query := range []string{"actor=forged", "limit=0", "offset=100001", "limit=1&limit=2", "limit=%ff"} {
			f.request(t, "GET", f.path+"?"+query, f.editor, nil, 400)
		}
		f.request(t, "GET", detail+"?revision=1", f.editor, nil, 400)
		f.request(t, "GET", "/databases/not-a-uuid/drafts", f.editor, nil, 400)
		f.request(t, "GET", f.path+"/not-a-uuid", f.editor, nil, 400)
		f.request(t, "GET", f.path+"/"+uuid.NewString(), f.editor, nil, 404)
		f.request(t, "GET", f.otherPath+"/"+d.ID.String(), f.editor, nil, 404)
		f.request(t, "POST", f.otherPath+"/"+d.ID.String(), f.editor, draftValues(f.editor, "cross target", "x", "1"), 404)
	})
	t.Run("create save PRG and optimistic conflict", func(t *testing.T) {
		w := f.request(t, "POST", f.path, f.editor, draftValues(f.editor, input.Title, input.SQL, ""), 303)
		location := w.Header().Get("Location")
		if !strings.HasPrefix(location, f.path+"/") {
			t.Fatal("create redirect did not target saved draft")
		}
		id, err := uuid.Parse(strings.TrimPrefix(location, f.path+"/"))
		if err != nil {
			t.Fatal("invalid resource redirect")
		}
		created, err := f.store.Get(ctx, f.a, id)
		if err != nil || created.Title != input.Title || created.SQL != input.SQL || created.CreatedBy != f.actor || created.Revision != 1 {
			t.Fatal("POST create did not preserve decoded input/actor")
		}
		f.request(t, "GET", location, f.editor, nil, 200)
		f.request(t, "GET", location, f.editor, nil, 200)
		savedSQL := "CREATE TABLE public.never_executed(id int); UPDATE public.draft_marker SET value='changed';"
		w = f.request(t, "POST", location, f.otherEditor, draftValues(f.otherEditor, "saved by another editor", savedSQL, "1"), 303)
		if w.Header().Get("Location") != location {
			t.Fatal("save redirect changed resource")
		}
		w = f.request(t, "POST", location, f.editor, draftValues(f.editor, input.Title, input.SQL, "1"), 409)
		has(t, w, `name="expected_revision" value="1"`, html.EscapeString(input.Title), html.EscapeString(input.SQL), "Open the latest version")
		stored, err := f.store.Get(ctx, f.a, id)
		if err != nil || stored.Revision != 2 || stored.SQL != savedSQL || stored.UpdatedBy != f.otherActor {
			t.Fatal("stale form overwrote newer revision or actor")
		}
		latest := f.request(t, "GET", location, f.editor, nil, 200)
		has(t, latest, `name="expected_revision" value="2"`, "draft_editor_b")
	})
	t.Run("field validation preserves input and rejects forged fields", func(t *testing.T) {
		w := f.request(t, "POST", f.path, f.editor, draftValues(f.editor, " ", input.SQL, ""), 422)
		has(t, w, "Title must contain", html.EscapeString(input.SQL))
		w = f.request(t, "POST", detail, f.editor, draftValues(f.editor, "still here", "", "1"), 422)
		has(t, w, "SQL must contain", `value="still here"`, `name="expected_revision" value="1"`)
		f.request(t, "POST", detail, f.editor, draftValues(f.editor, strings.Repeat("猫", 121), input.SQL, "1"), 422)
		f.request(t, "POST", detail, f.editor, draftValues(f.editor, "large", strings.Repeat("x", MaxSQLBytes+1), "1"), 422)
		for _, field := range []string{"actor", "database_id", "request_key"} {
			v := draftValues(f.editor, "title", "sql", "")
			v.Set(field, "forged")
			f.request(t, "POST", f.path, f.editor, v, 400)
		}
		for _, revision := range []string{"0", "-1", "garbage"} {
			f.request(t, "POST", detail, f.editor, draftValues(f.editor, "title", "sql", revision), 400)
		}
		duplicate := draftValues(f.editor, "title", "sql", "")
		duplicate["title"] = []string{"a", "b"}
		f.request(t, "POST", f.path, f.editor, duplicate, 400)
		f.request(t, "POST", f.path+"?actor=forged", f.editor, draftValues(f.editor, "title", "sql", ""), 400)
		stored, err := f.store.Get(ctx, f.a, d.ID)
		if err != nil || stored.Revision != 1 || stored.SQL != input.SQL {
			t.Fatal("rejected form changed draft")
		}
	})
	t.Run("CSRF origin body bounds and media type", func(t *testing.T) {
		bad := draftValues(f.editor, "title", "sql", "")
		bad.Set("_csrf", f.viewer.csrf)
		f.request(t, "POST", f.path, f.editor, bad, 403)
		for _, tc := range []struct {
			origin, media, body string
			want                int
		}{
			{"", "application/x-www-form-urlencoded", draftValues(f.editor, "title", "sql", "").Encode(), 403},
			{"http://foreign.test", "application/x-www-form-urlencoded", draftValues(f.editor, "title", "sql", "").Encode(), 403},
			{f.origin, "application/json", `{}`, 415},
			{f.origin, "application/x-www-form-urlencoded", strings.Repeat("x", int(httpx.MaxFormBody)+1), 413},
			{f.origin, "application/x-www-form-urlencoded", "_csrf=" + f.editor.csrf + "&title=%ff&sql=x", 400},
		} {
			r := httptest.NewRequest("POST", f.path, strings.NewReader(tc.body))
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: f.editor.token})
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("Content-Type", tc.media)
			w := httptest.NewRecorder()
			f.app.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("origin/media/body status %d, want %d", w.Code, tc.want)
			}
		}
	})
	t.Run("grant and session changes apply immediately", func(t *testing.T) {
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := f.store.pool.Exec(ctx, sql, args...); err != nil {
				t.Fatal("cannot update isolated access fixture")
			}
		}
		exec(`UPDATE databrain.database_targets SET is_active=false WHERE id=$1`, f.a)
		f.request(t, "GET", detail, f.editor, nil, 404)
		f.request(t, "POST", detail, f.editor, draftValues(f.editor, "x", "x", "1"), 404)
		exec(`UPDATE databrain.database_targets SET is_active=true WHERE id=$1`, f.a)
		exec(`UPDATE databrain.groups SET is_active=false WHERE id=$1`, f.editGroup)
		f.request(t, "GET", detail, f.editor, nil, 403) // A remaining View grant cannot edit.
		exec(`UPDATE databrain.groups SET is_active=true WHERE id=$1`, f.editGroup)
		exec(`UPDATE databrain.sessions SET revoked_at=now() WHERE id=$1`, f.editor.id)
		f.request(t, "GET", detail, f.editor, nil, 303)
		f.request(t, "POST", detail, f.editor, draftValues(f.editor, "x", "x", "1"), 303)
	})
	t.Run("save never executes target or reserves an attempt", func(t *testing.T) {
		var value string
		var relations, attempts int
		if err := f.target.QueryRow(ctx, "SELECT value FROM public.draft_marker").Scan(&value); err != nil || value != "unchanged 猫" {
			t.Fatal("draft HTTP changed target data")
		}
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`).Scan(&relations); err != nil || relations != 1 {
			t.Fatal("draft HTTP changed target schema")
		}
		if err := f.store.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.migration_attempts").Scan(&attempts); err != nil || attempts != 0 {
			t.Fatal("draft HTTP started execution")
		}
		if f.store.pool.Stat().AcquiredConns() != 0 {
			t.Fatal("draft HTTP leaked platform connections")
		}
		f.request(t, "GET", detail+"/review", f.otherEditor, nil, 404)
		f.request(t, "POST", detail+"/execute", f.otherEditor, nil, 404)
	})
}
