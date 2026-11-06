package httpx

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// SPA mempertahankan route lama sampai consumer React tersedia.
func SPA(mux *http.ServeMux, legacy http.Handler, dir string) (http.Handler, *os.Root, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return legacy, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if _, err := root.Stat("index.html"); err != nil {
		root.Close()
		return nil, nil, err
	}
	assets := http.FileServerFS(root.FS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
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
		_, pattern := mux.Handler(r)
		application := r.URL.Path == "/" || r.URL.Path == "/databases" ||
			strings.HasPrefix(r.URL.Path, "/admin/") || strings.HasPrefix(r.URL.Path, "/databases/")
		migrated := r.URL.Path == "/" || r.URL.Path == "/databases"
		if r.Method == http.MethodGet && application && path.Ext(r.URL.Path) == "" &&
			path.Clean(r.URL.Path) == r.URL.Path && (migrated || pattern == "") {
			pageHeaders(w, r)
			http.ServeFileFS(w, r, root.FS(), "index.html")
			return
		}
		legacy.ServeHTTP(w, r)
	}), root, nil
}
