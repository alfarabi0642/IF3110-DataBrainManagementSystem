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

// Test hanya membuat dan menghapus database bernama unik pada cluster disposable tervalidasi. Database
// aplikasi dan target yang ada tidak direset.
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
	if err := tx.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema='databrain'").Scan(&count); err != nil || count != 25 {
		t.Fatal("runtime cannot read all 25 application tables")
	}
	var version int
	var dirty bool
	if err := tx.QueryRow("SELECT version, dirty FROM public.schema_migrations").Scan(&version, &dirty); err != nil || uint(version) != Version || dirty {
		t.Fatal("runtime cannot inspect clean schema version")
	}
	// Hash dan envelope sintetis memenuhi constraint penyimpanan; test ini tidak menguji autentikasi.
	exec(`INSERT INTO databrain.users (id,username,display_name,password_hash) VALUES
	    ('00000000-0000-0000-0000-000000000001','test_actor','Test Actor','synthetic')`)
	exec(`INSERT INTO databrain.groups (id,name) VALUES ('00000000-0000-0000-0000-000000000001','Editors')`)
	denied(`INSERT INTO databrain.groups (id,name) VALUES ('00000000-0000-0000-0000-000000000002','editors')`, "23505")
	exec(`INSERT INTO databrain.groups (id,name) VALUES ('00000000-0000-0000-0000-000000000002','Operators')`)
	exec(`INSERT INTO databrain.memberships (group_id,user_id) VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001')`)
	denied(`INSERT INTO databrain.memberships SELECT * FROM databrain.memberships`, "23505")
	exec(`INSERT INTO databrain.database_targets (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
	    SELECT id,'Target ' || id,'localhost',55432,'target_' || id,'target',decode(repeat('00',30),'hex'),'disable','00000000-0000-0000-0000-000000000001'
	    FROM (VALUES ('00000000-0000-0000-0000-000000000001'::uuid),('00000000-0000-0000-0000-000000000002'::uuid)) v(id)`)
	denied(`UPDATE databrain.database_targets SET host='elsewhere'`, "P0001")
	exec(`UPDATE databrain.groups SET leader_id='00000000-0000-0000-0000-000000000001' WHERE id='00000000-0000-0000-0000-000000000001'`)
	denied(`UPDATE databrain.groups SET leader_id='00000000-0000-0000-0000-000000000001' WHERE id='00000000-0000-0000-0000-000000000002'`, "23503")
	exec(`INSERT INTO databrain.migration_policy_generations (target_id) VALUES ('00000000-0000-0000-0000-000000000001')`)
	exec(`UPDATE databrain.migration_policy_generations SET generation=2 WHERE target_id='00000000-0000-0000-0000-000000000001'`)
	denied(`UPDATE databrain.migration_policy_generations SET generation=0`, "23514")
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
	denied(`UPDATE databrain.migration_drafts SET title='Frozen edit' WHERE id='00000000-0000-0000-0000-000000000001'`, "P0001")
	exec(`INSERT INTO databrain.migration_definitions
	    (id,target_id,draft_id,revision,up_sql,down_sql,irreversible,irreversible_reason,up_sha256,down_sha256,source)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-000000000001',1,'SELECT 1','',true,'Legacy M1 revision has no down SQL.',
	    decode(repeat('00',32),'hex'),NULL,'legacy_attempt')`)
	denied(`UPDATE databrain.migration_definitions SET up_sql='SELECT 2'`, "42501")
	exec(`INSERT INTO databrain.migration_requests
	    (id,target_id,draft_id,definition_id,actor_id,actor_username,revision,target_config_revision,
	     head_generation,policy_generation,request_key)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-000000000001','test_actor',1,1,1,1,
	    '00000000-0000-0000-0000-0000000000d1')`)
	exec(`INSERT INTO databrain.migration_request_groups
	    (target_id,request_id,group_id,group_name,access_level,leader_id)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-000000000001','Editors','view_edit','00000000-0000-0000-0000-000000000001')`)
	exec(`INSERT INTO databrain.migration_request_decisions
	    (target_id,request_id,group_id,actor_id,leader_id)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-000000000001')`)
	exec(`UPDATE databrain.migration_requests SET state='approved',updated_at=now()`)
	denied(`UPDATE databrain.migration_requests SET revision=2`, "P0001")
	denied(`UPDATE databrain.migration_request_decisions SET actor_id='00000000-0000-0000-0000-000000000002'`, "42501")
	denied(strings.ReplaceAll(attempt, "'reserved'", "'running'"), "P0001")
	other := strings.Replace(attempt, "VALUES ('00000000-0000-0000-0000-000000000001'", "VALUES ('00000000-0000-0000-0000-000000000002'", 1)
	other = strings.Replace(other, ",1,1,", ",1,2,", 1)
	denied(other, "23505") // Pasangan actor/key harus unik.
	other = strings.Replace(other, "'test_actor','00000000-0000-0000-0000-000000000001'", "'test_actor','00000000-0000-0000-0000-000000000002'", 1)
	denied(other, "23505") // Key lain tidak boleh membuat attempt belum selesai kedua.
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
	exec(`INSERT INTO databrain.migration_executions
	    (id,target_id,actor_id,request_id,definition_id,revision,target_config_revision,direction,mode,plan_hash,sql_snapshot)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-0000000000a1','00000000-0000-0000-0000-0000000000b1',1,1,'apply','normal',repeat('0',64),'SELECT 1')`)
	denied(`INSERT INTO databrain.migration_executions
	    (id,target_id,actor_id,request_id,definition_id,revision,target_config_revision,direction,mode,plan_hash,sql_snapshot,phase)
	    VALUES ('00000000-0000-0000-0000-000000000009','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-0000000000a2','00000000-0000-0000-0000-0000000000b2',1,1,'apply','normal',repeat('0',64),'SELECT 1','running')`, "P0001")
	denied(`UPDATE databrain.migration_executions SET sql_snapshot='SELECT 2'`, "P0001")
	denied(`UPDATE databrain.migration_executions SET phase='finished'`, "P0001") // Outcome terminal wajib diisi bersamaan.
	denied(`UPDATE databrain.migration_requests SET state='consumed'`, "23514")
	exec(`UPDATE databrain.migration_requests SET state='consumed',
	    execution_id='00000000-0000-0000-0000-000000000001',job_id=1,
	    consumed_request_key='00000000-0000-0000-0000-0000000000d1',consumed_at=now()`)
	denied(`UPDATE databrain.migration_requests SET state='stale'`, "P0001")
	exec(`INSERT INTO databrain.migration_committed_events
	    (id,target_id,execution_id,definition_id,actor_id,revision,direction,mode,plan_hash,commit_evidence,
	     parent_execution_id,canceled_execution_id,generation,committed_at)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-000000000001',1,'apply','normal',repeat('0',64),'commit_ack',
	    NULL,NULL,2,now())`)
	exec(`UPDATE databrain.migration_target_heads SET active_execution_id='00000000-0000-0000-0000-000000000001',
	    generation=2,updated_at=now() WHERE target_id='00000000-0000-0000-0000-000000000001'`)
	denied(`UPDATE databrain.migration_committed_events SET revision=2`, "42501")
	exec(`INSERT INTO databrain.execution_receipts(target_id,actor_id,request_key,intent_hash,execution_id,job_id)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
	    '00000000-0000-0000-0000-0000000000c1',repeat('0',64),'00000000-0000-0000-0000-000000000001',1)`)
	denied(`UPDATE databrain.execution_receipts SET job_id=2`, "42501") // Runtime tidak memiliki UPDATE receipt.
	exec(`INSERT INTO databrain.migration_worker_attempts(execution_id,attempt_number,job_id)
	    VALUES ('00000000-0000-0000-0000-000000000001',1,1)`)
	exec(`UPDATE databrain.migration_worker_attempts SET state='failed',finished_at=now(),
	    failure_code='SQL_ERROR',failure_message='Failed'`)
	denied(`UPDATE databrain.migration_worker_attempts SET failure_message='Changed'`, "P0001")
	exec(`INSERT INTO databrain.migration_streams (target_id,active_execution_id)
	    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001')`)
	denied(`UPDATE databrain.migration_streams SET active_attempt_id='00000000-0000-0000-0000-000000000001'`, "23514")
	exec(`UPDATE databrain.migration_streams SET active_execution_id=NULL`)
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
		`DELETE FROM databrain.migration_executions`,
		`DELETE FROM databrain.execution_receipts`,
		`DELETE FROM databrain.migration_definitions`,
		`DELETE FROM databrain.migration_definition_issues`,
		`DELETE FROM databrain.migration_policy_generations`,
		`DELETE FROM databrain.migration_requests`,
		`DELETE FROM databrain.migration_request_groups`,
		`DELETE FROM databrain.migration_request_decisions`,
		`DELETE FROM databrain.migration_committed_events`,
		`DELETE FROM databrain.migration_target_heads`,
		`ALTER TABLE databrain.migration_committed_events DISABLE TRIGGER ALL`,
		`ALTER TABLE databrain.migration_requests DISABLE TRIGGER ALL`,
		`UPDATE databrain.migration_definitions SET source='submission'`,
		`ALTER TABLE databrain.migration_definitions DISABLE TRIGGER ALL`,
		`ALTER TABLE databrain.migration_executions DISABLE TRIGGER ALL`,
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

func TestM1UpgradePreservesAttemptsAndUnknownGate(t *testing.T) {
	migratorURL, _ := migrationFixture(t)
	versionOne, err := files.ReadFile("000001_platform.up.sql")
	if err != nil {
		t.Fatal("cannot read embedded version 1 migration")
	}
	partial := fstest.MapFS{"000001_platform.up.sql": {Data: versionOne}}
	result, err := up(migratorURL, partial)
	if err == nil || !result.HasVersion || result.Version != 1 || result.Dirty {
		t.Fatalf("partial version 1 fixture failed safely: %+v, %v", result, err)
	}
	db, err := sql.Open("postgres", migratorURL)
	if err != nil {
		t.Fatal("cannot open migration fixture")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const legacy = `
INSERT INTO databrain.users (id,username,display_name,password_hash)
    VALUES ('00000000-0000-0000-0000-000000000001','upgrade_actor','Upgrade Actor','synthetic');
INSERT INTO databrain.database_targets (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
    VALUES ('00000000-0000-0000-0000-000000000001','Upgrade target','localhost',55432,'upgrade_db','target',
    decode(repeat('00',30),'hex'),'disable','00000000-0000-0000-0000-000000000001');
INSERT INTO databrain.migration_drafts (id,target_id,title,sql,created_by,updated_by)
    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001','Upgrade draft','SELECT 1',
    '00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001');
INSERT INTO databrain.migration_attempts (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,
    target_config_revision,sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',1,1,'Upgrade draft','Upgrade target',1,'SELECT 1',
    decode(repeat('00',32),'hex'),'00000000-0000-0000-0000-000000000001','upgrade_actor',
    '00000000-0000-0000-0000-000000000001','reserved');
UPDATE databrain.migration_attempts SET state='unknown',failure_code='PROCESS_INTERRUPTED',
    failure_message='Upgrade fixture' WHERE id='00000000-0000-0000-0000-000000000001';
INSERT INTO databrain.migration_streams (target_id,next_version,active_attempt_id)
    VALUES ('00000000-0000-0000-0000-000000000001',2,'00000000-0000-0000-0000-000000000001');`
	if _, err := db.ExecContext(ctx, legacy); err != nil {
		t.Fatalf("cannot seed version 1 unknown gate: %v", err)
	}
	result, err = Up(migratorURL)
	if err != nil || result.Version != Version || result.Dirty || result.NoChange {
		t.Fatalf("version 1 upgrade failed: %+v, %v", result, err)
	}
	var state, sqlSnapshot, failure, activeAttempt, activeExecution, executions, receipts string
	err = db.QueryRowContext(ctx, `SELECT a.state,a.sql_snapshot,a.failure_code,
	    coalesce(s.active_attempt_id::text,''),coalesce(s.active_execution_id::text,''),
	    (SELECT count(*)::text FROM databrain.migration_executions),
	    (SELECT count(*)::text FROM databrain.execution_receipts)
	    FROM databrain.migration_attempts a JOIN databrain.migration_streams s ON s.target_id=a.target_id
	    WHERE a.id='00000000-0000-0000-0000-000000000001'`).
		Scan(&state, &sqlSnapshot, &failure, &activeAttempt, &activeExecution, &executions, &receipts)
	if err != nil || state != "unknown" || sqlSnapshot != "SELECT 1" || failure != "PROCESS_INTERRUPTED" ||
		activeAttempt == "" || activeExecution != "" || executions != "0" || receipts != "0" {
		t.Fatalf("upgrade changed legacy attempt or unknown gate: %q %q %q %q %q %s/%s %v",
			state, sqlSnapshot, failure, activeAttempt, activeExecution, executions, receipts, err)
	}
	var definitionSQL, definitionReason, definitionSource string
	var irreversible bool
	err = db.QueryRowContext(ctx, `SELECT up_sql, irreversible, coalesce(irreversible_reason,''), source
	    FROM databrain.migration_definitions WHERE target_id='00000000-0000-0000-0000-000000000001'
	    AND draft_id='00000000-0000-0000-0000-000000000001' AND revision=1`).
		Scan(&definitionSQL, &irreversible, &definitionReason, &definitionSource)
	if err != nil || definitionSQL != "SELECT 1" || !irreversible || definitionReason == "" || definitionSource != "legacy_attempt" {
		t.Fatalf("legacy definition backfill wrong: %q %v %q %q %v", definitionSQL, irreversible, definitionReason, definitionSource, err)
	}
	var draftIrreversible bool
	var draftReason string
	if err := db.QueryRowContext(ctx, `SELECT irreversible, coalesce(irreversible_reason,'')
	    FROM databrain.migration_drafts WHERE target_id='00000000-0000-0000-0000-000000000001'
	    AND id='00000000-0000-0000-0000-000000000001'`).Scan(&draftIrreversible, &draftReason); err != nil || !draftIrreversible || draftReason == "" {
		t.Fatalf("legacy draft did not receive its irreversible reason: %v %q %v", draftIrreversible, draftReason, err)
	}
	var issues int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM databrain.migration_definition_issues`).Scan(&issues); err != nil || issues != 0 {
		t.Fatalf("consistent legacy import created issues: %d %v", issues, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE databrain.migration_definitions SET up_sql='SELECT 2'`); err == nil {
		t.Fatal("migrator changed an immutable definition")
	} else {
		var pgErr *pq.Error
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
			t.Fatalf("definition mutation returned unexpected error: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE databrain.migration_drafts SET title='Frozen after upgrade'`); err == nil {
		t.Fatal("frozen legacy draft was updated after upgrade")
	} else {
		var pgErr *pq.Error
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
			t.Fatalf("frozen draft update returned unexpected error: %v", err)
		}
	}
}

func TestHistoricalV2RejectedAndUnmatchedExecutionReported(t *testing.T) {
	migratorURL, _ := migrationFixture(t)
	versionOne, err := files.ReadFile("000001_platform.up.sql")
	if err != nil {
		t.Fatal("cannot read embedded version 1 migration")
	}
	versionTwo, err := files.ReadFile("000002_platform.up.sql")
	if err != nil {
		t.Fatal("cannot read embedded version 2 migration")
	}
	const definitionsStart = "-- Immutable migration definitions, legacy import, and frozen draft identities."
	historical, remaining, found := strings.Cut(string(versionTwo), definitionsStart)
	if !found {
		t.Fatal("cannot find historical version 2 boundary")
	}
	partial := fstest.MapFS{
		"000001_platform.up.sql": {Data: versionOne},
		"000002_platform.up.sql": {Data: []byte(historical)},
	}
	result, err := up(migratorURL, partial)
	if err == nil || !strings.Contains(err.Error(), "predates the consolidated migration") || !result.HasVersion || result.Version != Version || result.Dirty {
		t.Fatalf("partial version 2 fixture failed safely: %+v, %v", result, err)
	}
	db, err := sql.Open("postgres", migratorURL)
	if err != nil {
		t.Fatal("cannot open migration fixture")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const seed = `
INSERT INTO databrain.users (id,username,display_name,password_hash)
    VALUES ('00000000-0000-0000-0000-000000000001','upgrade_actor','Upgrade Actor','synthetic');
INSERT INTO databrain.database_targets (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
    VALUES ('00000000-0000-0000-0000-000000000001','Upgrade target','localhost',55432,'upgrade_db','target',
    decode(repeat('00',30),'hex'),'disable','00000000-0000-0000-0000-000000000001');
INSERT INTO databrain.migration_executions
    (id,target_id,actor_id,request_id,definition_id,revision,target_config_revision,direction,mode,plan_hash,sql_snapshot,phase)
    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-0000000000a1',
    '00000000-0000-0000-0000-0000000000b1',1,1,'apply','normal',repeat('0',64),'SELECT 1','queued');`
	if _, err := db.ExecContext(ctx, seed); err != nil {
		t.Fatalf("cannot seed version 2 execution: %v", err)
	}
	// A clean old v2 must not be mistaken for the complete schema.
	result, err = Up(migratorURL)
	if err == nil || !strings.Contains(err.Error(), "predates the consolidated migration") || !result.NoChange || result.Dirty {
		t.Fatalf("historical version 2 was not rejected safely: %+v, %v", result, err)
	}
	// Exercise the existing report SQL against seeded old v2 data in this fixture.
	if _, err := db.ExecContext(ctx, definitionsStart+remaining); err != nil {
		t.Fatalf("legacy execution report statements failed: %v", err)
	}
	var issues int
	var detail string
	if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(max(detail),'') FROM databrain.migration_definition_issues
	    WHERE kind='unmatched_execution' AND target_id='00000000-0000-0000-0000-000000000001'`).Scan(&issues, &detail); err != nil {
		t.Fatal(err)
	}
	if issues != 1 || !strings.Contains(detail, "00000000-0000-0000-0000-000000000001") {
		t.Fatalf("unmatched execution not reported: %d %q", issues, detail)
	}
	var definitions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM databrain.migration_definitions`).Scan(&definitions); err != nil || definitions != 0 {
		t.Fatalf("unexpected legacy definitions: %d %v", definitions, err)
	}
}

func TestLegacyDefinitionConflictReported(t *testing.T) {
	migratorURL, _ := migrationFixture(t)
	versionOne, err := files.ReadFile("000001_platform.up.sql")
	if err != nil {
		t.Fatal("cannot read embedded version 1 migration")
	}
	partial := fstest.MapFS{"000001_platform.up.sql": {Data: versionOne}}
	result, err := up(migratorURL, partial)
	if err == nil || !result.HasVersion || result.Version != 1 || result.Dirty {
		t.Fatalf("partial version 1 fixture failed: %+v, %v", result, err)
	}
	db, err := sql.Open("postgres", migratorURL)
	if err != nil {
		t.Fatal("cannot open migration fixture")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const seed = `
INSERT INTO databrain.users (id,username,display_name,password_hash)
    VALUES ('00000000-0000-0000-0000-000000000001','conflict_actor','Conflict Actor','synthetic');
INSERT INTO databrain.database_targets (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
    VALUES ('00000000-0000-0000-0000-000000000001','Conflict target','localhost',55432,'conflict_db','target',
    decode(repeat('00',30),'hex'),'disable','00000000-0000-0000-0000-000000000001');
INSERT INTO databrain.migration_drafts (id,target_id,title,sql,created_by,updated_by)
    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001','Conflict draft','SELECT 2',
    '00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001');
INSERT INTO databrain.migration_attempts (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,
    target_config_revision,sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
    VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',1,1,'Conflict draft','Conflict target',1,'SELECT 1',
    decode(repeat('00',32),'hex'),'00000000-0000-0000-0000-000000000001','conflict_actor',
    '00000000-0000-0000-0000-0000000000a1','reserved');
UPDATE databrain.migration_attempts SET state='failed',finished_at=now(),outcome_evidence='rollback_ack',
    failure_code='SQL_ERROR',failure_message='Synthetic failure.' WHERE id='00000000-0000-0000-0000-000000000001';
INSERT INTO databrain.migration_attempts (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,
    target_config_revision,sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
    VALUES ('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',1,2,'Conflict draft','Conflict target',1,'SELECT 2',
    decode(repeat('00',32),'hex'),'00000000-0000-0000-0000-000000000001','conflict_actor',
    '00000000-0000-0000-0000-0000000000a2','reserved');`
	if _, err := db.ExecContext(ctx, seed); err != nil {
		t.Fatalf("cannot seed conflicting legacy attempts: %v", err)
	}
	result, err = Up(migratorURL)
	if err != nil || result.Version != Version || result.Dirty {
		t.Fatalf("conflict upgrade failed: %+v, %v", result, err)
	}
	var issues, definitions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM databrain.migration_definition_issues
	    WHERE kind='legacy_snapshot_mismatch'`).Scan(&issues); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM databrain.migration_definitions`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if issues != 1 || definitions != 0 {
		t.Fatalf("conflicting legacy snapshot was not isolated: issues=%d definitions=%d", issues, definitions)
	}
	if _, err := db.ExecContext(ctx, `UPDATE databrain.migration_drafts SET title='frozen conflict'`); err == nil {
		t.Fatal("draft with conflicting attempts was not frozen")
	}
}
