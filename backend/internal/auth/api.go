package auth

import (
	"context"
	"errors"
	"net/http"
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
