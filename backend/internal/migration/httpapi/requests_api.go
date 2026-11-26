package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

type requestGroupDTO struct {
	GroupID        string       `json:"group_id"`
	GroupName      string       `json:"group_name"`
	Access         access.Level `json:"access"`
	LeaderID       string       `json:"leader_id"`
	LeaderUsername string       `json:"leader_username"`
	Decision       *decisionDTO `json:"decision,omitempty"`
	Eligible       bool         `json:"eligible"`
}

type decisionDTO struct {
	Actor     string    `json:"actor"`
	LeaderID  string    `json:"leader_id"`
	CreatedAt time.Time `json:"created_at"`
}

type RequestDTO struct {
	ID                 string                 `json:"id"`
	DraftID            string                 `json:"draft_id"`
	DefinitionID       string                 `json:"definition_id"`
	Title              string                 `json:"title"`
	ActorID            string                 `json:"actor_id"`
	Actor              string                 `json:"actor"`
	Revision           string                 `json:"revision"`
	Direction          string                 `json:"direction"`
	Mode               string                 `json:"mode"`
	State              string                 `json:"state"`
	StaleReason        string                 `json:"stale_reason,omitempty"`
	CreatedAt          time.Time              `json:"created_at"`
	UpdatedAt          time.Time              `json:"updated_at"`
	SQL                *string                `json:"sql,omitempty"`
	DownSQL            *string                `json:"down_sql,omitempty"`
	Irreversible       bool                   `json:"irreversible"`
	IrreversibleReason string                 `json:"irreversible_reason,omitempty"`
	ConfigRevision     string                 `json:"config_revision"`
	PolicyGeneration   string                 `json:"policy_generation"`
	HeadGeneration     string                 `json:"head_generation"`
	ExpectedHeadID     *string                `json:"expected_head_id"`
	Groups             []requestGroupDTO      `json:"groups,omitempty"`
	EligibleGroupIDs   []string               `json:"eligible_group_ids,omitempty"`
	ExecutionID        *string                `json:"execution_id"`
	JobID              *string                `json:"job_id"`
	ConsumedAt         *time.Time             `json:"consumed_at"`
	Batch              *sqlpolicy.BatchConfig `json:"batch,omitempty"`
}

func requestSummaryDTO(request migration.RequestSummary) RequestDTO {
	return RequestDTO{
		ID: request.ID.String(), DraftID: request.DraftID.String(), DefinitionID: request.DefinitionID.String(),
		Title: request.Title, ActorID: request.ActorID.String(), Actor: request.ActorUsername,
		Revision:  strconv.FormatInt(request.Revision, 10),
		Direction: request.Direction, Mode: request.Mode, State: request.State, StaleReason: request.StaleReason,
		CreatedAt: request.CreatedAt, UpdatedAt: request.UpdatedAt,
	}
}

func requestDetailDTO(request migration.ReviewRequest) RequestDTO {
	result := requestSummaryDTO(request.RequestSummary)
	result.SQL, result.DownSQL = &request.SQL, &request.DownSQL
	result.Irreversible = request.Irreversible
	result.IrreversibleReason = request.IrreversibleReason
	result.ConfigRevision = strconv.Itoa(request.ConfigRevision)
	result.PolicyGeneration = strconv.FormatInt(request.PolicyGeneration, 10)
	result.HeadGeneration = strconv.FormatInt(request.HeadGeneration, 10)
	if request.ExpectedHeadID != nil {
		value := request.ExpectedHeadID.String()
		result.ExpectedHeadID = &value
	}
	result.Groups = make([]requestGroupDTO, 0, len(request.Groups))
	for _, group := range request.Groups {
		item := requestGroupDTO{GroupID: group.GroupID.String(), GroupName: group.GroupName, Access: group.Access,
			LeaderID: group.LeaderID.String(), LeaderUsername: group.LeaderUsername, Eligible: group.Eligible}
		if group.Decision != nil {
			item.Decision = &decisionDTO{Actor: group.Decision.ActorUsername, LeaderID: group.Decision.LeaderID.String(),
				CreatedAt: group.Decision.CreatedAt}
		}
		result.Groups = append(result.Groups, item)
	}
	result.EligibleGroupIDs = make([]string, 0, len(request.EligibleGroupIDs))
	for _, id := range request.EligibleGroupIDs {
		result.EligibleGroupIDs = append(result.EligibleGroupIDs, id.String())
	}
	if request.ExecutionID != nil {
		value := request.ExecutionID.String()
		result.ExecutionID = &value
	}
	if request.JobID != nil {
		value := strconv.FormatInt(*request.JobID, 10)
		result.JobID = &value
	}
	result.ConsumedAt = request.ConsumedAt
	result.Batch = request.Batch
	return result
}

func (h *APIHandler) submitRequestAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	draftID, err := draftPathID(r, "draft_id")
	var in struct {
		Revision   string                 `json:"expected_revision"`
		RequestKey string                 `json:"request_key"`
		Batch      *sqlpolicy.BatchConfig `json:"batch"`
	}
	if err == nil {
		err = httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"expected_revision", "request_key"}, []string{"batch"})
	}
	var revision int64
	if err == nil {
		revision, err = httpx.Int64Value(mapToValues("expected_revision", in.Revision), "expected_revision", 1, 1<<62)
	}
	var key uuid.UUID
	if err == nil {
		key, err = uuid.Parse(in.RequestKey)
		if err != nil {
			err = &problem.Error{Code: problem.InvalidRequest}
		}
	}
	var request migration.ReviewRequest
	if err == nil {
		if in.Batch != nil {
			request, err = h.service.SubmitBatchRequest(r.Context(), s.principal, s.target, draftID, revision, key, *in.Batch)
		} else {
			request, err = h.service.SubmitRequest(r.Context(), s.principal, s.target, draftID, revision, key)
		}
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/databases/"+s.target.String()+"/requests/"+request.ID.String())
	httpx.WriteJSON(w, r, 201, struct {
		Database databaseDTO `json:"database"`
		Request  RequestDTO  `json:"request"`
	}{s.database, requestDetailDTO(request)}, nil)
}

func (h *APIHandler) requestsAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	limit, offset, err := draftPagination(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	requests, more, err := h.service.ListRequests(r.Context(), s.principal, s.target, limit, offset)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items := make([]RequestDTO, 0, len(requests))
	for _, request := range requests {
		items = append(items, requestSummaryDTO(request))
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO  `json:"database"`
		Requests []RequestDTO `json:"requests"`
	}{s.database, items}, httpx.NewPage(limit, offset, more))
}

func (h *APIHandler) requestAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	requestID, err := draftPathID(r, "request_id")
	var request migration.ReviewRequest
	if err == nil {
		request, err = h.service.GetRequest(r.Context(), s.principal, s.target, requestID)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO `json:"database"`
		Request  RequestDTO  `json:"request"`
	}{s.database, requestDetailDTO(request)}, nil)
}

func (h *APIHandler) approveRequestAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	requestID, err := draftPathID(r, "request_id")
	var in struct {
		GroupID string `json:"group_id"`
	}
	if err == nil {
		err = httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"group_id"}, nil)
	}
	var groupID uuid.UUID
	if err == nil {
		groupID, err = uuid.Parse(in.GroupID)
		if err != nil {
			err = &problem.Error{Code: problem.InvalidRequest}
		}
	}
	var request migration.ReviewRequest
	if err == nil {
		request, err = h.service.ApproveRequest(r.Context(), s.principal, s.target, requestID, groupID)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO `json:"database"`
		Request  RequestDTO  `json:"request"`
	}{s.database, requestDetailDTO(request)}, nil)
}

func (h *APIHandler) submitRollbackRequestAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	h.submitHeadMotionAPI(w, r, s, execstore.DirectionRollback)
}

func (h *APIHandler) submitForwardRequestAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	h.submitHeadMotionAPI(w, r, s, execstore.DirectionForward)
}

func (h *APIHandler) submitHeadMotionAPI(w http.ResponseWriter, r *http.Request, s apiScope, direction string) {
	definitionID, err := draftPathID(r, "definition_id")
	var in struct {
		SourceExecutionID string `json:"source_execution_id"`
		ExpectedRevision  string `json:"expected_revision"`
		RequestKey        string `json:"request_key"`
	}
	if err == nil {
		err = httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in,
			[]string{"source_execution_id", "expected_revision", "request_key"}, nil)
	}
	var sourceID, key uuid.UUID
	var expectedHeadGen int64
	if err == nil {
		sourceID, err = uuid.Parse(in.SourceExecutionID)
		if err != nil {
			err = &problem.Error{Code: problem.InvalidRequest}
		}
	}
	if err == nil {
		expectedHeadGen, err = httpx.Int64Value(mapToValues("expected_revision", in.ExpectedRevision), "expected_revision", 1, 1<<62)
	}
	if err == nil {
		key, err = uuid.Parse(in.RequestKey)
		if err != nil {
			err = &problem.Error{Code: problem.InvalidRequest}
		}
	}
	var request migration.ReviewRequest
	if err == nil {
		if direction == execstore.DirectionRollback {
			request, err = h.service.SubmitRollbackRequest(r.Context(), s.principal, s.target, definitionID, sourceID, expectedHeadGen, key)
		} else {
			request, err = h.service.SubmitForwardRequest(r.Context(), s.principal, s.target, definitionID, sourceID, expectedHeadGen, key)
		}
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/databases/"+s.target.String()+"/requests/"+request.ID.String())
	httpx.WriteJSON(w, r, 201, struct {
		Database databaseDTO `json:"database"`
		Request  RequestDTO  `json:"request"`
	}{s.database, requestDetailDTO(request)}, nil)
}

func (h *APIHandler) submitResumeRequestAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	h.submitRecoveryRequestAPI(w, r, s, execstore.DirectionResume)
}

func (h *APIHandler) submitCompensationRequestAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	h.submitRecoveryRequestAPI(w, r, s, execstore.DirectionCompensate)
}

func (h *APIHandler) submitRecoveryRequestAPI(w http.ResponseWriter, r *http.Request, s apiScope, direction string) {
	executionID, err := draftPathID(r, "execution_id")
	var in struct {
		RequestKey string `json:"request_key"`
	}
	if err == nil {
		err = httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"request_key"}, nil)
	}
	var key uuid.UUID
	if err == nil {
		key, err = uuid.Parse(in.RequestKey)
		if err != nil {
			err = &problem.Error{Code: problem.InvalidRequest}
		}
	}
	var request migration.ReviewRequest
	if err == nil {
		request, err = h.service.SubmitRecoveryRequest(r.Context(), s.principal, s.target, executionID, key, direction)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/databases/"+s.target.String()+"/requests/"+request.ID.String())
	httpx.WriteJSON(w, r, 201, struct {
		Database databaseDTO `json:"database"`
		Request  RequestDTO  `json:"request"`
	}{s.database, requestDetailDTO(request)}, nil)
}

func mapToValues(key, value string) map[string][]string {
	return map[string][]string{key: {value}}
}

func (h *APIHandler) executeRequestAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	requestID, err := draftPathID(r, "request_id")
	var in struct {
		RequestKey string `json:"request_key"`
	}
	if err == nil {
		err = httpx.DecodeJSON(w, r, httpx.MaxFormBody, &in, []string{"request_key"}, nil)
	}
	var key uuid.UUID
	if err == nil {
		key, err = uuid.Parse(in.RequestKey)
		if err != nil {
			err = &problem.Error{Code: problem.InvalidRequest}
		}
	}
	var admission migration.Admission
	if err == nil {
		admission, err = h.service.AcceptRequest(r.Context(), s.principal, s.target, requestID, key)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/databases/"+s.target.String()+"/executions/"+admission.ExecutionID.String())
	httpx.WriteJSON(w, r, 202, struct {
		RequestID   string `json:"request_id"`
		ExecutionID string `json:"execution_id"`
		JobID       string `json:"job_id"`
		State       string `json:"state"`
		Replayed    bool   `json:"replayed"`
	}{admission.RequestID.String(), admission.ExecutionID.String(), strconv.FormatInt(admission.JobID, 10), "queued", admission.Replayed}, nil)
}
