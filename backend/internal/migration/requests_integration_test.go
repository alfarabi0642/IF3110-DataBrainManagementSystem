package migration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/httpapi"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func reviewFixture(t *testing.T) (runnerFixture, draftHTTPFixture, *Service, uuid.UUID) {
	t.Helper()
	f, web := newWorkflowFixture(t, "http://localhost:18087")
	var viewGroup uuid.UUID
	if err := f.store.Pool().QueryRow(context.Background(), `SELECT id FROM databrain.groups WHERE name='Runner viewers'`).Scan(&viewGroup); err != nil {
		t.Fatal(err)
	}
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=$2 WHERE id=$1`, f.editGroup, f.actor)
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=$2 WHERE id=$1`, viewGroup, f.viewer.UserID)
	domain, err := NewService(f.store, f.runner, f.guard)
	if err != nil {
		t.Fatal(err)
	}
	return f, web, domain, viewGroup
}

func reviewDraft(t *testing.T, f runnerFixture, title string) Draft {
	t.Helper()
	d, err := f.store.Create(context.Background(), f.a, f.actor, DraftInput{
		Title: title, SQL: "CREATE TABLE public.review_once(id int)",
		DownSQL: "DROP TABLE public.review_once",
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func groupInRequest(t *testing.T, request ReviewRequest, groupID uuid.UUID) RequestGroup {
	t.Helper()
	for _, group := range request.Groups {
		if group.GroupID == groupID {
			return group
		}
	}
	t.Fatalf("group %s missing from request", groupID)
	return RequestGroup{}
}

func TestReviewSubmitAndApprovePostgres(t *testing.T) {
	f, _, domain, viewGroup := reviewFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	draft := reviewDraft(t, f, "Review draft")
	key := uuid.New()
	request, err := domain.SubmitRequest(ctx, f.editor, f.a, draft.ID, 1, key)
	if err != nil {
		t.Fatal(err)
	}
	if request.State != "pending_review" || request.SQL != draft.SQL || request.DownSQL != draft.DownSQL ||
		request.Irreversible || len(request.Groups) != 2 || request.ExpectedHeadID != nil || request.HeadGeneration != 1 {
		t.Fatalf("submitted request is wrong: %+v", request)
	}
	if groupInRequest(t, request, f.editGroup).LeaderID != f.actor || groupInRequest(t, request, viewGroup).LeaderID != f.viewer.UserID {
		t.Fatal("required leader snapshot wrong")
	}
	read, err := f.store.Get(ctx, f.a, draft.ID)
	if err != nil || !read.Frozen {
		t.Fatalf("submission did not freeze definition: %+v %v", read, err)
	}
	if _, err := f.store.Save(ctx, f.a, draft.ID, f.otherActor, SaveDraftInput{DraftInput: DraftInput{Title: "changed", SQL: "SELECT 1"}, ExpectedRevision: 1}); err == nil {
		t.Fatal("submitted draft was editable")
	} else {
		RequireDraftCode(t, err, problem.DraftFrozen)
	}
	replay, err := domain.SubmitRequest(ctx, f.editor, f.a, draft.ID, 1, key)
	if err != nil || replay.ID != request.ID {
		t.Fatalf("identical submit did not replay: %+v %v", replay, err)
	}
	otherDraft := reviewDraft(t, f, "Other review draft")
	_, err = domain.SubmitRequest(ctx, f.editor, f.a, otherDraft.ID, 1, key)
	RequireDraftCode(t, err, problem.IdempotencyConflict)
	_, err = domain.SubmitRequest(ctx, f.viewer, f.a, otherDraft.ID, 1, uuid.New())
	RequireDraftCode(t, err, problem.DatabaseAccessDenied)

	viewerRequest, err := domain.GetRequest(ctx, f.viewer, f.a, request.ID)
	if err != nil || len(viewerRequest.EligibleGroupIDs) != 1 || viewerRequest.EligibleGroupIDs[0] != viewGroup {
		t.Fatalf("viewer eligibility wrong: %+v %v", viewerRequest, err)
	}
	afterViewer, err := domain.ApproveRequest(ctx, f.viewer, f.a, request.ID, viewGroup)
	if err != nil || afterViewer.State != "pending_review" || groupInRequest(t, afterViewer, viewGroup).Decision == nil {
		t.Fatalf("first vote wrong: %+v %v", afterViewer, err)
	}
	afterEditor, err := domain.ApproveRequest(ctx, f.editor, f.a, request.ID, f.editGroup)
	if err != nil || afterEditor.State != "approved" || groupInRequest(t, afterEditor, f.editGroup).Decision == nil {
		t.Fatalf("final vote wrong: %+v %v", afterEditor, err)
	}
	if _, err := domain.ApproveRequest(ctx, f.viewer, f.a, request.ID, viewGroup); err != nil {
		t.Fatalf("identical vote was not idempotent: %v", err)
	}
	_, err = domain.ApproveRequest(ctx, f.otherEditor, f.a, request.ID, f.editGroup)
	RequireDraftCode(t, err, problem.DatabaseAccessDenied)
	_, err = domain.GetRequest(ctx, f.editor, f.b, request.ID)
	RequireDraftCode(t, err, problem.ReviewRequestNotFound)
	items, _, err := domain.ListRequests(ctx, f.viewer, f.a, 50, 0)
	if err != nil || len(items) < 1 || items[0].ID != request.ID {
		t.Fatalf("request list wrong: %+v %v", items, err)
	}
	// A policy change invalidates the approved request without deleting its decisions.
	runnerSQL(t, f.store.Pool(), `INSERT INTO databrain.migration_policy_generations(target_id,generation)
	    VALUES ($1,2) ON CONFLICT (target_id) DO UPDATE SET generation=databrain.migration_policy_generations.generation+1`, f.a)
	stale, err := domain.GetRequest(ctx, f.viewer, f.a, request.ID)
	if err != nil || stale.State != "stale" || stale.StaleReason != "policy_changed" || len(stale.Groups) != 2 {
		t.Fatalf("stale request wrong: %+v %v", stale, err)
	}
	_, err = domain.ApproveRequest(ctx, f.viewer, f.a, request.ID, viewGroup)
	RequireDraftCode(t, err, problem.ReviewRequestStale)
	var state, reason string
	if err := f.store.Pool().QueryRow(ctx, `SELECT state,coalesce(stale_reason,'') FROM databrain.migration_requests
	    WHERE target_id=$1 AND id=$2`, f.a, request.ID).Scan(&state, &reason); err != nil || state != "stale" || reason != "policy_changed" {
		t.Fatalf("stale state not persisted: %q %q %v", state, reason, err)
	}
	var decisions int
	if err := f.store.Pool().QueryRow(ctx, `SELECT count(*) FROM databrain.migration_request_decisions
	    WHERE target_id=$1 AND request_id=$2`, f.a, request.ID).Scan(&decisions); err != nil || decisions != 2 {
		t.Fatalf("stale request lost decisions: %d %v", decisions, err)
	}
}

func TestReviewLeaderlessAndConcurrentVotesPostgres(t *testing.T) {
	f, _, domain, viewGroup := reviewFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A leaderless required group blocks submission before the definition is frozen.
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=NULL WHERE id=$1`, viewGroup)
	leaderlessDraft := reviewDraft(t, f, "Leaderless review")
	_, err := domain.SubmitRequest(ctx, f.editor, f.a, leaderlessDraft.ID, 1, uuid.New())
	RequireDraftCode(t, err, problem.ApprovalConfigurationIncomplete)
	read, err := f.store.Get(ctx, f.a, leaderlessDraft.ID)
	if err != nil || read.Frozen {
		t.Fatalf("failed submission froze draft: %+v %v", read, err)
	}
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=$2 WHERE id=$1`, viewGroup, f.viewer.UserID)
	// Two final votes race; the request row lock preserves both decisions and one approved state.
	draft := reviewDraft(t, f, "Concurrent review")
	request, err := domain.SubmitRequest(ctx, f.editor, f.a, draft.ID, 1, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() {
		_, err := domain.ApproveRequest(ctx, f.viewer, f.a, request.ID, viewGroup)
		results <- err
	}()
	go func() {
		_, err := domain.ApproveRequest(ctx, f.editor, f.a, request.ID, f.editGroup)
		results <- err
	}()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent vote failed: %v", err)
		}
	}
	final, err := domain.GetRequest(ctx, f.viewer, f.a, request.ID)
	if err != nil || final.State != "approved" || len(final.Groups) != 2 {
		t.Fatalf("concurrent approval wrong: %+v %v", final, err)
	}
	var decisions int
	if err := f.store.Pool().QueryRow(ctx, `SELECT count(*) FROM databrain.migration_request_decisions
	    WHERE target_id=$1 AND request_id=$2`, f.a, request.ID).Scan(&decisions); err != nil || decisions != 2 {
		t.Fatalf("concurrent votes lost or duplicated: %d %v", decisions, err)
	}
	// A consistent multi-group leader can vote for both groups on a fresh request.
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=$2 WHERE id=$1`, viewGroup, f.actor)
	runnerSQL(t, f.store.Pool(), `INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, viewGroup, f.actor)
	draft = reviewDraft(t, f, "Two-group leader")
	request, err = domain.SubmitRequest(ctx, f.editor, f.a, draft.ID, 1, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	detail, err := domain.GetRequest(ctx, f.editor, f.a, request.ID)
	if err != nil || len(detail.EligibleGroupIDs) != 2 {
		t.Fatalf("two-group leader eligibility wrong: %+v %v", detail, err)
	}
	for _, group := range detail.Groups {
		if _, err := domain.ApproveRequest(ctx, f.editor, f.a, request.ID, group.GroupID); err != nil {
			t.Fatal(err)
		}
	}
	final, err = domain.GetRequest(ctx, f.editor, f.a, request.ID)
	if err != nil || final.State != "approved" {
		t.Fatalf("two-group approval wrong: %+v %v", final, err)
	}
}

func TestReviewAPI(t *testing.T) {
	f, web, _, viewGroup := reviewFixture(t)
	ctx := context.Background()
	draft := reviewDraft(t, f, "API review")
	key := uuid.NewString()
	result := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/drafts/"+draft.ID.String()+"/submit",
		web.editor, jsonBody(t, map[string]string{"expected_revision": "1", "request_key": key}), 201)).Request
	if result.ID == "" || result.State != "pending_review" || len(result.Groups) != 2 || result.SQL == nil {
		t.Fatalf("API submit wrong: %+v", result)
	}
	web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/drafts/"+draft.ID.String()+"/submit",
		web.viewer, jsonBody(t, map[string]string{"expected_revision": "1", "request_key": uuid.NewString()}), 403)
	detail := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "GET", "/api/databases/"+f.a.String()+"/requests/"+result.ID, web.viewer, "", 200)).Request
	if detail.ID != result.ID || len(detail.EligibleGroupIDs) != 1 || detail.Groups[0].LeaderUsername == "" {
		t.Fatalf("API detail wrong: %+v", detail)
	}
	web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/requests/"+result.ID+"/approve",
		web.viewer, jsonBody(t, map[string]string{"group_id": viewGroup.String()}), 200)
	web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/requests/"+result.ID+"/approve",
		web.otherEditor, jsonBody(t, map[string]string{"group_id": f.editGroup.String()}), 403)
	approved := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/requests/"+result.ID+"/approve",
		web.editor, jsonBody(t, map[string]string{"group_id": f.editGroup.String()}), 200)).Request
	if approved.State != "approved" || len(approved.Groups) != 2 {
		t.Fatalf("API approval wrong: %+v", approved)
	}
	list := apiData[struct {
		Requests []httpapi.RequestDTO `json:"requests"`
	}](t, web.jsonRequest(t, "GET", "/api/databases/"+f.a.String()+"/requests", web.viewer, "", 200))
	if len(list.Requests) != 1 || list.Requests[0].ID != result.ID {
		t.Fatalf("API list wrong: %+v", list)
	}
	var executions, jobs int
	if err := f.store.Pool().QueryRow(ctx, `SELECT count(*) FROM databrain.migration_executions`).Scan(&executions); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Pool().QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if executions != 0 || jobs != 0 {
		t.Fatalf("review created execution/job: %d %d", executions, jobs)
	}
	executeKey := uuid.NewString()
	web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/requests/"+result.ID+"/execute",
		web.otherEditor, jsonBody(t, map[string]string{"request_key": executeKey}), 403)
	admission := apiData[struct {
		RequestID   string `json:"request_id"`
		ExecutionID string `json:"execution_id"`
		JobID       string `json:"job_id"`
		State       string `json:"state"`
		Replayed    bool   `json:"replayed"`
	}](t, web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/requests/"+result.ID+"/execute",
		web.editor, jsonBody(t, map[string]string{"request_key": executeKey}), 202))
	if admission.RequestID != result.ID || admission.ExecutionID == "" || admission.JobID == "" || admission.State != "queued" || admission.Replayed {
		t.Fatalf("API admission wrong: %+v", admission)
	}
	replay := apiData[struct {
		ExecutionID string `json:"execution_id"`
		JobID       string `json:"job_id"`
		Replayed    bool   `json:"replayed"`
	}](t, web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/requests/"+result.ID+"/execute",
		web.editor, jsonBody(t, map[string]string{"request_key": executeKey}), 202))
	if !replay.Replayed || replay.ExecutionID != admission.ExecutionID || replay.JobID != admission.JobID {
		t.Fatalf("API admission replay wrong: %+v", replay)
	}
	code := func(w *httptest.ResponseRecorder, want string) {
		t.Helper()
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != want {
			t.Fatalf("expected %s, got %s", want, w.Body.String())
		}
	}
	code(web.jsonRequest(t, "POST", "/api/databases/"+f.a.String()+"/requests/"+result.ID+"/execute",
		web.editor, jsonBody(t, map[string]string{"request_key": uuid.NewString()}), 409), "IDEMPOTENCY_CONFLICT")
	executed := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "GET", "/api/databases/"+f.a.String()+"/requests/"+result.ID, web.viewer, "", 200)).Request
	if executed.State != "consumed" || executed.ExecutionID == nil || *executed.ExecutionID != admission.ExecutionID || executed.JobID == nil {
		t.Fatalf("API consumed request wrong: %+v", executed)
	}
}

func TestAdmissionPostgres(t *testing.T) {
	f, _, domain, viewGroup := reviewFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	draft := reviewDraft(t, f, "Admission review")
	request, err := domain.SubmitRequest(ctx, f.editor, f.a, draft.ID, 1, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	// Pending requests cannot be admitted.
	if _, err := domain.AcceptRequest(ctx, f.editor, f.a, request.ID, uuid.New()); err == nil {
		t.Fatal("pending request admitted")
	} else {
		RequireDraftCode(t, err, problem.ReviewRequestNotApproved)
	}
	if _, err := domain.ApproveRequest(ctx, f.viewer, f.a, request.ID, viewGroup); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.ApproveRequest(ctx, f.editor, f.a, request.ID, f.editGroup); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.AcceptRequest(ctx, f.otherEditor, f.a, request.ID, uuid.New()); err == nil {
		t.Fatal("non-initiating editor admitted")
	} else {
		RequireDraftCode(t, err, problem.DatabaseAccessDenied)
	}
	key := uuid.New()
	first, err := domain.AcceptRequest(ctx, f.editor, f.a, request.ID, key)
	if err != nil || first.Replayed || first.ExecutionID == uuid.Nil || first.JobID < 1 {
		t.Fatalf("admission failed: %+v %v", first, err)
	}
	replay, err := domain.AcceptRequest(ctx, f.editor, f.a, request.ID, key)
	if err != nil || !replay.Replayed || replay.ExecutionID != first.ExecutionID || replay.JobID != first.JobID {
		t.Fatalf("admission replay wrong: %+v %v", replay, err)
	}
	if _, err := domain.AcceptRequest(ctx, f.editor, f.a, request.ID, uuid.New()); err == nil {
		t.Fatal("consumed request accepted with a new key")
	} else {
		RequireDraftCode(t, err, problem.IdempotencyConflict)
	}
	var state, phase string
	var executionID uuid.UUID
	var jobID int64
	if err := f.store.Pool().QueryRow(ctx, `SELECT state,execution_id,job_id FROM databrain.migration_requests
	    WHERE target_id=$1 AND id=$2`, f.a, request.ID).Scan(&state, &executionID, &jobID); err != nil ||
		state != "consumed" || executionID != first.ExecutionID || jobID != first.JobID {
		t.Fatalf("request admission binding wrong: %q %v %d %v", state, executionID, jobID, err)
	}
	if err := f.store.Pool().QueryRow(ctx, `SELECT phase FROM databrain.migration_executions
	    WHERE target_id=$1 AND id=$2`, f.a, first.ExecutionID).Scan(&phase); err != nil || phase != "queued" {
		t.Fatalf("execution not queued: %q %v", phase, err)
	}
	var executions, jobs int
	if err := f.store.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM databrain.migration_executions),
	    (SELECT count(*) FROM databrain.river_job)`).Scan(&executions, &jobs); err != nil || executions != 1 || jobs != 1 {
		t.Fatalf("admission counts wrong: %d/%d %v", executions, jobs, err)
	}
	var marker string
	if err := f.target.QueryRow(ctx, `SELECT value FROM public.draft_marker`).Scan(&marker); err != nil || marker != "unchanged 猫" {
		t.Fatalf("admission executed target SQL: %q %v", marker, err)
	}
	detail, err := domain.GetRequest(ctx, f.editor, f.a, request.ID)
	if err != nil || detail.State != "consumed" || detail.ExecutionID == nil || *detail.ExecutionID != first.ExecutionID {
		t.Fatalf("consumed request detail wrong: %+v %v", detail, err)
	}
}

func TestRequestAcceptanceCommitFaultPostgres(t *testing.T) {
	f, web := newWorkflowFixture(t, "http://localhost:18090")
	ctx := context.Background()
	var viewGroup uuid.UUID
	if err := f.store.Pool().QueryRow(ctx, `SELECT id FROM databrain.groups WHERE name='Runner viewers'`).Scan(&viewGroup); err != nil {
		t.Fatal(err)
	}
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=$2 WHERE id=$1`, f.editGroup, f.actor)
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.groups SET leader_id=$2 WHERE id=$1`, viewGroup, f.viewer.UserID)
	draft, err := f.store.Create(ctx, f.a, f.actor, DraftInput{
		Title: "Commit fault", SQL: "CREATE TABLE public.commit_fault(id int)", DownSQL: "DROP TABLE public.commit_fault",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/databases/" + f.a.String()
	request := apiData[struct {
		Request httpapi.RequestDTO `json:"request"`
	}](t, web.jsonRequest(t, "POST", base+"/drafts/"+draft.ID.String()+"/submit", web.editor,
		jsonBody(t, map[string]string{"expected_revision": "1", "request_key": uuid.NewString()}), 201)).Request
	web.jsonRequest(t, "POST", base+"/requests/"+request.ID+"/approve", web.viewer,
		jsonBody(t, map[string]string{"group_id": viewGroup.String()}), 200)
	web.jsonRequest(t, "POST", base+"/requests/"+request.ID+"/approve", web.editor,
		jsonBody(t, map[string]string{"group_id": f.editGroup.String()}), 200)
	// A deferred platform trigger rejects the commit after EnqueueStore has written execution, receipt,
	// request consumption and the River row. Everything must roll back and the handler must not return
	// 202 or a second job.
	runnerSQL(t, f.schema, `CREATE FUNCTION databrain.reject_acceptance_commit() RETURNS trigger LANGUAGE plpgsql AS $$
	    BEGIN
	        IF NEW.state='consumed' THEN RAISE EXCEPTION 'synthetic acceptance commit failure'; END IF;
	        RETURN NEW;
	    END; $$;
	    CREATE CONSTRAINT TRIGGER reject_acceptance_commit AFTER UPDATE ON databrain.migration_requests
	    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION databrain.reject_acceptance_commit();`)
	defer runnerSQL(t, f.schema, `DROP TRIGGER reject_acceptance_commit ON databrain.migration_requests;
	    DROP FUNCTION databrain.reject_acceptance_commit()`)
	w := web.jsonRequest(t, "POST", base+"/requests/"+request.ID+"/execute", web.editor,
		jsonBody(t, map[string]string{"request_key": uuid.NewString()}), 503)
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "PLATFORM_UNAVAILABLE" {
		t.Fatalf("commit fault response wrong: %s", w.Body.String())
	}
	var state string
	var executions, receipts, jobs, consumed int
	if err := f.store.Pool().QueryRow(ctx, `SELECT
	    (SELECT state FROM databrain.migration_requests WHERE target_id=$1 AND id=$2),
	    (SELECT count(*) FROM databrain.migration_executions),
	    (SELECT count(*) FROM databrain.execution_receipts),
	    (SELECT count(*) FROM databrain.river_job),
	    (SELECT count(*) FROM databrain.migration_requests WHERE target_id=$1 AND id=$2 AND state='consumed')`,
		f.a, request.ID).Scan(&state, &executions, &receipts, &jobs, &consumed); err != nil {
		t.Fatal(err)
	}
	if state != "approved" || consumed != 0 || executions != 0 || receipts != 0 || jobs != 0 {
		t.Fatalf("failed commit leaked acceptance: state=%q consumed=%d executions=%d receipts=%d jobs=%d",
			state, consumed, executions, receipts, jobs)
	}
}
