package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func TestLoginDatabase(t *testing.T) {
	service, ctx := sessionFixture(t)
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(service, renderer)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	password := "Fixture-login-password"
	hash, err := HashPassword(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.pool.Exec(ctx, `INSERT INTO databrain.users(username,display_name,password_hash)
	    VALUES ('login_user','Login User',$1)`, hash); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil).WithContext(ctx))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("login page did not issue one anonymous cookie")
	}
	oldToken := cookies[0].Value
	oldState, _, err := service.Validate(ctx, oldToken, true)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"_csrf": {oldState.CSRF}, "username": {"  LOGIN_USER  "}, "password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode())).WithContext(ctx)
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", service.appOrigin)
	r.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/databases" {
		t.Fatalf("login = %d %q body=%q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	newCookies := w.Result().Cookies()
	if len(newCookies) != 1 || newCookies[0].Value == "" || newCookies[0].Value == oldToken {
		t.Fatal("login did not replace the anonymous cookie")
	}
	state, _, err := service.Validate(ctx, newCookies[0].Value, false)
	if err != nil || state.Kind != AuthenticatedSession || state.Principal.UserID == [16]byte{} {
		t.Fatalf("new login family = %+v error=%v", state, err)
	}
	if _, _, err := service.Validate(ctx, oldToken, true); !problemIs(err, problem.SessionInvalid) {
		t.Fatal("old anonymous family remained valid after login")
	}
}

func TestLoginRateLimitDatabase(t *testing.T) {
	service, ctx := sessionFixture(t)
	for range usernameLimit {
		if err := service.checkLoginRateLimit(ctx, "limited_user", "192.0.2.20:4321"); err != nil {
			t.Fatal(err)
		}
	}
	var limited *RateLimitError
	if err := service.checkLoginRateLimit(ctx, "limited_user", "192.0.2.20:4321"); !errors.As(err, &limited) || limited.RetryAfter < 1 || limited.RetryAfter > 900 {
		t.Fatalf("limit error=%v retry=%+v", err, limited)
	}
	var rows, usernameAttempts, clientAttempts int
	if err := service.pool.QueryRow(ctx, `SELECT count(*),
	    coalesce(max(attempts) FILTER (WHERE key_type='username'),0),
	    coalesce(max(attempts) FILTER (WHERE key_type='client'),0)
	    FROM databrain.login_windows`).Scan(&rows, &usernameAttempts, &clientAttempts); err != nil ||
		rows != 2 || usernameAttempts != usernameLimit || clientAttempts != usernameLimit {
		t.Fatalf("buckets=%d username=%d client=%d error=%v", rows, usernameAttempts, clientAttempts, err)
	}
}

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
	mux := http.NewServeMux()
	handler.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil).WithContext(ctx))
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
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/databases" {
		t.Fatalf("authenticated login page = %d %q", w.Code, w.Header().Get("Location"))
	}

	if _, err := service.pool.Exec(ctx, "UPDATE databrain.sessions SET reauth_required=true WHERE id=$1", state.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/login", nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Please sign in again") {
		t.Fatalf("reauth login page = %d", w.Code)
	}

	form := url.Values{"_csrf": {state.CSRF}}
	r = httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(form.Encode())).WithContext(ctx)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", service.appOrigin)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
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
