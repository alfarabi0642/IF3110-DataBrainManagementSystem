package httpx

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

const MaxResponseBytes = 4 << 20

type Renderer struct {
	templates *template.Template
}

// HasTemplate memeriksa ketersediaan template sebelum route dipasang.
func (renderer *Renderer) HasTemplate(name string) bool {
	return renderer != nil && renderer.templates != nil && renderer.templates.Lookup(name) != nil
}

// LoadTemplates menerima direktori UI yang belum ada. Jika ada, seluruh template .html harus valid;
// error baca/parse menghentikan startup.
func LoadTemplates(dir string) (*Renderer, error) {
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.IsDir() {
		return nil, &problem.Error{Code: problem.InternalError}
	}
	var files []string
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".html" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		return nil, &problem.Error{Code: problem.InternalError}
	}
	templates, err := template.ParseFiles(files...)
	if err != nil {
		return nil, &problem.Error{Code: problem.InternalError}
	}
	return &Renderer{templates: templates}, nil
}

var errResultTooLarge = &problem.Error{Code: problem.ResultTooLarge}

type renderBuffer struct {
	buffer bytes.Buffer
}

func (b *renderBuffer) Write(p []byte) (int, error) {
	if len(p) > MaxResponseBytes-b.buffer.Len() {
		return 0, errResultTooLarge
	}
	return b.buffer.Write(p)
}

// Render baru menulis response setelah template dan batas ukuran HTML lolos. Data error form hanya
// boleh memakai SafeFormValues terpilih, tanpa request atau form asli.
func (renderer *Renderer) Render(w http.ResponseWriter, r *http.Request, status int, name string, data any) error {
	if renderer == nil || renderer.templates == nil || status < 200 || status > 599 {
		return &problem.Error{Code: problem.InternalError}
	}
	var body renderBuffer
	if err := renderer.templates.ExecuteTemplate(&body, name, data); err != nil {
		if errors.Is(err, errResultTooLarge) {
			return errResultTooLarge
		}
		return &problem.Error{Code: problem.InternalError}
	}
	pageHeaders(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(body.buffer.Bytes())
	return err
}

// ErrorInfo memetakan kode ke pesan publik tetap. Message dari error tidak ditampilkan; error mentah,
// terbungkus, atau kode asing menjadi 500.
func ErrorInfo(err error) (int, problem.Code, string) {
	var p *problem.Error
	if errors.As(err, &p) && p != nil {
		switch p.Code {
		case problem.InvalidRequest:
			return 400, p.Code, "Invalid form or query."
		case problem.RouteNotFound:
			return 404, p.Code, "Page not found."
		case problem.MethodNotAllowed:
			return 405, p.Code, "Method not allowed."
		case problem.BodyTooLarge, problem.ResultTooLarge:
			return 413, p.Code, "The request or result is too large."
		case problem.UnsupportedMediaType:
			return 415, p.Code, "Use a UTF-8 URL-encoded form."
		case problem.ValidationFailed:
			return 422, p.Code, "Check the form fields."
		case problem.RevisionConflict:
			return 409, p.Code, "The resource changed. Open the latest version before saving."
		case problem.AlreadyAuthenticated:
			return 409, p.Code, "Log out before signing in as another account."
		case problem.UsernameTaken:
			return 409, p.Code, "That username is already in use."
		case problem.GroupNameTaken:
			return 409, p.Code, "That group name is already in use."
		case problem.PlatformLimitReached:
			return 409, p.Code, "The platform account or group limit has been reached."
		case problem.LastAdmin:
			return 409, p.Code, "At least one active platform administrator is required."
		case problem.IdempotencyConflict:
			return 409, p.Code, "This request key belongs to a different draft or revision."
		case problem.DraftAlreadyExecuted:
			return 409, p.Code, "This draft revision already succeeded. Save a new revision before another execution."
		case problem.ExecutionCapacity:
			return 409, p.Code, "Migration execution capacity is full. Check the receipt before trying again."
		case problem.SQLInvalid:
			return 422, p.Code, "The saved SQL is empty or has invalid syntax."
		case problem.SQLUnsupported:
			return 422, p.Code, "The saved SQL contains an unsupported migration operation."
		case problem.TargetBusy:
			return 409, p.Code, "This database has an unresolved migration. Check its history before making changes."
		case problem.TargetConflict:
			return 409, p.Code, "A connection for this database already exists."
		case problem.TargetConnectionFailed:
			return 422, p.Code, "The target connection could not be verified. Check its settings."
		case problem.TargetUnavailable:
			return 503, p.Code, "Target unavailable."
		case problem.RelationNotFound:
			return 404, p.Code, "Schema or table not available."
		case problem.RelationChanged:
			return 409, p.Code, "The table changed or is busy. Open it again."
		case problem.CellTooLarge:
			return 422, p.Code, "A cell exceeds the explorer size limit."
		case problem.UnsupportedColumnType:
			return 422, p.Code, "A column cannot be displayed as text."
		case problem.SessionInvalid:
			return 401, p.Code, "Authentication required."
		case problem.AuthBusy:
			return 503, p.Code, "Authentication is busy. Try again."
		case problem.CSRFInvalid, problem.OriginRejected:
			return 403, p.Code, "Open a new form and try again."
		case problem.AdminRequired, problem.DatabaseAccessDenied:
			return 403, p.Code, "Access denied."
		case problem.DatabaseNotFound, problem.DraftNotFound, problem.AttemptNotFound,
			problem.UserNotFound, problem.GroupNotFound:
			return 404, p.Code, "Resource not available."
		case problem.PlatformUnavailable:
			return 503, p.Code, "Platform unavailable."
		case problem.RequestTimeout:
			return 504, p.Code, "Request timed out."
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 504, problem.RequestTimeout, "Request timed out."
	}
	return 500, problem.InternalError, "Internal server error."
}

// LayoutUser berisi identitas login untuk header bersama.
type LayoutUser struct {
	DisplayName string
	Username    string
	IsAdmin     bool
}

// LayoutDatabase berisi database halaman; CanEdit menentukan tampilan link Drafts.
type LayoutDatabase struct {
	ID      string
	Name    string
	CanEdit bool
}

// Layout disisipkan pada data halaman. User nil menyembunyikan navigasi login/logout; Database nil
// dipakai di luar halaman database. Active memilih item navigasi aktif.
type Layout struct {
	Title    string
	CSRF     string
	Active   string
	User     *LayoutUser
	Database *LayoutDatabase
}

type errorPage struct {
	Layout
	Message, Code, RequestID string
}

// RenderError menampilkan pesan tetap dari ErrorInfo. Jika render gagal, gunakan WriteError teks
// biasa.
func (renderer *Renderer) RenderError(w http.ResponseWriter, r *http.Request, layout Layout, err error) {
	status, code, message := ErrorInfo(err)
	layout.Title = "Error"
	page := errorPage{Layout: layout, Message: message, Code: string(code), RequestID: RequestID(r.Context())}
	if renderer.Render(w, r, status, "error", page) != nil {
		WriteError(w, r, err)
	}
}

// WriteError dipakai sebelum renderer atau session tersedia dan tidak menampilkan error mentah.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var p *problem.Error
	if errors.As(err, &p) && p != nil && p.Code == problem.SessionInvalid {
		_ = SeeOther(w, r, "/login")
		return
	}
	status, code, message := ErrorInfo(err)
	pageHeaders(w, r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(w, string(code)+": "+message+"\nRequest ID: "+RequestID(r.Context()), status)
}

func pageHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if id := RequestID(r.Context()); id != "" {
		w.Header().Set("X-Request-ID", id)
	}
}

// SeeOther hanya menerima path internal buatan server dengan query ter-encode. Jangan memakai return
// URL dari client atau memasukkan rahasia.
func SeeOther(w http.ResponseWriter, r *http.Request, location string) error {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") ||
		u.Fragment != "" || strings.ContainsAny(location, "\\\r\n") ||
		strings.ContainsRune(u.Path, '\\') || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return &problem.Error{Code: problem.InternalError}
	}
	pageHeaders(w, r)
	http.Redirect(w, r, location, http.StatusSeeOther)
	return nil
}
