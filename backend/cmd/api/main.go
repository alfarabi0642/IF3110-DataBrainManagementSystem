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
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		// Config errors name only the field/rule; network errors stay private.
		logger.Error("HTTP server stopped", "code", "HTTP_SERVER_FAILED")
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid HTTP configuration", "reason", err.Error())
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	logger.Info("HTTP server listening; platform unavailable")
	return serveHTTP(ctx, newServer(cfg), listener)
}

func newServer(cfg config.Config) *http.Server {
	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpx.NewRouter(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func serveHTTP(ctx context.Context, server *http.Server, listener net.Listener) error {
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
	case <-ctx.Done():
		shutdown, cancelShutdown := context.WithTimeout(context.Background(), 70*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdown); err != nil {
			cancelRequests()
			_ = server.Close()
			<-stopped
			return err
		}
		err := <-stopped
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
