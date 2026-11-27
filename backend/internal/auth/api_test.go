package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

func TestSessionAPIDatabase(t *testing.T) {
	service, ctx := sessionFixture(t)
	userID := insertSessionUser(t, service, ctx, true)
	state, token, err := service.create(ctx, AuthenticatedSession, userID, 1)
	if err != nil {
		t.Fatal(err)
	}
	middleware, _ := NewMiddleware(service)
	mux := http.NewServeMux()
	middleware.RegisterSessionAPI(mux)
	mux.Handle("GET /api/protected", middleware.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, r, 200, struct{}{}, nil)
	})))
	request := func(path, cookie, origin, site string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Sec-Fetch-Site", site)
		w := httptest.NewRecorder()
		httpx.APIHandler(mux).ServeHTTP(w, r)
		return w
	}
	for _, test := range []struct {
		cookie, state string
	}{
		{"", "anonymous"}, {token, "authenticated"},
	} {
		w := request("/api/session", test.cookie, service.appOrigin, "same-origin")
		var result struct {
			Data sessionDTO `json:"data"`
		}
		if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 200 || result.Data.State != test.state ||
			len(result.Data.CSRFToken) != 43 || !strings.HasSuffix(result.Data.AbsoluteExpiresAt, "Z") {
			t.Fatal("invalid session bootstrap projection")
		}
		for _, forbidden := range []string{token, state.ID.String(), "security_version", "password", "sealed"} {
			if strings.Contains(w.Body.String(), forbidden) {
				t.Fatal("internal session data leaked")
			}
		}
		if test.state == "anonymous" && (result.Data.User != nil || len(w.Result().Cookies()) != 1) {
			t.Fatal("anonymous bootstrap did not issue a cookie")
		}
		if test.state == "authenticated" && (result.Data.User == nil || result.Data.User.ID != userID.String()) {
			t.Fatal("authenticated identity missing")
		}
	}
	for _, test := range []struct {
		path, cookie, origin, site string
		status                     int
	}{
		{"/api/session?unknown=1", token, "", "", 400},
		{"/api/session", token, "http://wrong.example", "same-origin", 403},
		{"/api/session", token, "", "cross-site", 403},
		{"/api/protected", "", "", "", 401},
	} {
		w := request(test.path, test.cookie, test.origin, test.site)
		if w.Code != test.status || !json.Valid(w.Body.Bytes()) || w.Header().Get("Location") != "" {
			t.Fatalf("API denial = %d", w.Code)
		}
	}
	if _, err := service.pool.Exec(ctx, "UPDATE databrain.sessions SET reauth_required=true WHERE id=$1", state.ID); err != nil {
		t.Fatal(err)
	}
	w := request("/api/session", token, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"reauth_required"`) {
		t.Fatal("reauth bootstrap state missing")
	}
	w = request("/api/protected", token, "", "")
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"code":"REAUTH_REQUIRED"`) {
		t.Fatal("reauth session reached protected handler")
	}
	if _, err := service.pool.Exec(ctx, "UPDATE databrain.sessions SET revoked_at=clock_timestamp() WHERE id=$1", state.ID); err != nil {
		t.Fatal(err)
	}
	w = request("/api/session", token, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"anonymous"`) || len(w.Result().Cookies()) != 1 {
		t.Fatal("revoked family did not receive anonymous bootstrap")
	}
}
