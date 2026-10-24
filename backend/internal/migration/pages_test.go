package migration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

// These tests render the shipped templates with the handlers' real page structs,
// so a renamed field fails here instead of at request time. No database is needed.
func pageLayout(title, active string, canEdit bool) httpx.Layout {
	return httpx.Layout{
		Title: title, CSRF: "tok", Active: active,
		User:     &httpx.LayoutUser{DisplayName: "Ana", Username: "ana"},
		Database: &httpx.LayoutDatabase{ID: "db1", Name: "prod", CanEdit: canEdit},
	}
}

func renderMigrationPage(t *testing.T, name string, page any) string {
	t.Helper()
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	w := httptest.NewRecorder()
	if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/", nil), 200, name, page); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return strings.ReplaceAll(w.Body.String(), "\r", "") // template files may have CRLF endings on Windows
}

func pageHas(t *testing.T, body string, present bool, parts ...string) {
	t.Helper()
	for _, s := range parts {
		if strings.Contains(body, s) != present {
			t.Errorf("present=%v for %q in:\n%s", present, s, body)
		}
	}
}

func TestDraftPagesRender(t *testing.T) {
	body := renderMigrationPage(t, "drafts", DraftsPage{
		Layout: pageLayout("Drafts", "drafts", true),
		Drafts: []DraftItem{{ID: "d1", Title: "<b>t</b>", Revision: "3", UpdatedBy: "ana", UpdatedAt: "2026-10-24 01:00:00"}},
		Pager:  DraftPager{Next: "/drafts?limit=50&offset=50"},
		Form:   DraftInput{Title: `"><x>`, SQL: "</textarea><script>"},
		Error:  "Check the form fields.",
	})
	pageHas(t, body, true, `href="/databases/db1/drafts/d1"`, "&lt;b&gt;t", `rel="next"`, "Check the form fields.",
		`action="/databases/db1/drafts"`, `name="_csrf" value="tok"`, "&lt;/textarea&gt;&lt;script&gt;")
	pageHas(t, body, false, "<b>t</b>", "<script>", `name="expected_revision"`)

	pageHas(t, renderMigrationPage(t, "drafts", DraftsPage{Layout: pageLayout("Drafts", "drafts", true)}), true, "No drafts yet")

	item := DraftItem{ID: "d1", Title: "t", SQL: "\nSELECT 1; </textarea><script>", Revision: "3", UpdatedBy: "ana", UpdatedAt: "x"}
	body = renderMigrationPage(t, "draft", DraftPage{Layout: pageLayout("t", "drafts", true), Draft: item, CanReview: true})
	pageHas(t, body, true, `action="/databases/db1/drafts/d1"`, `name="expected_revision" value="3"`, `name="_csrf" value="tok"`,
		"required>\n\nSELECT 1;", "&lt;/textarea&gt;&lt;script&gt;", `/drafts/d1/review"`, `href="/databases/db1/drafts">All drafts`)
	pageHas(t, body, false, "<script>", "newer revision")

	body = renderMigrationPage(t, "draft", DraftPage{Layout: pageLayout("t", "drafts", true), Draft: item, Conflict: true})
	pageHas(t, body, true, "newer revision")
	pageHas(t, body, false, `/drafts/d1/review"`) // CanReview false hides the review link
}

func TestReviewPageRender(t *testing.T) {
	draft := DraftItem{ID: "d1", Title: "<i>t</i>", SQL: "SELECT '</pre><script>';", Revision: "3"}
	body := renderMigrationPage(t, "review", ReviewPage{Layout: pageLayout("Review", "drafts", true), Draft: draft, RequestKey: "key-1"})
	pageHas(t, body, true, `action="/databases/db1/drafts/d1/execute"`, `name="expected_revision" value="3"`,
		`name="request_key" value="key-1"`, `name="_csrf" value="tok"`, "&lt;/pre&gt;&lt;script&gt;", "Execute saved revision")
	pageHas(t, body, false, "<script>", "<i>t</i>")

	// An uncertain outcome must never offer another execute, only ways to check the same key.
	body = renderMigrationPage(t, "review", ReviewPage{
		Layout: pageLayout("Check migration request", "drafts", true), Draft: DraftItem{ID: "d1", Revision: "3"}, RequestKey: "key-1",
		Error: "Request timed out.", Code: "REQUEST_TIMEOUT", RequestID: "req-9",
		ReviewHref:  "/databases/db1/drafts/d1/review?expected_revision=3&request_key=key-1",
		ReceiptHref: "/databases/db1/migrations?request_key=key-1",
	})
	pageHas(t, body, true, "Request timed out.", "REQUEST_TIMEOUT", "req-9", `href="/databases/db1/migrations?request_key=key-1"`, "Open the same review")
	pageHas(t, body, false, "/execute", "Execute saved revision")
}

func TestHistoryPagesRender(t *testing.T) {
	item := func(state string, extra func(*AttemptItem)) AttemptItem {
		a := AttemptItem{ID: "a1", Version: "9", Title: "t", Revision: "1", Actor: "ana", State: state,
			SQL: "<script>x</script>", SQLHash: "h", CreatedAt: "c"}
		if extra != nil {
			extra(&a)
		}
		return a
	}
	attemptPage := func(a AttemptItem) string {
		return renderMigrationPage(t, "attempt", AttemptPage{Layout: pageLayout("Version 9", "history", true), Attempt: a})
	}

	body := renderMigrationPage(t, "history", HistoryPage{Layout: pageLayout("History", "history", true), SearchKey: "k"})
	pageHas(t, body, true, `method="get"`, `name="request_key" value="k"`, "has that request key")
	if strings.Count(body, "_csrf") != 1 { // the header's logout form only
		t.Error("GET search form must not carry a CSRF field")
	}
	body = renderMigrationPage(t, "history", HistoryPage{Layout: pageLayout("History", "history", true),
		Attempts: []AttemptItem{item("running", nil), item("unknown", nil)}, Pager: DraftPager{Next: "/n"}})
	pageHas(t, body, true, `href="/databases/db1/migrations/a1"`, `class="badge running"`, `class="badge unknown"`, `rel="next"`)

	body = attemptPage(item("unknown", func(a *AttemptItem) { a.FirstUnknownAt = "f" }))
	pageHas(t, body, true, "stay blocked", "Marked unknown", "&lt;script&gt;")
	pageHas(t, body, false, "/execute", "<script>", "Refresh status")
	if strings.Count(body, "<form") != 1 { // the header's logout form only: no retry control
		t.Error("unknown attempt must not offer a form")
	}

	pageHas(t, attemptPage(item("running", nil)), true, `href="/databases/db1/migrations/a1">Refresh status`)

	body = attemptPage(item("failed", func(a *AttemptItem) {
		a.FailureCode, a.FailureMessage, a.SQLState, a.StatementIndex = "SQL_ERROR", "m", "42P07", 2
	}))
	pageHas(t, body, true, "Nothing was committed", "SQL_ERROR", "42P07", "<dt>Statement</dt><dd>2</dd>")
	pageHas(t, body, false, "Refresh status")

	body = attemptPage(item("succeeded", func(a *AttemptItem) { a.FinishedAt = "done" }))
	pageHas(t, body, true, "committed to the database", "Finished")
	pageHas(t, body, false, "Statement", "Refresh status")
}
