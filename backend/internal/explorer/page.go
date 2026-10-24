package explorer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxCellBytes = 64 << 10
	maxPageBytes = 512 << 10
)

// Cell preserves PostgreSQL's text output, including empty strings and numeric
// precision. Null distinguishes SQL NULL from the text "null" or "".
type Cell struct {
	Null bool
	Text string
}

type RowPage struct {
	Table    Table
	Columns  []string
	Rows     [][]Cell
	Ordering string // primary_key_asc or unspecified; no snapshot across requests.
	Limit    int
	Offset   int
	HasMore  bool
}

// Page reads at most limit rows plus a lookahead used only for HasMore. Callers
// must authorize the target first and render with httpx's bounded HTML renderer.
func (e *Explorer) Page(ctx context.Context, targetID uuid.UUID, schema, table string, limit, offset int) (RowPage, error) {
	if targetID == uuid.Nil || !validName(schema) || !validName(table) || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return RowPage{}, failure(problem.InvalidRequest)
	}
	if !userSchema(schema) {
		return RowPage{}, failure(problem.RelationNotFound)
	}
	var result RowPage
	err := e.read(ctx, targetID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = readPage(ctx, tx, schema, table, limit, offset)
		return err
	})
	if err != nil {
		return RowPage{}, err
	}
	return result, nil
}

func readPage(ctx context.Context, tx pgx.Tx, schema, table string, limit, offset int) (RowPage, error) {
	var oid uint32
	var kind string
	if err := resolveTable(ctx, tx, schema, table, &oid, &kind); err != nil {
		return RowPage{}, err
	}
	if err := lockTable(ctx, tx, schema, table, oid); err != nil {
		return RowPage{}, err
	}
	var metadata Catalog
	remaining := maxMetadataBytes
	if err := readColumns(ctx, tx, oid, &remaining, &metadata); err != nil {
		return RowPage{}, err
	}
	keys, err := primaryKey(ctx, tx, oid)
	if err != nil {
		return RowPage{}, err
	}
	result := RowPage{Table: Table{Schema: schema, Name: table, Kind: kind}, Limit: limit, Offset: offset, Ordering: "unspecified"}
	if len(keys) > 0 {
		result.Ordering = "primary_key_asc"
	}
	for _, column := range metadata.Columns {
		result.Columns = append(result.Columns, column.Name)
	}
	rows, err := tx.Query(ctx, pageSQL(schema, table, result.Columns, keys), limit+1, offset, limit, maxCellBytes, maxPageBytes)
	if err != nil {
		return RowPage{}, rowValueError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var position int64
		var values []*string
		if err := rows.Scan(&status, &result.HasMore, &position, &values); err != nil {
			return RowPage{}, rowValueError(ctx, err)
		}
		switch status {
		case string(problem.CellTooLarge):
			return RowPage{}, failure(problem.CellTooLarge)
		case string(problem.ResultTooLarge):
			return RowPage{}, failure(problem.ResultTooLarge)
		}
		if position == 0 { // Empty page sentinel; a real all-NULL row has a position.
			continue
		}
		if len(values) != len(result.Columns) {
			return RowPage{}, failure(problem.RelationChanged)
		}
		cells := make([]Cell, len(values))
		for i, value := range values {
			cells[i].Null = value == nil
			if value != nil {
				cells[i].Text = *value
			}
		}
		result.Rows = append(result.Rows, cells)
	}
	if err := rows.Err(); err != nil {
		return RowPage{}, rowValueError(ctx, err)
	}
	return result, nil
}

func primaryKey(ctx context.Context, tx pgx.Tx, oid uint32) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT a.attname::text FROM pg_catalog.pg_index i
	    CROSS JOIN LATERAL pg_catalog.unnest(i.indkey) WITH ORDINALITY k(num,pos)
	    JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.num
	    WHERE i.indrelid=$1 AND i.indisprimary AND k.pos<=i.indnkeyatts ORDER BY k.pos`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		keys = append(keys, name)
	}
	return keys, rows.Err()
}

// Only catalog identifiers enter this SQL, each quoted separately. Pagination
// and limits are bound values. Text arrays preserve NULLs without JSON/number
// conversions. MATERIALIZED keeps casts on the target until the budget passes.
func pageSQL(schema, table string, columns, keys []string) string {
	casts := make([]string, len(columns))
	for i, name := range columns {
		casts[i] = pgx.Identifier{"r", name}.Sanitize() + "::pg_catalog.text"
	}
	order := ""
	if len(keys) > 0 {
		parts := make([]string, len(keys))
		for i, name := range keys {
			parts[i] = pgx.Identifier{"r", name}.Sanitize() + " ASC"
		}
		order = "ORDER BY " + strings.Join(parts, ",")
	}
	return fmt.Sprintf(`WITH page AS MATERIALIZED (
	    SELECT row_number() OVER (%s) AS pos,ARRAY[%s]::pg_catalog.text[] AS cells
	    FROM %s AS r %s LIMIT $1 OFFSET $2
	), sizes AS (
	    SELECT p.pos,coalesce(max(octet_length(c.value)),0) AS max_cell,
	        coalesce(sum(octet_length(c.value)),0) AS bytes
	    FROM page p LEFT JOIN LATERAL pg_catalog.unnest(p.cells) c(value) ON true GROUP BY p.pos
	), budget AS MATERIALIZED (
	    SELECT count(*)>$3 AS has_more,
	        coalesce(max(max_cell) FILTER (WHERE pos<=$2+$3),0)>$4 AS cell_large,
	        coalesce(sum(bytes) FILTER (WHERE pos<=$2+$3),0)>$5 AS page_large FROM sizes
	)
	SELECT CASE WHEN cell_large THEN 'CELL_TOO_LARGE' WHEN page_large THEN 'RESULT_TOO_LARGE' ELSE '' END AS status,
	    has_more,0::bigint AS pos,NULL::pg_catalog.text[] AS cells
	FROM budget WHERE cell_large OR page_large OR NOT EXISTS(SELECT 1 FROM page)
	UNION ALL
	SELECT '',b.has_more,p.pos,p.cells FROM page p CROSS JOIN budget b
	WHERE NOT b.cell_large AND NOT b.page_large AND p.pos<=$2+$3
	ORDER BY pos`, order, strings.Join(casts, ","), pgx.Identifier{schema, table}.Sanitize(), order)
}

func rowValueError(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if strings.HasPrefix(pgErr.Code, "22") || pgErr.Code == "42846" || pgErr.Code == "42883" || pgErr.Code == "42804" {
				return failure(problem.UnsupportedColumnType)
			}
			if pgErr.Code == "54000" {
				return failure(problem.ResultTooLarge)
			}
		}
	}
	return catalogError(ctx, err)
}
