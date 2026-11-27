package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type pgxTraceKey struct{}

type pgxTraceState struct {
	span        trace.Span
	started     time.Time
	safe        SanitizedSQL
	args        []string
	operation   string
	transaction string
}

// QueryTracer instruments every platform/target pgx query with sanitized OTel attributes and JSON
// logs. It never records raw SQL, bound values or driver Detail/Hint/Where.
type QueryTracer struct{}

func (QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	safe := SanitizeSQL(data.SQL)
	operation := sqlOperation(safe.Template)
	transaction := sqlTransaction(safe.Template)
	ctx, span := otel.Tracer("databrain/sql").Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient))
	args := RedactArgs(data.Args)
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", operation),
		attribute.String("db.statement", safe.Template),
		attribute.String("db.statement.hash", safe.Hash),
		attribute.Int("db.args.count", len(data.Args)),
	)
	if transaction != "" {
		span.SetAttributes(attribute.String("db.transaction.operation", transaction))
	}
	fields := append(sqlFields(ctx, safe, args, operation, transaction), FieldStatus, "started", "duration_ms", int64(0))
	logger(ctx).InfoContext(ctx, "SQL query started", fields...)
	return context.WithValue(ctx, pgxTraceKey{}, pgxTraceState{span: span, started: time.Now(), safe: safe,
		args: args, operation: operation, transaction: transaction})
}

func (QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	state, ok := ctx.Value(pgxTraceKey{}).(pgxTraceState)
	if !ok {
		return
	}
	safeErr := SanitizeError(data.Err)
	elapsed := time.Since(state.started)
	result := "confirmed"
	status := "ok"
	if state.transaction == "commit" && data.Err != nil {
		result = "unknown"
		status = "error"
	} else if data.Err != nil {
		result = "failed"
		status = "error"
	}
	attrs := []attribute.KeyValue{
		attribute.Int64("db.duration_ms", elapsed.Milliseconds()),
		attribute.Int64("db.rows_affected", data.CommandTag.RowsAffected()),
	}
	if state.transaction != "" {
		attrs = append(attrs, attribute.String("db.transaction.result", result))
	}
	if data.Err != nil {
		attrs = append(attrs, attribute.String("db.sqlstate", safeErr.Code), attribute.String("error.kind", safeErr.Kind))
		state.span.SetStatus(codes.Error, safeErr.Code)
	}
	state.span.SetAttributes(attrs...)
	state.span.End()
	fields := append(sqlFields(ctx, state.safe, state.args, state.operation, state.transaction),
		FieldStatus, status, "duration_ms", elapsed.Milliseconds(), "rows", data.CommandTag.RowsAffected())
	if state.transaction != "" {
		fields = append(fields, "transaction_result", result)
	}
	if data.Err != nil {
		fields = append(fields, "sqlstate", safeErr.Code, "error_kind", safeErr.Kind)
	}
	logger(ctx).InfoContext(ctx, "SQL query completed", fields...)
}

func sqlFields(ctx context.Context, safe SanitizedSQL, args []string, operation, transaction string) []any {
	fields := []any{"operation", operation, "sql_template", safe.Template, "sql_hash", safe.Hash,
		"args", args}
	if transaction != "" {
		fields = append(fields, "transaction", transaction)
	}
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		fields = append(fields, "trace_id", span.SpanContext().TraceID().String(),
			"span_id", span.SpanContext().SpanID().String())
	}
	if extra := LogAttrs(ctx); len(extra) > 0 {
		fields = append(fields, extra...)
	}
	return fields
}

func logger(_ context.Context) *slog.Logger { return slog.Default() }

func sqlOperation(sanitizedSQL string) string {
	fields := strings.Fields(strings.TrimSpace(sanitizedSQL))
	if len(fields) == 0 {
		return "unknown"
	}
	value := strings.ToUpper(strings.Trim(fields[0], ";"))
	if len(value) == 0 || len(value) > 24 {
		return "other"
	}
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return "other"
		}
	}
	return value
}

func sqlTransaction(sanitizedSQL string) string {
	switch sqlOperation(sanitizedSQL) {
	case "BEGIN", "START":
		return "begin"
	case "COMMIT":
		return "commit"
	case "ROLLBACK":
		return "rollback"
	default:
		return ""
	}
}
