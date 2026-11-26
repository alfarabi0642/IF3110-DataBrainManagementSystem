package sqlpolicy

import (
	"errors"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func requireProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var prob *problem.Error
	if !errors.As(err, &prob) {
		t.Fatalf("expected problem.Error %q, got %T (%v)", want, err, err)
	}
	if prob.Code != want {
		t.Fatalf("expected problem code %q, got %q", want, prob.Code)
	}
}

func validBatchConfig() BatchConfig {
	return BatchConfig{Schema: "public", Table: "accounts", KeyColumn: "id", ValueColumn: "balance", Delta: 5, BatchSize: 100}
}

func TestBuildBatchPlan(t *testing.T) {
	config := validBatchConfig()
	plan, err := BuildBatchPlan(config)
	if err != nil {
		t.Fatal(err)
	}
	wantUp := `UPDATE "public"."accounts" SET "balance"="balance"+$2 WHERE "id"=ANY($1)`
	wantDown := `UPDATE "public"."accounts" SET "balance"="balance"-$2 WHERE "id"=ANY($1)`
	if plan.UpSQL != wantUp || plan.DownSQL != wantDown {
		t.Fatalf("generated templates changed: up=%q down=%q", plan.UpSQL, plan.DownSQL)
	}
	if plan.TemplateHash == "" || len(plan.TemplateHash) != 64 {
		t.Fatal("template hash is missing or not SHA-256 hex")
	}
	if err := VerifyBatchPlan(plan); err != nil {
		t.Fatalf("generated plan failed its own verification: %v", err)
	}
	again, err := BuildBatchPlan(config)
	if err != nil || again.TemplateHash != plan.TemplateHash {
		t.Fatal("template hash is not deterministic")
	}
	defaulted, err := BuildBatchPlan(BatchConfig{Schema: "public", Table: "accounts", KeyColumn: "id", ValueColumn: "balance", Delta: 1})
	if err != nil || defaulted.Config.BatchSize != DefaultBatchSize {
		t.Fatalf("default batch size was not applied: %+v, %v", defaulted.Config, err)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*BatchConfig)
	}{
		{"schema", func(c *BatchConfig) { c.Schema = "archive" }},
		{"table", func(c *BatchConfig) { c.Table = "ledger" }},
		{"key", func(c *BatchConfig) { c.KeyColumn = "account_id" }},
		{"value", func(c *BatchConfig) { c.ValueColumn = "amount" }},
		{"delta", func(c *BatchConfig) { c.Delta++ }},
		{"batch size", func(c *BatchConfig) { c.BatchSize++ }},
	} {
		t.Run("hash binds "+tt.name, func(t *testing.T) {
			mutated := config
			tt.mutate(&mutated)
			other, err := BuildBatchPlan(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if other.TemplateHash == plan.TemplateHash {
				t.Fatal("template hash did not bind the config field")
			}
		})
	}
}

func TestBuildBatchPlanRejectsInvalidConfig(t *testing.T) {
	base := validBatchConfig()
	for _, tt := range []struct {
		name   string
		mutate func(*BatchConfig)
	}{
		{"empty schema", func(c *BatchConfig) { c.Schema = "" }},
		{"system schema", func(c *BatchConfig) { c.Schema = "pg_catalog" }},
		{"empty table", func(c *BatchConfig) { c.Table = "" }},
		{"long table", func(c *BatchConfig) { c.Table = strings.Repeat("a", 64) }},
		{"empty key", func(c *BatchConfig) { c.KeyColumn = "" }},
		{"long key", func(c *BatchConfig) { c.KeyColumn = strings.Repeat("a", 64) }},
		{"empty value", func(c *BatchConfig) { c.ValueColumn = "" }},
		{"same key and value", func(c *BatchConfig) { c.ValueColumn = c.KeyColumn }},
		{"zero delta", func(c *BatchConfig) { c.Delta = 0 }},
		{"negative batch size", func(c *BatchConfig) { c.BatchSize = -1 }},
		{"oversized batch", func(c *BatchConfig) { c.BatchSize = MaxBatchSize + 1 }},
		{"invalid UTF-8", func(c *BatchConfig) { c.Table = "\xff" }},
		{"NUL identifier", func(c *BatchConfig) { c.Table = "a\x00" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := base
			tt.mutate(&config)
			_, err := BuildBatchPlan(config)
			requireProblemCode(t, err, problem.ValidationFailed)
		})
	}
}

func TestVerifyBatchPlanRejectsSubstitution(t *testing.T) {
	plan, err := BuildBatchPlan(validBatchConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*BatchPlan)
	}{
		{"up SQL", func(p *BatchPlan) { p.UpSQL += " " }},
		{"down SQL", func(p *BatchPlan) { p.DownSQL += " " }},
		{"template hash", func(p *BatchPlan) { p.TemplateHash = strings.Repeat("0", 64) }},
		{"config delta", func(p *BatchPlan) { p.Config.Delta++ }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mutated := plan
			tt.mutate(&mutated)
			requireProblemCode(t, VerifyBatchPlan(mutated), problem.SQLUnsupported)
		})
	}
}
