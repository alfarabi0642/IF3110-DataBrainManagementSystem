package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/telemetry"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/migrations"
)

const usage = "Usage: manage migrate up\n       manage bootstrap-admin --username NAME --display-name NAME --password-file PATH\n       manage resolve-attempt --attempt UUID --outcome succeeded|failed --operator NAME --evidence-file PATH\n       manage reconcile-execution --execution UUID\n"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) == 2 && args[0] == "migrate" && args[1] == "up" {
		shutdown := enableMigratorTelemetry()
		defer shutdown()
		databaseURL, err := config.LoadMigratorDatabaseURL()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		result, err := migrations.Up(databaseURL)
		if err != nil {
			if result.HasVersion {
				fmt.Fprintf(stderr, "Platform schema version=%d dirty=%t.\n", result.Version, result.Dirty)
			}
			fmt.Fprintln(stderr, err)
			return 1
		}
		queue, err := jobs.Migrate(databaseURL)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "Platform schema version=%d dirty=%t no_change=%t.\n", result.Version, result.Dirty, result.NoChange)
		fmt.Fprintf(stdout, "Queue schema version=%d no_change=%t.\n", queue.Version, queue.NoChange)
		return 0
	}
	if len(args) > 0 && args[0] == "bootstrap-admin" {
		return runBootstrapAdmin(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "resolve-attempt" {
		return runResolveAttempt(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "reconcile-execution" {
		return runReconcileExecution(args[1:], stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	if len(args) != 0 {
		// Jangan mencetak argumen mentah karena dapat berisi rahasia.
		fmt.Fprintln(stderr, "Unknown command.")
	}
	return 2
}

// enableMigratorTelemetry remains opt-in so successful operator commands keep their quiet stderr
// contract. DATABRAIN_TELEMETRY=console exports the sanitized otelsql migration spans and JSON logs.
func enableMigratorTelemetry() func() {
	if os.Getenv("DATABRAIN_TELEMETRY") != "console" {
		return func() {}
	}
	logger := telemetry.NewLogger("databrain-manage")
	slog.SetDefault(logger)
	tracer, err := telemetry.NewTracerProvider("databrain-manage", logger)
	if err != nil {
		logger.Warn("migrator telemetry unavailable", "error_kind", "init_failed")
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracer.Shutdown(ctx); err != nil {
			logger.Warn("migrator telemetry shutdown failed", "error_kind", "shutdown_failed")
		}
	}
}
