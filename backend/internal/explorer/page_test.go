package explorer

import (
	"context"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPageSelection(t *testing.T) {
	e := New(nil)
	for _, tc := range []struct {
		schema, table string
		limit, offset int
		code          problem.Code
	}{
		{"", "table", 50, 0, problem.InvalidRequest},
		{"public", "", 50, 0, problem.InvalidRequest},
		{"public", "bad\x00name", 50, 0, problem.InvalidRequest},
		{"public", strings.Repeat("猫", 22), 50, 0, problem.InvalidRequest},
		{string([]byte{0xff}), "table", 50, 0, problem.InvalidRequest},
		{"public", "table", 0, 0, problem.InvalidRequest},
		{"public", "table", 201, 0, problem.InvalidRequest},
		{"public", "table", 50, -1, problem.InvalidRequest},
		{"public", "table", 50, 100001, problem.InvalidRequest},
		{"pg_temp_1", "table", 50, 0, problem.RelationNotFound},
	} {
		_, err := e.Page(context.Background(), uuid.New(), tc.schema, tc.table, tc.limit, tc.offset)
		checkCode(t, err, tc.code)
	}
	_, err := e.Page(context.Background(), uuid.Nil, "public", "table", 50, 0)
	checkCode(t, err, problem.InvalidRequest)
}

func TestPageErrorRedaction(t *testing.T) {
	for _, tc := range []struct {
		state string
		code  problem.Code
	}{
		{"42846", problem.UnsupportedColumnType},
		{"22000", problem.UnsupportedColumnType},
		{"54000", problem.ResultTooLarge},
		{"42P01", problem.RelationNotFound},
		{"57014", problem.RequestTimeout},
	} {
		checkCode(t, rowValueError(context.Background(), &pgconn.PgError{Code: tc.state, Message: "SENSITIVE"}), tc.code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checkCode(t, rowValueError(ctx, &pgconn.PgError{Code: "22000", Message: "SENSITIVE"}), problem.RequestTimeout)
}
