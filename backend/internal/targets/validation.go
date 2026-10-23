package targets

import (
	"strings"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

type CreateInput struct {
	Name, Host, DatabaseName, Username, Password, SSLMode string
	Port                                                  int
}

// EditInput deliberately excludes the immutable host, port and database name.
// Empty Password retains the existing credential.
type EditInput struct {
	ExpectedRevision                  int64
	Name, Username, Password, SSLMode string
	IsActive                          bool
}

func failure(code problem.Code) error { return &problem.Error{Code: code} }

func validName(name string) bool {
	return utf8.ValidString(name) && utf8.RuneCountInString(name) >= 1 &&
		utf8.RuneCountInString(name) <= 80 && !strings.ContainsRune(name, 0)
}

func validIdentifier(value string) bool {
	return utf8.ValidString(value) && len(value) >= 1 && len(value) <= 63 && !strings.ContainsRune(value, 0)
}

func validPassword(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= 1 &&
		utf8.RuneCountInString(value) <= 1024 && !strings.ContainsRune(value, 0)
}

func (p *Provider) validate(in CreateInput) (CreateInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	host, err := config.NormalizeTargetHost(in.Host)
	if err != nil || !validName(in.Name) || in.Port < 1 || in.Port > 65535 ||
		!validIdentifier(in.DatabaseName) || !validIdentifier(in.Username) || !validPassword(in.Password) {
		return CreateInput{}, failure(problem.ValidationFailed)
	}
	in.Host = host
	if !p.allowed[host] || in.SSLMode != "verify-full" && in.SSLMode != "disable" ||
		p.settings.AppEnv == "production" && in.SSLMode != "verify-full" {
		return CreateInput{}, failure(problem.ValidationFailed)
	}
	return in, nil
}
