package migration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type runnerFixture struct {
	draftFixture
	runner                                   *Runner
	guard                                    access.Authorizer
	observer, second, schema                 *pgx.Conn
	c, editGroup                             uuid.UUID
	editor, otherEditor, viewer, none, admin access.Principal
}

func runnerSQL(t *testing.T, db platformdb.DBTX, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal("isolated runner fixture statement failed")
	}
}

func newRunnerFixture(t *testing.T) runnerFixture {
	t.Helper()
	f, store := newAttemptFixture(t) // Pemeriksaan owner, path, dan identitas fixture tetap aktif.
	ctx := context.Background()
	secrets, err := secret.New(bytes.Repeat([]byte{0x82}, 32))
	if err != nil {
		t.Fatal(err)
	}
	password := os.Getenv("DATABRAIN_TARGET_PASSWORD")
	if password == "" {
		t.Fatal("disposable target credential required")
	}
	sealed, err := secrets.SealTarget(f.a, []byte(password))
	if err != nil {
		t.Fatal("cannot seal synthetic target credential")
	}
	runnerSQL(t, f.store.pool, `UPDATE databrain.database_targets SET password_sealed=$2 WHERE id=$1`, f.a, sealed)
	sealed, err = secrets.SealTarget(f.b, []byte(password))
	if err != nil {
		t.Fatal("cannot seal offline target credential")
	}
	runnerSQL(t, f.store.pool, `UPDATE databrain.database_targets SET password_sealed=$2 WHERE id=$1`, f.b, sealed)
	aName := f.target.Config().Database
	runnerSQL(t, f.target, "GRANT CONNECT ON DATABASE "+pgx.Identifier{aName}.Sanitize()+" TO databrain_target")
	runnerSQL(t, f.target, "GRANT USAGE,CREATE ON SCHEMA public TO databrain_target; GRANT ALL ON ALL TABLES IN SCHEMA public TO databrain_target")
	cName := "db_runner_target_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	runnerSQL(t, f.target, "CREATE DATABASE "+pgx.Identifier{cName}.Sanitize()+" OWNER databrain_target")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var owner string
		if err := f.target.QueryRow(cleanup, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1`, cName).Scan(&owner); err != nil || owner != "databrain_target" {
			t.Error("second runner target ownership changed; cleanup refused")
			return
		}
		if _, err := f.target.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{cName}.Sanitize()); err != nil {
			t.Error("cannot drop this test's second target; check leaked connections")
		}
	})
	runnerSQL(t, f.target, "REVOKE ALL ON DATABASE "+pgx.Identifier{cName}.Sanitize()+" FROM PUBLIC")
	cfg := f.target.Config().Copy()
	cfg.Database = cName
	second, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot inspect second target")
	}
	t.Cleanup(func() { closeDraftTestConn(second) })
	cfg = f.target.Config().Copy()
	cfg.Database = "postgres"
	observer, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot inspect isolated target connections")
	}
	t.Cleanup(func() { closeDraftTestConn(observer) })
	cfg = f.target.Config().Copy()
	cfg.Database = f.store.pool.Config().ConnConfig.Database
	schema, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot prepare isolated platform failure fixtures")
	}
	t.Cleanup(func() { closeDraftTestConn(schema) })
	provider, err := targets.NewProvider(f.store.pool, secrets, config.TargetSettings{AppEnv: "test", AllowedHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	cTarget, err := provider.Create(ctx, f.actor, targets.CreateInput{Name: "Runner C", Host: "127.0.0.1", Port: 55432, DatabaseName: cName, Username: "databrain_target", Password: password, SSLMode: "disable"})
	if err != nil {
		t.Fatal("cannot create real second target configuration")
	}
	guard, err := access.NewAuthorizer(f.store.pool)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, provider, guard)
	if err != nil {
		t.Fatal(err)
	}
	viewer, none, admin := uuid.New(), uuid.New(), uuid.New()
	for i, user := range []uuid.UUID{viewer, none, admin} {
		runnerSQL(t, f.store.pool, `INSERT INTO databrain.users(id,username,display_name,password_hash,is_platform_admin) VALUES ($1,$2,'Runner test','not-a-login-hash',$3)`, user, fmt.Sprintf("runner_actor_%d", i), user == admin)
	}
	editGroup, viewGroup := uuid.New(), uuid.New()
	runnerSQL(t, f.store.pool, `INSERT INTO databrain.groups(id,name) VALUES ($1,'Runner editors'),($2,'Runner viewers')`, editGroup, viewGroup)
	runnerSQL(t, f.store.pool, `INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$2),($1,$3),($4,$2),($4,$5)`, editGroup, f.actor, f.otherActor, viewGroup, viewer)
	for _, target := range []uuid.UUID{f.a, f.b, cTarget.ID} {
		runnerSQL(t, f.store.pool, `INSERT INTO databrain.database_grants(group_id,target_id,access_level) VALUES ($1,$2,'view_edit'),($3,$2,'view_only')`, editGroup, target, viewGroup)
	}
	service, err := auth.NewService(f.store.pool, secrets, auth.Policy{AppEnv: "test", AppOrigin: "http://localhost:18082"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal := func(user uuid.UUID) access.Principal {
		state, token, err := service.CreateAnonymous(ctx)
		if err != nil {
			t.Fatal("cannot initialize isolated session")
		}
		runnerSQL(t, f.store.pool, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, user)
		validated, _, err := service.Validate(ctx, token, false)
		if err != nil {
			t.Fatal("real session validation failed")
		}
		return validated.Principal
	}
	return runnerFixture{draftFixture: f, runner: runner, guard: guard, observer: observer, second: second, schema: schema, c: cTarget.ID, editGroup: editGroup, editor: principal(f.actor), otherEditor: principal(f.otherActor), viewer: principal(viewer), none: principal(none), admin: principal(admin)}
}

type storedRunnerOutcome struct {
	state, evidence, code, sqlstate   string
	index                             int
	active, commit, finished, started bool
}

func runnerOutcome(t *testing.T, f runnerFixture, r AttemptReceipt) storedRunnerOutcome {
	t.Helper()
	var got storedRunnerOutcome
	err := f.store.pool.QueryRow(context.Background(), `SELECT a.state,COALESCE(a.outcome_evidence,''),COALESCE(a.failure_code,''),COALESCE(a.failure_sqlstate,''),COALESCE(a.failure_statement_index,0),
	    s.active_attempt_id IS NOT NULL,a.commit_started_at IS NOT NULL,a.finished_at IS NOT NULL,a.started_at IS NOT NULL
	    FROM databrain.migration_attempts a JOIN databrain.migration_streams s ON s.target_id=a.target_id WHERE a.target_id=$1 AND a.id=$2`, r.TargetID, r.ID).
		Scan(&got.state, &got.evidence, &got.code, &got.sqlstate, &got.index, &got.active, &got.commit, &got.finished, &got.started)
	if err != nil {
		t.Fatal("cannot read isolated execution result")
	}
	return got
}

func executeRunnerDraft(t *testing.T, f runnerFixture, target uuid.UUID, sql string) (Draft, ReserveInput, AttemptReceipt) {
	t.Helper()
	d := attemptDraft(t, f.draftFixture, target, sql)
	input := ReserveInput{1, uuid.New()}
	r, err := f.runner.Execute(context.Background(), f.editor, target, d.ID, input)
	if err != nil || r.ID == uuid.Nil {
		t.Fatalf("execute did not return a readable receipt: %v", err)
	}
	return d, input, r
}

func TestRunnerPostgres(t *testing.T) {
	f := newRunnerFixture(t)
	ctx := context.Background()
	t.Run("one transaction commits all statements and replays snapshot", func(t *testing.T) {
		sql := "\r\n-- 猫\r\nCREATE TABLE runner_committed(id int PRIMARY KEY,value text);\r\nINSERT INTO public.runner_committed VALUES(1,'猫;a'); UPDATE public.runner_committed SET value=value||';b'; CREATE TABLE runner_settings(statement_timeout text,lock_timeout text,path text,app text); INSERT INTO runner_settings SELECT current_setting('statement_timeout'),current_setting('lock_timeout'),current_setting('search_path'),current_setting('application_name');"
		d, input, r := executeRunnerDraft(t, f, f.a, sql)
		got := runnerOutcome(t, f, r)
		if r.State != "succeeded" || got.state != "succeeded" || got.evidence != "commit_ack" || got.active || !got.commit || !got.finished || !got.started {
			t.Fatal("commit outcome or stream is incorrect")
		}
		var value, stored string
		if err := f.target.QueryRow(ctx, `SELECT value FROM public.runner_committed WHERE id=1`).Scan(&value); err != nil || value != "猫;a;b" {
			t.Fatal("target did not commit the complete script")
		}
		var statementTimeout, lockTimeout, path, app string
		if err := f.target.QueryRow(ctx, `SELECT * FROM public.runner_settings`).Scan(&statementTimeout, &lockTimeout, &path, &app); err != nil || statementTimeout != "30s" || lockTimeout != "3s" || path != "public" || app != "databrain:"+r.ID.String() {
			t.Fatal("target transaction settings/application name are incorrect")
		}
		if err := f.store.pool.QueryRow(ctx, `SELECT sql_snapshot FROM databrain.migration_attempts WHERE target_id=$1 AND id=$2`, f.a, r.ID).Scan(&stored); err != nil || stored != sql {
			t.Fatal("executed snapshot differs from exact SQL")
		}
		_, err := f.store.Save(ctx, f.a, d.ID, f.otherActor, SaveDraftInput{DraftInput{"changed", "INVALID newest SQL"}, 1})
		if err != nil {
			t.Fatal(err)
		}
		replay, err := f.runner.Execute(ctx, f.editor, f.a, d.ID, input)
		if err != nil || replay.ID != r.ID || replay.State != "succeeded" {
			t.Fatal("same-key replay used latest draft or executed again")
		}
		if err := f.target.QueryRow(ctx, `SELECT count(*)::text||':'||min(value) FROM public.runner_committed`).Scan(&value); err != nil || value != "1:猫;a;b" {
			t.Fatal("replay changed target data")
		}
	})
	t.Run("last statement failure rolls back earlier DDL and DML", func(t *testing.T) {
		_, _, r := executeRunnerDraft(t, f, f.a, `CREATE TABLE public.runner_rolled_back(id int); UPDATE public.runner_committed SET value='must roll back'; INSERT INTO public.runner_missing VALUES(1);`)
		got := runnerOutcome(t, f, r)
		if r.State != "failed" || got.evidence != "rollback_ack" || got.code != "SQL_ERROR" || got.sqlstate != "42P01" || got.index != 3 || got.active || got.commit || !got.finished {
			t.Fatal("rollback outcome/diagnostics incorrect")
		}
		var absent bool
		var value string
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.runner_rolled_back') IS NULL,(SELECT value FROM public.runner_committed WHERE id=1)`).Scan(&absent, &value); err != nil || !absent || value != "猫;a;b" {
			t.Fatal("late failure left partial target changes")
		}
	})
	t.Run("deferred target constraint gives definitive server rollback", func(t *testing.T) {
		_, _, r := executeRunnerDraft(t, f, f.a, `CREATE TABLE public.runner_parent(id int PRIMARY KEY); CREATE TABLE public.runner_child(id int REFERENCES public.runner_parent(id) DEFERRABLE INITIALLY DEFERRED); INSERT INTO public.runner_child VALUES(9);`)
		got := runnerOutcome(t, f, r)
		if r.State != "failed" || got.evidence != "server_rollback" || got.sqlstate != "23503" || got.active || !got.commit {
			t.Fatal("deferred constraint commit outcome was misclassified")
		}
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.runner_parent') IS NULL AND to_regclass('public.runner_child') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("rejected COMMIT left target tables")
		}
	})
	t.Run("denial before reservation cannot allocate an attempt", func(t *testing.T) {
		d := attemptDraft(t, f.draftFixture, f.a, `CREATE TABLE public.runner_denied(id int)`)
		for _, tc := range []struct {
			p    access.Principal
			code problem.Code
		}{{access.Principal{}, problem.SessionInvalid}, {f.viewer, problem.DatabaseAccessDenied}, {f.none, problem.DatabaseNotFound}, {f.admin, problem.DatabaseNotFound}} {
			key := uuid.New()
			_, err := f.runner.Execute(ctx, tc.p, f.a, d.ID, ReserveInput{1, key})
			requireDraftCode(t, err, tc.code)
			var n int
			if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM databrain.migration_attempts WHERE target_id=$1 AND request_key=$2`, f.a, key).Scan(&n); err != nil || n != 0 {
				t.Fatal("denied call created an attempt")
			}
		}
		_, err := f.runner.Execute(ctx, f.editor, f.c, d.ID, ReserveInput{1, uuid.New()})
		requireDraftCode(t, err, problem.DraftNotFound)
	})
	t.Run("connection failure does not begin SQL", func(t *testing.T) {
		_, _, r := executeRunnerDraft(t, f, f.b, `CREATE TABLE public.runner_offline(id int)`)
		got := runnerOutcome(t, f, r)
		if r.State != "failed" || got.code != "CONNECTION_FAILED" || got.evidence != "not_started" || got.active || got.commit {
			t.Fatal("offline target outcome incorrect")
		}
	})
	t.Run("guard checks revocation immediately before BEGIN", func(t *testing.T) {
		var calls atomic.Int32
		wrapped := &runnerCheckingGuard{Authorizer: f.guard, before: func() {
			if calls.Add(1) == 2 {
				runnerSQL(t, f.store.pool, `UPDATE databrain.sessions SET revoked_at=now() WHERE id=$1`, f.otherEditor.SessionID)
			}
		}}
		runner, err := NewRunner(f.runner.store, f.runner.provider, wrapped)
		if err != nil {
			t.Fatal(err)
		}
		d := attemptDraft(t, f.draftFixture, f.a, `CREATE TABLE public.runner_revoked(id int)`)
		r, err := runner.Execute(ctx, f.otherEditor, f.a, d.ID, ReserveInput{1, uuid.New()})
		if err != nil || r.State != "failed" || calls.Load() != 2 {
			t.Fatal("second real guard check did not deny")
		}
		got := runnerOutcome(t, f, r)
		if got.code != "ACCESS_REVOKED" || got.evidence != "not_started" || got.active || got.commit {
			t.Fatal("revocation outcome incorrect")
		}
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.runner_revoked') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("revoked call touched target")
		}
		_, err = runner.Execute(ctx, f.otherEditor, f.a, d.ID, ReserveInput{1, r.RequestKey})
		requireDraftCode(t, err, problem.SessionInvalid)
	})
	t.Run("new draft edits cannot replace a claimed execution snapshot", func(t *testing.T) {
		d := attemptDraft(t, f.draftFixture, f.a, `CREATE TABLE public.runner_snapshot(id int); INSERT INTO public.runner_snapshot VALUES(1)`)
		var calls atomic.Int32
		wrapped := &runnerCheckingGuard{Authorizer: f.guard, before: func() {
			if calls.Add(1) == 2 {
				_, err := f.store.Save(ctx, f.a, d.ID, f.actor, SaveDraftInput{DraftInput{"new title", `CREATE TABLE public.runner_wrong_snapshot(id int)`}, 1})
				if err != nil {
					t.Fatal(err)
				}
			}
		}}
		runner, err := NewRunner(f.runner.store, f.runner.provider, wrapped)
		if err != nil {
			t.Fatal(err)
		}
		r, err := runner.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{1, uuid.New()})
		if err != nil || r.State != "succeeded" || calls.Load() != 2 {
			t.Fatal("claimed snapshot did not execute")
		}
		var n int
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.runner_snapshot),to_regclass('public.runner_wrong_snapshot') IS NULL`).Scan(&n, &absent); err != nil || n != 1 || !absent {
			t.Fatal("runner executed latest draft instead of snapshot")
		}
	})
	t.Run("mark commit failure rolls back instead of sending COMMIT", func(t *testing.T) {
		runnerSQL(t, f.schema, `CREATE FUNCTION databrain.runner_reject_phase() RETURNS trigger LANGUAGE plpgsql AS $$
		    BEGIN IF NEW.commit_started_at IS NOT NULL THEN RAISE EXCEPTION 'synthetic private phase failure'; END IF; RETURN NEW; END $$;
		    CREATE TRIGGER runner_reject_phase BEFORE UPDATE ON databrain.migration_attempts FOR EACH ROW EXECUTE FUNCTION databrain.runner_reject_phase();`)
		defer runnerSQL(t, f.schema, `DROP TRIGGER runner_reject_phase ON databrain.migration_attempts; DROP FUNCTION databrain.runner_reject_phase()`)
		_, _, r := executeRunnerDraft(t, f, f.a, `CREATE TABLE public.runner_phase_rejected(id int); INSERT INTO public.runner_phase_rejected VALUES(1);`)
		got := runnerOutcome(t, f, r)
		if r.State != "failed" || got.code != "OUTCOME_PERSISTENCE_FAILED" || got.evidence != "rollback_ack" || got.commit || got.active {
			t.Fatal("phase persistence failure allowed target COMMIT")
		}
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.runner_phase_rejected') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("phase persistence failure left target effects")
		}
	})
	t.Run("unrecorded outcome remains unknown and blocked after actual target commit", func(t *testing.T) {
		runnerSQL(t, f.schema, `CREATE FUNCTION databrain.runner_reject_success() RETURNS trigger LANGUAGE plpgsql AS $$
		    BEGIN IF NEW.state='succeeded' THEN RAISE EXCEPTION 'synthetic private outcome failure'; END IF; RETURN NEW; END $$;
		    CREATE TRIGGER runner_reject_success BEFORE UPDATE ON databrain.migration_attempts FOR EACH ROW EXECUTE FUNCTION databrain.runner_reject_success();`)
		defer runnerSQL(t, f.schema, `DROP TRIGGER runner_reject_success ON databrain.migration_attempts; DROP FUNCTION databrain.runner_reject_success()`)
		d, input, r := executeRunnerDraft(t, f, f.a, `CREATE TABLE public.runner_outcome_lost(id int); INSERT INTO public.runner_outcome_lost VALUES(1)`)
		got := runnerOutcome(t, f, r)
		if r.State != "unknown" || got.code != "OUTCOME_PERSISTENCE_FAILED" || !got.active || !got.commit || got.finished || got.evidence != "" {
			t.Fatal("outcome persistence failure advertised a terminal result or unblocked target")
		}
		var n int
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.runner_outcome_lost`).Scan(&n); err != nil || n != 1 {
			t.Fatal("target commit was not actually applied")
		}
		replay, err := f.runner.Execute(ctx, f.editor, f.a, d.ID, input)
		if err != nil || replay.ID != r.ID || replay.State != "unknown" {
			t.Fatal("unknown replay was not receipt-only")
		}
		_, err = f.runner.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{1, uuid.New()})
		requireDraftCode(t, err, problem.TargetBusy)
		if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.runner_outcome_lost`).Scan(&n); err != nil || n != 1 {
			t.Fatal("unknown replay repeated target SQL")
		}
	})
	if len(f.runner.slots) != 0 || f.store.pool.Stat().AcquiredConns() != 0 {
		t.Fatal("runner leaked admission or platform resources")
	}
}

// Hook membungkus Authorizer asli tanpa mengganti hasil otorisasinya.
type runnerCheckingGuard struct {
	access.Authorizer
	before func()
}

func (g *runnerCheckingGuard) RequireDatabase(ctx context.Context, p access.Principal, id uuid.UUID, action access.Action) (access.Level, error) {
	g.before()
	return g.Authorizer.RequireDatabase(ctx, p, id, action)
}

func waitRunnerTarget(t *testing.T, f runnerFixture, target, key uuid.UUID, waitEvent string) AttemptReceipt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		r, err := f.runner.store.Receipt(ctx, target, f.actor, key)
		if err == nil && r != nil {
			var ready bool
			if err := f.observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type=$2)`, "databrain:"+r.ID.String(), waitEvent).Scan(&ready); err == nil && ready {
				return *r
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("runner did not reach the target barrier")
		case <-tick.C:
		}
	}
}

func TestRunnerConcurrencyPostgres(t *testing.T) {
	f := newRunnerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runnerSQL(t, f.target, `CREATE TABLE public.runner_order(id int PRIMARY KEY,value int); INSERT INTO public.runner_order VALUES(1,0); GRANT ALL ON public.runner_order TO databrain_target`)
	runnerSQL(t, f.second, `CREATE TABLE public.runner_order(id int PRIMARY KEY,value int); INSERT INTO public.runner_order VALUES(1,0); GRANT ALL ON public.runner_order TO databrain_target`)
	d := attemptDraft(t, f.draftFixture, f.a, `UPDATE public.runner_order SET value=value+1 WHERE id=1`)
	dc := attemptDraft(t, f.draftFixture, f.c, `UPDATE public.runner_order SET value=value+1 WHERE id=1`)
	if _, err := ValidateSQL(d.SQL); err != nil {
		t.Fatal(err)
	} // Siapkan parser sebelum mengukur penantian lock.
	lockA, err := f.target.Begin(ctx)
	if err != nil {
		t.Fatal("cannot create target barrier")
	}
	defer rollbackAttempt(lockA)
	runnerSQL(t, lockA, `UPDATE public.runner_order SET value=value WHERE id=1`)
	type result struct {
		receipt AttemptReceipt
		err     error
	}
	results := make(chan result, 2)
	pending := 2
	var lockC pgx.Tx
	defer func() {
		cancel()
		rollbackAttempt(lockA)
		if lockC != nil {
			rollbackAttempt(lockC)
		}
		for range pending {
			<-results
		}
	}()
	key := uuid.New()
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			r, err := f.runner.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{1, key})
			results <- result{r, err}
		}()
	}
	close(start)
	active := waitRunnerTarget(t, f, f.a, key, "Lock")
	initialReplay := <-results
	pending--
	if initialReplay.err != nil || initialReplay.receipt.ID != active.ID || (initialReplay.receipt.State != "reserved" && initialReplay.receipt.State != "running") {
		t.Fatal("simultaneous same-key calls did not yield one owner and one receipt")
	}
	// Key yang sama dapat membaca receipt running tanpa mengambil slot baru.
	replay, err := f.runner.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{1, key})
	if err != nil || replay.ID != active.ID || replay.State != "running" {
		t.Fatal("concurrent replay did not find running receipt")
	}
	_, err = f.runner.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{1, uuid.New()})
	requireDraftCode(t, err, problem.TargetBusy)
	// Target C tetap dapat commit saat A diblokir.
	_, _, independent := executeRunnerDraft(t, f, f.c, `INSERT INTO public.runner_order VALUES(2,9)`)
	if independent.State != "succeeded" {
		t.Fatal("independent target was blocked by A")
	}
	lockC, err = f.second.Begin(ctx)
	if err != nil {
		t.Fatal("cannot create second target barrier")
	}
	defer rollbackAttempt(lockC)
	runnerSQL(t, lockC, `UPDATE public.runner_order SET value=value WHERE id=1`)
	keyC := uuid.New()
	pending++
	go func() {
		r, err := f.runner.Execute(ctx, f.editor, f.c, dc.ID, ReserveInput{1, keyC})
		results <- result{r, err}
	}()
	waitRunnerTarget(t, f, f.c, keyC, "Lock")
	fullKey := uuid.New()
	_, err = f.runner.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{1, fullKey})
	requireDraftCode(t, err, problem.ExecutionCapacity)
	if receipt, err := f.runner.store.Receipt(ctx, f.a, f.actor, fullKey); err != nil || receipt != nil {
		t.Fatal("capacity rejection created reservation")
	}
	replay, err = f.runner.Execute(ctx, f.editor, f.a, d.ID, ReserveInput{1, key})
	if err != nil || replay.ID != active.ID {
		t.Fatal("full admission blocked existing receipt")
	}
	if err := lockC.Commit(ctx); err != nil {
		t.Fatal("cannot release second barrier")
	}
	if err := lockA.Commit(ctx); err != nil {
		t.Fatal("cannot release first barrier")
	}
	for range 2 {
		out := <-results
		pending--
		if out.err != nil || out.receipt.State != "succeeded" {
			t.Fatalf("concurrent runner failed: %v", out.err)
		}
	}
	var value int
	if err := f.target.QueryRow(ctx, `SELECT value FROM public.runner_order WHERE id=1`).Scan(&value); err != nil || value != 1 {
		t.Fatal("same-key concurrency caused multiple effects")
	}
	_, _, next := executeRunnerDraft(t, f, f.a, `UPDATE public.runner_order SET value=value*10+2 WHERE id=1`)
	if next.State != "succeeded" || next.Version != "2" {
		t.Fatal("next target execution was not ordered")
	}
	if err := f.target.QueryRow(ctx, `SELECT value FROM public.runner_order WHERE id=1`).Scan(&value); err != nil || value != 12 {
		t.Fatal("target effects were not linear")
	}
	if len(f.runner.slots) != 0 || f.store.pool.Stat().AcquiredConns() != 0 {
		t.Fatal("concurrent execution leaked resources")
	}
}

func TestRunnerTimeoutAndCancelPostgres(t *testing.T) {
	f := newRunnerFixture(t)
	ctx := context.Background()
	t.Run("insufficient remaining commit budget rolls back", func(t *testing.T) {
		d := attemptDraft(t, f.draftFixture, f.a, `CREATE TABLE public.runner_short_deadline(id int)`)
		if _, err := ValidateSQL(d.SQL); err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		r, err := f.runner.Execute(short, f.editor, f.a, d.ID, ReserveInput{1, uuid.New()})
		if err != nil || r.State != "failed" {
			t.Fatal("short commit budget did not return confirmed failure")
		}
		got := runnerOutcome(t, f, r)
		if got.code != "SQL_TIMEOUT" || got.evidence != "rollback_ack" || got.commit || got.active {
			t.Fatal("short commit budget sent COMMIT or did not roll back")
		}
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.runner_short_deadline') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("insufficient budget committed target effects")
		}
	})
	t.Run("server lock timeout rolls back all earlier changes", func(t *testing.T) {
		runnerSQL(t, f.target, `CREATE TABLE public.runner_lock(id int PRIMARY KEY,value int); INSERT INTO public.runner_lock VALUES(1,0); GRANT ALL ON public.runner_lock TO databrain_target`)
		lock, err := f.target.Begin(ctx)
		if err != nil {
			t.Fatal("cannot begin lock barrier")
		}
		defer rollbackAttempt(lock)
		runnerSQL(t, lock, `UPDATE public.runner_lock SET value=1 WHERE id=1`)
		_, _, r := executeRunnerDraft(t, f, f.a, `CREATE TABLE public.runner_lock_rollback(id int); UPDATE public.runner_lock SET value=2 WHERE id=1`)
		got := runnerOutcome(t, f, r)
		if r.State != "failed" || got.evidence != "rollback_ack" || got.code != "LOCK_TIMEOUT" || got.sqlstate != "55P03" || got.index != 2 || got.active {
			t.Fatal("lock timeout outcome incorrect")
		}
		rollbackAttempt(lock)
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.runner_lock_rollback') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("lock timeout left earlier DDL")
		}
	})
	t.Run("server statement timeout rolls back the entire script", func(t *testing.T) {
		_, _, r := executeRunnerDraft(t, f, f.a, `CREATE TABLE public.runner_timeout(value text); INSERT INTO public.runner_timeout VALUES(pg_sleep(31)::text)`)
		got := runnerOutcome(t, f, r)
		if r.State != "failed" || got.evidence != "rollback_ack" || got.code != "SQL_TIMEOUT" || got.sqlstate != "57014" || got.index != 2 || got.active {
			t.Fatal("statement timeout outcome incorrect")
		}
		var absent bool
		if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.runner_timeout') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("statement timeout left target changes")
		}
	})
	t.Run("request cancellation persists honest outcome with a fresh context", func(t *testing.T) {
		d := attemptDraft(t, f.draftFixture, f.c, `CREATE TABLE public.runner_cancel(value text); INSERT INTO public.runner_cancel VALUES(pg_sleep(10)::text)`)
		if _, err := ValidateSQL(d.SQL); err != nil {
			t.Fatal(err)
		}
		canceled, stop := context.WithCancel(ctx)
		defer stop()
		type result struct {
			r   AttemptReceipt
			err error
		}
		done := make(chan result, 1)
		key := uuid.New()
		go func() {
			r, err := f.runner.Execute(canceled, f.editor, f.c, d.ID, ReserveInput{1, key})
			done <- result{r, err}
		}()
		consumed := false
		defer func() {
			stop()
			if !consumed {
				<-done
			}
		}()
		waitRunnerTarget(t, f, f.c, key, "Timeout")
		stop()
		out := <-done
		consumed = true
		r := out.r
		if out.err != nil || r.ID == uuid.Nil {
			t.Fatal("canceled execution did not persist a readable outcome")
		}
		got := runnerOutcome(t, f, r)
		if got.commit || (got.state == "failed" && (got.code != "CANCELED" || got.evidence != "rollback_ack" || got.active)) || (got.state == "unknown" && (got.code != "COMMIT_UNCERTAIN" || got.evidence != "" || !got.active)) || (got.state != "failed" && got.state != "unknown") {
			t.Fatal("cancellation claimed an unproved outcome")
		}
		var absent bool
		if err := f.second.QueryRow(ctx, `SELECT to_regclass('public.runner_cancel') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("canceled execution committed target changes")
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			var n int
			_ = f.observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1`, "databrain:"+r.ID.String()).Scan(&n)
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("canceled target connection leaked")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}
