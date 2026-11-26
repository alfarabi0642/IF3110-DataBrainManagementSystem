package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPGXTracerLogsSanitizedTransactionEvidence(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(newLoggerLevel(&output, "test", slog.LevelDebug))
	t.Cleanup(func() { slog.SetDefault(previous) })
	tracer := QueryTracer{}
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL: "BEGIN; -- SENSITIVE comment\nCOMMIT 'SENSITIVE'", Args: []any{"SENSITIVE"}, // malformed multi-statement is fail-closed
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("UPDATE 1"), Err: nil})
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected start/end logs, got %q", output.String())
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("non-JSON SQL log: %v", err)
		}
		if strings.Contains(line, "SENSITIVE") {
			t.Fatalf("SQL log leaked a literal/argument: %s", line)
		}
		if record[FieldService] != "test" {
			t.Fatalf("service missing: %+v", record)
		}
	}
	// A failed COMMIT must not be reported as confirmed.
	output.Reset()
	ctx = tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "COMMIT"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{Code: "08006", Message: "SENSITIVE"}})
	line := strings.TrimSpace(output.String())
	if strings.Contains(line, "SENSITIVE") || !strings.Contains(line, `"transaction_result":"unknown"`) ||
		!strings.Contains(line, `"sqlstate":"08006"`) || !strings.Contains(line, `"transaction":"commit"`) {
		t.Fatalf("failed commit evidence wrong: %s", line)
	}
}

func TestPGXTracerUsesReadableOperationAndRedactedArgs(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(newLoggerLevel(&output, "test", slog.LevelDebug))
	t.Cleanup(func() { slog.SetDefault(previous) })
	tracer := QueryTracer{}
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL:  "SELECT * FROM databrain.users WHERE username=$1 AND password=$2",
		Args: []any{"alice", "secret"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
	if strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "alice") {
		t.Fatalf("argument values leaked: %s", output.String())
	}
	if !strings.Contains(output.String(), `"operation":"SELECT"`) || !strings.Contains(output.String(), "$1: string") {
		t.Fatalf("operation/argument types missing: %s", output.String())
	}
}
