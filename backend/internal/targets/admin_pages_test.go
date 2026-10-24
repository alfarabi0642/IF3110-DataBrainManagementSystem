package targets

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

// Renders the shipped admin templates with the handler's real page structs, so a
// renamed field fails here instead of at request time. No database is needed.
func renderAdmin(t *testing.T, name string, page any) string {
	t.Helper()
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil || renderer == nil || !renderer.HasTemplate("admin_databases") || !renderer.HasTemplate("admin_database") {
		t.Fatalf("admin templates not available: %v", err)
	}
	w := httptest.NewRecorder()
	if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/", nil), 200, name, page); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return w.Body.String()
}

func requireText(t *testing.T, body string, present bool, want ...string) {
	t.Helper()
	for _, s := range want {
		if strings.Contains(body, s) != present {
			t.Errorf("present=%v for %q in:\n%s", present, s, body)
		}
	}
}

func TestDatabasesPage(t *testing.T) {
	layout := httpx.Layout{Title: "Databases", Active: "databases", CSRF: "tok", User: &httpx.LayoutUser{Username: "ana"}}
	page := DatabasesPage{Layout: layout, Pager: Pager{Next: "/databases?limit=50&offset=50"}, Databases: []DatabaseItem{
		{ID: "a", Name: "<i>edit</i>", Access: access.ViewEdit, ActiveAttempt: &ActiveAttempt{ID: "x", Version: "7", State: "running"}},
		{ID: "b", Name: "viewer", Access: access.ViewOnly},
	}}
	body := renderAdmin(t, "databases", page)
	requireText(t, body, true, "&lt;i&gt;edit", `href="/databases/a/drafts"`, `href="/databases/b/explorer"`, `href="/databases/b/migrations"`,
		`href="/databases/a/migrations/x"`, `class="badge running"`, "View and edit", "View only", `rel="next"`)
	requireText(t, body, false, "<i>edit", `href="/databases/b/drafts"`, `rel="prev"`)

	page.Databases, page.Pager = nil, Pager{}
	requireText(t, renderAdmin(t, "databases", page), true, "Ask an admin")
}

func TestAdminDatabasePage(t *testing.T) {
	id := uuid.MustParse("3f2b8e64-0c1a-4c57-9b0e-5d8f1a2b3c4d")
	target := Target{ID: id, Name: "<b>prod</b>", Host: "db.example", Port: 5432, DatabaseName: "orders", Username: "ro",
		SSLMode: "verify-full", IsActive: true, PasswordStored: true, ConfigRevision: 7, UpdatedAt: time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)}
	layout := httpx.Layout{Title: "Connection", Active: "connections", CSRF: "tok", User: &httpx.LayoutUser{Username: "root", IsAdmin: true}}
	page := AdminDatabasePage{Layout: layout, Target: target, Form: formOf(target),
		Probe: &ProbeResult{OK: false, Reason: AuthenticationFailed}}

	body := renderAdmin(t, "admin_database", page)
	requireText(t, body, true, "&lt;b&gt;prod", `action="/admin/databases/`+id.String()+`"`, `action="/admin/databases/`+id.String()+`/test"`,
		`name="expected_revision" value="7"`, `name="_csrf" value="tok"`, `id="password" name="password" type="password" autocomplete="new-password" value=""`,
		"rejected the username or password", "2026-10-24 01:02:03 UTC", "Stored", `<option value="true" selected>`, "Save changes")
	requireText(t, body, false, "<b>prod", `name="host"`, `name="port"`, `name="database_name"`)

	// A conflict shows the user's input but resubmits against the current revision.
	page.Form.Name, page.Form.ExpectedRevision = "mine", "3"
	page.Conflict, page.Probe = true, nil
	body = renderAdmin(t, "admin_database", page)
	requireText(t, body, true, `value="mine"`, `name="expected_revision" value="7"`, "Someone changed this connection", "Save over the current version")

	// A working probe reports version and latency.
	page.Conflict, page.Probe = false, &ProbeResult{OK: true, ServerVersion: 170011, Latency: 12 * time.Millisecond}
	requireText(t, renderAdmin(t, "admin_database", page), true, "Connection works", "170011", "12ms")
}

func TestAdminDatabasesPage(t *testing.T) {
	layout := httpx.Layout{Title: "Connections", Active: "connections", CSRF: "tok", User: &httpx.LayoutUser{Username: "root", IsAdmin: true}}
	page := AdminDatabasesPage{Layout: layout,
		Databases: []Target{{ID: uuid.New(), Name: "prod", Host: "db.example", Port: 5432, DatabaseName: "orders", Username: "ro", SSLMode: "verify-full", IsActive: false}},
		Pager:     Pager{Next: "/admin/databases?limit=50&offset=50"},
		Form:      ConnectionForm{Name: `"><x>`, Port: "5432", SSLMode: "disable"},
		Error:     "Check the form fields.", Code: "VALIDATION_FAILED"}

	body := renderAdmin(t, "admin_databases", page)
	requireText(t, body, true, "db.example:5432", "Inactive", `rel="next"`, "Check the form fields.", "VALIDATION_FAILED",
		`action="/admin/databases"`, `id="password" name="password" type="password" required autocomplete="new-password" value=""`,
		`<option value="disable" selected>`, `name="host"`, `name="database_name"`, `name="ssl_mode"`)
	requireText(t, body, false, `"><x>`)

	page.Databases, page.Pager, page.Form, page.Error = nil, Pager{}, ConnectionForm{SSLMode: "verify-full"}, ""
	requireText(t, renderAdmin(t, "admin_databases", page), true, "No connections yet", `<option value="verify-full" selected>`)
}
