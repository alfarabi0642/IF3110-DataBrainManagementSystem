package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newAttemptFixture(t *testing.T) (draftFixture, *AttemptStore) {
	t.Helper()
	f := newDraftFixture(t)
	if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO databrain.migration_streams(target_id) VALUES ($1),($2)`, f.a, f.b); err != nil {
		t.Fatal("cannot initialize isolated streams")
	}
	return f, NewAttemptStore(f.store.pool)
}

func attemptDraft(t *testing.T, f draftFixture, target uuid.UUID, sql string) Draft {
	t.Helper()
	d, err := f.store.Create(context.Background(), target, f.actor, DraftInput{"  Perubahan 猫 \t", sql})
	if err != nil {
		t.Fatal("cannot create test draft")
	}
	return d
}

func mustReserve(t *testing.T, s *AttemptStore, f draftFixture, d Draft, input ReserveInput) Reservation {
	t.Helper()
	r, err := s.Reserve(context.Background(), d.TargetID, d.ID, f.actor, input)
	if err != nil || !r.Created() || r.Receipt.ID == uuid.Nil || r.Receipt.State != "reserved" {
		t.Fatalf("reservation failed: %v", err)
	}
	return r
}

// Fixture hasil menguji urutan reservasi dengan metadata platform saja, tanpa SQL target.
func finishAttemptFixture(t *testing.T, s *AttemptStore, r Reservation, success bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal("cannot begin outcome fixture")
	}
	defer rollbackAttempt(tx)
	if success {
		_, err = tx.Exec(ctx, `UPDATE databrain.migration_attempts SET state='running',started_at=now() WHERE id=$1 AND state='reserved';`, r.Receipt.ID)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE databrain.migration_attempts SET state='succeeded',commit_started_at=now(),finished_at=now(),outcome_evidence='commit_ack' WHERE id=$1`, r.Receipt.ID)
		}
	} else {
		_, err = tx.Exec(ctx, `UPDATE databrain.migration_attempts SET state='failed',finished_at=now(),outcome_evidence='not_started',failure_code='CANCELED',failure_message='Not started.' WHERE id=$1`, r.Receipt.ID)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE databrain.migration_streams SET active_attempt_id=NULL WHERE target_id=$1 AND active_attempt_id=$2`, r.Receipt.TargetID, r.Receipt.ID)
	}
	if err != nil || tx.Commit(ctx) != nil {
		t.Fatal("cannot finalize isolated outcome fixture")
	}
}

func TestAttemptStorePostgres(t *testing.T) {
	f, s := newAttemptFixture(t)
	ctx := context.Background()
	sql := " \r\n-- 猫 exact snapshot\r\nCREATE TABLE public.never_executed(id int);\r\nINSERT INTO public.draft_marker VALUES ('a;b'); \t\n"
	d := attemptDraft(t, f, f.a, sql)
	input := ReserveInput{1, uuid.New()}
	r := mustReserve(t, s, f, d, input)
	if r.Receipt.Version != "1" || r.Receipt.DraftID != d.ID || r.Receipt.DraftRevision != 1 || r.Receipt.ActorID != f.actor || r.Receipt.RequestKey != input.RequestKey {
		t.Fatal("receipt does not identify the stored request")
	}
	t.Run("exact snapshot atomic stream and immutable fields", func(t *testing.T) {
		var title, target, username, storedSQL string
		var hash []byte
		var config, next int64
		var active uuid.UUID
		err := s.pool.QueryRow(ctx, `SELECT a.title_snapshot,a.target_name_snapshot,a.executor_username_snapshot,
		    a.sql_snapshot,a.sql_sha256,a.target_config_revision,s.next_version,s.active_attempt_id
		    FROM databrain.migration_attempts a JOIN databrain.migration_streams s ON s.target_id=a.target_id WHERE a.id=$1`, r.Receipt.ID).
			Scan(&title, &target, &username, &storedSQL, &hash, &config, &next, &active)
		wantHash := sha256.Sum256([]byte(sql))
		if err != nil || title != d.Title || target != "Target A" || username != "draft_editor_a" || storedSQL != sql || !bytes.Equal(hash, wantHash[:]) || config != 1 || next != 2 || active != r.Receipt.ID {
			t.Fatal("snapshot/version/stream was not stored atomically and exactly")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE databrain.migration_attempts SET sql_snapshot='changed' WHERE id=$1`, r.Receipt.ID); err == nil {
			t.Fatal("runtime changed immutable execution snapshot")
		}
	})
	t.Run("replay and mismatched key precede busy and current revision", func(t *testing.T) {
		replay, err := s.Reserve(ctx, f.a, d.ID, f.actor, input)
		if err != nil || replay.Created() || replay.Receipt != r.Receipt {
			t.Fatal("same-key replay must return the existing receipt")
		}
		for _, args := range []struct {
			draft uuid.UUID
			rev   int64
		}{{uuid.New(), 1}, {d.ID, 2}} {
			_, err := s.Reserve(ctx, f.a, args.draft, f.actor, ReserveInput{args.rev, input.RequestKey})
			requireDraftCode(t, err, problem.IdempotencyConflict)
		}
		_, err = s.Reserve(ctx, f.a, d.ID, f.otherActor, input)
		requireDraftCode(t, err, problem.TargetBusy)
		for _, ids := range [][2]uuid.UUID{{f.a, f.otherActor}, {f.b, f.actor}} {
			found, err := s.Receipt(ctx, ids[0], ids[1], input.RequestKey)
			if err != nil || found != nil {
				t.Fatal("receipt leaked across actor or target")
			}
		}
		if _, err := s.Claim(ctx, replay); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("only creator claims once despite copied or modified receipt", func(t *testing.T) {
		if claimed, err := NewAttemptStore(s.pool).Claim(ctx, r); claimed || err != nil {
			t.Fatal("another store consumed the creator permit")
		}
		copy := r
		copy.Receipt.ID, copy.Receipt.TargetID = uuid.New(), f.b
		claimed, err := s.Claim(ctx, copy)
		if err != nil || !claimed {
			t.Fatalf("creator claim failed: %v", err)
		}
		if claimed, err := s.Claim(ctx, r); err != nil || claimed {
			t.Fatal("creator claimed twice")
		}
		var state string
		var started bool
		if err := s.pool.QueryRow(ctx, `SELECT state,started_at IS NOT NULL FROM databrain.migration_attempts WHERE id=$1`, r.Receipt.ID).Scan(&state, &started); err != nil || state != "running" || !started {
			t.Fatal("claim did not persist running/start time on its original attempt")
		}
	})
	finishAttemptFixture(t, s, r, true)
	t.Run("database constraints independently enforce deduplication", func(t *testing.T) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal("cannot begin constraint checks")
		}
		defer rollbackAttempt(tx)
		clone := `INSERT INTO databrain.migration_attempts
		    (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,target_config_revision,
		     sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
		    SELECT $2,target_id,draft_id,draft_revision,$3,title_snapshot,target_name_snapshot,target_config_revision,
		     sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,$4,'reserved'
		    FROM databrain.migration_attempts WHERE id=$1`
		denied := func(query, code, constraint string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(ctx, "SAVEPOINT attempt_constraint"); err != nil {
				t.Fatal("cannot create constraint savepoint")
			}
			_, err := tx.Exec(ctx, query, args...)
			var p *pgconn.PgError
			if !errors.As(err, &p) || p.Code != code || constraint != "" && p.ConstraintName != constraint {
				t.Fatal("database did not enforce expected attempt constraint")
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT attempt_constraint"); err != nil {
				t.Fatal("cannot roll back constraint case")
			}
		}
		denied(clone, "23505", "", r.Receipt.ID, uuid.New(), int64(2), input.RequestKey)
		denied(clone, "23505", "", r.Receipt.ID, uuid.New(), int64(1), uuid.New())
		second := uuid.New()
		if _, err := tx.Exec(ctx, clone, r.Receipt.ID, second, int64(2), uuid.New()); err != nil {
			t.Fatal("cannot create constraint attempt")
		}
		denied(clone, "23505", "one_unresolved_attempt_per_target", r.Receipt.ID, uuid.New(), int64(3), uuid.New())
		if _, err := tx.Exec(ctx, `UPDATE databrain.migration_attempts SET state='running',started_at=now() WHERE id=$1`, second); err != nil {
			t.Fatal("cannot start constraint attempt")
		}
		denied(`UPDATE databrain.migration_attempts SET state='succeeded',finished_at=now(),outcome_evidence='commit_ack' WHERE id=$1`, "23505", "one_success_per_draft_revision", second)
		if _, err := tx.Exec(ctx, `UPDATE databrain.migration_streams SET active_attempt_id=$2 WHERE target_id=$1`, f.b, r.Receipt.ID); err != nil {
			t.Fatal("cannot prepare deferred stream scope check")
		}
		denied(`SET CONSTRAINTS ALL IMMEDIATE`, "23503", "")
	})
	t.Run("old success stale revision and edited draft replay", func(t *testing.T) {
		_, err := s.Reserve(ctx, f.a, d.ID, f.actor, ReserveInput{1, uuid.New()})
		requireDraftCode(t, err, problem.DraftAlreadyExecuted)
		changed, err := f.store.Save(ctx, f.a, d.ID, f.otherActor, SaveDraftInput{DraftInput{"New title", "invalid newest SQL"}, 1})
		if err != nil || changed.Revision != 2 {
			t.Fatal("cannot edit draft after reservation")
		}
		_, err = s.Reserve(ctx, f.a, d.ID, f.actor, ReserveInput{1, uuid.New()})
		requireDraftCode(t, err, problem.RevisionConflict)
		_, err = s.Reserve(ctx, f.a, d.ID, f.actor, ReserveInput{1 << 40, uuid.New()})
		requireDraftCode(t, err, problem.RevisionConflict)
		if _, err := s.pool.Exec(ctx, `UPDATE databrain.database_targets SET name='Renamed target',config_revision=2,is_active=false WHERE id=$1`, f.a); err != nil {
			t.Fatal("cannot edit test target metadata")
		}
		replay, err := s.Reserve(ctx, f.a, d.ID, f.actor, input)
		if err != nil || replay.Created() || replay.Receipt.ID != r.Receipt.ID || replay.Receipt.State != "succeeded" {
			t.Fatal("old key was checked after latest draft or inactive target")
		}
		var storedSQL, title, target string
		if err := s.pool.QueryRow(ctx, `SELECT sql_snapshot,title_snapshot,target_name_snapshot FROM databrain.migration_attempts WHERE id=$1`, r.Receipt.ID).Scan(&storedSQL, &title, &target); err != nil || storedSQL != sql || title != d.Title || target != "Target A" {
			t.Fatal("later edits changed execution snapshot")
		}
	})
	t.Run("new requests validate resource active target and SQL without reservation", func(t *testing.T) {
		_, err := s.Reserve(ctx, f.a, d.ID, f.actor, ReserveInput{2, uuid.New()})
		requireDraftCode(t, err, problem.DatabaseNotFound)
		for _, target := range []uuid.UUID{f.b, uuid.New()} {
			_, err := s.Reserve(ctx, target, d.ID, f.actor, ReserveInput{1, uuid.New()})
			want := problem.DraftNotFound
			if target != f.b {
				want = problem.DatabaseNotFound
			}
			requireDraftCode(t, err, want)
		}
		for _, tc := range []struct {
			sql  string
			code problem.Code
		}{{"-- comment only", problem.SQLInvalid}, {"CREAT TABLE broken", problem.SQLInvalid}, {"BEGIN; CREATE TABLE x(id int); COMMIT;", problem.SQLUnsupported}} {
			bad := attemptDraft(t, f, f.b, tc.sql)
			_, err := s.Reserve(ctx, f.b, bad.ID, f.actor, ReserveInput{1, uuid.New()})
			requireDraftCode(t, err, tc.code)
		}
		var next, count int
		var idle bool
		if err := s.pool.QueryRow(ctx, `SELECT next_version,active_attempt_id IS NULL,(SELECT count(*) FROM databrain.migration_attempts WHERE target_id=$1) FROM databrain.migration_streams WHERE target_id=$1`, f.b).Scan(&next, &idle, &count); err != nil || next != 1 || !idle || count != 0 {
			t.Fatal("validation allocated a version or attempt")
		}
	})
	t.Run("new revision and another actor may reserve with their own key scope", func(t *testing.T) {
		if _, err := s.pool.Exec(ctx, `UPDATE databrain.database_targets SET is_active=true WHERE id=$1`, f.a); err != nil {
			t.Fatal("cannot reactivate isolated target")
		}
		changed, err := f.store.Save(ctx, f.a, d.ID, f.otherActor, SaveDraftInput{DraftInput{"New title", "CREATE TABLE public.still_not_run(id int)"}, 2})
		if err != nil {
			t.Fatal("cannot prepare new draft revision")
		}
		newer, err := s.Reserve(ctx, f.a, d.ID, f.otherActor, ReserveInput{changed.Revision, input.RequestKey})
		if err != nil || !newer.Created() || newer.Receipt.Version != "2" || newer.Receipt.DraftRevision != 3 || newer.Receipt.ActorID != f.otherActor {
			t.Fatal("new revision/actor was mistaken for old success or key")
		}
		var config int
		var username, target string
		if err := s.pool.QueryRow(ctx, `SELECT target_config_revision,executor_username_snapshot,target_name_snapshot FROM databrain.migration_attempts WHERE target_id=$1 AND id=$2`, f.a, newer.Receipt.ID).Scan(&config, &username, &target); err != nil || config != 2 || username != "draft_editor_b" || target != "Renamed target" {
			t.Fatal("new reservation did not capture current actor/target configuration")
		}
		replay, err := s.Reserve(ctx, f.a, d.ID, f.actor, input)
		if err != nil || replay.Created() || replay.Receipt.ID != r.Receipt.ID || replay.Receipt.State != "succeeded" {
			t.Fatal("busy target/latest revision hid an old receipt")
		}
		finishAttemptFixture(t, s, newer, false)
	})
	t.Run("failed retry advances version and unknown stays blocked", func(t *testing.T) {
		db := attemptDraft(t, f, f.b, "CREATE TABLE public.not_run(id int)")
		key := ReserveInput{1, input.RequestKey} // Key yang sama terpisah pada target lain.
		first := mustReserve(t, s, f, db, key)
		finishAttemptFixture(t, s, first, false)
		retry := mustReserve(t, s, f, db, ReserveInput{1, uuid.New()})
		if first.Receipt.Version != "1" || retry.Receipt.Version != "2" {
			t.Fatal("failed reservation did not consume its version")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE databrain.migration_attempts SET state='unknown',failure_code='PROCESS_INTERRUPTED',failure_message='Interrupted.' WHERE id=$1`, retry.Receipt.ID); err != nil {
			t.Fatal("cannot create unknown outcome fixture")
		}
		claimed, err := s.Claim(ctx, retry)
		if err != nil || claimed {
			t.Fatal("unknown attempt was claimed")
		}
		_, err = s.Reserve(ctx, f.b, db.ID, f.actor, ReserveInput{1, uuid.New()})
		requireDraftCode(t, err, problem.TargetBusy)
		for _, old := range []Reservation{first, retry} {
			replay, err := s.Reserve(ctx, f.b, db.ID, f.actor, ReserveInput{1, old.Receipt.RequestKey})
			if err != nil || replay.Created() || replay.Receipt.ID != old.Receipt.ID {
				t.Fatal("failed/unknown replay did not return old receipt")
			}
		}
	})
	t.Run("target unchanged and leases released", func(t *testing.T) {
		var marker string
		var relations int
		if err := f.target.QueryRow(ctx, `SELECT value FROM public.draft_marker`).Scan(&marker); err != nil || marker != "unchanged 猫" {
			t.Fatal("reservation changed target data")
		}
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`).Scan(&relations); err != nil || relations != 1 {
			t.Fatal("reservation executed target SQL")
		}
		if s.pool.Stat().AcquiredConns() != 0 {
			t.Fatal("attempt store leaked platform leases")
		}
	})
}

func waitAttemptBlockers(t *testing.T, pool *pgxpool.Pool, blocker, want int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var n int
		// PostgreSQL dapat mengantre penunggu lock kedua di belakang yang pertama; blocker langsung tidak
		// harus menunjuk PID penghalang.
		if err := pool.QueryRow(context.Background(), `SELECT count(DISTINCT pid) FROM pg_stat_activity
		    WHERE datname=current_database() AND pid<>$1 AND wait_event_type='Lock'
		    AND query LIKE 'SELECT next_version,active_attempt_id%'`, blocker).Scan(&n); err != nil {
			t.Fatal("cannot inspect independent lock waiters")
		}
		if n == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("requests did not reach stream lock on independent connections: %d waiters", n)
		case <-tick.C:
		}
	}
}

func TestAttemptConcurrencyPostgres(t *testing.T) {
	f, s := newAttemptFixture(t)
	ctx := context.Background()
	d := attemptDraft(t, f, f.a, "CREATE TABLE public.never_run(id int)")
	db := attemptDraft(t, f, f.b, d.SQL)
	type result struct {
		r   Reservation
		err error
	}
	for _, sameKey := range []bool{true, false} {
		name := "different keys"
		if sameKey {
			name = "same key"
		}
		t.Run(name, func(t *testing.T) {
			lock, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal("cannot begin barrier")
			}
			defer rollbackAttempt(lock)
			var pid int
			if err := lock.QueryRow(ctx, `SELECT pg_backend_pid() FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, f.a).Scan(&pid); err != nil {
				t.Fatal("cannot lock stream barrier")
			}
			key := uuid.New()
			start, results := make(chan struct{}), make(chan result, 2)
			pending := 2
			defer func() {
				rollbackAttempt(lock)
				if pending > 0 {
					for range pending {
						out := <-results
						if out.err == nil && out.r.Created() {
							finishAttemptFixture(t, s, out.r, false)
						}
					}
				}
			}()
			for i := range 2 {
				requestKey := key
				if !sameKey && i == 1 {
					requestKey = uuid.New()
				}
				go func() {
					<-start
					r, err := s.Reserve(ctx, f.a, d.ID, f.actor, ReserveInput{1, requestKey})
					results <- result{r, err}
				}()
			}
			close(start)
			waitAttemptBlockers(t, s.pool, pid, 2)
			// Stream lain harus selesai saat stream A masih terkunci.
			other, err := s.Reserve(ctx, f.b, db.ID, f.actor, ReserveInput{1, key})
			if err != nil || !other.Created() {
				t.Fatal("target B waited for target A")
			}
			finishAttemptFixture(t, s, other, false)
			if err := lock.Commit(ctx); err != nil {
				t.Fatal("cannot release barrier")
			}
			created, replayed, busy := 0, 0, 0
			var winner Reservation
			var replayID uuid.UUID
			for range 2 {
				out := <-results
				pending--
				if out.err != nil {
					requireDraftCode(t, out.err, problem.TargetBusy)
					busy++
				} else if out.r.Created() {
					created++
					winner = out.r
				} else {
					replayed++
					replayID = out.r.Receipt.ID
					if out.r.Receipt.RequestKey != key {
						t.Fatal("wrong replay key")
					}
				}
			}
			if created != 1 || sameKey && (replayed != 1 || busy != 0) || !sameKey && (busy != 1 || replayed != 0) {
				t.Fatal("concurrent requests created duplicate reservations")
			}
			wantVersion := "2"
			if sameKey {
				wantVersion = "1"
			}
			if winner.Receipt.Version != wantVersion || sameKey && replayID != winner.Receipt.ID {
				t.Fatal("concurrent reservation allocated incorrect version/receipt")
			}
			claims := make(chan bool, 2)
			for range 2 {
				go func() { claimed, err := s.Claim(ctx, winner); claims <- claimed && err == nil }()
			}
			one, two := <-claims, <-claims
			if one == two {
				t.Fatal("creator copies did not produce exactly one claim")
			}
			finishAttemptFixture(t, s, winner, false)
		})
	}
	t.Run("cancellation and stream update failure roll back atomically", func(t *testing.T) {
		lock, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal("cannot begin cancellation barrier")
		}
		defer rollbackAttempt(lock)
		if _, err := lock.Exec(ctx, `SELECT target_id FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, f.a); err != nil {
			t.Fatal("cannot lock stream")
		}
		canceled, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		defer stop()
		key := uuid.New()
		_, err = s.Reserve(canceled, f.a, d.ID, f.actor, ReserveInput{1, key})
		requireDraftCode(t, err, problem.PlatformUnavailable)
		rollbackAttempt(lock)
		if receipt, err := s.Receipt(ctx, f.a, f.actor, key); err != nil || receipt != nil {
			t.Fatal("canceled reservation persisted")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE databrain.migration_streams SET next_version=9223372036854775807 WHERE target_id=$1`, f.a); err != nil {
			t.Fatal("cannot prepare overflow fixture")
		}
		_, err = s.Reserve(ctx, f.a, d.ID, f.actor, ReserveInput{1, key})
		requireDraftCode(t, err, problem.PlatformUnavailable)
		var next int64
		var idle bool
		if err := s.pool.QueryRow(ctx, `SELECT next_version,active_attempt_id IS NULL FROM databrain.migration_streams WHERE target_id=$1`, f.a).Scan(&next, &idle); err != nil || next != 9223372036854775807 || !idle {
			t.Fatal("failed stream update changed stream")
		}
		if receipt, err := s.Receipt(ctx, f.a, f.actor, key); err != nil || receipt != nil {
			t.Fatal("failed stream update left an attempt")
		}
	})
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM databrain.migration_attempts WHERE target_id=$1`, f.a).Scan(&count); err != nil || count != 2 || s.pool.Stat().AcquiredConns() != 0 {
		t.Fatal("concurrency allocated extra attempts or leaked leases")
	}
}

// Gangguan transport membuang response satu COMMIT setelah diterima. PostgreSQL benar-benar commit;
// store harus mencari bukti lewat koneksi baru, tanpa menebak rollback atau mengulang SQL.
type lostCommitAckConn struct {
	net.Conn
	drop atomic.Bool
}

func (c *lostCommitAckConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.drop.Swap(false) {
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

type lostCommitAckTracer struct{ armed atomic.Bool }

func (tr *lostCommitAckTracer) TraceQueryStart(ctx context.Context, c *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == "commit" && tr.armed.Swap(false) {
		c.PgConn().Conn().(*lostCommitAckConn).drop.Store(true)
	}
	return ctx
}
func (*lostCommitAckTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAttemptCommitProofPostgres(t *testing.T) {
	f, original := newAttemptFixture(t)
	ctx := context.Background()
	cfg := original.pool.Config()
	tracer := &lostCommitAckTracer{}
	cfg.ConnConfig.Tracer = tracer
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &lostCommitAckConn{Conn: conn}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot open independent fault-test pool")
	}
	t.Cleanup(pool.Close)
	s := NewAttemptStore(pool)
	d := attemptDraft(t, f, f.a, "CREATE TABLE public.never_run(id int)")
	input := ReserveInput{1, uuid.New()}
	tracer.armed.Store(true)
	r := mustReserve(t, s, f, d, input)
	if tracer.armed.Load() {
		t.Fatal("reservation COMMIT fault did not fire")
	}
	if claimed, err := s.recoverClaim(r.Receipt); claimed || err == nil {
		t.Fatal("reserved receipt is not proof of running")
	}
	tracer.armed.Store(true)
	if claimed, err := s.Claim(ctx, r); !claimed || err != nil || tracer.armed.Load() {
		t.Fatalf("claim lost acknowledgment was not proved: %v", err)
	}
	if claimed, err := s.Claim(ctx, r); claimed || err != nil {
		t.Fatal("uncertain claim replayed")
	}
	finishAttemptFixture(t, s, r, false)
	other, err := s.recoverReservation(f.a, d.ID, f.actor, input.RequestKey, uuid.New(), 1)
	if err != nil || other.Created() || other.Receipt.ID != r.Receipt.ID {
		t.Fatal("existing receipt granted ownership to a different creator")
	}
	_, err = s.recoverReservation(f.a, d.ID, f.actor, uuid.New(), uuid.New(), 1)
	requireDraftCode(t, err, problem.PlatformUnavailable)

	// Deferred trigger nyata menolak COMMIT setelah INSERT/UPDATE berhasil. Trigger hanya dipasang oleh
	// owner pada database test terisolasi.
	cfgOwner := f.target.Config().Copy()
	cfgOwner.Database = pool.Config().ConnConfig.Database
	owner, err := pgx.ConnectConfig(ctx, cfgOwner)
	if err != nil {
		t.Fatal("cannot open fixture schema owner")
	}
	t.Cleanup(func() { closeDraftTestConn(owner) })
	if _, err := owner.Exec(ctx, `CREATE FUNCTION databrain.reject_test_commit() RETURNS trigger LANGUAGE plpgsql AS $$
	    BEGIN
	        IF NEW.title_snapshot='reject reservation commit' OR (NEW.title_snapshot='reject claim commit' AND NEW.state='running') THEN
	            RAISE EXCEPTION 'synthetic private commit failure';
	        END IF;
	        RETURN NEW;
	    END; $$;
	    CREATE CONSTRAINT TRIGGER reject_test_commit AFTER INSERT OR UPDATE ON databrain.migration_attempts
	    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION databrain.reject_test_commit();`); err != nil {
		t.Fatal("cannot initialize deferred failure fixture")
	}
	bad := attemptDraft(t, f, f.a, d.SQL)
	if _, err := pool.Exec(ctx, `UPDATE databrain.migration_drafts SET title='reject reservation commit' WHERE id=$1`, bad.ID); err != nil {
		t.Fatal("cannot configure reservation rejection")
	}
	key := uuid.New()
	_, err = s.Reserve(ctx, f.a, bad.ID, f.actor, ReserveInput{1, key})
	requireDraftCode(t, err, problem.PlatformUnavailable)
	if receipt, err := s.Receipt(ctx, f.a, f.actor, key); err != nil || receipt != nil {
		t.Fatal("failed commit left a receipt")
	}
	if _, err := pool.Exec(ctx, `UPDATE databrain.migration_drafts SET title='reject claim commit' WHERE id=$1`, bad.ID); err != nil {
		t.Fatal("cannot configure claim rejection")
	}
	claim := mustReserve(t, s, f, bad, ReserveInput{1, key})
	if claim.Receipt.Version != "2" {
		t.Fatal("rolled-back reservation consumed version")
	}
	claimed, err := s.Claim(ctx, claim)
	if claimed {
		t.Fatal("failed claim commit authorized SQL")
	}
	requireDraftCode(t, err, problem.PlatformUnavailable)
	stored, err := s.Receipt(ctx, f.a, f.actor, key)
	if err != nil || stored == nil || stored.State != "reserved" {
		t.Fatal("failed claim changed durable state")
	}
	if claimed, err := s.Claim(ctx, claim); claimed || err != nil {
		t.Fatal("inconclusive claim permit was reused")
	}
	pool.Close()
	_, err = s.recoverReservation(f.a, bad.ID, f.actor, key, claim.Receipt.ID, 1)
	requireDraftCode(t, err, problem.PlatformUnavailable)
	if claimed, err := s.recoverClaim(claim.Receipt); claimed || err == nil {
		t.Fatal("unavailable proof authorized SQL")
	}
}
