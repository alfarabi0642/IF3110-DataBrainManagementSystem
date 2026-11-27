package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "databrain/runtime"

type logAttrsKey struct{}

// WithLogAttrs menyematkan pasangan atribut korelasi (mis. actor_id, database_id, execution_id,
// job_id, attempt, batch) ke ctx agar QueryTracer menyertakannya secara konsisten pada log SQL.
func WithLogAttrs(ctx context.Context, kv ...any) context.Context {
	if len(kv) < 2 {
		return ctx
	}
	prev := LogAttrs(ctx)
	merged := make([]any, 0, len(prev)+len(kv))
	merged = append(merged, prev...)
	merged = append(merged, kv...)
	return context.WithValue(ctx, logAttrsKey{}, merged)
}

// LogAttrs mengembalikan salinan atribut korelasi yang tersemat pada ctx.
func LogAttrs(ctx context.Context) []any {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(logAttrsKey{}).([]any)
	if len(attrs) == 0 {
		return nil
	}
	out := make([]any, len(attrs))
	copy(out, attrs)
	return out
}

// Tracer mengembalikan tracer bersama untuk span HTTP, antrean River, worker, lock, batch, dan finalizer.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// NewLoggerWithWriter membangun logger JSON terstruktur ke writer tertentu (berguna untuk pengujian
// dan pengambilan sampel log tanpa mengubah schema produksi).
func NewLoggerWithWriter(w io.Writer, service string) *slog.Logger {
	return newLogger(w, service)
}

// NewTracerProviderWithExporter memasang TracerProvider dengan exporter khusus (mis. in-memory
// exporter pada pengujian integrasi trace propagation).
func NewTracerProviderWithExporter(service string, exporter sdktrace.SpanExporter, logger *slog.Logger) (*sdktrace.TracerProvider, error) {
	return newTracerProvider(service, exporter, logger)
}

// InjectTraceContext mengekstrak header W3C traceparent dan tracestate dari ctx aktif agar dapat
// dibawa oleh payload job River melintasi batas proses API -> Worker.
func InjectTraceContext(ctx context.Context) (string, string) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier["traceparent"], carrier["tracestate"]
}

// ExtractTraceContext memasang parent span W3C dari payload job River ke ctx worker.
func ExtractTraceContext(ctx context.Context, traceparent, tracestate string) context.Context {
	if traceparent == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{
		"traceparent": traceparent,
	}
	if tracestate != "" {
		carrier["tracestate"] = tracestate
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// TraceAttrs mengembalikan pasangan atribut slog (trace_id, span_id) jika ctx membawa span aktif.
func TraceAttrs(ctx context.Context) []any {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []any{
		FieldTraceID, sc.TraceID().String(),
		FieldSpanID, sc.SpanID().String(),
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// HTTPMiddleware mengekstrak header W3C TraceContext masuk, membuka span server http.request tanpa
// mencatat path mentah, query string, body, cookie, atau kredensial, dan meneruskan ctx ke handler.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := Tracer().Start(ctx, "http.request",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
			),
		)
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		span.SetAttributes(
			attribute.String("http.route", route),
			attribute.Int("http.status_code", rec.status),
			attribute.Int64(FieldDuration, time.Since(started).Milliseconds()),
		)
		if rec.status >= http.StatusBadRequest {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
	})
}
