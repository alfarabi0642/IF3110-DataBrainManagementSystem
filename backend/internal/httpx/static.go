package httpx

import (
	"errors"
	"io/fs"
	"net/http"
	"os"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

// MountStatic registers assets only when the directory exists. The caller must
// close the returned root after HTTP shutdown. os.Root also confines symlinks.
func MountStatic(mux *http.ServeMux, dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &problem.Error{Code: problem.InternalError}
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(root.FS())))
	return root, nil
}
