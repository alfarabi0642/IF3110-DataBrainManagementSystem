package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func TestAuthPageAndLogoutDatabase(t *testing.T) {
	service, ctx := sessionFixture(t)
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(service, renderer)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	handler.LoginPageHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil).WithContext(ctx))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `action="/login"`) {
		t.Fatalf("anonymous login page = %d", w.Code)
	}
	if cookies := w.Result().Cookies(); len(cookies) != 1 || cookies[0].Name != SessionCookieName || cookies[0].Value == "" {
		t.Fatal("anonymous login page did not issue one usable family cookie")
	}

	userID := insertSessionUser(t, service, ctx, false)
	state, token := authenticatedFixture(t, service, ctx, userID)
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/login", nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	handler.LoginPageHandler().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/databases" {
		t.Fatalf("authenticated login page = %d %q", w.Code, w.Header().Get("Location"))
	}

	if _, err := service.pool.Exec(ctx, "UPDATE databrain.sessions SET reauth_required=true WHERE id=$1", state.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/login", nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	handler.LoginPageHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Please sign in again") {
		t.Fatalf("reauth login page = %d", w.Code)
	}

	form := url.Values{"_csrf": {state.CSRF}}
	r = httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(form.Encode())).WithContext(ctx)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", service.appOrigin)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	w = httptest.NewRecorder()
	handler.LogoutHandler().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("logout = %d %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookieName || cookies[0].MaxAge != -1 {
		t.Fatalf("logout cookie = %v", cookies)
	}
	_, _, err = service.Validate(ctx, token, true)
	requireProblem(t, err, problem.SessionInvalid)
}
