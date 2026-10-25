// Package migrations menerapkan schema platform secara terpisah dari SQL pengguna.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Version menunjukkan versi schema platform yang didukung build ini.
const Version uint = 1

//go:embed *.up.sql
var files embed.FS

type Result struct {
	Version    uint
	HasVersion bool
	Dirty      bool
	NoChange   bool
}

// Up tidak mereset atau memaksa versi. Error menyembunyikan detail driver yang dapat memuat credential
// atau SQL lengkap.
func Up(databaseURL string) (Result, error) {
	return up(databaseURL, files)
}

func up(databaseURL string, files fs.FS) (result Result, err error) {
	source, err := iofs.New(files, ".")
	if err != nil {
		return result, errors.New("cannot read embedded platform migrations")
	}
	defer source.Close()
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return result, errors.New("cannot open migration database")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, errors.New("cannot connect to migration database")
	}
	defer conn.Close()
	driver, err := postgres.WithConnection(ctx, conn, &postgres.Config{
		SchemaName: "public", MigrationsTable: "schema_migrations",
		StatementTimeout: 10 * time.Second,
	})
	if err != nil {
		return result, errors.New("cannot initialize platform migration metadata")
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return result, errors.New("cannot initialize platform migrations")
	}
	defer func() {
		sourceErr, databaseErr := m.Close()
		if err == nil && (sourceErr != nil || databaseErr != nil) {
			err = errors.New("migration cleanup failed; check the recorded version before retrying")
		}
	}()
	m.LockTimeout = 5 * time.Second
	version, dirty, versionErr := m.Version()
	if versionErr == nil {
		result = Result{Version: version, HasVersion: true, Dirty: dirty}
		if dirty {
			return result, errors.New("platform schema is dirty; inspect and repair it manually")
		}
		if version > Version {
			return result, errors.New("platform schema is newer than this build")
		}
	} else if !errors.Is(versionErr, migrate.ErrNilVersion) {
		return result, errors.New("cannot read platform schema version")
	}
	upErr := m.Up()
	version, dirty, versionErr = m.Version()
	if versionErr != nil {
		return Result{}, errors.New("cannot confirm platform schema version after migration")
	}
	result = Result{Version: version, HasVersion: true, Dirty: dirty, NoChange: errors.Is(upErr, migrate.ErrNoChange)}
	if upErr != nil && !result.NoChange {
		return result, errors.New("platform migration failed; inspect the recorded version and dirty state before manual repair")
	}
	if dirty || version != Version {
		return result, errors.New("platform schema did not reach the supported clean version")
	}
	return result, nil
}
