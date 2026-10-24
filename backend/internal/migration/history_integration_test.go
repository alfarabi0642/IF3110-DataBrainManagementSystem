package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAttemptHistoryPostgres(t *testing.T) {
	f, s := newAttemptFixture(t)
	ctx := context.Background()
	// Valid policy SQL at the size limit. The snapshot is read, never executed.
	prefix := " \r\n-- 猫 exact snapshot; <script>\r\nCREATE TABLE public.never_executed(id int);\r\n--"
	sql := prefix + strings.Repeat("x", MaxSQLBytes-len(prefix)-3) + "\t\r\n"
	d := attemptDraft(t, f, f.a, sql)
	key := uuid.New()
	first := mustReserve(t, s, f, d, ReserveInput{1, key})
	finishAttemptFixture(t, s, first, true)
	secondDraft := attemptDraft(t, f, f.a, "CREATE TABLE public.also_not_executed(id int);")
	second, err := s.Reserve(ctx, f.a, secondDraft.ID, f.otherActor, ReserveInput{1, key})
	if err != nil || !second.Created() {
		t.Fatal("cannot create another actor's receipt using the same key")
	}
	if _, err := s.persistOutcome(ctx, second.Receipt, attemptOutcome{"failed", "not_started", &ExecutionFailure{Code: "SQL_ERROR", Message: "PRIVATE_SQL_DSN", SQLState: "23505", StatementIndex: 2}}); err != nil {
		t.Fatal("cannot initialize failed history")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE databrain.migration_streams SET next_version=10 WHERE target_id=$1`, f.a); err != nil {
		t.Fatal("cannot set this test's version gap")
	}
	third := mustReserve(t, s, f, secondDraft, ReserveInput{1, uuid.New()})
	if _, err := s.Claim(ctx, third); err != nil {
		t.Fatal("cannot claim history fixture")
	}
	if err := s.markCommit(ctx, third.Receipt); err != nil {
		t.Fatal("cannot initialize commit phase")
	}
	if _, err := s.persistOutcome(ctx, third.Receipt, uncertain("COMMIT_UNCERTAIN", "PRIVATE_UNKNOWN")); err != nil {
		t.Fatal("cannot initialize unknown history")
	}
	otherDraft := attemptDraft(t, f, f.b, "CREATE TABLE public.offline_never_executed(id int);")
	other := mustReserve(t, s, f, otherDraft, ReserveInput{1, key})

	t.Run("snapshots ordering and bounded pagination", func(t *testing.T) {
		if _, err := f.store.Save(ctx, f.a, d.ID, f.otherActor, SaveDraftInput{DraftInput{"New title", "changed draft SQL"}, 1}); err != nil {
			t.Fatal("cannot change current draft")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE databrain.users SET display_name='New private profile' WHERE id=$1`, f.actor); err != nil {
			t.Fatal("cannot change current profile")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE databrain.database_targets SET name='New target name',config_revision=2 WHERE id=$1`, f.a); err != nil {
			t.Fatal("cannot change current display metadata")
		}
		got, err := s.Get(ctx, f.a, first.Receipt.ID)
		hash := sha256.Sum256([]byte(sql))
		if err != nil || got == nil || got.SQL != sql || got.SQLHash != hex.EncodeToString(hash[:]) || got.Title != d.Title || got.TargetName != "Target A" || got.ActorUsername != "draft_editor_a" || got.ActorID != f.actor || got.DraftRevision != 1 || got.TargetConfigRevision != 1 || got.State != "succeeded" || got.Failure != nil || got.ResolutionSummary != "" || got.ResolvedAt != nil {
			t.Fatal("history did not preserve its exact immutable snapshots")
		}
		for _, when := range []*time.Time{&got.CreatedAt, got.StartedAt, got.CommitStartedAt, got.FinishedAt} {
			if when == nil || when.IsZero() || when.Location() != time.UTC {
				t.Fatal("history time missing or not UTC")
			}
		}
		for i, want := range []string{"10", "2", "1"} {
			page, more, err := s.List(ctx, f.a, 1, i)
			if err != nil || len(page) != 1 || page[0].Version != want || page[0].TargetID != f.a || more != (i < 2) || page[0].CreatedAt.Location() != time.UTC {
				t.Fatal("numeric DESC ordering, target scope or lookahead incorrect")
			}
		}
		for _, offset := range []int{3, 100000} {
			page, more, err := s.List(ctx, f.a, 200, offset)
			if err != nil || len(page) != 0 || more {
				t.Fatal("empty page or pagination boundary incorrect")
			}
		}
		page, more, err := s.List(ctx, f.b, 50, 0)
		if err != nil || more || len(page) != 1 || page[0].ID != other.Receipt.ID || page[0].State != "reserved" || page[0].StartedAt != nil || page[0].FinishedAt != nil {
			t.Fatal("other target's pending history incorrect")
		}
	})
	t.Run("receipt actor and target isolation with detail scope", func(t *testing.T) {
		for _, want := range []AttemptReceipt{first.Receipt, second.Receipt, other.Receipt} {
			found, err := s.Receipt(ctx, want.TargetID, want.ActorID, key)
			if err != nil || found == nil || found.ID != want.ID || found.DraftRevision != 1 {
				t.Fatal("same key did not identify the correct actor and target")
			}
		}
		for _, ids := range [][2]uuid.UUID{{f.b, f.otherActor}, {f.a, uuid.New()}} {
			found, err := s.Receipt(ctx, ids[0], ids[1], key)
			if err != nil || found != nil {
				t.Fatal("receipt leaked another actor or target")
			}
		}
		for _, ids := range [][2]uuid.UUID{{f.b, first.Receipt.ID}, {f.a, other.Receipt.ID}, {f.a, uuid.New()}} {
			got, err := s.Get(ctx, ids[0], ids[1])
			if err != nil || got != nil {
				t.Fatal("detail leaked a cross-target or missing attempt")
			}
		}
		page, more, err := s.List(ctx, uuid.New(), 50, 0)
		if err != nil || len(page) != 0 || more {
			t.Fatal("missing target should have no history")
		}
	})
	t.Run("safe failed unknown and operator resolution data", func(t *testing.T) {
		failed, err := s.Get(ctx, f.a, second.Receipt.ID)
		if err != nil || failed == nil || failed.State != "failed" || failed.Failure == nil || failed.Failure.Code != "SQL_ERROR" || failed.Failure.SQLState != "23505" || failed.Failure.StatementIndex != 2 || strings.Contains(failed.Failure.Message, "PRIVATE") || failed.FinishedAt == nil {
			t.Fatal("failed history lost safe failure diagnostics")
		}
		unknown, err := s.Get(ctx, f.a, third.Receipt.ID)
		if err != nil || unknown == nil || unknown.State != "unknown" || unknown.FirstUnknownAt == nil || unknown.FirstUnknownAt.Location() != time.UTC || unknown.CommitStartedAt == nil || unknown.FinishedAt != nil || unknown.ResolvedAt != nil || unknown.ResolutionSummary != "" || unknown.Failure == nil || unknown.Failure.Code != "COMMIT_UNCERTAIN" {
			t.Fatal("unknown history misrepresented the unconfirmed outcome")
		}
		// These are legal metadata fixtures, not recovery implementation or proof
		// of a target outcome. A-08.4 owns the operator command and its locks.
		resolveHistoryFixture(t, s, third.Receipt, false)
		resolved, err := s.Get(ctx, f.a, third.Receipt.ID)
		if err != nil || resolved == nil || resolved.State != "failed" || resolved.ResolvedAt == nil || resolved.ResolvedAt.Location() != time.UTC || resolved.FirstUnknownAt == nil || !resolved.FirstUnknownAt.Equal(*unknown.FirstUnknownAt) || resolved.ResolutionSummary != "An operator verified that the migration did not commit." || strings.Contains(fmt.Sprintf("%+v", resolved), "PRIVATE") {
			t.Fatal("operator rollback summary leaked private data or lost uncertainty history")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE databrain.migration_streams SET next_version=9007199254740993 WHERE target_id=$1`, f.a); err != nil {
			t.Fatal("cannot initialize large history version")
		}
		r := mustReserve(t, s, f, secondDraft, ReserveInput{1, uuid.New()})
		if _, err := s.persistOutcome(ctx, r.Receipt, uncertain("PROCESS_INTERRUPTED", "PRIVATE_PROCESS")); err != nil {
			t.Fatal("cannot initialize interrupted fixture")
		}
		resolveHistoryFixture(t, s, r.Receipt, true)
		resolved, err = s.Get(ctx, f.a, r.Receipt.ID)
		if err != nil || resolved == nil || resolved.Version != "9007199254740993" || resolved.State != "succeeded" || resolved.Failure != nil || resolved.ResolutionSummary != "An operator verified that the migration committed." || resolved.FirstUnknownAt == nil || resolved.ResolvedAt == nil {
			t.Fatal("operator commit summary or bigint precision incorrect")
		}
		page, more, err := s.List(ctx, f.a, 200, 0)
		if err != nil || more || len(page) != 4 || page[0].Version != "9007199254740993" || page[1].Version != "10" {
			t.Fatal("large versions were rounded or sorted as text")
		}
		if claimed, err := s.Claim(ctx, other); err != nil || !claimed {
			t.Fatal("cannot initialize running history")
		}
		running, err := s.Get(ctx, f.b, other.Receipt.ID)
		if err != nil || running == nil || running.State != "running" || running.StartedAt == nil || running.FinishedAt != nil || running.Failure != nil {
			t.Fatal("running history incorrect")
		}
	})
	t.Run("read only refresh cancellation and cleanup", func(t *testing.T) {
		before := historyStateFixture(t, s)
		cfg := s.pool.Config()
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal("cannot open read-only platform pool")
		}
		defer pool.Close()
		reader := NewAttemptStore(pool)
		for range 2 {
			if _, _, err := reader.List(ctx, f.a, 50, 0); err != nil {
				t.Fatal("history list attempted a write")
			}
			if _, err := reader.Get(ctx, f.a, first.Receipt.ID); err != nil {
				t.Fatal("history detail attempted a write")
			}
			if _, err := reader.Receipt(ctx, f.a, f.actor, key); err != nil {
				t.Fatal("receipt refresh attempted a write")
			}
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		_, _, err = reader.List(canceled, f.a, 50, 0)
		requireDraftCode(t, err, problem.PlatformUnavailable)
		_, err = reader.Get(canceled, f.a, first.Receipt.ID)
		requireDraftCode(t, err, problem.PlatformUnavailable)
		if !reflect.DeepEqual(before, historyStateFixture(t, s)) || pool.Stat().AcquiredConns() != 0 || s.pool.Stat().AcquiredConns() != 0 {
			t.Fatal("read/refresh changed attempts or streams, or leaked a lease")
		}
		var marker string
		var relations int
		if err := f.target.QueryRow(ctx, `SELECT value FROM public.draft_marker`).Scan(&marker); err != nil || marker != "unchanged 猫" {
			t.Fatal("history changed target rows")
		}
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`).Scan(&relations); err != nil || relations != 1 {
			t.Fatal("history changed target schema")
		}
	})
}

func resolveHistoryFixture(t *testing.T, s *AttemptStore, r AttemptReceipt, success bool) {
	t.Helper()
	tx, err := s.pool.Begin(context.Background())
	if err != nil {
		t.Fatal("cannot begin resolution fixture")
	}
	defer rollbackAttempt(tx)
	state, evidence := "failed", "operator_rollback"
	if success {
		state, evidence = "succeeded", "operator_commit"
	}
	_, err = tx.Exec(context.Background(), `UPDATE databrain.migration_attempts SET state=$2,
        outcome_evidence=$3,finished_at=now(),resolved_at=now(),resolved_by_operator='PRIVATE_OPERATOR',
        resolution_reason='PRIVATE_REASON_EVIDENCE',failure_code=CASE WHEN $2='succeeded' THEN NULL ELSE failure_code END,
        failure_message=CASE WHEN $2='succeeded' THEN NULL ELSE failure_message END WHERE id=$1 AND state='unknown'`, r.ID, state, evidence)
	if err == nil {
		_, err = tx.Exec(context.Background(), `UPDATE databrain.migration_streams SET active_attempt_id=NULL WHERE target_id=$1 AND active_attempt_id=$2`, r.TargetID, r.ID)
	}
	if err != nil || tx.Commit(context.Background()) != nil {
		t.Fatal("cannot commit legal resolution fixture")
	}
}

func historyStateFixture(t *testing.T, s *AttemptStore) [2]string {
	t.Helper()
	var state [2]string
	for i, table := range []string{"migration_attempts", "migration_streams"} {
		// Table names are fixed test constants; private data stays in memory.
		if err := s.pool.QueryRow(context.Background(), `SELECT jsonb_agg(to_jsonb(a))::text FROM (SELECT * FROM databrain.`+table+` ORDER BY target_id`+[]string{`,version`, ``}[i]+`) a`).Scan(&state[i]); err != nil {
			t.Fatal("cannot inspect isolated history state")
		}
	}
	return state
}
