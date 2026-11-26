package migration_test

import (
	"context"
	"testing"
	"time"

	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/httpapi"
	"github.com/google/uuid"
)

// TestDomainAcceptancePostgres exercises the repeatable domain approval and admission path:
// editor submit, view-only and edit leaders vote, initiating editor admits one queued execution/job,
// viewers read status/history/activity, and every outsider denial leaves target SQL untouched.
func TestDomainAcceptancePostgres(t *testing.T) {
	f, web := newWorkflowFixture(t, "http://localhost:18089")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var viewGroup uuid.UUID
	if err := f.store.Pool().QueryRow(ctx, `SELECT id FROM databrain.groups WHERE name='Runner viewers'`).Scan(&viewGroup); err != nil {
		t.Fatal(err)
	}
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=$2 WHERE id=$1`, f.editGroup, f.actor)
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=$2 WHERE id=$1`, viewGroup, f.viewer.UserID)
	draft, err := f.store.Create(ctx, f.a, f.actor, DraftInput{
		Title: "Acceptance migration", SQL: "CREATE TABLE public.acceptance_once(id int)",
		DownSQL: "DROP TABLE public.acceptance_once",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/databases/" + f.a.String()
	submitted := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "POST", base+"/drafts/"+draft.ID.String()+"/submit", web.editor,
		jsonBody(t, map[string]string{"expected_revision": "1", "request_key": uuid.NewString()}), 201)).Request
	if submitted.State != "pending_review" || len(submitted.Groups) != 2 || submitted.SQL == nil ||
		*submitted.SQL != "CREATE TABLE public.acceptance_once(id int)" {
		t.Fatalf("submission acceptance wrong: %+v", submitted)
	}
	// Unsubmitted/edit-right and outsider boundaries are enforced from the session, not the body.
	web.jsonRequest(t, "POST", base+"/drafts/"+draft.ID.String()+"/submit", web.viewer,
		jsonBody(t, map[string]string{"expected_revision": "1", "request_key": uuid.NewString()}), 403)
	if _, err := f.store.Save(ctx, f.a, draft.ID, f.otherActor, SaveDraftInput{DraftInput: DraftInput{Title: "edited", SQL: "SELECT 1"}, ExpectedRevision: 1}); err == nil {
		t.Fatal("submitted definition remained editable")
	}
	viewerDetail := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "GET", base+"/requests/"+submitted.ID, web.viewer, "", 200)).Request
	if len(viewerDetail.EligibleGroupIDs) != 1 || viewerDetail.EligibleGroupIDs[0] != viewGroup.String() {
		t.Fatalf("viewer leader eligibility wrong: %+v", viewerDetail)
	}
	web.jsonRequest(t, "POST", base+"/requests/"+submitted.ID+"/approve", web.otherEditor,
		jsonBody(t, map[string]string{"group_id": f.editGroup.String()}), 403)
	web.jsonRequest(t, "POST", base+"/requests/"+submitted.ID+"/approve", web.admin,
		jsonBody(t, map[string]string{"group_id": f.editGroup.String()}), 404)
	web.jsonRequest(t, "POST", base+"/requests/"+submitted.ID+"/approve", web.viewer,
		jsonBody(t, map[string]string{"group_id": viewGroup.String()}), 200)
	pending := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "GET", base+"/requests/"+submitted.ID, web.viewer, "", 200)).Request
	if pending.State != "pending_review" {
		t.Fatalf("single vote approved early: %+v", pending)
	}
	web.jsonRequest(t, "POST", base+"/requests/"+submitted.ID+"/approve", web.editor,
		jsonBody(t, map[string]string{"group_id": f.editGroup.String()}), 200)
	approved := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "GET", base+"/requests/"+submitted.ID, web.viewer, "", 200)).Request
	if approved.State != "approved" {
		t.Fatalf("two votes did not approve: %+v", approved)
	}
	executeKey := uuid.NewString()
	web.jsonRequest(t, "POST", base+"/requests/"+submitted.ID+"/execute", web.viewer,
		jsonBody(t, map[string]string{"request_key": executeKey}), 403)
	web.jsonRequest(t, "POST", base+"/requests/"+submitted.ID+"/execute", web.otherEditor,
		jsonBody(t, map[string]string{"request_key": executeKey}), 403)
	admission := apiData[struct {
		ExecutionID string `json:"execution_id"`
		JobID       string `json:"job_id"`
		State       string `json:"state"`
	}](t, web.jsonRequest(t, "POST", base+"/requests/"+submitted.ID+"/execute", web.editor,
		jsonBody(t, map[string]string{"request_key": executeKey}), 202))
	if admission.ExecutionID == "" || admission.JobID == "" || admission.State != "queued" {
		t.Fatalf("admission response wrong: %+v", admission)
	}
	web.jsonRequest(t, "POST", base+"/requests/"+submitted.ID+"/execute", web.editor,
		jsonBody(t, map[string]string{"request_key": executeKey}), 202)
	execution := apiData[struct {
		Phase string `json:"phase"`
	}](t, web.jsonRequest(t, "GET", base+"/executions/"+admission.ExecutionID, web.viewer, "", 200))
	if execution.Phase != "queued" {
		t.Fatalf("execution status wrong: %+v", execution)
	}
	consumed := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "GET", base+"/requests/"+submitted.ID, web.viewer, "", 200)).Request
	if consumed.State != "consumed" || consumed.ExecutionID == nil || *consumed.ExecutionID != admission.ExecutionID || consumed.JobID == nil {
		t.Fatalf("consumed request binding wrong: %+v", consumed)
	}
	history := apiData[struct {
		Events []httpapi.CommittedEventDTO `json:"events"`
	}](t, web.jsonRequest(t, "GET", base+"/history?hide_rollback=true", web.viewer, "", 200))
	if len(history.Events) != 0 {
		t.Fatalf("unfinalized execution entered committed history: %+v", history)
	}
	activity := apiData[struct {
		Activity []httpapi.ActivityDTO `json:"activity"`
	}](t, web.jsonRequest(t, "GET", base+"/activity?limit=50", web.viewer, "", 200))
	var sawExecution bool
	for _, item := range activity.Activity {
		if item.SourceKind == "execution" && item.ExecutionID == admission.ExecutionID {
			sawExecution = true
		}
	}
	if !sawExecution {
		t.Fatalf("queued execution missing from activity: %+v", activity)
	}
	var marker string
	if err := f.target.QueryRow(ctx, `SELECT value FROM public.draft_marker`).Scan(&marker); err != nil || marker != "unchanged 猫" {
		t.Fatalf("acceptance path executed target SQL: %q %v", marker, err)
	}
}
