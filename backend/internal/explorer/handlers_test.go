package explorer

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestExplorerSelection(t *testing.T) {
	for _, query := range []string{
		"unknown=1", "schema=a&schema=b", "schema=", "table=", "table=x", "schema=%ff", "schema=%zz", "schema=a%00b",
		"schema=" + strings.Repeat("猫", 22), "tab=", "tab=other", "tab=columns", "schema=s&table=t&tab=columns&limit=1",
		"schema=s&table=t&tab=indexes&offset=0", "limit=0", "limit=201", "limit=", "limit=%2B1", "offset=-1", "offset=100001", "offset=",
	} {
		if _, err := parseSelection(httptest.NewRequest("GET", "/?"+query, nil)); err == nil {
			t.Errorf("accepted invalid explorer query %q", query)
		}
	}
	s, err := parseSelection(httptest.NewRequest("GET", "/", nil))
	if err != nil || s.tab != "data" || s.limit != 50 || s.offset != 0 {
		t.Fatal("incorrect explorer defaults")
	}
	s, err = parseSelection(httptest.NewRequest("GET", "/?schema=s&table=t&tab=columns", nil))
	if err != nil || s.tab != "columns" {
		t.Fatal("metadata selection rejected")
	}
	path, schema, table := "/databases/target/explorer", `x &"猫.schema`, `'; DROP TABLE x;--`
	for _, tab := range []string{"data", "columns", "constraints", "indexes"} {
		href := explorerHref(path, schema, table, tab, 2, 0)
		u, err := url.Parse(href)
		if err != nil || u.Path != path || u.Query().Get("schema") != schema || u.Query().Get("table") != table || u.Query().Has("offset") {
			t.Fatal("selection link lost identifier or retained offset")
		}
		if _, err := parseSelection(httptest.NewRequest("GET", href, nil)); err != nil {
			t.Fatal("server generated an invalid tab link")
		}
	}
	if _, err := NewHandler(nil, nil, nil, nil); err == nil {
		t.Fatal("handler accepted missing dependencies")
	}
}
