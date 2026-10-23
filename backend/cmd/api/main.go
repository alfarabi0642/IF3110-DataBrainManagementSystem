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

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		// Config errors name only the field/rule; network errors stay private.
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

	// Validate UI sources at startup when supplied. Page handlers will receive
	// the renderer when their real templates and auth dependencies are available.
	if _, err := httpx.LoadTemplates("web/templates"); err != nil {
		logger.Error("unable to load HTML templates")
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
	mux := httpx.NewRouter(db.Ready)
	assets, err := httpx.MountStatic(mux, "web/static")
	if err != nil {
		logger.Error("unable to open static directory")
		return err
	}
	if assets != nil {
		defer assets.Close()
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	logger.Info("HTTP server listening; platform ready")
	return serveHTTP(ctx, newServer(cfg, httpx.Middleware(logger, mux)), listener, db.Lost())
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
	// Signal cancellation starts draining; active requests retain their context
	// until they finish or the shutdown deadline forces connections closed.
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
		// Lock loss remains fatal while an ordinary shutdown is draining.
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
