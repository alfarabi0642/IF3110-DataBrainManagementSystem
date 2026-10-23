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

// LoadTemplates permits an absent UI directory. An existing directory must
// contain valid .html templates; startup must stop on any load/parse error.
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

// Render writes nothing until execution and the escaped response size succeed.
// Form error data must contain only explicitly selected SafeFormValues, never
// the request or the original form. Each feature defines its own page struct.
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

// ErrorInfo maps known codes to fixed public messages. Even a typed error's
// Message is not echoed; raw/wrapped errors and unknown codes become 500.
// Add feature-specific codes here when their handlers are implemented.
func ErrorInfo(err error) (int, problem.Code, string) {
	var p *problem.Error
	if errors.As(err, &p) && p != nil {
		switch p.Code {
		case problem.InvalidRequest:
			return 400, p.Code, "Invalid form or query."
		case problem.BodyTooLarge, problem.ResultTooLarge:
			return 413, p.Code, "The request or result is too large."
		case problem.UnsupportedMediaType:
			return 415, p.Code, "Use a UTF-8 URL-encoded form."
		case problem.ValidationFailed:
			return 422, p.Code, "Check the form fields."
		case problem.RevisionConflict:
			return 409, p.Code, "The resource changed. Open the latest version before saving."
		case problem.CSRFInvalid, problem.OriginRejected:
			return 403, p.Code, "Open a new form and try again."
		case problem.AdminRequired, problem.DatabaseAccessDenied:
			return 403, p.Code, "Access denied."
		case problem.DatabaseNotFound:
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

// LayoutUser is the signed-in identity shown by the shared page header.
type LayoutUser struct {
	DisplayName string
	Username    string
	IsAdmin     bool
}

// Layout holds the fields web/templates/layout.html needs. Page structs embed
// it. User is nil on signed-out pages, which hides navigation and logout.
type Layout struct {
	Title string
	CSRF  string
	User  *LayoutUser
}

type errorPage struct {
	Layout
	Message, Code, RequestID string
}

// RenderError renders the "error" page with the fixed public message from
// ErrorInfo. If rendering fails it falls back to the plain-text WriteError.
func (renderer *Renderer) RenderError(w http.ResponseWriter, r *http.Request, layout Layout, err error) {
	status, code, message := ErrorInfo(err)
	layout.Title = "Error"
	page := errorPage{Layout: layout, Message: message, Code: string(code), RequestID: RequestID(r.Context())}
	if renderer.Render(w, r, status, "error", page) != nil {
		WriteError(w, r, err)
	}
}

// WriteError is a plain-text error response for code that runs before a
// renderer or session exists. It never renders raw error text.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
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

// SeeOther accepts only a server-built internal path with an optional encoded
// query. Never pass a client-supplied return URL or include secrets in it.
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
