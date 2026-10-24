package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/google/uuid"
)

func TestResolveCommandInput(t *testing.T) {
	for _, args := range [][]string{
		{"resolve-attempt"},
		{"resolve-attempt", "--attempt", "PRIVATE_INVALID"},
		{"resolve-attempt", "--attempt", uuid.NewString(), "--outcome", "unknown", "--operator", "PRIVATE_NAME", "--evidence-file", "PRIVATE_PATH"},
		{"resolve-attempt", "--evidence", "PRIVATE_EVIDENCE"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 || out.Len() != 0 || strings.Contains(errOut.String(), "PRIVATE") {
			t.Fatal("invalid command was accepted or exposed input")
		}
	}
	path := filepath.Join(t.TempDir(), "evidence")
	want := "PRIVATE_REASON\r\n  Evidence 猫\n"
	if err := os.WriteFile(path, []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readEvidenceFile(path); err != nil || got != want {
		t.Fatal("evidence was normalized or not read")
	}
	for _, data := range []string{"", " \r\n", string([]byte{0xff}), "NUL\x00", strings.Repeat("x", migration.MaxResolutionBytes+1)} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := readEvidenceFile(path); err == nil || got != "" || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("invalid evidence accepted or leaked")
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"resolve-attempt", "--attempt", uuid.NewString(), "--outcome", "failed", "--operator", "PRIVATE_NAME", "--evidence-file", filepath.Join(t.TempDir(), "PRIVATE_MISSING")}, &out, &errOut); code != 1 || out.Len() != 0 || strings.Contains(errOut.String(), "PRIVATE") {
		t.Fatal("file failure exit/output unsafe")
	}
}
