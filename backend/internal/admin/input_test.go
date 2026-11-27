package admin

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func requireAdminCode(t *testing.T, err error, code problem.Code) {
	t.Helper()
	var value *problem.Error
	if !errors.As(err, &value) || value.Code != code {
		t.Fatalf("error = %v; want %s", err, code)
	}
}

func TestAdminListPagination(t *testing.T) {
	for _, test := range []struct {
		query         string
		limit, offset int
	}{
		{"", 50, 0},
		{"?limit=200&offset=100000", 200, 100000},
		{"?offset=25", 50, 25},
	} {
		limit, offset, err := parsePagination(httptest.NewRequest("GET", "/admin/users"+test.query, nil))
		if err != nil || limit != test.limit || offset != test.offset {
			t.Fatalf("pagination %q = %d,%d,%v", test.query, limit, offset, err)
		}
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?offset=-1", "?limit=1&limit=2", "?search=x"} {
		_, _, err := parsePagination(httptest.NewRequest("GET", "/admin/users"+query, nil))
		requireAdminCode(t, err, problem.InvalidRequest)
	}
}
