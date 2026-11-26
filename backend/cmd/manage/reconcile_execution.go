package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/google/uuid"
)

const reconcileExecutionUsage = "Usage: manage reconcile-execution --execution UUID\n"

// runReconcileExecution menjalankan rekonsiliasi berbasis bukti target journal untuk satu eksekusi. Tidak ada flag force-success & hasil yang tidak dapat dibuktikan tetap diblokir.
func runReconcileExecution(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("reconcile-execution", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var rawID string
	flags.StringVar(&rawID, "execution", "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, reconcileExecutionUsage)
			return 0
		}
		fmt.Fprint(stderr, reconcileExecutionUsage)
		fmt.Fprintln(stderr, "Invalid arguments.")
		return 2
	}
	if flags.NArg() != 0 || rawID == "" {
		fmt.Fprint(stderr, reconcileExecutionUsage)
		fmt.Fprintln(stderr, "Execution UUID is required.")
		return 2
	}
	executionID, err := uuid.Parse(rawID)
	if err != nil || executionID == uuid.Nil {
		fmt.Fprint(stderr, reconcileExecutionUsage)
		fmt.Fprintln(stderr, "Invalid execution UUID.")
		return 2
	}

	appEnv := os.Getenv("APP_ENV")
	if appEnv == "" {
		appEnv = "development"
	}
	databaseURL, err := config.LoadPlatformDatabaseURL(appEnv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	key, err := config.LoadSecretKey()
	if err != nil {
		fmt.Fprintln(stderr, "Invalid application secret configuration.")
		return 1
	}
	secrets, err := secret.New(key)
	if err != nil {
		fmt.Fprintln(stderr, "Invalid application secret configuration.")
		return 1
	}
	targetSettings, err := config.LoadTargetSettings(appEnv)
	if err != nil {
		fmt.Fprintln(stderr, "Invalid target configuration.")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := platformdb.OpenPool(ctx, databaseURL)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		_ = pool.Close(closeCtx)
	}()

	provider, err := targets.NewProvider(pool.Pool, secrets, targetSettings)
	if err != nil {
		fmt.Fprintln(stderr, "Invalid target provider configuration.")
		return 1
	}
	result, err := migration.ReconcileExecution(ctx, pool.Pool, provider, key, executionID)
	if err != nil {
		fmt.Fprintf(stderr, "Reconciliation refused (evidence=%s): %v\n", result.Evidence, err)
		return 1
	}
	fmt.Fprintf(stdout, "Execution %s reconciled: evidence=%s phase=%s outcome=%s already_done=%t.\n",
		result.ExecutionID, result.Evidence, result.FinalPhase, result.Outcome, result.AlreadyDone)
	return 0
}
