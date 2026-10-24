package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

func TestLoginTemplateKeepsAuthenticationDataSafe(t *testing.T) {
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	page := LoginPage{
		Layout:   httpx.Layout{Title: "Log in", CSRF: "csrf-token"},
		Username: `<script>alert("username")</script>`,
		Error:    `<b>Invalid credentials.</b>`,
	}
	w := httptest.NewRecorder()
	if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/login", nil), http.StatusUnauthorized, "login", page); err != nil {
		t.Fatalf("render login: %v", err)
	}
	body := w.Body.String()

	for _, want := range []string{
		`action="/login"`, `name="_csrf" value="csrf-token"`,
		`name="username"`, `name="password" type="password"`,
		`&lt;script&gt;alert`, `&lt;b&gt;Invalid credentials.&lt;/b&gt;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	for _, bad := range []string{"<script>", "<b>Invalid", "must-never-render", `action="/logout"`, `href="/admin/`} {
		if strings.Contains(body, bad) {
			t.Errorf("unsafe login output %q in:\n%s", bad, body)
		}
	}
	if strings.Count(body, `name="_csrf"`) != 1 {
		t.Fatal("login must render exactly one CSRF field")
	}

	page.Reauth = true
	w = httptest.NewRecorder()
	if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/login", nil), http.StatusOK, "login", page); err != nil {
		t.Fatalf("render reauthentication: %v", err)
	}
	body = w.Body.String()
	if !strings.Contains(body, "Please sign in again") || !strings.Contains(body, `action="/logout"`) ||
		strings.Count(body, `name="_csrf"`) != 2 {
		t.Fatalf("reauthentication controls missing or malformed:\n%s", body)
	}
}
