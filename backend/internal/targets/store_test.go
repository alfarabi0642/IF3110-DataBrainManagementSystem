package targets

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type targetFixture struct {
	p             *Provider
	owner         *pgx.Conn
	schema        *pgx.Conn
	actor         uuid.UUID
	input, second CreateInput
	platformName  string
	role          string
}

func closeTarget(conn *pgx.Conn) {
	cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = conn.Close(cleanup)
}

func fixture(t *testing.T) targetFixture {
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
	t.Cleanup(func() { closeTarget(owner) })
	var directory, role, database string
	err = owner.QueryRow(ctx, `SELECT current_setting('data_directory'),current_user,current_database()`).Scan(&directory, &role, &database)
	expected, pathErr := filepath.Abs("../../../.tmp/postgres/data")
	if err != nil || pathErr != nil || !strings.EqualFold(filepath.Clean(directory), filepath.Clean(expected)) || role != "databrain_fixture_owner" || database != "postgres" {
		t.Fatal("fixture identity does not match this repository; mutation refused")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal("cannot generate disposable role credential")
	}
	password := hex.EncodeToString(key)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	targetRole := "db_target_test_" + suffix
	quotedRole := pgx.Identifier{targetRole}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE ROLE "+quotedRole+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
		t.Fatal("cannot create disposable target role")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := owner.Exec(cleanup, "DROP ROLE "+quotedRole); err != nil {
			t.Error("cannot remove the target role owned by this test")
		}
	})
	names := []string{"db_target_platform_" + suffix, "db_target_school_" + suffix, "db_target_inventory_" + suffix}
	for i, name := range names {
		dbOwner := targetRole
		if i == 0 {
			dbOwner = "databrain_migrator"
		}
		quoted := pgx.Identifier{name}.Sanitize()
		if _, err := owner.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER "+pgx.Identifier{dbOwner}.Sanitize()); err != nil {
			t.Fatal("cannot create isolated target test database")
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var actual string
			if err := owner.QueryRow(cleanup, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", name).Scan(&actual); err != nil || actual != dbOwner {
				t.Error("test database ownership changed; cleanup refused")
				return
			}
			if _, err := owner.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
				t.Error("cannot drop isolated database; check for leaked test connections")
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
		t.Fatal("test schema migration failed")
	}
	u.User = url.UserPassword("databrain_runtime", os.Getenv("DATABRAIN_RUNTIME_PASSWORD"))
	db, err := platformdb.Open(ctx, u.String())
	if err != nil {
		t.Fatal("cannot open restricted platform pool")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := db.Close(cleanup); err != nil {
			t.Error("test platform pool did not close")
		}
	})
	u.User = url.UserPassword("databrain_migrator", os.Getenv("DATABRAIN_MIGRATOR_PASSWORD"))
	schema, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal("cannot connect to this test's isolated schema")
	}
	t.Cleanup(func() { closeTarget(schema) })
	secrets, err := secret.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal("cannot initialize synthetic test key")
	}
	p, err := NewProvider(db.Pool, secrets, config.TargetSettings{AppEnv: "test", AllowedHosts: []string{"127.0.0.1", "localhost"}})
	if err != nil {
		t.Fatal("cannot initialize target provider")
	}
	actor := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO databrain.users(id,username,display_name,password_hash) VALUES ($1,'fixture_editor','Synthetic editor','fixture-not-an-account-hash')`, actor); err != nil {
		t.Fatal("cannot create actor fixture")
	}
	input := CreateInput{Name: " School 猫 ", Host: "127.0.0.1", Port: 55432, DatabaseName: names[1], Username: targetRole, Password: password, SSLMode: "disable"}
	second := input
	second.Name, second.DatabaseName = "Inventory", names[2]
	return targetFixture{p: p, owner: owner, schema: schema, actor: actor, input: input, second: second, platformName: names[0], role: targetRole}
}

func mustCreate(t *testing.T, f targetFixture, in CreateInput) Target {
	t.Helper()
	result, err := f.p.Create(context.Background(), f.actor, in)
	if err != nil {
		t.Fatalf("valid fixture target create failed with safe error: %v", err)
	}
	return result
}

func editOf(target Target) EditInput {
	return EditInput{ExpectedRevision: target.ConfigRevision, Name: target.Name, Username: target.Username, SSLMode: target.SSLMode, IsActive: target.IsActive}
}

func waitSlots(t *testing.T, slots chan struct{}, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(slots) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(slots) != want {
		t.Fatal("connection capacity was not released after pgx cleanup")
	}
}

func TestTargetStoreAndConnections(t *testing.T) {
	f := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p := f.p
	t.Run("stream insert failure rolls back target", func(t *testing.T) {
		_, err := f.schema.Exec(ctx, `CREATE FUNCTION databrain.reject_fixture_stream() RETURNS trigger LANGUAGE plpgsql AS $$
		    BEGIN RAISE EXCEPTION 'synthetic stream failure'; END $$;
		    CREATE TRIGGER reject_fixture_stream BEFORE INSERT ON databrain.migration_streams
		    FOR EACH ROW EXECUTE FUNCTION databrain.reject_fixture_stream()`)
		if err != nil {
			t.Fatal("cannot install isolated stream failure fixture")
		}
		defer func() {
			if _, err := f.schema.Exec(context.Background(), "DROP TRIGGER reject_fixture_stream ON databrain.migration_streams; DROP FUNCTION databrain.reject_fixture_stream()"); err != nil {
				t.Error("cannot remove isolated stream failure fixture")
			}
		}()
		_, err = p.Create(ctx, f.actor, f.input)
		checkCode(t, err, problem.PlatformUnavailable)
		var count int
		if err := p.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.database_targets").Scan(&count); err != nil || count != 0 {
			t.Fatal("failed stream insert left an orphan target")
		}
	})
	first := mustCreate(t, f, f.input)
	second := mustCreate(t, f, f.second)

	t.Run("atomic store and encryption", func(t *testing.T) {
		var targets, streams int
		if err := p.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM databrain.database_targets),(SELECT count(*) FROM databrain.migration_streams)`).Scan(&targets, &streams); err != nil || targets != 2 || streams != 2 {
			t.Fatal("target and stream were not both persisted")
		}
		stored, err := p.load(ctx, first.ID)
		if err != nil || bytes.Contains(stored.sealed, []byte(f.input.Password)) || bytes.Equal(stored.sealed, []byte(f.input.Password)) || !first.PasswordStored || first.ConfigRevision != 1 || first.Name != "School 猫" {
			t.Fatal("stored credentials or metadata do not follow the contract")
		}
		if plaintext, err := p.secrets.OpenTarget(first.ID, stored.sealed); err != nil || string(plaintext) != f.input.Password {
			t.Fatal("stored credential could not be decrypted for its target")
		}
		if plaintext, err := p.secrets.OpenTarget(second.ID, stored.sealed); err == nil || plaintext != nil {
			t.Fatal("credential could be moved to another target")
		}
		if strings.Contains(fmt.Sprintf("%+v", first), f.input.Password) || strings.Contains(fmt.Sprintf("%+v", first), hex.EncodeToString(stored.sealed)) {
			t.Fatal("admin data exposes credentials")
		}
		_, err = p.Create(ctx, f.actor, f.input)
		checkCode(t, err, problem.TargetConflict)
		_, err = p.Create(ctx, uuid.New(), CreateInput{Name: "invalid actor", Host: f.input.Host, Port: f.input.Port, DatabaseName: "missing_database", Username: f.input.Username, Password: f.input.Password, SSLMode: "disable"})
		checkCode(t, err, problem.TargetConnectionFailed)
		if err := p.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.database_targets").Scan(&targets); err != nil || targets != 2 {
			t.Fatal("failed create probe inserted a target")
		}
	})

	t.Run("dedicated read-only and migration connections", func(t *testing.T) {
		a, err := p.OpenExplorer(ctx, first.ID)
		if err != nil {
			t.Fatal("cannot open first explorer")
		}
		defer closeTarget(a)
		b, err := p.OpenExplorer(ctx, second.ID)
		if err != nil {
			t.Fatal("cannot open second explorer")
		}
		defer closeTarget(b)
		var dbA, dbB, readonly, timeout string
		if err := a.QueryRow(ctx, "SELECT current_database(),current_setting('default_transaction_read_only'),current_setting('statement_timeout')").Scan(&dbA, &readonly, &timeout); err != nil || readonly != "on" || timeout != "3s" {
			t.Fatal("explorer connection policy is not read-only and bounded")
		}
		if err := b.QueryRow(ctx, "SELECT current_database()").Scan(&dbB); err != nil || dbA != f.input.DatabaseName || dbB != f.second.DatabaseName || a.PgConn().PID() == b.PgConn().PID() {
			t.Fatal("target connections were mixed or reused")
		}
		if _, err := a.Exec(ctx, "CREATE TABLE explorer_must_not_write(id int)"); err == nil {
			t.Fatal("explorer allowed a write")
		}
		attempt := uuid.New()
		migration, err := p.OpenMigration(ctx, first.ID, first.ConfigRevision, attempt)
		if err != nil {
			t.Fatal("cannot open migration connection")
		}
		defer closeTarget(migration)
		var application string
		if err := migration.QueryRow(ctx, "SELECT current_setting('application_name'),current_setting('default_transaction_read_only')").Scan(&application, &readonly); err != nil || application != "databrain:"+attempt.String() || readonly != "off" {
			t.Fatal("migration connection metadata or write policy is wrong")
		}
		// A rollback proves target write capability without leaving fixture data.
		tx, err := migration.Begin(ctx)
		if err != nil {
			t.Fatal("cannot begin target verification transaction")
		}
		defer rollback(tx)
		if _, err := tx.Exec(ctx, "CREATE TABLE connection_probe(id int)"); err != nil {
			t.Fatal("migration role cannot write target")
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal("verification rollback failed")
		}
	})
	waitSlots(t, p.explorer, 0)
	waitSlots(t, p.migration, 0)

	t.Run("revision no-op and password preservation", func(t *testing.T) {
		before, _ := p.load(ctx, first.ID)
		unchanged, err := p.Edit(ctx, first.ID, editOf(first))
		if err != nil || unchanged.ConfigRevision != first.ConfigRevision {
			t.Fatal("identical edit changed revision")
		}
		edit := editOf(first)
		edit.Name = "Renamed school"
		updated, err := p.Edit(ctx, first.ID, edit)
		if err != nil || updated.ConfigRevision != 2 {
			t.Fatal("rename did not increment revision")
		}
		after, _ := p.load(ctx, first.ID)
		if !bytes.Equal(before.sealed, after.sealed) {
			t.Fatal("empty edit password replaced the stored secret")
		}
		_, err = p.Edit(ctx, first.ID, edit)
		checkCode(t, err, problem.RevisionConflict)
		_, err = p.OpenMigration(ctx, first.ID, first.ConfigRevision, uuid.New())
		checkCode(t, err, problem.RevisionConflict)
		first = updated
	})

	t.Run("competing revisions", func(t *testing.T) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		errors := make(chan error, 2)
		for _, name := range []string{"concurrent A", "concurrent B"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				edit := editOf(first)
				edit.Name = name
				_, err := p.Edit(ctx, first.ID, edit)
				errors <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errors)
		succeeded, conflicts := 0, 0
		for err := range errors {
			if err == nil {
				succeeded++
			} else if isCode(err, problem.RevisionConflict) {
				conflicts++
			} else {
				t.Fatal("unexpected edit race error")
			}
		}
		if succeeded != 1 || conflicts != 1 {
			t.Fatal("competing revisions overwrote each other")
		}
		var err error
		first, err = p.Get(ctx, first.ID)
		if err != nil {
			t.Fatal("could not reload winning revision")
		}
	})

	t.Run("reactivation and password replacement", func(t *testing.T) {
		edit := editOf(first)
		edit.IsActive = false
		inactive, err := p.Edit(ctx, first.ID, edit)
		if err != nil {
			t.Fatal("deactivation failed")
		}
		_, err = p.OpenExplorer(ctx, first.ID)
		checkCode(t, err, problem.DatabaseNotFound)
		_, err = p.Create(ctx, f.actor, f.input)
		checkCode(t, err, problem.TargetConflict)
		before, _ := p.load(ctx, first.ID)
		newBytes := make([]byte, 32)
		if _, err := rand.Read(newBytes); err != nil {
			t.Fatal("cannot generate replacement credential")
		}
		newPassword := hex.EncodeToString(newBytes)
		if _, err := f.owner.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.role}.Sanitize()+" PASSWORD '"+newPassword+"'"); err != nil {
			t.Fatal("cannot replace this test role's credential")
		}
		edit = editOf(inactive)
		edit.IsActive = true
		_, err = p.Edit(ctx, first.ID, edit)
		checkCode(t, err, problem.TargetConnectionFailed)
		afterFailure, _ := p.load(ctx, first.ID)
		if afterFailure.IsActive || afterFailure.ConfigRevision != inactive.ConfigRevision || !bytes.Equal(before.sealed, afterFailure.sealed) {
			t.Fatal("failed reactivation changed stored data")
		}
		edit.Password = newPassword
		first, err = p.Edit(ctx, first.ID, edit)
		if err != nil || !first.IsActive {
			t.Fatal("reactivation with replacement credential failed")
		}
		after, _ := p.load(ctx, first.ID)
		password, openErr := p.secrets.OpenTarget(first.ID, after.sealed)
		if openErr != nil || string(password) != newPassword || bytes.Equal(before.sealed, after.sealed) {
			t.Fatal("replacement credential not encrypted and persisted")
		}
		result, err := p.Test(ctx, second.ID)
		if err != nil || result.OK || result.Reason != AuthenticationFailed {
			t.Fatal("stored probe failed to report bad credentials without mutation")
		}
		second, err = p.Edit(ctx, second.ID, EditInput{ExpectedRevision: second.ConfigRevision, Name: second.Name, Username: second.Username, Password: newPassword, SSLMode: "disable", IsActive: true})
		if err != nil {
			t.Fatal("cannot update second isolated target credential")
		}
		f.input.Password, f.second.Password = newPassword, newPassword
	})

	t.Run("capacity cancellation and close", func(t *testing.T) {
		for _, migration := range []bool{false, true} {
			limit, slots, code := 4, p.explorer, problem.TargetUnavailable
			if migration {
				limit, slots, code = 2, p.migration, problem.ExecutionCapacity
			}
			open := func(ctx context.Context) (*pgx.Conn, error) {
				if migration {
					return p.OpenMigration(ctx, first.ID, first.ConfigRevision, uuid.New())
				}
				return p.OpenExplorer(ctx, first.ID)
			}
			conns := make([]*pgx.Conn, 0, limit)
			for range limit {
				conn, err := open(ctx)
				if err != nil {
					t.Fatal("capacity rejected below its limit")
				}
				conns = append(conns, conn)
				defer closeTarget(conn)
			}
			_, err := open(ctx)
			checkCode(t, err, code)
			for _, conn := range conns {
				closeTarget(conn)
			}
			waitSlots(t, slots, 0)
			conn, err := open(ctx)
			if err != nil {
				t.Fatal("close did not release connection capacity")
			}
			queryCtx, stop := context.WithTimeout(ctx, 20*time.Millisecond)
			_, queryErr := conn.Exec(queryCtx, "SELECT pg_sleep(5)")
			stop()
			closeTarget(conn)
			if queryErr == nil {
				t.Fatal("target query ignored cancellation")
			}
			waitSlots(t, slots, 0)
			canceled, stop := context.WithCancel(ctx)
			stop()
			_, err = open(canceled)
			checkCode(t, err, problem.TargetUnavailable)
		}
	})

	t.Run("safe probe failures and platform separation", func(t *testing.T) {
		for _, tc := range []struct {
			in     CreateInput
			reason ProbeReason
		}{
			{func() CreateInput { in := f.input; in.Password = "SENSITIVE-wrong-password"; return in }(), AuthenticationFailed},
			{func() CreateInput { in := f.input; in.SSLMode = "verify-full"; return in }(), TLSFailed},
			{func() CreateInput { in := f.input; in.DatabaseName = f.platformName; return in }(), PlatformTargetForbidden},
			{func() CreateInput { in := f.input; in.DatabaseName = f.platformName; in.Host = "LOCALHOST."; return in }(), PlatformTargetForbidden},
			{func() CreateInput { in := f.input; in.Username = "databrain_runtime"; return in }(), PlatformTargetForbidden},
			{func() CreateInput {
				in := f.input
				in.Username = "databrain_fixture_owner"
				in.Password = os.Getenv("DATABRAIN_OWNER_PASSWORD")
				return in
			}(), UnsafeRole},
		} {
			result, err := p.Probe(ctx, tc.in)
			checkCode(t, err, problem.TargetConnectionFailed)
			if result.OK || result.Reason != tc.reason {
				t.Fatalf("wrong safe probe failure reason; want %s, got %s", tc.reason, result.Reason)
			}
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal("cannot allocate a disposable closed port")
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		bad := f.input
		bad.Port = port
		result, err := p.Probe(ctx, bad)
		checkCode(t, err, problem.TargetConnectionFailed)
		if result.Reason != ConnectionRefused {
			t.Fatalf("wrong closed-port reason: %s", result.Reason)
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		result, err = p.Probe(canceled, f.input)
		checkCode(t, err, problem.TargetConnectionFailed)
		if result.Reason != Timeout {
			t.Fatal("probe cancellation ignored")
		}
		// A real TCP peer that never speaks PostgreSQL must still respect a
		// shorter caller deadline during connection establishment.
		stalled, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal("cannot open stalled peer fixture")
		}
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := stalled.Accept()
			if err == nil {
				accepted <- conn
			}
		}()
		defer func() {
			_ = stalled.Close()
			select {
			case conn := <-accepted:
				_ = conn.Close()
			case <-time.After(time.Second):
				t.Error("stalled peer was not accepted")
			}
		}()
		bad.Port = stalled.Addr().(*net.TCPAddr).Port
		short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		started := time.Now()
		result, err = p.Probe(short, bad)
		stop()
		checkCode(t, err, problem.TargetConnectionFailed)
		if result.Reason != Timeout || time.Since(started) > time.Second {
			t.Fatal("probe ignored the caller's connect deadline")
		}
	})

	t.Run("rename and deactivate while target offline", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal("cannot allocate offline fixture port")
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		id := uuid.New()
		sealed, err := p.secrets.SealTarget(id, []byte(f.input.Password))
		if err != nil {
			t.Fatal("cannot seal offline fixture credential")
		}
		_, err = p.pool.Exec(ctx, `INSERT INTO databrain.database_targets(id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
		    VALUES ($1,'Offline','127.0.0.1',$2,'offline',$3,$4,'disable',$5)`, id, port, f.role, sealed, f.actor)
		if err != nil {
			t.Fatal("cannot insert offline target fixture")
		}
		if _, err := p.pool.Exec(ctx, "INSERT INTO databrain.migration_streams(target_id) VALUES ($1)", id); err != nil {
			t.Fatal("cannot insert offline stream fixture")
		}
		edit := EditInput{ExpectedRevision: 1, Name: "Renamed offline", Username: f.role, SSLMode: "disable", IsActive: false}
		updated, err := p.Edit(ctx, id, edit)
		if err != nil || updated.ConfigRevision != 2 || updated.IsActive {
			t.Fatal("rename/deactivation required a live target")
		}
		result, err := p.Test(ctx, id)
		if err != nil || result.OK || result.Reason != ConnectionRefused {
			t.Fatal("stored offline target probe did not return a safe result")
		}
	})

	t.Run("unresolved attempts block edits", func(t *testing.T) {
		draftID, attemptID := uuid.New(), uuid.New()
		if _, err := p.pool.Exec(ctx, `INSERT INTO databrain.migration_drafts(id,target_id,title,sql,created_by,updated_by) VALUES ($1,$2,'Busy fixture','SELECT 1',$3,$3)`, draftID, first.ID, f.actor); err != nil {
			t.Fatal("cannot insert busy draft fixture")
		}
		hash := sha256.Sum256([]byte("SELECT 1"))
		_, err := p.pool.Exec(ctx, `INSERT INTO databrain.migration_attempts
		    (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,target_config_revision,
		    sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
		    VALUES ($1,$2,$3,1,1,'Busy fixture',$4,$5,'SELECT 1',$6,$7,'fixture_editor',$8,'reserved')`,
			attemptID, first.ID, draftID, first.Name, first.ConfigRevision, hash[:], f.actor, uuid.New())
		if err != nil {
			t.Fatal("cannot insert reserved attempt fixture")
		}
		if _, err := p.pool.Exec(ctx, "UPDATE databrain.migration_streams SET active_attempt_id=$2,next_version=2 WHERE target_id=$1", first.ID, attemptID); err != nil {
			t.Fatal("cannot link busy stream fixture")
		}
		for _, transition := range []string{"", "UPDATE databrain.migration_attempts SET state='running',started_at=now() WHERE id=$1", "UPDATE databrain.migration_attempts SET state='unknown',failure_code='PROCESS_INTERRUPTED',failure_message='Synthetic interrupted fixture' WHERE id=$1"} {
			if transition != "" {
				if _, err := p.pool.Exec(ctx, transition, attemptID); err != nil {
					t.Fatal("cannot transition busy fixture")
				}
			}
			edit := editOf(first)
			edit.Name = "Must not change"
			edit.IsActive = false
			_, err := p.Edit(ctx, first.ID, edit)
			checkCode(t, err, problem.TargetBusy)
			unchanged, err := p.Get(ctx, first.ID)
			if err != nil || unchanged.ConfigRevision != first.ConfigRevision || unchanged.Name != first.Name || !unchanged.IsActive {
				t.Fatal("unresolved attempt did not protect target configuration")
			}
		}
		edit := editOf(second)
		edit.Name = "Independent target"
		if _, err := p.Edit(ctx, second.ID, edit); err != nil {
			t.Fatal("busy target blocked an independent target")
		}
	})

	// No test connection should remain on either target, including failed probes.
	waitSlots(t, p.explorer, 0)
	waitSlots(t, p.migration, 0)
	var active int
	if err := f.owner.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname IN ($1,$2)", f.input.DatabaseName, f.second.DatabaseName).Scan(&active); err != nil || active != 0 {
		t.Fatal("target connections leaked")
	}
}
