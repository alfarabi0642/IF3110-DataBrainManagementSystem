package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/admin"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/explorer"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		// Pesan konfigurasi hanya menyebut field dan aturan; detail jaringan tetap privat.
		logger.Error("HTTP server stopped", "code", "HTTP_SERVER_FAILED")
		os.Exit(1)
	}
}

func run(logger *slog.Logger) (err error) {
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid HTTP configuration", "reason", err.Error())
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	key, err := config.LoadSecretKey()
	if err != nil {
		logger.Error("invalid application secret configuration")
		return err
	}
	secrets, err := secret.New(key)
	if err != nil {
		return err
	}
	targetSettings, err := config.LoadTargetSettings(cfg.AppEnv)
	if err != nil {
		logger.Error("invalid target configuration")
		return err
	}
	databaseURL, err := config.LoadPlatformDatabaseURL(cfg.AppEnv)
	if err != nil {
		logger.Error("invalid platform configuration", "reason", err.Error())
		return err
	}
	db, err := platformdb.Open(ctx, databaseURL)
	if err != nil {
		logger.Error("platform startup failed", "reason", err.Error())
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := db.Close(cleanup); closeErr != nil {
			logger.Error("platform shutdown incomplete")
			if err == nil {
				err = closeErr
			}
		}
	}()
	if _, err := migration.RecoverInterrupted(ctx, db); err != nil {
		logger.Error("migration startup recovery failed; HTTP not started")
		return err
	}
	mux := httpx.NewRouter(db.Ready)
	provider, err := targets.NewProvider(db.Pool, secrets, targetSettings)
	if err != nil {
		return err
	}
	sessions, err := auth.NewService(db.Pool, secrets, auth.Policy{
		AppEnv: cfg.AppEnv, AppOrigin: cfg.AppOrigin, TrustedProxyCIDRs: cfg.TrustedProxyCIDRs,
	}, nil)
	if err != nil {
		return err
	}
	middleware, err := auth.NewMiddleware(sessions)
	if err != nil {
		return err
	}
	apiMux := http.NewServeMux()
	middleware.RegisterSessionAPI(apiMux)
	provider.RegisterListAPI(apiMux, middleware)
	authorizer, err := access.NewAuthorizer(db.Pool)
	if err != nil {
		return err
	}
	adminService, err := admin.NewService(db.Pool, sessions)
	if err != nil {
		return err
	}
	adminService.RegisterAPI(apiMux, middleware)
	provider.RegisterAdminAPI(apiMux, middleware, authorizer)
	explorerService := explorer.New(provider)
	explorerService.RegisterAPI(apiMux, middleware, db.Pool, authorizer)
	runner, err := migration.NewRunner(migration.NewAttemptStore(db.Pool), provider, authorizer)
	if err != nil {
		return err
	}
	draftHandler, err := migration.NewAPIHandler(migration.NewDraftStore(db.Pool), runner, authorizer)
	if err != nil {
		return err
	}
	draftHandler.RegisterAPI(apiMux, middleware)
	assets, err := httpx.MountStatic(mux, cfg.StaticDir)
	if err != nil || assets == nil {
		logger.Error("unable to open static directory")
		if err == nil {
			err = os.ErrNotExist
		}
		return err
	}
	defer assets.Close()
	application, frontend, err := httpx.SPA(mux, cfg.SPADir)
	if err != nil {
		logger.Error("unable to open frontend build")
		return err
	}
	if frontend != nil {
		defer frontend.Close()
	}
	api := httpx.APIHandler(apiMux)
	routes := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if httpx.IsAPI(r) {
			api.ServeHTTP(w, r)
			return
		}
		application.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	cleanupCtx, cancelCleanup := context.WithCancel(ctx)
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		runSessionCleanup(cleanupCtx, time.Minute, sessions.Cleanup, logger)
	}()
	defer func() {
		cancelCleanup()
		<-cleanupDone
	}()
	logger.Info("HTTP server listening; platform ready")
	return serveHTTP(ctx, newServer(cfg, httpx.Middleware(logger, routes)), listener, db.Lost())
}

func runSessionCleanup(ctx context.Context, interval time.Duration, cleanup func(context.Context) error, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cleanup(ctx); err != nil {
				logger.Error("session cleanup failed", "code", "SESSION_CLEANUP_FAILED")
			}
		}
	}
}

func newServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func serveHTTP(ctx context.Context, server *http.Server, listener net.Listener, lockLost <-chan struct{}) error {
	// Penghentian server menunggu request aktif selesai. Context request tetap hidup sampai selesai atau
	// batas shutdown menutup koneksi.
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server.BaseContext = func(net.Listener) context.Context { return requests }

	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()

	select {
	case err := <-stopped:
		return err
	case <-lockLost:
		cancelRequests()
		_ = server.Close()
		<-stopped
		return platformdb.ErrLockLost
	case <-ctx.Done():
		// Hilangnya process lock tetap fatal selama server menunggu request selesai.
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-lockLost:
				cancelRequests()
				_ = server.Close()
			case <-watchDone:
			}
		}()
		shutdown, cancelShutdown := context.WithTimeout(context.Background(), 70*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdown); err != nil {
			cancelRequests()
			_ = server.Close()
			<-stopped
			return err
		}
		err := <-stopped
		select {
		case <-lockLost:
			return platformdb.ErrLockLost
		default:
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
