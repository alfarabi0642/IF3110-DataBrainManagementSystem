package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

type CommittedEventDTO struct {
	ID                  string    `json:"id"`
	ExecutionID         string    `json:"execution_id"`
	DefinitionID        string    `json:"definition_id"`
	Title               string    `json:"title"`
	Actor               string    `json:"actor"`
	Revision            string    `json:"revision"`
	Direction           string    `json:"direction"`
	Mode                string    `json:"mode"`
	Status              string    `json:"status"`
	PlanHash            string    `json:"plan_hash"`
	ParentExecutionID   *string   `json:"parent_execution_id"`
	CanceledExecutionID *string   `json:"canceled_execution_id"`
	Generation          string    `json:"generation"`
	CommittedAt         time.Time `json:"committed_at"`
	SQL                 *string   `json:"sql,omitempty"`
	DownSQL             *string   `json:"down_sql,omitempty"`
	Irreversible        bool      `json:"irreversible"`
	IrreversibleReason  string    `json:"irreversible_reason,omitempty"`
}

func eventSummaryDTO(event migration.CommittedEventSummary) CommittedEventDTO {
	return CommittedEventDTO{
		ID: event.ID.String(), ExecutionID: event.ExecutionID.String(), DefinitionID: event.DefinitionID.String(),
		Title: event.Title, Actor: event.ActorUsername, Revision: strconv.FormatInt(event.Revision, 10),
		Direction: event.Direction, Mode: event.Mode, Status: event.Status, PlanHash: event.PlanHash,
		ParentExecutionID: uuidPointer(event.ParentExecutionID), CanceledExecutionID: uuidPointer(event.CanceledExecutionID),
		Generation: strconv.FormatInt(event.Generation, 10), CommittedAt: event.CommittedAt,
	}
}

func eventDetailDTO(event *migration.CommittedEventDetail) CommittedEventDTO {
	result := eventSummaryDTO(event.CommittedEventSummary)
	result.SQL, result.DownSQL = &event.SQL, &event.DownSQL
	result.Irreversible = event.Irreversible
	result.IrreversibleReason = event.IrreversibleReason
	return result
}

func uuidPointer(value *uuid.UUID) *string {
	if value == nil {
		return nil
	}
	s := value.String()
	return &s
}

type ActivityDTO struct {
	SourceKind    string    `json:"source_kind"`
	ID            string    `json:"id"`
	ExecutionID   string    `json:"execution_id,omitempty"`
	AttemptID     string    `json:"attempt_id,omitempty"`
	AttemptNumber *int      `json:"attempt_number,omitempty"`
	Title         string    `json:"title"`
	Actor         string    `json:"actor"`
	State         string    `json:"state"`
	Revision      string    `json:"revision"`
	CreatedAt     time.Time `json:"created_at"`
}

func activityItemDTO(item migration.ActivityItem) ActivityDTO {
	return ActivityDTO{SourceKind: item.SourceKind, ID: item.ID, ExecutionID: item.ExecutionID,
		AttemptID: item.AttemptID, AttemptNumber: item.AttemptNumber, Title: item.Title, Actor: item.Actor,
		State: item.State, Revision: strconv.FormatInt(item.Revision, 10), CreatedAt: item.CreatedAt}
}

func historyPageParams(r *http.Request) (int, int, *bool, error) {
	values, err := httpx.ParseQuery(r, "limit", "offset", "hide_rollback")
	if err != nil {
		return 0, 0, nil, err
	}
	limit, offset := int64(50), int64(0)
	if values.Has("limit") {
		limit, err = httpx.Int64Value(values, "limit", 1, 200)
	}
	if err == nil && values.Has("offset") {
		offset, err = httpx.Int64Value(values, "offset", 0, 100000)
	}
	var hide *bool
	if err == nil && values.Has("hide_rollback") {
		value := values.Get("hide_rollback")
		if value != "true" && value != "false" {
			return 0, 0, nil, &problem.Error{Code: problem.InvalidRequest}
		}
		parsed := value == "true"
		hide = &parsed
	}
	return int(limit), int(offset), hide, err
}

type headExecutionDTO struct {
	ExecutionID        string `json:"execution_id"`
	DefinitionID       string `json:"definition_id"`
	Title              string `json:"title"`
	Revision           string `json:"revision"`
	Irreversible       bool   `json:"irreversible"`
	IrreversibleReason string `json:"irreversible_reason,omitempty"`
	CanRollback        bool   `json:"can_rollback"`
}

type forwardCandidateDTO struct {
	SourceExecutionID string `json:"source_execution_id"`
	DefinitionID      string `json:"definition_id"`
	Title             string `json:"title"`
	Revision          string `json:"revision"`
}

type HeadStateDTO struct {
	Initialized    bool                 `json:"initialized"`
	HeadGeneration string               `json:"head_generation"`
	RedoDepth      int                  `json:"redo_depth"`
	Head           *headExecutionDTO    `json:"head"`
	NextForward    *forwardCandidateDTO `json:"next_forward"`
}

func apiHeadState(st migration.TargetHeadState) *HeadStateDTO {
	out := &HeadStateDTO{
		Initialized:    st.Initialized,
		HeadGeneration: strconv.FormatInt(st.HeadGeneration, 10),
		RedoDepth:      st.RedoDepth,
	}
	if st.Head != nil {
		out.Head = &headExecutionDTO{
			ExecutionID:        st.Head.ExecutionID.String(),
			DefinitionID:       st.Head.DefinitionID.String(),
			Title:              st.Head.Title,
			Revision:           strconv.FormatInt(st.Head.Revision, 10),
			Irreversible:       st.Head.Irreversible,
			IrreversibleReason: st.Head.IrreversibleReason,
			CanRollback:        st.Head.CanRollback,
		}
	}
	if st.NextForward != nil {
		out.NextForward = &forwardCandidateDTO{
			SourceExecutionID: st.NextForward.SourceExecutionID.String(),
			DefinitionID:      st.NextForward.DefinitionID.String(),
			Title:             st.NextForward.Title,
			Revision:          strconv.FormatInt(st.NextForward.Revision, 10),
		}
	}
	return out
}

func (h *APIHandler) historyAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	limit, offset, hide, err := historyPageParams(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	if hide == nil {
		items, more, err := h.service.History(r.Context(), s.principal, s.target, limit, offset)
		if err != nil {
			httpx.WriteJSONError(w, r, err)
			return
		}
		attempts := make([]AttemptDTO, 0, len(items))
		for _, item := range items {
			attempts = append(attempts, apiAttempt(item))
		}
		httpx.WriteJSON(w, r, 200, struct {
			Database databaseDTO  `json:"database"`
			Attempts []AttemptDTO `json:"attempts"`
		}{s.database, attempts}, httpx.NewPage(limit, offset, more))
		return
	}
	events, more, err := h.service.CommittedHistory(r.Context(), s.principal, s.target, limit, offset, *hide)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	headState, err := h.service.TargetHead(r.Context(), s.principal, s.target)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items := make([]CommittedEventDTO, 0, len(events))
	for _, event := range events {
		items = append(items, eventSummaryDTO(event))
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database  databaseDTO         `json:"database"`
		Events    []CommittedEventDTO `json:"events"`
		HeadState *HeadStateDTO       `json:"head_state,omitempty"`
	}{s.database, items, apiHeadState(headState)}, httpx.NewPage(limit, offset, more))
}

func (h *APIHandler) committedEventAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	_, err := httpx.ParseQuery(r)
	id, idErr := draftPathID(r, "execution_id")
	if err == nil {
		err = idErr
	}
	var event *migration.CommittedEventDetail
	if err == nil {
		event, err = h.service.CommittedEvent(r.Context(), s.principal, s.target, id)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	if event == nil {
		httpx.WriteJSONError(w, r, &problem.Error{Code: problem.ExecutionNotFound})
		return
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO       `json:"database"`
		Event    CommittedEventDTO `json:"event"`
	}{s.database, eventDetailDTO(event)}, nil)
}

func (h *APIHandler) activityAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	values, err := httpx.ParseQuery(r, "limit", "offset")
	var limit, offset int64 = 50, 0
	if err == nil && values.Has("limit") {
		limit, err = httpx.Int64Value(values, "limit", 1, 200)
	}
	if err == nil && values.Has("offset") {
		offset, err = httpx.Int64Value(values, "offset", 0, 100000)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items, more, err := h.service.Activity(r.Context(), s.principal, s.target, int(limit), int(offset))
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	activity := make([]ActivityDTO, 0, len(items))
	for _, item := range items {
		activity = append(activity, activityItemDTO(item))
	}
	httpx.WriteJSON(w, r, 200, struct {
		Database databaseDTO   `json:"database"`
		Activity []ActivityDTO `json:"activity"`
	}{s.database, activity}, httpx.NewPage(int(limit), int(offset), more))
}
