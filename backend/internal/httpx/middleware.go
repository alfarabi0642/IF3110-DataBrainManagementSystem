package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

type requestIDKey struct{}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func LimitBody(limit int64, next http.Handler) http.Handler {
	if limit <= 0 {
		limit = NormalBodyLimit
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			WriteError(w, r, &problem.Error{Code: problem.BodyTooLarge})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// Middleware generates its own ID and logs only server route patterns and
// bounded metadata. URLs, query strings, headers, bodies and panic values are
// intentionally excluded. Apply per-route LimitBody before auth parses a form.
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	next = LimitBody(MaxFormBody, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.NewRandom()
		if err != nil {
			WriteError(w, r, &problem.Error{Code: problem.InternalError})
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id.String()))
		w.Header().Set("X-Request-ID", id.String())
		w.Header().Set("Cache-Control", "no-store")
		response := &statusWriter{ResponseWriter: w}
		started := time.Now()
		defer func() {
			panicValue := recover()
			aborted := panicValue != nil && (response.status != 0 || panicValue == http.ErrAbortHandler)
			if panicValue != nil && !aborted {
				WriteError(response, r, &problem.Error{Code: problem.InternalError})
			}
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}
			method := r.Method
			switch method {
			case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT":
			default:
				method = "OTHER"
			}
			logger.Info("HTTP request", "request_id", id.String(), "method", method,
				"route", r.Pattern, "status", status, "duration", time.Since(started), "panicked", panicValue != nil)
			if aborted {
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(response, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
