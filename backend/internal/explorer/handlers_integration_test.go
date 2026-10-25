package explorer

import (
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type explorerSession struct {
	id    uuid.UUID
	token string
}

func explorerSessionFor(t *testing.T, f catalogFixture, service *auth.Service, user uuid.UUID) explorerSession {
	t.Helper()
	state, token, err := service.CreateAnonymous(context.Background())
	if err != nil {
		t.Fatal("cannot create explorer session fixture")
	}
	if user != uuid.Nil {
		// Session dibuat langsung untuk mengisolasi pengujian explorer dari login. Request tetap melewati
		// validator cookie dan Authorizer asli.
		if _, err := f.pool.Exec(context.Background(), `UPDATE databrain.sessions
		    SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, user); err != nil {
			t.Fatal("cannot authenticate explorer session fixture")
		}
	}
	return explorerSession{state.ID, token}
}

func TestExplorerHandlersPostgres(t *testing.T) {
	f := newCatalogFixture(t) // Identitas fixture diperiksa sebelum mutasi.
	ctx := context.Background()
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil {
		t.Fatal("real explorer templates unavailable")
	}
	service, err := auth.NewService(f.pool, f.secrets, auth.Policy{AppEnv: "test", AppOrigin: "http://localhost:8080"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := auth.NewMiddleware(service)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := access.NewAuthorizer(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(f.e, f.pool, guard, renderer)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handler.Register(mux, middleware)
	app := httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), mux)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal("cannot update isolated explorer platform fixture")
		}
	}
	viewer, editor, noGrant, admin := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, user := range []struct {
		id    uuid.UUID
		name  string
		admin bool
	}{{viewer, "explorer_viewer", false}, {editor, "explorer_editor", false}, {noGrant, "explorer_none", false}, {admin, "explorer_admin", true}} {
		exec(`INSERT INTO databrain.users(id,username,display_name,password_hash,is_platform_admin)
		    VALUES ($1,$2,'Display <script>identity</script>','not-a-login-hash',$3)`, user.id, user.name, user.admin)
	}
	viewGroup, editGroup := uuid.New(), uuid.New()
	exec(`INSERT INTO databrain.groups(id,name) VALUES ($1,'Explorer viewers'),($2,'Explorer editors')`, viewGroup, editGroup)
	exec(`INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$2),($3,$4)`, viewGroup, viewer, editGroup, editor)
	for _, id := range f.targets {
		exec(`INSERT INTO databrain.database_grants(group_id,target_id,access_level) VALUES ($1,$2,'view_only'),($3,$2,'view_edit')`, viewGroup, id, editGroup)
	}
	exec(`UPDATE databrain.database_targets SET name='Database <b>fixture</b>' WHERE id=$1`, f.targets[0])
	viewSession := explorerSessionFor(t, f, service, viewer)
	editSession := explorerSessionFor(t, f, service, editor)
	noneSession := explorerSessionFor(t, f, service, noGrant)
	adminSession := explorerSessionFor(t, f, service, admin)
	anonSession := explorerSessionFor(t, f, service, uuid.Nil)
	path := "/databases/" + f.targets[0].String() + "/explorer"
	secondPath := "/databases/" + f.targets[1].String() + "/explorer"
	schema, table := `Odd &"猫.schema`, `Order '<x>.table`
	qs, qt := pgx.Identifier{schema}.Sanitize(), pgx.Identifier{schema, table}.Sanitize()
	fixtureSQL(t, f.writers[0], "CREATE SCHEMA "+qs+"; CREATE TABLE "+qt+` (
	    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, note text DEFAULT '<default>',
	    amount numeric(30,4) DEFAULT 1.2500, computed bigint GENERATED ALWAYS AS (id+1) STORED,
	    CONSTRAINT "amount check" CHECK(amount>=0)); INSERT INTO `+qt+`(note,amount) VALUES
	    (NULL,12345678901234567890.1234),('null',2),('',3),('<script>row</script>',4);
	    CREATE INDEX "note idx" ON `+qt+`(note); CREATE TABLE `+qs+`.empty_table(id int);
	    CREATE TABLE `+qs+`.unordered(note text); INSERT INTO `+qs+`.unordered VALUES ('no key');
	    CREATE TABLE `+qs+`.oversized(note text); INSERT INTO `+qs+`.oversized VALUES (repeat('x',65537));
	    CREATE TABLE `+qs+`.big_page(note text); INSERT INTO `+qs+`.big_page SELECT repeat('x',65536) FROM generate_series(1,9);`)
	fixtureSQL(t, f.writers[1], "CREATE SCHEMA "+qs+"; CREATE TABLE "+qt+"(other text); INSERT INTO "+qt+" VALUES ('SECOND_TARGET_ONLY')")
	request := func(t *testing.T, method, href string, session explorerSession, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, href, nil)
		if session.token != "" {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.token})
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("explorer status=%d, want %d", w.Code, want)
		}
		body := w.Body.String()
		for _, secret := range []string{session.token, "databrain_target", "127.0.0.1", "password_sealed", "postgres://"} {
			if secret != "" && strings.Contains(body, secret) {
				t.Fatal("explorer response disclosed private connection or session data")
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("explorer response can be cached")
		}
		return w
	}
	has := func(t *testing.T, w *httptest.ResponseRecorder, parts ...string) {
		t.Helper()
		for _, part := range parts {
			if !strings.Contains(w.Body.String(), part) {
				t.Fatalf("explorer HTML missing %q", part)
			}
		}
	}
	selected := explorerHref(path, schema, table, "data", 2, 0)
	t.Run("session and grant before target lookup", func(t *testing.T) {
		for _, session := range []explorerSession{{}, {token: "invalid"}, anonSession} {
			w := request(t, "GET", selected, session, 303)
			if w.Header().Get("Location") != "/login" {
				t.Fatal("missing session did not redirect to login")
			}
		}
		for _, session := range []explorerSession{noneSession, adminSession} {
			request(t, "GET", selected, session, 404)
			request(t, "GET", path+"?schema=bad&table=missing&unknown=1", session, 404)
		}
		request(t, "GET", "/databases/"+uuid.NewString()+"/explorer", viewSession, 404)
		request(t, "GET", "/databases/not-a-uuid/explorer", viewSession, 400)
		request(t, "POST", selected, editSession, 405)
	})
	t.Run("schemas tables tabs encoded links and pagination", func(t *testing.T) {
		has(t, request(t, "GET", path, viewSession, 200), html.EscapeString(schema))
		has(t, request(t, "GET", explorerHref(path, schema, "", "data", 2, 0), viewSession, 200), html.EscapeString(table))
		first := request(t, "GET", selected, viewSession, 200)
		has(t, first, `class="null">NULL`, `>null</td>`, "12345678901234567890.1234", `rel="next"`, `Database &lt;b&gt;fixture&lt;/b&gt;`)
		if strings.Contains(first.Body.String(), "/drafts") || strings.Contains(first.Body.String(), "<script>") || strings.Contains(first.Body.String(), `rel="prev"`) {
			t.Fatal("viewer navigation, escaping or first-page controls incorrect")
		}
		has(t, request(t, "GET", selected, editSession, 200), "/drafts")
		next := explorerHref(path, schema, table, "data", 2, 2)
		last := request(t, "GET", next, viewSession, 200)
		has(t, last, "(empty)", "&lt;script&gt;row&lt;/script&gt;", `rel="prev"`)
		if strings.Contains(last.Body.String(), `rel="next"`) {
			t.Fatal("last page still has next link")
		}
		// Setiap link explorer diuji kembali melalui handler asli.
		for _, entry := range strings.Split(last.Body.String(), `href="`)[1:] {
			href := html.UnescapeString(strings.SplitN(entry, `"`, 2)[0])
			if strings.HasPrefix(href, path) {
				u, err := url.Parse(href)
				if err != nil || u.Query().Get("schema") != schema && u.Query().Has("table") {
					t.Fatal("identifier lost in link")
				}
				if u.Query().Get("tab") != "" && u.Query().Get("tab") != "data" && (u.Query().Has("offset") || u.Query().Has("limit")) {
					t.Fatal("metadata tab retained data pagination")
				}
				request(t, "GET", href, viewSession, 200)
			}
		}
		has(t, request(t, "GET", explorerHref(path, schema, table, "columns", 2, 0), viewSession, 200), "always", "stored", "numeric(30,4)", "&lt;default&gt;")
		has(t, request(t, "GET", explorerHref(path, schema, table, "constraints", 2, 0), viewSession, 200), "Primary key", "Check", "amount")
		has(t, request(t, "GET", explorerHref(path, schema, table, "indexes", 2, 0), viewSession, 200), "note idx", "btree", "CREATE INDEX")
	})
	t.Run("empty unordered failures limits and target isolation", func(t *testing.T) {
		has(t, request(t, "GET", explorerHref(path, schema, "empty_table", "data", 2, 0), viewSession, 200), "No rows on this page.")
		has(t, request(t, "GET", explorerHref(path, schema, "empty_table", "constraints", 2, 0), viewSession, 200), "no constraints")
		has(t, request(t, "GET", explorerHref(path, schema, "empty_table", "indexes", 2, 0), viewSession, 200), "no indexes")
		has(t, request(t, "GET", explorerHref(path, schema, "unordered", "data", 2, 0), viewSession, 200), "order is unspecified")
		has(t, request(t, "GET", explorerHref(path, schema, "oversized", "data", 2, 0), viewSession, 422), "CELL_TOO_LARGE")
		has(t, request(t, "GET", explorerHref(path, schema, "big_page", "data", 9, 0), viewSession, 413), "RESULT_TOO_LARGE")
		request(t, "GET", explorerHref(path, schema, "missing", "data", 2, 0), viewSession, 404)
		for _, query := range []string{"schema=", "schema=x&schema=y", "unknown=x", "schema=%ff", "schema=%zz", "limit=0", "schema=s&table=t&tab=columns&offset=0"} {
			request(t, "GET", path+"?"+query, viewSession, 400)
		}
		second := request(t, "GET", explorerHref(secondPath, schema, table, "data", 2, 0), viewSession, 200)
		has(t, second, "SECOND_TARGET_ONLY")
		if strings.Contains(second.Body.String(), "12345678901234567890.1234") || strings.Contains(second.Body.String(), "Database &lt;b&gt;fixture") {
			t.Fatal("previous target data leaked into next target")
		}
		exec(`UPDATE databrain.database_targets SET username='missing_explorer_role' WHERE id=$1`, f.targets[0])
		has(t, request(t, "GET", selected, viewSession, 503), "TARGET_UNAVAILABLE")
		exec(`UPDATE databrain.database_targets SET username='databrain_target' WHERE id=$1`, f.targets[0])
	})
	t.Run("current grants target state and revoked sessions", func(t *testing.T) {
		exec(`UPDATE databrain.database_targets SET is_active=false WHERE id=$1`, f.targets[0])
		request(t, "GET", selected, viewSession, 404)
		exec(`UPDATE databrain.database_targets SET is_active=true WHERE id=$1`, f.targets[0])
		exec(`UPDATE databrain.groups SET is_active=false WHERE id=$1`, viewGroup)
		request(t, "GET", selected, viewSession, 404)
		exec(`UPDATE databrain.groups SET is_active=true WHERE id=$1`, viewGroup)
		exec(`DELETE FROM databrain.database_grants WHERE group_id=$1 AND target_id=$2`, viewGroup, f.targets[0])
		request(t, "GET", selected, viewSession, 404)
		request(t, "GET", explorerHref(secondPath, schema, table, "data", 2, 0), viewSession, 200)
		exec(`UPDATE databrain.sessions SET revoked_at=clock_timestamp() WHERE id=$1`, viewSession.id)
		request(t, "GET", secondPath, viewSession, 303)
	})
	// Handler tidak menulis ke target; perubahan hanya berasal dari SQL fixture yang eksplisit.
	var count int
	if err := f.writers[0].QueryRow(ctx, "SELECT count(*) FROM "+qt).Scan(&count); err != nil || count != 4 {
		t.Fatal("explorer requests changed target rows")
	}
}
