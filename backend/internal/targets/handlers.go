package targets

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Pager struct{ Prev, Next string }

type ActiveAttempt struct{ ID, Version, State string }

type DatabaseItem struct {
	ID, Name      string
	Access        access.Level
	ActiveAttempt *ActiveAttempt
}

type DatabasesPage struct {
	httpx.Layout
	Databases []DatabaseItem
	Pager     Pager
}

// ConnectionForm tidak menyimpan password. Input password selalu kosong, termasuk setelah error
// validasi atau revision.
type ConnectionForm struct {
	Name, Host, Port, DatabaseName, Username, SSLMode, ExpectedRevision string
	IsActive                                                            bool
}

// Data ini dipakai template admin_databases dan admin_database. Route baru dipasang setelah kedua
// template tersedia.
type AdminDatabasesPage struct {
	httpx.Layout
	Databases   []Target
	Pager       Pager
	Form        ConnectionForm
	Error, Code string
}

type AdminDatabasePage struct {
	httpx.Layout
	Target      Target
	Form        ConnectionForm
	Probe       *ProbeResult
	Error, Code string
	Conflict    bool
}

type pageRenderer interface {
	Render(http.ResponseWriter, *http.Request, int, string, any) error
	RenderError(http.ResponseWriter, *http.Request, httpx.Layout, error)
	HasTemplate(string) bool
}

type Handler struct {
	provider   *Provider
	authorizer access.Authorizer
	renderer   pageRenderer
}

func NewHandler(provider *Provider, authorizer access.Authorizer, renderer *httpx.Renderer) (*Handler, error) {
	if provider == nil || authorizer == nil || !renderer.HasTemplate("databases") || !renderer.HasTemplate("error") {
		return nil, failure(problem.InternalError)
	}
	return &Handler{provider: provider, authorizer: authorizer, renderer: renderer}, nil
}

// Register selalu memasang daftar user. Form dan aksi admin dipasang bersama agar mutasi memiliki
// halaman sukses/error. Nilai hasil menunjukkan ketersediaan UI admin.
func (h *Handler) Register(mux *http.ServeMux, middleware *auth.Middleware) bool {
	mux.Handle("GET /databases", middleware.RequireSession(http.HandlerFunc(h.databases)))
	if !h.renderer.HasTemplate("admin_databases") || !h.renderer.HasTemplate("admin_database") {
		return false
	}
	mux.Handle("GET /admin/databases", middleware.RequireSession(http.HandlerFunc(h.adminDatabases)))
	mux.Handle("GET /admin/databases/{database_id}", middleware.RequireSession(http.HandlerFunc(h.adminDatabase)))
	post := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, middleware.RequireSession(httpx.LimitBody(httpx.NormalBodyLimit, middleware.RequireCSRF(handler))))
	}
	post("POST /admin/databases", h.create)
	post("POST /admin/databases/{database_id}", h.edit)
	post("POST /admin/databases/{database_id}/test", h.test)
	return true
}

// layout membaca metadata tampilan dari identitas middleware. Validasi session/token dan hak admin
// tetap dilakukan oleh auth dan Authorizer, tanpa identitas dari client atau query.
func (h *Handler) layout(r *http.Request, title, active string, admin bool) (httpx.Layout, access.Principal, error) {
	state, ok := auth.SessionFromContext(r.Context())
	principal, present := access.FromContext(r.Context())
	if !ok || !present || state.Kind != auth.AuthenticatedSession || state.Principal != principal {
		return httpx.Layout{}, access.Principal{}, failure(problem.SessionInvalid)
	}
	layout := httpx.Layout{Title: title, Active: active, CSRF: state.CSRF}
	if admin {
		if err := h.authorizer.RequireAdmin(r.Context(), principal); err != nil {
			return layout, principal, err
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, h.provider.pool)
	if err != nil {
		return layout, principal, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	var user httpx.LayoutUser
	err = lease.QueryRow(ctx, `SELECT display_name,username,is_platform_admin FROM databrain.users
	    WHERE id=$1 AND is_active AND security_version=$2`, principal.UserID, principal.SecurityVersion).
		Scan(&user.DisplayName, &user.Username, &user.IsAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return layout, principal, failure(problem.SessionInvalid)
	}
	if err != nil {
		return layout, principal, failure(problem.PlatformUnavailable)
	}
	layout.User = &user
	return layout, principal, nil
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, layout httpx.Layout, err error) {
	var value *problem.Error
	if errors.As(err, &value) && value.Code == problem.SessionInvalid {
		httpx.WriteError(w, r, err)
		return
	}
	h.renderer.RenderError(w, r, layout, err)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, layout httpx.Layout, status int, name string, page any) {
	if err := h.renderer.Render(w, r, status, name, page); err != nil {
		h.fail(w, r, layout, err)
	}
}

func pagination(r *http.Request) (int, int, error) {
	values, err := httpx.ParseQuery(r, "limit", "offset")
	if err != nil {
		return 0, 0, err
	}
	limit, offset := int64(50), int64(0)
	if values.Has("limit") {
		limit, err = httpx.Int64Value(values, "limit", 1, 200)
	}
	if err == nil && values.Has("offset") {
		offset, err = httpx.Int64Value(values, "offset", 0, 100000)
	}
	return int(limit), int(offset), err
}

func pager(path string, limit, offset int, more bool) Pager {
	link := func(offset int) string {
		return path + "?" + url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}.Encode()
	}
	var result Pager
	if offset > 0 {
		result.Prev = link(max(0, offset-limit))
	}
	if more && offset+limit <= 100000 {
		result.Next = link(offset + limit)
	}
	return result
}

func (h *Handler) databases(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "Databases", "databases", false)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	limit, offset, err := pagination(r)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	items, more, err := h.provider.ListForUser(r.Context(), principal.UserID, limit, offset)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	page := DatabasesPage{Layout: layout, Pager: pager("/databases", limit, offset, more)}
	for _, item := range items {
		database := DatabaseItem{ID: item.ID.String(), Name: item.Name, Access: item.EffectiveAccess}
		if item.ActiveAttemptID != nil {
			database.ActiveAttempt = &ActiveAttempt{ID: item.ActiveAttemptID.String(), Version: item.ActiveAttemptVersion, State: item.ActiveAttemptState}
		}
		page.Databases = append(page.Databases, database)
	}
	h.render(w, r, layout, http.StatusOK, "databases", page)
}

func (h *Handler) adminDatabases(w http.ResponseWriter, r *http.Request) {
	layout, _, err := h.layout(r, "Connections", "connections", true)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	limit, offset, err := pagination(r)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.renderAdminList(w, r, layout, limit, offset, ConnectionForm{SSLMode: "verify-full", IsActive: true}, nil)
}

func (h *Handler) renderAdminList(w http.ResponseWriter, r *http.Request, layout httpx.Layout, limit, offset int, form ConnectionForm, formErr error) {
	items, more, err := h.provider.ListAdmin(r.Context(), limit, offset)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	page := AdminDatabasesPage{Layout: layout, Databases: items, Pager: pager("/admin/databases", limit, offset, more), Form: form}
	status := http.StatusOK
	if formErr != nil {
		var code problem.Code
		status, code, page.Error = httpx.ErrorInfo(formErr)
		page.Code = string(code)
	}
	h.render(w, r, layout, status, "admin_databases", page)
}

func targetID(r *http.Request) (uuid.UUID, error) {
	return httpx.UUIDValue(url.Values{"database_id": {r.PathValue("database_id")}}, "database_id")
}

func formOf(target Target) ConnectionForm {
	return ConnectionForm{Name: target.Name, Host: target.Host, Port: strconv.Itoa(target.Port), DatabaseName: target.DatabaseName,
		Username: target.Username, SSLMode: target.SSLMode, IsActive: target.IsActive, ExpectedRevision: strconv.FormatInt(target.ConfigRevision, 10)}
}

func submittedForm(values url.Values) ConnectionForm {
	values = httpx.SafeFormValues(values, "name", "host", "port", "database_name", "username", "ssl_mode", "is_active", "expected_revision")
	return ConnectionForm{Name: values.Get("name"), Host: values.Get("host"), Port: values.Get("port"), DatabaseName: values.Get("database_name"),
		Username: values.Get("username"), SSLMode: values.Get("ssl_mode"), IsActive: values.Get("is_active") == "true", ExpectedRevision: values.Get("expected_revision")}
}

func (h *Handler) adminDatabase(w http.ResponseWriter, r *http.Request) {
	layout, _, err := h.layout(r, "Connection", "connections", true)
	if err == nil {
		_, err = httpx.ParseQuery(r)
	}
	var target Target
	if err == nil {
		var id uuid.UUID
		id, err = targetID(r)
		if err == nil {
			target, err = h.provider.Get(r.Context(), id)
		}
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.render(w, r, layout, http.StatusOK, "admin_database", AdminDatabasePage{Layout: layout, Target: target, Form: formOf(target)})
}

func createInput(values url.Values) (CreateInput, error) {
	if err := httpx.ValidateFields(values, []string{"_csrf", "name", "host", "port", "database_name", "username", "password", "ssl_mode"}, nil); err != nil {
		return CreateInput{}, err
	}
	port, err := httpx.Int64Value(values, "port", 1, 65535)
	return CreateInput{Name: values.Get("name"), Host: values.Get("host"), Port: int(port), DatabaseName: values.Get("database_name"),
		Username: values.Get("username"), Password: values.Get("password"), SSLMode: values.Get("ssl_mode")}, err
}

func editInput(values url.Values) (EditInput, error) {
	if err := httpx.ValidateFields(values, []string{"_csrf", "expected_revision", "name", "username", "ssl_mode", "is_active"}, []string{"password"}); err != nil {
		return EditInput{}, err
	}
	revision, err := httpx.Int64Value(values, "expected_revision", 1, math.MaxInt64)
	if err != nil {
		return EditInput{}, err
	}
	active, err := httpx.BoolValue(values, "is_active")
	return EditInput{ExpectedRevision: revision, Name: values.Get("name"), Username: values.Get("username"), Password: values.Get("password"),
		SSLMode: values.Get("ssl_mode"), IsActive: active}, err
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "Connections", "connections", true)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	in, err := createInput(r.PostForm) // RequireCSRF sudah membaca form sekali dengan batas ukuran.
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	target, err := h.provider.Create(r.Context(), principal.UserID, in)
	if err != nil {
		status, _, _ := httpx.ErrorInfo(err)
		if status == 422 || status == 409 {
			h.renderAdminList(w, r, layout, 50, 0, submittedForm(r.PostForm), err)
		} else {
			h.fail(w, r, layout, err)
		}
		return
	}
	_ = httpx.SeeOther(w, r, "/admin/databases/"+target.ID.String())
}

func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	layout, _, err := h.layout(r, "Connection", "connections", true)
	var id uuid.UUID
	var in EditInput
	if err == nil {
		id, err = targetID(r)
	}
	if err == nil {
		in, err = editInput(r.PostForm)
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	target, err := h.provider.Edit(r.Context(), id, in)
	if err != nil {
		status, code, message := httpx.ErrorInfo(err)
		if status != 409 && status != 422 {
			h.fail(w, r, layout, err)
			return
		}
		current, loadErr := h.provider.Get(r.Context(), id)
		if loadErr != nil {
			h.fail(w, r, layout, loadErr)
			return
		}
		form := submittedForm(r.PostForm)
		form.Host, form.Port, form.DatabaseName = current.Host, strconv.Itoa(current.Port), current.DatabaseName
		page := AdminDatabasePage{Layout: layout, Target: current, Form: form, Error: message, Code: string(code), Conflict: code == problem.RevisionConflict}
		h.render(w, r, layout, status, "admin_database", page)
		return
	}
	_ = httpx.SeeOther(w, r, "/admin/databases/"+target.ID.String())
}

func (h *Handler) test(w http.ResponseWriter, r *http.Request) {
	layout, _, err := h.layout(r, "Connection", "connections", true)
	var id uuid.UUID
	if err == nil {
		id, err = targetID(r)
	}
	if err == nil {
		err = httpx.ValidateFields(r.PostForm, []string{"_csrf"}, nil)
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	target, err := h.provider.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	probe, err := h.provider.Test(r.Context(), id)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.render(w, r, layout, http.StatusOK, "admin_database", AdminDatabasePage{Layout: layout, Target: target, Form: formOf(target), Probe: &probe})
}
