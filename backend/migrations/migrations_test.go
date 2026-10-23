package migrations

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Tests create and drop only uniquely named databases in the verified A-03
// disposable cluster. No existing application or target database is reset.
func migrationFixture(t *testing.T) (string, string) {
	t.Helper()
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	if raw == "" {
		t.Skip("set DATABRAIN_TEST_ADMIN_URL for the local disposable PostgreSQL integration tests")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.User == nil ||
		u.User.Username() != "databrain_fixture_owner" || u.Path != "/postgres" {
		t.Fatal("integration tests require the A-03 fixture owner on 127.0.0.1:55432/postgres")
	}
	q := u.Query()
	q.Set("connect_timeout", "3")
	q.Set("statement_timeout", "30000")
	q.Set("lock_timeout", "3000")
	u.RawQuery = q.Encode()
	owner, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal("cannot open fixture connection")
	}
	t.Cleanup(func() { owner.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var directory, role, database string
	if err := owner.QueryRowContext(ctx, "SELECT current_setting('data_directory'), current_user, current_database()").Scan(&directory, &role, &database); err != nil {
		t.Fatal("cannot inspect fixture identity")
	}
	expected, err := filepath.Abs("../../.tmp/postgres/data")
	if err != nil || !strings.EqualFold(filepath.Clean(directory), filepath.Clean(expected)) || role != "databrain_fixture_owner" || database != "postgres" {
		t.Fatal("fixture directory or identity does not match this repository")
	}
	name := "databrain_migrations_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pq.QuoteIdentifier(name)
	if _, err := owner.ExecContext(ctx, "CREATE DATABASE "+quoted+" OWNER databrain_migrator"); err != nil {
		t.Fatal("cannot create isolated migration test database")
	}
	t.Cleanup(func() {
		var actualOwner string
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := owner.QueryRowContext(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", name).Scan(&actualOwner); err != nil || actualOwner != "databrain_migrator" {
			t.Error("cleanup refused: test database ownership changed")
			return
		}
		if _, err := owner.ExecContext(ctx, "DROP DATABASE "+quoted); err != nil {
			t.Error("could not drop isolated migration test database")
		}
	})
	if _, err := owner.ExecContext(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC; GRANT CONNECT ON DATABASE "+quoted+" TO databrain_runtime"); err != nil {
		t.Fatal("cannot isolate migration test database")
	}
	u.Path = "/" + name
	u.User = url.UserPassword("databrain_migrator", os.Getenv("DATABRAIN_MIGRATOR_PASSWORD"))
	migratorURL := u.String()
	u.User = url.UserPassword("databrain_runtime", os.Getenv("DATABRAIN_RUNTIME_PASSWORD"))
	return migratorURL, u.String()
}

func TestPlatformMigrations(t *testing.T) {
	migratorURL, runtimeURL := migrationFixture(t)
	result, err := Up(migratorURL)
	if err != nil || result != (Result{Version: Version, HasVersion: true}) {
		t.Fatalf("fresh apply failed: result=%+v error=%v", result, err)
	}
	result, err = Up(migratorURL)
	if err != nil || result != (Result{Version: Version, HasVersion: true, NoChange: true}) {
		t.Fatalf("rerun failed: result=%+v error=%v", result, err)
	}
	db, err := sql.Open("postgres", runtimeURL)
	if err != nil {
		t.Fatal("cannot open runtime test connection")
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal("cannot begin runtime verification")
	}
	defer tx.Rollback()
	exec := func(query string) {
		t.Helper()
		if _, err := tx.Exec(query); err != nil {
			t.Fatal("schema fixture statement failed")
		}
	}
	denied := func(query, code string) {
		t.Helper()
		exec("SAVEPOINT constraint_case")
		_, err := tx.Exec(query)
		var pgErr *pq.Error
		if !errors.As(err, &pgErr) || string(pgErr.Code) != code {
			t.Fatalf("expected SQLSTATE %s for %s", code, query)
		}
		exec("ROLLBACK TO SAVEPOINT constraint_case")
		exec("RELEASE SAVEPOINT constraint_case")
	}
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM databrain.security_guard WHERE singleton").Scan(&count); err != nil || count != 1 {
		t.Fatal("security guard seed missing or duplicated")
	}
	if err := tx.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema='databrain'").Scan(&count); err != nil || count != 12 {
		t.Fatal("runtime cannot read all 12 application tables")
	}
	var version int
	var dirty bool
	if err := tx.QueryRow("SELECT version, dirty FROM public.schema_migrations").Scan(&version, &dirty); err != nil || uint(version) != Version || dirty {
		t.Fatal("runtime cannot inspect clean schema version")
	}
	// Synthetic hashes/envelopes satisfy storage constraints; no auth is implemented here.
	exec(`INSERT INTO databrain.users (id,username,display_name,password_hash) VALUES
	    ('00000000-0000-0000-0000-000000000001','test_actor','Test Actor','synthetic')`)
	exec(`INSERT INTO databrain.groups (id,name) VALUES ('00000000-0000-0000-0000-000000000001','Editors')`)
	denied(`INSERT INTO databrain.groups (id,name) VALUES ('00000000-0000-0000-0000-000000000002','editors')`, "23505")
	exec(`INSERT INTO databrain.memberships (group_id,user_id) VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001')`)
	denied(`INSERT INTO databrain.memberships SELECT * FROM databrain.memberships`, "23505")
	exec(`INSERT INTO databrain.database_targets (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
	    SELECT id,'Target ' || id,'localhost',55432,'target_' || id,'target',decode(repeat('00',30),'hex'),'disable','00000000-0000-0000-0000-000000000001'
	    FROM (VALUES ('00000000-0000-0000-0000-000000000001'::uuid),('00000000-0000-0000-0000-000000000002'::uuid)) v(id)`)
	denied(`UPDATE databrain.database_targets SET host='elsewhere'`, "P0001")
	exec(`INSERT INTO databrain.database_grants (group_id,target_id,access_level) VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001','view_edit')`)
	denied(`UPDATE databrain.database_grants SET access_level='admin'`, "23514")
	exec(`INSERT INTO databrain.migration_drafts (id,target_id,title,sql,created_by,updated_by)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001','Draft','SELECT 1',
	    '00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001')`)
	attempt := `INSERT INTO databrain.migration_attempts (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,
	    target_config_revision,sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',1,1,
	    'Draft','Target',1,'SELECT 1',decode(repeat('00',32),'hex'),'00000000-0000-0000-0000-000000000001','test_actor','00000000-0000-0000-0000-000000000001','reserved')`
	exec(attempt)
	denied(strings.ReplaceAll(attempt, "'reserved'", "'running'"), "P0001")
	other := strings.Replace(attempt, "VALUES ('00000000-0000-0000-0000-000000000001'", "VALUES ('00000000-0000-0000-0000-000000000002'", 1)
	other = strings.Replace(other, ",1,1,", ",1,2,", 1)
	denied(other, "23505") // Same actor/key is unique.
	other = strings.Replace(other, "'test_actor','00000000-0000-0000-0000-000000000001'", "'test_actor','00000000-0000-0000-0000-000000000002'", 1)
	denied(other, "23505") // A different key cannot create a second unresolved attempt.
	denied(`UPDATE databrain.migration_attempts SET sql_snapshot='SELECT 2'`, "P0001")
	denied(`UPDATE databrain.migration_attempts SET state='succeeded',finished_at=now(),outcome_evidence='commit_ack'`, "P0001")
	denied(`UPDATE databrain.migration_attempts SET state='failed',finished_at=now(),failure_code='SQL_ERROR',failure_message='Failed'`, "23514")
	exec(`UPDATE databrain.migration_attempts SET state='unknown',failure_code='PROCESS_INTERRUPTED',failure_message='Interrupted'`)
	denied(`UPDATE databrain.migration_attempts SET first_unknown_at=now()+interval '1 second'`, "P0001")
	denied(`UPDATE databrain.migration_attempts SET state='succeeded',finished_at=now(),outcome_evidence='commit_ack',failure_code=NULL,failure_message=NULL`, "P0001")
	exec(`UPDATE databrain.migration_attempts SET state='succeeded',finished_at=now(),outcome_evidence='operator_commit',failure_code=NULL,failure_message=NULL,
	    resolved_at=now(),resolved_by_operator='test',resolution_reason='Synthetic fixture evidence'`)
	denied(`UPDATE databrain.migration_attempts SET finished_at=now()+interval '1 second'`, "P0001")
	exec(other)
	exec(`UPDATE databrain.migration_attempts SET state='running',started_at=now() WHERE version=2`)
	denied(`UPDATE databrain.migration_attempts SET state='succeeded',finished_at=now(),outcome_evidence='commit_ack' WHERE version=2`, "23505")
	denied(`INSERT INTO databrain.migration_attempts (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,
	    target_config_revision,sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
	    SELECT '00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000002',draft_id,draft_revision,1,title_snapshot,
	    target_name_snapshot,target_config_revision,sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,'reserved'
	    FROM databrain.migration_attempts WHERE version=1`, "23503")
	exec(`INSERT INTO databrain.migration_streams (target_id,active_attempt_id) VALUES ('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001')`)
	denied(`SET CONSTRAINTS ALL IMMEDIATE`, "23503")
	exec(`DELETE FROM databrain.session_tokens WHERE false`)
	exec(`INSERT INTO databrain.sessions (id,kind,current_token_hash,current_token_sealed,absolute_expires_at)
	    VALUES ('00000000-0000-0000-0000-000000000001','anonymous',decode(repeat('00',32),'hex'),decode(repeat('00',30),'hex'),now()+interval '10 minutes')`)
	denied(`SET CONSTRAINTS databrain.session_canonical_token_fk IMMEDIATE`, "23503")
	exec(`INSERT INTO databrain.session_tokens (session_id,token_hash,valid_until) VALUES
	    ('00000000-0000-0000-0000-000000000001',decode(repeat('00',32),'hex'),now()+interval '10 minutes')`)
	exec(`SET CONSTRAINTS databrain.session_canonical_token_fk IMMEDIATE`)
	denied(`DELETE FROM databrain.session_tokens`, "23503")
	for _, query := range []string{
		`CREATE SCHEMA runtime_forbidden`,
		`CREATE TABLE databrain.runtime_forbidden (id int)`,
		`ALTER TABLE databrain.users ADD COLUMN forbidden int`,
		`ALTER TABLE databrain.migration_attempts DISABLE TRIGGER ALL`,
		`CREATE OR REPLACE FUNCTION databrain.protect_attempt() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`,
		`DELETE FROM databrain.migration_attempts`,
		`TRUNCATE databrain.migration_attempts`,
		`DELETE FROM databrain.migration_drafts`,
		`UPDATE public.schema_migrations SET dirty=true`,
		`DELETE FROM databrain.security_guard`,
		`SET ROLE databrain_migrator`,
	} {
		denied(query, "42501")
	}
}

func TestFailedMigrationStaysDirty(t *testing.T) {
	migratorURL, _ := migrationFixture(t)
	broken := fstest.MapFS{"000001_broken.up.sql": {Data: []byte("CREATE TABLE public.partial_migration (id int); SELECT SENSITIVE_INVALID_SQL;")}}
	result, err := up(migratorURL, broken)
	if err == nil || !result.HasVersion || result.Version != 1 || !result.Dirty || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatalf("failed migration did not report safe dirty state: %+v, %v", result, err)
	}
	db, _ := sql.Open("postgres", migratorURL)
	defer db.Close()
	var rolledBack bool
	if err := db.QueryRow("SELECT to_regclass('public.partial_migration') IS NULL").Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatal("failed multi-statement migration left partial DDL")
	}
	result, err = Up(migratorURL)
	if err == nil || !result.Dirty || !strings.Contains(err.Error(), "dirty") {
		t.Fatal("dirty schema was reset or accepted on rerun")
	}
}
