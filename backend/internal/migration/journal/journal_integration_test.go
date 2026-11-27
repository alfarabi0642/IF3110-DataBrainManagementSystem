package journal

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func closeJournalTestConn(conn *pgx.Conn) {
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

type journalTestFixture struct {
	ownerDB    *pgx.Conn
	targetConn *pgx.Conn
	dbName     string
	ddl        string
	appSecret  []byte
}

func newJournalFixture(t *testing.T, dbOwner string) journalTestFixture {
	t.Helper()
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	if raw == "" {
		t.Skip("set DATABRAIN_TEST_ADMIN_URL for the disposable PostgreSQL fixture")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.User == nil || u.User.Username() != "databrain_fixture_owner" || u.Path != "/postgres" {
		t.Fatal("test requires the repository fixture owner on 127.0.0.1:55432/postgres")
	}
	ddlBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "dev", "target-journal.sql"))
	if err != nil {
		t.Fatalf("cannot read backend/dev/target-journal.sql: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("cannot connect to disposable fixture")
	}
	t.Cleanup(func() { closeJournalTestConn(admin) })

	var directory, role, database string
	err = admin.QueryRow(ctx, `SELECT current_setting('data_directory'),current_user,current_database()`).Scan(&directory, &role, &database)
	expected, pathErr := filepath.Abs("../../../../.tmp/postgres/data")
	if err != nil || pathErr != nil || !strings.EqualFold(filepath.Clean(directory), filepath.Clean(expected)) || role != "databrain_fixture_owner" || database != "postgres" {
		t.Fatal("fixture identity mismatch; mutation refused")
	}

	dbName := "db_journal_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER "+pgx.Identifier{dbOwner}.Sanitize()); err != nil {
		t.Fatalf("cannot create isolated journal test database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var actual string
		if err := admin.QueryRow(cleanup, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", dbName).Scan(&actual); err != nil || actual != dbOwner {
			t.Error("database ownership changed; cleanup refused")
			return
		}
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
			t.Error("cannot drop journal test database; check leaked connections")
		}
	})
	if _, err := admin.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT CONNECT, CREATE ON DATABASE "+quoted+" TO databrain_target"); err != nil {
		t.Fatal(err)
	}

	ownerURL, _ := url.Parse(raw)
	ownerURL.Path = "/" + dbName
	ownerConn, err := pgx.Connect(ctx, ownerURL.String())
	if err != nil {
		t.Fatalf("cannot connect as owner to %s: %v", dbName, err)
	}
	t.Cleanup(func() { closeJournalTestConn(ownerConn) })

	if _, err := ownerConn.Exec(ctx, "REVOKE ALL ON SCHEMA public FROM PUBLIC; GRANT USAGE, CREATE ON SCHEMA public TO databrain_target"); err != nil {
		t.Fatal(err)
	}

	targetURL, _ := url.Parse(raw)
	targetURL.Path = "/" + dbName
	targetURL.User = url.UserPassword("databrain_target", os.Getenv("DATABRAIN_TARGET_PASSWORD"))
	targetConn, err := pgx.Connect(ctx, targetURL.String())
	if err != nil {
		t.Fatalf("cannot connect as databrain_target to %s: %v", dbName, err)
	}
	t.Cleanup(func() { closeJournalTestConn(targetConn) })

	return journalTestFixture{
		ownerDB:    ownerConn,
		targetConn: targetConn,
		dbName:     dbName,
		ddl:        string(ddlBytes),
		appSecret:  bytes.Repeat([]byte{0x5a}, 32),
	}
}

func TestTargetJournalProvisioningAndVerificationPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("missing journal fails closed before SQL", func(t *testing.T) {
		f := newJournalFixture(t, "databrain_fixture_owner")
		if err := VerifyTargetJournal(ctx, f.targetConn); !errors.Is(err, ErrJournalMissing) {
			t.Fatalf("expected ErrJournalMissing, got %v", err)
		}
	})

	t.Run("provisioning refuses target DB owned by databrain_target", func(t *testing.T) {
		f := newJournalFixture(t, "databrain_target")
		if _, err := f.ownerDB.Exec(ctx, f.ddl); err == nil {
			t.Fatal("target-journal.sql must refuse a database owned by databrain_target")
		}
	})

	t.Run("provisioning is idempotent and VerifyTargetJournal passes", func(t *testing.T) {
		f := newJournalFixture(t, "databrain_fixture_owner")
		if _, err := f.ownerDB.Exec(ctx, f.ddl); err != nil {
			t.Fatalf("first target-journal.sql run failed: %v", err)
		}
		if _, err := f.ownerDB.Exec(ctx, f.ddl); err != nil {
			t.Fatalf("second idempotent target-journal.sql run failed: %v", err)
		}
		if err := VerifyTargetJournal(ctx, f.targetConn); err != nil {
			t.Fatalf("VerifyTargetJournal rejected valid setup: %v", err)
		}
	})

	t.Run("unsafe privilege drift fails closed", func(t *testing.T) {
		f := newJournalFixture(t, "databrain_fixture_owner")
		if _, err := f.ownerDB.Exec(ctx, f.ddl); err != nil {
			t.Fatal(err)
		}
		// Direct SELECT on execution_commits must be rejected.
		if _, err := f.ownerDB.Exec(ctx, `GRANT SELECT ON databrain_runtime.execution_commits TO databrain_target`); err != nil {
			t.Fatal(err)
		}
		if err := VerifyTargetJournal(ctx, f.targetConn); !errors.Is(err, ErrJournalUnsafe) {
			t.Fatalf("expected ErrJournalUnsafe with direct table SELECT grant, got %v", err)
		}
		if _, err := f.ownerDB.Exec(ctx, `REVOKE SELECT ON databrain_runtime.execution_commits FROM databrain_target`); err != nil {
			t.Fatal(err)
		}
		// Schema CREATE grant must be rejected.
		if _, err := f.ownerDB.Exec(ctx, `GRANT CREATE ON SCHEMA databrain_runtime TO databrain_target`); err != nil {
			t.Fatal(err)
		}
		if err := VerifyTargetJournal(ctx, f.targetConn); !errors.Is(err, ErrJournalUnsafe) {
			t.Fatalf("expected ErrJournalUnsafe with schema CREATE grant, got %v", err)
		}
		if _, err := f.ownerDB.Exec(ctx, `REVOKE CREATE ON SCHEMA databrain_runtime FROM databrain_target`); err != nil {
			t.Fatal(err)
		}
		// Disabled immutability trigger must be rejected.
		if _, err := f.ownerDB.Exec(ctx, `ALTER TABLE databrain_runtime.execution_commits DISABLE TRIGGER protect_execution_commits`); err != nil {
			t.Fatal(err)
		}
		if err := VerifyTargetJournal(ctx, f.targetConn); !errors.Is(err, ErrJournalUnsafe) {
			t.Fatalf("expected ErrJournalUnsafe with disabled immutability trigger, got %v", err)
		}
		if _, err := f.ownerDB.Exec(ctx, `ALTER TABLE databrain_runtime.execution_commits ENABLE TRIGGER protect_execution_commits`); err != nil {
			t.Fatal(err)
		}
		if err := VerifyTargetJournal(ctx, f.targetConn); err != nil {
			t.Fatalf("expected VerifyTargetJournal to recover after restoring grants: %v", err)
		}
	})

	t.Run("changed schema version fails closed with ErrJournalVersion", func(t *testing.T) {
		f := newJournalFixture(t, "databrain_fixture_owner")
		if _, err := f.ownerDB.Exec(ctx, f.ddl); err != nil {
			t.Fatal(err)
		}
		// Simulate an unrecognized future schema version.
		if _, err := f.ownerDB.Exec(ctx, `
			ALTER TABLE databrain_runtime.journal_meta DISABLE TRIGGER protect_journal_meta;
			ALTER TABLE databrain_runtime.journal_meta DROP CONSTRAINT journal_meta_schema_version_check;
			UPDATE databrain_runtime.journal_meta SET schema_version = 99 WHERE id = 1;
			ALTER TABLE databrain_runtime.journal_meta ENABLE TRIGGER protect_journal_meta;
		`); err != nil {
			t.Fatal(err)
		}
		if err := VerifyTargetJournal(ctx, f.targetConn); !errors.Is(err, ErrJournalVersion) {
			t.Fatalf("expected ErrJournalVersion, got %v", err)
		}
	})
}

func TestTargetJournalAtomicLifecyclePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newJournalFixture(t, "databrain_fixture_owner")
	if _, err := f.ownerDB.Exec(ctx, f.ddl); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTargetJournal(ctx, f.targetConn); err != nil {
		t.Fatal(err)
	}

	rootID := uuid.New()
	otherID := uuid.New()
	actionID := uuid.New()
	planHash := strings.Repeat("a", 64)
	otherHash := strings.Repeat("b", 64)
	secret, err := DeriveOwnerSecret(f.appSecret, rootID)
	if err != nil {
		t.Fatal(err)
	}
	wrongSecret, err := DeriveOwnerSecret(f.appSecret, otherID)
	if err != nil {
		t.Fatal(err)
	}

	// Operations without the session advisory lock (3110,2) must fail.
	if err := AcquireJournalOwner(ctx, f.targetConn, rootID, rootID, planHash, secret); !errors.Is(err, ErrJournalLockRequired) {
		t.Fatalf("expected ErrJournalLockRequired without advisory lock, got %v", err)
	}

	lock, acquired, err := TryTargetLock(ctx, f.targetConn)
	if err != nil || !acquired {
		t.Fatalf("cannot acquire target lock: %v", err)
	}
	defer func() { _ = lock.Release(ctx) }()

	// Acquire owner (and idempotent re-acquire).
	if err := AcquireJournalOwner(ctx, f.targetConn, rootID, rootID, planHash, secret); err != nil {
		t.Fatalf("AcquireJournalOwner failed: %v", err)
	}
	if err := AcquireJournalOwner(ctx, f.targetConn, rootID, rootID, planHash, secret); err != nil {
		t.Fatalf("idempotent AcquireJournalOwner failed: %v", err)
	}
	if err := AcquireJournalOwner(ctx, f.targetConn, otherID, otherID, otherHash, wrongSecret); !errors.Is(err, ErrJournalOwned) {
		t.Fatalf("expected ErrJournalOwned for unrelated execution, got %v", err)
	}

	owner, err := ReadJournalOwner(ctx, f.targetConn)
	if err != nil || owner == nil || owner.RootExecutionID != rootID || owner.PlanHash != planHash {
		t.Fatalf("unexpected ReadJournalOwner result: %+v, err=%v", owner, err)
	}

	// Rolled-back transaction must leave neither user data nor commit evidence.
	txFail, err := f.targetConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ArmJournalTx(ctx, txFail, rootID, rootID, planHash); err != nil {
		t.Fatalf("ArmJournalTx failed: %v", err)
	}
	if _, err := txFail.Exec(ctx, `CREATE TABLE public.rolled_back_item (id int)`); err != nil {
		t.Fatal(err)
	}
	if err := txFail.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	commits, err := ReadJournalCommits(ctx, f.targetConn, rootID)
	if err != nil || len(commits) != 0 {
		t.Fatalf("rolled-back tx must have 0 commits, got %d, err=%v", len(commits), err)
	}

	// Committed transaction writes user SQL + commit evidence atomically.
	txOK, err := f.targetConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := ArmJournalTx(ctx, txOK, rootID, rootID, planHash)
	if err != nil {
		t.Fatalf("ArmJournalTx failed: %v", err)
	}
	if _, err := txOK.Exec(ctx, `CREATE SCHEMA user_app`); err != nil {
		t.Fatalf("legitimate CREATE SCHEMA failed: %v", err)
	}
	if _, err := txOK.Exec(ctx, `CREATE TABLE user_app.items (id int PRIMARY KEY, label text); INSERT INTO user_app.items VALUES (1, 'ok')`); err != nil {
		t.Fatalf("legitimate DDL/DML failed: %v", err)
	}
	if err := guard.RecordCommit(ctx, txOK, 0, "apply", 1, nil); err != nil {
		t.Fatalf("RecordCommit failed: %v", err)
	}
	if err := txOK.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	commits, err = ReadJournalCommits(ctx, f.targetConn, rootID)
	if err != nil || len(commits) != 1 || commits[0].BatchOrdinal != 0 || commits[0].Direction != "apply" || commits[0].RowsAffected != 1 {
		t.Fatalf("unexpected committed evidence: %+v, err=%v", commits, err)
	}

	// Transfer owner requires the matching secret.
	if err := TransferJournalOwner(ctx, f.targetConn, rootID, actionID, otherHash, wrongSecret); !errors.Is(err, ErrJournalAuth) {
		t.Fatalf("expected ErrJournalAuth for wrong transfer secret, got %v", err)
	}
	if err := TransferJournalOwner(ctx, f.targetConn, rootID, actionID, otherHash, secret); err != nil {
		t.Fatalf("TransferJournalOwner failed: %v", err)
	}
	owner, err = ReadJournalOwner(ctx, f.targetConn)
	if err != nil || owner == nil || owner.ActionExecutionID != actionID || owner.PlanHash != otherHash {
		t.Fatalf("owner not transferred: %+v, err=%v", owner, err)
	}

	// Clear owner requires matching secret and preserves commit evidence.
	if err := ClearJournalOwner(ctx, f.targetConn, rootID, wrongSecret); !errors.Is(err, ErrJournalAuth) {
		t.Fatalf("expected ErrJournalAuth for wrong clear secret, got %v", err)
	}
	if err := ClearJournalOwner(ctx, f.targetConn, rootID, secret); err != nil {
		t.Fatalf("ClearJournalOwner failed: %v", err)
	}
	if err := ClearJournalOwner(ctx, f.targetConn, rootID, secret); err != nil {
		t.Fatalf("idempotent ClearJournalOwner failed: %v", err)
	}
	owner, err = ReadJournalOwner(ctx, f.targetConn)
	if err != nil || owner != nil {
		t.Fatalf("expected cleared owner to be nil, got %+v, err=%v", owner, err)
	}
	commits, err = ReadJournalCommits(ctx, f.targetConn, rootID)
	if err != nil || len(commits) != 1 {
		t.Fatalf("clearing owner must never delete commit evidence, got %+v, err=%v", commits, err)
	}
}

func TestTargetJournalAdversarialPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newJournalFixture(t, "databrain_fixture_owner")
	if _, err := f.ownerDB.Exec(ctx, f.ddl); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTargetJournal(ctx, f.targetConn); err != nil {
		t.Fatal(err)
	}

	// Direct DDL/DML/role-switch attacks executed as databrain_target must all fail.
	directAttacks := []struct {
		name string
		sql  string
	}{
		{"select tx_nonces", `SELECT * FROM databrain_runtime.tx_nonces`},
		{"select unresolved_owner", `SELECT * FROM databrain_runtime.unresolved_owner`},
		{"select execution_commits", `SELECT * FROM databrain_runtime.execution_commits`},
		{"insert execution_commits", `INSERT INTO databrain_runtime.execution_commits (root_execution_id, batch_ordinal, direction, action_execution_id, plan_hash) VALUES ('00000000-0000-0000-0000-000000000001', 0, 'apply', '00000000-0000-0000-0000-000000000001', repeat('a', 64))`},
		{"update journal_meta", `UPDATE databrain_runtime.journal_meta SET schema_version = 99`},
		{"delete execution_commits", `DELETE FROM databrain_runtime.execution_commits`},
		{"truncate execution_commits", `TRUNCATE databrain_runtime.execution_commits`},
		{"drop schema cascade", `DROP SCHEMA databrain_runtime CASCADE`},
		{"drop table cascade", `DROP TABLE databrain_runtime.execution_commits CASCADE`},
		{"alter table", `ALTER TABLE databrain_runtime.execution_commits ADD COLUMN evil text`},
		{"create shadow table", `CREATE TABLE databrain_runtime.shadow (id int)`},
		{"set role journal owner", `SET ROLE databrain_journal_owner`},
		{"call internal lock helper", `SELECT databrain_runtime._require_target_lock()`},
	}
	for _, attack := range directAttacks {
		t.Run("direct/"+attack.name, func(t *testing.T) {
			if _, err := f.targetConn.Exec(ctx, attack.sql); err == nil {
				t.Fatalf("adversarial statement unexpectedly succeeded: %s", attack.sql)
			}
		})
	}

	// Set up pre-existing hostile triggers and rules on user tables to test indirect attacks
	// during a real armed transaction under the advisory lock.
	rootID := uuid.New()
	forgedID := uuid.New()
	planHash := strings.Repeat("c", 64)
	secret, err := DeriveOwnerSecret(f.appSecret, rootID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.ownerDB.Exec(ctx, `
		CREATE TABLE public.victim_forge_commit (id int);
		CREATE TABLE public.victim_rearm (id int);
		CREATE TABLE public.victim_clear_owner (id int);
		CREATE TABLE public.victim_hijack_owner (id int);
		CREATE TABLE public.victim_drop_journal (id int);
		CREATE TABLE public.victim_rule_attack (id int);
		GRANT ALL ON public.victim_forge_commit, public.victim_rearm, public.victim_clear_owner,
		             public.victim_hijack_owner, public.victim_drop_journal, public.victim_rule_attack
		          TO databrain_target;

		CREATE FUNCTION public.trig_forge_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			PERFORM databrain_runtime.record_commit(
				'`+rootID.String()+`'::uuid,
				'`+rootID.String()+`'::uuid,
				'`+planHash+`',
				0, 'apply', 1, NULL, decode(repeat('11', 32), 'hex')
			);
			RETURN NEW;
		END;
		$fn$;
		CREATE TRIGGER t_forge BEFORE INSERT ON public.victim_forge_commit
			FOR EACH ROW EXECUTE FUNCTION public.trig_forge_commit();

		CREATE FUNCTION public.trig_rearm() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			PERFORM databrain_runtime.arm_tx(
				'`+rootID.String()+`'::uuid,
				'`+rootID.String()+`'::uuid,
				'`+planHash+`',
				pg_catalog.sha256(decode(repeat('22', 32), 'hex'))
			);
			RETURN NEW;
		END;
		$fn$;
		CREATE TRIGGER t_rearm BEFORE INSERT ON public.victim_rearm
			FOR EACH ROW EXECUTE FUNCTION public.trig_rearm();

		CREATE FUNCTION public.trig_clear_owner() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			PERFORM databrain_runtime.clear_owner('`+rootID.String()+`'::uuid, decode(repeat('33', 32), 'hex'));
			RETURN NEW;
		END;
		$fn$;
		CREATE TRIGGER t_clear BEFORE INSERT ON public.victim_clear_owner
			FOR EACH ROW EXECUTE FUNCTION public.trig_clear_owner();

		CREATE FUNCTION public.trig_hijack_owner() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			PERFORM databrain_runtime.acquire_owner(
				'`+forgedID.String()+`'::uuid,
				'`+forgedID.String()+`'::uuid,
				repeat('f', 64),
				decode(repeat('44', 32), 'hex')
			);
			RETURN NEW;
		END;
		$fn$;
		CREATE TRIGGER t_hijack BEFORE INSERT ON public.victim_hijack_owner
			FOR EACH ROW EXECUTE FUNCTION public.trig_hijack_owner();

		CREATE FUNCTION public.trig_drop_journal() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			DROP TABLE databrain_runtime.execution_commits CASCADE;
			RETURN NEW;
		END;
		$fn$;
		CREATE TRIGGER t_drop BEFORE INSERT ON public.victim_drop_journal
			FOR EACH ROW EXECUTE FUNCTION public.trig_drop_journal();

		CREATE RULE r_clear_owner AS ON INSERT TO public.victim_rule_attack DO ALSO
			SELECT databrain_runtime.clear_owner('`+rootID.String()+`'::uuid, decode(repeat('55', 32), 'hex'));
	`); err != nil {
		t.Fatalf("failed to seed hostile trigger/rule fixtures: %v", err)
	}

	lock, acquired, err := TryTargetLock(ctx, f.targetConn)
	if err != nil || !acquired {
		t.Fatalf("cannot acquire target lock: %v", err)
	}
	defer func() { _ = lock.Release(ctx) }()

	if err := AcquireJournalOwner(ctx, f.targetConn, rootID, rootID, planHash, secret); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ClearJournalOwner(ctx, f.targetConn, rootID, secret) }()

	indirectAttacks := []struct {
		name string
		sql  string
	}{
		{"trigger forging record_commit", `INSERT INTO public.victim_forge_commit VALUES (1)`},
		{"trigger re-arming tx_nonces", `INSERT INTO public.victim_rearm VALUES (1)`},
		{"trigger clearing unresolved_owner", `INSERT INTO public.victim_clear_owner VALUES (1)`},
		{"trigger hijacking unresolved_owner", `INSERT INTO public.victim_hijack_owner VALUES (1)`},
		{"trigger dropping execution_commits", `INSERT INTO public.victim_drop_journal VALUES (1)`},
		{"rule clearing unresolved_owner", `INSERT INTO public.victim_rule_attack VALUES (1)`},
	}

	for _, attack := range indirectAttacks {
		t.Run("indirect/"+attack.name, func(t *testing.T) {
			// Verify the SQL itself passes ValidateSQL (simulating an allowed DML hitting a hostile target object).
			if _, err := sqlpolicy.ValidateSQL(attack.sql); err != nil {
				t.Fatalf("expected test DML to pass ValidateSQL: %v", err)
			}
			tx, err := f.targetConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			if _, err := ArmJournalTx(ctx, tx, rootID, rootID, planHash); err != nil {
				t.Fatalf("ArmJournalTx failed: %v", err)
			}
			if _, err := tx.Exec(ctx, attack.sql); err == nil {
				t.Fatalf("hostile indirect attack unexpectedly succeeded: %s", attack.name)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}

			// Confirm zero forged commits and intact owner.
			commits, err := ReadJournalCommits(ctx, f.targetConn, rootID)
			if err != nil || len(commits) != 0 {
				t.Fatalf("forged commit evidence appeared: %+v, err=%v", commits, err)
			}
			owner, err := ReadJournalOwner(ctx, f.targetConn)
			if err != nil || owner == nil || owner.RootExecutionID != rootID {
				t.Fatalf("owner was altered or cleared: %+v, err=%v", owner, err)
			}
		})
	}
}
