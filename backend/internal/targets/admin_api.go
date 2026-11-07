package targets

import (
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

type connectionDTO struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	DatabaseName   string `json:"database_name"`
	Username       string `json:"username"`
	SSLMode        string `json:"ssl_mode"`
	IsActive       bool   `json:"is_active"`
	PasswordStored bool   `json:"password_stored"`
	Revision       int64  `json:"config_revision,string"`
	UpdatedAt      string `json:"updated_at"`
}

func connectionOf(t Target) connectionDTO {
	return connectionDTO{t.ID.String(), t.Name, t.Host, t.Port, t.DatabaseName, t.Username,
		t.SSLMode, t.IsActive, t.PasswordStored, t.ConfigRevision, t.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

func (p *Provider) RegisterAdminAPI(mux *http.ServeMux, middleware *auth.Middleware, authorizer access.Authorizer) {
	guard := func(next http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := access.FromContext(r.Context())
			if !ok {
				httpx.WriteJSONError(w, r, failure(problem.SessionInvalid))
				return
			}
			if err := authorizer.RequireAdmin(r.Context(), actor); err != nil {
				httpx.WriteJSONError(w, r, err)
				return
			}
			next(w, r)
		})
	}
	mux.Handle("GET /api/admin/databases", middleware.RequireSession(guard(p.connectionsAPI)))
	mux.Handle("GET /api/admin/databases/{database_id}", middleware.RequireSession(guard(p.connectionAPI)))
	post := func(pattern string, next http.HandlerFunc) {
		mux.Handle(pattern, middleware.RequireSession(httpx.LimitBody(httpx.NormalBodyLimit, middleware.RequireCSRF(guard(next)))))
	}
	post("POST /api/admin/databases", p.createAPI)
	post("POST /api/admin/databases/{database_id}", p.editAPI)
	post("POST /api/admin/databases/{database_id}/test", p.testAPI)
}

func (p *Provider) connectionsAPI(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items, more, err := p.ListAdmin(r.Context(), limit, offset)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	data := make([]connectionDTO, 0, len(items))
	for _, item := range items {
		data = append(data, connectionOf(item))
	}
	httpx.WriteJSON(w, r, 200, data, httpx.NewPage(limit, offset, more))
}

func (p *Provider) connectionAPI(w http.ResponseWriter, r *http.Request) {
	id, err := targetID(r)
	if err == nil {
		_, err = httpx.ParseQuery(r)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	target, err := p.Get(r.Context(), id)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, connectionOf(target), nil)
}

func (p *Provider) createAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string `json:"name"`
		Host         string `json:"host"`
		Port         int    `json:"port"`
		DatabaseName string `json:"database_name"`
		Username     string `json:"username"`
		Password     string `json:"password"`
		SSLMode      string `json:"ssl_mode"`
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, []string{"name", "host", "port", "database_name", "username", "password", "ssl_mode"}, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	actor, _ := access.FromContext(r.Context())
	target, err := p.Create(r.Context(), actor.UserID, CreateInput{Name: in.Name, Host: in.Host, Port: in.Port,
		DatabaseName: in.DatabaseName, Username: in.Username, Password: in.Password, SSLMode: in.SSLMode})
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/admin/databases/"+target.ID.String())
	httpx.WriteJSON(w, r, 201, connectionOf(target), nil)
}

func (p *Provider) editAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision string `json:"expected_revision"`
		Name     string `json:"name"`
		Username string `json:"username"`
		Password string `json:"password"`
		SSLMode  string `json:"ssl_mode"`
		Active   bool   `json:"is_active"`
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, []string{"expected_revision", "name", "username", "ssl_mode", "is_active"}, []string{"password"})
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	id, err := targetID(r)
	var revision int64
	if err == nil {
		revision, err = httpx.Int64Value(url.Values{"revision": {in.Revision}}, "revision", 1, math.MaxInt64)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	target, err := p.Edit(r.Context(), id, EditInput{ExpectedRevision: revision, Name: in.Name,
		Username: in.Username, Password: in.Password, SSLMode: in.SSLMode, IsActive: in.Active})
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, connectionOf(target), nil)
}

func (p *Provider) testAPI(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, nil, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	id, err := targetID(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	probe, err := p.Test(r.Context(), id)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, struct {
		OK            bool        `json:"ok"`
		Reason        ProbeReason `json:"reason"`
		ServerVersion int         `json:"server_version"`
		LatencyMS     float64     `json:"latency_ms"`
		Latency       string      `json:"latency"`
	}{probe.OK, probe.Reason, probe.ServerVersion, float64(probe.Latency) / float64(time.Millisecond), probe.Latency.String()}, nil)
}
