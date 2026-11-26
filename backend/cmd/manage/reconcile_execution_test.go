package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReconcileExecutionCLIValidation(t *testing.T) {
	t.Run("help exits 0 and shows usage", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := run([]string{"reconcile-execution", "--help"}, &out, &errOut); code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
		if !strings.Contains(out.String(), "reconcile-execution --execution UUID") {
			t.Fatalf("unexpected usage: %s", out.String())
		}
	})

	t.Run("missing execution UUID exits 2", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := run([]string{"reconcile-execution"}, &out, &errOut); code != 2 {
			t.Fatalf("expected exit 2, got %d", code)
		}
	})

	t.Run("invalid UUID exits 2", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := run([]string{"reconcile-execution", "--execution", "not-a-uuid"}, &out, &errOut); code != 2 {
			t.Fatalf("expected exit 2, got %d", code)
		}
	})

	t.Run("unknown force-success flag is rejected with exit 2", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := run([]string{"reconcile-execution", "--execution", "00000000-0000-0000-0000-000000000001", "--force-success"}, &out, &errOut); code != 2 {
			t.Fatalf("expected exit 2 for unknown flag, got %d", code)
		}
	})
}
