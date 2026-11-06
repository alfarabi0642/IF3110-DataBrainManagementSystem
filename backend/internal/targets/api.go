package targets

import (
	"net/http"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

type activeAttemptDTO struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	State   string `json:"state"`
}

type databaseDTO struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Access        access.Level      `json:"access"`
	ActiveAttempt *activeAttemptDTO `json:"active_attempt"`
}

// RegisterListAPI hanya memproyeksikan daftar workspace, tanpa renderer atau credential target.
func (p *Provider) RegisterListAPI(mux *http.ServeMux, middleware *auth.Middleware) {
	mux.Handle("GET /api/databases", middleware.RequireSession(http.HandlerFunc(p.databasesAPI)))
}

func (p *Provider) databasesAPI(w http.ResponseWriter, r *http.Request) {
	principal, ok := access.FromContext(r.Context())
	if !ok {
		httpx.WriteJSONError(w, r, failure(problem.SessionInvalid))
		return
	}
	limit, offset, err := pagination(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items, more, err := p.ListForUser(r.Context(), principal.UserID, limit, offset)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	data := make([]databaseDTO, 0, len(items))
	for _, item := range items {
		database := databaseDTO{ID: item.ID.String(), Name: item.Name, Access: item.EffectiveAccess}
		if item.ActiveAttemptID != nil {
			database.ActiveAttempt = &activeAttemptDTO{ID: item.ActiveAttemptID.String(),
				Version: item.ActiveAttemptVersion, State: item.ActiveAttemptState}
		}
		data = append(data, database)
	}
	httpx.WriteJSON(w, r, http.StatusOK, data, httpx.NewPage(limit, offset, more))
}
