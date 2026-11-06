package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/google/uuid"
)

func TestAdminAPI(t *testing.T) {
	s, actor := adminFixture(t)
	sessions := s.sessions.(*auth.Service)
	middleware, err := auth.NewMiddleware(sessions)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterAPI(mux, middleware)
	middleware.RegisterSessionAPI(mux)
	state, token, err := sessions.CreateAnonymous(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, actor.UserID); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, want int) map[string]json.RawMessage {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:8080")
		r.Header.Set("X-CSRF-Token", state.CSRF)
		w := httptest.NewRecorder()
		httpx.APIHandler(mux).ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		for _, secret := range []string{token, "Api-user-password", "password_hash", "security_version", "<html"} {
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
	createdID := func(result map[string]json.RawMessage) string {
		t.Helper()
		var data struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(result["data"], &data); err != nil || data.ID == "" {
			t.Fatal("missing created ID")
		}
		return data.ID
	}
	request("GET", "/api/admin/users?limit=1", "", 200)
	for _, body := range []string{`{}`, `{"name":"x","name":"y"}`, `{"name":null}`, `{"name":"x","actor":"client"}`} {
		request("POST", "/api/admin/groups", body, 400)
	}
	request("POST", "/api/admin/groups", `{"name":" "}`, 422)
	group := createdID(request("POST", "/api/admin/groups", `{"name":"API group"}`, 201))
	request("POST", "/api/admin/groups", `{"name":"API group"}`, 409)
	user := createdID(request("POST", "/api/admin/users", `{"username":"api_user","display_name":"API user","password":"Api-user-password","is_platform_admin":false}`, 201))
	base := "/api/admin/users/" + user
	request("GET", base, "", 200)
	request("POST", base, `{"expected_revision":1,"display_name":"x","is_active":true,"is_platform_admin":false}`, 400)
	request("POST", base, `{"expected_revision":"1","display_name":"x","is_active":"true","is_platform_admin":false}`, 400)
	request("POST", base, `{"expected_revision":"1","display_name":"Renamed","is_active":true,"is_platform_admin":false}`, 200)
	request("POST", base, `{"expected_revision":"1","display_name":"Stale","is_active":true,"is_platform_admin":false}`, 409)
	request("POST", base+"/password", `{"expected_revision":"2","password":"Api-user-password"}`, 200)
	groupPath := "/api/admin/groups/" + group
	target := uuid.NewString()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO databrain.database_targets
	    (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
	    VALUES ($1,'API target','localhost',5432,'school','role',decode(repeat('31',30),'hex'),'disable',$2)`, target, actor.UserID); err != nil {
		t.Fatal(err)
	}
	grantBody := fmt.Sprintf(`{"database_id":%q,"access_level":"view_edit"}`, target)
	request("POST", groupPath+"/databases", grantBody, 200)
	request("POST", groupPath+"/databases", fmt.Sprintf(`{"database_id":%q,"access_level":"admin"}`, target), 422)
	request("POST", groupPath+"/members", fmt.Sprintf(`{"user_id":%q}`, user), 200)
	detail := request("GET", groupPath, "", 200)
	if !strings.Contains(string(detail["data"]), `"username":"api_user"`) {
		t.Fatal("membership missing")
	}
	request("POST", groupPath, `{"expected_revision":"1","name":"API group","is_active":false}`, 200)
	request("POST", groupPath, `{"expected_revision":"1","name":"Stale","is_active":true}`, 409)
	request("POST", groupPath+"/databases/"+target+"/remove", `{}`, 200)
	request("POST", groupPath+"/databases", grantBody, 422)
	request("POST", groupPath+"/members/"+user+"/remove", `{}`, 200)
	request("POST", groupPath+"/members", fmt.Sprintf(`{"user_id":%q}`, user), 422)
	request("POST", "/api/admin/users/"+actor.UserID.String(), `{"expected_revision":"1","display_name":"Fixture admin","is_active":true,"is_platform_admin":false}`, 409)
	for _, header := range []string{"Origin", "X-CSRF-Token"} {
		r := httptest.NewRequest("POST", "/api/admin/groups", strings.NewReader(`{"name":"Denied"}`))
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:8080")
		r.Header.Set("X-CSRF-Token", state.CSRF)
		r.Header.Set(header, "invalid")
		w := httptest.NewRecorder()
		httpx.APIHandler(mux).ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("admin API accepted invalid CSRF/Origin")
		}
	}
	// Session non-admin tetap ditolak meskipun memanggil endpoint langsung.
	if _, err := s.pool.Exec(t.Context(), `UPDATE databrain.sessions SET user_id=$2,security_version=(SELECT security_version FROM databrain.users WHERE id=$2) WHERE id=$1`, state.ID, user); err != nil {
		t.Fatal(err)
	}
	request("GET", "/api/admin/users", "", 403)
	request("POST", "/api/admin/groups", `{"name":"Denied"}`, 403)
	if _, err := s.pool.Exec(t.Context(), `UPDATE databrain.sessions SET user_id=$2,security_version=(SELECT security_version FROM databrain.users WHERE id=$2) WHERE id=$1`, state.ID, actor.UserID); err != nil {
		t.Fatal(err)
	}
	request("POST", "/api/admin/users/"+actor.UserID.String()+"/password", `{"expected_revision":"1","password":"Api-user-password"}`, 200)
	request("GET", "/api/admin/users", "", 401)
}
