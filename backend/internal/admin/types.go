package admin

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

type UserItem struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsActive    bool   `json:"is_active"`
	IsAdmin     bool   `json:"is_platform_admin"`
	Revision    int64  `json:"revision,string"`
}

type GroupItem struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	IsActive          bool    `json:"is_active"`
	Revision          int64   `json:"revision,string"`
	LeaderID          *string `json:"leader_id"`
	LeaderUsername    string  `json:"leader_username,omitempty"`
	LeaderDisplayName string  `json:"leader_display_name,omitempty"`
}

type MemberItem struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsActive    bool   `json:"is_active"`
}

type GrantItem struct {
	DatabaseID   string       `json:"database_id"`
	DatabaseName string       `json:"database_name"`
	Access       access.Level `json:"access"`
}

type DatabaseChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

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
	Name     string
	LeaderID uuid.UUID
}

type EditGroupInput struct {
	ExpectedRevision int64
	Name             string
	IsActive         bool
}

func validationFailed() error { return &problem.Error{Code: problem.ValidationFailed} }

func validateCreateUser(in CreateUserInput) (CreateUserInput, error) {
	displayName, err := auth.NormalizeDisplayName(in.DisplayName)
	if auth.ValidateUsername(in.Username) != nil || err != nil || auth.ValidatePassword(in.Password) != nil {
		return CreateUserInput{}, validationFailed()
	}
	in.DisplayName = displayName
	return in, nil
}

func normalizeGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 ||
		utf8.RuneCountInString(name) > 64 || strings.ContainsRune(name, 0) {
		return "", validationFailed()
	}
	return name, nil
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

func pathID(r *http.Request, name string) (uuid.UUID, error) { return parsePathID(r.PathValue(name)) }
