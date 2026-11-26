package migration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	migrationworker "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/worker"
	"github.com/google/uuid"
)

func seedBatchTestTable(t *testing.T, ctx context.Context, h workerTestHarness, rows int) sqlpolicy.BatchConfig {
	t.Helper()
	if _, err := h.targetEnd.Exec(ctx, `
		CREATE TABLE public.accounts (
			id bigint PRIMARY KEY,
			balance bigint NOT NULL
		)
	`); err != nil {
		t.Fatalf("create accounts table failed: %v", err)
	}
	if _, err := h.targetEnd.Exec(ctx, `
		INSERT INTO public.accounts (id, balance)
		SELECT g, g * 100 FROM generate_series(1, $1) AS g
	`, rows); err != nil {
		t.Fatalf("seed accounts table failed: %v", err)
	}
	return sqlpolicy.BatchConfig{
		Schema:      "public",
		Table:       "accounts",
		KeyColumn:   "id",
		ValueColumn: "balance",
		Delta:       25,
		BatchSize:   10,
	}
}

func (h workerTestHarness) approveAndAdmitRequest(t *testing.T, ctx context.Context, req ReviewRequest) Admission {
	t.Helper()
	var err error
	for _, g := range req.Groups {
		p := h.principalForUser(t, ctx, g.LeaderID)
		req, err = h.svc.ApproveRequest(ctx, p, h.rf.a, req.ID, g.GroupID)
		if err != nil {
			t.Fatalf("ApproveRequest for group %s failed: %v", g.GroupName, err)
		}
	}
	adm, err := h.svc.AcceptRequest(ctx, h.editor, h.rf.a, req.ID, uuid.New())
	if err != nil {
		t.Fatalf("AcceptRequest failed: %v", err)
	}
	return adm
}

func TestBatchWorkerFullApplyAndBatchesAPIPostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	cfg := seedBatchTestTable(t, ctx, h, 25)
	plan, err := sqlpolicy.BuildBatchPlan(cfg)
	if err != nil {
		t.Fatalf("BuildBatchPlan failed: %v", err)
	}
	d, err := h.svc.CreateDraft(ctx, h.editor, h.rf.a, DraftInput{
		Title:   "batch-add-25",
		SQL:     plan.UpSQL,
		DownSQL: plan.DownSQL,
	})
	if err != nil {
		t.Fatalf("CreateDraft failed: %v", err)
	}
	req, err := h.svc.SubmitBatchRequest(ctx, h.editor, h.rf.a, d.ID, d.Revision, uuid.New(), cfg)
	if err != nil {
		t.Fatalf("SubmitBatchRequest failed: %v", err)
	}
	adm := h.approveAndAdmitRequest(t, ctx, req)

	worker, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Execute(ctx, adm.ExecutionID, req.ID, adm.JobID); err != nil {
		t.Fatalf("batch Execute failed: %v", err)
	}

	view, err := h.svc.Execution(ctx, h.editor, h.rf.a, adm.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Phase != "finished" || view.Outcome == nil || *view.Outcome != "succeeded" {
		t.Fatalf("expected finished/succeeded, got phase=%s outcome=%v", view.Phase, view.Outcome)
	}
	if view.CommittedBatches != 3 || view.CommittedRows != 25 {
		t.Fatalf("expected 3 batches / 25 rows, got %d / %d", view.CommittedBatches, view.CommittedRows)
	}
	if view.TotalRows == nil || *view.TotalRows != 25 {
		t.Fatalf("expected total_rows=25, got %v", view.TotalRows)
	}

	// Verify target balances all incremented by +25.
	var sum int64
	if err := h.targetEnd.QueryRow(ctx, `SELECT sum(balance) FROM public.accounts`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	expectedSum := int64(25*26/2)*100 + int64(25*25)
	if sum != expectedSum {
		t.Fatalf("expected sum=%d, got %d", expectedSum, sum)
	}

	// Verify GET .../batches projection.
	batches, more, err := h.svc.ListExecutionBatches(ctx, h.editor, h.rf.a, adm.ExecutionID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(batches) != 3 {
		t.Fatalf("expected 3 batches without more, got len=%d more=%v", len(batches), more)
	}
	if batches[0].BatchOrdinal != 1 || batches[0].RowsAffected != 10 ||
		batches[1].BatchOrdinal != 2 || batches[1].RowsAffected != 10 ||
		batches[2].BatchOrdinal != 3 || batches[2].RowsAffected != 5 {
		t.Fatalf("unexpected batch rows: %+v", batches)
	}

	// Verify head advanced once and target gate is released.
	var headID *uuid.UUID
	var activeExec *uuid.UUID
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT active_execution_id FROM databrain.migration_target_heads WHERE target_id=$1`, h.rf.a).Scan(&headID); err != nil {
		t.Fatal(err)
	}
	if headID == nil || *headID != adm.ExecutionID {
		t.Fatalf("expected head=%s, got %v", adm.ExecutionID, headID)
	}
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT active_execution_id FROM databrain.migration_streams WHERE target_id=$1`, h.rf.a).Scan(&activeExec); err != nil {
		t.Fatal(err)
	}
	if activeExec != nil {
		t.Fatalf("expected cleared stream gate, got %s", *activeExec)
	}
}

func TestBatchWorkerPartialFailureAndApprovedResumePostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	cfg := seedBatchTestTable(t, ctx, h, 25)
	plan, err := sqlpolicy.BuildBatchPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.svc.CreateDraft(ctx, h.editor, h.rf.a, DraftInput{
		Title:   "batch-partial-resume",
		SQL:     plan.UpSQL,
		DownSQL: plan.DownSQL,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := h.svc.SubmitBatchRequest(ctx, h.editor, h.rf.a, d.ID, d.Revision, uuid.New(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	adm := h.approveAndAdmitRequest(t, ctx, req)

	worker, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Fail deterministically on batch 2 after batch 1 commits.
	worker.BeforeBatch = func(ordinal int) error {
		if ordinal == 2 {
			return errors.New("injected mid-batch failure on ordinal 2")
		}
		return nil
	}
	if err := worker.Execute(ctx, adm.ExecutionID, req.ID, adm.JobID); err == nil {
		t.Fatal("expected error from injected mid-batch failure")
	}

	view, err := h.svc.Execution(ctx, h.editor, h.rf.a, adm.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Phase != "blocked" || view.Outcome == nil || *view.Outcome != "partial" || !view.HasPartialCommits {
		t.Fatalf("expected blocked/partial with has_partial_commits, got phase=%s outcome=%v partial=%v",
			view.Phase, view.Outcome, view.HasPartialCommits)
	}
	if view.CommittedBatches != 1 || view.CommittedRows != 10 {
		t.Fatalf("expected 1 batch / 10 rows committed, got %d / %d", view.CommittedBatches, view.CommittedRows)
	}
	// Head must NOT have advanced.
	var headID *uuid.UUID
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT active_execution_id FROM databrain.migration_target_heads WHERE target_id=$1`, h.rf.a).Scan(&headID); err != nil {
		t.Fatal(err)
	}
	if headID != nil {
		t.Fatalf("expected head to remain nil on partial commit, got %s", *headID)
	}

	// Submit approved resume request.
	resumeReq, err := h.svc.SubmitRecoveryRequest(ctx, h.editor, h.rf.a, adm.ExecutionID, uuid.New(), execstore.DirectionResume)
	if err != nil {
		t.Fatalf("SubmitRecoveryRequest(resume) failed: %v", err)
	}
	resumeAdm := h.approveAndAdmitRequest(t, ctx, resumeReq)

	// Clear fault injection and run resume worker.
	worker.BeforeBatch = nil
	if err := worker.Execute(ctx, resumeAdm.ExecutionID, resumeReq.ID, resumeAdm.JobID); err != nil {
		t.Fatalf("resume Execute failed: %v", err)
	}

	// Verify all 25 rows incremented exactly ONCE (batch 1 was NOT replayed).
	var sum int64
	if err := h.targetEnd.QueryRow(ctx, `SELECT sum(balance) FROM public.accounts`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	expectedSum := int64(25*26/2)*100 + int64(25*25)
	if sum != expectedSum {
		t.Fatalf("expected sum=%d (no replay of batch 1), got %d", expectedSum, sum)
	}

	// Verify resume execution finished/succeeded and head advanced to resume execution.
	rView, err := h.svc.Execution(ctx, h.editor, h.rf.a, resumeAdm.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if rView.Phase != "finished" || rView.Outcome == nil || *rView.Outcome != "succeeded" {
		t.Fatalf("expected resume finished/succeeded, got phase=%s outcome=%v", rView.Phase, rView.Outcome)
	}
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT active_execution_id FROM databrain.migration_target_heads WHERE target_id=$1`, h.rf.a).Scan(&headID); err != nil {
		t.Fatal(err)
	}
	if headID == nil || *headID != adm.ExecutionID {
		t.Fatalf("expected head=%s (root apply execution), got %v", adm.ExecutionID, headID)
	}
}

func TestBatchWorkerApprovedReverseCompensationAndUnsafeExternalMutationPostgres(t *testing.T) {
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	cfg := seedBatchTestTable(t, ctx, h, 25)
	plan, err := sqlpolicy.BuildBatchPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.svc.CreateDraft(ctx, h.editor, h.rf.a, DraftInput{
		Title:   "batch-partial-compensate",
		SQL:     plan.UpSQL,
		DownSQL: plan.DownSQL,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := h.svc.SubmitBatchRequest(ctx, h.editor, h.rf.a, d.ID, d.Revision, uuid.New(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	adm := h.approveAndAdmitRequest(t, ctx, req)

	worker, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Let batches 1 and 2 commit, then fail before batch 3.
	worker.BeforeBatch = func(ordinal int) error {
		if ordinal == 3 {
			return errors.New("injected failure on ordinal 3")
		}
		return nil
	}
	if err := worker.Execute(ctx, adm.ExecutionID, req.ID, adm.JobID); err == nil {
		t.Fatal("expected injected error on batch 3")
	}

	// Simulate an external write to row id=5 (in batch 1) while batch 2 is untouched.
	// Descending compensation must revert batch 2 (ids 11..20) first, then detect external mutation
	// on batch 1 (id=5) and stop with blocked/partially_reverted WITHOUT clobbering id=5.
	if _, err := h.targetEnd.Exec(ctx, `UPDATE public.accounts SET balance = 999999 WHERE id = 5`); err != nil {
		t.Fatal(err)
	}

	compReq, err := h.svc.SubmitRecoveryRequest(ctx, h.editor, h.rf.a, adm.ExecutionID, uuid.New(), execstore.DirectionCompensate)
	if err != nil {
		t.Fatalf("SubmitRecoveryRequest(compensate) failed: %v", err)
	}
	compAdm := h.approveAndAdmitRequest(t, ctx, compReq)

	worker.BeforeBatch = nil
	if err := worker.Execute(ctx, compAdm.ExecutionID, compReq.ID, compAdm.JobID); err == nil {
		t.Fatal("expected compensation to stop with COMPENSATION_UNSAFE")
	}

	cView, err := h.svc.Execution(ctx, h.editor, h.rf.a, compAdm.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if cView.Phase != "blocked" || cView.Outcome == nil || *cView.Outcome != "partially_reverted" {
		t.Fatalf("expected blocked/partially_reverted, got phase=%s outcome=%v", cView.Phase, cView.Outcome)
	}
	if cView.Failure == nil || cView.Failure.Code != "COMPENSATION_UNSAFE" {
		t.Fatalf("expected COMPENSATION_UNSAFE failure, got %+v", cView.Failure)
	}

	// Batch 2 (id=15) MUST have been reverted to its before-image (15*100 = 1500).
	var bal15, bal5 int64
	if err := h.targetEnd.QueryRow(ctx, `SELECT balance FROM public.accounts WHERE id=15`).Scan(&bal15); err != nil {
		t.Fatal(err)
	}
	if bal15 != 1500 {
		t.Fatalf("expected batch 2 row id=15 reverted to 1500, got %d", bal15)
	}
	// Externally modified row id=5 MUST remain untouched (999999).
	if err := h.targetEnd.QueryRow(ctx, `SELECT balance FROM public.accounts WHERE id=5`).Scan(&bal5); err != nil {
		t.Fatal(err)
	}
	if bal5 != 999999 {
		t.Fatalf("expected external edit on id=5 preserved (999999), got %d", bal5)
	}

	// Now restore id=5 to the expected post-batch-1 value (5*100 + 25 = 525) and re-run compensation
	// via a second approved compensation request to finish reverting batch 1 cleanly.
	if _, err := h.targetEnd.Exec(ctx, `UPDATE public.accounts SET balance = 525 WHERE id = 5`); err != nil {
		t.Fatal(err)
	}
	compReq2, err := h.svc.SubmitRecoveryRequest(ctx, h.editor, h.rf.a, compAdm.ExecutionID, uuid.New(), execstore.DirectionCompensate)
	if err != nil {
		t.Fatalf("second SubmitRecoveryRequest(compensate) failed: %v", err)
	}
	compAdm2 := h.approveAndAdmitRequest(t, ctx, compReq2)
	if err := worker.Execute(ctx, compAdm2.ExecutionID, compReq2.ID, compAdm2.JobID); err != nil {
		t.Fatalf("second compensate Execute failed: %v", err)
	}

	cView2, err := h.svc.Execution(ctx, h.editor, h.rf.a, compAdm2.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if cView2.Phase != "finished" || cView2.Outcome == nil || *cView2.Outcome != "succeeded" {
		t.Fatalf("expected finished/succeeded, got phase=%s outcome=%v", cView2.Phase, cView2.Outcome)
	}
	// All 25 rows must now be back to their exact original before-image sum.
	var finalSum int64
	if err := h.targetEnd.QueryRow(ctx, `SELECT sum(balance) FROM public.accounts`).Scan(&finalSum); err != nil {
		t.Fatal(err)
	}
	origSum := int64(25*26/2) * 100
	if finalSum != origSum {
		t.Fatalf("expected original sum=%d after full compensation, got %d", origSum, finalSum)
	}
	// Head must remain nil (prior head unchanged).
	var headID *uuid.UUID
	if err := h.rf.db.Pool.QueryRow(ctx, `SELECT active_execution_id FROM databrain.migration_target_heads WHERE target_id=$1`, h.rf.a).Scan(&headID); err != nil {
		t.Fatal(err)
	}
	if headID != nil {
		t.Fatalf("expected head to remain nil after compensation, got %s", *headID)
	}
}
