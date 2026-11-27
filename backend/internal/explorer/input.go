package explorer

import (
	"net/http"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

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
