package admin

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func requireAdminCode(t *testing.T, err error, code problem.Code) {
	t.Helper()
	var value *problem.Error
	if !errors.As(err, &value) || value.Code != code {
		t.Fatalf("error = %v; want %s", err, code)
	}
}

func TestAdminFormParsers(t *testing.T) {
	csrf := "csrf-token"
	user, err := parseCreateUser(url.Values{
		"_csrf": {csrf}, "username": {"ana_1"}, "display_name": {"  Ana 猫  "},
		"password": {"猫猫猫猫"}, "is_platform_admin": {"false"},
	})
	if err != nil || user.Username != "ana_1" || user.DisplayName != "Ana 猫" ||
		user.Password != "猫猫猫猫" || user.IsPlatformAdmin {
		t.Fatalf("create user = %+v, %v", user, err)
	}
	edit, err := parseEditUser(url.Values{
		"_csrf": {csrf}, "expected_revision": {"7"}, "display_name": {" Ana "},
		"is_active": {"false"}, "is_platform_admin": {"true"},
	})
	if err != nil || edit.ExpectedRevision != 7 || edit.DisplayName != "Ana" || edit.IsActive || !edit.IsPlatformAdmin {
		t.Fatalf("edit user = %+v, %v", edit, err)
	}
	reset, err := parseResetPassword(url.Values{
		"_csrf": {csrf}, "expected_revision": {"8"}, "password": {" no trimming "},
	})
	if err != nil || reset.ExpectedRevision != 8 || reset.Password != " no trimming " {
		t.Fatalf("reset password = %+v, %v", reset, err)
	}
	group, err := parseCreateGroup(url.Values{"_csrf": {csrf}, "name": {"  Engineering  "}})
	if err != nil || group.Name != "Engineering" {
		t.Fatalf("create group = %+v, %v", group, err)
	}
	groupEdit, err := parseEditGroup(url.Values{
		"_csrf": {csrf}, "expected_revision": {"2"}, "name": {" Readers "}, "is_active": {"true"},
	})
	if err != nil || groupEdit.ExpectedRevision != 2 || groupEdit.Name != "Readers" || !groupEdit.IsActive {
		t.Fatalf("edit group = %+v, %v", groupEdit, err)
	}
	userID, databaseID := uuid.New(), uuid.New()
	member, err := parseMember(url.Values{"_csrf": {csrf}, "user_id": {userID.String()}})
	if err != nil || member.UserID != userID {
		t.Fatalf("member = %+v, %v", member, err)
	}
	grant, err := parseGrant(url.Values{
		"_csrf": {csrf}, "database_id": {databaseID.String()}, "access_level": {string(access.ViewEdit)},
	})
	if err != nil || grant.DatabaseID != databaseID || grant.Access != access.ViewEdit {
		t.Fatalf("grant = %+v, %v", grant, err)
	}
	if err := parseNoFields(url.Values{"_csrf": {csrf}}); err != nil {
		t.Fatal(err)
	}
	if parsed, err := parsePathID(userID.String()); err != nil || parsed != userID {
		t.Fatalf("path ID = %v, %v", parsed, err)
	}
}

func TestAdminFormParsersRejectStructuralInput(t *testing.T) {
	valid := url.Values{
		"_csrf": {"csrf-token"}, "username": {"ana"}, "display_name": {"Ana"},
		"password": {"valid-password"}, "is_platform_admin": {"false"},
	}
	for _, change := range []func(url.Values){
		func(v url.Values) { delete(v, "_csrf") },
		func(v url.Values) { v["username"] = []string{"ana", "budi"} },
		func(v url.Values) { v.Set("actor_id", uuid.NewString()) },
		func(v url.Values) { v.Set("is_platform_admin", "yes") },
	} {
		values := valid.Clone()
		change(values)
		_, err := parseCreateUser(values)
		requireAdminCode(t, err, problem.InvalidRequest)
	}
	for _, revision := range []string{"", "0", "+1", "1.5", "9223372036854775808"} {
		_, err := parseResetPassword(url.Values{
			"_csrf": {"csrf-token"}, "expected_revision": {revision}, "password": {"valid-password"},
		})
		requireAdminCode(t, err, problem.InvalidRequest)
	}
	if err := parseNoFields(url.Values{"_csrf": {"csrf-token"}, "user_id": {uuid.NewString()}}); err == nil {
		t.Fatal("remove form accepted an ID already owned by the path")
	}
	if _, err := parsePathID("F056DC66-7C58-4AFD-A8AF-38E1EDA90A2D"); err == nil {
		t.Fatal("path accepted a noncanonical UUID")
	}
}

func TestAdminFormParsersRejectInvalidDomainValues(t *testing.T) {
	for _, values := range []url.Values{
		{"_csrf": {"t"}, "username": {"Admin"}, "display_name": {"Admin"}, "password": {"valid-password"}, "is_platform_admin": {"false"}},
		{"_csrf": {"t"}, "username": {"admin"}, "display_name": {" "}, "password": {"valid-password"}, "is_platform_admin": {"false"}},
		{"_csrf": {"t"}, "username": {"admin"}, "display_name": {"Admin"}, "password": {"short"}, "is_platform_admin": {"false"}},
	} {
		_, err := parseCreateUser(values)
		requireAdminCode(t, err, problem.ValidationFailed)
	}
	for _, name := range []string{" ", strings.Repeat("界", 65), "bad\x00name"} {
		_, err := parseCreateGroup(url.Values{"_csrf": {"t"}, "name": {name}})
		requireAdminCode(t, err, problem.ValidationFailed)
	}
	_, err := parseGrant(url.Values{
		"_csrf": {"t"}, "database_id": {uuid.NewString()}, "access_level": {"owner"},
	})
	requireAdminCode(t, err, problem.ValidationFailed)
}

func TestAdminListPagination(t *testing.T) {
	for _, test := range []struct {
		query         string
		limit, offset int
	}{
		{"", 50, 0},
		{"?limit=200&offset=100000", 200, 100000},
		{"?offset=25", 50, 25},
	} {
		limit, offset, err := parsePagination(httptest.NewRequest("GET", "/admin/users"+test.query, nil))
		if err != nil || limit != test.limit || offset != test.offset {
			t.Fatalf("pagination %q = %d,%d,%v", test.query, limit, offset, err)
		}
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?offset=-1", "?limit=1&limit=2", "?search=x"} {
		_, _, err := parsePagination(httptest.NewRequest("GET", "/admin/users"+query, nil))
		requireAdminCode(t, err, problem.InvalidRequest)
	}
	if pager := listPager("/admin/users", 50, 50, true); pager.Prev != "/admin/users?limit=50&offset=0" ||
		pager.Next != "/admin/users?limit=50&offset=100" {
		t.Fatalf("pager = %+v", pager)
	}
	if pager := listPager("/admin/groups", 200, 100000, true); pager.Next != "" || pager.Prev == "" {
		t.Fatalf("bounded pager = %+v", pager)
	}
}

func TestEveryAdminFormRequiresExactFields(t *testing.T) {
	id := "f056dc66-7c58-4afd-a8af-38e1eda90a2d"
	for _, test := range []struct {
		name   string
		values url.Values
		parse  func(url.Values) error
	}{
		{"create user", url.Values{"_csrf": {"t"}, "username": {"ana"}, "display_name": {"Ana"}, "password": {"valid-password"}, "is_platform_admin": {"false"}},
			func(v url.Values) error { _, err := parseCreateUser(v); return err }},
		{"edit user", url.Values{"_csrf": {"t"}, "expected_revision": {"1"}, "display_name": {"Ana"}, "is_active": {"true"}, "is_platform_admin": {"false"}},
			func(v url.Values) error { _, err := parseEditUser(v); return err }},
		{"reset", url.Values{"_csrf": {"t"}, "expected_revision": {"1"}, "password": {"valid-password"}},
			func(v url.Values) error { _, err := parseResetPassword(v); return err }},
		{"create group", url.Values{"_csrf": {"t"}, "name": {"Readers"}},
			func(v url.Values) error { _, err := parseCreateGroup(v); return err }},
		{"edit group", url.Values{"_csrf": {"t"}, "expected_revision": {"1"}, "name": {"Readers"}, "is_active": {"true"}},
			func(v url.Values) error { _, err := parseEditGroup(v); return err }},
		{"member", url.Values{"_csrf": {"t"}, "user_id": {id}},
			func(v url.Values) error { _, err := parseMember(v); return err }},
		{"grant", url.Values{"_csrf": {"t"}, "database_id": {id}, "access_level": {"view_only"}},
			func(v url.Values) error { _, err := parseGrant(v); return err }},
		{"remove", url.Values{"_csrf": {"t"}}, parseNoFields},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.parse(test.values); err != nil {
				t.Fatal(err)
			}
			for key := range test.values {
				missing := test.values.Clone()
				delete(missing, key)
				requireAdminCode(t, test.parse(missing), problem.InvalidRequest)
				duplicate := test.values.Clone()
				duplicate.Add(key, duplicate.Get(key))
				requireAdminCode(t, test.parse(duplicate), problem.InvalidRequest)
			}
			unknown := test.values.Clone()
			unknown.Set("actor_id", id)
			requireAdminCode(t, test.parse(unknown), problem.InvalidRequest)
			if test.name == "edit user" {
				immutable := test.values.Clone()
				immutable.Set("username", "different_user")
				requireAdminCode(t, test.parse(immutable), problem.InvalidRequest)
			}
		})
	}
}
