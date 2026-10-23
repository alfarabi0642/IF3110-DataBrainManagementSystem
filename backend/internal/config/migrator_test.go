package config

import (
	"net/url"
	"strings"
	"testing"
)

func TestMigratorDatabaseURL(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql"} {
		raw := scheme + "://migrator:SENSITIVE@localhost:55432/platform?sslmode=verify-full&sslrootcert=root.crt"
		got, err := migratorDatabaseURL(raw, "production")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(got)
		if u.Query().Get("sslmode") != "verify-full" || u.Query().Get("sslrootcert") != "root.crt" ||
			u.Query().Get("connect_timeout") != "5" || u.Query().Get("statement_timeout") != "10000" ||
			u.Query().Get("lock_timeout") != "3000" || u.Query().Get("search_path") != "pg_catalog,public" {
			t.Fatal("TLS options or migration limits were not preserved")
		}
	}
}

func TestMigratorDatabaseURLRejectsInvalidWithoutSecrets(t *testing.T) {
	for _, tt := range []struct{ raw, env string }{
		{"", ""},
		{"host=localhost password=SENSITIVE", ""},
		{"https://migrator:SENSITIVE@localhost/platform", ""},
		{"postgres://migrator:SENSITIVE@/platform", ""},
		{"postgres://localhost/platform", ""},
		{"postgres://migrator:SENSITIVE@localhost/", ""},
		{"postgres://migrator:SENSITIVE@localhost:0/platform", ""},
		{"postgres://migrator:SENSITIVE@localhost/platform#SENSITIVE", ""},
		{"postgres://migrator:SENSITIVE@localhost/platform?sslmode=%zz", ""},
		{"postgres://migrator:SENSITIVE@localhost/platform?sslmode=disable&sslmode=require", ""},
		{"postgres://migrator:SENSITIVE@localhost/platform?x-multi-statement=true", ""},
		{"postgres://migrator:SENSITIVE@localhost/platform?sslmode=disable", "production"},
		{"postgres://migrator:SENSITIVE@localhost/platform", "production"},
		{"postgres://migrator:SENSITIVE@localhost/platform", "staging"},
		{"postgres://migrator:SENSITIVE@localhost/platform", ""},
	} {
		got, err := migratorDatabaseURL(tt.raw, tt.env)
		if err == nil || got != "" || strings.Contains(err.Error(), "SENSITIVE") {
			t.Fatal("invalid migration configuration accepted or disclosed")
		}
	}
}

func TestLoadMigratorDatabaseURLDoesNotNeedHTTPConfig(t *testing.T) {
	t.Setenv("MIGRATOR_DATABASE_URL", "postgres://migrator:SENSITIVE@localhost/platform?sslmode=disable")
	t.Setenv("APP_ENV", "test")
	t.Setenv("APP_ORIGIN", "INVALID_HTTP_ORIGIN")
	if _, err := LoadMigratorDatabaseURL(); err != nil {
		t.Fatal("migrator must load independently of HTTP configuration")
	}
}
