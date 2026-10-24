package targets

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestHandlerInputAndPagination(t *testing.T) {
	for _, query := range []string{"limit=0", "limit=201", "offset=-1", "offset=100001", "limit=", "limit=1&limit=2", "actor=x", "limit=%zz", "limit=+1"} {
		if _, _, err := pagination(httptest.NewRequest("GET", "/databases?"+query, nil)); err == nil {
			t.Errorf("accepted query %q", query)
		}
	}
	limit, offset, err := pagination(httptest.NewRequest("GET", "/databases", nil))
	if err != nil || limit != 50 || offset != 0 {
		t.Fatal("pagination defaults differ from the contract")
	}
	if p := pager("/databases", 50, 25, true); p.Prev != "/databases?limit=50&offset=0" || p.Next != "/databases?limit=50&offset=75" {
		t.Fatal("pager did not preserve limit and clamp the previous offset")
	}
	if p := pager("/databases", 200, 99900, true); p.Next != "" {
		t.Fatal("pager linked beyond the supported offset")
	}
	values := url.Values{"_csrf": {"csrf"}, "expected_revision": {"1"}, "name": {"Name"}, "username": {"role"}, "ssl_mode": {"disable"}, "is_active": {"true"}}
	for _, password := range []string{"", "replacement"} {
		if password != "" {
			values.Set("password", password)
		}
		in, err := editInput(values)
		if err != nil || in.Password != password || !in.IsActive {
			t.Fatal("edit password omission/replacement mapping failed")
		}
	}
	if strings.Contains(fmt.Sprintf("%+v", submittedForm(values)), "replacement") {
		t.Fatal("redisplay form retained password")
	}
	values.Set("host", "new-host")
	if _, err := editInput(values); err == nil {
		t.Fatal("edit accepted immutable identity fields")
	}
	if _, err := NewHandler(nil, nil, nil); err == nil {
		t.Fatal("missing handler dependencies accepted")
	}
}

// Admin templates are an external dependency. Capture their typed data without
// inventing markup; all auth, guards, queries, mutations and user HTML are real.
type captureAdminPages struct {
	*httpx.Renderer
	page any
}

func (c *captureAdminPages) HasTemplate(name string) bool {
	return name == "admin_databases" || name == "admin_database" || c.Renderer.HasTemplate(name)
}

func (c *captureAdminPages) Render(w http.ResponseWriter, r *http.Request, status int, name string, page any) error {
	if name != "admin_databases" && name != "admin_database" {
		return c.Renderer.Render(w, r, status, name, page)
	}
	c.page = page
	w.WriteHeader(status)
	return nil
}

type handlerSession struct {
	id          uuid.UUID
	token, csrf string
}

func seedHandlerSession(t *testing.T, f targetFixture, service *auth.Service, user uuid.UUID) handlerSession {
	t.Helper()
	ctx := context.Background()
	state, token, err := service.CreateAnonymous(ctx)
	if err != nil {
		t.Fatal("cannot seed handler session token")
	}
	// Seed only this isolated database. There is no application login handler yet.
	if _, err := f.p.pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, user); err != nil {
		t.Fatal("cannot seed authenticated handler session")
	}
	if _, _, err := service.Validate(ctx, token, false); err != nil {
		t.Fatal("seeded session did not pass the real validator")
	}
	return handlerSession{id: state.ID, token: token, csrf: state.CSRF}
}

func TestDatabaseHandlers(t *testing.T) {
	f := fixture(t) // Verifies owner, path and system identifier before mutation.
	ctx := context.Background()
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil {
		t.Fatal("cannot load real page templates")
	}
	service, err := auth.NewService(f.p.pool, f.p.secrets, auth.Policy{AppEnv: "test", AppOrigin: "http://localhost:8080"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := auth.NewMiddleware(service)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := access.NewAuthorizer(f.p.pool)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(f.p, authorizer, renderer)
	if err != nil {
		t.Fatal(err)
	}
	realMux := http.NewServeMux()
	adminAvailable := handler.Register(realMux, middleware)
	if adminAvailable != (renderer.HasTemplate("admin_databases") && renderer.HasTemplate("admin_database")) {
		t.Fatal("admin registration does not match its template dependencies")
	}
	if !adminAvailable {
		for _, method := range []string{"GET", "POST"} {
			response := httptest.NewRecorder()
			realMux.ServeHTTP(response, httptest.NewRequest(method, "/admin/databases", nil))
			if response.Code != http.StatusNotFound {
				t.Fatal("admin routes mounted without their real templates")
			}
		}
	}
	capture := &captureAdminPages{Renderer: renderer}
	handler.renderer = capture
	mux := http.NewServeMux()
	if !handler.Register(mux, middleware) {
		t.Fatal("independent admin handlers not registered with their page dependency")
	}
	app := httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), mux)
	admin, viewer, noGrant := uuid.New(), uuid.New(), uuid.New()
	for _, user := range []struct {
		id    uuid.UUID
		name  string
		admin bool
	}{
		{admin, "admin_fixture", true}, {viewer, "viewer_fixture", false}, {noGrant, "ungranted_fixture", false},
	} {
		if _, err := f.p.pool.Exec(ctx, `INSERT INTO databrain.users(id,username,display_name,password_hash,is_platform_admin)
		    VALUES ($1,$2,'Display <script>text</script>','fixture-not-an-account-hash',$3)`, user.id, user.name, user.admin); err != nil {
			t.Fatal("cannot insert handler user fixture")
		}
	}
	adminSession := seedHandlerSession(t, f, service, admin)
	editorSession := seedHandlerSession(t, f, service, f.actor)
	viewerSession := seedHandlerSession(t, f, service, viewer)
	noneSession := seedHandlerSession(t, f, service, noGrant)
	request := func(method, path string, session handlerSession, values url.Values) *httptest.ResponseRecorder {
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
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "http://localhost:8080")
		}
		response := httptest.NewRecorder()
		capture.page = nil
		app.ServeHTTP(response, r)
		if strings.Contains(response.Body.String(), session.token) && session.token != "" || strings.Contains(response.Body.String(), f.input.Password) {
			t.Fatal("response disclosed a session token or target password")
		}
		return response
	}
	status := func(response *httptest.ResponseRecorder, want int) {
		t.Helper()
		if response.Code != want {
			t.Fatalf("response status=%d, want %d; body=%s", response.Code, want, response.Body.String())
		}
	}
	createForm := func(in CreateInput) url.Values {
		return url.Values{"_csrf": {adminSession.csrf}, "name": {in.Name}, "host": {in.Host}, "port": {strconv.Itoa(in.Port)},
			"database_name": {in.DatabaseName}, "username": {in.Username}, "password": {in.Password}, "ssl_mode": {in.SSLMode}}
	}
	firstInput := f.input
	firstInput.Name = "A <b>School</b>"
	first := mustCreate(t, f, firstInput)
	var second Target
	t.Run("create probe failure, validation, actor and redirect", func(t *testing.T) {
		bad := f.second
		bad.DatabaseName = "missing_handler_database"
		status(request("POST", "/admin/databases", adminSession, createForm(bad)), 422)
		page := capture.page.(AdminDatabasesPage)
		if page.Code != "TARGET_CONNECTION_FAILED" || page.Form.DatabaseName != bad.DatabaseName || strings.Contains(fmt.Sprintf("%+v", page), bad.Password) {
			t.Fatal("failed probe form lost safe data or retained password")
		}
		bad.Name = " "
		status(request("POST", "/admin/databases", adminSession, createForm(bad)), 422)
		var count int
		if err := f.p.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.database_targets").Scan(&count); err != nil || count != 1 {
			t.Fatal("failed create saved a record")
		}
		response := request("POST", "/admin/databases", adminSession, createForm(f.second))
		status(response, 303)
		id, err := uuid.Parse(strings.TrimPrefix(response.Header().Get("Location"), "/admin/databases/"))
		if err != nil {
			t.Fatal("create did not redirect to canonical detail URL")
		}
		second, err = f.p.Get(ctx, id)
		if err != nil || second.CreatedBy != admin {
			t.Fatal("created actor did not come from session")
		}
		status(request("POST", "/admin/databases", adminSession, createForm(f.second)), 409)
	})
	if second.ID == uuid.Nil {
		t.Fatal("second handler target unavailable")
	}
	viewGroup, editGroup := uuid.New(), uuid.New()
	if _, err := f.p.pool.Exec(ctx, `INSERT INTO databrain.groups(id,name) VALUES ($1,'Handler viewers'),($2,'Handler editors')`, viewGroup, editGroup); err != nil {
		t.Fatal("cannot seed handler groups")
	}
	if _, err := f.p.pool.Exec(ctx, `INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$3),($1,$4),($2,$3)`, viewGroup, editGroup, f.actor, viewer); err != nil {
		t.Fatal("cannot seed memberships")
	}
	if _, err := f.p.pool.Exec(ctx, `INSERT INTO databrain.database_grants(group_id,target_id,access_level)
	    VALUES ($1,$3,'view_only'),($1,$4,'view_only'),($2,$3,'view_edit')`, viewGroup, editGroup, first.ID, second.ID); err != nil {
		t.Fatal("cannot seed grants")
	}
	draft, attempt := uuid.New(), uuid.New()
	if _, err := f.p.pool.Exec(ctx, `INSERT INTO databrain.migration_drafts(id,target_id,title,sql,created_by,updated_by)
	    VALUES ($1,$2,'Handler draft','SELECT 1',$3,$3)`, draft, first.ID, f.actor); err != nil {
		t.Fatal("cannot seed active attempt draft")
	}
	hash := sha256.Sum256([]byte("SELECT 1"))
	if _, err := f.p.pool.Exec(ctx, `INSERT INTO databrain.migration_attempts
	    (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,target_config_revision,
	     sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
	    VALUES ($1,$2,$3,1,9007199254740993,'Handler draft',$4,1,'SELECT 1',$5,$6,'fixture_editor',$7,'reserved')`,
		attempt, first.ID, draft, first.Name, hash[:], f.actor, uuid.New()); err != nil {
		t.Fatal("cannot seed active attempt")
	}
	if _, err := f.p.pool.Exec(ctx, "UPDATE databrain.migration_streams SET active_attempt_id=$2 WHERE target_id=$1", first.ID, attempt); err != nil {
		t.Fatal("cannot link active attempt")
	}
	t.Run("grant-filtered typed user page and escaping", func(t *testing.T) {
		response := request("GET", "/databases", editorSession, nil)
		status(response, 200)
		body := response.Body.String()
		for _, want := range []string{"A &lt;b&gt;School&lt;/b&gt;", "Version 9007199254740993", "reserved", `name="_csrf" value="` + editorSession.csrf + `"`, "/databases/" + first.ID.String() + "/drafts"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing user page content %q", want)
			}
		}
		for _, hidden := range []string{f.input.Host, f.input.Username, f.input.DatabaseName, "<b>School</b>", "/databases/" + second.ID.String() + "/drafts", "SELECT 1"} {
			if strings.Contains(body, hidden) {
				t.Errorf("user page exposed %q", hidden)
			}
		}
		if response.Header().Get("Content-Type") != "text/html; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("page headers differ from contract")
		}
		body = request("GET", "/databases", viewerSession, nil).Body.String()
		if strings.Contains(body, "/drafts") || !strings.Contains(body, "Display &lt;script&gt;text&lt;/script&gt;") {
			t.Fatal("viewer got draft links or unsafe display identity")
		}
		for _, session := range []handlerSession{adminSession, noneSession} {
			response := request("GET", "/databases", session, nil)
			status(response, 200)
			if !strings.Contains(response.Body.String(), "You cannot open any database yet") || strings.Contains(response.Body.String(), first.ID.String()) {
				t.Fatal("ungranted user/admin received a database")
			}
		}
	})
	t.Run("list pagination and malformed query", func(t *testing.T) {
		response := request("GET", "/databases?limit=1", editorSession, nil)
		status(response, 200)
		if !strings.Contains(response.Body.String(), `href="/databases?limit=1&amp;offset=1" rel="next"`) {
			t.Fatal("next page missing")
		}
		response = request("GET", "/databases?limit=1&offset=1", editorSession, nil)
		status(response, 200)
		if !strings.Contains(response.Body.String(), `rel="prev"`) || strings.Contains(response.Body.String(), `rel="next"`) {
			t.Fatal("last page links are wrong")
		}
		for _, query := range []string{"limit=201", "offset=100001", "limit=1&limit=2", "user_id=" + admin.String()} {
			status(request("GET", "/databases?"+query, editorSession, nil), 400)
		}
	})
	t.Run("admin list includes inactive and ungranted metadata", func(t *testing.T) {
		edit := editOf(second)
		edit.IsActive = false
		updated, err := f.p.Edit(ctx, second.ID, edit)
		if err != nil {
			t.Fatal(err)
		}
		second = updated
		status(request("GET", "/admin/databases?limit=1&offset=1", adminSession, nil), 200)
		page := capture.page.(AdminDatabasesPage)
		if len(page.Databases) != 1 || page.Databases[0].ID != second.ID || page.Databases[0].IsActive || page.Pager.Prev == "" || page.Pager.Next != "" {
			t.Fatal("admin list reused user grants or lost inactive/pagination data")
		}
		status(request("GET", "/admin/databases/"+second.ID.String(), adminSession, nil), 200)
		detail := capture.page.(AdminDatabasePage)
		if !detail.Target.PasswordStored || detail.Form.ExpectedRevision != strconv.FormatInt(second.ConfigRevision, 10) || detail.Target.Host == "" {
			t.Fatal("safe detail metadata missing")
		}
		if strings.Contains(fmt.Sprintf("%+v", detail), f.input.Password) {
			t.Fatal("admin detail contains password")
		}
		status(request("GET", "/databases", editorSession, nil), 200)
	})
	t.Run("session and admin denial precede operations", func(t *testing.T) {
		for _, session := range []handlerSession{viewerSession, editorSession, noneSession} {
			status(request("GET", "/admin/databases", session, nil), 403)
			form := createForm(f.input)
			form.Set("_csrf", session.csrf)
			status(request("POST", "/admin/databases", session, form), 403)
		}
		for _, session := range []handlerSession{{}, {token: strings.Repeat("!", 43)}} {
			for _, method := range []string{"GET", "POST"} {
				response := request(method, "/admin/databases?invalid=1", session, nil)
				status(response, 303)
				if response.Header().Get("Location") != "/login" {
					t.Fatal("invalid session did not redirect to login")
				}
			}
		}
		for _, condition := range []string{"revoked_at=clock_timestamp()", "reauth_required=true", "last_seen_at=clock_timestamp()-interval '31 minutes'", "created_at=clock_timestamp()-interval '2 hours',absolute_expires_at=clock_timestamp()-interval '1 second'"} {
			session := seedHandlerSession(t, f, service, admin)
			if _, err := f.p.pool.Exec(ctx, "UPDATE databrain.sessions SET "+condition+" WHERE id=$1", session.id); err != nil {
				t.Fatal("cannot prepare invalid session")
			}
			response := request("GET", "/databases?invalid=1", session, nil)
			status(response, 303)
			if condition == "reauth_required=true" {
				for _, cookie := range response.Result().Cookies() {
					if cookie.MaxAge == -1 {
						t.Fatal("reauth family cookie was cleared")
					}
				}
			}
		}
	})
	t.Run("CSRF origin unknown fields and body limit", func(t *testing.T) {
		for _, mutation := range []func(url.Values){
			func(v url.Values) { v.Set("_csrf", strings.Repeat("B", 43)) },
			func(v url.Values) { v.Set("actor_id", admin.String()) },
			func(v url.Values) { v.Add("name", "duplicate") },
		} {
			form := createForm(f.input)
			mutation(form)
			response := request("POST", "/admin/databases", adminSession, form)
			if response.Code != 400 && response.Code != 403 {
				t.Fatal("invalid form passed to Provider")
			}
		}
		for _, origin := range []string{"", "https://foreign.example"} {
			r := httptest.NewRequest("POST", "/admin/databases", strings.NewReader(createForm(f.input).Encode()))
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: adminSession.token})
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", origin)
			response := httptest.NewRecorder()
			app.ServeHTTP(response, r)
			status(response, 403)
		}
		form := createForm(f.input)
		form.Set("name", strings.Repeat("x", int(httpx.NormalBodyLimit)+1))
		status(request("POST", "/admin/databases", adminSession, form), 413)
	})
	t.Run("edit revision, immutable fields and password preservation", func(t *testing.T) {
		path := "/admin/databases/" + second.ID.String()
		form := url.Values{"_csrf": {adminSession.csrf}, "expected_revision": {strconv.FormatInt(second.ConfigRevision, 10)}, "name": {"Renamed <script>text</script>"},
			"username": {second.Username}, "ssl_mode": {second.SSLMode}, "is_active": {"false"}}
		before, err := f.p.load(ctx, second.ID)
		if err != nil {
			t.Fatal(err)
		}
		response := request("POST", path, adminSession, form)
		status(response, 303)
		if response.Header().Get("Location") != path {
			t.Fatal("edit redirect is wrong")
		}
		after, err := f.p.load(ctx, second.ID)
		if err != nil || string(before.sealed) != string(after.sealed) || after.ConfigRevision != before.ConfigRevision+1 {
			t.Fatal("omitted password was changed or revision did not advance")
		}
		form.Set("password", "SENSITIVE-replacement")
		status(request("POST", path, adminSession, form), 409)
		page := capture.page.(AdminDatabasePage)
		if !page.Conflict || page.Form.ExpectedRevision != form.Get("expected_revision") || strings.Contains(fmt.Sprintf("%+v", page), "SENSITIVE-replacement") {
			t.Fatal("revision conflict lost stale revision or retained password")
		}
		form.Set("host", "new-host")
		status(request("POST", path, adminSession, form), 400)
		form.Del("host")
		form.Del("password")
		form.Set("expected_revision", "1")
		form.Set("username", first.Username)
		status(request("POST", "/admin/databases/"+first.ID.String(), adminSession, form), 409)
		if page := capture.page.(AdminDatabasePage); page.Code != "TARGET_BUSY" {
			t.Fatal("active migration did not block edit")
		}
		status(request("GET", "/admin/databases/INVALID", adminSession, nil), 400)
		status(request("GET", "/admin/databases/"+uuid.NewString(), adminSession, nil), 404)
		status(request("GET", path+"?unexpected=1", adminSession, nil), 400)
	})
	t.Run("platform failure is safe", func(t *testing.T) {
		canceled, stop := context.WithCancel(ctx)
		stop()
		r := httptest.NewRequest("GET", "/databases", nil).WithContext(canceled)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: editorSession.token})
		response := httptest.NewRecorder()
		app.ServeHTTP(response, r)
		status(response, 503)
		if strings.Contains(response.Body.String(), editorSession.token) || strings.Contains(response.Body.String(), f.input.Password) {
			t.Fatal("platform failure disclosed credentials")
		}
	})
	t.Run("stored probe success and failure stay 200 without saving", func(t *testing.T) {
		path := "/admin/databases/" + second.ID.String()
		before, err := f.p.Get(ctx, second.ID)
		if err != nil {
			t.Fatal(err)
		}
		form := url.Values{"_csrf": {adminSession.csrf}}
		status(request("POST", path+"/test", adminSession, form), 200)
		page := capture.page.(AdminDatabasePage)
		if page.Probe == nil || !page.Probe.OK || !supportedServerVersion(page.Probe.ServerVersion) {
			t.Fatal("stored probe did not return safe success metadata")
		}
		if _, err := f.owner.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.role}.Sanitize()+" NOLOGIN"); err != nil {
			t.Fatal("cannot prepare isolated probe failure")
		}
		defer func() {
			if _, err := f.owner.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.role}.Sanitize()+" LOGIN"); err != nil {
				t.Error("cannot restore isolated target role")
			}
		}()
		status(request("POST", path+"/test", adminSession, form), 200)
		page = capture.page.(AdminDatabasePage)
		if page.Probe == nil || page.Probe.OK || page.Probe.Reason != AuthenticationFailed {
			t.Fatal("failed stored probe was not a safe 200 result")
		}
		after, err := f.p.Get(ctx, second.ID)
		if err != nil || before != after {
			t.Fatal("probe saved configuration")
		}
		form.Set("username", "untrusted")
		status(request("POST", path+"/test", adminSession, form), 400)
	})
	if f.p.pool.Stat().AcquiredConns() != 0 {
		t.Fatal("handlers leaked a platform connection")
	}
}
