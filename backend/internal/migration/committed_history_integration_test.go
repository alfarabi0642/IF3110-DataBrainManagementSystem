package migration_test

import (
	"context"
	"testing"
	"time"

	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/httpapi"
	"github.com/google/uuid"
)

func TestCommittedHistoryProjectionPostgres(t *testing.T) {
	f, web := newWorkflowFixture(t, "http://localhost:18088")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	domain, err := NewService(f.store, f.runner, f.guard)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := f.store.Create(ctx, f.a, f.actor, DraftInput{
		Title: "History target", SQL: "CREATE TABLE public.history_once(id int)",
		DownSQL: "DROP TABLE public.history_once",
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := f.store.SnapshotDefinition(ctx, f.a, draft.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	executionIDs := make([]uuid.UUID, 5)
	for index := range executionIDs {
		executionIDs[index] = uuid.New()
		runnerSQL(t, f.store.Pool(), `INSERT INTO databrain.migration_executions
		    (id,target_id,actor_id,request_id,definition_id,revision,target_config_revision,
		     direction,mode,plan_hash,sql_snapshot,down_snapshot,phase)
		    VALUES ($1,$2,$3,$4,$5,1,1,'apply','normal',repeat('0',64),
		     'CREATE TABLE public.history_once(id int)','DROP TABLE public.history_once','queued')`,
			executionIDs[index], f.a, f.actor, uuid.New(), definition.ID)
	}
	events := []struct {
		id         uuid.UUID
		direction  string
		parent     *uuid.UUID
		canceled   *uuid.UUID
		generation int64
	}{
		{executionIDs[0], "apply", nil, nil, 2},
		{executionIDs[1], "apply", &executionIDs[0], nil, 3},
		{executionIDs[2], "rollback", &executionIDs[0], &executionIDs[1], 4},
		{executionIDs[3], "forward", &executionIDs[0], nil, 5},
		{executionIDs[4], "apply", &executionIDs[3], nil, 6},
	}
	for _, event := range events {
		runnerSQL(t, f.store.Pool(), `INSERT INTO databrain.migration_committed_events
		    (id,target_id,execution_id,definition_id,actor_id,revision,direction,mode,plan_hash,
		     commit_evidence,parent_execution_id,canceled_execution_id,generation,committed_at)
		    VALUES ($1,$2,$3,$4,$5,1,$6,'normal',repeat('0',64),'commit_ack',$7,$8,$9,$10)`,
			event.id, f.a, event.id, definition.ID, f.actor, event.direction, event.parent, event.canceled,
			event.generation, time.Date(2026, 11, 25, 8, int(event.generation), 0, 0, time.UTC))
	}
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.migration_target_heads SET active_execution_id=$2,
	    generation=6,updated_at=now() WHERE target_id=$1`, f.a, executionIDs[4])
	// A failed legacy attempt and worker attempt must remain visible in activity even when committed
	// rollback pairs are hidden.
	attemptID := uuid.New()
	runnerSQL(t, f.store.Pool(), `INSERT INTO databrain.migration_attempts
	    (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,
	     target_config_revision,sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
	    VALUES ($1,$2,$3,1,1,'History target','Target A',1,'SELECT 1',decode(repeat('00',32),'hex'),
	     $4,'draft_editor_a',$5,'reserved')`, attemptID, f.a, draft.ID, f.actor, uuid.New())
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.migration_attempts SET state='failed',finished_at=now(),
	    outcome_evidence='rollback_ack',failure_code='SQL_ERROR',failure_message='Failed history attempt'
	    WHERE id=$1`, attemptID)
	runnerSQL(t, f.store.Pool(), `INSERT INTO databrain.migration_worker_attempts(execution_id,attempt_number,job_id)
	    VALUES ($1,1,1)`, executionIDs[4])
	runnerSQL(t, f.store.Pool(), `UPDATE databrain.migration_worker_attempts SET state='failed',finished_at=now(),
	    failure_code='SQL_ERROR',failure_message='Worker failed' WHERE execution_id=$1 AND attempt_number=1`, executionIDs[4])

	all, more, err := domain.CommittedHistory(ctx, f.editor, f.a, 10, 0, false)
	if err != nil || more || len(all) != 5 || all[0].ID != executionIDs[4] || all[4].ID != executionIDs[0] {
		t.Fatalf("unfiltered history wrong: %+v %v", all, err)
	}
	filtered, more, err := domain.CommittedHistory(ctx, f.editor, f.a, 10, 0, true)
	if err != nil || more || len(filtered) != 3 ||
		filtered[0].ID != executionIDs[4] || filtered[1].ID != executionIDs[3] || filtered[2].ID != executionIDs[0] {
		t.Fatalf("hide-rollback history wrong: %+v %v", filtered, err)
	}
	pageOne, more, err := domain.CommittedHistory(ctx, f.editor, f.a, 2, 0, true)
	if err != nil || !more || len(pageOne) != 2 || pageOne[1].ID != executionIDs[3] {
		t.Fatalf("filtered first page wrong: %+v %v", pageOne, err)
	}
	pageTwo, more, err := domain.CommittedHistory(ctx, f.editor, f.a, 2, 2, true)
	if err != nil || more || len(pageTwo) != 1 || pageTwo[0].ID != executionIDs[0] {
		t.Fatalf("filtered second page wrong: %+v %v", pageTwo, err)
	}
	detail, err := domain.CommittedEvent(ctx, f.editor, f.a, executionIDs[3])
	if err != nil || detail == nil || detail.SQL != "CREATE TABLE public.history_once(id int)" ||
		detail.DownSQL != "DROP TABLE public.history_once" || detail.Direction != "forward" ||
		detail.ParentExecutionID == nil || *detail.ParentExecutionID != executionIDs[0] {
		t.Fatalf("event detail wrong: %+v %v", detail, err)
	}
	if cross, err := domain.CommittedEvent(ctx, f.editor, f.b, executionIDs[3]); err != nil || cross != nil {
		t.Fatalf("cross-target event disclosed: %+v %v", cross, err)
	}
	activity, more, err := domain.Activity(ctx, f.viewer, f.a, 50, 0)
	if err != nil || more {
		t.Fatalf("activity read failed: %+v %v", activity, err)
	}
	var sawFailedAttempt, sawWorker bool
	for _, item := range activity {
		if item.SourceKind == "legacy_attempt" && item.State == "failed" {
			sawFailedAttempt = true
		}
		if item.SourceKind == "worker_attempt" && item.State == "failed" {
			sawWorker = true
		}
	}
	if !sawFailedAttempt || !sawWorker {
		t.Fatalf("activity lost failure evidence: %+v", activity)
	}

	// API surface: explicit filter, invalid/duplicate boolean, legacy no-flag, event detail and activity.
	base := "/api/databases/" + f.a.String()
	legacy := apiData[struct {
		Attempts []httpapi.AttemptDTO `json:"attempts"`
	}](t, web.jsonRequest(t, "GET", base+"/history", web.viewer, "", 200))
	if len(legacy.Attempts) != 1 || legacy.Attempts[0].State != "failed" {
		t.Fatalf("legacy history contract changed: %+v", legacy)
	}
	filteredAPI := apiData[struct {
		Events    []httpapi.CommittedEventDTO `json:"events"`
		HeadState *httpapi.HeadStateDTO       `json:"head_state"`
	}](t, web.jsonRequest(t, "GET", base+"/history?hide_rollback=true&limit=10", web.viewer, "", 200))
	if len(filteredAPI.Events) != 3 || filteredAPI.Events[0].ExecutionID != executionIDs[4].String() || filteredAPI.Events[0].SQL != nil {
		t.Fatalf("filtered API wrong: %+v", filteredAPI)
	}
	if filteredAPI.HeadState == nil || filteredAPI.HeadState.Head == nil ||
		filteredAPI.HeadState.Head.ExecutionID != executionIDs[4].String() ||
		!filteredAPI.HeadState.Head.CanRollback || filteredAPI.HeadState.HeadGeneration != "6" {
		t.Fatalf("head_state projection wrong: %+v", filteredAPI.HeadState)
	}
	web.jsonRequest(t, "GET", base+"/history?hide_rollback=maybe", web.viewer, "", 400)
	web.jsonRequest(t, "GET", base+"/history?hide_rollback=true&hide_rollback=false", web.viewer, "", 400)
	eventAPI := apiData[struct {
		Event httpapi.CommittedEventDTO `json:"event"`
	}](t, web.jsonRequest(t, "GET", base+"/history/events/"+executionIDs[3].String(), web.viewer, "", 200)).Event
	if eventAPI.SQL == nil || *eventAPI.SQL != "CREATE TABLE public.history_once(id int)" || eventAPI.DownSQL == nil {
		t.Fatalf("event API detail wrong: %+v", eventAPI)
	}
	web.jsonRequest(t, "GET", "/api/databases/"+f.b.String()+"/history/events/"+executionIDs[3].String(), web.viewer, "", 404)
	activityAPI := apiData[struct {
		Activity []httpapi.ActivityDTO `json:"activity"`
	}](t, web.jsonRequest(t, "GET", base+"/activity?limit=50", web.viewer, "", 200))
	if len(activityAPI.Activity) == 0 {
		t.Fatal("API activity empty")
	}
}
