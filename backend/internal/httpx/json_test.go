package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func TestDecodeJSON(t *testing.T) {
	for _, test := range []struct {
		name, body, media string
		status            int
		chunked           bool
	}{
		{"valid", `{"title":"Saved","active":false}`, "application/json; charset=UTF-8", 200, false},
		{"invalid", `{"title":`, "application/json", 400, false},
		{"unknown", `{"title":"Saved","active":true,"password":"secret-marker"}`, "application/json", 400, false},
		{"duplicate", `{"title":"Saved","title":"secret-marker","active":true}`, "application/json", 400, false},
		{"escaped duplicate", `{"title":"Saved","\u0074itle":"secret-marker","active":true}`, "application/json", 400, false},
		{"wrong case", `{"Title":"Saved","active":true}`, "application/json", 400, false},
		{"missing", `{"title":"Saved"}`, "application/json", 400, false},
		{"null field", `{"title":null,"active":true}`, "application/json", 400, false},
		{"wrong type", `{"title":"Saved","active":"true"}`, "application/json", 400, false},
		{"null object", `null`, "application/json", 400, false},
		{"array", `[]`, "application/json", 400, false},
		{"trailing", `{"title":"Saved","active":true}{}`, "application/json", 400, false},
		{"invalid UTF8", "{\"title\":\"\xff\",\"active\":true}", "application/json", 400, false},
		{"wrong media", `{}`, "text/plain", 415, false},
		{"wrong charset", `{}`, "application/json; charset=latin1", 415, false},
		{"oversized known", strings.Repeat(" ", 129), "application/json", 413, false},
		{"oversized chunked", strings.Repeat(" ", 129), "application/json", 413, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/example", strings.NewReader(test.body))
			r.Header.Set("Content-Type", test.media)
			if test.chunked {
				r.ContentLength = -1
			}
			w := httptest.NewRecorder()
			var input struct {
				Title  string `json:"title"`
				Active bool   `json:"active"`
			}
			if err := DecodeJSON(w, r, 128, &input, []string{"title", "active"}, nil); err != nil {
				WriteJSONError(w, r, err)
			} else {
				WriteJSON(w, r, 200, struct{}{}, nil)
			}
			if w.Code != test.status || strings.Contains(w.Body.String(), "secret-marker") || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestJSONErrorsAndRecoveryAreSanitized(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{
		{errors.New("postgres://secret-marker"), 500},
		{&problem.Error{Code: problem.ValidationFailed, Message: "secret-marker"}, 422},
		{&problem.Error{Code: problem.SessionInvalid}, 401},
		{&problem.Error{Code: problem.ReauthRequired}, 401},
		{&problem.Error{Code: problem.DatabaseAccessDenied}, 403},
		{&problem.Error{Code: problem.PlatformUnavailable}, 503},
	} {
		var logs bytes.Buffer
		handler := Middleware(slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, r, test.err)
		}))
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/example?password=secret-marker", nil)
		r.Header.Set("Cookie", "databrain_sid=secret-marker")
		handler.ServeHTTP(w, r)
		var body errorEnvelope
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || w.Code != test.status ||
			body.RequestID == "" || body.RequestID != w.Header().Get("X-Request-ID") || w.Header().Get("Location") != "" ||
			strings.Contains(w.Body.String()+logs.String(), "secret-marker") {
			t.Fatalf("unsafe response = %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret-marker")
	})).ServeHTTP(w, httptest.NewRequest("GET", "/api/example", nil))
	if w.Code != 500 || !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), "secret-marker") {
		t.Fatalf("panic response = %d %s", w.Code, w.Body.String())
	}
}

func TestJSONResponseLimitBeforeHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, httptest.NewRequest("GET", "/api/example", nil), 201, strings.Repeat("x", MaxResponseBytes), nil)
	var body errorEnvelope
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || w.Code != 413 || body.Error.Code != problem.ResultTooLarge {
		t.Fatalf("overflow = %d %.100s", w.Code, w.Body.String())
	}
}

func TestAPIRouting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/example", func(w http.ResponseWriter, r *http.Request) { WriteJSON(w, r, 200, struct{}{}, nil) })
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/example", 200}, {"POST", "/api/example", 405},
		{"GET", "/api", 404}, {"GET", "/api/missing", 404},
		{"POST", "/api/missing", 404}, {"GET", "/api//example", 400},
	} {
		w := httptest.NewRecorder()
		APIHandler(mux).ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status || !json.Valid(w.Body.Bytes()) || w.Header().Get("Location") != "" {
			t.Fatalf("%s %s = %d %s", test.method, test.path, w.Code, w.Body.String())
		}
		if test.status == 405 && !strings.Contains(w.Header().Get("Allow"), "GET") {
			t.Fatal("missing Allow header")
		}
	}
}
