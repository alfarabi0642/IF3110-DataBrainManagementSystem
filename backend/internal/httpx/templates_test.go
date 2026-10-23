package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testUser struct {
	DisplayName, Username string
	IsAdmin               bool
}

type testErrorPage struct {
	Title, CSRF              string
	User                     *testUser
	Message, Code, RequestID string
}

// The shipped templates must parse and escape layout and page data.
func TestWebTemplatesRenderErrorPage(t *testing.T) {
	renderer, err := LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	render := func(page testErrorPage) string {
		w := httptest.NewRecorder()
		if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/", nil), 404, "error", page); err != nil {
			t.Fatal(err)
		}
		return w.Body.String()
	}

	hostile := `<script>alert(1)</script>`
	body := render(testErrorPage{Title: "Not found", Message: hostile, Code: "DATABASE_NOT_FOUND", RequestID: "req-1"})
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "req-1") {
		t.Fatalf("error page not escaped: %s", body)
	}
	if strings.Contains(body, `action="/logout"`) || strings.Contains(body, "/databases") {
		t.Fatal("signed-out page shows account navigation")
	}

	body = render(testErrorPage{Title: "Not found", CSRF: "tok", User: &testUser{DisplayName: hostile, Username: "ana"}})
	if strings.Contains(body, "<script>") || !strings.Contains(body, `name="_csrf" value="tok"`) || strings.Contains(body, "/admin/") {
		t.Fatalf("viewer layout wrong: %s", body)
	}
	if body = render(testErrorPage{User: &testUser{IsAdmin: true}}); !strings.Contains(body, `href="/admin/users"`) {
		t.Fatal("admin navigation missing")
	}
}
