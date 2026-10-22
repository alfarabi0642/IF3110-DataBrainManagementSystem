package httpx

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMiddlewareRequestIDsAndRedaction(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /drafts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if RequestID(r.Context()) != w.Header().Get("X-Request-ID") {
			t.Error("handler and response request IDs differ")
		}
		w.Header().Set("Content-Type", "text/html")
		WriteError(w, r, errors.New("postgres://SENSITIVE_DRIVER"))
	})
	handler := Middleware(logger, mux)
	ids := make(map[string]bool)
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/drafts/SENSITIVE_PATH?password=SENSITIVE_QUERY", strings.NewReader("sql=SENSITIVE_BODY"))
		r.Header.Set("Cookie", "databrain_sid=SENSITIVE_COOKIE")
		r.Header.Set("X-Request-ID", "SENSITIVE_HEADER")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		id := w.Header().Get("X-Request-ID")
		if _, err := uuid.Parse(id); err != nil || ids[id] || !strings.Contains(w.Body.String(), id) {
			t.Fatalf("missing/reused request ID: %q", id)
		}
		ids[id] = true
		if w.Code != 500 || strings.Contains(w.Body.String(), "SENSITIVE") || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatal("raw error disclosed")
		}
	}
	if strings.Contains(logs.String(), "SENSITIVE") || !strings.Contains(logs.String(), "POST /drafts/{id}") || !strings.Contains(logs.String(), "status=500") {
		t.Fatalf("unsafe/incomplete access log: %s", logs.String())
	}
}

func TestMiddlewareRecovery(t *testing.T) {
	for _, written := range []bool{false, true} {
		var logs bytes.Buffer
		handler := Middleware(slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if written {
				w.Write([]byte("already sent"))
			}
			panic("SENSITIVE_PANIC")
		}))
		w := httptest.NewRecorder()
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/SENSITIVE_PATH", nil))
		}()
		if written {
			if recovered != http.ErrAbortHandler || w.Body.String() != "already sent" {
				t.Fatal("partial response must abort without an appended error page")
			}
		} else if recovered != nil || w.Code != 500 || !strings.Contains(w.Body.String(), "INTERNAL_ERROR") {
			t.Fatalf("panic was not safely handled: %v, %d", recovered, w.Code)
		}
		if strings.Contains(logs.String()+w.Body.String(), "SENSITIVE") || !strings.Contains(logs.String(), "panicked=true") {
			t.Fatal("panic or URL leaked into logs/response")
		}
	}
}

func TestMiddlewareBodyLimits(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	for _, chunked := range []bool{false, true} {
		handler := Middleware(logger, LimitBody(LoginBodyLimit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := ParseForm(w, r, LoginBodyLimit)
			if err != nil {
				WriteError(w, r, err)
			}
		})))
		r := formRequest("password=" + strings.Repeat("x", int(LoginBodyLimit)))
		if chunked {
			r.ContentLength = -1
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 413 || w.Header().Get("X-Request-ID") == "" {
			t.Fatalf("body limit = %d %v", w.Code, w.Header())
		}
	}
	called := false
	handler := Middleware(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := formRequest(strings.Repeat("x", int(MaxFormBody)+1))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if called || w.Code != 413 {
		t.Fatal("global cap did not reject the oversized request before dispatch")
	}
}
