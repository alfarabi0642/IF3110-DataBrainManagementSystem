package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

// The shipped templates must parse, escape layout data, and render errors.
func TestWebTemplatesRenderErrorPage(t *testing.T) {
	renderer, err := LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	render := func(layout Layout, err error) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		renderer.RenderError(w, httptest.NewRequest(http.MethodGet, "/", nil), layout, err)
		return w
	}
	notFound := &problem.Error{Code: problem.DatabaseNotFound, Message: "<script>alert(1)</script>"}

	w := render(Layout{}, notFound)
	body := w.Body.String()
	if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "DATABASE_NOT_FOUND") || strings.Contains(body, "<script>") {
		t.Fatalf("signed-out error page wrong: %d %s", w.Code, body)
	}
	if strings.Contains(body, `action="/logout"`) || strings.Contains(body, "/databases") {
		t.Fatal("signed-out page shows account navigation")
	}

	hostile := `<script>alert(1)</script>`
	body = render(Layout{CSRF: "tok", User: &LayoutUser{DisplayName: hostile, Username: "ana"}}, notFound).Body.String()
	if strings.Contains(body, "<script>") || !strings.Contains(body, `name="_csrf" value="tok"`) || strings.Contains(body, "/admin/") {
		t.Fatalf("viewer layout wrong: %s", body)
	}
	if body = render(Layout{User: &LayoutUser{IsAdmin: true}}, notFound).Body.String(); !strings.Contains(body, `href="/admin/users"`) {
		t.Fatal("admin navigation missing")
	}
}

func TestWebTemplatesDatabaseNav(t *testing.T) {
	renderer, err := LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	render := func(layout Layout) string {
		w := httptest.NewRecorder()
		renderer.RenderError(w, httptest.NewRequest(http.MethodGet, "/", nil), layout, &problem.Error{Code: problem.DatabaseNotFound})
		return w.Body.String()
	}
	user := &LayoutUser{Username: "ana"}
	id := "3f2b8e64-0c1a-4c57-9b0e-5d8f1a2b3c4d"

	body := render(Layout{User: user, Active: "databases"})
	if strings.Contains(body, `aria-label="Database"`) || !strings.Contains(body, `href="/databases" aria-current="page"`) {
		t.Fatalf("main nav wrong without a database: %s", body)
	}

	body = render(Layout{User: user, Active: "history", Database: &LayoutDatabase{ID: id, Name: `<b>prod</b>`}})
	if strings.Contains(body, "<b>prod") || strings.Contains(body, "/drafts") ||
		!strings.Contains(body, `href="/databases/`+id+`/explorer"`) ||
		!strings.Contains(body, `/migrations" aria-current="page"`) {
		t.Fatalf("view-only database nav wrong: %s", body)
	}

	body = render(Layout{User: user, Active: "drafts", Database: &LayoutDatabase{ID: id, Name: "prod", CanEdit: true}})
	if !strings.Contains(body, `/drafts" aria-current="page"`) {
		t.Fatalf("editor nav missing Drafts: %s", body)
	}
}

func TestErrorPagesRendersRouterFailures(t *testing.T) {
	renderer, err := LoadTemplates("../../web/templates")
	if err != nil {
		t.Fatal(err)
	}
	mux := NewRouter(func(context.Context) bool { return true })
	handler := ErrorPages(renderer, mux)
	for _, test := range []struct {
		method, path string
		status       int
		code, allow  string
	}{
		{http.MethodGet, "/missing", 404, "ROUTE_NOT_FOUND", ""},
		{http.MethodPost, "/healthz", 405, "METHOD_NOT_ALLOWED", "GET, HEAD"},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status || w.Header().Get("Allow") != test.allow ||
			!strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), test.code) {
			t.Fatalf("%s %s = %d %v %q", test.method, test.path, w.Code, w.Header(), w.Body.String())
		}
	}
}

func TestRenderErrorFallsBackToPlainText(t *testing.T) {
	w := httptest.NewRecorder()
	(*Renderer)(nil).RenderError(w, httptest.NewRequest(http.MethodGet, "/", nil), Layout{}, &problem.Error{Code: problem.InternalError})
	if w.Code != 500 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("no plain-text fallback: %d %v", w.Code, w.Header())
	}
}
