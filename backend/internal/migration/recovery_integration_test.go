package migration

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRecoveryPostgres(t *testing.T) {
	f, s := newAttemptFixture(t)
	ctx := context.Background()
	d := attemptDraft(t, f, f.a, `CREATE TABLE public.must_never_replay(id int)`)
	terminal := mustReserve(t, s, f, d, ReserveInput{1, uuid.New()})
	finishAttemptFixture(t, s, terminal, false)
	a := mustReserve(t, s, f, d, ReserveInput{1, uuid.New()})
	dbDraft := attemptDraft(t, f, f.b, d.SQL)
	b := mustReserve(t, s, f, dbDraft, ReserveInput{1, uuid.New()})
	if claimed, err := s.Claim(ctx, b); err != nil || !claimed {
		t.Fatal("cannot initialize running orphan")
	}
	url := f.db.Pool.Config().ConnString()
	if err := f.db.Close(ctx); err != nil {
		t.Fatal("cannot stop old platform process")
	}
	db, err := platformdb.Open(ctx, url)
	if err != nil {
		t.Fatal("cannot restart platform process")
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	s = NewAttemptStore(db.Pool)

	t.Run("restart marks orphans unknown and preserves blocking", func(t *testing.T) {
		n, err := RecoverInterrupted(ctx, db)
		if err != nil || n != 2 {
			t.Fatalf("startup did not recover both orphan states: %v", err)
		}
		for _, r := range []Reservation{a, b} {
			got, err := s.Get(ctx, r.Receipt.TargetID, r.Receipt.ID)
			if err != nil || got == nil || got.State != "unknown" || got.Failure.Code != "PROCESS_INTERRUPTED" || got.FirstUnknownAt == nil || got.FinishedAt != nil || got.SQL != d.SQL {
				t.Fatal("orphan lost snapshot or was falsely finalized")
			}
			replay, err := s.Reserve(ctx, r.Receipt.TargetID, r.Receipt.DraftID, r.Receipt.ActorID, ReserveInput{1, r.Receipt.RequestKey})
			if err != nil || replay.Created() || replay.Receipt.State != "unknown" {
				t.Fatal("restart replay granted execution ownership")
			}
			_, err = s.Reserve(ctx, r.Receipt.TargetID, r.Receipt.DraftID, r.Receipt.ActorID, ReserveInput{1, uuid.New()})
			requireDraftCode(t, err, problem.TargetBusy)
		}
		before := historyStateFixture(t, s)
		if n, err := RecoverInterrupted(ctx, db); err != nil || n != 0 || before != historyStateFixture(t, s) {
			t.Fatal("repeated recovery changed unknown/terminal history")
		}
	})
	t.Run("reject insufficient evidence and non unknown state", func(t *testing.T) {
		before := historyStateFixture(t, s)
		if err := ResolveAttempt(ctx, db, a.Receipt.ID, ResolutionInput{"failed", "operator", " \n"}); !errors.Is(err, ErrResolutionInput) {
			t.Fatal("empty evidence accepted")
		}
		for _, id := range []uuid.UUID{terminal.Receipt.ID, uuid.New()} {
			if err := ResolveAttempt(ctx, db, id, ResolutionInput{"failed", "operator", "PRIVATE_EVIDENCE"}); !errors.Is(err, ErrResolutionState) {
				t.Fatal("terminal or absent attempt accepted")
			}
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		if err := ResolveAttempt(canceled, db, a.Receipt.ID, ResolutionInput{"failed", "operator", "PRIVATE_EVIDENCE"}); err == nil || before != historyStateFixture(t, s) {
			t.Fatal("canceled/invalid resolution changed history")
		}
	})
	t.Run("operator resolution is atomic and evidence stays private", func(t *testing.T) {
		cfg := f.target.Config().Copy()
		cfg.Database = db.Pool.Config().ConnConfig.Database
		owner, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal("cannot initialize resolution rollback fixture")
		}
		defer closeDraftTestConn(owner)
		runnerSQL(t, owner, `CREATE FUNCTION databrain.reject_resolution_clear() RETURNS trigger LANGUAGE plpgsql AS $$
            BEGIN IF OLD.active_attempt_id IS NOT NULL AND NEW.active_attempt_id IS NULL THEN RAISE EXCEPTION 'PRIVATE_TRIGGER'; END IF; RETURN NEW; END; $$;
            CREATE TRIGGER reject_resolution_clear BEFORE UPDATE ON databrain.migration_streams FOR EACH ROW EXECUTE FUNCTION databrain.reject_resolution_clear();`)
		before := historyStateFixture(t, s)
		input := ResolutionInput{"failed", "PRIVATE_OPERATOR", "PRIVATE_REASON_EVIDENCE\r\n"}
		err = ResolveAttempt(ctx, db, a.Receipt.ID, input)
		requireDraftCode(t, err, problem.PlatformUnavailable)
		if before != historyStateFixture(t, s) {
			t.Fatal("stream failure committed a partial resolution")
		}
		runnerSQL(t, owner, `DROP TRIGGER reject_resolution_clear ON databrain.migration_streams; DROP FUNCTION databrain.reject_resolution_clear()`)
		for i, r := range []Reservation{a, b} {
			old, _ := s.Get(ctx, r.Receipt.TargetID, r.Receipt.ID)
			if i == 1 {
				input.Outcome = "succeeded"
			}
			if err := ResolveAttempt(ctx, db, r.Receipt.ID, input); err != nil {
				t.Fatal("operator evidence was not saved")
			}
			got, err := s.Get(ctx, r.Receipt.TargetID, r.Receipt.ID)
			if err != nil || got.State != input.Outcome || got.ResolvedAt == nil || got.ResolvedAt.Location() != time.UTC || !got.FirstUnknownAt.Equal(*old.FirstUnknownAt) || got.ResolutionSummary == "" || strings.Contains(got.ResolutionSummary, "PRIVATE") {
				t.Fatal("resolution lost uncertainty history or exposed private evidence")
			}
			var operator, evidence string
			var idle bool
			if err := db.Pool.QueryRow(ctx, `SELECT a.resolved_by_operator,a.resolution_reason,s.active_attempt_id IS NULL FROM databrain.migration_attempts a JOIN databrain.migration_streams s ON s.target_id=a.target_id WHERE a.id=$1`, r.Receipt.ID).Scan(&operator, &evidence, &idle); err != nil || !idle || operator != input.Operator || evidence != input.Evidence {
				t.Fatal("private evidence or stream clear was not atomic")
			}
			if err := ResolveAttempt(ctx, db, r.Receipt.ID, input); !errors.Is(err, ErrResolutionState) {
				t.Fatal("resolved terminal attempt changed twice")
			}
		}
		_, err = s.Reserve(ctx, f.b, dbDraft.ID, f.actor, ReserveInput{1, uuid.New()})
		requireDraftCode(t, err, problem.DraftAlreadyExecuted)
		// Revision gagal hanya boleh memakai key baru secara sengaja. Resolusi tidak membuat reservasi atau
		// menjalankan attempt baru.
		r, err := s.Reserve(ctx, f.a, d.ID, f.actor, ReserveInput{1, uuid.New()})
		if err != nil || !r.Created() || r.Receipt.Version != "3" {
			t.Fatal("explicit new reservation after confirmed rollback failed")
		}
		if err := ResolveAttempt(ctx, db, r.Receipt.ID, input); !errors.Is(err, ErrResolutionState) {
			t.Fatal("reserved attempt was resolved")
		}
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.must_never_replay') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("startup or resolution executed target SQL")
		}
	})
	t.Run("maintenance shares process lock without false monitor loss", func(t *testing.T) {
		err := db.WithProcessLock(ctx, func(conn *pgx.Conn) error {
			other, err := platformdb.Open(ctx, url)
			if other != nil || !errors.Is(err, platformdb.ErrProcessLock) {
				t.Fatal("maintenance overlapped another server")
			}
			_, err = conn.Exec(ctx, `SELECT pg_sleep(3)`)
			return err
		})
		if err != nil || !db.Ready(ctx) {
			t.Fatal("monitor used a busy maintenance connection or expired its check early")
		}
	})
}

func TestRecoveryCommitBoundariesPostgres(t *testing.T) {
	f := newRunnerFixture(t)
	ctx := context.Background()
	d := attemptDraft(t, f.draftFixture, f.a, `CREATE TABLE public.recovery_once(id int); INSERT INTO public.recovery_once VALUES(1)`)
	input := ReserveInput{1, uuid.New()}
	r := mustReserve(t, f.runner.store, f.draftFixture, d, input)
	if ok, err := f.runner.store.Claim(ctx, r); err != nil || !ok {
		t.Fatal("cannot claim commit-boundary fixture")
	}
	r.Receipt.State = "running"
	// Buang response COMMIT server asli pada batas yang sama dengan Runner. Simulasi gangguan ini tidak
	// memerlukan hook production baru.
	tracer := &lostCommitAckTracer{}
	cfg := f.target.Config().Copy()
	cfg.User, cfg.Password = "databrain_target", os.Getenv("DATABRAIN_TARGET_PASSWORD")
	cfg.Tracer = tracer
	cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &lostCommitAckConn{Conn: c}, nil
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot open real target fault connection")
	}
	defer closeDraftTestConn(conn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal("cannot begin target fault transaction")
	}
	for _, sql := range []string{`CREATE TABLE public.recovery_once(id int)`, `INSERT INTO public.recovery_once VALUES(1)`} {
		if _, err := tx.Exec(ctx, sql, pgx.QueryExecModeExec); err != nil {
			t.Fatal("target fault script failed")
		}
	}
	if err := f.runner.store.markCommit(ctx, r.Receipt); err != nil {
		t.Fatal("cannot record commit phase")
	}
	tracer.armed.Store(true)
	commitErr := tx.Commit(ctx)
	if commitErr == nil || tracer.armed.Load() {
		t.Fatal("target acknowledgment fault did not fire")
	}
	got, err := f.runner.store.finishExecution(r.Receipt, commitOutcome(conn, commitErr), time.Now().Add(5*time.Second))
	if err != nil || got.State != "unknown" {
		t.Fatal("lost target acknowledgment was falsely finalized")
	}
	var count int
	if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.recovery_once`).Scan(&count); err != nil || count != 1 {
		t.Fatal("target did not actually commit once")
	}
	replay, err := f.runner.Execute(ctx, f.editor, f.a, d.ID, input)
	if err != nil || replay.ID != got.ID || replay.State != "unknown" {
		t.Fatal("same key replayed an unknown target transaction")
	}
	if _, err := f.runner.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{1, uuid.New()}); err == nil {
		t.Fatal("unknown target was not blocked")
	}
	if n, err := RecoverInterrupted(ctx, f.db); err != nil || n != 0 {
		t.Fatal("startup inferred an unknown outcome")
	}
	if err := ResolveAttempt(ctx, f.db, got.ID, ResolutionInput{"succeeded", "test operator", "Test observed the acknowledged server response and one committed row."}); err != nil {
		t.Fatal("operator evidence could not resolve target commit")
	}

	t.Run("lost platform outcome acknowledgment reads terminal proof", func(t *testing.T) {
		d := attemptDraft(t, f.draftFixture, f.a, `INSERT INTO public.recovery_once VALUES(2)`)
		r := mustReserve(t, f.runner.store, f.draftFixture, d, ReserveInput{1, uuid.New()})
		if ok, err := f.runner.store.Claim(ctx, r); err != nil || !ok {
			t.Fatal("cannot claim outcome fixture")
		}
		runnerSQL(t, f.target, d.SQL) // Perubahan target nyata sebelum hasil disimpan.
		r.Receipt.State = "running"
		pcfg := f.store.pool.Config()
		pcfg.ConnConfig.DialFunc, pcfg.ConnConfig.Tracer = cfg.DialFunc, tracer
		pool, err := pgxpool.NewWithConfig(ctx, pcfg)
		if err != nil {
			t.Fatal("cannot open platform fault pool")
		}
		defer pool.Close()
		store := NewAttemptStore(pool)
		tracer.armed.Store(true)
		out, err := store.finishExecution(r.Receipt, attemptOutcome{state: "succeeded", evidence: "commit_ack"}, time.Now().Add(5*time.Second))
		if err != nil || out.State != "succeeded" || tracer.armed.Load() {
			t.Fatal("lost outcome acknowledgment overwrote committed terminal proof")
		}
		if o := runnerOutcome(t, f, out); o.active || o.evidence != "commit_ack" {
			t.Fatal("outcome and stream did not commit atomically")
		}
	})
	t.Run("unavailable outcome store leaves running for startup recovery", func(t *testing.T) {
		d := attemptDraft(t, f.draftFixture, f.c, `CREATE TABLE public.recovery_orphan(id int)`)
		r := mustReserve(t, f.runner.store, f.draftFixture, d, ReserveInput{1, uuid.New()})
		if ok, err := f.runner.store.Claim(ctx, r); err != nil || !ok {
			t.Fatal("cannot claim unavailable outcome fixture")
		}
		runnerSQL(t, f.second, d.SQL)
		r.Receipt.State = "running"
		pool, err := pgxpool.NewWithConfig(ctx, f.store.pool.Config())
		if err != nil {
			t.Fatal("cannot initialize unavailable outcome pool")
		}
		pool.Close()
		_, err = NewAttemptStore(pool).finishExecution(r.Receipt, attemptOutcome{state: "succeeded", evidence: "commit_ack"}, time.Now().Add(3*time.Second))
		requireDraftCode(t, err, problem.PlatformUnavailable)
		if o := runnerOutcome(t, f, r.Receipt); o.state != "running" || !o.active {
			t.Fatal("unavailable persistence invented a terminal outcome")
		}
		if n, err := RecoverInterrupted(ctx, f.db); err != nil || n != 1 {
			t.Fatal("startup did not block the unresolved committed target")
		}
		if o := runnerOutcome(t, f, r.Receipt); o.state != "unknown" || !o.active || o.code != "PROCESS_INTERRUPTED" {
			t.Fatal("startup falsely claimed rollback or unlocked the target")
		}
		var present bool
		if err := f.second.QueryRow(ctx, `SELECT to_regclass('public.recovery_orphan') IS NOT NULL`).Scan(&present); err != nil || !present {
			t.Fatal("startup replayed or removed the actual target effect")
		}
	})
}
