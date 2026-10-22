// Package httpx provides the shared HTTP infrastructure.
package httpx

import (
	"net/http"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func NewRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	return mux
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	// A listening HTTP socket is not platform readiness. Replace this with the
	// actual platform/schema/process-lock check when platformdb is implemented.
	err := &problem.Error{Code: problem.PlatformUnavailable, Message: "unavailable"}
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, err.Message, http.StatusServiceUnavailable)
}
