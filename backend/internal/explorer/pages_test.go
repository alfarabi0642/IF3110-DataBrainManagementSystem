package explorer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

// Render template explorer dengan tipe halaman asli agar perubahan field terdeteksi tanpa database.
func renderExplorerPage(t *testing.T, page ExplorerPage) string {
	t.Helper()
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	page.Layout = httpx.Layout{
		Title: "Explorer", CSRF: "tok", Active: "explorer",
		User:     &httpx.LayoutUser{DisplayName: "Ana", Username: "ana"},
		Database: &httpx.LayoutDatabase{ID: "db1", Name: "prod"},
	}
	w := httptest.NewRecorder()
	if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/", nil), 200, "explorer", page); err != nil {
		t.Fatalf("render explorer: %v", err)
	}
	return strings.ReplaceAll(w.Body.String(), "\r", "") // Template di Windows dapat memakai CRLF.
}

func explorerHas(t *testing.T, body string, present bool, parts ...string) {
	t.Helper()
	for _, s := range parts {
		if strings.Contains(body, s) != present {
			t.Errorf("present=%v for %q in:\n%s", present, s, body)
		}
	}
}

func TestExplorerPageRender(t *testing.T) {
	// Daftar schema, lalu daftar tabel.
	body := renderExplorerPage(t, ExplorerPage{Schemas: []ObjectLink{{Name: "pub<b>", Href: "/x?schema=pub%3Cb%3E"}}})
	explorerHas(t, body, true, `href="/x?schema=pub%3Cb%3E"`, "pub&lt;b&gt;")
	explorerHas(t, body, false, "pub<b>")
	body = renderExplorerPage(t, ExplorerPage{Schema: "public", AllHref: "/a", Tables: []ObjectLink{{Name: "orders", Href: "/t"}}})
	explorerHas(t, body, true, "Tables in public", `href="/t"`, `href="/a"`)
	explorerHas(t, body, false, "Table views")

	tabs := []TabLink{{Label: "Data", Href: "/d", Current: true}, {Label: "Columns", Href: "/c"}}
	page := ExplorerPage{Schema: "public", Table: "t", Tab: "data", AllHref: "/a", SchemaHref: "/s", Tabs: tabs, Data: &DataPage{
		Columns: []string{"a", "b", "c"},
		Rows: [][]Cell{
			{{Null: true}, {Text: "null"}, {Text: ""}},
			{{Text: "<b>x</b>"}, {Text: "9007199254740993"}, {Text: "ok"}},
		},
		Ordering: "Ordered by primary key ascending.", Next: "/n",
	}}
	body = renderExplorerPage(t, page)
	explorerHas(t, body, true, `<span class="null">NULL</span>`, ">null</td>", `class="empty-string"`, "&lt;b&gt;x", "9007199254740993",
		`aria-current="page">Data`, `rel="next"`, "Ordered by primary key ascending.")
	explorerHas(t, body, false, "<b>x</b>", `rel="prev"`)
	if strings.Count(body, `class="null"`) != 1 {
		t.Error("only the real NULL may use the NULL marker")
	}

	// Default kolom berupa *string; nilai tampilan sudah dipetakan.
	def := "'n/a'::text"
	body = renderExplorerPage(t, ExplorerPage{Schema: "public", Table: "t", Tab: "columns", Tabs: tabs, ColumnInfo: []Column{
		{Ordinal: 1, Name: "id", Type: "bigint", Identity: "by default"},
		{Ordinal: 2, Name: "note", Type: "text", Nullable: true, Default: &def, Generated: "stored"},
	}})
	explorerHas(t, body, true, "<td>id</td>", "by default", "<code>&#39;n/a&#39;::text</code>", "stored")

	body = renderExplorerPage(t, ExplorerPage{Schema: "public", Table: "t", Tab: "constraints", Tabs: tabs,
		Constraints: []ConstraintInfo{{Kind: "Primary key", Columns: "id, region", Definition: "PRIMARY KEY (id, region)"}}})
	explorerHas(t, body, true, "Primary key", "id, region", "PRIMARY KEY (id, region)")

	body = renderExplorerPage(t, ExplorerPage{Schema: "public", Table: "t", Tab: "indexes", Tabs: tabs,
		Indexes: []Index{{Name: "orders_pkey", Method: "btree", Unique: true, Primary: true, Valid: true, Definition: "CREATE UNIQUE INDEX ..."}}})
	explorerHas(t, body, true, "orders_pkey", "btree", "CREATE UNIQUE INDEX")
	explorerHas(t, renderExplorerPage(t, ExplorerPage{Schema: "public", Table: "t", Tab: "indexes", Tabs: tabs}), true, "no indexes")
}
