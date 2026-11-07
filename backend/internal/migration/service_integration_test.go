package migration

import (
	"context"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func TestServicePostgres(t *testing.T) {
	f := newRunnerFixture(t)
	s, err := NewService(f.store, f.runner, f.guard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := s.CreateDraft(ctx, f.editor, f.a, DraftInput{"Service draft", "invalid SQL"})
	if err != nil || d.SQL != "invalid SQL" || d.UpdatedByUsername != "draft_editor_a" {
		t.Fatal("service could not preserve an invalid draft without HTTP or templates")
	}
	_, err = s.SaveDraft(ctx, f.viewer, f.a, d.ID, SaveDraftInput{DraftInput{"forged", "SELECT 1"}, 1})
	requireDraftCode(t, err, problem.DatabaseAccessDenied)
	_, err = s.GetDraft(ctx, f.none, f.a, d.ID)
	requireDraftCode(t, err, problem.DatabaseNotFound)
	_, err = s.GetDraft(ctx, f.editor, f.b, d.ID)
	requireDraftCode(t, err, problem.DraftNotFound)
	_, err = s.SaveDraft(ctx, f.editor, f.a, d.ID, SaveDraftInput{DraftInput{"conflict", "SELECT 1"}, 2})
	requireDraftCode(t, err, problem.RevisionConflict)
	sql := "\r\n-- exact SQL; 猫\r\nCREATE TABLE public.service_once(id int PRIMARY KEY); INSERT INTO public.service_once VALUES(1);"
	d, err = s.SaveDraft(ctx, f.editor, f.a, d.ID, SaveDraftInput{DraftInput{"Saved service", sql}, 1})
	if err != nil || d.Revision != 2 {
		t.Fatal("service did not save the expected revision")
	}
	review, err := s.Review(ctx, f.editor, f.a, d.ID, nil)
	if err != nil || review.Draft == nil || review.Receipt != nil || review.Input.ExpectedRevision != 2 || review.Input.RequestKey == uuid.Nil {
		t.Fatal("review did not return the stored revision and request key")
	}
	items, more, err := s.ListDrafts(ctx, f.editor, f.a, 50, 0)
	if err != nil || more || len(items) != 1 || items[0].UpdatedByUsername != d.UpdatedByUsername {
		t.Fatal("service list lost draft metadata")
	}
	var absent bool
	if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.service_once') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatal("review executed SQL")
	}
	if history, _, err := s.History(ctx, f.viewer, f.a, 50, 0); err != nil || len(history) != 0 {
		t.Fatal("review reserved an attempt")
	}
	_, err = s.Execute(ctx, f.viewer, f.a, d.ID, review.Input)
	requireDraftCode(t, err, problem.DatabaseAccessDenied)
	r, err := s.Execute(ctx, f.editor, f.a, d.ID, review.Input)
	if err != nil || r.State != "succeeded" {
		t.Fatal("service execution did not commit")
	}
	if _, err := s.SaveDraft(ctx, f.editor, f.a, d.ID, SaveDraftInput{DraftInput{"latest", "invalid latest SQL"}, 2}); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.Execute(ctx, f.editor, f.a, d.ID, review.Input)
	if err != nil || replayed != r {
		t.Fatal("same key did not replay the original receipt")
	}
	reviewed, err := s.Review(ctx, f.editor, f.a, d.ID, &review.Input)
	if err != nil || reviewed.Receipt == nil || *reviewed.Receipt != r || reviewed.Draft != nil {
		t.Fatal("review read the latest revision before matching receipt")
	}
	_, err = s.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{3, review.Input.RequestKey})
	requireDraftCode(t, err, problem.IdempotencyConflict)
	if receipt, err := s.Receipt(ctx, f.otherEditor, f.a, review.Input.RequestKey); err != nil || receipt != nil {
		t.Fatal("service exposed another actor's receipt")
	}
	if receipt, err := s.Receipt(ctx, f.editor, f.a, review.Input.RequestKey); err != nil || receipt == nil || *receipt != r {
		t.Fatal("service lost the actor's receipt")
	}
	a, err := s.Attempt(ctx, f.viewer, f.a, r.ID)
	if err != nil || a.SQL != sql || a.Title != "Saved service" || a.DraftRevision != 2 {
		t.Fatal("service history lost immutable snapshots")
	}
	_, err = s.Attempt(ctx, f.viewer, f.b, r.ID)
	requireDraftCode(t, err, problem.AttemptNotFound)
	database, err := s.Database(ctx, f.viewer, f.a, access.DatabaseView)
	if err != nil || database.ID != f.a || database.Access != access.ViewOnly {
		t.Fatal("service workspace lost target access")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.CreateDraft(canceled, f.editor, f.a, DraftInput{"canceled", sql}); err == nil {
		t.Fatal("service ignored caller cancellation")
	}
	var count int
	if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.service_once`).Scan(&count); err != nil || count != 1 {
		t.Fatal("receipt replay repeated target SQL")
	}
}

func TestServiceExecutionContextPostgres(t *testing.T) {
	f := newRunnerFixture(t)
	s, err := NewService(f.store, f.runner, f.guard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d := attemptDraft(t, f.draftFixture, f.a, `UPDATE public.draft_marker SET value='must not commit'`)
	lock, err := f.target.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackAttempt(lock)
	runnerSQL(t, lock, `UPDATE public.draft_marker SET value=value`)
	input := ReserveInput{1, uuid.New()}
	caller, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		receipt AttemptReceipt
		err     error
	}
	done := make(chan result, 1)
	go func() {
		r, err := s.Execute(caller, f.editor, f.a, d.ID, input)
		done <- result{r, err}
	}()
	waitRunnerTarget(t, f, f.a, input.RequestKey, "Lock")
	cancel()
	select {
	case got := <-done:
		outcome := runnerOutcome(t, f, got.receipt)
		if got.err != nil || outcome.commit || (outcome.state != "failed" && outcome.state != "unknown") ||
			(outcome.state == "failed" && (outcome.evidence != "rollback_ack" || outcome.code != "CANCELED" || outcome.active)) ||
			(outcome.state == "unknown" && (outcome.evidence != "" || !outcome.active)) {
			t.Fatal("service cancellation did not retain honest transaction evidence")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("service detached active execution from caller cancellation")
	}
}
