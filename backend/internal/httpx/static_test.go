package httpx

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectAssetsRemainAvailable(t *testing.T) {
	mux := NewRouter(nil)
	root, err := MountStatic(mux, "../../web/static")
	if err != nil || root == nil {
		t.Fatal("project assets missing")
	}
	defer root.Close()
	for _, asset := range []struct{ name, contentType string }{
		{"app.js", "javascript"}, {"favicon.svg", "image/svg+xml"}, {"style.css", "text/css"},
	} {
		want, err := os.ReadFile("../../web/static/" + asset.name)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/static/"+asset.name, nil))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) || !strings.Contains(w.Header().Get("Content-Type"), asset.contentType) {
			t.Fatalf("asset %s unavailable or wrong MIME", asset.name)
		}
	}
}

func TestMountStaticConfinesAssets(t *testing.T) {
	base := t.TempDir()
	assets := filepath.Join(base, "static")
	if err := os.Mkdir(assets, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "style.css"), []byte("body { color: black; }"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "secret.txt"), []byte("SENSITIVE"), 0600); err != nil {
		t.Fatal(err)
	}
	mux := NewRouter(nil)
	root, err := MountStatic(mux, assets)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/static/style.css", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "color: black") {
		t.Fatalf("asset not served: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/static/%2e%2e/secret.txt", "/static/../secret.txt", "/static/..%5csecret.txt", "/static/secret.txt"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code == 200 || strings.Contains(w.Body.String(), "SENSITIVE") {
			t.Fatalf("asset root escaped through %q", path)
		}
	}
	missing := NewRouter(nil)
	if root, err := MountStatic(missing, filepath.Join(base, "missing")); err != nil || root != nil {
		t.Fatalf("missing static directory must stay absent: %v", err)
	}
}
