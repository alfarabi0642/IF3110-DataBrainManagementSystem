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
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/telemetry"
)

// forcedStopGrace membatasi handler yang mengabaikan pembatalan context dan shutdown server
// kesehatan setelah drain.
const forcedStopGrace = 5 * time.Second

func main() {
	logger := telemetry.NewLogger(telemetry.ServiceWorker)
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		// Pesan konfigurasi hanya menyebut field dan aturan; detail koneksi tetap privat.
		logger.Error("worker stopped", "code", "WORKER_FAILED")
		os.Exit(1)
	}
}

func run(logger *slog.Logger) (err error) {
	settings, err := config.LoadWorkerSettings()
	if err != nil {
		logger.Error("invalid worker configuration", "reason", err.Error())
		return err
	}
	databaseURL, err := config.LoadPlatformDatabaseURL(settings.AppEnv)
	if err != nil {
		logger.Error("invalid platform configuration", "reason", err.Error())
		return err
	}
	key, err := config.LoadSecretKey()
	if err != nil {
		logger.Error("invalid application secret configuration")
		return err
	}
	secrets, err := secret.New(key)
	if err != nil {
		return err
	}
	targetSettings, err := config.LoadTargetSettings(settings.AppEnv)
	if err != nil {
		logger.Error("invalid target configuration")
		return err
	}
	lifecycle, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tracer, tracerErr := telemetry.NewTracerProvider(telemetry.ServiceWorker, logger)
	if tracerErr != nil {
		logger.Error("telemetry initialization failed", "code", "TELEMETRY_INIT_FAILED")
	} else {
		defer func() {
			if shutdownErr := telemetry.Shutdown(tracer); shutdownErr != nil {
				logger.Error("telemetry shutdown incomplete", "code", "TELEMETRY_SHUTDOWN_FAILED")
			}
		}()
	}

	pool, err := platformdb.OpenPool(lifecycle, databaseURL)
	if err != nil {
		logger.Error("worker platform startup failed", "reason", err.Error())
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := pool.Close(cleanup); closeErr != nil {
			logger.Error("worker platform shutdown incomplete")
			if err == nil {
				err = closeErr
			}
		}
	}()
	if !jobs.Ready(lifecycle, pool.Pool) {
		logger.Error("queue schema is missing; run manage migrate up with the migrator credential", "code", "QUEUE_SCHEMA_MISSING")
		return jobs.ErrQueueSchema
	}
	provider, err := targets.NewProvider(pool.Pool, secrets, targetSettings)
	if err != nil {
		return err
	}
	executor, err := migration.NewWorkerExecutor(pool.Pool, provider, key, logger)
	if err != nil {
		return err
	}
	client, err := jobs.NewWorker(pool.Pool, settings, jobs.NewWorkersWithExecution(executor.HandleJob), logger)
	if err != nil {
		logger.Error("invalid worker limits")
		return err
	}
	listener, err := net.Listen("tcp", settings.HealthAddr)
	if err != nil {
		return err
	}
	health := &http.Server{
		Handler: httpx.NewRouter(func(ctx context.Context) bool {
			select {
			case <-lifecycle.Done():
				return false
			case <-client.Stopped():
				return false
			default:
			}
			return pool.Ready(ctx) && jobs.Ready(ctx, pool.Pool)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	defer health.Close()
	// Context start yang dibatalkan memulai soft stop River; SoftStopTimeout membatalkan job yang
	// melewati batas drain.
	startCtx, cancelStart := context.WithCancel(lifecycle)
	defer cancelStart()
	if err := client.Start(startCtx); err != nil {
		logger.Error("queue worker startup failed", "code", "QUEUE_START_FAILED")
		return err
	}
	served := make(chan error, 1)
	go func() { served <- health.Serve(listener) }()
	logger.Info("queue worker listening", "addr", settings.HealthAddr, "concurrency", settings.Concurrency)

	var runErr error
	select {
	case <-lifecycle.Done():
		logger.Info("queue worker draining", "timeout", settings.DrainTimeout)
	case serveErr := <-served:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("worker health server stopped")
			runErr = serveErr
		}
	}
	cancelStart()
	if drainErr := drainWorker(client, settings.DrainTimeout); drainErr != nil {
		logger.Error("queue worker drain did not finish", "code", "WORKER_DRAIN_FAILED")
		if runErr == nil {
			runErr = drainErr
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), forcedStopGrace)
	defer cancel()
	if shutdownErr := health.Shutdown(shutdown); shutdownErr != nil {
		logger.Error("worker health shutdown incomplete")
		if runErr == nil {
			runErr = shutdownErr
		}
	}
	return runErr
}

// drainWorker menunggu soft stop River setelah context start dibatalkan. Job yang masih berjalan pada
// batas drain dibatalkan agar proses tidak menggantung; outcome target tetap menjadi tanggung jawab
// worker pengganti.
func drainWorker(client riverClient, drain time.Duration) error {
	deadline := time.NewTimer(drain)
	defer deadline.Stop()
	select {
	case <-client.Stopped():
		return nil
	case <-deadline.C:
	}
	force, cancel := context.WithTimeout(context.Background(), forcedStopGrace)
	defer cancel()
	return client.StopAndCancel(force)
}

type riverClient interface {
	Stopped() <-chan struct{}
	StopAndCancel(context.Context) error
}
