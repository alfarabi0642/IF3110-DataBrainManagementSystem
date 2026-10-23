package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func csrfRequest(origin, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("_csrf="+token+"&username=user"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestRequireCSRFGatesOriginAndToken(t *testing.T) {
	const origin = "http://localhost:8080"
	middleware := &Middleware{service: &Service{appOrigin: origin}}
	expected := strings.Repeat("A", 43)
	state := Session{CSRF: expected}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.PostForm.Get("username") != "user" {
			t.Error("parsed form was not preserved for the handler")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := middleware.RequireCSRF(next)

	for _, tt := range []struct {
		name, origin, token, fetchSite string
		status                         int
	}{
		{"missing origin", "", expected, "", http.StatusForbidden},
		{"foreign origin", "https://evil.example", expected, "", http.StatusForbidden},
		{"cross site", origin, expected, "cross-site", http.StatusForbidden},
		{"wrong token", origin, strings.Repeat("B", 43), "", http.StatusForbidden},
		{"valid", origin, expected, "same-origin", http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called = false
			r := csrfRequest(tt.origin, tt.token)
			if tt.fetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			r = withSession(r, state, false)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.status || called != (tt.status == http.StatusNoContent) {
				t.Fatalf("response=%d called=%t", w.Code, called)
			}
		})
	}
}

func TestRequireSessionRedirectsMissingCookie(t *testing.T) {
	middleware := &Middleware{service: &Service{cookieName: SessionCookieName}}
	called := false
	w := httptest.NewRecorder()
	middleware.RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(
		w, httptest.NewRequest(http.MethodGet, "/databases", nil))
	if called || w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("missing session response=%d location=%q called=%t", w.Code, w.Header().Get("Location"), called)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookieName || cookies[0].MaxAge != -1 {
		t.Fatalf("missing session cookie cleanup=%v", cookies)
	}
}

func TestLoginSessionRejectsCrossSiteBeforeCreatingSession(t *testing.T) {
	middleware := &Middleware{service: &Service{appOrigin: "http://localhost:8080"}}
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	_, err := middleware.LoginSession(httptest.NewRecorder(), r)
	requireProblem(t, err, problem.OriginRejected)
}
