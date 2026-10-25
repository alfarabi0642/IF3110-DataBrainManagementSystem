package admin

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

func renderAdminTemplate(t *testing.T, name string, data any) string {
	t.Helper()
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatalf("templates not loaded: %v", err)
	}
	w := httptest.NewRecorder()
	if err := renderer.Render(w, httptest.NewRequest(http.MethodGet, "/admin", nil), http.StatusOK, name, data); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	body := w.Body.String()
	// Periksa setiap form agar token logout yang valid tidak menutupi token mutasi yang hilang atau ganda.
	for _, form := range regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`).FindAllString(body, -1) {
		if !strings.Contains(form, `method="post"`) || strings.Count(form, `name="_csrf"`) != 1 ||
			!strings.Contains(form, `name="_csrf" value="csrf-token"`) {
			t.Fatalf("invalid POST/CSRF contract: %s", form)
		}
	}
	for _, input := range regexp.MustCompile(`<input\b[^>]*>`).FindAllString(body, -1) {
		if strings.Contains(input, `type="password"`) && strings.Contains(input, `value=`) {
			t.Fatal("password input must never have a value attribute")
		}
	}
	return body
}

func TestAdminEmptyPages(t *testing.T) {
	layout := httpx.Layout{Title: "Admin", CSRF: "csrf-token"}
	for _, test := range []struct {
		name string
		page any
		want string
	}{
		{"admin_users", UsersPage{Layout: layout}, "No users yet."},
		{"admin_groups", GroupsPage{Layout: layout}, "No groups yet."},
		{"admin_user", UserPage{Layout: layout, UserProfile: UserItem{ID: "user-1", Revision: 1}, Form: UserForm{ExpectedRevision: "1"}}, "not a member of any group"},
		{"admin_group", GroupPage{Layout: layout, GroupProfile: GroupItem{ID: "group-1", Revision: 1}, Form: GroupForm{ExpectedRevision: "1"}}, "no database grants"},
	} {
		body := renderAdminTemplate(t, test.name, test.page)
		requireAdminContains(t, body, test.want)
		if test.name == "admin_group" {
			requireAdminContains(t, body, "no members", "No active users", "No active databases")
			rejectAdminContains(t, body, `name="user_id"`, `name="database_id"`)
		}
	}
}

func requireAdminContains(t *testing.T, body string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(body, value) {
			t.Errorf("missing %q in:\n%s", value, body)
		}
	}
}

func rejectAdminContains(t *testing.T, body string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(body, value) {
			t.Errorf("unexpected %q in:\n%s", value, body)
		}
	}
}

func TestAdminUserTemplates(t *testing.T) {
	layout := httpx.Layout{Title: "Admin", CSRF: "csrf-token", Active: "users",
		User: &httpx.LayoutUser{DisplayName: "Admin", Username: "admin", IsAdmin: true}}
	body := renderAdminTemplate(t, "admin_users", UsersPage{
		Layout: layout,
		Users:  []UserItem{{ID: "user-1", Username: "ana", DisplayName: `<i>Ana</i>`, IsActive: false, IsAdmin: true, Revision: 4}},
		Pager:  Pager{Next: "/admin/users?limit=50&offset=50"},
		Form:   UserForm{Username: `"><script>x</script>`, DisplayName: "New user"},
		Error:  "Check the form fields.",
	})
	requireAdminContains(t, body,
		`href="/admin/users/user-1"`, "&lt;i&gt;Ana&lt;/i&gt;", "Inactive", "yes", `rel="next"`,
		`action="/admin/users"`, `name="username"`, `name="display_name"`, `name="password" type="password"`,
		`name="is_platform_admin"`, `option value="false" selected`, `name="_csrf" value="csrf-token"`,
		`maxlength="80"`, `aria-describedby="username-help"`, `class="panel form-panel"`, "Platform user accounts",
	)
	rejectAdminContains(t, body, "<i>Ana", "<script>", "create-secret", `name="password" value=`)

	body = renderAdminTemplate(t, "admin_user", UserPage{
		Layout:      layout,
		UserProfile: UserItem{ID: "user-1", Username: "ana", DisplayName: "Ana", IsActive: true, Revision: 9},
		Groups:      []GroupItem{{ID: "group-1", Name: `<b>Engineering</b>`, IsActive: true}},
		Form:        UserForm{DisplayName: `"><img src=x>`, IsAdmin: true, ExpectedRevision: "8"},
		Error:       "The resource changed.",
		Conflict:    true,
	})
	requireAdminContains(t, body,
		`action="/admin/users/user-1"`, `action="/admin/users/user-1/password"`,
		`name="expected_revision" value="8"`, `name="expected_revision" value="9"`,
		`name="is_active"`, `name="is_platform_admin"`, `href="/admin/groups/group-1"`,
		"&lt;b&gt;Engineering&lt;/b&gt;", "Open the latest version", "The username is permanent.",
		"signs this user out", "signs this user out of every session", "Groups containing this user",
	)
	rejectAdminContains(t, body, "<img", "<b>Engineering", "reset-secret", `name="username"`, `name="password" value=`, "The resource changed.")
	if strings.Count(body, `class="status failed"`) != 1 {
		t.Fatal("user revision conflict rendered duplicate error alerts")
	}
}

func TestAdminGroupTemplates(t *testing.T) {
	layout := httpx.Layout{Title: "Admin", CSRF: "csrf-token", Active: "groups",
		User: &httpx.LayoutUser{DisplayName: "Admin", Username: "admin", IsAdmin: true}}
	body := renderAdminTemplate(t, "admin_groups", GroupsPage{
		Layout: layout,
		Groups: []GroupItem{{ID: "group-1", Name: `<i>Readers</i>`, IsActive: false, Revision: 2}},
		Pager:  Pager{Prev: "/admin/groups?limit=50&offset=0"},
		Form:   GroupForm{Name: `"><script>x</script>`},
		Error:  "Check the form fields.",
	})
	requireAdminContains(t, body, `href="/admin/groups/group-1"`, "&lt;i&gt;Readers&lt;/i&gt;", "Inactive", `rel="prev"`, `action="/admin/groups"`, `name="name"`, `maxlength="64"`, "Access groups")
	rejectAdminContains(t, body, "<i>Readers", "<script>")

	body = renderAdminTemplate(t, "admin_group", GroupPage{
		Layout:          layout,
		GroupProfile:    GroupItem{ID: "group-1", Name: "Readers", IsActive: true, Revision: 6},
		Form:            GroupForm{Name: `"><img src=x>`, ExpectedRevision: "5"},
		Members:         []MemberItem{{ID: "user-1", Username: "ana", DisplayName: `<b>Ana</b>`, IsActive: true}},
		MemberChoices:   []MemberItem{{ID: "user-2", Username: "budi", DisplayName: `<i>Budi</i>`}},
		Grants:          []GrantItem{{DatabaseID: "db-1", DatabaseName: `<em>Production</em>`, Access: access.ViewEdit}},
		DatabaseChoices: []DatabaseChoice{{ID: "db-2", Name: `<strong>Staging</strong>`}},
		Conflict:        true,
	})
	requireAdminContains(t, body,
		`action="/admin/groups/group-1"`, `name="expected_revision" value="5"`,
		`action="/admin/groups/group-1/members/user-1/remove"`, `action="/admin/groups/group-1/members"`, `name="user_id"`,
		`action="/admin/groups/group-1/databases/db-1/remove"`, `action="/admin/groups/group-1/databases"`,
		`name="database_id"`, `name="access_level"`, "View and edit", "Open the latest version",
		"&lt;b&gt;Ana&lt;/b&gt;", "&lt;i&gt;Budi&lt;/i&gt;", "&lt;em&gt;Production&lt;/em&gt;", "&lt;strong&gt;Staging&lt;/strong&gt;",
		"Current group members", "Current database grants", `class="table-action"`, "signs out its members",
	)
	rejectAdminContains(t, body,
		"<img", "<b>Ana", "<i>Budi", "<em>Production", "<strong>Staging",
		`type="hidden" name="user_id"`, `type="hidden" name="database_id"`,
	)
}

func TestInactiveGroupDoesNotOfferMemberAddition(t *testing.T) {
	layout := httpx.Layout{Title: "Group", CSRF: "csrf-token"}
	body := renderAdminTemplate(t, "admin_group", GroupPage{
		Layout:          layout,
		GroupProfile:    GroupItem{ID: "group-1", Name: "Disabled", Revision: 2},
		Form:            GroupForm{Name: "Disabled", ExpectedRevision: "2"},
		MemberChoices:   []MemberItem{{ID: "user-1", Username: "ana", DisplayName: "Ana", IsActive: true}},
		DatabaseChoices: []DatabaseChoice{{ID: "db-1", Name: "Production"}},
	})
	requireAdminContains(t, body, "No active users can be added while this group is inactive", `name="database_id"`, "only grant removal or a downgrade")
	rejectAdminContains(t, body, `action="/admin/groups/group-1/members"`, `name="user_id"`)
}
