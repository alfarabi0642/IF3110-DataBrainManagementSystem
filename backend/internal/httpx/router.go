// Package httpx menyediakan infrastruktur HTTP bersama.
package httpx

import (
	"context"
	"net/http"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func NewRouter(ready func(context.Context) bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		healthz(w, r, ready)
	})
	return mux
}

// ErrorPages mempertahankan routing dan header Allow ServeMux, lalu mengganti teks 404/405 bawaannya
// dengan halaman error bersama.
func ErrorPages(renderer *Renderer, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, pattern := mux.Handler(r)
		if pattern != "" || renderer == nil {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &routeProbe{header: make(http.Header)}
		handler.ServeHTTP(probe, r)
		var code problem.Code
		switch probe.status {
		case http.StatusNotFound:
			code = problem.RouteNotFound
		case http.StatusMethodNotAllowed:
			code = problem.MethodNotAllowed
			w.Header().Set("Allow", probe.header.Get("Allow"))
		default:
			mux.ServeHTTP(w, r)
			return
		}
		renderer.RenderError(w, r, Layout{}, &problem.Error{Code: code})
	})
}

type routeProbe struct {
	header http.Header
	status int
}

func (w *routeProbe) Header() http.Header { return w.header }

func (w *routeProbe) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *routeProbe) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(p), nil
}

func healthz(w http.ResponseWriter, r *http.Request, ready func(context.Context) bool) {
	if _, err := ParseQuery(r); err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if ready == nil || !ready(r.Context()) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
