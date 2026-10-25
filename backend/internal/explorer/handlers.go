package explorer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ObjectLink struct{ Name, Href string }
type TabLink struct {
	Label, Href string
	Current     bool
}
type DataPage struct {
	Columns              []string
	Rows                 [][]Cell
	Ordering, Prev, Next string
}
type ConstraintInfo struct{ Kind, Columns, Definition string }

// ExplorerPage berisi data tampilan tanpa metadata koneksi. Link dibangun dari path tetap dan nilai
// yang di-encode.
type ExplorerPage struct {
	httpx.Layout
	Schema, Table, Tab, AllHref, SchemaHref string
	Schemas, Tables                         []ObjectLink
	Tabs                                    []TabLink
	Data                                    *DataPage
	ColumnInfo                              []Column
	Constraints                             []ConstraintInfo
	Indexes                                 []Index
}

type Handler struct {
	explorer   *Explorer
	pool       *pgxpool.Pool
	authorizer access.Authorizer
	renderer   *httpx.Renderer
}

func NewHandler(explorer *Explorer, pool *pgxpool.Pool, authorizer access.Authorizer, renderer *httpx.Renderer) (*Handler, error) {
	if explorer == nil || explorer.provider == nil || pool == nil || authorizer == nil ||
		!renderer.HasTemplate("explorer") || !renderer.HasTemplate("error") {
		return nil, failure(problem.InternalError)
	}
	return &Handler{explorer: explorer, pool: pool, authorizer: authorizer, renderer: renderer}, nil
}

func (h *Handler) Register(mux *http.ServeMux, middleware *auth.Middleware) {
	mux.Handle("GET /databases/{database_id}/explorer", middleware.RequireSession(http.HandlerFunc(h.show)))
}

type selection struct {
	schema, table, tab string
	limit, offset      int
}

func parseSelection(r *http.Request) (selection, error) {
	s := selection{tab: "data", limit: 50}
	values, err := httpx.ParseQuery(r, "schema", "table", "tab", "limit", "offset")
	if err != nil {
		return s, err
	}
	s.schema, s.table = values.Get("schema"), values.Get("table")
	if values.Has("schema") && !validName(s.schema) || values.Has("table") && (!validName(s.table) || s.schema == "") {
		return s, failure(problem.InvalidRequest)
	}
	if values.Has("tab") {
		s.tab = values.Get("tab")
	}
	switch s.tab {
	case "data":
	case "columns", "constraints", "indexes":
		if s.table == "" || values.Has("limit") || values.Has("offset") {
			return s, failure(problem.InvalidRequest)
		}
	default:
		return s, failure(problem.InvalidRequest)
	}
	if values.Has("limit") {
		value, err := httpx.Int64Value(values, "limit", 1, 200)
		if err != nil {
			return s, err
		}
		s.limit = int(value)
	}
	if values.Has("offset") {
		value, err := httpx.Int64Value(values, "offset", 0, 100000)
		if err != nil {
			return s, err
		}
		s.offset = int(value)
	}
	return s, nil
}

func explorerHref(path, schema, table, tab string, limit, offset int) string {
	values := url.Values{}
	if schema != "" {
		values.Set("schema", schema)
	}
	if table != "" {
		values.Set("table", table)
		values.Set("tab", tab)
	}
	if tab == "data" {
		values.Set("limit", strconv.Itoa(limit))
		if offset > 0 {
			values.Set("offset", strconv.Itoa(offset))
		}
	}
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

func (h *Handler) show(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	layout := httpx.Layout{Title: "Explorer", Active: "explorer"}
	fail := func(err error) {
		var p *problem.Error
		if errors.As(err, &p) && p.Code == problem.SessionInvalid {
			httpx.WriteError(w, r, err)
			return
		}
		h.renderer.RenderError(w, r, layout, err)
	}
	state, ok := auth.SessionFromContext(ctx)
	principal, present := access.FromContext(ctx)
	if !ok || !present || state.Kind != auth.AuthenticatedSession || state.Principal != principal {
		fail(failure(problem.SessionInvalid))
		return
	}
	layout.CSRF = state.CSRF
	id, err := httpx.UUIDValue(url.Values{"database_id": {r.PathValue("database_id")}}, "database_id")
	if err != nil {
		fail(err)
		return
	}
	level, err := h.authorizer.RequireDatabase(ctx, principal, id, access.DatabaseView)
	if err != nil {
		fail(err)
		return
	}
	s, err := parseSelection(r)
	if err != nil {
		fail(err)
		return
	}
	// Baca hanya field tampilan yang diperlukan layout setelah pemeriksaan akses.
	lease, err := platformdb.Acquire(ctx, h.pool)
	if err != nil {
		fail(failure(problem.PlatformUnavailable))
		return
	}
	user := &httpx.LayoutUser{}
	database := &httpx.LayoutDatabase{ID: id.String(), CanEdit: level == access.ViewEdit}
	err = lease.QueryRow(ctx, `SELECT u.display_name,u.username,u.is_platform_admin,t.name
	    FROM databrain.users u CROSS JOIN databrain.database_targets t
	    WHERE u.id=$1 AND u.is_active AND u.security_version=$2 AND t.id=$3 AND t.is_active`,
		principal.UserID, principal.SecurityVersion, id).Scan(&user.DisplayName, &user.Username, &user.IsAdmin, &database.Name)
	lease.Release()
	if errors.Is(err, pgx.ErrNoRows) {
		fail(failure(problem.DatabaseNotFound))
		return
	}
	if err != nil {
		fail(failure(problem.PlatformUnavailable))
		return
	}
	layout.User, layout.Database = user, database
	path := "/databases/" + id.String() + "/explorer"
	page := ExplorerPage{Layout: layout, Schema: s.schema, Table: s.table, Tab: s.tab,
		AllHref:    explorerHref(path, "", "", "data", s.limit, 0),
		SchemaHref: explorerHref(path, s.schema, "", "data", s.limit, 0)}
	if s.table != "" && s.tab == "data" {
		rows, err := h.explorer.Page(ctx, id, s.schema, s.table, s.limit, s.offset)
		if err != nil {
			fail(err)
			return
		}
		data := &DataPage{Columns: rows.Columns, Rows: rows.Rows,
			Ordering: "Ordered by primary key ascending. Pages may change between requests."}
		if rows.Ordering == "unspecified" {
			data.Ordering = "Row order is unspecified (no primary key). Pages may change between requests."
		}
		if s.offset > 0 {
			data.Prev = explorerHref(path, s.schema, s.table, "data", s.limit, max(0, s.offset-s.limit))
		}
		if rows.HasMore && s.offset+s.limit <= 100000 {
			data.Next = explorerHref(path, s.schema, s.table, "data", s.limit, s.offset+s.limit)
		}
		page.Data = data
	} else {
		catalog, err := h.explorer.Catalog(ctx, id, s.schema, s.table)
		if err != nil {
			fail(err)
			return
		}
		for _, name := range catalog.Schemas {
			page.Schemas = append(page.Schemas, ObjectLink{name, explorerHref(path, name, "", "data", s.limit, 0)})
		}
		for _, table := range catalog.Tables {
			page.Tables = append(page.Tables, ObjectLink{table.Name, explorerHref(path, s.schema, table.Name, "data", s.limit, 0)})
		}
		page.ColumnInfo, page.Indexes = catalog.Columns, catalog.Indexes
		for i := range page.ColumnInfo {
			column := &page.ColumnInfo[i]
			column.Identity = map[string]string{"a": "always", "d": "by default"}[column.Identity]
			column.Generated = map[string]string{"s": "stored"}[column.Generated]
		}
		for _, constraint := range catalog.Constraints {
			kind := map[string]string{"p": "Primary key", "f": "Foreign key", "u": "Unique", "c": "Check", "x": "Exclusion", "t": "Constraint trigger"}[constraint.Kind]
			if kind == "" {
				kind = constraint.Kind
			}
			page.Constraints = append(page.Constraints, ConstraintInfo{kind, strings.Join(constraint.Columns, ", "), constraint.Definition})
		}
	}
	if s.table != "" {
		for _, tab := range []struct{ value, label string }{{"data", "Data"}, {"columns", "Columns"}, {"constraints", "Constraints"}, {"indexes", "Indexes"}} {
			page.Tabs = append(page.Tabs, TabLink{Label: tab.label, Current: tab.value == s.tab,
				Href: explorerHref(path, s.schema, s.table, tab.value, s.limit, 0)})
		}
	}
	if err := h.renderer.Render(w, r, http.StatusOK, "explorer", page); err != nil {
		fail(err)
	}
}
