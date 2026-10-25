package httpx

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

const (
	NormalBodyLimit int64 = 64 << 10
	LoginBodyLimit  int64 = 8 << 10
	MaxFormBody     int64 = 1 << 20
)

// ParseForm membaca body POST sekali sebelum CSRF. Handler berikutnya memakai ValidateFields pada
// r.PostForm. Request yang sudah diparsing ditolak agar error lama atau batas ukuran sebelumnya tidak
// terabaikan.
func ParseForm(w http.ResponseWriter, r *http.Request, limit int64) (url.Values, error) {
	if r.PostForm != nil || r.Form != nil {
		return nil, &problem.Error{Code: problem.InternalError}
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return nil, &problem.Error{Code: problem.InvalidRequest}
	}
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" ||
		(params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		return nil, &problem.Error{Code: problem.UnsupportedMediaType}
	}
	if limit <= 0 {
		limit = NormalBodyLimit
	}
	if r.ContentLength > limit {
		return nil, &problem.Error{Code: problem.BodyTooLarge}
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, &problem.Error{Code: problem.BodyTooLarge}
		}
		return nil, &problem.Error{Code: problem.InvalidRequest}
	}
	if err := validateScalars(r.PostForm); err != nil {
		return nil, err
	}
	return r.PostForm, nil
}

func ParseQuery(r *http.Request, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, &problem.Error{Code: problem.InvalidRequest}
	}
	if err := ValidateFields(values, nil, allowed); err != nil {
		return nil, err
	}
	return values, nil
}

// ValidateFields memeriksa keberadaan field; handler memeriksa panjang dan maknanya. Izinkan _csrf
// dalam daftar field; validasi format dan perbandingan token tetap milik auth.
func ValidateFields(values url.Values, required, optional []string) error {
	if err := validateScalars(values); err != nil {
		return err
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, name := range optional {
		allowed[name] = true
	}
	for _, name := range required {
		allowed[name] = true
		if _, ok := values[name]; !ok {
			return &problem.Error{Code: problem.InvalidRequest}
		}
	}
	for name := range values {
		if !allowed[name] {
			return &problem.Error{Code: problem.InvalidRequest}
		}
	}
	return nil
}

func validateScalars(values url.Values) error {
	for name, entries := range values {
		if !utf8.ValidString(name) || len(entries) != 1 || !utf8.ValidString(entries[0]) {
			return &problem.Error{Code: problem.InvalidRequest}
		}
	}
	return nil
}

func Int64Value(values url.Values, name string, min, max int64) (int64, error) {
	raw := values.Get(name)
	n, err := strconv.ParseInt(raw, 10, 64)
	if len(values[name]) != 1 || err != nil || n < min || n > max || strings.HasPrefix(raw, "+") {
		return 0, &problem.Error{Code: problem.InvalidRequest}
	}
	return n, nil
}

func BoolValue(values url.Values, name string) (bool, error) {
	if len(values[name]) != 1 {
		return false, &problem.Error{Code: problem.InvalidRequest}
	}
	switch values.Get(name) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, &problem.Error{Code: problem.InvalidRequest}
	}
}

func UUIDValue(values url.Values, name string) (uuid.UUID, error) {
	raw := values.Get(name)
	id, err := uuid.Parse(raw)
	if len(values[name]) != 1 || err != nil || id.String() != raw {
		return uuid.Nil, &problem.Error{Code: problem.InvalidRequest}
	}
	return id, nil
}

// SafeFormValues memilih field yang boleh ditampilkan ulang. Password dan token CSRF kiriman tidak
// disalin; auth memasok token halaman terkini.
func SafeFormValues(values url.Values, names ...string) url.Values {
	safe := make(url.Values)
	for _, name := range names {
		if name == "password" || name == "_csrf" {
			continue
		}
		if entries := values[name]; len(entries) == 1 {
			safe.Set(name, entries[0])
		}
	}
	return safe
}
