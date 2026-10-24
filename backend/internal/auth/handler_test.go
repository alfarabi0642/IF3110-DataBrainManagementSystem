package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

type handlerRenderer struct {
	page   any
	name   string
	status int
	err    error
}

func (r *handlerRenderer) HasTemplate(string) bool { return true }
func (r *handlerRenderer) Render(_ http.ResponseWriter, _ *http.Request, status int, name string, page any) error {
	r.status, r.name, r.page = status, name, page
	return r.err
}
func (r *handlerRenderer) RenderError(w http.ResponseWriter, _ *http.Request, _ httpx.Layout, err error) {
	r.err = err
	w.WriteHeader(http.StatusTeapot)
}

type loginSessionResult struct {
	state Session
	err   error
}

func (s loginSessionResult) LoginSession(http.ResponseWriter, *http.Request) (Session, error) {
	return s.state, s.err
}

func TestLoginPageHandlerStates(t *testing.T) {
	for _, test := range []struct {
		name         string
		state        Session
		err          error
		query        string
		wantStatus   int
		wantLocation string
		wantReauth   bool
	}{
		{"anonymous", Session{Kind: AnonymousSession, CSRF: "csrf"}, nil, "", 0, "", false},
		{"reauth", Session{Kind: AuthenticatedSession, CSRF: "csrf", ReauthRequired: true}, nil, "", 0, "", true},
		{"authenticated", Session{}, ErrAlreadyAuthenticated, "", http.StatusSeeOther, "/databases", false},
		{"unknown query", Session{}, nil, "?return_to=/admin", http.StatusTeapot, "", false},
		{"store failure", Session{}, sessionFailure(problem.PlatformUnavailable), "", http.StatusTeapot, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			renderer := &handlerRenderer{}
			h := &Handler{loginSessions: loginSessionResult{test.state, test.err}, renderer: renderer}
			w := httptest.NewRecorder()
			h.LoginPageHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login"+test.query, nil))
			if test.wantStatus != 0 && w.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d", w.Code, test.wantStatus)
			}
			if w.Header().Get("Location") != test.wantLocation {
				t.Fatalf("location=%q", w.Header().Get("Location"))
			}
			if test.wantStatus == 0 {
				page, ok := renderer.page.(LoginPage)
				if !ok || renderer.name != "login" || renderer.status != http.StatusOK || page.Reauth != test.wantReauth || page.CSRF != "csrf" {
					t.Fatalf("page=%+v renderer=%+v", page, renderer)
				}
			}
		})
	}
}

func TestAuthHandlerRejectsMissingDependenciesAndAnonymousLogout(t *testing.T) {
	if handler, err := NewHandler(nil, nil); handler != nil || err == nil {
		t.Fatal("missing handler dependencies accepted")
	}
	renderer := &handlerRenderer{}
	h := &Handler{renderer: renderer}
	w := httptest.NewRecorder()
	h.logout(w, httptest.NewRequest(http.MethodPost, "/logout", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" || renderer.err != nil {
		t.Fatalf("anonymous logout = %d %q %v", w.Code, w.Header().Get("Location"), renderer.err)
	}
}
