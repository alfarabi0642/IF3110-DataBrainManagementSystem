package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func sqlHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func TestSanitizeSQLRedactsLiteralsAndComments(t *testing.T) {
	raw := "SELECT 'SENSITIVE', 123, 1.5, B'101', X'AF', E'esc\\n', $tag$dollar SENSITIVE$tag$, " +
		"U&'d\\0061t\\+000061' -- SENSITIVE line\nFROM users /* SENSITIVE block */ " +
		"WHERE password='SENSITIVE' AND id=$1"
	safe := SanitizeSQL(raw)
	if safe.Template == redactedSQL || !safe.Redacted || safe.Hash != sqlHash(raw) {
		t.Fatalf("sanitization failed closed unexpectedly: %+v", safe)
	}
	for _, sensitive := range []string{"SENSITIVE", "esc", "dollar", "--", "/*"} {
		if strings.Contains(safe.Template, sensitive) {
			t.Fatalf("template leaked %q: %q", sensitive, safe.Template)
		}
	}
	if !strings.Contains(safe.Template, "id=$1") || !strings.Contains(safe.Template, "users") {
		t.Fatalf("template lost structure: %q", safe.Template)
	}
}

func TestSanitizeSQLWithoutSensitiveTokensKeepsTemplate(t *testing.T) {
	raw := "SELECT id FROM public.users WHERE id=$1 AND name IS NOT NULL"
	safe := SanitizeSQL(raw)
	if safe.Template != raw || safe.Redacted || safe.Hash != sqlHash(raw) {
		t.Fatalf("plain SQL changed: %+v", safe)
	}
}

func TestSanitizeSQLFailsClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":        "",
		"malformed":    "SELECT 'unterminated SENSITIVE",
		"nul":          "SELECT 'SENSITIVE'\x00",
		"invalid utf8": "SELECT '\xff SENSITIVE'",
		"oversized":    "SELECT '" + strings.Repeat("a", MaxLogSQLBytes) + "'",
	} {
		t.Run(name, func(t *testing.T) {
			safe := SanitizeSQL(raw)
			if safe.Template != redactedSQL || !safe.Redacted || safe.Hash != sqlHash(raw) || strings.Contains(safe.Template, "SENSITIVE") {
				t.Fatalf("fail-closed sanitization wrong: %+v", safe)
			}
		})
	}
}

func TestRedactArgsKeepsPositionAndTypeOnly(t *testing.T) {
	args := []any{"SENSITIVE", []byte("SENSITIVE"), 123, nil, &net.OpError{Op: "dial", Net: "tcp"}}
	got := RedactArgs(args)
	want := []string{"$1: string", "$2: []uint8", "$3: int", "$4: null", "$5: *net.OpError"}
	if len(got) != len(want) {
		t.Fatalf("argument descriptors = %v; want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("argument %d = %q; want %q", index+1, got[index], want[index])
		}
	}
	for _, value := range got {
		if strings.Contains(value, "SENSITIVE") {
			t.Fatal("argument descriptor leaked a value")
		}
	}
	many := make([]any, MaxLogArgs+3)
	for index := range many {
		many[index] = "SENSITIVE"
	}
	capped := RedactArgs(many)
	if len(capped) != MaxLogArgs+1 || !strings.Contains(capped[len(capped)-1], "3 more arguments") {
		t.Fatalf("argument cap failed: %v", capped)
	}
}

func TestSanitizeErrorNeverCopiesDriverText(t *testing.T) {
	driver := &pgconn.PgError{Code: "23505", Message: "SENSITIVE", Detail: "SENSITIVE", Hint: "SENSITIVE", Where: "SENSITIVE"}
	if got := SanitizeError(driver); got != (SafeError{Code: "23505", Kind: "sql"}) {
		t.Fatalf("driver error not sanitized: %+v", got)
	}
	for name, err := range map[string]error{
		"driver":     driver,
		"connection": &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("SENSITIVE")},
		"deadline":   fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
		"canceled":   fmt.Errorf("wrapped: %w", context.Canceled),
		"unknown":    errors.New("postgres://runtime:SENSITIVE@example/db"),
	} {
		t.Run(name, func(t *testing.T) {
			got := SanitizeError(err)
			if strings.Contains(fmt.Sprintf("%+v", got), "SENSITIVE") || got.Kind == "" {
				t.Fatalf("safe error leaked or lacked kind: %+v", got)
			}
		})
	}
	if got := SanitizeError(nil); got != (SafeError{}) {
		t.Fatalf("nil error produced %+v", got)
	}
}
