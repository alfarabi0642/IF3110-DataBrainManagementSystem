package httpapi

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/legacy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

type databaseDTO struct {
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	Access access.Level `json:"access"`
}

type DraftDTO struct {
	ID                 string     `json:"id"`
	Title              string     `json:"title"`
	SQL                *string    `json:"sql,omitempty"`
	DownSQL            *string    `json:"down_sql,omitempty"`
	Irreversible       bool       `json:"irreversible"`
	IrreversibleReason *string    `json:"irreversible_reason,omitempty"`
	Frozen             bool       `json:"frozen"`
	Revision           string     `json:"revision"`
	UpdatedBy          string     `json:"updated_by,omitempty"`
	UpdatedAt          *time.Time `json:"updated_at,omitempty"`
}

type ReceiptDTO struct {
	ID         string `json:"id"`
	TargetID   string `json:"target_id"`
	DraftID    string `json:"draft_id"`
	Revision   string `json:"revision"`
	RequestKey string `json:"request_key"`
	Version    string `json:"version"`
	State      string `json:"state"`
}

func apiReceipt(r legacy.AttemptReceipt) ReceiptDTO {
	return ReceiptDTO{r.ID.String(), r.TargetID.String(), r.DraftID.String(), strconv.FormatInt(r.DraftRevision, 10), r.RequestKey.String(), r.Version, r.State}
}

type AttemptDTO struct {
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

func apiAttempt(a legacy.AttemptSummary) AttemptDTO {
	return AttemptDTO{ID: a.ID.String(), Version: a.Version, Title: a.Title, TargetName: a.TargetName,
		Revision: strconv.FormatInt(a.DraftRevision, 10), Actor: a.ActorUsername, State: a.State,
		CreatedAt: a.CreatedAt, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt}
}

type apiScope struct {
	principal access.Principal
	target    uuid.UUID
	database  databaseDTO
}

type APIHandler struct {
	service              *migration.Service
	legacyExecuteRetired bool
}

func NewAPIHandler(service *migration.Service) (*APIHandler, error) {
	if service == nil {
		return nil, &problem.Error{Code: problem.InternalError}
	}
	return &APIHandler{service: service}, nil
}

// RetireLegacyExecute menonaktifkan jalur sinkron M1 POST /drafts/{draft_id}/execute dengan 410
// LEGACY_EXECUTE_RETIRED agar seluruh eksekusi produksi wajib melalui approval dan antrean River.
func (h *APIHandler) RetireLegacyExecute() {
	h.legacyExecuteRetired = true
}

// RegisterAPI mendaftarkan endpoint draft, review, approval, rollback/forward, history, dan execution.
func (h *APIHandler) RegisterAPI(mux *http.ServeMux, middleware *auth.Middleware) {
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
	register("GET /api/databases/{database_id}/history/events/{execution_id}", access.DatabaseView, false, h.committedEventAPI)
	register("GET /api/databases/{database_id}/activity", access.DatabaseView, false, h.activityAPI)
	register("GET /api/databases/{database_id}/attempts/{attempt_id}", access.DatabaseView, false, h.attemptAPI)
	register("GET /api/databases/{database_id}/receipts/{request_key}", access.DatabaseView, false, h.receiptAPI)
	register("POST /api/databases/{database_id}/drafts/{draft_id}/submit", access.MigrationWrite, true, h.submitRequestAPI)
	register("POST /api/databases/{database_id}/migrations/{definition_id}/rollback-request", access.MigrationWrite, true, h.submitRollbackRequestAPI)
	register("POST /api/databases/{database_id}/migrations/{definition_id}/forward-request", access.MigrationWrite, true, h.submitForwardRequestAPI)
	register("GET /api/databases/{database_id}/requests", access.DatabaseView, false, h.requestsAPI)
	register("GET /api/databases/{database_id}/requests/{request_id}", access.DatabaseView, false, h.requestAPI)
	register("POST /api/databases/{database_id}/requests/{request_id}/approve", access.DatabaseView, true, h.approveRequestAPI)
	register("POST /api/databases/{database_id}/requests/{request_id}/execute", access.MigrationWrite, true, h.executeRequestAPI)
	register("GET /api/databases/{database_id}/executions/{execution_id}", access.DatabaseView, false, h.executionAPI)
	register("GET /api/databases/{database_id}/executions/{execution_id}/batches", access.DatabaseView, false, h.executionBatchesAPI)
	register("POST /api/databases/{database_id}/executions/{execution_id}/resume-requests", access.MigrationWrite, true, h.submitResumeRequestAPI)
	register("POST /api/databases/{database_id}/executions/{execution_id}/compensation-requests", access.MigrationWrite, true, h.submitCompensationRequestAPI)
	// Job lookup tidak memiliki database di path; handler meresolusi target dan mengotorisasi sendiri.
	mux.Handle("GET /api/jobs/{job_id}", middleware.RequireSession(http.HandlerFunc(h.jobAPI)))
}

func (h *APIHandler) prepareAPI(r *http.Request, action access.Action) (apiScope, error) {
	var s apiScope
	var ok bool
	s.principal, ok = access.FromContext(r.Context())
	if !ok {
		return s, &problem.Error{Code: problem.SessionInvalid}
	}
	id, err := draftPathID(r, "database_id")
	if err != nil {
		return s, err
	}
	database, err := h.service.Database(r.Context(), s.principal, id, action)
	if err != nil {
		return s, err
	}
	s.target, s.database = id, databaseDTO{ID: id.String(), Name: database.Name, Access: database.Access}
	return s, nil
}

func draftDTOs(drafts []migration.DraftSummary) []DraftDTO {
	result := make([]DraftDTO, 0, len(drafts))
	for _, d := range drafts {
		result = append(result, DraftDTO{ID: d.ID.String(), Title: d.Title, Revision: strconv.FormatInt(d.Revision, 10), Frozen: d.Frozen, UpdatedBy: d.UpdatedByUsername, UpdatedAt: &d.UpdatedAt})
	}
	return result
}

func (h *APIHandler) writeDraftAPI(w http.ResponseWriter, r *http.Request, s apiScope, d migration.Draft, status int) {
	items := draftDTOs([]migration.DraftSummary{d.DraftSummary})
	items[0].SQL = &d.SQL
	items[0].DownSQL = &d.DownSQL
	items[0].Irreversible = d.Irreversible
	if d.IrreversibleReason != "" {
		items[0].IrreversibleReason = &d.IrreversibleReason
	}
	items[0].Frozen = d.Frozen
	httpx.WriteJSON(w, r, status, struct {
		Database databaseDTO `json:"database"`
		Draft    DraftDTO    `json:"draft"`
	}{s.database, items[0]}, nil)
}

func (h *APIHandler) draftsAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	limit, offset, err := draftPagination(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	drafts, more, err := h.service.ListDrafts(r.Context(), s.principal, s.target, limit, offset)
	var items []DraftDTO
	if err == nil {
		items = draftDTOs(drafts)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO `json:"database"`
		Drafts   []DraftDTO  `json:"drafts"`
	}{s.database, items}, httpx.NewPage(limit, offset, more))
}

func (h *APIHandler) draftAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	_, err := httpx.ParseQuery(r)
	id, idErr := draftPathID(r, "draft_id")
	if err == nil {
		err = idErr
	}
	var d migration.Draft
	if err == nil {
		d, err = h.service.GetDraft(r.Context(), s.principal, s.target, id)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	h.writeDraftAPI(w, r, s, d, 200)
}

func (h *APIHandler) createDraftAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	var in struct {
		Title              string `json:"title"`
		SQL                string `json:"sql"`
		DownSQL            string `json:"down_sql"`
		Irreversible       bool   `json:"irreversible"`
		IrreversibleReason string `json:"irreversible_reason"`
	}
	err := httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"title", "sql"},
		[]string{"down_sql", "irreversible", "irreversible_reason"})
	var d migration.Draft
	if err == nil {
		d, err = h.service.CreateDraft(r.Context(), s.principal, s.target, migration.DraftInput{
			Title: in.Title, SQL: in.SQL, DownSQL: in.DownSQL, Irreversible: in.Irreversible,
			IrreversibleReason: in.IrreversibleReason,
		})
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/databases/"+s.target.String()+"/drafts/"+d.ID.String())
	h.writeDraftAPI(w, r, s, d, 201)
}

func (h *APIHandler) saveDraftAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	var in struct {
		Title              string `json:"title"`
		SQL                string `json:"sql"`
		DownSQL            string `json:"down_sql"`
		Irreversible       bool   `json:"irreversible"`
		IrreversibleReason string `json:"irreversible_reason"`
		Revision           string `json:"expected_revision"`
	}
	err := httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"title", "sql", "expected_revision"},
		[]string{"down_sql", "irreversible", "irreversible_reason"})
	id, idErr := draftPathID(r, "draft_id")
	if err == nil {
		err = idErr
	}
	var revision int64
	if err == nil {
		revision, err = httpx.Int64Value(url.Values{"expected_revision": {in.Revision}}, "expected_revision", 1, math.MaxInt64)
	}
	var d migration.Draft
	if err == nil {
		d, err = h.service.SaveDraft(r.Context(), s.principal, s.target, id, migration.SaveDraftInput{DraftInput: migration.DraftInput{
			Title: in.Title, SQL: in.SQL, DownSQL: in.DownSQL, Irreversible: in.Irreversible,
			IrreversibleReason: in.IrreversibleReason,
		}, ExpectedRevision: revision})
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	h.writeDraftAPI(w, r, s, d, 200)
}

func (h *APIHandler) reviewAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	id, err := draftPathID(r, "draft_id")
	values, queryErr := httpx.ParseQuery(r, "expected_revision", "request_key")
	if err == nil {
		err = queryErr
	}
	if err == nil && values.Has("expected_revision") != values.Has("request_key") {
		err = &problem.Error{Code: problem.InvalidRequest}
	}
	var input *legacy.ReserveInput
	if err == nil && len(values) != 0 {
		parsed, parseErr := reviewInput(values)
		input, err = &parsed, parseErr
	}
	var review migration.Review
	if err == nil {
		review, err = h.service.Review(r.Context(), s.principal, s.target, id, input)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	var resultReceipt *ReceiptDTO
	var draft *DraftDTO
	if review.Receipt != nil {
		value := apiReceipt(*review.Receipt)
		resultReceipt = &value
	} else {
		d := review.Draft
		draft = &DraftDTO{ID: id.String(), Title: d.Title, SQL: &d.SQL, Revision: strconv.FormatInt(d.Revision, 10)}
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database   databaseDTO `json:"database"`
		Draft      *DraftDTO   `json:"draft"`
		Revision   string      `json:"expected_revision"`
		RequestKey string      `json:"request_key"`
		Receipt    *ReceiptDTO `json:"receipt"`
	}{s.database, draft, strconv.FormatInt(review.Input.ExpectedRevision, 10), review.Input.RequestKey.String(), resultReceipt}, nil)
}

func (h *APIHandler) executeAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	if h.legacyExecuteRetired {
		httpx.WriteJSONError(w, r, &problem.Error{Code: problem.LegacyExecuteRetired})
		return
	}
	var in struct {
		Revision string `json:"expected_revision"`
		Key      string `json:"request_key"`
	}
	err := httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"expected_revision", "request_key"}, nil)
	id, idErr := draftPathID(r, "draft_id")
	if err == nil {
		err = idErr
	}
	var input legacy.ReserveInput
	if err == nil {
		input, err = reviewInput(url.Values{"expected_revision": {in.Revision}, "request_key": {in.Key}})
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	receipt, err := h.service.Execute(r.Context(), s.principal, s.target, id, input)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, apiReceipt(receipt), nil)
}

func (h *APIHandler) attemptAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	_, err := httpx.ParseQuery(r)
	id, idErr := draftPathID(r, "attempt_id")
	if err == nil {
		err = idErr
	}
	var a *legacy.AttemptDetail
	if err == nil {
		a, err = h.service.Attempt(r.Context(), s.principal, s.target, id)
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
		Attempt  AttemptDTO  `json:"attempt"`
	}{s.database, item}, nil)
}

func (h *APIHandler) receiptAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	_, err := httpx.ParseQuery(r)
	key, keyErr := draftPathID(r, "request_key")
	if err == nil {
		err = keyErr
	}
	var receipt *legacy.AttemptReceipt
	if err == nil {
		receipt, err = h.service.Receipt(r.Context(), s.principal, s.target, key)
		if err == nil && receipt == nil {
			err = &problem.Error{Code: problem.AttemptNotFound}
		}
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, apiReceipt(*receipt), nil)
}
