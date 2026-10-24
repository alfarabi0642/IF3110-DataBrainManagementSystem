package migration

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func TestDraftHandlerInput(t *testing.T) {
	values := url.Values{"_csrf": {"synthetic"}, "title": {" 猫 draft "}, "sql": {"\r\nNOT SQL \t"}, "expected_revision": {"3"}}
	in, err := submittedDraft(values, true)
	if err != nil || in.ExpectedRevision != 3 || in.Title != values.Get("title") || in.SQL != values.Get("sql") || draftFormError(in.DraftInput) != "" {
		t.Fatal("form input was normalized or SQL syntax was rejected")
	}
	for _, change := range []func(url.Values){
		func(v url.Values) { v.Set("actor", "forged") },
		func(v url.Values) { v.Set("database_id", "forged") },
		func(v url.Values) { v["title"] = []string{"a", "b"} },
		func(v url.Values) { v.Del("sql") },
		func(v url.Values) { v.Set("expected_revision", "0") },
		func(v url.Values) { v.Set("expected_revision", "1.5") },
	} {
		copy := url.Values{}
		for key, entries := range values {
			copy[key] = append([]string(nil), entries...)
		}
		change(copy)
		_, err := submittedDraft(copy, true)
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	for _, input := range []DraftInput{{" ", "sql"}, {"title", ""}, {"title", strings.Repeat("x", MaxSQLBytes+1)}} {
		if draftFormError(input) == "" {
			t.Fatal("invalid field has no validation message")
		}
	}
	if _, err := NewHandler(nil, nil, nil, nil); err == nil {
		t.Fatal("missing dependencies accepted")
	}
}

func TestDraftHandlerPagination(t *testing.T) {
	for _, query := range []string{"limit=0", "limit=201", "offset=-1", "offset=100001", "limit=1&limit=2", "actor=x", "limit=%zz"} {
		_, _, err := draftPagination(httptest.NewRequest("GET", "/drafts?"+query, nil))
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	limit, offset, err := draftPagination(httptest.NewRequest("GET", "/drafts", nil))
	if err != nil || limit != 50 || offset != 0 {
		t.Fatal("pagination defaults differ")
	}
	if p := draftPager("/drafts", 50, 25, true); p.Prev != "/drafts?limit=50&offset=0" || p.Next != "/drafts?limit=50&offset=75" {
		t.Fatal("pager did not preserve bounds")
	}
	if p := draftPager("/drafts", 200, 99900, true); p.Next != "" {
		t.Fatal("pager linked beyond maximum offset")
	}
}
