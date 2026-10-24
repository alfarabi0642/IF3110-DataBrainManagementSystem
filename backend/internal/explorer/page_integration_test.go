package explorer

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRowPagePostgres(t *testing.T) {
	f := newCatalogFixture(t)
	ctx := context.Background()
	schema, table := `Rows "猫.schema`, `Order "猫.table`
	qs, qt := pgx.Identifier{schema}.Sanitize(), pgx.Identifier{schema, table}.Sanitize()
	fixtureSQL(t, f.writers[0], "CREATE SCHEMA "+qs+"; CREATE TABLE "+qt+` (
	    "Key b" int,"Key a" int,exact numeric(40,10),big bigint,optional text,empty text,literal text,
	    document json,"binary" bytea,list int[],moment timestamptz,day date,flag boolean,
	    PRIMARY KEY ("Key b","Key a") INCLUDE (exact));
	    INSERT INTO `+qt+` SELECT b,a,123456789012345678901234567890.1234567890,9223372036854775807,
	        NULL,'','null','{"n":12345678901234567890}',decode('00ff','hex'),ARRAY[1,NULL,3],
	        '2026-10-24 17:00:00+07'::timestamptz,'2026-10-24'::date,true
	    FROM (VALUES (10,0),(2,2),(2,1)) v(b,a);
	    CREATE TABLE public.unordered (note text); INSERT INTO public.unordered VALUES (NULL),('null'),(''),('猫');
	    CREATE TABLE public.empty_rows(id int PRIMARY KEY);
	    CREATE TABLE public.no_columns(); INSERT INTO public.no_columns SELECT FROM generate_series(1,3);
	    CREATE TABLE public.events(id int PRIMARY KEY,note text) PARTITION BY RANGE (id);
	    CREATE TABLE public.events_0 PARTITION OF public.events FOR VALUES FROM (0) TO (100);
	    INSERT INTO public.events VALUES (3,'third'),(1,'first');
	    CREATE VIEW public.hidden_view AS SELECT 1 AS id`)
	fixtureSQL(t, f.writers[1], "CREATE SCHEMA "+qs+"; CREATE TABLE "+qt+"(only_second text); INSERT INTO "+qt+" VALUES ('isolated')")
	page := func(t *testing.T, target uuid.UUID, schema, table string, limit, offset int) RowPage {
		t.Helper()
		result, err := f.e.Page(ctx, target, schema, table, limit, offset)
		if err != nil {
			t.Fatalf("row page failed: %v", err)
		}
		return result
	}
	t.Run("composite primary key pagination and exact text", func(t *testing.T) {
		first := page(t, f.targets[0], schema, table, 2, 0)
		if first.Table.Schema != schema || first.Table.Name != table || first.Ordering != "primary_key_asc" ||
			first.Limit != 2 || first.Offset != 0 || !first.HasMore || len(first.Rows) != 2 || len(first.Columns) != 13 ||
			first.Columns[0] != "Key b" || first.Columns[1] != "Key a" || first.Rows[0][1].Text != "1" || first.Rows[1][1].Text != "2" {
			t.Fatal("composite key order, pagination or quoted identifiers incorrect")
		}
		want := []Cell{{Text: "2"}, {Text: "1"}, {Text: "123456789012345678901234567890.1234567890"},
			{Text: "9223372036854775807"}, {Null: true}, {Text: ""}, {Text: "null"},
			{Text: `{"n":12345678901234567890}`}, {Text: `\x00ff`}, {Text: "{1,NULL,3}"},
			{Text: "2026-10-24 10:00:00+00"}, {Text: "2026-10-24"}, {Text: "true"}}
		if !reflect.DeepEqual(first.Rows[0], want) {
			t.Fatal("precision, NULL, empty string or PostgreSQL text encoding changed")
		}
		last := page(t, f.targets[0], schema, table, 2, 2)
		if last.HasMore || len(last.Rows) != 1 || last.Rows[0][0].Text != "10" || last.Offset != 2 {
			t.Fatal("last page or numeric primary key ordering incorrect")
		}
		if end := page(t, f.targets[0], schema, table, 200, 100000); len(end.Rows) != 0 || end.HasMore || len(end.Columns) != 13 {
			t.Fatal("offset beyond data lost metadata or returned rows")
		}
	})
	t.Run("no key empty zero columns and partitioned rows", func(t *testing.T) {
		withoutKey := page(t, f.targets[0], "public", "unordered", 10, 0)
		if withoutKey.Ordering != "unspecified" || withoutKey.HasMore || len(withoutKey.Rows) != 4 {
			t.Fatal("keyless table order or row count incorrect")
		}
		seen := make(map[Cell]bool)
		for _, row := range withoutKey.Rows {
			seen[row[0]] = true
		}
		if len(seen) != 4 || !seen[Cell{Null: true}] || !seen[Cell{Text: "null"}] || !seen[Cell{Text: ""}] || !seen[Cell{Text: "猫"}] {
			t.Fatal("keyless text or NULL values were merged")
		}
		if empty := page(t, f.targets[0], "public", "empty_rows", 50, 0); len(empty.Rows) != 0 || empty.HasMore || !reflect.DeepEqual(empty.Columns, []string{"id"}) {
			t.Fatal("empty table page incorrect")
		}
		if zero := page(t, f.targets[0], "public", "no_columns", 1, 1); len(zero.Rows) != 1 || len(zero.Rows[0]) != 0 || !zero.HasMore {
			t.Fatal("zero-column table rows were lost")
		}
		if partitioned := page(t, f.targets[0], "public", "events", 1, 0); partitioned.Table.Kind != "p" || !partitioned.HasMore || partitioned.Rows[0][0].Text != "1" {
			t.Fatal("partitioned row page incorrect")
		}
	})
	t.Run("target scope and missing relations", func(t *testing.T) {
		second := page(t, f.targets[1], schema, table, 10, 0)
		if !reflect.DeepEqual(second.Columns, []string{"only_second"}) || len(second.Rows) != 1 || second.Rows[0][0].Text != "isolated" {
			t.Fatal("row data crossed target boundaries")
		}
		for _, selection := range [][2]string{{"absent", "table"}, {schema, "absent"}, {"public", "hidden_view"}, {schema, `'; DROP TABLE anything;--`}} {
			_, err := f.e.Page(ctx, f.targets[0], selection[0], selection[1], 50, 0)
			checkCode(t, err, problem.RelationNotFound)
		}
		_, err := f.e.Page(ctx, uuid.New(), schema, table, 50, 0)
		checkCode(t, err, problem.DatabaseNotFound)
	})
	t.Run("shared transaction rejects writes", func(t *testing.T) {
		err := f.e.read(ctx, f.targets[0], func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE "+qt+" SET optional='must not write'")
			pgErr, ok := err.(*pgconn.PgError)
			if !ok || pgErr.Code != "25006" {
				t.Error("explorer transaction did not reject mutation")
			}
			return err
		})
		checkCode(t, err, problem.TargetUnavailable)
	})
	t.Run("cell and raw page bounds before transfer", func(t *testing.T) {
		fixtureSQL(t, f.writers[0], `CREATE TABLE public.page_limits(id int PRIMARY KEY,note text);
		    INSERT INTO public.page_limits VALUES (1,repeat('猫',21845)),(2,repeat('猫',21846))`)
		first := page(t, f.targets[0], "public", "page_limits", 1, 0)
		if !first.HasMore || len(first.Rows[0][1].Text) != 65535 {
			t.Fatal("lookahead cell was transferred or rejected the visible page")
		}
		result, err := f.e.Page(ctx, f.targets[0], "public", "page_limits", 1, 1)
		checkCode(t, err, problem.CellTooLarge)
		if !reflect.DeepEqual(result, RowPage{}) {
			t.Fatal("oversized cell returned a partial page")
		}
		fixtureSQL(t, f.writers[0], "UPDATE public.page_limits SET note=repeat('x',65536) WHERE id=1")
		if exact := page(t, f.targets[0], "public", "page_limits", 1, 0); len(exact.Rows[0][1].Text) != maxCellBytes {
			t.Fatal("exact cell byte limit was rejected")
		}
		fixtureSQL(t, f.writers[0], `TRUNCATE public.page_limits;
		    INSERT INTO public.page_limits SELECT i,repeat('x',65535) FROM generate_series(1,8) i`)
		if exact := page(t, f.targets[0], "public", "page_limits", 8, 0); len(exact.Rows) != 8 || exact.HasMore {
			t.Fatal("exact 512 KiB page was rejected")
		}
		fixtureSQL(t, f.writers[0], "UPDATE public.page_limits SET note=repeat('x',65536) WHERE id=1")
		result, err = f.e.Page(ctx, f.targets[0], "public", "page_limits", 8, 0)
		checkCode(t, err, problem.ResultTooLarge)
		if !reflect.DeepEqual(result, RowPage{}) {
			t.Fatal("oversized raw page was truncated or partly returned")
		}
		if smaller := page(t, f.targets[0], "public", "page_limits", 4, 0); len(smaller.Rows) != 4 || !smaller.HasMore {
			t.Fatal("smaller bounded page did not recover")
		}
	})
	t.Run("conversion failure timeout and cancellation", func(t *testing.T) {
		fixtureSQL(t, f.writers[0], `CREATE TYPE public.page_value AS ENUM ('reject','slow');
		    CREATE FUNCTION public.page_text(public.page_value) RETURNS text LANGUAGE plpgsql AS $$ BEGIN
		        IF $1='reject'::public.page_value THEN RAISE EXCEPTION USING ERRCODE='22000',MESSAGE='SENSITIVE'; END IF;
		        PERFORM pg_sleep(10); RETURN 'value'; END $$;
		    CREATE CAST (public.page_value AS text) WITH FUNCTION public.page_text(public.page_value);
		    CREATE TABLE public.conversion(value public.page_value); INSERT INTO public.conversion VALUES ('reject')`)
		_, err := f.e.Page(ctx, f.targets[0], "public", "conversion", 1, 0)
		checkCode(t, err, problem.UnsupportedColumnType)
		fixtureSQL(t, f.writers[0], "UPDATE public.conversion SET value='slow'")
		_, err = f.e.Page(ctx, f.targets[0], "public", "conversion", 1, 0)
		checkCode(t, err, problem.RequestTimeout)
		canceled, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := f.e.Page(canceled, f.targets[0], "public", "conversion", 1, 0); done <- err }()
		deadline := time.Now().Add(2 * time.Second)
		for {
			var waiting bool
			err := f.owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND application_name='databrain-explorer' AND wait_event='PgSleep')`, f.databases[0]).Scan(&waiting)
			if err != nil || time.Now().After(deadline) {
				t.Fatal("row page never reached the controlled output conversion")
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
			t.Fatal("canceled page did not finish cleanup")
		}
	})
	t.Run("connection cleanup and capacity reuse", func(t *testing.T) {
		for i := 0; i < 6; i++ {
			page(t, f.targets[0], schema, table, 2, 0)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			var count int
			if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=ANY($1::text[]) AND application_name='databrain-explorer'`, f.databases).Scan(&count); err != nil {
				t.Fatal("cannot inspect test-owned page connections")
			}
			if count == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("row page leaked a target connection")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}
