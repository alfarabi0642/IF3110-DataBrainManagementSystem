package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func formRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/drafts", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	return r
}

func requireCode(t *testing.T, err error, code problem.Code) {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) || p.Code != code {
		t.Fatalf("error = %v; want %s", err, code)
	}
}

func TestParseFormPreservesValues(t *testing.T) {
	sql := " \r\nSELECT '猫; & <script>';\n\t"
	input := url.Values{"title": {"猫"}, "sql": {sql}, "password": {"secret"}, "_csrf": {"token"}}
	r := formRequest(input.Encode())
	values, err := ParseForm(httptest.NewRecorder(), r, NormalBodyLimit)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFields(values, []string{"title", "sql"}, []string{"password", "_csrf"}); err != nil {
		t.Fatal(err)
	}
	if values.Get("sql") != sql || values.Get("title") != "猫" {
		t.Fatal("form values were trimmed or normalized")
	}
	safe := SafeFormValues(values, "title", "sql", "password", "_csrf")
	if _, ok := safe["password"]; ok {
		t.Fatal("password copied for redisplay")
	}
	if _, ok := safe["_csrf"]; ok {
		t.Fatal("submitted token copied for redisplay")
	}
	safe.Set("sql", "changed")
	if r.PostForm.Get("sql") != sql {
		t.Fatal("safe values alias the submitted values")
	}
}

func TestParseFormRejectsInvalidInput(t *testing.T) {
	for _, tt := range []struct {
		name, body, query, media string
		code                     problem.Code
	}{
		{name: "malformed escape", body: "title=%zz", code: problem.InvalidRequest},
		{name: "invalid UTF-8 value", body: "title=%ff", code: problem.InvalidRequest},
		{name: "invalid UTF-8 key", body: "%ff=value", code: problem.InvalidRequest},
		{name: "duplicate field", body: "title=a&title=b", code: problem.InvalidRequest},
		{name: "semicolon encoding", body: "title=a;b", code: problem.InvalidRequest},
		{name: "query injection", body: "title=body", query: "?title=query", code: problem.InvalidRequest},
		{name: "query-only field", query: "?title=query", code: problem.InvalidRequest},
		{name: "empty query delimiter", body: "title=a", query: "?", code: problem.InvalidRequest},
		{name: "JSON", body: "{}", media: "application/json", code: problem.UnsupportedMediaType},
		{name: "multipart", body: "title=a", media: "multipart/form-data; boundary=x", code: problem.UnsupportedMediaType},
		{name: "wrong charset", body: "title=a", media: "application/x-www-form-urlencoded; charset=iso-8859-1", code: problem.UnsupportedMediaType},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := formRequest(tt.body)
			parsedURL, err := url.Parse("/drafts" + tt.query)
			if err != nil {
				t.Fatal(err)
			}
			r.URL = parsedURL
			if tt.media != "" {
				r.Header.Set("Content-Type", tt.media)
			}
			_, err = ParseForm(httptest.NewRecorder(), r, NormalBodyLimit)
			requireCode(t, err, tt.code)
		})
	}
	for _, values := range []url.Values{{"actor": {"forged"}}, {"title": {"one", "two"}}, {"title": {"\xff"}}, {}} {
		requireCode(t, ValidateFields(values, []string{"title"}, nil), problem.InvalidRequest)
	}
	if err := ValidateFields(url.Values{"title": {""}}, []string{"title"}, nil); err != nil {
		t.Fatal("empty and missing fields must remain distinct")
	}
}

func TestFormBodyLimitAndPreviousParsing(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		r := formRequest("title=" + strings.Repeat("x", 20))
		if chunked {
			r.ContentLength = -1
		}
		_, err := ParseForm(httptest.NewRecorder(), r, 10)
		requireCode(t, err, problem.BodyTooLarge)
	}
	r := formRequest("title=ok&sql=%zz")
	_ = r.ParseForm()
	_, err := ParseForm(httptest.NewRecorder(), r, NormalBodyLimit)
	requireCode(t, err, problem.InternalError)
}

func TestQueryAndScalarTypes(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?limit=50&schema=%E7%8C%AB", nil)
	v, err := ParseQuery(r, "limit", "schema")
	if err != nil || v.Get("schema") != "猫" {
		t.Fatalf("query = %v, %v", v, err)
	}
	if n, err := Int64Value(v, "limit", 1, 200); err != nil || n != 50 {
		t.Fatalf("limit = %d, %v", n, err)
	}
	for _, query := range []string{"foreign=x", "limit=1&limit=2", "limit=%zz", "schema=%ff", "%ff=x"} {
		_, err := ParseQuery(httptest.NewRequest(http.MethodGet, "/?"+query, nil), "limit", "schema")
		requireCode(t, err, problem.InvalidRequest)
	}
	for _, value := range []string{"", "0", "201", "+1", "1.5", " 1", "9223372036854775808"} {
		_, err := Int64Value(url.Values{"n": {value}}, "n", 1, 200)
		requireCode(t, err, problem.InvalidRequest)
	}
	for _, value := range []string{"true", "false"} {
		got, err := BoolValue(url.Values{"active": {value}}, "active")
		if err != nil || got != (value == "true") {
			t.Fatalf("boolean = %v, %v", got, err)
		}
	}
	for _, value := range []string{"1", "True", "", "yes"} {
		_, err := BoolValue(url.Values{"active": {value}}, "active")
		requireCode(t, err, problem.InvalidRequest)
	}
	id := "f056dc66-7c58-4afd-a8af-38e1eda90a2d"
	if got, err := UUIDValue(url.Values{"id": {id}}, "id"); err != nil || got.String() != id {
		t.Fatalf("UUID = %v, %v", got, err)
	}
	for _, value := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), "invalid"} {
		_, err := UUIDValue(url.Values{"id": {value}}, "id")
		requireCode(t, err, problem.InvalidRequest)
	}
}
