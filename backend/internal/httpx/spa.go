package httpx

import (
	"net/http"
	"os"
	"path"
	"strings"
)

// SPA hanya memberi entry HTML pada browser route; build yang hilang menggagalkan startup.
func SPA(mux *http.ServeMux, dir string) (http.Handler, *os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	if info, err := root.Stat("index.html"); err != nil || !info.Mode().IsRegular() {
		root.Close()
		if err == nil {
			err = os.ErrInvalid
		}
		return nil, nil, err
	}
	assets := http.FileServerFS(root.FS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			if path.Clean(r.URL.Path) != r.URL.Path || strings.Contains(r.URL.Path, "\\") {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
				return
			}
			if info, err := root.Stat(strings.TrimPrefix(r.URL.Path, "/")); err != nil || !info.Mode().IsRegular() {
				http.NotFound(w, r)
				return
			}
			assets.ServeHTTP(w, r)
			return
		}
		application := r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/databases" ||
			strings.HasPrefix(r.URL.Path, "/admin/") || strings.HasPrefix(r.URL.Path, "/databases/")
		reserved := IsAPI(r) || r.URL.Path == "/assets" || r.URL.Path == "/static" ||
			strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/healthz/")
		if !reserved && path.Ext(r.URL.Path) == "" && path.Clean(r.URL.Path) == r.URL.Path && !strings.Contains(r.URL.Path, "\\") {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				pageHeaders(w, r)
				http.ServeFileFS(w, r, root.FS(), "index.html")
				return
			}
			if application {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}), root, nil
}
