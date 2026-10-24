package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Page data is a plain map here: templates only name fields, and each handler
// will supply its own struct with the same names.
func renderPage(t *testing.T, name string, data map[string]any) string {
	t.Helper()
	renderer, err := LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	base := map[string]any{
		"Title": "T", "CSRF": "tok", "Active": "",
		"User":     map[string]any{"DisplayName": "Ana", "Username": "ana"},
		"Database": map[string]any{"ID": "db1", "Name": "prod", "CanEdit": true},
	}
	for k, v := range data {
		base[k] = v
	}
	w := httptest.NewRecorder()
	if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/", nil), 200, name, base); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return w.Body.String()
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("missing %q in:\n%s", s, body)
		}
	}
}

func mustNotContain(t *testing.T, body string, bad ...string) {
	t.Helper()
	for _, s := range bad {
		if strings.Contains(body, s) {
			t.Errorf("unexpected %q in:\n%s", s, body)
		}
	}
}

func TestDatabasesPage(t *testing.T) {
	rows := []map[string]any{
		{"ID": "a", "Name": "<i>edit</i>", "Access": "view_edit", "ActiveAttempt": map[string]any{"ID": "x", "Version": "7", "State": "running"}},
		{"ID": "b", "Name": "viewer", "Access": "view_only", "ActiveAttempt": nil},
	}
	body := renderPage(t, "databases", map[string]any{"Databases": rows, "Pager": map[string]any{"Prev": "", "Next": "/databases?limit=50&offset=50"}})
	mustContain(t, body, "&lt;i&gt;edit", `href="/databases/a/drafts"`, `href="/databases/b/explorer"`,
		`href="/databases/a/migrations/x"`, `class="badge running"`, `rel="next"`)
	mustNotContain(t, body, "<i>edit", `href="/databases/b/drafts"`, "rel=\"prev\"")

	body = renderPage(t, "databases", map[string]any{"Databases": nil, "Pager": nil})
	mustContain(t, body, "Ask an admin")
}

func TestExplorerPage(t *testing.T) {
	cell := func(null bool, text string) map[string]any { return map[string]any{"Null": null, "Text": text} }
	data := map[string]any{
		"Schema": "public", "Table": "t", "Tab": "data", "AllHref": "/a", "SchemaHref": "/s",
		"Tabs": []map[string]any{{"Label": "Data", "Href": "/d", "Current": true}, {"Label": "Columns", "Href": "/c", "Current": false}},
		"Data": map[string]any{
			"Columns": []string{"a", "b", "c"},
			"Rows":    [][]map[string]any{{cell(true, ""), cell(false, "null"), cell(false, "")}, {cell(false, "<b>x</b>"), cell(false, "9007199254740993"), cell(false, "ok")}},
			"Prev":    "", "Next": "/n",
		},
	}
	body := renderPage(t, "explorer", data)
	mustContain(t, body, `<span class="null">NULL</span>`, ">null</td>", `class="empty-string"`, "&lt;b&gt;x", "9007199254740993", `aria-current="page">Data`, `rel="next"`)
	mustNotContain(t, body, "<b>x</b>")
	if strings.Count(body, `class="null"`) != 1 {
		t.Error("only the real NULL may use the NULL marker")
	}

	body = renderPage(t, "explorer", map[string]any{"Schema": "public", "Table": "t", "Tab": "indexes", "Tabs": nil, "Indexes": nil, "AllHref": "/a", "SchemaHref": "/s"})
	mustContain(t, body, "no indexes")
	body = renderPage(t, "explorer", map[string]any{"Schemas": []map[string]any{{"Name": "public", "Href": "/x?schema=public"}}})
	mustContain(t, body, `href="/x?schema=public"`)
}

func TestDraftPages(t *testing.T) {
	draft := map[string]any{"ID": "d1", "Title": `"><x>`, "SQL": "\nSELECT 1; </textarea><script>", "Revision": 3, "UpdatedBy": "ana", "UpdatedAt": "t"}
	body := renderPage(t, "draft", map[string]any{"Draft": draft, "Error": "", "Conflict": true, "CanReview": true})
	mustContain(t, body, `name="_csrf" value="tok"`, `name="expected_revision" value="3"`, `action="/databases/db1/drafts/d1"`,
		"&lt;/textarea&gt;&lt;script&gt;", "newer revision", `/drafts/d1/review`)
	// The template newline may be LF or CRLF; the SQL's leading newline must remain.
	const textarea = `<textarea id="sql" name="sql" spellcheck="false" required>`
	if !strings.Contains(body, textarea+"\n\nSELECT 1;") && !strings.Contains(body, textarea+"\r\n\nSELECT 1;") {
		t.Error("SQL textarea did not preserve the leading SQL newline after the template newline")
	}
	mustNotContain(t, body, "<script>", `"><x>`)
	body = renderPage(t, "draft", map[string]any{"Draft": draft, "Error": "", "Conflict": false, "CanReview": false})
	mustNotContain(t, body, `/drafts/d1/review`, `/execute`)

	body = renderPage(t, "drafts", map[string]any{"Drafts": nil, "Pager": nil, "Form": map[string]any{"Title": "", "SQL": ""}, "Error": "Check the form fields."})
	mustContain(t, body, `action="/databases/db1/drafts"`, `name="title"`, "Check the form fields.", "No drafts yet")
	mustNotContain(t, body, `name="expected_revision"`)

	body = renderPage(t, "review", map[string]any{"Draft": draft, "RequestKey": "key-1"})
	mustContain(t, body, `action="/databases/db1/drafts/d1/execute"`, `name="expected_revision" value="3"`, `name="request_key" value="key-1"`, `name="_csrf" value="tok"`)
	mustNotContain(t, body, "<script>")
}

func TestHistoryPages(t *testing.T) {
	body := renderPage(t, "history", map[string]any{"Attempts": nil, "Pager": nil, "SearchKey": "k"})
	mustContain(t, body, `method="get"`, `name="request_key" value="k"`, "has that request key")
	if strings.Count(body, "_csrf") != 1 { // the header's logout form only
		t.Error("GET search form must not carry a CSRF field")
	}

	attempt := func(state string, extra map[string]any) map[string]any {
		a := map[string]any{"ID": "a1", "Version": "9", "Title": "t", "Revision": 1, "Actor": "ana", "State": state,
			"SQL": "<script>x</script>", "SQLHash": "h", "CreatedAt": "c"}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	body = renderPage(t, "attempt", map[string]any{"Attempt": attempt("unknown", map[string]any{"FirstUnknownAt": "f"})})
	mustContain(t, body, "stay blocked", "Marked unknown", "&lt;script&gt;")
	mustNotContain(t, body, "/execute", "<script>", "Refresh status")
	if strings.Count(body, "<form") != 1 { // the header's logout form only: no retry control
		t.Error("unknown attempt must not offer a form")
	}

	body = renderPage(t, "attempt", map[string]any{"Attempt": attempt("running", nil)})
	mustContain(t, body, `href="/databases/db1/migrations/a1">Refresh status`)

	body = renderPage(t, "attempt", map[string]any{"Attempt": attempt("failed", map[string]any{"FailureCode": "SQL_ERROR", "FailureMessage": "m", "SQLState": "42P07", "StatementIndex": 2})})
	mustContain(t, body, "Nothing was committed", "SQL_ERROR", "42P07")
	mustNotContain(t, body, "Refresh status")
}
