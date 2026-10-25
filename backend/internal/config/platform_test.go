package config

import (
	"strings"
	"testing"
)

func TestPlatformDatabaseURL(t *testing.T) {
	for _, tt := range []struct{ raw, env string }{
		{"postgres://runtime:SENSITIVE@localhost:55432/platform?sslmode=disable", "development"},
		{"postgresql://runtime:SENSITIVE@localhost/platform?sslmode=verify-full&sslrootcert=root.crt", "production"},
	} {
		t.Setenv("PLATFORM_DATABASE_URL", tt.raw)
		got, err := LoadPlatformDatabaseURL(tt.env)
		if err != nil || got != tt.raw {
			t.Fatal("valid runtime DSN rejected or changed")
		}
	}
}

func TestPlatformDatabaseURLRejectsInvalidWithoutFallback(t *testing.T) {
	t.Setenv("MIGRATOR_DATABASE_URL", "postgres://migrator:SENSITIVE@localhost/platform")
	for _, raw := range []string{
		"", "host=localhost password=SENSITIVE", "https://runtime:SENSITIVE@localhost/platform",
		"postgres://runtime:SENSITIVE@/platform", "postgres://localhost/platform",
		"postgres://runtime:SENSITIVE@localhost/", "postgres://runtime:SENSITIVE@localhost:0/platform",
		"postgres://runtime:SENSITIVE@localhost/platform#SENSITIVE",
		"postgres://runtime:SENSITIVE@localhost/platform?sslmode=%zz",
		"postgres://runtime:SENSITIVE@localhost/platform?sslmode=disable&sslmode=require",
		"postgres://runtime:SENSITIVE@localhost/platform?pool_max_conns=100",
		"postgres://runtime:SENSITIVE@localhost/platform?x-migrations-table=other",
	} {
		t.Setenv("PLATFORM_DATABASE_URL", raw)
		got, err := LoadPlatformDatabaseURL("test")
		if err == nil || got != "" || strings.Contains(err.Error(), "SENSITIVE") {
			t.Fatal("invalid platform config accepted, disclosed, or fell back to migrator")
		}
	}
	for _, mode := range []string{"", "disable", "prefer", "require", "verify-ca"} {
		t.Setenv("PLATFORM_DATABASE_URL", "postgres://runtime:SENSITIVE@localhost/platform?sslmode="+mode)
		if _, err := LoadPlatformDatabaseURL("production"); err == nil || strings.Contains(err.Error(), "SENSITIVE") {
			t.Fatal("production accepted unverified TLS or disclosed DSN")
		}
	}
}
