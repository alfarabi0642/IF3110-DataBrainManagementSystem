package config

import (
	"net/url"
	"strings"
	"testing"
)

func TestSeparateDatabasePasswords(t *testing.T) {
	password := " sp@ce:/?#%$'\\猫 "
	t.Setenv("APP_ENV", "test")
	for _, item := range []struct {
		prefix string
		load   func() (string, error)
	}{
		{"PLATFORM", func() (string, error) { return LoadPlatformDatabaseURL("test") }},
		{"MIGRATOR", LoadMigratorDatabaseURL},
	} {
		t.Run(item.prefix, func(t *testing.T) {
			t.Setenv(item.prefix+"_DATABASE_URL", "postgres://runtime:old@localhost:55432/platform?sslmode=disable")
			t.Setenv(item.prefix+"_DATABASE_PASSWORD", password)
			raw, err := item.load()
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal("invalid encoded URL")
			}
			got, _ := u.User.Password()
			if got != password || u.Host != "localhost:55432" || u.Path != "/platform" || u.Fragment != "" {
				t.Fatal("password altered URL structure or bytes")
			}
			t.Setenv(item.prefix+"_DATABASE_URL", "invalid-SENSITIVE")
			raw, err = item.load()
			if err == nil || raw != "" || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "SENSITIVE") {
				t.Fatal("invalid config exposed secret")
			}
		})
	}
}
