package targets

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

func TestConnectionAPI(t *testing.T) {
	f := fixture(t)
	service, err := auth.NewService(f.p.pool, f.p.secrets, auth.Policy{AppEnv: "test", AppOrigin: "http://localhost:8080"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := auth.NewMiddleware(service)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := access.NewAuthorizer(f.p.pool)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.p.RegisterAdminAPI(mux, middleware, authorizer)
	f.p.RegisterListAPI(mux, middleware)
	if _, err := f.p.pool.Exec(t.Context(), `UPDATE databrain.users SET is_platform_admin=true WHERE id=$1`, f.actor); err != nil {
		t.Fatal(err)
	}
	session := seedHandlerSession(t, f, service, f.actor)
	request := func(method, path, body string, want int, code string) map[string]json.RawMessage {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.token})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:8080")
		r.Header.Set("X-CSRF-Token", session.csrf)
		w := httptest.NewRecorder()
		httpx.APIHandler(mux).ServeHTTP(w, r)
		if w.Code != want || code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		for _, secret := range []string{f.input.Password, session.token, "password_sealed", "password_hash", "SQLSTATE", "<html"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("API disclosed private data")
			}
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	input := func(in CreateInput) string {
		return fmt.Sprintf(`{"name":%q,"host":%q,"port":%d,"database_name":%q,"username":%q,"password":%q,"ssl_mode":%q}`, in.Name, in.Host, in.Port, in.DatabaseName, in.Username, in.Password, in.SSLMode)
	}
	result := request("POST", "/api/admin/databases", input(f.input), 201, "")
	var target connectionDTO
	if err := json.Unmarshal(result["data"], &target); err != nil || target.Revision != 1 {
		t.Fatal("invalid connection DTO")
	}
	base := "/api/admin/databases/" + target.ID
	request("GET", base, "", 200, "")
	request("GET", "/api/admin/databases?limit=1", "", 200, "")
	request("GET", "/api/admin/databases?actor=x", "", 400, "INVALID_REQUEST")
	request("POST", "/api/admin/databases", input(f.input), 409, "TARGET_CONFLICT")
	bad := f.second
	bad.Password = "invalid-secret-password"
	request("POST", "/api/admin/databases", input(bad), 422, "TARGET_CONNECTION_FAILED")
	request("POST", base, `{"expected_revision":"1","name":"Renamed","username":`+fmt.Sprintf("%q", f.input.Username)+`,"ssl_mode":"disable","is_active":true}`, 200, "")
	request("POST", base, `{"expected_revision":"1","name":"Stale","username":`+fmt.Sprintf("%q", f.input.Username)+`,"ssl_mode":"disable","is_active":true}`, 409, "REVISION_CONFLICT")
	request("POST", base, `{"expected_revision":"2","name":"x","host":"another","username":"x","ssl_mode":"disable","is_active":true}`, 400, "INVALID_REQUEST")
	request("POST", base+"/test", `{}`, 200, "")
	// Platform admin tanpa grant tidak memperoleh akses target.
	list := request("GET", "/api/databases", "", 200, "")
	if string(list["data"]) != "[]" {
		t.Fatal("admin inherited target access")
	}
	for _, header := range []string{"Origin", "X-CSRF-Token"} {
		r := httptest.NewRequest("POST", base+"/test", strings.NewReader(`{}`))
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session.token})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:8080")
		r.Header.Set("X-CSRF-Token", session.csrf)
		r.Header.Set(header, "invalid")
		w := httptest.NewRecorder()
		httpx.APIHandler(mux).ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("connection API accepted invalid CSRF/Origin")
		}
	}
	if _, err := f.owner.Exec(t.Context(), "ALTER ROLE "+`"`+f.role+`"`+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	probe := request("POST", base+"/test", `{}`, 200, "")
	if !strings.Contains(string(probe["data"]), `"ok":false`) {
		t.Fatal("failed probe reported success")
	}
	if _, err := f.p.pool.Exec(t.Context(), `UPDATE databrain.users SET is_platform_admin=false WHERE id=$1`, f.actor); err != nil {
		t.Fatal(err)
	}
	request("GET", base, "", 403, "ADMIN_REQUIRED")
	request("POST", base+"/test", `{}`, 403, "ADMIN_REQUIRED")
}
