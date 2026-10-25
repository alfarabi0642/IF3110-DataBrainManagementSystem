package explorer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func checkCode(t *testing.T, err error, code problem.Code) {
	t.Helper()
	var safe *problem.Error
	if !errors.As(err, &safe) || safe.Code != code || safe.Message != "" || err.Error() != string(code) {
		t.Fatalf("expected safe code %s, got %v", code, err)
	}
}

func TestCatalogSelection(t *testing.T) {
	e := New(nil) // Tolak pilihan tidak valid sebelum membuka target.
	for _, tc := range []struct {
		schema, table string
		code          problem.Code
	}{
		{"", "table", problem.InvalidRequest},
		{"public", "bad\x00name", problem.InvalidRequest},
		{"public", strings.Repeat("猫", 22), problem.InvalidRequest},
		{string([]byte{0xff}), "", problem.InvalidRequest},
		{"pg_catalog", "pg_class", problem.RelationNotFound},
		{"information_schema", "", problem.RelationNotFound},
		{"pg_toast", "", problem.RelationNotFound},
		{"pg_temp_1", "", problem.RelationNotFound},
		{"pg_toast_temp_1", "", problem.RelationNotFound},
	} {
		_, err := e.Catalog(context.Background(), uuid.New(), tc.schema, tc.table)
		checkCode(t, err, tc.code)
	}
	_, err := e.Catalog(context.Background(), uuid.Nil, "", "")
	checkCode(t, err, problem.InvalidRequest)
	for _, name := range []string{`a".猫`, `'; DROP TABLE anything;--`, strings.Repeat("a", 63), " "} {
		if !validName(name) || !userSchema(name) {
			t.Fatal("valid quoted identifier was rejected")
		}
	}
}

func TestCatalogErrorRedaction(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code problem.Code
	}{
		{pgx.ErrNoRows, problem.RelationNotFound},
		{&pgconn.PgError{Code: "42P01", Message: "SENSITIVE"}, problem.RelationNotFound},
		{&pgconn.PgError{Code: "3F000", Detail: "SENSITIVE"}, problem.RelationNotFound},
		{&pgconn.PgError{Code: "55P03", Message: "SENSITIVE"}, problem.RelationChanged},
		{&pgconn.PgError{Code: "57014", Message: "SENSITIVE"}, problem.RequestTimeout},
		{&pgconn.PgError{Code: "42501", Detail: "SENSITIVE"}, problem.TargetUnavailable},
		{errors.New("SENSITIVE"), problem.TargetUnavailable},
		{context.DeadlineExceeded, problem.RequestTimeout},
		{&problem.Error{Code: problem.ResultTooLarge, Message: "SENSITIVE"}, problem.ResultTooLarge},
	} {
		checkCode(t, catalogError(context.Background(), tc.err), tc.code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checkCode(t, catalogError(ctx, errors.New("SENSITIVE")), problem.RequestTimeout)
}
