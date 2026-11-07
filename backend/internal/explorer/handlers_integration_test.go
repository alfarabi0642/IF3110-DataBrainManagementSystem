package explorer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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

func TestExplorerAPIPostgres(t *testing.T) {
	f := newCatalogFixture(t) // Identitas fixture diperiksa sebelum mutasi.
	ctx := context.Background()
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
	mux := http.NewServeMux()
	f.e.RegisterAPI(mux, middleware, f.pool, guard)
	app := httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), httpx.APIHandler(mux))
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
				t.Fatalf("explorer JSON missing %q", part)
			}
		}
	}
	selected := explorerHref(path, schema, table, "data", 2, 0)
	t.Run("JSON selection access precision and response bounds", func(t *testing.T) {
		api := "/api" + path
		read := func(href string) (explorerDTO, *httpx.Page) {
			t.Helper()
			w := request(t, "GET", href, viewSession, 200)
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatal("API returned a non-JSON response")
			}
			var body struct {
				Data explorerDTO `json:"data"`
				Page *httpx.Page `json:"page"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			return body.Data, body.Page
		}
		for _, session := range []explorerSession{{}, {token: "invalid"}, anonSession} {
			w := request(t, "GET", api, session, 401)
			if w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), `"error"`) {
				t.Fatal("API redirected or omitted error envelope")
			}
		}
		for _, session := range []explorerSession{noneSession, adminSession} {
			request(t, "GET", api+"?unknown=1", session, 404)
		}
		request(t, "GET", "/api/databases/"+uuid.NewString()+"/explorer", viewSession, 404)
		request(t, "GET", "/api/databases/invalid/explorer", viewSession, 400)
		request(t, "POST", api, editSession, 405)
		root, _ := read(api)
		if len(root.Schemas) == 0 || root.Database.Access != access.ViewOnly || root.Database.Name != "Database <b>fixture</b>" {
			t.Fatal("schema list or safe workspace projection incorrect")
		}
		list, _ := read(explorerHref(api, schema, "", "data", 2, 0))
		if len(list.Tables) != 5 {
			t.Fatal("table list incorrect")
		}
		first, page := read(explorerHref(api, schema, table, "data", 2, 0))
		if first.Schema != schema || first.Table != table || first.Tab != "data" || first.Data == nil ||
			len(first.Data.Rows) != 2 || first.Data.Rows[0][1] != (Cell{Null: true}) || first.Data.Rows[1][1] != (Cell{Text: "null"}) ||
			first.Data.Rows[0][2].Text != "12345678901234567890.1234" || page == nil || page.NextOffset == nil || *page.NextOffset != 2 || page.PrevOffset != nil {
			t.Fatal("JSON selection, NULL, precision or first page changed")
		}
		last, page := read(explorerHref(api, schema, table, "data", 2, 2))
		if last.Data.Rows[0][1] != (Cell{Text: ""}) || page.NextOffset != nil || page.PrevOffset == nil || *page.PrevOffset != 0 {
			t.Fatal("empty text or last page changed")
		}
		for _, tab := range []string{"columns", "constraints", "indexes"} {
			metadata, page := read(explorerHref(api, schema, table, tab, 2, 0))
			if metadata.Tab != tab || metadata.Data != nil || page != nil || len(metadata.Columns) != 4 || len(metadata.Constraints) != 2 || len(metadata.Indexes) != 2 || metadata.Columns[0].Default != nil || metadata.Columns[0].Identity != "a" {
				t.Fatal("metadata types or absent default changed")
			}
		}
		second, _ := read(explorerHref("/api"+secondPath, schema, table, "data", 2, 0))
		if !reflect.DeepEqual(second.Data.Columns, []string{"other"}) || second.Data.Rows[0][0].Text != "SECOND_TARGET_ONLY" {
			t.Fatal("JSON data crossed targets")
		}
		for _, query := range []string{"schema=", "schema=x&schema=y", "unknown=x", "schema=%ff", "schema=%zz", "table=t", "limit=0", "limit=201", "offset=-1", "offset=100001", "limit=1.5", "tab=other", "schema=s&table=t&tab=columns&offset=0"} {
			request(t, "GET", api+"?"+query, viewSession, 400)
		}
		for _, missing := range [][2]string{{"missing", table}, {schema, "missing"}, {"pg_catalog", "pg_class"}, {schema, `'; DROP TABLE x;--`}} {
			request(t, "GET", explorerHref(api, missing[0], missing[1], "data", 2, 0), viewSession, 404)
		}
		for _, item := range []struct {
			table         string
			limit, status int
			code          string
		}{
			{"oversized", 2, 422, "CELL_TOO_LARGE"}, {"big_page", 9, 413, "RESULT_TOO_LARGE"},
		} {
			has(t, request(t, "GET", explorerHref(api, schema, item.table, "data", item.limit, 0), viewSession, item.status), item.code)
		}
		fixtureSQL(t, f.writers[0], "CREATE TABLE "+qs+`.changing(id bigint); INSERT INTO `+qs+`.changing VALUES (9223372036854775807)`)
		changed, _ := read(explorerHref(api, schema, "changing", "data", 1, 0))
		if changed.Data.Rows[0][0].Text != "9223372036854775807" {
			t.Fatal("bigint precision changed")
		}
		fixtureSQL(t, f.writers[0], "ALTER TABLE "+qs+`.changing RENAME COLUMN id TO renamed`)
		changed, _ = read(explorerHref(api, schema, "changing", "data", 1, 0))
		if changed.Data.Columns[0] != "renamed" {
			t.Fatal("stale column metadata reused")
		}
		fixtureSQL(t, f.writers[0], "DROP TABLE "+qs+`.changing`)
		request(t, "GET", explorerHref(api, schema, "changing", "data", 1, 0), viewSession, 404)
		definitions := make([]string, 900)
		for i := range definitions {
			definitions[i] = "c" + strconv.Itoa(i) + " int"
		}
		fixtureSQL(t, f.writers[0], "CREATE TABLE "+qs+".json_budget ("+strings.Join(definitions, ",")+"); INSERT INTO "+qs+".json_budget SELECT FROM generate_series(1,200)")
		w := request(t, "GET", explorerHref(api, schema, "json_budget", "data", 200, 0), viewSession, 413)
		if strings.Contains(w.Body.String(), `"data"`) || !strings.Contains(w.Body.String(), "RESULT_TOO_LARGE") {
			t.Fatal("oversized JSON returned a partial success")
		}
		read(explorerHref(api, schema, "json_budget", "data", 1, 0))
	})
	t.Run("current grants target state and revoked sessions", func(t *testing.T) {
		exec(`UPDATE databrain.database_targets SET is_active=false WHERE id=$1`, f.targets[0])
		request(t, "GET", "/api"+selected, viewSession, 404)
		exec(`UPDATE databrain.database_targets SET is_active=true WHERE id=$1`, f.targets[0])
		exec(`UPDATE databrain.groups SET is_active=false WHERE id=$1`, viewGroup)
		request(t, "GET", "/api"+selected, viewSession, 404)
		exec(`UPDATE databrain.groups SET is_active=true WHERE id=$1`, viewGroup)
		exec(`DELETE FROM databrain.database_grants WHERE group_id=$1 AND target_id=$2`, viewGroup, f.targets[0])
		request(t, "GET", "/api"+selected, viewSession, 404)
		request(t, "GET", explorerHref("/api"+secondPath, schema, table, "data", 2, 0), viewSession, 200)
		exec(`UPDATE databrain.sessions SET revoked_at=clock_timestamp() WHERE id=$1`, viewSession.id)
		request(t, "GET", "/api"+secondPath, viewSession, 401)
	})
	// Handler tidak menulis ke target; perubahan hanya berasal dari SQL fixture yang eksplisit.
	var count int
	if err := f.writers[0].QueryRow(ctx, "SELECT count(*) FROM "+qt).Scan(&count); err != nil || count != 4 {
		t.Fatal("explorer requests changed target rows")
	}
}
