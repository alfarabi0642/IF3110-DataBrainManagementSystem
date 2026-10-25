package httpx

import (
	"errors"
	"io/fs"
	"net/http"
	"os"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

// MountStatic memasang asset jika direktori tersedia. Tutup root setelah HTTP shutdown; os.Root juga
// membatasi symlink.
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
