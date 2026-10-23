package main

import (
	"bytes"
	"strings"
	"testing"
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
		{"bootstrap unavailable", []string{"bootstrap-admin", "--password", "SENSITIVE"}, 2},
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
