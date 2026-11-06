package migration

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

type databaseDTO struct {
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	Access access.Level `json:"access"`
}

type draftDTO struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	SQL       *string    `json:"sql,omitempty"`
	Revision  string     `json:"revision"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type receiptDTO struct {
	ID         string `json:"id"`
	TargetID   string `json:"target_id"`
	DraftID    string `json:"draft_id"`
	Revision   string `json:"revision"`
	RequestKey string `json:"request_key"`
	Version    string `json:"version"`
	State      string `json:"state"`
}

func apiReceipt(r AttemptReceipt) receiptDTO {
	return receiptDTO{r.ID.String(), r.TargetID.String(), r.DraftID.String(), strconv.FormatInt(r.DraftRevision, 10), r.RequestKey.String(), r.Version, r.State}
}

type attemptDTO struct {
	ID                string      `json:"id"`
	Version           string      `json:"version"`
	Title             string      `json:"title"`
	TargetName        string      `json:"target_name"`
	Revision          string      `json:"revision"`
	Actor             string      `json:"actor"`
	State             string      `json:"state"`
	CreatedAt         time.Time   `json:"created_at"`
	StartedAt         *time.Time  `json:"started_at"`
	FinishedAt        *time.Time  `json:"finished_at"`
	SQL               *string     `json:"sql,omitempty"`
	SQLHash           string      `json:"sql_hash,omitempty"`
	CommitStartedAt   *time.Time  `json:"commit_started_at,omitempty"`
	FirstUnknownAt    *time.Time  `json:"first_unknown_at,omitempty"`
	ResolvedAt        *time.Time  `json:"resolved_at,omitempty"`
	ResolutionSummary string      `json:"resolution_summary,omitempty"`
	Failure           *failureDTO `json:"failure,omitempty"`
}

type failureDTO struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	SQLState       string `json:"sqlstate"`
	StatementIndex int    `json:"statement_index"`
}

func apiAttempt(a AttemptSummary) attemptDTO {
	return attemptDTO{ID: a.ID.String(), Version: a.Version, Title: a.Title, TargetName: a.TargetName,
		Revision: strconv.FormatInt(a.DraftRevision, 10), Actor: a.ActorUsername, State: a.State,
		CreatedAt: a.CreatedAt, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt}
}

type apiScope struct {
	principal access.Principal
	target    uuid.UUID
	database  databaseDTO
}

func NewAPIHandler(store *DraftStore, runner *Runner, authorizer access.Authorizer) (*Handler, error) {
	if store == nil || store.pool == nil || runner == nil || runner.store == nil || runner.store.pool != store.pool || authorizer == nil {
		return nil, draftFailure(problem.InternalError)
	}
	return &Handler{store: store, runner: runner, authorizer: authorizer}, nil
}

// RegisterAPI mempertahankan runner sinkron, replay, dan snapshot tersimpan.
func (h *Handler) RegisterAPI(mux *http.ServeMux, middleware *auth.Middleware) {
	register := func(pattern string, action access.Action, post bool, fn func(http.ResponseWriter, *http.Request, apiScope)) {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			timeout := 10 * time.Second
			if action == access.MigrationExecute {
				timeout = 65 * time.Second
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			r = r.WithContext(ctx)
			s, err := h.prepareAPI(r, action)
			if err != nil {
				httpx.WriteJSONError(w, r, err)
				return
			}
			fn(w, r, s)
		})
		var protected http.Handler = next
		if post {
			protected = httpx.LimitBody(httpx.MaxFormBody, middleware.RequireCSRF(protected))
		}
		mux.Handle(pattern, middleware.RequireSession(protected))
	}
	register("GET /api/databases/{database_id}/drafts", access.MigrationWrite, false, h.draftsAPI)
	register("POST /api/databases/{database_id}/drafts", access.MigrationWrite, true, h.createDraftAPI)
	register("GET /api/databases/{database_id}/drafts/{draft_id}", access.MigrationWrite, false, h.draftAPI)
	register("POST /api/databases/{database_id}/drafts/{draft_id}", access.MigrationWrite, true, h.saveDraftAPI)
	register("GET /api/databases/{database_id}/drafts/{draft_id}/review", access.MigrationWrite, false, h.reviewAPI)
	register("POST /api/databases/{database_id}/drafts/{draft_id}/execute", access.MigrationExecute, true, h.executeAPI)
	register("GET /api/databases/{database_id}/history", access.DatabaseView, false, h.historyAPI)
	register("GET /api/databases/{database_id}/attempts/{attempt_id}", access.DatabaseView, false, h.attemptAPI)
	register("GET /api/databases/{database_id}/receipts/{request_key}", access.DatabaseView, false, h.receiptAPI)
}

func (h *Handler) prepareAPI(r *http.Request, action access.Action) (apiScope, error) {
	var s apiScope
	var ok bool
	s.principal, ok = access.FromContext(r.Context())
	if !ok {
		return s, draftFailure(problem.SessionInvalid)
	}
	id, err := draftPathID(r, "database_id")
	if err != nil {
		return s, err
	}
	level, err := h.authorizer.RequireDatabase(r.Context(), s.principal, id, action)
	if err != nil {
		return s, err
	}
	s.target, s.database = id, databaseDTO{ID: id.String(), Access: level}
	lease, err := platformdb.Acquire(r.Context(), h.store.pool)
	if err != nil {
		return s, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	err = lease.QueryRow(r.Context(), `SELECT name FROM databrain.database_targets WHERE id=$1 AND is_active`, id).Scan(&s.database.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, draftFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return s, draftFailure(problem.PlatformUnavailable)
	}
	return s, nil
}

func (h *Handler) draftDTOs(ctx context.Context, drafts []DraftSummary) ([]draftDTO, error) {
	items, err := h.display(ctx, drafts)
	if err != nil {
		return nil, err
	}
	result := make([]draftDTO, 0, len(items))
	for i, item := range items {
		result = append(result, draftDTO{ID: item.ID, Title: item.Title, Revision: item.Revision, UpdatedBy: item.UpdatedBy, UpdatedAt: &drafts[i].UpdatedAt})
	}
	return result, nil
}

func (h *Handler) writeDraftAPI(w http.ResponseWriter, r *http.Request, s apiScope, d Draft, status int) {
	items, err := h.draftDTOs(r.Context(), []DraftSummary{d.DraftSummary})
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items[0].SQL = &d.SQL
	httpx.WriteJSON(w, r, status, struct {
		Database databaseDTO `json:"database"`
		Draft    draftDTO    `json:"draft"`
	}{s.database, items[0]}, nil)
}

func (h *Handler) draftsAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	limit, offset, err := draftPagination(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	drafts, more, err := h.store.List(r.Context(), s.target, limit, offset)
	var items []draftDTO
	if err == nil {
		items, err = h.draftDTOs(r.Context(), drafts)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO `json:"database"`
		Drafts   []draftDTO  `json:"drafts"`
	}{s.database, items}, httpx.NewPage(limit, offset, more))
}

func (h *Handler) draftAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	_, err := httpx.ParseQuery(r)
	id, idErr := draftPathID(r, "draft_id")
	if err == nil {
		err = idErr
	}
	var d Draft
	if err == nil {
		d, err = h.store.Get(r.Context(), s.target, id)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	h.writeDraftAPI(w, r, s, d, 200)
}

func (h *Handler) createDraftAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	var in struct {
		Title string `json:"title"`
		SQL   string `json:"sql"`
	}
	err := httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"title", "sql"}, nil)
	var d Draft
	if err == nil {
		d, err = h.store.Create(r.Context(), s.target, s.principal.UserID, DraftInput{in.Title, in.SQL})
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/databases/"+s.target.String()+"/drafts/"+d.ID.String())
	h.writeDraftAPI(w, r, s, d, 201)
}

func (h *Handler) saveDraftAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	var in struct {
		Title    string `json:"title"`
		SQL      string `json:"sql"`
		Revision string `json:"expected_revision"`
	}
	err := httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"title", "sql", "expected_revision"}, nil)
	id, idErr := draftPathID(r, "draft_id")
	if err == nil {
		err = idErr
	}
	var revision int64
	if err == nil {
		revision, err = httpx.Int64Value(url.Values{"expected_revision": {in.Revision}}, "expected_revision", 1, math.MaxInt64)
	}
	var d Draft
	if err == nil {
		d, err = h.store.Save(r.Context(), s.target, id, s.principal.UserID, SaveDraftInput{DraftInput{in.Title, in.SQL}, revision})
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	h.writeDraftAPI(w, r, s, d, 200)
}

func (h *Handler) reviewAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	id, err := draftPathID(r, "draft_id")
	values, queryErr := httpx.ParseQuery(r, "expected_revision", "request_key")
	if err == nil {
		err = queryErr
	}
	if err == nil && values.Has("expected_revision") != values.Has("request_key") {
		err = attemptFailure(problem.InvalidRequest)
	}
	var input ReserveInput
	var d Draft
	var receipt *AttemptReceipt
	if err == nil && len(values) != 0 {
		input, err = reviewInput(values)
		if err == nil {
			receipt, err = h.runner.replay(r.Context(), s.target, id, s.principal.UserID, input)
		}
	}
	if err == nil && receipt == nil {
		d, err = h.store.Get(r.Context(), s.target, id)
	}
	if err == nil && len(values) == 0 {
		input = ReserveInput{d.Revision, uuid.New()}
	}
	if err == nil && receipt == nil && input.ExpectedRevision != d.Revision {
		err = attemptFailure(problem.RevisionConflict)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	var resultReceipt *receiptDTO
	var draft *draftDTO
	if receipt != nil {
		value := apiReceipt(*receipt)
		resultReceipt = &value
	} else {
		draft = &draftDTO{ID: id.String(), Title: d.Title, SQL: &d.SQL, Revision: strconv.FormatInt(d.Revision, 10)}
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database   databaseDTO `json:"database"`
		Draft      *draftDTO   `json:"draft"`
		Revision   string      `json:"expected_revision"`
		RequestKey string      `json:"request_key"`
		Receipt    *receiptDTO `json:"receipt"`
	}{s.database, draft, strconv.FormatInt(input.ExpectedRevision, 10), input.RequestKey.String(), resultReceipt}, nil)
}

func (h *Handler) executeAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	var in struct {
		Revision string `json:"expected_revision"`
		Key      string `json:"request_key"`
	}
	err := httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"expected_revision", "request_key"}, nil)
	id, idErr := draftPathID(r, "draft_id")
	if err == nil {
		err = idErr
	}
	var input ReserveInput
	if err == nil {
		input, err = reviewInput(url.Values{"expected_revision": {in.Revision}, "request_key": {in.Key}})
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	receipt, err := h.runner.Execute(r.Context(), s.principal, s.target, id, input)
	if err != nil {
		_, code, _ := httpx.ErrorInfo(err)
		if code != problem.SessionInvalid && code != problem.DatabaseNotFound && code != problem.DatabaseAccessDenied {
			// Baca receipt setelah error persistence tanpa mengulang eksekusi atau mengganti key.
			end, _ := r.Context().Deadline()
			ctx, cancel := cleanupExecutionContext(end, 2*time.Second)
			stored, lookupErr := h.runner.replay(ctx, s.target, id, s.principal.UserID, input)
			cancel()
			if lookupErr == nil && stored != nil {
				receipt, err = *stored, nil
			}
		}
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, apiReceipt(receipt), nil)
}

func (h *Handler) historyAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	limit, offset, err := draftPagination(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items, more, err := h.runner.store.List(r.Context(), s.target, limit, offset)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	attempts := make([]attemptDTO, 0, len(items))
	for _, item := range items {
		attempts = append(attempts, apiAttempt(item))
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO  `json:"database"`
		Attempts []attemptDTO `json:"attempts"`
	}{s.database, attempts}, httpx.NewPage(limit, offset, more))
}

func (h *Handler) attemptAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	_, err := httpx.ParseQuery(r)
	id, idErr := draftPathID(r, "attempt_id")
	if err == nil {
		err = idErr
	}
	var a *AttemptDetail
	if err == nil {
		a, err = h.runner.store.Get(r.Context(), s.target, id)
		if err == nil && a == nil {
			err = attemptFailure(problem.AttemptNotFound)
		}
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	item := apiAttempt(a.AttemptSummary)
	item.SQL, item.SQLHash = &a.SQL, a.SQLHash
	item.CommitStartedAt, item.FirstUnknownAt, item.ResolvedAt = a.CommitStartedAt, a.FirstUnknownAt, a.ResolvedAt
	item.ResolutionSummary = a.ResolutionSummary
	if a.Failure != nil {
		f := a.Failure
		item.Failure = &failureDTO{f.Code, f.Message, f.SQLState, f.StatementIndex}
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO `json:"database"`
		Attempt  attemptDTO  `json:"attempt"`
	}{s.database, item}, nil)
}

func (h *Handler) receiptAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	_, err := httpx.ParseQuery(r)
	key, keyErr := draftPathID(r, "request_key")
	if err == nil {
		err = keyErr
	}
	var receipt *AttemptReceipt
	if err == nil {
		receipt, err = h.runner.store.Receipt(r.Context(), s.target, s.principal.UserID, key)
		if err == nil && receipt == nil {
			err = attemptFailure(problem.AttemptNotFound)
		}
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, apiReceipt(*receipt), nil)
}
