package migration_test

import (
	"context"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func TestValidateBatchRecipePostgres(t *testing.T) {
	f := newDraftFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec := func(sql string) {
		t.Helper()
		if _, err := f.target.Exec(ctx, sql); err != nil {
			t.Fatalf("target batch fixture statement failed: %v", err)
		}
	}
	base := func(table string) sqlpolicy.BatchConfig {
		return sqlpolicy.BatchConfig{Schema: "public", Table: table, KeyColumn: "id", ValueColumn: "value", Delta: 3, BatchSize: 100}
	}
	reject := func(t *testing.T, config sqlpolicy.BatchConfig) {
		t.Helper()
		if _, err := sqlpolicy.ValidateBatchRecipe(ctx, f.target, config); !hasProblemCode(err, problem.SQLUnsupported) {
			t.Fatalf("unsupported recipe was accepted or misclassified: %v", err)
		}
	}

	t.Run("supported recipe", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_ok (id bigint PRIMARY KEY, value bigint NOT NULL, note text)`)
		plan, err := sqlpolicy.ValidateBatchRecipe(ctx, f.target, base("batch_ok"))
		if err != nil {
			t.Fatal(err)
		}
		if plan.UpSQL != `UPDATE "public"."batch_ok" SET "value"="value"+$2 WHERE "id"=ANY($1)` ||
			plan.DownSQL != `UPDATE "public"."batch_ok" SET "value"="value"-$2 WHERE "id"=ANY($1)` {
			t.Fatalf("catalog-validated plan changed its template: %+v", plan)
		}
		if err := sqlpolicy.VerifyBatchPlan(plan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing relation", func(t *testing.T) {
		reject(t, base("batch_missing"))
	})

	t.Run("missing column", func(t *testing.T) {
		config := base("batch_ok")
		config.ValueColumn = "missing"
		reject(t, config)
	})

	t.Run("no primary key", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_no_pk (id bigint, value bigint NOT NULL)`)
		reject(t, base("batch_no_pk"))
	})

	t.Run("composite primary key", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_composite (id bigint, tenant bigint, value bigint NOT NULL,
		    PRIMARY KEY (id, tenant))`)
		reject(t, base("batch_composite"))
	})

	t.Run("nullable value", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_nullable (id bigint PRIMARY KEY, value bigint)`)
		reject(t, base("batch_nullable"))
	})

	t.Run("non-bigint value", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_int4 (id bigint PRIMARY KEY, value integer NOT NULL)`)
		reject(t, base("batch_int4"))
	})

	t.Run("non-bigint key", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_key4 (id integer PRIMARY KEY, value bigint NOT NULL)`)
		reject(t, base("batch_key4"))
	})

	t.Run("view", func(t *testing.T) {
		exec(`CREATE VIEW public.batch_view AS SELECT 1::bigint AS id, 2::bigint AS value`)
		reject(t, base("batch_view"))
	})

	t.Run("partitioned table and partition", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_part (id bigint NOT NULL, value bigint NOT NULL, PRIMARY KEY (id))
		    PARTITION BY RANGE (id)`)
		exec(`CREATE TABLE public.batch_part_1 PARTITION OF public.batch_part FOR VALUES FROM (0) TO (100)`)
		reject(t, base("batch_part"))
		reject(t, base("batch_part_1"))
	})

	t.Run("row level security", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_rls (id bigint PRIMARY KEY, value bigint NOT NULL)`)
		exec(`ALTER TABLE public.batch_rls ENABLE ROW LEVEL SECURITY`)
		reject(t, base("batch_rls"))
	})

	t.Run("trigger", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_trigger (id bigint PRIMARY KEY, value bigint NOT NULL)`)
		exec(`CREATE FUNCTION public.batch_trigger_fn() RETURNS trigger LANGUAGE plpgsql AS
		    $$ BEGIN RETURN NEW; END $$`)
		exec(`CREATE TRIGGER batch_trigger_change BEFORE UPDATE ON public.batch_trigger
		    FOR EACH ROW EXECUTE FUNCTION public.batch_trigger_fn()`)
		reject(t, base("batch_trigger"))
	})

	t.Run("rule", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_rule (id bigint PRIMARY KEY, value bigint NOT NULL)`)
		exec(`CREATE RULE batch_rule_noop AS ON UPDATE TO public.batch_rule DO ALSO NOTHING`)
		reject(t, base("batch_rule"))
	})

	t.Run("generated value", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_generated (id bigint PRIMARY KEY,
		    value bigint GENERATED ALWAYS AS (id + 1) STORED)`)
		reject(t, base("batch_generated"))
	})

	t.Run("identity value", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_identity (id bigint PRIMARY KEY,
		    value bigint GENERATED ALWAYS AS IDENTITY)`)
		reject(t, base("batch_identity"))
	})

	t.Run("unlogged table", func(t *testing.T) {
		exec(`CREATE UNLOGGED TABLE public.batch_unlogged (id bigint PRIMARY KEY, value bigint NOT NULL)`)
		reject(t, base("batch_unlogged"))
	})

	t.Run("inheritance parent", func(t *testing.T) {
		exec(`CREATE TABLE public.batch_parent (id bigint PRIMARY KEY, value bigint NOT NULL)`)
		exec(`CREATE TABLE public.batch_child () INHERITS (public.batch_parent)`)
		reject(t, base("batch_parent"))
	})

	t.Run("domain value", func(t *testing.T) {
		exec(`CREATE DOMAIN public.batch_domain AS bigint`)
		exec(`CREATE TABLE public.batch_domain_t (id bigint PRIMARY KEY, value public.batch_domain NOT NULL)`)
		reject(t, base("batch_domain_t"))
	})
}
