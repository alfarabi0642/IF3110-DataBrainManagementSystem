package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
)

func TestManageUsageAndUnavailableCommands(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		code int
	}{
		{"no command", nil, 2},
		{"help", []string{"--help"}, 0},
		{"unsupported migration action", []string{"migrate", "force", "SENSITIVE"}, 2},
		{"down unavailable", []string{"migrate", "down"}, 2},
		{"bootstrap rejects password argument", []string{"bootstrap-admin", "--password", "SENSITIVE"}, 2},
		{"unknown command", []string{"SENSITIVE"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.code {
				t.Fatalf("exit code = %d; want %d", code, tt.code)
			}
			output := stdout.String() + stderr.String()
			if !strings.Contains(output, "Usage: manage migrate up") || strings.Contains(output, "SENSITIVE") {
				t.Fatalf("misleading or unsafe usage: %q", output)
			}
			if (tt.code == 0 && stderr.Len() != 0) || (tt.code != 0 && stdout.Len() != 0) {
				t.Fatal("usage written to the wrong stream")
			}
		})
	}
}

func TestBootstrapRequiresFieldsWithoutDatabase(t *testing.T) {
	t.Setenv("PLATFORM_DATABASE_URL", "postgres://runtime:SENSITIVE@localhost/platform")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"bootstrap-admin", "--username", "admin"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "--password-file") || strings.Contains(stderr.String(), "SENSITIVE") {
		t.Fatal("bootstrap usage was missing or disclosed configuration")
	}
}

func TestReadPasswordFilePreservesBytesAndBoundsInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	want := "Valid-pass!\n"
	if err := os.WriteFile(path, []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readPasswordFile(path)
	if err != nil || got != want {
		t.Fatalf("password file changed: %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 129)), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readPasswordFile(path); got != "" || !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("oversized password accepted: len=%d error=%v", len(got), err)
	}
	sensitivePath := filepath.Join(dir, "SENSITIVE-missing")
	if _, err := readPasswordFile(sensitivePath); err == nil || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatal("password file error disclosed its path")
	}
}

func TestMigrateRequiresSeparateDSN(t *testing.T) {
	t.Setenv("MIGRATOR_DATABASE_URL", "")
	t.Setenv("PLATFORM_DATABASE_URL", "postgres://runtime:SENSITIVE@localhost/platform")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"migrate", "up"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "MIGRATOR_DATABASE_URL") || strings.Contains(stderr.String(), "SENSITIVE") {
		t.Fatal("migration config failure must be safe and must not fall back to the runtime DSN")
	}
}
