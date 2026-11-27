// Package telemetry menyediakan logger JSON, tracer OpenTelemetry, dan sanitasi SQL untuk log/span.
package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	pg "github.com/pganalyze/pg_query_go/v6"
	query "github.com/wasilibs/go-pgquery"
)

const (
	// MaxLogSQLBytes membatasi SQL yang disanitasi. Query yang lebih besar dicatat sebagai marker
	// beserta hash, tidak pernah sebagian mentah.
	MaxLogSQLBytes = 32 << 10
	// MaxLogArgs membatasi jumlah parameter yang dideskripsikan.
	MaxLogArgs = 64

	redactedSQL = "[redacted sql]"
)

// SanitizedSQL adalah hasil sanitasi query. Template tidak memuat komentar atau nilai literal; Hash
// selalu hash byte asli untuk korelasi.
type SanitizedSQL struct {
	Template string
	Hash     string
	Redacted bool
}

// SanitizeSQL menghapus komentar dan mengganti literal string/angka/byte/bit dengan marker. Parser
// atau scanner yang gagal menghasilkan marker dan hash saja (fail closed); raw SQL tidak pernah
// dikembalikan.
func SanitizeSQL(raw string) SanitizedSQL {
	hash := sha256.Sum256([]byte(raw))
	result := SanitizedSQL{Template: redactedSQL, Hash: hex.EncodeToString(hash[:]), Redacted: true}
	if len(raw) == 0 || len(raw) > MaxLogSQLBytes || !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return result
	}
	if _, err := query.Parse(raw); err != nil {
		return result
	}
	scan, err := query.Scan(raw)
	if err != nil || len(scan.Tokens) == 0 {
		return result
	}
	type replacement struct {
		start, end int
		text       string
	}
	replacements := make([]replacement, 0)
	for _, token := range scan.Tokens {
		start, end := int(token.Start), int(token.End)
		if start < 0 || end < start || end > len(raw) {
			return result
		}
		text := ""
		switch token.Token {
		case pg.Token_SQL_COMMENT, pg.Token_C_COMMENT:
			text = " "
		case pg.Token_SCONST, pg.Token_USCONST, pg.Token_ICONST, pg.Token_FCONST, pg.Token_BCONST, pg.Token_XCONST:
			text = "?"
		default:
			continue
		}
		if end == start {
			return result
		}
		replacements = append(replacements, replacement{start, end, text})
	}
	if len(replacements) == 0 {
		return SanitizedSQL{Template: raw, Hash: result.Hash}
	}
	var builder strings.Builder
	builder.Grow(len(raw))
	last := 0
	for _, item := range replacements {
		if item.start < last {
			return result
		}
		builder.WriteString(raw[last:item.start])
		builder.WriteString(item.text)
		last = item.end
	}
	builder.WriteString(raw[last:])
	return SanitizedSQL{Template: builder.String(), Hash: result.Hash, Redacted: true}
}

// RedactArgs mendeskripsikan posisi dan tipe parameter tanpa nilainya. Nilai sensitif tidak pernah
// diformat.
func RedactArgs(args []any) []string {
	result := make([]string, 0, min(len(args), MaxLogArgs))
	for index, arg := range args {
		if index >= MaxLogArgs {
			result = append(result, fmt.Sprintf("... %d more arguments", len(args)-MaxLogArgs))
			break
		}
		result = append(result, fmt.Sprintf("$%d: %s", index+1, safeArgumentType(arg)))
	}
	return result
}

func safeArgumentType(value any) string {
	if value == nil {
		return "null"
	}
	return reflect.TypeOf(value).String()
}

// SafeError hanya memuat SQLSTATE dan kategori tetap. Message, Detail, Hint, Where, DSN, dan isi
// driver error tidak pernah dikembalikan.
type SafeError struct {
	Code string
	Kind string
}

// SanitizeError mengklasifikasikan error tanpa menyalin teksnya.
func SanitizeError(err error) SafeError {
	if err == nil {
		return SafeError{}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr != nil {
		return SafeError{Code: pgErr.Code, Kind: "sql"}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return SafeError{Code: "timeout", Kind: "timeout"}
	case errors.Is(err, context.Canceled):
		return SafeError{Code: "canceled", Kind: "canceled"}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return SafeError{Code: "connection", Kind: "connection"}
	}
	return SafeError{Code: "error", Kind: "unknown"}
}
