package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

// Render setiap contoh dengan template asli agar perubahan field terdeteksi.
func TestSamplePagesRender(t *testing.T) {
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range samplePages() {
		if seen[p.path] {
			t.Errorf("duplicate preview path %s", p.path)
		}
		seen[p.path] = true
		w := httptest.NewRecorder()
		if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, p.path, nil), http.StatusOK, p.name, p.data); err != nil {
			t.Errorf("%s (%s): %v", p.path, p.name, err)
			continue
		}
		if body := w.Body.String(); !strings.Contains(body, "<h1>") || strings.Contains(body, "&lt;no value&gt;") || strings.Contains(body, "<no value>") {
			t.Errorf("%s rendered an empty heading or a missing value", p.path)
		}
	}
}
