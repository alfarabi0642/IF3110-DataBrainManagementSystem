package httpx

import (
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

func TestRenderErrorFallsBackToPlainText(t *testing.T) {
	w := httptest.NewRecorder()
	(*Renderer)(nil).RenderError(w, httptest.NewRequest(http.MethodGet, "/", nil), Layout{}, &problem.Error{Code: problem.InternalError})
	if w.Code != 500 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("no plain-text fallback: %d %v", w.Code, w.Header())
	}
}
