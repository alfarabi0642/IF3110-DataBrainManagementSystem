package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
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

func TestRowPageRenderLimit(t *testing.T) {
	renderer, err := httpx.LoadTemplates("../../web/templates")
	if err != nil || renderer == nil {
		t.Fatal("real explorer template unavailable")
	}
	data := map[string]any{"Columns": []string{"value"}, "Rows": [][]Cell{{{Null: true}}, {{Text: "null"}}, {{Text: ""}}, {{Text: "<script>"}}}, "Ordering": "unspecified", "Prev": "", "Next": ""}
	page := map[string]any{"Title": "Rows", "User": nil, "Database": nil, "Schema": "public", "Table": "rows", "Tab": "data", "Tabs": nil, "AllHref": "", "SchemaHref": "", "Data": data}
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := renderer.Render(w, request, 200, "explorer", page); err != nil {
		t.Fatal("cannot render row cells with real template")
	}
	for _, expected := range []string{`<span class="null">NULL</span>`, `(empty)`, `&lt;script&gt;`, `>null</td>`} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Fatal("NULL, literal text, empty string or escaped text lost in rendering")
		}
	}
	// A wide page of NULLs has zero raw text bytes but can exceed the HTML cap.
	columns, row := make([]string, 600), make([]Cell, 600)
	for i := range columns {
		columns[i], row[i] = "column", Cell{Null: true}
	}
	rows := make([][]Cell, 200)
	for i := range rows {
		rows[i] = row
	}
	data["Columns"], data["Rows"] = columns, rows
	w = httptest.NewRecorder()
	checkCode(t, renderer.Render(w, request, 200, "explorer", page), problem.ResultTooLarge)
	if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
		t.Fatal("oversized HTML sent a partial response")
	}
}
