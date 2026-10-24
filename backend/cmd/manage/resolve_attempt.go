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
	"github.com/google/uuid"
)

func runResolveAttempt(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("resolve-attempt", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	attempt := flags.String("attempt", "", "")
	outcome := flags.String("outcome", "", "")
	operator := flags.String("operator", "", "")
	path := flags.String("evidence-file", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *path == "" || *operator == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	id, err := uuid.Parse(*attempt)
	if err != nil || id == uuid.Nil || id.String() != *attempt || (*outcome != "succeeded" && *outcome != "failed") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	evidence, err := readEvidenceFile(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	input := migration.ResolutionInput{Outcome: *outcome, Operator: *operator, Evidence: evidence}
	if err := migration.ValidateResolution(input); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	appEnv := os.Getenv("APP_ENV")
	if appEnv == "" {
		appEnv = "development"
	}
	if appEnv != "development" && appEnv != "test" && appEnv != "production" {
		fmt.Fprintln(stderr, "APP_ENV must be development, test, or production")
		return 1
	}
	databaseURL, err := config.LoadPlatformDatabaseURL(appEnv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := platformdb.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	resolveErr := migration.ResolveAttempt(ctx, db, id, input)
	cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
	closeErr := db.Close(cleanup)
	stop()
	if resolveErr != nil {
		fmt.Fprintln(stderr, resolveErr)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintln(stderr, platformdb.ErrUnavailable)
		return 1
	}
	fmt.Fprintf(stdout, "Attempt %s resolved as %s.\n", id, input.Outcome)
	return 0
}

func readEvidenceFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read operator evidence file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, migration.MaxResolutionBytes+1))
	if err != nil {
		return "", errors.New("cannot read operator evidence file")
	}
	if err := migration.ValidateResolution(migration.ResolutionInput{Outcome: "failed", Operator: "operator", Evidence: string(data)}); err != nil {
		return "", err
	}
	return string(data), nil
}
