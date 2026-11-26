package migration_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/httpapi"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/legacy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func createExecution(t *testing.T, f runnerFixture) (uuid.UUID, int64) {
	return createExecutionContext(t, f, f.a)
}

func executionErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal("invalid error envelope")
	}
	return envelope.Error.Code
}

func TestExecutionAPIPostgres(t *testing.T) {
	f, web := newWorkflowFixture(t, "http://localhost:18086")
	executionID, jobID := createExecution(t, f)
	base := "/api/databases/" + f.a.String()
	detail := base + "/executions/" + executionID.String()
	ctx := context.Background()

	t.Run("viewer reads durable projection without SQL", func(t *testing.T) {
		w := web.jsonRequest(t, "GET", detail, web.viewer, "", 200)
		data := apiData[httpapi.ExecutionDTO](t, w)
		if data.ID != executionID.String() || data.TargetID != f.a.String() || data.Direction != execstore.DirectionApply ||
			data.Mode != execstore.ModeNormal || data.Phase != "queued" || data.Outcome != nil ||
			data.CommittedRows != "0" || data.CommittedBatches != "0" || data.TotalRows != nil ||
			data.HasPartialCommits || data.ReconciliationPending || data.Failure != nil || len(data.Actions) != 0 {
			t.Fatalf("unexpected execution projection: %+v", data)
		}
		body := w.Body.String()
		if strings.Contains(body, "CREATE TABLE") || strings.Contains(body, "private_execution_probe") ||
			strings.Contains(body, "request_key") {
			t.Fatalf("projection exposed SQL or request binding: %s", body)
		}
	})

	t.Run("job lookup is durable after queue cleanup and authorization", func(t *testing.T) {
		jobPath := "/api/jobs/" + strconv.FormatInt(jobID, 10)
		data := apiData[httpapi.ExecutionDTO](t, web.jsonRequest(t, "GET", jobPath, web.viewer, "", 200))
		if data.ID != executionID.String() {
			t.Fatal("job lookup resolved a different execution")
		}
		if _, err := f.store.Pool().Exec(ctx, "DELETE FROM databrain.river_job WHERE id=$1", jobID); err != nil {
			t.Fatal(err)
		}
		data = apiData[httpapi.ExecutionDTO](t, web.jsonRequest(t, "GET", jobPath, web.viewer, "", 200))
		if data.ID != executionID.String() {
			t.Fatal("job lookup depended on River retention")
		}
		w := web.jsonRequest(t, "GET", jobPath, web.none, "", 404)
		if executionErrorCode(t, w.Body.Bytes()) != string(problem.DatabaseNotFound) {
			t.Fatal("ungranted job lookup did not conceal the target")
		}
		for _, invalid := range []string{"0", "-1", "abc", "9223372036854775808"} {
			w := web.jsonRequest(t, "GET", "/api/jobs/"+invalid, web.viewer, "", 400)
			if executionErrorCode(t, w.Body.Bytes()) != string(problem.InvalidRequest) {
				t.Fatal("invalid job ID was not rejected")
			}
		}
		unknown := web.jsonRequest(t, "GET", "/api/jobs/9223372036854775807", web.viewer, "", 404)
		if executionErrorCode(t, unknown.Body.Bytes()) != string(problem.DatabaseNotFound) {
			t.Fatal("unknown job did not use concealed not-found")
		}
	})

	t.Run("no grant, cross-target and invalid execution IDs", func(t *testing.T) {
		w := web.jsonRequest(t, "GET", detail, web.none, "", 404)
		if executionErrorCode(t, w.Body.Bytes()) != string(problem.DatabaseNotFound) {
			t.Fatal("no-grant detail was not concealed")
		}
		cross := "/api/databases/" + f.b.String() + "/executions/" + executionID.String()
		w = web.jsonRequest(t, "GET", cross, web.viewer, "", 404)
		if executionErrorCode(t, w.Body.Bytes()) != string(problem.ExecutionNotFound) {
			t.Fatal("cross-target execution was not concealed")
		}
		missing := base + "/executions/" + uuid.New().String()
		w = web.jsonRequest(t, "GET", missing, web.viewer, "", 404)
		if executionErrorCode(t, w.Body.Bytes()) != string(problem.ExecutionNotFound) {
			t.Fatal("missing execution did not return execution-not-found")
		}
		web.jsonRequest(t, "GET", base+"/executions/not-a-uuid", web.viewer, "", 400)
		web.jsonRequest(t, "GET", detail, web.anonymous, "", 401)
	})

	t.Run("unknown state stays distinct from failure", func(t *testing.T) {
		if _, err := f.store.Pool().Exec(ctx, "UPDATE databrain.migration_executions SET phase='running' WHERE id=$1", executionID); err != nil {
			t.Fatal(err)
		}
		running := apiData[httpapi.ExecutionDTO](t, web.jsonRequest(t, "GET", detail, web.viewer, "", 200))
		if running.Phase != "running" || running.StartedAt == nil || running.Outcome != nil {
			t.Fatalf("running projection is wrong: %+v", running)
		}
		if _, err := f.store.Pool().Exec(ctx, `UPDATE databrain.migration_executions
		    SET phase='blocked',outcome='unknown',reconciliation_pending=true WHERE id=$1`, executionID); err != nil {
			t.Fatal(err)
		}
		blocked := apiData[httpapi.ExecutionDTO](t, web.jsonRequest(t, "GET", detail, web.viewer, "", 200))
		if blocked.Phase != "blocked" || blocked.Outcome == nil || *blocked.Outcome != "unknown" ||
			!blocked.ReconciliationPending || blocked.Failure != nil || blocked.FinishedAt != nil {
			t.Fatalf("unknown projection is wrong: %+v", blocked)
		}
		if _, err := f.store.Pool().Exec(ctx, `UPDATE databrain.migration_executions
		    SET phase='finished',outcome='failed',failure_code='SQL_ERROR',
		    failure_message='The target rejected a statement.',failure_sqlstate='23505' WHERE id=$1`, executionID); err != nil {
			t.Fatal(err)
		}
		failed := apiData[httpapi.ExecutionDTO](t, web.jsonRequest(t, "GET", detail, web.viewer, "", 200))
		if failed.Phase != "finished" || failed.Outcome == nil || *failed.Outcome != "failed" || failed.FinishedAt == nil ||
			failed.Failure == nil || failed.Failure.Code != "SQL_ERROR" || failed.Failure.SQLState != "23505" {
			t.Fatalf("failed projection is wrong: %+v", failed)
		}
	})

	t.Run("bigint counters stay decimal strings", func(t *testing.T) {
		secondID, _ := createExecutionContext(t, f, f.b)
		if _, err := f.store.Pool().Exec(ctx, `UPDATE databrain.migration_executions
		    SET phase='running',committed_rows=9007199254740993,committed_batches=9007199254740995,
		    total_rows=9007199254740997 WHERE id=$1`, secondID); err != nil {
			t.Fatal(err)
		}
		path := "/api/databases/" + f.b.String() + "/executions/" + secondID.String()
		data := apiData[httpapi.ExecutionDTO](t, web.jsonRequest(t, "GET", path, web.viewer, "", 200))
		if data.CommittedRows != "9007199254740993" || data.CommittedBatches != "9007199254740995" ||
			data.TotalRows == nil || *data.TotalRows != "9007199254740997" {
			t.Fatalf("bigint counters lost precision: %+v", data)
		}
	})
}

// createExecutionContext membuat eksekusi pada target tertentu tanpa menyentuh stream target lain.
func createExecutionContext(t *testing.T, f runnerFixture, target uuid.UUID) (uuid.UUID, int64) {
	t.Helper()
	enqueuer, err := jobs.NewEnqueuer()
	if err != nil {
		t.Fatal(err)
	}
	store, err := execstore.NewEnqueueStore(f.store.Pool(), enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	op := execstore.ApprovedOperation{
		RequestID: uuid.New(), DefinitionID: uuid.New(), TargetID: target, ActorID: f.actor,
		Revision: 1, TargetConfigRevision: 1, Direction: execstore.DirectionApply, Mode: execstore.ModeNormal,
		SQLSnapshot: "SELECT 1",
	}
	op.PlanHash = execstore.PlanHash(op)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := f.store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.RollbackAttempt(tx)
	result, err := store.Enqueue(ctx, tx, op, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return result.ExecutionID, result.JobID
}
