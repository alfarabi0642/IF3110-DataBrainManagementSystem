package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthUnavailable(t *testing.T) {
	w := httptest.NewRecorder()
	NewRouter(nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "unavailable\n" {
		t.Fatalf("health = %d %q; want 503 unavailable", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected health headers: %v", w.Header())
	}
}

func TestRoutesRemainLimited(t *testing.T) {
	mux := NewRouter(nil)
	for _, path := range []string{"/", "/login", "/databases", "/admin/users", "/static/style.css", "/healthz/extra", "/missing"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s = %d; want 404", path, w.Code)
			}
		})
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if w.Code != http.StatusMethodNotAllowed || !strings.Contains(w.Header().Get("Allow"), "GET") {
		t.Fatalf("POST health = %d, Allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

func TestHealthRejectsUnexpectedQuery(t *testing.T) {
	for _, query := range []string{"extra=value", "extra=one&extra=two", "extra=%zz"} {
		w := httptest.NewRecorder()
		NewRouter(nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz?"+query, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unexpected health query accepted: %d", w.Code)
		}
	}
}

func TestHealthUsesCurrentReadiness(t *testing.T) {
	ready := true
	mux := NewRouter(func(ctx context.Context) bool { return ctx.Err() == nil && ready })
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != status || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected health response: %d %v", w.Code, w.Header())
		}
		if status == http.StatusOK && w.Body.String() != "ok\n" {
			t.Fatal("ready response must be ok")
		}
		ready = false
	}
}
