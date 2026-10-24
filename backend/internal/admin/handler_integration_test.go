package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

func TestAdminHandlersDatabase(t *testing.T) {
	service, actor := adminFixture(t)
	sessions := service.sessions.(*auth.Service)
	state, token, err := sessions.CreateAnonymous(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.pool.Exec(t.Context(), `UPDATE databrain.sessions
	    SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, actor.UserID); err != nil {
		t.Fatal(err)
	}
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(service, sessions, renderer)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var body *strings.Reader
		if form == nil {
			body = strings.NewReader("")
		} else {
			body = strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, path, body)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "http://localhost:8080")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodGet, "/admin/users", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Fixture admin") {
		t.Fatalf("users page = %d body=%q", w.Code, w.Body.String())
	}
	form := url.Values{"_csrf": {state.CSRF}, "name": {"Handler group"}}
	w := request(http.MethodPost, "/admin/groups", form)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/admin/groups/") {
		t.Fatalf("create group = %d location=%q body=%q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if detail := request(http.MethodGet, w.Header().Get("Location"), nil); detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "Handler group") {
		t.Fatalf("group detail = %d body=%q", detail.Code, detail.Body.String())
	}
	invalid := url.Values{"_csrf": {state.CSRF}}
	if w := request(http.MethodPost, "/admin/users", invalid); w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "password=") {
		t.Fatalf("invalid create user = %d body=%q", w.Code, w.Body.String())
	}
}
