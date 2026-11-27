package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestLoggerEmitsSchemaJSON(t *testing.T) {
	var output bytes.Buffer
	logger := newLogger(&output, ServiceAPI)
	logger.Info("HTTP request", FieldRequestID, "request", FieldStatus, 200, "route", "/api/databases")
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("log is not one JSON object: %v", err)
	}
	if record[FieldEvent] != "HTTP request" || record[FieldService] != ServiceAPI || record[FieldStatus] != float64(200) ||
		record[FieldRequestID] != "request" || record["route"] != "/api/databases" {
		t.Fatalf("schema fields are wrong: %+v", record)
	}
	if _, ok := record["msg"]; ok {
		t.Fatal("legacy msg key remained")
	}
	if level := record["level"]; level != "INFO" {
		t.Fatalf("level = %v", level)
	}
	if stamp, ok := record["time"].(string); !ok || !strings.HasSuffix(stamp, "Z") {
		t.Fatalf("time is not UTC RFC3339: %v", record["time"])
	}
}

func TestLoggerDefaultsServiceName(t *testing.T) {
	var output bytes.Buffer
	newLogger(&output, "").Info("event")
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil || record[FieldService] != "databrain" {
		t.Fatalf("default service missing: %v %+v", err, record)
	}
}

type blockingShutdownExporter struct{}

func (blockingShutdownExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (blockingShutdownExporter) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestTracerShutdownRespectsDeadline(t *testing.T) {
	provider, err := newTracerProvider("test", blockingShutdownExporter{}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = provider.Shutdown(ctx)
	if err == nil {
		t.Fatal("blocking exporter shutdown did not honor its deadline")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("shutdown exceeded its bound: %s", elapsed)
	}
	if err := Shutdown(nil); err != nil {
		t.Fatalf("nil provider shutdown failed: %v", err)
	}
}
