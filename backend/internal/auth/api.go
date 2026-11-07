package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/jackc/pgx/v5"
)

type sessionUserDTO struct {
	ID              string `json:"id"`
	Username        string `json:"username"`
	DisplayName     string `json:"display_name"`
	IsPlatformAdmin bool   `json:"is_platform_admin"`
}

type sessionDTO struct {
	State             string          `json:"state"`
	User              *sessionUserDTO `json:"user"`
	CSRFToken         string          `json:"csrf_token"`
	AbsoluteExpiresAt string          `json:"absolute_expires_at"`
}

func (m *Middleware) RegisterSessionAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/session", m.sessionAPI)
	mux.Handle("POST /api/login", httpx.LimitBody(httpx.LoginBodyLimit, m.RequireCSRF(http.HandlerFunc(m.loginAPI))))
	mux.Handle("POST /api/logout", httpx.LimitBody(httpx.LoginBodyLimit, m.RequireCSRF(http.HandlerFunc(m.logoutAPI))))
}

func (m *Middleware) loginAPI(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(w, r, httpx.LoginBodyLimit, &input, []string{"username", "password"}, nil); err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	state, _ := SessionFromContext(r.Context())
	if state.Kind == AuthenticatedSession && !state.ReauthRequired {
		httpx.WriteJSONError(w, r, ErrAlreadyAuthenticated)
		return
	}
	username := NormalizeLoginUsername(input.Username)
	if err := m.service.checkLoginRateLimit(r.Context(), username, r.RemoteAddr, strings.Join(r.Header.Values("X-Forwarded-For"), ",")); err != nil {
		var limited *RateLimitError
		if errors.As(err, &limited) {
			w.Header().Set("Retry-After", strconv.Itoa(limited.RetryAfter))
			err = sessionFailure(problem.LoginRateLimited)
		}
		httpx.WriteJSONError(w, r, err)
		return
	}
	newState, token, err := m.service.Login(r.Context(), state, username, input.Password)
	if errors.Is(err, ErrInvalidCredentials) {
		err = sessionFailure(problem.InvalidCredentials)
	}
	if err != nil {
		if problemIs(err, problem.SessionInvalid) {
			http.SetCookie(w, m.service.ClearCookie())
		}
		httpx.WriteJSONError(w, r, err)
		return
	}
	http.SetCookie(w, m.service.Cookie(token, newState.AbsoluteExpiresAt))
	httpx.WriteJSON(w, r, http.StatusOK, struct{}{}, nil)
}

func (m *Middleware) logoutAPI(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if err := httpx.DecodeJSON(w, r, httpx.LoginBodyLimit, &input, nil, nil); err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	state, _ := SessionFromContext(r.Context())
	if err := m.service.Logout(r.Context(), state); err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	http.SetCookie(w, m.service.ClearCookie())
	httpx.WriteJSON(w, r, http.StatusOK, struct{}{}, nil)
}

func (m *Middleware) sessionAPI(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.ParseQuery(r); err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	state, err := m.LoginSession(w, r)
	if err != nil && !errors.Is(err, ErrAlreadyAuthenticated) {
		httpx.WriteJSONError(w, r, err)
		return
	}
	data := sessionDTO{State: string(state.Kind), CSRFToken: state.CSRF,
		AbsoluteExpiresAt: state.AbsoluteExpiresAt.UTC().Format(time.RFC3339Nano)}
	if state.Kind == AuthenticatedSession {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		lease, err := platformdb.Acquire(ctx, m.service.pool)
		if err != nil {
			httpx.WriteJSONError(w, r, sessionFailure(problem.PlatformUnavailable))
			return
		}
		defer lease.Release()
		user := sessionUserDTO{ID: state.Principal.UserID.String()}
		err = lease.QueryRow(ctx, `SELECT username,display_name,is_platform_admin FROM databrain.users
		    WHERE id=$1 AND is_active AND security_version=$2`, state.Principal.UserID, state.Principal.SecurityVersion).
			Scan(&user.Username, &user.DisplayName, &user.IsPlatformAdmin)
		if err != nil {
			code := problem.PlatformUnavailable
			if errors.Is(err, pgx.ErrNoRows) {
				code = problem.SessionInvalid
			}
			httpx.WriteJSONError(w, r, sessionFailure(code))
			return
		}
		data.User = &user
		if state.ReauthRequired {
			data.State = "reauth_required"
		}
	}
	httpx.WriteJSON(w, r, http.StatusOK, data, nil)
}
