// Command preview serves the real templates with sample data so pages can be
// viewed before their handlers exist. Run from backend/: go run ./cmd/preview
// Delete this directory once real handlers render these pages.
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

const (
	dbID      = "3f2b8e64-0c1a-4c57-9b0e-5d8f1a2b3c4d"
	draftID   = "7a1d52c0-6b0e-4f0d-8c0b-2f6f9e0a1b11"
	attemptID = "c2d9a4e8-1f53-4a7b-9e0c-6d3b8f2a4e55"
	userID    = "5b7e2f10-3c4d-4e5f-8a6b-7c8d9e0f1a2b"
	groupID   = "0d1e2f30-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
	sampleSQL = "CREATE TABLE orders (\n  id bigint PRIMARY KEY,\n  note text\n);\n-- <b>not html</b>\nINSERT INTO orders VALUES (1, 'a;b');"
	hash      = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
)

type page struct {
	path, name string
	data       map[string]any
}

func layout(title, active string, db bool, edit bool) map[string]any {
	data := map[string]any{
		"Title": title, "CSRF": "preview-csrf-token", "Active": active,
		"User": map[string]any{"DisplayName": "Ana Editor", "Username": "ana", "IsAdmin": false},
	}
	if db {
		data["Database"] = map[string]any{"ID": dbID, "Name": "orders-prod", "CanEdit": edit}
	}
	return data
}

func with(data map[string]any, extra map[string]any) map[string]any {
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func cell(text string) map[string]any { return map[string]any{"Null": false, "Text": text} }

func attempt(state string, extra map[string]any) map[string]any {
	a := map[string]any{
		"ID": attemptID, "Version": "42", "Title": "Add orders table", "Revision": 3, "Actor": "ana",
		"State": state, "SQL": sampleSQL, "SQLHash": hash, "CreatedAt": "2026-10-23 07:41:02",
	}
	return with(a, extra)
}

// admin switches the sample page to an admin user so the admin navigation shows.
func admin(data, extra map[string]any) map[string]any {
	data["User"] = map[string]any{"DisplayName": "Root Admin", "Username": "root", "IsAdmin": true}
	return with(data, extra)
}

// connection builds the admin_database sample. A nil probe means none was run.
func connection(probe map[string]any, conflict bool) map[string]any {
	data := map[string]any{
		"Target": map[string]any{
			"ID": dbID, "Name": "orders-prod", "Host": "db.internal", "Port": 5432, "DatabaseName": "orders", "Username": "databrain_ro",
			"SSLMode": "verify-full", "IsActive": true, "PasswordStored": true, "ConfigRevision": 7,
			"UpdatedAt": time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC),
		},
		"Form":  map[string]any{"Name": "orders-prod", "Username": "databrain_ro", "SSLMode": "verify-full", "IsActive": true, "ExpectedRevision": "7"},
		"Error": "", "Code": "", "Conflict": conflict,
	}
	if probe != nil {
		data["Probe"] = probe
	}
	if conflict {
		data["Form"] = map[string]any{"Name": "my new name", "Username": "databrain_ro", "SSLMode": "disable", "IsActive": false, "ExpectedRevision": "3"}
	}
	return data
}

// login builds a login sample: no signed-in user, no password field in the data.
func login(extra map[string]any) map[string]any {
	return with(map[string]any{"Title": "Log in", "CSRF": "preview-csrf-token", "Active": "", "Username": "", "Error": "", "Reauth": false}, extra)
}

// userProfile builds the admin_user sample. A conflict keeps the user's unsaved input.
func userProfile(conflict bool) map[string]any {
	form := map[string]any{"DisplayName": "Ana Editor", "IsActive": true, "IsAdmin": false, "ExpectedRevision": "4"}
	if conflict {
		form = map[string]any{"DisplayName": "My new name", "IsActive": false, "IsAdmin": true, "ExpectedRevision": "3"}
	}
	return admin(layout("User", "users", false, false), map[string]any{
		"UserProfile": map[string]any{"ID": userID, "Username": "ana", "DisplayName": "Ana Editor", "IsActive": true, "IsAdmin": false, "Revision": 4},
		"Groups": []map[string]any{
			{"ID": groupID, "Name": "Analysts", "IsActive": true},
			{"ID": "99999999-8888-4777-8666-555555555555", "Name": "Retired team", "IsActive": false},
		},
		"Form": form, "Error": "", "Conflict": conflict,
	})
}

// groupProfile builds the admin_group sample. A conflict keeps the user's unsaved input.
func groupProfile(conflict bool) map[string]any {
	form := map[string]any{"Name": "Analysts", "IsActive": true, "ExpectedRevision": "2"}
	if conflict {
		form = map[string]any{"Name": "Renamed team", "IsActive": false, "ExpectedRevision": "1"}
	}
	member := func(id, username, name string, active bool) map[string]any {
		return map[string]any{"ID": id, "Username": username, "DisplayName": name, "IsActive": active}
	}
	return admin(layout("Group", "groups", false, false), map[string]any{
		"GroupProfile":  map[string]any{"ID": groupID, "Name": "Analysts", "IsActive": true, "Revision": 2},
		"Form":          form,
		"Members":       []map[string]any{member(userID, "ana", "Ana Editor", true), member("22222222-3333-4444-8555-666666666666", "budi", "Budi", false)},
		"MemberChoices": []map[string]any{member("33333333-4444-4555-8666-777777777777", "citra", "Citra", true)},
		"Grants": []map[string]any{
			{"DatabaseID": dbID, "DatabaseName": "orders-prod", "Access": "view_edit"},
			{"DatabaseID": "11111111-2222-4333-8444-555555555555", "DatabaseName": "analytics", "Access": "view_only"},
		},
		"DatabaseChoices": []map[string]any{{"ID": dbID, "Name": "orders-prod"}, {"ID": "11111111-2222-4333-8444-555555555555", "Name": "analytics"}},
		"Error":           "", "Conflict": conflict,
	})
}

// samplePages lists every preview page with the data its template reads.
func samplePages() []page {
	pager := map[string]any{"Prev": "", "Next": "/databases?limit=50&offset=50"}
	explorer := func(extra map[string]any) map[string]any {
		return with(layout("Explorer", "explorer", true, true), with(map[string]any{
			"AllHref": "/preview/explorer", "SchemaHref": "/preview/explorer-tables",
		}, extra))
	}
	tabs := func(current string) []map[string]any {
		var out []map[string]any
		for _, t := range [][2]string{{"data", "Data"}, {"columns", "Columns"}, {"constraints", "Constraints"}, {"indexes", "Indexes"}} {
			out = append(out, map[string]any{"Label": t[1], "Href": "/preview/explorer-" + t[0], "Current": t[0] == current})
		}
		return out
	}
	table := func(tab string, extra map[string]any) map[string]any {
		return explorer(with(map[string]any{"Schema": "public", "Table": "orders", "Tab": tab, "Tabs": tabs(tab)}, extra))
	}
	historyRow := func(version, state string) map[string]any {
		return map[string]any{"ID": attemptID, "Version": version, "Title": "Add orders table", "Revision": 3,
			"Actor": "ana", "CreatedAt": "2026-10-23 07:41:02", "State": state}
	}

	return []page{
		{"/preview/databases", "databases", with(layout("Databases", "databases", false, false), map[string]any{
			"Databases": []map[string]any{
				{"ID": dbID, "Name": "orders-prod", "Access": "view_edit", "ActiveAttempt": map[string]any{"ID": attemptID, "Version": "42", "State": "running"}},
				{"ID": "11111111-2222-4333-8444-555555555555", "Name": "analytics", "Access": "view_only", "ActiveAttempt": nil},
			},
			"Pager": pager,
		})},
		{"/preview/databases-empty", "databases", with(layout("Databases", "databases", false, false), map[string]any{"Databases": nil, "Pager": nil})},
		{"/preview/explorer", "explorer", explorer(map[string]any{"Schemas": []map[string]any{{"Name": "public", "Href": "/preview/explorer-tables"}, {"Name": "billing", "Href": "/preview/explorer-tables"}}})},
		{"/preview/explorer-tables", "explorer", explorer(map[string]any{"Schema": "public", "Tables": []map[string]any{{"Name": "orders", "Href": "/preview/explorer-data"}, {"Name": "customers", "Href": "/preview/explorer-data"}}})},
		{"/preview/explorer-data", "explorer", table("data", map[string]any{"Data": map[string]any{
			"Ordering": "Rows follow the primary key.",
			"Columns":  []string{"id", "note", "payload"},
			"Rows": [][]map[string]any{
				{cell("9007199254740993"), map[string]any{"Null": true}, cell("null")},
				{cell("2"), cell(""), cell("<b>not html</b>")},
			},
			"Prev": "/preview/explorer-data", "Next": "/preview/explorer-data",
		}})},
		{"/preview/explorer-columns", "explorer", table("columns", map[string]any{"ColumnInfo": []map[string]any{
			{"Ordinal": 1, "Name": "id", "Type": "bigint", "Nullable": false, "Default": "", "Identity": "by default", "Generated": ""},
			{"Ordinal": 2, "Name": "note", "Type": "text", "Nullable": true, "Default": "'n/a'::text", "Identity": "", "Generated": ""},
		}})},
		{"/preview/explorer-constraints", "explorer", table("constraints", map[string]any{"Constraints": []map[string]any{
			{"Kind": "primary key", "Columns": "id", "Definition": "PRIMARY KEY (id)"},
		}})},
		{"/preview/explorer-indexes", "explorer", table("indexes", map[string]any{"Indexes": []map[string]any{
			{"Name": "orders_pkey", "Method": "btree", "Unique": true, "Primary": true, "Valid": true, "Definition": "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)"},
		}})},
		{"/preview/drafts", "drafts", with(layout("Drafts", "drafts", true, true), map[string]any{
			"Drafts": []map[string]any{{"ID": draftID, "Title": "Add orders table", "Revision": 3, "UpdatedBy": "ana", "UpdatedAt": "2026-10-23 07:40:00"}},
			"Pager":  pager, "Form": map[string]any{"Title": "", "SQL": ""}, "Error": "",
		})},
		{"/preview/drafts-error", "drafts", with(layout("Drafts", "drafts", true, true), map[string]any{
			"Drafts": nil, "Pager": nil, "Form": map[string]any{"Title": "Half done", "SQL": "</textarea><script>alert(1)</script>"},
			"Error": "Check the form fields. Title must be 1 to 120 characters.",
		})},
		{"/preview/draft", "draft", with(layout("Add orders table", "drafts", true, true), map[string]any{
			"Draft": map[string]any{"ID": draftID, "Title": "Add orders table", "SQL": sampleSQL, "Revision": 3, "UpdatedBy": "ana", "UpdatedAt": "2026-10-23 07:40:00"},
			"Error": "", "Conflict": false,
		})},
		{"/preview/draft-conflict", "draft", with(layout("Add orders table", "drafts", true, true), map[string]any{
			"Draft": map[string]any{"ID": draftID, "Title": "My unsaved title", "SQL": sampleSQL, "Revision": 3, "UpdatedBy": "salman", "UpdatedAt": "2026-10-23 07:44:10"},
			"Error": "", "Conflict": true,
		})},
		{"/preview/review", "review", with(layout("Review: Add orders table", "drafts", true, true), map[string]any{
			"Draft":      map[string]any{"ID": draftID, "Title": "Add orders table", "SQL": sampleSQL, "Revision": 3},
			"RequestKey": "0b7f3c9e-2d41-4c8a-9a5e-1e6d7c8b9a00",
		})},
		{"/preview/history", "history", with(layout("History", "history", true, true), map[string]any{
			"Attempts": []map[string]any{historyRow("42", "running"), historyRow("41", "unknown"), historyRow("40", "failed"), historyRow("39", "succeeded")},
			"Pager":    pager, "SearchKey": "",
		})},
		{"/preview/history-search", "history", with(layout("History", "history", true, true), map[string]any{
			"Attempts": nil, "Pager": nil, "SearchKey": "0b7f3c9e-2d41-4c8a-9a5e-1e6d7c8b9a00",
		})},
		{"/preview/attempt-succeeded", "attempt", with(layout("Version 42", "history", true, true), map[string]any{"Attempt": attempt("succeeded", map[string]any{"FinishedAt": "2026-10-23 07:41:03"})})},
		{"/preview/attempt-failed", "attempt", with(layout("Version 42", "history", true, true), map[string]any{"Attempt": attempt("failed", map[string]any{
			"FinishedAt": "2026-10-23 07:41:03", "FailureCode": "SQL_ERROR", "FailureMessage": "relation \"orders\" already exists.", "SQLState": "42P07", "StatementIndex": 1})})},
		{"/preview/attempt-unknown", "attempt", with(layout("Version 42", "history", true, true), map[string]any{"Attempt": attempt("unknown", map[string]any{
			"FailureCode": "COMMIT_UNCERTAIN", "FailureMessage": "The commit result was not confirmed.", "FirstUnknownAt": "2026-10-23 07:41:30"})})},
		{"/preview/attempt-running", "attempt", with(layout("Version 42", "history", true, true), map[string]any{"Attempt": attempt("running", nil)})},
		{"/preview/admin-databases", "admin_databases", admin(layout("Connections", "connections", false, false), map[string]any{
			"Databases": []map[string]any{
				{"ID": dbID, "Name": "orders-prod", "Host": "db.internal", "Port": 5432, "DatabaseName": "orders", "Username": "databrain_ro", "SSLMode": "verify-full", "IsActive": true},
				{"ID": "11111111-2222-4333-8444-555555555555", "Name": "analytics", "Host": "dw.internal", "Port": 5432, "DatabaseName": "dw", "Username": "ro", "SSLMode": "disable", "IsActive": false},
			},
			"Pager": pager, "Form": map[string]any{"Name": "", "Host": "", "Port": "5432", "DatabaseName": "", "Username": "", "SSLMode": "verify-full"},
			"Error": "", "Code": "",
		})},
		{"/preview/admin-databases-error", "admin_databases", admin(layout("Connections", "connections", false, false), map[string]any{
			"Databases": nil, "Pager": nil, "Error": "Check the form fields.", "Code": "VALIDATION_FAILED",
			"Form": map[string]any{"Name": "orders-prod", "Host": "db.internal", "Port": "5432", "DatabaseName": "orders", "Username": "ro", "SSLMode": "disable"},
		})},
		{"/preview/admin-database", "admin_database", admin(layout("Connection", "connections", false, false), connection(nil, false))},
		{"/preview/admin-database-probe-ok", "admin_database", admin(layout("Connection", "connections", false, false),
			connection(map[string]any{"OK": true, "Reason": "", "ServerVersion": 170011, "Latency": 12 * time.Millisecond}, false))},
		{"/preview/admin-database-probe-failed", "admin_database", admin(layout("Connection", "connections", false, false),
			connection(map[string]any{"OK": false, "Reason": "authentication_failed", "ServerVersion": 0, "Latency": time.Duration(0)}, false))},
		{"/preview/admin-database-conflict", "admin_database", admin(layout("Connection", "connections", false, false), connection(nil, true))},
		{"/preview/login", "login", login(nil)},
		{"/preview/login-error", "login", login(map[string]any{"Username": "ana", "Error": "Username or password is incorrect."})},
		{"/preview/login-reauth", "login", login(map[string]any{"Username": "ana", "Reauth": true})},
		{"/preview/admin-users", "admin_users", admin(layout("Users", "users", false, false), map[string]any{
			"Users": []map[string]any{
				{"ID": userID, "Username": "ana", "DisplayName": "Ana Editor", "IsActive": true, "IsAdmin": false, "Revision": 4},
				{"ID": "44444444-5555-4666-8777-888888888888", "Username": "root", "DisplayName": "Root Admin", "IsActive": true, "IsAdmin": true, "Revision": 1},
			},
			"Pager": pager, "Form": map[string]any{"Username": "", "DisplayName": "", "IsAdmin": false}, "Error": "",
		})},
		{"/preview/admin-users-error", "admin_users", admin(layout("Users", "users", false, false), map[string]any{
			"Users": nil, "Pager": nil, "Error": "Check the form fields.",
			"Form": map[string]any{"Username": "Bad Name", "DisplayName": "Bad", "IsAdmin": true},
		})},
		{"/preview/admin-user", "admin_user", userProfile(false)},
		{"/preview/admin-user-conflict", "admin_user", userProfile(true)},
		{"/preview/admin-groups", "admin_groups", admin(layout("Groups", "groups", false, false), map[string]any{
			"Groups": []map[string]any{
				{"ID": groupID, "Name": "Analysts", "IsActive": true, "Revision": 2},
				{"ID": "99999999-8888-4777-8666-555555555555", "Name": "Retired team", "IsActive": false, "Revision": 5},
			},
			"Pager": pager, "Form": map[string]any{"Name": ""}, "Error": "",
		})},
		{"/preview/admin-groups-error", "admin_groups", admin(layout("Groups", "groups", false, false), map[string]any{
			"Groups": nil, "Pager": nil, "Error": "A group with that name already exists.", "Form": map[string]any{"Name": "Analysts"},
		})},
		{"/preview/admin-group", "admin_group", groupProfile(false)},
		{"/preview/admin-group-conflict", "admin_group", groupProfile(true)},
	}
}

func main() {
	renderer, err := httpx.LoadTemplates("web/templates")
	if err != nil || renderer == nil {
		log.Fatal("cannot load web/templates: run from backend/")
	}
	pages := samplePages()

	mux := http.NewServeMux()
	if _, err := httpx.MountStatic(mux, "web/static"); err != nil {
		log.Fatal(err)
	}
	for _, p := range pages {
		mux.HandleFunc("GET "+p.path, func(w http.ResponseWriter, r *http.Request) {
			if err := renderer.Render(w, r, http.StatusOK, p.name, p.data); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>Preview</title><h1>Preview pages</h1><ul>")
		for _, p := range pages {
			fmt.Fprintf(w, `<li><a href="%s">%s</a></li>`, p.path, p.path)
		}
	})
	log.Println("preview on http://localhost:8081")
	log.Fatal(http.ListenAndServe("127.0.0.1:8081", mux))
}
