package migration

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type draftFixture struct {
	db                      *platformdb.DB
	store                   *DraftStore
	target                  *pgx.Conn
	actor, otherActor, a, b uuid.UUID
}

func closeDraftTestConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

func newDraftFixture(t *testing.T) draftFixture {
	t.Helper()
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	if raw == "" {
		t.Skip("set DATABRAIN_TEST_ADMIN_URL for the verified A-03 disposable fixture")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.User == nil || u.User.Username() != "databrain_fixture_owner" || u.Path != "/postgres" {
		t.Fatal("test requires the A-03 fixture owner on 127.0.0.1:55432/postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("cannot connect to disposable fixture")
	}
	t.Cleanup(func() { closeDraftTestConn(owner) })
	var directory, role, database, system string
	err = owner.QueryRow(ctx, `SELECT current_setting('data_directory'),current_user,current_database(),system_identifier::text FROM pg_control_system()`).Scan(&directory, &role, &database, &system)
	expected, pathErr := filepath.Abs("../../../.tmp/postgres/data")
	if err != nil || pathErr != nil || !strings.EqualFold(filepath.Clean(directory), filepath.Clean(expected)) || role != "databrain_fixture_owner" || database != "postgres" || system != "7699504395596375996" {
		t.Fatal("fixture identity mismatch; mutation refused")
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	names := []string{"db_draft_platform_" + suffix, "db_draft_target_" + suffix}
	for i, name := range names {
		dbOwner := "databrain_fixture_owner"
		if i == 0 {
			dbOwner = "databrain_migrator"
		}
		quoted := pgx.Identifier{name}.Sanitize()
		if _, err := owner.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER "+pgx.Identifier{dbOwner}.Sanitize()); err != nil {
			t.Fatal("cannot create isolated draft test database")
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var actual string
			if err := owner.QueryRow(cleanup, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", name).Scan(&actual); err != nil || actual != dbOwner {
				t.Error("database ownership changed; cleanup refused")
				return
			}
			if _, err := owner.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
				t.Error("cannot drop this test's database; check leaked connections")
			}
		})
		if _, err := owner.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC"); err != nil {
			t.Fatal("cannot isolate test database")
		}
	}
	if _, err := owner.Exec(ctx, "GRANT CONNECT ON DATABASE "+pgx.Identifier{names[0]}.Sanitize()+" TO databrain_runtime"); err != nil {
		t.Fatal("cannot grant runtime access to test platform")
	}
	u.Path = "/" + names[0]
	u.User = url.UserPassword("databrain_migrator", os.Getenv("DATABRAIN_MIGRATOR_PASSWORD"))
	if _, err := migrations.Up(u.String()); err != nil {
		t.Fatal("test migration failed")
	}
	u.User = url.UserPassword("databrain_runtime", os.Getenv("DATABRAIN_RUNTIME_PASSWORD"))
	db, err := platformdb.Open(ctx, u.String())
	if err != nil {
		t.Fatal("cannot open runtime platform pool")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := db.Close(cleanup); err != nil {
			t.Error("draft pool failed to close")
		}
	})
	u, _ = url.Parse(raw)
	u.Path = "/" + names[1]
	target, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal("cannot inspect isolated target")
	}
	t.Cleanup(func() { closeDraftTestConn(target) })
	if _, err := target.Exec(ctx, "CREATE TABLE public.draft_marker(value text); INSERT INTO public.draft_marker VALUES ('unchanged 猫')"); err != nil {
		t.Fatal("cannot initialize isolated target marker")
	}
	f := draftFixture{db: db, store: NewDraftStore(db.Pool), target: target, actor: uuid.New(), otherActor: uuid.New(), a: uuid.New(), b: uuid.New()}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO databrain.users(id,username,display_name,password_hash)
	    VALUES ($1,'draft_editor_a','Editor A','synthetic-not-a-login-hash'),($2,'draft_editor_b','Editor B','synthetic-not-a-login-hash')`, f.actor, f.otherActor); err != nil {
		t.Fatal("cannot create synthetic draft actors")
	}
	// Credentials are intentionally unusable. Target b is also offline; saving
	// drafts must work without a Provider, decryption or network probe.
	if _, err := db.Pool.Exec(ctx, `INSERT INTO databrain.database_targets
	    (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
	    VALUES ($1,'Target A','127.0.0.1',55432,$3,'databrain_target',decode(repeat('00',30),'hex'),'disable',$4),
	           ($2,'Offline B','127.0.0.1',1,'offline','databrain_target',decode(repeat('00',30),'hex'),'disable',$4)`, f.a, f.b, names[1], f.actor); err != nil {
		t.Fatal("cannot create scoped target metadata")
	}
	var canConnect bool
	if err := db.Pool.QueryRow(ctx, "SELECT has_database_privilege(current_user,$1,'CONNECT')", names[1]).Scan(&canConnect); err != nil || canConnect {
		t.Fatal("platform runtime must not connect to the target")
	}
	return f
}

func TestDraftStorePostgres(t *testing.T) {
	f := newDraftFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := f.store
	input := DraftInput{Title: "  Perubahan 猫 \t", SQL: " \r\n-- 猫 invalid SQL\r\nCREAT TABLE ';';\t\n "}
	d, err := s.Create(ctx, f.a, f.actor, input)
	if err != nil || d.ID == uuid.Nil || d.TargetID != f.a || d.Title != input.Title || d.SQL != input.SQL || d.Revision != 1 || d.CreatedBy != f.actor || d.UpdatedBy != f.actor || d.CreatedAt.IsZero() || !d.CreatedAt.Equal(d.UpdatedAt) || d.CreatedAt.Location() != time.UTC {
		t.Fatalf("create did not preserve exact input/audit metadata: %v", err)
	}
	t.Run("read update and stale revision", func(t *testing.T) {
		read, err := s.Get(ctx, f.a, d.ID)
		if err != nil || read != d {
			t.Fatal("detail differs from stored draft")
		}
		changed := DraftInput{"  valid SQL stays draft  ", "CREATE TABLE public.never_executed(id int); UPDATE public.draft_marker SET value='changed';"}
		saved, err := s.Save(ctx, f.a, d.ID, f.otherActor, SaveDraftInput{changed, 1})
		if err != nil || saved.Revision != 2 || saved.SQL != changed.SQL || saved.Title != changed.Title || saved.CreatedBy != f.actor || saved.UpdatedBy != f.otherActor || !saved.CreatedAt.Equal(d.CreatedAt) || saved.UpdatedAt.Before(d.UpdatedAt) || saved.UpdatedAt.Location() != time.UTC {
			t.Fatalf("save did not preserve revision/input/audit metadata: %v", err)
		}
		_, err = s.Save(ctx, f.a, d.ID, f.actor, SaveDraftInput{input, 1})
		requireDraftCode(t, err, problem.RevisionConflict)
		_, err = s.Save(ctx, f.a, d.ID, f.actor, SaveDraftInput{input, 1 << 40})
		requireDraftCode(t, err, problem.RevisionConflict)
		read, err = s.Get(ctx, f.a, d.ID)
		if err != nil || read != saved {
			t.Fatal("stale save overwrote newer input")
		}
	})
	t.Run("target scope and missing resources", func(t *testing.T) {
		_, err := s.Get(ctx, f.b, d.ID)
		requireDraftCode(t, err, problem.DraftNotFound)
		_, err = s.Save(ctx, f.b, d.ID, f.actor, SaveDraftInput{input, 2})
		requireDraftCode(t, err, problem.DraftNotFound)
		_, err = s.Get(ctx, f.a, uuid.New())
		requireDraftCode(t, err, problem.DraftNotFound)
		_, err = s.Save(ctx, f.a, uuid.New(), f.actor, SaveDraftInput{input, 1})
		requireDraftCode(t, err, problem.DraftNotFound)
		_, err = s.Create(ctx, uuid.New(), f.actor, input)
		requireDraftCode(t, err, problem.DatabaseNotFound)
		_, err = s.Create(ctx, f.a, uuid.New(), input)
		requireDraftCode(t, err, problem.ValidationFailed)
		if _, err := s.Create(ctx, f.b, f.actor, input); err != nil {
			t.Fatal("draft save required an online target")
		}
	})
	t.Run("bounded summary pagination", func(t *testing.T) {
		large := DraftInput{strings.Repeat("猫", 120), strings.Repeat("猫", MaxSQLBytes/3) + "aa"}
		latest, err := s.Create(ctx, f.a, f.actor, large)
		if err != nil || latest.SQL != large.SQL || latest.Title != large.Title {
			t.Fatal("maximum valid input was not preserved")
		}
		// Equal timestamps exercise the UUID tie breaker deterministically.
		if _, err := s.pool.Exec(ctx, "UPDATE databrain.migration_drafts SET updated_at='2026-01-01T00:00:00Z' WHERE target_id=$1", f.a); err != nil {
			t.Fatal("cannot set this test's ordering fixture")
		}
		first, more, err := s.List(ctx, f.a, 1, 0)
		if err != nil || len(first) != 1 || !more || first[0].TargetID != f.a {
			t.Fatal("first summary page is incorrect")
		}
		second, more, err := s.List(ctx, f.a, 1, 1)
		if err != nil || len(second) != 1 || more || second[0].TargetID != f.a || strings.Compare(first[0].ID.String(), second[0].ID.String()) >= 0 {
			t.Fatal("summary tie breaker/lookahead is incorrect")
		}
		if _, err := s.Save(ctx, f.a, latest.ID, f.actor, SaveDraftInput{large, 1}); err != nil {
			t.Fatal("cannot update latest draft")
		}
		items, more, err := s.List(ctx, f.a, 200, 0)
		if err != nil || len(items) != 2 || more || items[0].ID != latest.ID || items[0].Revision != 2 {
			t.Fatal("updated_at descending order is incorrect")
		}
		for _, offset := range []int{2, 100000} {
			items, more, err := s.List(ctx, f.a, 50, offset)
			if err != nil || len(items) != 0 || more {
				t.Fatal("empty summary page is incorrect")
			}
		}
		items, more, err = s.List(ctx, f.b, 50, 0)
		if err != nil || len(items) != 1 || more || items[0].TargetID != f.b {
			t.Fatal("list leaked another target's drafts")
		}
	})
	t.Run("concurrent writers cannot overwrite", func(t *testing.T) {
		current, err := s.Get(ctx, f.a, d.ID)
		if err != nil {
			t.Fatal("cannot read concurrency fixture")
		}
		start := make(chan struct{})
		type result struct {
			draft Draft
			err   error
		}
		results := make(chan result, 2)
		for _, actor := range []uuid.UUID{f.actor, f.otherActor} {
			go func(actor uuid.UUID) {
				<-start
				draft, err := s.Save(ctx, f.a, d.ID, actor, SaveDraftInput{DraftInput{actor.String(), actor.String()}, current.Revision})
				results <- result{draft, err}
			}(actor)
		}
		close(start)
		success, conflicts := 0, 0
		var winner Draft
		for range 2 {
			r := <-results
			if r.err == nil {
				success++
				winner = r.draft
			} else {
				requireDraftCode(t, r.err, problem.RevisionConflict)
				conflicts++
			}
		}
		stored, err := s.Get(ctx, f.a, d.ID)
		if err != nil || success != 1 || conflicts != 1 || stored != winner || stored.Revision != current.Revision+1 || stored.Title != stored.UpdatedBy.String() || stored.SQL != stored.Title {
			t.Fatal("concurrent save lost an update or changed revision twice")
		}
	})
	t.Run("failure and cancellation preserve stored draft", func(t *testing.T) {
		before, err := s.Get(ctx, f.a, d.ID)
		if err != nil {
			t.Fatal("cannot read draft")
		}
		_, err = s.Save(ctx, f.a, d.ID, uuid.New(), SaveDraftInput{input, before.Revision})
		requireDraftCode(t, err, problem.ValidationFailed)
		canceled, stop := context.WithCancel(ctx)
		stop()
		_, err = s.Save(canceled, f.a, d.ID, f.actor, SaveDraftInput{input, before.Revision})
		requireDraftCode(t, err, problem.PlatformUnavailable)
		after, err := s.Get(ctx, f.a, d.ID)
		if err != nil || before != after {
			t.Fatal("failed save changed the draft")
		}
	})
	t.Run("target and execution state unchanged", func(t *testing.T) {
		var value string
		var relations, attempts int
		if err := f.target.QueryRow(ctx, "SELECT value FROM public.draft_marker").Scan(&value); err != nil || value != "unchanged 猫" {
			t.Fatal("draft save changed target data")
		}
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`).Scan(&relations); err != nil || relations != 1 {
			t.Fatal("draft save changed target schema")
		}
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.migration_attempts").Scan(&attempts); err != nil || attempts != 0 {
			t.Fatal("draft save created an execution attempt")
		}
		if s.pool.Stat().AcquiredConns() != 0 {
			t.Fatal("draft store leaked a platform lease")
		}
	})
}
