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
		{"migrate unavailable", []string{"migrate", "up"}, 2},
		{"bootstrap unavailable", []string{"bootstrap-admin", "--password", "SENSITIVE"}, 2},
		{"unknown command", []string{"SENSITIVE"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.code {
				t.Fatalf("exit code = %d; want %d", code, tt.code)
			}
			output := stdout.String() + stderr.String()
			if !strings.Contains(output, "No management commands are implemented yet.") || strings.Contains(output, "SENSITIVE") {
				t.Fatalf("misleading or unsafe usage: %q", output)
			}
			if (tt.code == 0 && stderr.Len() != 0) || (tt.code != 0 && stdout.Len() != 0) {
				t.Fatal("usage written to the wrong stream")
			}
		})
	}
}
