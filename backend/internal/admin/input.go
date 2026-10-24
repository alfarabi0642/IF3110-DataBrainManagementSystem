package admin

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

type CreateUserInput struct {
	Username        string
	DisplayName     string
	Password        string
	IsPlatformAdmin bool
}

type EditUserInput struct {
	ExpectedRevision int64
	DisplayName      string
	IsActive         bool
	IsPlatformAdmin  bool
}

type ResetPasswordInput struct {
	ExpectedRevision int64
	Password         string
}

type CreateGroupInput struct {
	Name string
}

type EditGroupInput struct {
	ExpectedRevision int64
	Name             string
	IsActive         bool
}

type MemberInput struct {
	UserID uuid.UUID
}

type GrantInput struct {
	DatabaseID uuid.UUID
	Access     access.Level
}

func validationFailed() error { return &problem.Error{Code: problem.ValidationFailed} }

// These parsers consume PostForm only after RequireCSRF has authenticated the
// family and verified Origin/token. They do not validate CSRF or authorize.
func parseCreateUser(values url.Values) (CreateUserInput, error) {
	if err := httpx.ValidateFields(values,
		[]string{"_csrf", "username", "display_name", "password", "is_platform_admin"}, nil); err != nil {
		return CreateUserInput{}, err
	}
	admin, err := httpx.BoolValue(values, "is_platform_admin")
	if err != nil {
		return CreateUserInput{}, err
	}
	return validateCreateUser(CreateUserInput{Username: values.Get("username"), DisplayName: values.Get("display_name"),
		Password: values.Get("password"), IsPlatformAdmin: admin})
}

func validateCreateUser(in CreateUserInput) (CreateUserInput, error) {
	displayName, err := auth.NormalizeDisplayName(in.DisplayName)
	if auth.ValidateUsername(in.Username) != nil || err != nil || auth.ValidatePassword(in.Password) != nil {
		return CreateUserInput{}, validationFailed()
	}
	in.DisplayName = displayName
	return in, nil
}

func parseEditUser(values url.Values) (EditUserInput, error) {
	if err := httpx.ValidateFields(values,
		[]string{"_csrf", "expected_revision", "display_name", "is_active", "is_platform_admin"}, nil); err != nil {
		return EditUserInput{}, err
	}
	revision, err := httpx.Int64Value(values, "expected_revision", 1, math.MaxInt64)
	if err != nil {
		return EditUserInput{}, err
	}
	active, err := httpx.BoolValue(values, "is_active")
	if err != nil {
		return EditUserInput{}, err
	}
	admin, err := httpx.BoolValue(values, "is_platform_admin")
	if err != nil {
		return EditUserInput{}, err
	}
	displayName, err := auth.NormalizeDisplayName(values.Get("display_name"))
	if err != nil {
		return EditUserInput{}, validationFailed()
	}
	return EditUserInput{ExpectedRevision: revision, DisplayName: displayName,
		IsActive: active, IsPlatformAdmin: admin}, nil
}

func parseResetPassword(values url.Values) (ResetPasswordInput, error) {
	if err := httpx.ValidateFields(values, []string{"_csrf", "expected_revision", "password"}, nil); err != nil {
		return ResetPasswordInput{}, err
	}
	revision, err := httpx.Int64Value(values, "expected_revision", 1, math.MaxInt64)
	if err != nil {
		return ResetPasswordInput{}, err
	}
	if auth.ValidatePassword(values.Get("password")) != nil {
		return ResetPasswordInput{}, validationFailed()
	}
	return ResetPasswordInput{ExpectedRevision: revision, Password: values.Get("password")}, nil
}

func normalizeGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 ||
		utf8.RuneCountInString(name) > 64 || strings.ContainsRune(name, 0) {
		return "", validationFailed()
	}
	return name, nil
}

func parseCreateGroup(values url.Values) (CreateGroupInput, error) {
	if err := httpx.ValidateFields(values, []string{"_csrf", "name"}, nil); err != nil {
		return CreateGroupInput{}, err
	}
	name, err := normalizeGroupName(values.Get("name"))
	if err != nil {
		return CreateGroupInput{}, err
	}
	return CreateGroupInput{Name: name}, nil
}

func parseEditGroup(values url.Values) (EditGroupInput, error) {
	if err := httpx.ValidateFields(values, []string{"_csrf", "expected_revision", "name", "is_active"}, nil); err != nil {
		return EditGroupInput{}, err
	}
	revision, err := httpx.Int64Value(values, "expected_revision", 1, math.MaxInt64)
	if err != nil {
		return EditGroupInput{}, err
	}
	active, err := httpx.BoolValue(values, "is_active")
	if err != nil {
		return EditGroupInput{}, err
	}
	name, err := normalizeGroupName(values.Get("name"))
	if err != nil {
		return EditGroupInput{}, err
	}
	return EditGroupInput{ExpectedRevision: revision, Name: name, IsActive: active}, nil
}

func parseMember(values url.Values) (MemberInput, error) {
	if err := httpx.ValidateFields(values, []string{"_csrf", "user_id"}, nil); err != nil {
		return MemberInput{}, err
	}
	id, err := httpx.UUIDValue(values, "user_id")
	if err != nil {
		return MemberInput{}, err
	}
	return MemberInput{UserID: id}, nil
}

func parseGrant(values url.Values) (GrantInput, error) {
	if err := httpx.ValidateFields(values, []string{"_csrf", "database_id", "access_level"}, nil); err != nil {
		return GrantInput{}, err
	}
	id, err := httpx.UUIDValue(values, "database_id")
	if err != nil {
		return GrantInput{}, err
	}
	level := access.Level(values.Get("access_level"))
	if level != access.ViewOnly && level != access.ViewEdit {
		return GrantInput{}, validationFailed()
	}
	return GrantInput{DatabaseID: id, Access: level}, nil
}

func parseNoFields(values url.Values) error {
	return httpx.ValidateFields(values, []string{"_csrf"}, nil)
}

func parsePathID(value string) (uuid.UUID, error) {
	return httpx.UUIDValue(url.Values{"id": {value}}, "id")
}

func parsePagination(r *http.Request) (int, int, error) {
	values, err := httpx.ParseQuery(r, "limit", "offset")
	if err != nil {
		return 0, 0, err
	}
	limit, offset := int64(50), int64(0)
	if values.Has("limit") {
		limit, err = httpx.Int64Value(values, "limit", 1, 200)
	}
	if err == nil && values.Has("offset") {
		offset, err = httpx.Int64Value(values, "offset", 0, 100000)
	}
	return int(limit), int(offset), err
}

func listPager(path string, limit, offset int, more bool) Pager {
	link := func(nextOffset int) string {
		return path + "?" + url.Values{
			"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(nextOffset)},
		}.Encode()
	}
	var result Pager
	if offset > 0 {
		result.Prev = link(max(0, offset-limit))
	}
	if more && offset+limit <= 100000 {
		result.Next = link(offset + limit)
	}
	return result
}
