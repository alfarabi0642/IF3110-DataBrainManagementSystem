package httpapi

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

type executionActionDTO struct {
	Action     string `json:"action"`
	Enabled    bool   `json:"enabled"`
	ReasonCode string `json:"reason_code"`
}

type executionFailureDTO struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	SQLState string `json:"sqlstate"`
}

// executionDTO adalah kontrak baca yang sama untuk detail target-scoped dan lookup job. SQL, request
// key, credential, sebelum-image, dan bukti privat tidak termasuk.
type ExecutionDTO struct {
	ID                    string               `json:"id"`
	TargetID              string               `json:"target_id"`
	Direction             string               `json:"direction"`
	Mode                  string               `json:"mode"`
	Phase                 string               `json:"phase"`
	Outcome               *string              `json:"outcome"`
	CommittedRows         string               `json:"committed_rows"`
	CommittedBatches      string               `json:"committed_batches"`
	TotalRows             *string              `json:"total_rows"`
	HasPartialCommits     bool                 `json:"has_partial_commits"`
	ReconciliationPending bool                 `json:"reconciliation_pending"`
	CreatedAt             time.Time            `json:"created_at"`
	StartedAt             *time.Time           `json:"started_at"`
	FinishedAt            *time.Time           `json:"finished_at"`
	Failure               *executionFailureDTO `json:"failure,omitempty"`
	Actions               []executionActionDTO `json:"actions"`
}

func apiExecution(view execstore.ExecutionView) ExecutionDTO {
	actions := []executionActionDTO{}
	if view.Mode == execstore.ModeBatch && view.Phase == "blocked" && view.Outcome != nil {
		switch *view.Outcome {
		case "partial":
			actions = []executionActionDTO{
				{Action: "resume", Enabled: true, ReasonCode: ""},
				{Action: "compensate", Enabled: true, ReasonCode: ""},
			}
		case "partially_reverted":
			actions = []executionActionDTO{
				{Action: "resume", Enabled: false, ReasonCode: "COMPENSATION_IN_PROGRESS"},
				{Action: "compensate", Enabled: true, ReasonCode: ""},
			}
		}
	}
	result := ExecutionDTO{
		ID: view.ID.String(), TargetID: view.TargetID.String(), Direction: view.Direction,
		Mode: view.Mode, Phase: view.Phase, Outcome: view.Outcome,
		CommittedRows:         strconv.FormatInt(view.CommittedRows, 10),
		CommittedBatches:      strconv.FormatInt(view.CommittedBatches, 10),
		HasPartialCommits:     view.HasPartialCommits,
		ReconciliationPending: view.ReconciliationPending,
		CreatedAt:             view.CreatedAt, StartedAt: view.StartedAt, FinishedAt: view.FinishedAt,
		Actions: actions,
	}
	if view.TotalRows != nil {
		total := strconv.FormatInt(*view.TotalRows, 10)
		result.TotalRows = &total
	}
	if view.Failure != nil {
		result.Failure = &executionFailureDTO{Code: view.Failure.Code, Message: view.Failure.Message, SQLState: view.Failure.SQLState}
	}
	return result
}

type ExecutionBatchDTO struct {
	BatchOrdinal      string    `json:"batch_ordinal"`
	Direction         string    `json:"direction"`
	ActionExecutionID string    `json:"action_execution_id"`
	RowsAffected      string    `json:"rows_affected"`
	CursorEnd         *string   `json:"cursor_end"`
	CommittedAt       time.Time `json:"committed_at"`
}

func (h *APIHandler) executionAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	_, err := httpx.ParseQuery(r)
	executionID, idErr := draftPathID(r, "execution_id")
	if err == nil {
		err = idErr
	}
	var view *execstore.ExecutionView
	if err == nil {
		view, err = h.service.Execution(r.Context(), s.principal, s.target, executionID)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, apiExecution(*view), nil)
}

func (h *APIHandler) executionBatchesAPI(w http.ResponseWriter, r *http.Request, s apiScope) {
	limit, offset, err := draftPagination(r)
	executionID, idErr := draftPathID(r, "execution_id")
	if err == nil {
		err = idErr
	}
	var batches []migration.ExecutionBatchSummary
	var more bool
	if err == nil {
		batches, more, err = h.service.ListExecutionBatches(r.Context(), s.principal, s.target, executionID, limit, offset)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items := make([]ExecutionBatchDTO, 0, len(batches))
	for _, b := range batches {
		items = append(items, ExecutionBatchDTO{
			BatchOrdinal:      strconv.Itoa(b.BatchOrdinal),
			Direction:         b.Direction,
			ActionExecutionID: b.ActionExecutionID.String(),
			RowsAffected:      strconv.FormatInt(b.RowsAffected, 10),
			CursorEnd:         b.CursorEnd,
			CommittedAt:       b.CommittedAt,
		})
	}
	httpx.WriteJSON(w, r, http.StatusOK, struct {
		Database databaseDTO         `json:"database"`
		Batches  []ExecutionBatchDTO `json:"batches"`
	}{s.database, items}, httpx.NewPage(limit, offset, more))
}

// jobAPI tidak memiliki database di path; target diresolusi server-side lalu diotorisasi. Job yang
// tidak ada atau target tanpa akses memakai not-found terkonceal.
func (h *APIHandler) jobAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	_, err := httpx.ParseQuery(r)
	var jobID int64
	if err == nil {
		jobID, err = httpx.Int64Value(url.Values{"job_id": {r.PathValue("job_id")}}, "job_id", 1, math.MaxInt64)
	}
	principal, ok := access.FromContext(ctx)
	if err == nil && !ok {
		err = &problem.Error{Code: problem.SessionInvalid}
	}
	var view *execstore.ExecutionView
	if err == nil {
		view, err = h.service.ExecutionByJob(ctx, principal, jobID)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, apiExecution(*view), nil)
}
