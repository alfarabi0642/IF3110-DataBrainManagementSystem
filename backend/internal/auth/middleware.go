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
			_ = httpx.SeeOther(w, r, "/login")
			return
		}
		state, replacement, err := m.service.Validate(r.Context(), token, false)
		if err != nil {
			switch {
			case errors.Is(err, ErrReauthRequired):
				if replacement != "" {
					http.SetCookie(w, m.service.Cookie(replacement, state.AbsoluteExpiresAt))
				}
				_ = httpx.SeeOther(w, r, "/login")
			case problemIs(err, problem.SessionInvalid):
				http.SetCookie(w, m.service.ClearCookie())
				_ = httpx.SeeOther(w, r, "/login")
			default:
				httpx.WriteError(w, r, err)
			}
			return
		}
		if replacement != "" {
			http.SetCookie(w, m.service.Cookie(replacement, state.AbsoluteExpiresAt))
		}
		if state.Kind != AuthenticatedSession {
			_ = httpx.SeeOther(w, r, "/login")
			return
		}
		next.ServeHTTP(w, withSession(r, state, true))
	})
}

// RequireCSRF parses the bounded URL-encoded form. Protected POST routes wrap
// this inside RequireSession; login/logout may use it directly.
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
				httpx.WriteError(w, r, sessionFailure(problem.CSRFInvalid))
				return
			}
			var replacement string
			state, replacement, err = m.service.Validate(r.Context(), token, true)
			if err != nil {
				if problemIs(err, problem.SessionInvalid) {
					httpx.WriteError(w, r, sessionFailure(problem.CSRFInvalid))
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
		form, err := httpx.ParseForm(w, r, httpx.MaxFormBody)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if !validCSRF(state.CSRF, form.Get("_csrf")) {
			httpx.WriteError(w, r, sessionFailure(problem.CSRFInvalid))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoginSession reuses an anonymous or reauth family, or creates a fresh
// anonymous family for GET /login. A normal authenticated family is reported
// so the handler can redirect to /databases.
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
