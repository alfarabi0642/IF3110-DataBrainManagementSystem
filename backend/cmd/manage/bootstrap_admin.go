package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
)

func runBootstrapAdmin(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	username := flags.String("username", "", "")
	displayName := flags.String("display-name", "", "")
	passwordFile := flags.String("password-file", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *username == "" || *displayName == "" || *passwordFile == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	password, err := readPasswordFile(*passwordFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	appEnv := os.Getenv("APP_ENV")
	if appEnv == "" {
		appEnv = "development"
	}
	switch appEnv {
	case "development", "test", "production":
	default:
		fmt.Fprintln(stderr, "APP_ENV must be development, test, or production")
		return 1
	}
	databaseURL, err := config.LoadPlatformDatabaseURL(appEnv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	db, err := platformdb.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_, bootstrapErr := auth.BootstrapAdmin(ctx, db.Pool, *username, *displayName, password)
	cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	closeErr := db.Close(cleanup)
	cancel()
	if bootstrapErr != nil {
		fmt.Fprintln(stderr, bootstrapErr)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintln(stderr, platformdb.ErrUnavailable)
		return 1
	}
	fmt.Fprintln(stdout, "Platform admin created.")
	return 0
}

func readPasswordFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read password file")
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return "", errors.New("cannot read password file")
	}
	password := string(value)
	if err := auth.ValidatePassword(password); err != nil {
		return "", err
	}
	return password, nil
}
