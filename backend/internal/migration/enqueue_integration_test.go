package migration_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/legacy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newEnqueueFixture(t *testing.T) (draftFixture, *execstore.EnqueueStore) {
	t.Helper()
	f, _ := newAttemptFixture(t)
	enqueuer, err := jobs.NewEnqueuer()
	if err != nil {
		t.Fatal(err)
	}
	store, err := execstore.NewEnqueueStore(f.store.Pool(), enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	return f, store
}

func approvedOperation(target, actor uuid.UUID) execstore.ApprovedOperation {
	op := execstore.ApprovedOperation{
		RequestID: uuid.New(), DefinitionID: uuid.New(), TargetID: target, ActorID: actor,
		Revision: 1, TargetConfigRevision: 1, Direction: execstore.DirectionApply, Mode: execstore.ModeNormal,
		SQLSnapshot:  "CREATE TABLE public.enqueue_probe(id int)",
		DownSnapshot: "DROP TABLE public.enqueue_probe",
	}
	op.PlanHash = execstore.PlanHash(op)
	return op
}

func beginEnqueueTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func requireEnqueueCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, executions, receipts, queued int) {
	t.Helper()
	var gotExecutions, gotReceipts, gotJobs int
	err := pool.QueryRow(ctx, `SELECT
	    (SELECT count(*) FROM databrain.migration_executions),
	    (SELECT count(*) FROM databrain.execution_receipts),
	    (SELECT count(*) FROM databrain.river_job)`).Scan(&gotExecutions, &gotReceipts, &gotJobs)
	if err != nil || gotExecutions != executions || gotReceipts != receipts || gotJobs != queued {
		t.Fatalf("stored state = executions %d receipts %d jobs %d, %v; want %d/%d/%d",
			gotExecutions, gotReceipts, gotJobs, err, executions, receipts, queued)
	}
}

func hasProblemCode(err error, code problem.Code) bool {
	var failure *problem.Error
	return errors.As(err, &failure) && failure.Code == code
}

func TestEnqueueTransactionalAcceptancePostgres(t *testing.T) {
	f, store := newEnqueueFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	op := approvedOperation(f.a, f.actor)
	key := uuid.New()

	tx := beginEnqueueTx(t, ctx, f.store.Pool())
	created, err := store.Enqueue(ctx, tx, op, key)
	if err != nil || created.Replayed || created.ExecutionID == uuid.Nil || created.JobID < 1 {
		t.Fatalf("enqueue failed: %+v, %v", created, err)
	}
	// No other connection may observe the acceptance or job before the caller commits.
	requireEnqueueCounts(t, ctx, f.store.Pool(), 0, 0, 0)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	requireEnqueueCounts(t, ctx, f.store.Pool(), 0, 0, 0)

	tx = beginEnqueueTx(t, ctx, f.store.Pool())
	committed, err := store.Enqueue(ctx, tx, op, key)
	if err != nil || committed.Replayed || committed.ExecutionID == uuid.Nil || committed.JobID < 1 {
		t.Fatalf("committed enqueue failed: %+v, %v", committed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	requireEnqueueCounts(t, ctx, f.store.Pool(), 1, 1, 1)
	var phase, planHash string
	var active uuid.UUID
	if err := f.store.Pool().QueryRow(ctx, `SELECT e.phase,e.plan_hash,s.active_execution_id
	    FROM databrain.migration_executions e JOIN databrain.migration_streams s ON s.target_id=e.target_id
	    WHERE e.id=$1`, committed.ExecutionID).Scan(&phase, &planHash, &active); err != nil {
		t.Fatal(err)
	}
	if phase != "queued" || planHash != op.PlanHash || active != committed.ExecutionID {
		t.Fatalf("execution/stream mismatch: phase=%q hash=%q active=%s", phase, planHash, active)
	}
	var receiptIntent string
	var receiptExecution uuid.UUID
	var receiptJob int64
	if err := f.store.Pool().QueryRow(ctx, `SELECT intent_hash,execution_id,job_id FROM databrain.execution_receipts
	    WHERE target_id=$1 AND actor_id=$2 AND request_key=$3`, f.a, f.actor, key).
		Scan(&receiptIntent, &receiptExecution, &receiptJob); err != nil {
		t.Fatal(err)
	}
	if receiptIntent != op.PlanHash || receiptExecution != committed.ExecutionID || receiptJob != committed.JobID {
		t.Fatal("receipt does not bind the stored intent and job")
	}
	var kind string
	var encoded []byte
	if err := f.store.Pool().QueryRow(ctx, `SELECT kind,args FROM databrain.river_job WHERE id=$1`, committed.JobID).Scan(&kind, &encoded); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil || kind != "migration_execution" ||
		payload["execution_id"] != committed.ExecutionID.String() || payload["request_id"] != op.RequestID.String() ||
		payload["version"] != float64(jobs.PayloadVersion) {
		t.Fatalf("job payload does not carry execution references: kind=%q args=%s", kind, encoded)
	}
	if _, err := f.store.Pool().Exec(ctx, `UPDATE databrain.migration_executions SET sql_snapshot='SELECT 2'
	    WHERE id=$1`, committed.ExecutionID); err == nil {
		t.Fatal("runtime rewrote the immutable execution plan")
	}

	tx = beginEnqueueTx(t, ctx, f.store.Pool())
	replay, err := store.Enqueue(ctx, tx, op, key)
	if err != nil || !replay.Replayed || replay.ExecutionID != committed.ExecutionID || replay.JobID != committed.JobID {
		t.Fatalf("replay did not return the original acceptance: %+v, %v", replay, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	requireEnqueueCounts(t, ctx, f.store.Pool(), 1, 1, 1)

	tx = beginEnqueueTx(t, ctx, f.store.Pool())
	changed := op
	changed.SQLSnapshot += " "
	changed.PlanHash = execstore.PlanHash(changed)
	_, err = store.Enqueue(ctx, tx, changed, key)
	RequireDraftCode(t, err, problem.IdempotencyConflict)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	tx = beginEnqueueTx(t, ctx, f.store.Pool())
	_, err = store.Enqueue(ctx, tx, op, uuid.New())
	RequireDraftCode(t, err, problem.TargetBusy)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Key yang sama untuk actor lain tidak memiliki receipt; target yang sibuk menolak intent baru.
	other := op
	other.ActorID = f.otherActor
	other.PlanHash = execstore.PlanHash(other)
	tx = beginEnqueueTx(t, ctx, f.store.Pool())
	_, err = store.Enqueue(ctx, tx, other, key)
	RequireDraftCode(t, err, problem.TargetBusy)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	tx = beginEnqueueTx(t, ctx, f.store.Pool())
	invalid := op
	invalid.PlanHash = ""
	_, err = store.Enqueue(ctx, tx, invalid, uuid.New())
	RequireDraftCode(t, err, problem.InvalidRequest)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	requireEnqueueCounts(t, ctx, f.store.Pool(), 1, 1, 1)
}

func TestEnqueueConcurrentPostgres(t *testing.T) {
	f, store := newEnqueueFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type outcome struct {
		result execstore.EnqueueResult
		err    error
	}
	run := func(op execstore.ApprovedOperation, key uuid.UUID) outcome {
		tx, err := f.store.Pool().Begin(ctx)
		if err != nil {
			return outcome{err: err}
		}
		defer legacy.RollbackAttempt(tx)
		result, err := store.Enqueue(ctx, tx, op, key)
		if err != nil {
			return outcome{err: err}
		}
		if err := tx.Commit(ctx); err != nil {
			return outcome{err: err}
		}
		return outcome{result: result}
	}
	pair := func(first execstore.ApprovedOperation, firstKey uuid.UUID, second execstore.ApprovedOperation, secondKey uuid.UUID) []outcome {
		var wg sync.WaitGroup
		results := make(chan outcome, 2)
		for _, call := range []func() outcome{
			func() outcome { return run(first, firstKey) },
			func() outcome { return run(second, secondKey) },
		} {
			wg.Add(1)
			go func(call func() outcome) {
				defer wg.Done()
				results <- call()
			}(call)
		}
		wg.Wait()
		close(results)
		values := make([]outcome, 0, 2)
		for value := range results {
			values = append(values, value)
		}
		return values
	}

	op := approvedOperation(f.a, f.actor)
	key := uuid.New()
	outcomes := pair(op, key, op, key)
	replays := 0
	for _, value := range outcomes {
		if value.err != nil {
			t.Fatalf("identical concurrent acceptance failed: %v", value.err)
		}
		if value.result.Replayed {
			replays++
		}
	}
	if replays != 1 || outcomes[0].result.ExecutionID != outcomes[1].result.ExecutionID ||
		outcomes[0].result.JobID != outcomes[1].result.JobID {
		t.Fatalf("identical concurrent submissions did not share one acceptance: %+v", outcomes)
	}
	requireEnqueueCounts(t, ctx, f.store.Pool(), 1, 1, 1)

	op = approvedOperation(f.b, f.actor)
	outcomes = pair(op, uuid.New(), op, uuid.New())
	accepted, busy := 0, 0
	for _, value := range outcomes {
		switch {
		case value.err == nil:
			accepted++
		case hasProblemCode(value.err, problem.TargetBusy):
			busy++
		default:
			t.Fatalf("different concurrent acceptance returned unexpected error: %v", value.err)
		}
	}
	if accepted != 1 || busy != 1 {
		t.Fatalf("busy target admitted %d and rejected %d concurrent intents", accepted, busy)
	}
	requireEnqueueCounts(t, ctx, f.store.Pool(), 2, 2, 2)
}

func TestEnqueueRejectsStaleTargetConfigPostgres(t *testing.T) {
	f, store := newEnqueueFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	op := approvedOperation(f.b, f.actor)
	op.TargetConfigRevision = 2
	op.PlanHash = execstore.PlanHash(op)
	tx := beginEnqueueTx(t, ctx, f.store.Pool())
	_, err := store.Enqueue(ctx, tx, op, uuid.New())
	RequireDraftCode(t, err, problem.RevisionConflict)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	requireEnqueueCounts(t, ctx, f.store.Pool(), 0, 0, 0)
}

func TestEnqueueReceiptSerializationPostgres(t *testing.T) {
	f, store := newEnqueueFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if found, err := store.Receipt(ctx, f.a, f.actor, uuid.New()); err != nil || found != nil {
		t.Fatalf("absent receipt lookup failed: %+v, %v", found, err)
	}

	op := approvedOperation(f.a, f.actor)
	key := uuid.New()
	tx := beginEnqueueTx(t, ctx, f.store.Pool())
	if _, err := store.Enqueue(ctx, tx, op, key); err != nil {
		t.Fatal(err)
	}
	// Lookup harus menyerialkan dengan stream, sehingga penerimaan yang belum commit tidak terlihat.
	blocked, cancelBlocked := context.WithTimeout(ctx, 100*time.Millisecond)
	found, err := store.Receipt(blocked, f.a, f.actor, key)
	cancelBlocked()
	if err == nil || found != nil || !hasProblemCode(err, problem.PlatformUnavailable) {
		t.Fatalf("receipt observed an in-flight acceptance: %+v, %v", found, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if found, err := store.Receipt(ctx, f.a, f.actor, key); err != nil || found != nil {
		t.Fatalf("rolled-back acceptance left a receipt: %+v, %v", found, err)
	}

	tx = beginEnqueueTx(t, ctx, f.store.Pool())
	committed, err := store.Enqueue(ctx, tx, op, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	found, err = store.Receipt(ctx, f.a, f.actor, key)
	if err != nil || found == nil || found.Replayed || found.ExecutionID != committed.ExecutionID || found.JobID != committed.JobID {
		t.Fatalf("receipt lookup lost the acknowledged acceptance: %+v, %v", found, err)
	}
	if found, err := store.Receipt(ctx, f.a, f.otherActor, key); err != nil || found != nil {
		t.Fatalf("receipt leaked across actor: %+v, %v", found, err)
	}
	if found, err := store.Receipt(ctx, f.b, f.actor, key); err != nil || found != nil {
		t.Fatalf("receipt leaked across target: %+v, %v", found, err)
	}
	_, err = store.Receipt(ctx, uuid.Nil, f.actor, key)
	RequireDraftCode(t, err, problem.InvalidRequest)
}
