package migration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/admin"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/explorer"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

func TestBrowserSPA(t *testing.T) {
	if os.Getenv("DATABRAIN_E2E") != "1" {
		t.Skip("browser suite dijalankan lewat npm run test:e2e")
	}
	if os.Getenv("DATABRAIN_TEST_ADMIN_URL") == "" {
		t.Fatal("browser suite memerlukan disposable PostgreSQL fixture")
	}
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	f, web := newWorkflowFixture(t, "http://"+server.Listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	hash, err := auth.HashPassword(ctx, "BrowserFixture123!")
	if err != nil {
		t.Fatal(err)
	}
	runnerSQL(t, f.store.pool, `UPDATE databrain.users SET password_hash=$2,is_platform_admin=true WHERE id=$1`, f.actor, hash)
	runnerSQL(t, f.store.pool, `UPDATE databrain.users SET password_hash=$2 WHERE id=$1`, f.viewer.UserID, hash)
	runnerSQL(t, f.target, `CREATE TABLE public.browser_cells(id int PRIMARY KEY,value text,precise numeric); INSERT INTO public.browser_cells VALUES(1,NULL,9007199254740993),(2,'',1),(3,'null',2); GRANT SELECT ON public.browser_cells TO databrain_target`)
	middleware, err := auth.NewMiddleware(web.service)
	if err != nil {
		t.Fatal(err)
	}
	apiMux := http.NewServeMux()
	middleware.RegisterSessionAPI(apiMux)
	provider := f.runner.provider
	provider.RegisterListAPI(apiMux, middleware)
	provider.RegisterAdminAPI(apiMux, middleware, f.guard)
	accounts, err := admin.NewService(f.store.pool, web.service)
	if err != nil {
		t.Fatal(err)
	}
	accounts.RegisterAPI(apiMux, middleware)
	explorer.New(provider).RegisterAPI(apiMux, middleware, f.store.pool, f.guard)
	migrations, err := NewService(f.store, f.runner, f.guard)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAPIHandler(migrations)
	if err != nil {
		t.Fatal(err)
	}
	handler.RegisterAPI(apiMux, middleware)
	mux := httpx.NewRouter(f.db.Ready)
	assets, err := httpx.MountStatic(mux, "../../web/static")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assets.Close() })
	app, root, err := httpx.SPA(mux, "../../../frontend/dist")
	if err != nil {
		t.Fatal("build frontend sebelum menjalankan browser suite")
	}
	t.Cleanup(func() { _ = root.Close() })
	api := httpx.APIHandler(apiMux)
	server.Config.Handler = httpx.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if httpx.IsAPI(r) {
			api.ServeHTTP(w, r)
			return
		}
		app.ServeHTTP(w, r)
	}))
	server.Start()
	// Token ini tetap melalui expiry check backend, tanpa test endpoint pada aplikasi.
	expired, token, err := web.service.CreateAnonymous(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runnerSQL(t, f.store.pool, `UPDATE databrain.sessions SET created_at=now()-interval '1 hour',absolute_expires_at=now()-interval '1 minute' WHERE id=$1`, expired.ID)
	script, err := filepath.Abs("../../../frontend/e2e/browser.mjs")
	if err != nil {
		t.Fatal(err)
	}
	node := os.Getenv("DATABRAIN_NODE")
	if node == "" {
		node = "node"
	}
	cmd := exec.CommandContext(ctx, node, script)
	cmd.Env = append(os.Environ(), "E2E_ORIGIN="+server.URL, "E2E_TARGET="+f.a.String(), "E2E_EXPIRED_TOKEN="+token)
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatal("browser assertions gagal")
	}
	var rows int
	if err := f.target.QueryRow(ctx, `SELECT count(*) FROM public.browser_committed`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("browser execute tidak menghasilkan satu commit")
	}
}
