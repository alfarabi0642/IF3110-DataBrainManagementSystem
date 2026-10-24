package httpx

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func TestRenderFormErrorEscapesAndOmitsPassword(t *testing.T) {
	renderer := &Renderer{templates: template.Must(template.New("form").Parse(`<form><input name="title" value="{{.Get "title"}}"><input name="password" type="password" value="{{.Get "password"}}"><textarea>{{.Get "sql"}}</textarea></form>`))}
	values := url.Values{"title": {`"><script>alert(1)</script>`}, "sql": {`</textarea><script>alert(2)</script>`}, "password": {"SENSITIVE"}}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	err := renderer.Render(w, r, 422, "form", SafeFormValues(values, "title", "sql", "password"))
	if err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if w.Code != 422 || strings.Contains(body, "<script>") || strings.Contains(body, "SENSITIVE") || !strings.Contains(body, `type="password" value=""`) || !strings.Contains(body, "&lt;/textarea&gt;") {
		t.Fatalf("unsafe form response: %d %s", w.Code, body)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("unexpected page headers: %v", w.Header())
	}
}

func TestRenderFailureWritesNoPartialResponse(t *testing.T) {
	broken := template.Must(template.New("page").Funcs(template.FuncMap{"fail": func() (string, error) { return "", errors.New("SENSITIVE") }}).Parse(`partial {{fail}}`))
	for _, tt := range []struct {
		name      string
		templates *template.Template
		data      any
		code      problem.Code
	}{
		{"execution error", broken, nil, problem.InternalError},
		{"literal overflow", template.Must(template.New("page").Parse(strings.Repeat("a", MaxResponseBytes+1))), nil, problem.ResultTooLarge},
		{"escaping overflow", template.Must(template.New("page").Parse(`{{.}}`)), strings.Repeat("&", MaxResponseBytes/2), problem.ResultTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			err := (&Renderer{templates: tt.templates}).Render(w, r, 200, "page", tt.data)
			requireCode(t, err, tt.code)
			if w.Body.Len() != 0 || len(w.Header()) != 0 {
				t.Fatal("failed render wrote a partial response")
			}
			WriteError(w, r, err)
			if strings.Contains(w.Body.String(), "SENSITIVE") || strings.Contains(w.Body.String(), "partial") {
				t.Fatal("render failure disclosed template/error contents")
			}
		})
	}
	w := httptest.NewRecorder()
	renderer := &Renderer{templates: template.Must(template.New("page").Parse(`{{.}}`))}
	if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/", nil), 200, "page", strings.Repeat("a", MaxResponseBytes)); err != nil || w.Body.Len() != MaxResponseBytes {
		t.Fatalf("exact response boundary rejected: %v", err)
	}
}

func TestErrorInfoRedactsRawMessages(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
	}{
		{&problem.Error{Code: problem.InvalidRequest, Message: "SENSITIVE"}, 400},
		{&problem.Error{Code: problem.CSRFInvalid}, 403},
		{&problem.Error{Code: problem.SessionInvalid}, 401},
		{&problem.Error{Code: problem.OriginRejected}, 403},
		{&problem.Error{Code: problem.AdminRequired}, 403},
		{&problem.Error{Code: problem.DatabaseAccessDenied}, 403},
		{&problem.Error{Code: problem.DatabaseNotFound}, 404},
		{&problem.Error{Code: problem.RevisionConflict}, 409},
		{&problem.Error{Code: problem.TargetBusy, Message: "SENSITIVE"}, 409},
		{&problem.Error{Code: problem.TargetConflict, Message: "SENSITIVE"}, 409},
		{&problem.Error{Code: problem.TargetConnectionFailed, Message: "SENSITIVE"}, 422},
		{&problem.Error{Code: problem.TargetUnavailable, Message: "SENSITIVE"}, 503},
		{&problem.Error{Code: problem.BodyTooLarge}, 413},
		{&problem.Error{Code: problem.UnsupportedMediaType}, 415},
		{&problem.Error{Code: problem.ValidationFailed}, 422},
		{fmt.Errorf("SENSITIVE: %w", &problem.Error{Code: problem.PlatformUnavailable}), 503},
		{context.DeadlineExceeded, 504},
		{&problem.Error{Code: "SENSITIVE", Message: "SENSITIVE"}, 500},
		{errors.New("postgres://SENSITIVE"), 500},
	} {
		status, code, message := ErrorInfo(tt.err)
		if status != tt.status || strings.Contains(string(code)+message, "SENSITIVE") {
			t.Fatalf("unsafe or incorrect error mapping: %d %s %s", status, code, message)
		}
	}
}

func TestWriteErrorRedirectsInvalidSession(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/users", nil)
	WriteError(w, r, fmt.Errorf("wrapped: %w", &problem.Error{Code: problem.SessionInvalid}))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("session redirect = %d %v", w.Code, w.Header())
	}
}

func TestSeeOtherInternalPaths(t *testing.T) {
	for _, location := range []string{"/databases", "/drafts/review?" + url.Values{"schema": {"猫 & public"}}.Encode()} {
		w := httptest.NewRecorder()
		if err := SeeOther(w, httptest.NewRequest(http.MethodPost, "/", nil), location); err != nil {
			t.Fatal(err)
		}
		if w.Code != 303 || w.Header().Get("Location") != location || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("redirect = %d %v", w.Code, w.Header())
		}
	}
	for _, location := range []string{"https://evil.example", "//evil.example", "login", `/\evil.example`, "/%2fevil.example", "/%5cevil.example", "/%0d%0aLocation:evil", "/login#fragment"} {
		w := httptest.NewRecorder()
		err := SeeOther(w, httptest.NewRequest(http.MethodPost, "/", nil), location)
		requireCode(t, err, problem.InternalError)
		if w.Header().Get("Location") != "" || w.Body.Len() != 0 {
			t.Fatalf("unsafe redirect written for %q", location)
		}
	}
}

func TestLoadTemplates(t *testing.T) {
	dir := t.TempDir()
	if renderer, err := LoadTemplates(filepath.Join(dir, "missing")); err != nil || renderer != nil {
		t.Fatalf("absent UI must remain absent: %v", err)
	}
	if _, err := LoadTemplates(dir); err == nil {
		t.Fatal("existing empty template directory accepted")
	}
	pageDir := filepath.Join(dir, "pages")
	if err := os.Mkdir(pageDir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(pageDir, "page.html")
	if err := os.WriteFile(file, []byte(`{{define "page"}}<p>{{.}}</p>{{end}}`), 0600); err != nil {
		t.Fatal(err)
	}
	renderer, err := LoadTemplates(dir)
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	if err := os.WriteFile(file, []byte(`{{`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadTemplates(dir)
	requireCode(t, err, problem.InternalError)
}
