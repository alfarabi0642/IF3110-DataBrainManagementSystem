package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSPAFallbackBoundaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>SPA entry</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	mux := NewRouter(func(context.Context) bool { return true })
	mux.HandleFunc("GET /admin/users", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("legacy users")) })
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("legacy login")) })
	mux.HandleFunc("GET /databases/{database_id}/explorer", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("legacy explorer")) })
	mux.HandleFunc("GET /databases/{database_id}/drafts", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("legacy drafts")) })
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	handler, root, err := SPA(mux, mux, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, test := range []struct {
		method, path string
		status       int
		spa          bool
	}{
		{"GET", "/", 200, true}, {"GET", "/databases?limit=1", 200, true},
		{"GET", "/login", 200, true}, {"HEAD", "/login", 200, false},
		{"GET", "/admin/unknown", 200, true}, {"GET", "/admin/users", 200, true},
		{"GET", "/databases/target/explorer?schema=public&table=items&offset=2", 200, true},
		{"HEAD", "/databases/target/explorer", 200, false}, {"GET", "/databases/target/drafts", 200, true},
		{"GET", "/static/missing.css", 404, false}, {"GET", "/assets/missing.js", 404, false},
		{"GET", "/databases/missing.js", 404, false}, {"GET", "/missing.svg", 404, false},
		{"GET", "/api/missing", 404, false}, {"GET", "/healthz", 200, false},
		{"GET", "/healthz/missing", 404, false}, {"POST", "/databases", 404, false},
		{"POST", "/admin/unknown", 404, false}, {"POST", "/logout", 204, false},
		{"POST", "/assets/missing.js", 405, false}, {"HEAD", "/databases", 200, false},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status || strings.Contains(w.Body.String(), "SPA entry") != test.spa {
			t.Fatalf("%s %s = %d %s", test.method, test.path, w.Code, w.Body.String())
		}
	}
}

func TestSPAWithoutBuildPreservesLegacy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /databases", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("legacy")) })
	handler, root, err := SPA(mux, mux, filepath.Join(t.TempDir(), "absent"))
	if err != nil || root != nil {
		t.Fatalf("unexpected build dependency: %v", err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/databases", nil))
	if w.Body.String() != "legacy" {
		t.Fatal("legacy route was lost")
	}
}
