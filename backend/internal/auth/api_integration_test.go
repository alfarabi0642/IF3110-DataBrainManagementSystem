package auth

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func TestAuthJSONDatabase(t *testing.T) {
	service, ctx := sessionFixture(t)
	password := "Fixture-login-password"
	user, err := BootstrapAdmin(ctx, service.pool, "api_user", "API User", password)
	if err != nil {
		t.Fatal(err)
	}
	middleware, _ := NewMiddleware(service)
	mux := http.NewServeMux()
	middleware.RegisterSessionAPI(mux)
	mux.Handle("GET /api/protected", middleware.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, r, 200, struct{}{}, nil)
	})))
	handler := httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), httpx.APIHandler(mux))
	request := func(method, path, token, csrf, body string, status int, code string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		r.RemoteAddr = "192.0.2.10:1234"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", service.appOrigin)
		r.Header.Set("X-CSRF-Token", csrf)
		if token != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var envelope map[string]json.RawMessage
		if w.Code != status || json.Unmarshal(w.Body.Bytes(), &envelope) != nil ||
			w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" ||
			!strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s %s = %d %s", method, path, w.Code, w.Body.String())
		}
		var requestID string
		_ = json.Unmarshal(envelope["request_id"], &requestID)
		if requestID == "" || requestID != w.Header().Get("X-Request-ID") {
			t.Fatal("request ID mismatch")
		}
		if code != "" {
			if envelope["data"] != nil || !strings.Contains(string(envelope["error"]), `"code":"`+code+`"`) {
				t.Fatal("error envelope mismatch")
			}
		} else if envelope["error"] != nil || envelope["data"] == nil {
			t.Fatal("success envelope mismatch")
		}
		for _, secret := range []string{password, "security_version", "session_id", "canonical_token"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("secret leaked")
			}
		}
		return w
	}
	bootstrap := func(token string) (sessionDTO, string) {
		t.Helper()
		w := request("GET", "/api/session", token, "", "", 200, "")
		var result struct {
			Data sessionDTO `json:"data"`
		}
		if json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal("invalid bootstrap")
		}
		if cookies := w.Result().Cookies(); len(cookies) > 0 {
			token = cookies[len(cookies)-1].Value
		}
		return result.Data, token
	}
	loginBody := `{"username":"  API_USER  ","password":"` + password + `"}`
	anonymous, old := bootstrap("")
	request("POST", "/api/login", "", anonymous.CSRFToken, loginBody, 401, "SESSION_INVALID")
	for _, body := range []string{`{}`, `{"username":"api_user","password":null}`, `{"username":"api_user","password":"x","_csrf":"x"}`, `{"username":"a","username":"b","password":"x"}`, `{"username":1,"password":"x"}`} {
		request("POST", "/api/login", old, anonymous.CSRFToken, body, 400, "INVALID_REQUEST")
	}
	request("POST", "/api/login", old, anonymous.CSRFToken, strings.Repeat(" ", int(httpx.LoginBodyLimit)+1), 413, "BODY_TOO_LARGE")
	request("POST", "/api/login", old, anonymous.CSRFToken, `{"username":"api_user","password":"wrong-password"}`, 401, "INVALID_CREDENTIALS")
	w := request("POST", "/api/login", old, anonymous.CSRFToken, loginBody, 200, "")
	if strings.TrimSpace(w.Body.String()) == "" || !strings.Contains(w.Body.String(), `"data":{}`) {
		t.Fatal("mutation result must be empty DTO")
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Value == old || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatal("cookie policy changed")
	}
	authToken := cookie.Value
	_, _, err = service.Validate(ctx, old, true)
	requireProblem(t, err, problem.SessionInvalid)
	auth, _ := bootstrap(authToken)
	if auth.State != "authenticated" || auth.User.ID != user.String() || auth.CSRFToken == anonymous.CSRFToken {
		t.Fatal("login bootstrap mismatch")
	}
	state, _, _ := service.Validate(ctx, authToken, true)
	request("POST", "/api/login", authToken, auth.CSRFToken, loginBody, 409, "ALREADY_AUTHENTICATED")
	request("POST", "/api/logout", authToken, anonymous.CSRFToken, `{}`, 403, "CSRF_INVALID")

	execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET last_rotated_at=clock_timestamp()-interval '16 minutes' WHERE id=$1", state.ID)
	rotated, canonical := bootstrap(authToken)
	if canonical == authToken || rotated.CSRFToken != auth.CSRFToken || rotated.AbsoluteExpiresAt != auth.AbsoluteExpiresAt {
		t.Fatal("rotation changed family or deadline")
	}
	_, repaired := bootstrap(authToken)
	if repaired != canonical {
		t.Fatal("alias did not repair canonical cookie")
	}
	execLifecycle(t, service.pool, ctx, "UPDATE databrain.session_tokens SET valid_until=clock_timestamp()-interval '1 second' WHERE session_id=$1 AND token_hash<>$2", state.ID, tokenHashMust(t, canonical))
	request("GET", "/api/protected", authToken, "", "", 401, "SESSION_INVALID")
	request("GET", "/api/protected", canonical, "", "", 200, "")

	tx := lifecycleTx(t, service, ctx)
	if err := service.ReauthenticateUsers(ctx, tx, []uuid.UUID{user}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	request("GET", "/api/protected", canonical, "", "", 401, "REAUTH_REQUIRED")
	reauth, reauthToken := bootstrap(canonical)
	if reauth.State != "reauth_required" || reauth.AbsoluteExpiresAt != auth.AbsoluteExpiresAt {
		t.Fatal("reauth bootstrap mismatch")
	}
	w = request("POST", "/api/login", reauthToken, reauth.CSRFToken, loginBody, 200, "")
	newToken := w.Result().Cookies()[0].Value
	_, _, err = service.Validate(ctx, reauthToken, true)
	requireProblem(t, err, problem.SessionInvalid)
	newAuth, _ := bootstrap(newToken)
	if newAuth.CSRFToken == auth.CSRFToken || newAuth.State != "authenticated" {
		t.Fatal("reauth did not replace family")
	}
	w = request("POST", "/api/logout", newToken, newAuth.CSRFToken, `{}`, 200, "")
	if w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout did not clear cookie")
	}
	request("GET", "/api/protected", newToken, "", "", 401, "SESSION_INVALID")
	loggedOut, _ := bootstrap(newToken)
	if loggedOut.State != "anonymous" {
		t.Fatal("logout revived family")
	}

	for range usernameLimit {
		if err := service.checkLoginRateLimit(ctx, "limited_user", "192.0.2.10:1234", ""); err != nil {
			t.Fatal(err)
		}
	}
	w = request("POST", "/api/login", old, anonymous.CSRFToken, loginBody, 401, "SESSION_INVALID")
	limited, limitedToken := bootstrap("")
	w = request("POST", "/api/login", limitedToken, limited.CSRFToken, `{"username":" LIMITED_USER ","password":"wrong-password"}`, 429, "LOGIN_RATE_LIMITED")
	if retry, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || retry < 1 || retry > 900 {
		t.Fatal("invalid Retry-After")
	}
}

func tokenHashMust(t *testing.T, token string) []byte {
	t.Helper()
	raw, err := decodeToken(token)
	if err != nil {
		t.Fatal(err)
	}
	hash := tokenHash(raw)
	return hash[:]
}

func TestAuthJSONExpiryAndReauthLogoutDatabase(t *testing.T) {
	service, ctx := sessionFixture(t)
	user := insertSessionUser(t, service, ctx, false)
	middleware, _ := NewMiddleware(service)
	mux := http.NewServeMux()
	middleware.RegisterSessionAPI(mux)
	for _, expiry := range []string{"idle", "absolute", "anonymous", "reauth logout"} {
		t.Run(expiry, func(t *testing.T) {
			state, token := authenticatedFixture(t, service, ctx, user)
			switch expiry {
			case "idle":
				execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET last_seen_at=clock_timestamp()-interval '31 minutes' WHERE id=$1", state.ID)
			case "absolute":
				execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET created_at=clock_timestamp()-interval '9 hours',absolute_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", state.ID)
			case "anonymous":
				var err error
				state, token, err = service.CreateAnonymous(ctx)
				if err != nil {
					t.Fatal(err)
				}
				execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET created_at=clock_timestamp()-interval '11 minutes',absolute_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", state.ID)
			case "reauth logout":
				execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET reauth_required=true WHERE id=$1", state.ID)
			}
			r := httptest.NewRequest("POST", "/api/logout", strings.NewReader(`{}`)).WithContext(ctx)
			r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
			r.Header.Set("Origin", service.appOrigin)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CSRF-Token", state.CSRF)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if expiry == "reauth logout" {
				if w.Code != 200 {
					t.Fatalf("reauth logout = %d", w.Code)
				}
				_, _, err := service.Validate(ctx, token, true)
				requireProblem(t, err, problem.SessionInvalid)
			} else if w.Code != 401 || !strings.Contains(w.Body.String(), `"code":"SESSION_INVALID"`) {
				t.Fatalf("expired logout = %d", w.Code)
			}
			w = httptest.NewRecorder()
			r = httptest.NewRequest("GET", "/api/session", nil).WithContext(ctx)
			r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
			mux.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"anonymous"`) {
				t.Fatal("expired bootstrap did not recover")
			}
		})
	}
}
