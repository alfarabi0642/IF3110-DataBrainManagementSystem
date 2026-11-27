package explorer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type databaseDTO struct {
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	Access access.Level `json:"access"`
}

type explorerDTO struct {
	Database    databaseDTO  `json:"database"`
	Schema      string       `json:"schema"`
	Table       string       `json:"table"`
	Tab         string       `json:"tab"`
	Schemas     []string     `json:"schemas"`
	Tables      []Table      `json:"tables"`
	Columns     []Column     `json:"columns"`
	Constraints []Constraint `json:"constraints"`
	Indexes     []Index      `json:"indexes"`
	Data        *dataDTO     `json:"data"`
}

type dataDTO struct {
	Columns  []string `json:"columns"`
	Rows     [][]Cell `json:"rows"`
	Ordering string   `json:"ordering"`
}

// RegisterAPI memakai katalog dan query bounded yang sama tanpa dependency renderer.
func (e *Explorer) RegisterAPI(mux *http.ServeMux, middleware *auth.Middleware, pool *pgxpool.Pool, authorizer access.Authorizer) {
	mux.Handle("GET /api/databases/{database_id}/explorer", middleware.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		fail := func(err error) { httpx.WriteJSONError(w, r, err) }
		principal, ok := access.FromContext(ctx)
		if !ok {
			fail(failure(problem.SessionInvalid))
			return
		}
		id, err := httpx.UUIDValue(url.Values{"database_id": {r.PathValue("database_id")}}, "database_id")
		if err != nil {
			fail(err)
			return
		}
		// Otorisasi target mendahului validasi selection dan pembacaan metadata.
		level, err := authorizer.RequireDatabase(ctx, principal, id, access.DatabaseView)
		if err != nil {
			fail(err)
			return
		}
		s, err := parseSelection(r)
		if err != nil {
			fail(err)
			return
		}
		result := explorerDTO{Database: databaseDTO{ID: id.String(), Access: level}, Schema: s.schema, Table: s.table, Tab: s.tab,
			Schemas: []string{}, Tables: []Table{}, Columns: []Column{}, Constraints: []Constraint{}, Indexes: []Index{}}
		lease, err := platformdb.Acquire(ctx, pool)
		if err != nil {
			fail(failure(problem.PlatformUnavailable))
			return
		}
		err = lease.QueryRow(ctx, `SELECT name FROM databrain.database_targets WHERE id=$1 AND is_active`, id).Scan(&result.Database.Name)
		lease.Release()
		if errors.Is(err, pgx.ErrNoRows) {
			fail(failure(problem.DatabaseNotFound))
			return
		}
		if err != nil {
			fail(failure(problem.PlatformUnavailable))
			return
		}
		var page *httpx.Page
		if s.table != "" && s.tab == "data" {
			rows, err := e.Page(ctx, id, s.schema, s.table, s.limit, s.offset)
			if err != nil {
				fail(err)
				return
			}
			result.Data = &dataDTO{Columns: append([]string{}, rows.Columns...), Rows: append([][]Cell{}, rows.Rows...), Ordering: rows.Ordering}
			page = httpx.NewPage(rows.Limit, rows.Offset, rows.HasMore)
		} else {
			catalog, err := e.Catalog(ctx, id, s.schema, s.table)
			if err != nil {
				fail(err)
				return
			}
			result.Schemas = append(result.Schemas, catalog.Schemas...)
			result.Tables = append(result.Tables, catalog.Tables...)
			result.Columns = append(result.Columns, catalog.Columns...)
			result.Constraints = append(result.Constraints, catalog.Constraints...)
			result.Indexes = append(result.Indexes, catalog.Indexes...)
		}
		httpx.WriteJSON(w, r, http.StatusOK, result, page)
	})))
}
