package httpx

import (
	"context"
	"net/http"
)

func NewRouter(ready func(context.Context) bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		healthz(w, r, ready)
	})
	return mux
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
