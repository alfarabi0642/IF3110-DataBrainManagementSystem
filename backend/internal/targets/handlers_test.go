package targets

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/google/uuid"
)

func TestHandlerInputAndPagination(t *testing.T) {
	for _, query := range []string{"limit=0", "limit=201", "offset=-1", "offset=100001", "limit=", "limit=1&limit=2", "actor=x", "limit=%zz", "limit=+1"} {
		if _, _, err := pagination(httptest.NewRequest("GET", "/databases?"+query, nil)); err == nil {
			t.Errorf("accepted query %q", query)
		}
	}
	limit, offset, err := pagination(httptest.NewRequest("GET", "/databases", nil))
	if err != nil || limit != 50 || offset != 0 {
		t.Fatal("pagination defaults differ from the contract")
	}
}

type handlerSession struct {
	id          uuid.UUID
	token, csrf string
}

func seedHandlerSession(t *testing.T, f targetFixture, service *auth.Service, user uuid.UUID) handlerSession {
	t.Helper()
	ctx := context.Background()
	state, token, err := service.CreateAnonymous(ctx)
	if err != nil {
		t.Fatal("cannot seed handler session token")
	}
	// Session fixture dibuat langsung hanya pada database terisolasi agar test tidak bergantung pada alur
	// login.
	if _, err := f.p.pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, user); err != nil {
		t.Fatal("cannot seed authenticated handler session")
	}
	if _, _, err := service.Validate(ctx, token, false); err != nil {
		t.Fatal("seeded session did not pass the real validator")
	}
	return handlerSession{id: state.ID, token: token, csrf: state.CSRF}
}
