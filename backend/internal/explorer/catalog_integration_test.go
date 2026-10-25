package explorer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type catalogFixture struct {
	e         *Explorer
	pool      *pgxpool.Pool
	secrets   *secret.Secrets
	owner     *pgx.Conn
	writers   []*pgx.Conn
	targets   []uuid.UUID
	databases []string
}

func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

func newCatalogFixture(t *testing.T) catalogFixture {
	t.Helper()
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	if raw == "" {
		t.Skip("set DATABRAIN_TEST_ADMIN_URL for the verified A-03 disposable fixture")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.User == nil || u.User.Username() != "databrain_fixture_owner" || u.Path != "/postgres" {
		t.Fatal("catalog test requires the A-03 disposable fixture owner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("cannot connect to disposable fixture")
	}
	t.Cleanup(func() { closeConn(owner) })
	var directory, role, database string
	err = owner.QueryRow(ctx, `SELECT current_setting('data_directory'),current_user,current_database()`).Scan(&directory, &role, &database)
	expected, pathErr := filepath.Abs("../../../.tmp/postgres/data")
	if err != nil || pathErr != nil || !strings.EqualFold(filepath.Clean(directory), filepath.Clean(expected)) ||
		role != "databrain_fixture_owner" || database != "postgres" {
		t.Fatal("fixture identity does not match this repository; mutation refused")
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	names := []string{"db_catalog_platform_" + suffix, "db_catalog_first_" + suffix, "db_catalog_second_" + suffix}
	for i, name := range names {
		dbOwner := "databrain_target"
		if i == 0 {
			dbOwner = "databrain_migrator"
		}
		quoted := pgx.Identifier{name}.Sanitize()
		if _, err := owner.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER "+pgx.Identifier{dbOwner}.Sanitize()); err != nil {
			t.Fatal("cannot create isolated catalog database")
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
				t.Error("cannot remove catalog database; check for leaked connections")
			}
		})
		if _, err := owner.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC"); err != nil {
			t.Fatal("cannot isolate catalog database")
		}
	}
	if _, err := owner.Exec(ctx, "GRANT CONNECT ON DATABASE "+pgx.Identifier{names[0]}.Sanitize()+" TO databrain_runtime"); err != nil {
		t.Fatal("cannot grant runtime access to catalog platform")
	}
	u.Path = "/" + names[0]
	u.User = url.UserPassword("databrain_migrator", os.Getenv("DATABRAIN_MIGRATOR_PASSWORD"))
	if _, err := migrations.Up(u.String()); err != nil {
		t.Fatal("catalog platform migration failed")
	}
	u.User = url.UserPassword("databrain_runtime", os.Getenv("DATABRAIN_RUNTIME_PASSWORD"))
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal("cannot open catalog platform pool")
	}
	t.Cleanup(pool.Close)
	secrets, err := secret.New(bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal("cannot initialize synthetic catalog key")
	}
	provider, err := targets.NewProvider(pool, secrets, config.TargetSettings{AppEnv: "test", AllowedHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal("cannot initialize real catalog provider")
	}
	actor := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO databrain.users(id,username,display_name,password_hash) VALUES ($1,'catalog_fixture','Catalog fixture','fixture-not-an-account-hash')`, actor); err != nil {
		t.Fatal("cannot create synthetic catalog actor")
	}
	f := catalogFixture{e: New(provider), pool: pool, secrets: secrets, owner: owner, databases: names[1:]}
	for _, name := range names[1:] {
		input := targets.CreateInput{Name: name, Host: "127.0.0.1", Port: 55432, DatabaseName: name,
			Username: "databrain_target", Password: os.Getenv("DATABRAIN_TARGET_PASSWORD"), SSLMode: "disable"}
		target, err := provider.Create(ctx, actor, input)
		if err != nil {
			t.Fatal("cannot create catalog target through real provider")
		}
		f.targets = append(f.targets, target.ID)
		u.Path = "/" + name
		u.User = url.UserPassword(input.Username, input.Password)
		writer, err := pgx.Connect(ctx, u.String())
		if err != nil {
			t.Fatal("cannot open fixture target writer")
		}
		t.Cleanup(func() { closeConn(writer) })
		f.writers = append(f.writers, writer)
	}
	return f
}

func fixtureSQL(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, sql); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.Fatalf("catalog fixture SQL failed (SQLSTATE %s)", pgErr.Code)
		}
		t.Fatal("catalog fixture SQL failed")
	}
}

func TestCatalogPostgres(t *testing.T) {
	f := newCatalogFixture(t)
	ctx := context.Background()
	schema, table := `Odd "猫.schema`, `Order "猫.table`
	qs := pgx.Identifier{schema}.Sanitize()
	qt := pgx.Identifier{schema, table}.Sanitize()
	parent := pgx.Identifier{schema, "parent"}.Sanitize()
	fixtureSQL(t, f.writers[0], "CREATE SCHEMA "+qs+"; CREATE SCHEMA empty_schema; "+
		"CREATE TABLE "+parent+` ("Key b" int,"Key a" int,PRIMARY KEY ("Key b","Key a")); `+
		"CREATE TABLE "+qt+` (id bigint GENERATED ALWAYS AS IDENTITY,"Key a" int NOT NULL,"Key b" int,removed text,
		amount numeric(30,4) DEFAULT 1.2500,note text DEFAULT '',computed int GENERATED ALWAYS AS ("Key a"+1) STORED,
		CONSTRAINT "z pk" PRIMARY KEY ("Key b","Key a"),CONSTRAINT "a fk" FOREIGN KEY ("Key b","Key a") REFERENCES `+parent+`,
		CONSTRAINT "m check" CHECK (amount>=0)); ALTER TABLE `+qt+` DROP COLUMN removed;
		CREATE INDEX "hash idx" ON `+qt+` USING hash ("Key a");
		CREATE INDEX "expression idx" ON `+qt+` (lower(note)) WHERE amount>0;
		CREATE TABLE `+qs+`.events (id int PRIMARY KEY) PARTITION BY RANGE (id);
		CREATE TABLE `+qs+`.events_0 PARTITION OF `+qs+`.events FOR VALUES FROM (0) TO (10);
		CREATE VIEW `+qs+`.hidden_view AS SELECT 1 AS id;
		CREATE MATERIALIZED VIEW `+qs+`.hidden_materialized AS SELECT 1 AS id;
		CREATE TEMP TABLE temp_hidden (value text)`)
	fixtureSQL(t, f.writers[1], "CREATE SCHEMA "+qs+"; CREATE TABLE "+qt+" (only_second text)")

	read := func(t *testing.T, target uuid.UUID, schema, table string) Catalog {
		t.Helper()
		result, err := f.e.Catalog(ctx, target, schema, table)
		if err != nil {
			t.Fatalf("catalog read failed: %v", err)
		}
		return result
	}
	t.Run("schemas and table kinds", func(t *testing.T) {
		catalog := read(t, f.targets[0], "", "")
		if !slices.Contains(catalog.Schemas, schema) || !slices.Contains(catalog.Schemas, "empty_schema") || !slices.IsSorted(catalog.Schemas) {
			t.Fatal("user schemas are missing or unordered")
		}
		for _, name := range catalog.Schemas {
			if !userSchema(name) {
				t.Fatal("system or temporary schema is visible")
			}
		}
		catalog = read(t, f.targets[0], schema, "")
		var names []string
		for _, relation := range catalog.Tables {
			names = append(names, relation.Name)
			if relation.Schema != schema || (relation.Kind != "r" && relation.Kind != "p") {
				t.Fatal("incorrect table identity or kind")
			}
		}
		if !reflect.DeepEqual(names, []string{table, "events", "events_0", "parent"}) {
			t.Fatal("table listing included unsupported objects or lost quoted names")
		}
		if len(read(t, f.targets[0], "empty_schema", "").Tables) != 0 {
			t.Fatal("empty schema returned tables")
		}
	})
	t.Run("columns constraints and indexes", func(t *testing.T) {
		catalog := read(t, f.targets[0], schema, table)
		if catalog.Table == nil || catalog.Table.Name != table || catalog.Table.Schema != schema || catalog.Table.Kind != "r" || len(catalog.Columns) != 6 {
			t.Fatal("selected table identity or columns incorrect")
		}
		var ordinals []int
		for _, c := range catalog.Columns {
			ordinals = append(ordinals, c.Ordinal)
		}
		if !reflect.DeepEqual(ordinals, []int{1, 2, 3, 5, 6, 7}) || catalog.Columns[0].Identity != "a" || catalog.Columns[0].Type != "bigint" ||
			catalog.Columns[1].Nullable || catalog.Columns[1].Default != nil || catalog.Columns[3].Type != "numeric(30,4)" ||
			catalog.Columns[3].Default == nil || *catalog.Columns[3].Default != "1.2500" || catalog.Columns[4].Default == nil ||
			!catalog.Columns[4].Nullable || catalog.Columns[5].Generated != "s" || catalog.Columns[5].Default == nil {
			t.Fatal("column ordinal/type/default/nullability/identity/generated metadata incorrect")
		}
		if len(catalog.Constraints) != 3 || catalog.Constraints[0].Name != "a fk" || catalog.Constraints[0].Kind != "f" ||
			!reflect.DeepEqual(catalog.Constraints[0].Columns, []string{"Key b", "Key a"}) ||
			!strings.Contains(catalog.Constraints[0].Definition, "REFERENCES "+qs+".parent") ||
			catalog.Constraints[1].Kind != "c" || catalog.Constraints[2].Kind != "p" {
			t.Fatalf("constraint order, keys or FK definition incorrect: %+v", catalog.Constraints)
		}
		if len(catalog.Indexes) != 3 || catalog.Indexes[0].Name != "expression idx" || catalog.Indexes[0].Method != "btree" ||
			!strings.Contains(catalog.Indexes[0].Definition, "WHERE") || catalog.Indexes[1].Method != "hash" ||
			catalog.Indexes[1].Unique || !catalog.Indexes[2].Primary || !catalog.Indexes[2].Unique || !catalog.Indexes[2].Valid {
			t.Fatal("index method/flags/definition/order incorrect")
		}
		partitioned := read(t, f.targets[0], schema, "events")
		if partitioned.Table.Kind != "p" || len(partitioned.Constraints) != 1 || len(partitioned.Indexes) != 1 || !partitioned.Indexes[0].Primary {
			t.Fatal("partitioned table metadata incorrect")
		}
	})
	t.Run("target isolation and missing relations", func(t *testing.T) {
		second := read(t, f.targets[1], schema, table)
		if len(second.Columns) != 1 || second.Columns[0].Name != "only_second" || len(second.Constraints) != 0 || len(second.Indexes) != 0 {
			t.Fatal("catalog leaked data across targets")
		}
		for _, selection := range [][2]string{{"absent", ""}, {schema, "absent"}, {schema, "hidden_view"}, {schema, "hidden_materialized"}, {schema, `'; DROP TABLE parent;--`}} {
			_, err := f.e.Catalog(ctx, f.targets[0], selection[0], selection[1])
			checkCode(t, err, problem.RelationNotFound)
		}
		_, err := f.e.Catalog(ctx, uuid.New(), schema, table)
		checkCode(t, err, problem.DatabaseNotFound)
		read(t, f.targets[0], schema, "parent")
	})
	t.Run("explicit read only transaction", func(t *testing.T) {
		conn, err := f.e.provider.OpenExplorer(ctx, f.targets[0])
		if err != nil {
			t.Fatal("cannot open read-only fixture connection")
		}
		defer closeConn(conn)
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal("cannot begin read-only fixture transaction")
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, "CREATE TABLE must_not_write(id int)")
		pgErr, ok := err.(*pgconn.PgError)
		if !ok || pgErr.Code != "25006" {
			t.Fatal("target transaction permitted a write")
		}
	})
	t.Run("relation replacement and drop", func(t *testing.T) {
		for _, replace := range []bool{true, false} {
			name := "race_table"
			qualified := pgx.Identifier{schema, name}.Sanitize()
			fixtureSQL(t, f.writers[0], "CREATE TABLE "+qualified+" (old_column int)")
			conn, err := f.e.provider.OpenExplorer(ctx, f.targets[0])
			if err != nil {
				t.Fatal("cannot open race fixture reader")
			}
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			if err != nil {
				closeConn(conn)
				t.Fatal("cannot begin race reader")
			}
			var oid uint32
			var kind string
			if err := resolveTable(ctx, tx, schema, name, &oid, &kind); err != nil {
				t.Fatal("cannot resolve race fixture")
			}
			fixtureSQL(t, f.writers[0], "DROP TABLE "+qualified)
			if replace {
				fixtureSQL(t, f.writers[0], "CREATE TABLE "+qualified+" (replacement text)")
			}
			err = lockTable(ctx, tx, schema, name, oid)
			want := problem.RelationNotFound
			if replace {
				want = problem.RelationChanged
			}
			checkCode(t, catalogError(ctx, err), want)
			_ = tx.Rollback(ctx)
			closeConn(conn)
			if replace {
				fixtureSQL(t, f.writers[0], "DROP TABLE "+qualified)
			}
		}
	})
	t.Run("lock timeout cancellation and cleanup", func(t *testing.T) {
		blocker, err := f.writers[0].Begin(ctx)
		if err != nil {
			t.Fatal("cannot begin isolated blocker")
		}
		defer blocker.Rollback(ctx)
		if _, err := blocker.Exec(ctx, "LOCK TABLE "+qt+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal("cannot lock isolated fixture relation")
		}
		_, err = f.e.Catalog(ctx, f.targets[0], schema, table)
		checkCode(t, err, problem.RelationChanged)
		canceled, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := f.e.Catalog(canceled, f.targets[0], schema, table); done <- err }()
		deadline := time.Now().Add(3 * time.Second)
		for {
			var waiting bool
			err := f.owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND application_name='databrain-explorer' AND wait_event_type='Lock')`, f.databases[0]).Scan(&waiting)
			if err != nil || time.Now().After(deadline) {
				t.Fatal("catalog never reached the controlled relation lock")
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			checkCode(t, err, problem.RequestTimeout)
		case <-time.After(4 * time.Second):
			t.Fatal("canceled catalog did not finish cleanup")
		}
		_ = blocker.Rollback(ctx)
		// Request berulang juga membuktikan keempat slot Provider dilepas kembali.
		for i := 0; i < 6; i++ {
			read(t, f.targets[0], schema, table)
		}
	})
	t.Run("definition and aggregate limits", func(t *testing.T) {
		large := "'" + strings.Repeat("猫", 22000) + "'"
		for _, definition := range []string{
			"CREATE TABLE public.large_metadata (note text DEFAULT " + large + ")",
			"CREATE TABLE public.large_metadata (note text CHECK (note<>" + large + "))",
			"CREATE TABLE public.large_metadata (note text); CREATE INDEX large_definition ON public.large_metadata ((note || " + large + "))",
		} {
			fixtureSQL(t, f.writers[0], definition)
			result, err := f.e.Catalog(ctx, f.targets[0], "public", "large_metadata")
			checkCode(t, err, problem.ResultTooLarge)
			if !reflect.DeepEqual(result, Catalog{}) {
				t.Fatal("oversized metadata returned partial results")
			}
			fixtureSQL(t, f.writers[0], "DROP TABLE public.large_metadata")
		}
		var definitions []string
		for i := 0; i < 70; i++ {
			definitions = append(definitions, fmt.Sprintf("c%d text DEFAULT '%s'", i, strings.Repeat("x", 61000)))
		}
		fixtureSQL(t, f.writers[0], "CREATE TABLE public.large_metadata ("+strings.Join(definitions, ",")+")")
		_, err := f.e.Catalog(ctx, f.targets[0], "public", "large_metadata")
		checkCode(t, err, problem.ResultTooLarge)
		fixtureSQL(t, f.writers[0], "DROP TABLE public.large_metadata")
	})
	t.Run("no leaked explorer connections", func(t *testing.T) {
		deadline := time.Now().Add(3 * time.Second)
		for {
			var count int
			if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=ANY($1::text[]) AND application_name='databrain-explorer'`, f.databases).Scan(&count); err != nil {
				t.Fatal("cannot inspect test-owned explorer connections")
			}
			if count == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("catalog leaked an explorer connection")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}
