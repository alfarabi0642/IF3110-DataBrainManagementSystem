package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	ServiceAPI    = "databrain-api"
	ServiceWorker = "databrain-worker"

	// ShutdownTimeout membatasi flush/export saat proses berhenti; telemetry tidak boleh menahan
	// shutdown atau lock target.
	ShutdownTimeout = 5 * time.Second
)

// Field schema untuk event JSON. time/level disediakan handler; event berasal dari message log.
const (
	FieldEvent       = "event"
	FieldService     = "service"
	FieldActor       = "actor_id"
	FieldDatabase    = "database_id"
	FieldStatus      = "status"
	FieldRequestID   = "request_id"
	FieldTraceID     = "trace_id"
	FieldSpanID      = "span_id"
	FieldExecutionID = "execution_id"
	FieldJobID       = "job_id"
	FieldAttempt     = "attempt"
	FieldBatch       = "batch"
	FieldDuration    = "duration_ms"
	FieldRows        = "rows"
	FieldSQLState    = "sqlstate"
)

// NewLogger menulis JSON ke stderr. Field time dalam UTC dan message menjadi event agar schema log
// konsisten.
func NewLogger(service string) *slog.Logger {
	return newLogger(os.Stderr, service)
}

func newLogger(w io.Writer, service string) *slog.Logger {
	return newLoggerLevel(w, service, slog.LevelInfo)
}

func newLoggerLevel(w io.Writer, service string, level slog.Level) *slog.Logger {
	if service == "" {
		service = "databrain"
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.MessageKey {
				attr.Key = FieldEvent
			}
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				attr.Value = slog.TimeValue(attr.Value.Time().UTC())
			}
			return attr
		},
	})
	return slog.New(handler).With(FieldService, service)
}

// NewTracerProvider memasang tracer provider global dengan console exporter dan antrean export
// terbatas. Kegagalan inisialisasi harus diperlakukan sebagai observability yang tidak tersedia,
// bukan kegagalan transaksi aplikasi.
func NewTracerProvider(service string, logger *slog.Logger) (*sdktrace.TracerProvider, error) {
	exporter, err := stdouttrace.New(stdouttrace.WithWriter(os.Stderr))
	if err != nil {
		return nil, err
	}
	return newTracerProvider(service, exporter, logger)
}

func newTracerProvider(service string, exporter sdktrace.SpanExporter, logger *slog.Logger) (*sdktrace.TracerProvider, error) {
	if exporter == nil {
		return nil, errors.New("telemetry exporter is nil")
	}
	if service == "" {
		service = "databrain"
	}
	processor := sdktrace.NewBatchSpanProcessor(exporter,
		sdktrace.WithMaxQueueSize(2048),
		sdktrace.WithMaxExportBatchSize(512),
		sdktrace.WithBatchTimeout(5*time.Second),
		sdktrace.WithExportTimeout(ShutdownTimeout),
	)
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", service),
			attribute.String("service.namespace", "databrain"),
		)),
		sdktrace.WithSpanProcessor(processor),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		// Jangan menyalin detail exporter; error dapat memuat endpoint atau payload.
		if logger != nil {
			logger.Error("telemetry export failed", "code", "TELEMETRY_EXPORT_FAILED")
		}
	}))
	return provider, nil
}

// Shutdown melakukan flush dengan batas lima detik. Provider nil adalah no-op.
func Shutdown(provider *sdktrace.TracerProvider) error {
	if provider == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	return provider.Shutdown(ctx)
}
