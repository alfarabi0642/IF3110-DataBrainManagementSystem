package migration

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
)

type draftSession struct {
	id          uuid.UUID
	token, csrf string
}

type draftHTTPFixture struct {
	app                                                 http.Handler
	service                                             *auth.Service
	origin                                              string
	editor, otherEditor, viewer, none, admin, anonymous draftSession
}

func newWorkflowFixture(t *testing.T, origin string) (runnerFixture, draftHTTPFixture) {
	t.Helper()
	f := newRunnerFixture(t)
	secrets, err := secret.New(bytes.Repeat([]byte{0x82}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(f.store.pool, secrets, auth.Policy{AppEnv: "test", AppOrigin: origin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := auth.NewMiddleware(service)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := NewService(f.store, f.runner, f.guard)
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIHandler(domain)
	if err != nil {
		t.Fatal(err)
	}
	mux := httpx.NewRouter(f.db.Ready)
	api.RegisterAPI(mux, middleware)
	seed := func(user uuid.UUID) draftSession {
		state, token, err := service.CreateAnonymous(context.Background())
		if err != nil {
			t.Fatal("cannot initialize HTTP workflow session")
		}
		if user != uuid.Nil {
			runnerSQL(t, f.store.pool, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, user)
		}
		return draftSession{state.ID, token, state.CSRF}
	}
	return f, draftHTTPFixture{
		app: httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), httpx.APIHandler(mux)), service: service, origin: origin,
		editor: seed(f.actor), otherEditor: seed(f.otherActor), viewer: seed(f.viewer.UserID), none: seed(f.none.UserID), admin: seed(f.admin.UserID), anonymous: seed(uuid.Nil)}
}
