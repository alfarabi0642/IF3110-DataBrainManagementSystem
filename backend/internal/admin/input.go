package admin

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

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
