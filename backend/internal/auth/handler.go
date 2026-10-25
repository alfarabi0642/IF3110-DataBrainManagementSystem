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

type pageRenderer interface {
	Render(http.ResponseWriter, *http.Request, int, string, any) error
	RenderError(http.ResponseWriter, *http.Request, httpx.Layout, error)
	HasTemplate(string) bool
}

type loginSessionProvider interface {
	LoginSession(http.ResponseWriter, *http.Request) (Session, error)
}

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

// Register memasang seluruh route auth.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /login", h.LoginPageHandler())
	mux.Handle("POST /login", h.LoginHandler())
	mux.Handle("POST /logout", h.LogoutHandler())
}

// LoginPageHandler menangani GET /login.
func (h *Handler) LoginPageHandler() http.Handler {
	return http.HandlerFunc(h.loginPage)
}

func (h *Handler) LoginHandler() http.Handler {
	return httpx.LimitBody(httpx.LoginBodyLimit, h.middleware.RequireCSRF(http.HandlerFunc(h.login)))
}

// LogoutHandler membatasi ukuran form dan mengizinkan CSRF pada family yang memerlukan
// reauthentication untuk POST /logout.
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

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	state, ok := SessionFromContext(r.Context())
	if !ok {
		h.renderer.RenderError(w, r, httpx.Layout{}, sessionFailure(problem.SessionInvalid))
		return
	}
	username := NormalizeLoginUsername(r.PostForm.Get("username"))
	page := LoginPage{Layout: httpx.Layout{Title: "Log in", CSRF: state.CSRF}, Username: username,
		Reauth: state.Kind == AuthenticatedSession && state.ReauthRequired}
	if err := httpx.ValidateFields(r.PostForm, []string{"_csrf", "username", "password"}, nil); err != nil {
		_, _, page.Error = httpx.ErrorInfo(err)
		h.renderLogin(w, r, http.StatusBadRequest, page)
		return
	}
	if state.Kind == AuthenticatedSession && !state.ReauthRequired {
		_, _, page.Error = httpx.ErrorInfo(ErrAlreadyAuthenticated)
		h.renderLogin(w, r, http.StatusConflict, page)
		return
	}
	if err := h.service.checkLoginRateLimit(r.Context(), username, r.RemoteAddr, strings.Join(r.Header.Values("X-Forwarded-For"), ",")); err != nil {
		var limited *RateLimitError
		if errors.As(err, &limited) {
			w.Header().Set("Retry-After", strconv.Itoa(limited.RetryAfter))
			page.Error = "Too many login attempts. Try again later."
			h.renderLogin(w, r, http.StatusTooManyRequests, page)
			return
		}
		h.renderer.RenderError(w, r, page.Layout, err)
		return
	}
	newState, token, err := h.service.Login(r.Context(), state, username, r.PostForm.Get("password"))
	if errors.Is(err, ErrInvalidCredentials) {
		page.Error = "Invalid username or password."
		h.renderLogin(w, r, http.StatusUnauthorized, page)
		return
	}
	if err != nil {
		if problemIs(err, problem.AuthBusy) {
			page.Error = "Authentication is busy. Try again."
			h.renderLogin(w, r, http.StatusServiceUnavailable, page)
			return
		}
		if problemIs(err, problem.SessionInvalid) {
			http.SetCookie(w, h.service.ClearCookie())
			_ = httpx.SeeOther(w, r, "/login")
			return
		}
		h.renderer.RenderError(w, r, page.Layout, err)
		return
	}
	http.SetCookie(w, h.service.Cookie(token, newState.AbsoluteExpiresAt))
	_ = httpx.SeeOther(w, r, "/databases")
}

func (h *Handler) renderLogin(w http.ResponseWriter, r *http.Request, status int, page LoginPage) {
	if err := h.renderer.Render(w, r, status, "login", page); err != nil {
		h.renderer.RenderError(w, r, page.Layout, err)
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
