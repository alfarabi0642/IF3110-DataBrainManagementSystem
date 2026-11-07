package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func IsAPI(r *http.Request) bool {
	return r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")
}

type Page struct {
	Limit      int  `json:"limit"`
	Offset     int  `json:"offset"`
	HasMore    bool `json:"has_more"`
	NextOffset *int `json:"next_offset"`
	PrevOffset *int `json:"prev_offset"`
}

func NewPage(limit, offset int, more bool) *Page {
	page := &Page{Limit: limit, Offset: offset, HasMore: more}
	if offset > 0 {
		previous := max(0, offset-limit)
		page.PrevOffset = &previous
	}
	if more && offset+limit <= 100000 {
		next := offset + limit
		page.NextOffset = &next
	}
	return page
}

type successEnvelope struct {
	Data      any    `json:"data"`
	Page      *Page  `json:"page,omitempty"`
	RequestID string `json:"request_id"`
}

type publicError struct {
	Code    problem.Code `json:"code"`
	Message string       `json:"message"`
}

type errorEnvelope struct {
	Error     publicError `json:"error"`
	RequestID string      `json:"request_id"`
}

// WriteJSON membatasi response sebelum header ditulis; caller hanya memasok DTO publik.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, data any, page *Page) {
	if status < 200 || status >= 300 {
		WriteJSONError(w, r, &problem.Error{Code: problem.InternalError})
		return
	}
	var body renderBuffer
	if err := json.NewEncoder(&body).Encode(successEnvelope{data, page, RequestID(r.Context())}); err != nil {
		WriteJSONError(w, r, err)
		return
	}
	writeJSONBody(w, r, status, body.buffer.Bytes())
}

func WriteJSONError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := ErrorInfo(err)
	if code == problem.UnsupportedMediaType {
		message = "Use a UTF-8 JSON object."
	}
	body, _ := json.Marshal(errorEnvelope{publicError{code, message}, RequestID(r.Context())})
	writeJSONBody(w, r, status, append(body, '\n'))
}

func writeJSONBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	pageHeaders(w, r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// DecodeJSON menolak field ambigu dan nilai null; validasi domain tetap milik caller.
func DecodeJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any, required, optional []string) error {
	invalid := &problem.Error{Code: problem.InvalidRequest}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return invalid
	}
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || len(params) > 1 ||
		(len(params) == 1 && !strings.EqualFold(params["charset"], "utf-8")) {
		return &problem.Error{Code: problem.UnsupportedMediaType}
	}
	if limit <= 0 || limit > MaxFormBody {
		limit = MaxFormBody
	}
	if r.ContentLength > limit {
		return &problem.Error{Code: problem.BodyTooLarge}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			return &problem.Error{Code: problem.BodyTooLarge}
		}
		return invalid
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := uniqueJSONValue(decoder); err != nil {
		return invalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return invalid
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, name := range append(append([]string{}, required...), optional...) {
		allowed[name] = true
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return invalid
		}
	}
	for name, value := range fields {
		if !allowed[name] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid
		}
	}
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) != nil {
		return invalid
	}
	return nil
}

func uniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return &problem.Error{Code: problem.InvalidRequest}
			}
			keys[name] = true
		}
		if err := uniqueJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// APIHandler menjaga 404/405 dan path tidak canonical tetap JSON tanpa renderer.
func APIHandler(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path.Clean(r.URL.Path) != r.URL.Path {
			WriteJSONError(w, r, &problem.Error{Code: problem.InvalidRequest})
			return
		}
		handler, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &routeProbe{header: make(http.Header)}
		handler.ServeHTTP(probe, r)
		code := problem.RouteNotFound
		if probe.status == http.StatusMethodNotAllowed {
			code = problem.MethodNotAllowed
			w.Header().Set("Allow", probe.header.Get("Allow"))
		}
		WriteJSONError(w, r, &problem.Error{Code: code})
	})
}

type routeProbe struct {
	header http.Header
	status int
}

func (w *routeProbe) Header() http.Header { return w.header }

func (w *routeProbe) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *routeProbe) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(p), nil
}
