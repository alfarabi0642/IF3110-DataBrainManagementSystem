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

type pageRenderer interface {
	Render(http.ResponseWriter, *http.Request, int, string, any) error
	RenderError(http.ResponseWriter, *http.Request, httpx.Layout, error)
	HasTemplate(string) bool
}

type loginSessionProvider interface {
	LoginSession(http.ResponseWriter, *http.Request) (Session, error)
}

// Handler exposes individual Brain-owned handlers for Al's composition root.
// POST /login is intentionally absent until the rate limiter contract is fixed.
type Handler struct {
	service       *Service
	middleware    *Middleware
	loginSessions loginSessionProvider
	renderer      pageRenderer
}

func NewHandler(service *Service, renderer *httpx.Renderer) (*Handler, error) {
	middleware, err := NewMiddleware(service)
	if err != nil || !renderer.HasTemplate("login") || !renderer.HasTemplate("error") {
		return nil, sessionFailure(problem.InternalError)
	}
	return &Handler{service: service, middleware: middleware, loginSessions: middleware, renderer: renderer}, nil
}

// LoginPageHandler serves GET /login. Route registration stays in Al's mux.
func (h *Handler) LoginPageHandler() http.Handler {
	return http.HandlerFunc(h.loginPage)
}

// LogoutHandler includes the bounded form and AllowReauth CSRF boundary needed
// by POST /logout. Route registration stays in Al's mux.
func (h *Handler) LogoutHandler() http.Handler {
	return httpx.LimitBody(httpx.LoginBodyLimit, h.middleware.RequireCSRF(http.HandlerFunc(h.logout)))
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.ParseQuery(r); err != nil {
		h.renderer.RenderError(w, r, httpx.Layout{}, err)
		return
	}
	state, err := h.loginSessions.LoginSession(w, r)
	if errors.Is(err, ErrAlreadyAuthenticated) {
		_ = httpx.SeeOther(w, r, "/databases")
		return
	}
	if err != nil {
		h.renderer.RenderError(w, r, httpx.Layout{}, err)
		return
	}
	layout := httpx.Layout{Title: "Log in", CSRF: state.CSRF}
	page := LoginPage{Layout: layout, Reauth: state.Kind == AuthenticatedSession && state.ReauthRequired}
	if err := h.renderer.Render(w, r, http.StatusOK, "login", page); err != nil {
		h.renderer.RenderError(w, r, layout, err)
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	state, ok := SessionFromContext(r.Context())
	if !ok || state.Kind != AuthenticatedSession {
		httpx.WriteError(w, r, sessionFailure(problem.SessionInvalid))
		return
	}
	if err := httpx.ValidateFields(r.PostForm, []string{"_csrf"}, nil); err != nil {
		h.renderer.RenderError(w, r, httpx.Layout{CSRF: state.CSRF}, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, h.service.pool)
	if err != nil {
		h.renderer.RenderError(w, r, httpx.Layout{CSRF: state.CSRF}, sessionFailure(problem.PlatformUnavailable))
		return
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		h.renderer.RenderError(w, r, httpx.Layout{CSRF: state.CSRF}, sessionFailure(problem.PlatformUnavailable))
		return
	}
	defer rollbackAuth(tx)
	if err := h.service.RevokeSession(ctx, tx, state.ID); err != nil {
		h.renderer.RenderError(w, r, httpx.Layout{CSRF: state.CSRF}, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.renderer.RenderError(w, r, httpx.Layout{CSRF: state.CSRF}, sessionFailure(problem.PlatformUnavailable))
		return
	}
	http.SetCookie(w, h.service.ClearCookie())
	_ = httpx.SeeOther(w, r, "/login")
}
