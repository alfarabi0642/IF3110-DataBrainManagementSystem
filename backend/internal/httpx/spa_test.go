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
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log('app');"), 0600); err != nil {
		t.Fatal(err)
	}
	mux := NewRouter(func(context.Context) bool { return true })
	handler, root, err := SPA(mux, dir)
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
		{"GET", "/not-a-real-route", 200, true}, {"HEAD", "/not-a-real-route", 200, false},
		{"GET", "/admin/unknown", 200, true}, {"GET", "/admin/users", 200, true},
		{"GET", "/databases/target/explorer?schema=public&table=items&offset=2", 200, true},
		{"HEAD", "/databases/target/explorer", 200, false}, {"GET", "/databases/target/drafts", 200, true},
		{"GET", "/static/missing.css", 404, false}, {"GET", "/assets/missing.js", 404, false},
		{"GET", "/assets/app.js", 200, false}, {"HEAD", "/assets/app.js", 200, false},
		{"GET", "/assets/../index.html", 404, false}, {"GET", "/assets/", 404, false},
		{"GET", "/assets/%2e%2e/index.html", 404, false},
		{"GET", "/databases/missing.js", 404, false}, {"GET", "/missing.svg", 404, false},
		{"GET", "/api/missing", 404, false}, {"GET", "/healthz", 200, false},
		{"GET", "/api", 404, false}, {"GET", "/assets", 404, false},
		{"GET", "/healthz/missing", 404, false}, {"POST", "/databases", 405, false},
		{"POST", "/admin/unknown", 405, false}, {"POST", "/logout", 404, false},
		{"POST", "/assets/missing.js", 405, false}, {"HEAD", "/databases", 200, false},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status || strings.Contains(w.Body.String(), "SPA entry") != test.spa {
			t.Fatalf("%s %s = %d %s", test.method, test.path, w.Code, w.Body.String())
		}
	}
}

func TestSPARequiresBuild(t *testing.T) {
	mux := http.NewServeMux()
	for _, dir := range []string{filepath.Join(t.TempDir(), "absent"), t.TempDir()} {
		handler, root, err := SPA(mux, dir)
		if err == nil || handler != nil || root != nil {
			t.Fatal("missing SPA build accepted")
		}
	}
}
