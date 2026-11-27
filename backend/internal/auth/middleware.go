package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

type sessionContextKey struct{}

type Middleware struct {
	service *Service
}

func NewMiddleware(service *Service) (*Middleware, error) {
	if service == nil {
		return nil, errors.New("auth: session service is required")
	}
	return &Middleware{service: service}, nil
}

func SessionFromContext(ctx context.Context) (Session, bool) {
	state, ok := ctx.Value(sessionContextKey{}).(Session)
	return state, ok
}

func withSession(r *http.Request, state Session, principal bool) *http.Request {
	ctx := context.WithValue(r.Context(), sessionContextKey{}, state)
	if principal {
		ctx = access.WithPrincipal(ctx, state.Principal)
	}
	return r.WithContext(ctx)
}

func problemIs(err error, code problem.Code) bool {
	var value *problem.Error
	return errors.As(err, &value) && value != nil && value.Code == code
}

func (m *Middleware) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := m.service.cookieValue(r)
		if err != nil {
			http.SetCookie(w, m.service.ClearCookie())
			rejectSession(w, r, problem.SessionInvalid)
			return
		}
		state, replacement, err := m.service.Validate(r.Context(), token, false)
		if err != nil {
			switch {
			case errors.Is(err, ErrReauthRequired):
				if replacement != "" {
					http.SetCookie(w, m.service.Cookie(replacement, state.AbsoluteExpiresAt))
				}
				rejectSession(w, r, problem.ReauthRequired)
			case problemIs(err, problem.SessionInvalid):
				http.SetCookie(w, m.service.ClearCookie())
				rejectSession(w, r, problem.SessionInvalid)
			default:
				httpx.WriteError(w, r, err)
			}
			return
		}
		if replacement != "" {
			http.SetCookie(w, m.service.Cookie(replacement, state.AbsoluteExpiresAt))
		}
		if state.Kind != AuthenticatedSession {
			rejectSession(w, r, problem.SessionInvalid)
			return
		}
		next.ServeHTTP(w, withSession(r, state, true))
	})
}

func rejectSession(w http.ResponseWriter, r *http.Request, code problem.Code) {
	if httpx.IsAPI(r) {
		httpx.WriteJSONError(w, r, sessionFailure(code))
		return
	}
	_ = httpx.SeeOther(w, r, "/login")
}

// RequireCSRF memakai family dan pemeriksaan Origin yang sama untuk JSON dan form.
func (m *Middleware) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteError(w, r, sessionFailure(problem.InvalidRequest))
			return
		}
		state, ok := SessionFromContext(r.Context())
		if !ok {
			token, err := m.service.cookieValue(r)
			if err != nil {
				m.rejectCSRFFamily(w, r)
				return
			}
			var replacement string
			state, replacement, err = m.service.Validate(r.Context(), token, true)
			if err != nil {
				if problemIs(err, problem.SessionInvalid) {
					m.rejectCSRFFamily(w, r)
				} else {
					httpx.WriteError(w, r, err)
				}
				return
			}
			if replacement != "" {
				http.SetCookie(w, m.service.Cookie(replacement, state.AbsoluteExpiresAt))
			}
			r = withSession(r, state, false)
		}
		if values := r.Header.Values("Origin"); len(values) != 1 || values[0] != m.service.appOrigin ||
			strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
			httpx.WriteError(w, r, sessionFailure(problem.OriginRejected))
			return
		}
		var submitted string
		if httpx.IsAPI(r) {
			if values := r.Header.Values("X-CSRF-Token"); len(values) == 1 {
				submitted = values[0]
			}
		} else {
			form, err := httpx.ParseForm(w, r, httpx.MaxFormBody)
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			submitted = form.Get("_csrf")
		}
		if !validCSRF(state.CSRF, submitted) {
			httpx.WriteError(w, r, sessionFailure(problem.CSRFInvalid))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Middleware) rejectCSRFFamily(w http.ResponseWriter, r *http.Request) {
	if httpx.IsAPI(r) {
		http.SetCookie(w, m.service.ClearCookie())
		httpx.WriteJSONError(w, r, sessionFailure(problem.SessionInvalid))
		return
	}
	httpx.WriteError(w, r, sessionFailure(problem.CSRFInvalid))
}

// LoginSession memakai family anonymous atau reauth yang ada, atau membuat anonymous baru untuk GET
// /login. Family authenticated biasa diteruskan agar handler dapat mengarahkan ke /databases.
func (m *Middleware) LoginSession(w http.ResponseWriter, r *http.Request) (Session, error) {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return Session{}, sessionFailure(problem.OriginRejected)
	}
	if values := r.Header.Values("Origin"); len(values) > 1 || len(values) == 1 && values[0] != m.service.appOrigin {
		return Session{}, sessionFailure(problem.OriginRejected)
	}
	if token, err := m.service.cookieValue(r); err == nil {
		state, replacement, err := m.service.Validate(r.Context(), token, true)
		if err == nil {
			if replacement != "" {
				http.SetCookie(w, m.service.Cookie(replacement, state.AbsoluteExpiresAt))
			}
			if state.Kind == AuthenticatedSession && !state.ReauthRequired {
				return state, ErrAlreadyAuthenticated
			}
			return state, nil
		}
		if !problemIs(err, problem.SessionInvalid) {
			return Session{}, err
		}
	}
	state, token, err := m.service.CreateAnonymous(r.Context())
	if err != nil {
		return Session{}, err
	}
	http.SetCookie(w, m.service.Cookie(token, state.AbsoluteExpiresAt))
	return state, nil
}
