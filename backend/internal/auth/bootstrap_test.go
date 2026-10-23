package auth

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestBootstrapAdminValidatesBeforeDatabase(t *testing.T) {
	for _, tt := range []struct {
		name, username, displayName, password string
		wantErr                               error
	}{
		{"username", "Admin", "Admin", "Valid-test-password", ErrInvalidUsername},
		{"display name empty", "admin", " \t", "Valid-test-password", ErrInvalidDisplayName},
		{"display name invalid UTF-8", "admin", "Admin\xff", "Valid-test-password", ErrInvalidDisplayName},
		{"display name too long", "admin", strings.Repeat("界", 81), "Valid-test-password", ErrInvalidDisplayName},
		{"password", "admin", "Admin", "short", ErrInvalidPassword},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id, err := BootstrapAdmin(context.Background(), nil, tt.username, tt.displayName, tt.password)
			if id != uuid.Nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("id=%v error=%v; want %v", id, err, tt.wantErr)
			}
		})
	}
}

func bootstrapFixture(t *testing.T) *platformdb.DB {
	t.Helper()
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	if raw == "" {
		t.Skip("set DATABRAIN_TEST_ADMIN_URL for the B-01 bootstrap integration tests")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.User == nil ||
		u.User.Username() != "databrain_fixture_owner" || u.Path != "/postgres" {
		t.Fatal("integration tests require the repository fixture owner on 127.0.0.1:55432/postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("cannot connect to fixture owner")
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	var directory, role, database string
	if err := owner.QueryRow(ctx, "SELECT current_setting('data_directory'),current_user,current_database()").Scan(&directory, &role, &database); err != nil {
		t.Fatal("cannot inspect fixture identity")
	}
	expected, err := filepath.Abs("../../../.tmp/postgres/data")
	if err != nil || !strings.EqualFold(filepath.Clean(directory), filepath.Clean(expected)) || role != "databrain_fixture_owner" || database != "postgres" {
		t.Fatal("fixture directory or identity does not match this repository")
	}
	name := "databrain_bootstrap_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER databrain_migrator"); err != nil {
		t.Fatal("cannot create isolated bootstrap test database")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var actualOwner string
		if err := owner.QueryRow(cleanup, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", name).Scan(&actualOwner); err != nil || actualOwner != "databrain_migrator" {
			t.Error("cleanup refused: bootstrap database ownership changed")
			return
		}
		if _, err := owner.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
			t.Error("could not drop isolated bootstrap test database")
		}
	})
	if _, err := owner.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC; GRANT CONNECT ON DATABASE "+quoted+" TO databrain_runtime"); err != nil {
		t.Fatal("cannot isolate bootstrap test database")
	}
	u.Path = "/" + name
	u.User = url.UserPassword("databrain_migrator", os.Getenv("DATABRAIN_MIGRATOR_PASSWORD"))
	if _, err := migrations.Up(u.String()); err != nil {
		t.Fatal("bootstrap fixture migration failed")
	}
	u.User = url.UserPassword("databrain_runtime", os.Getenv("DATABRAIN_RUNTIME_PASSWORD"))
	db, err := platformdb.Open(ctx, u.String())
	if err != nil {
		t.Fatal("cannot open bootstrap runtime pool")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := db.Close(cleanup); err != nil {
			t.Error("cannot close bootstrap runtime pool")
		}
	})
	return db
}

func TestBootstrapAdminFirstOnly(t *testing.T) {
	db := bootstrapFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := BootstrapAdmin(ctx, db.Pool, "first_admin", "  First Admin  ", "Valid-test-password")
	if err != nil || id == uuid.Nil {
		t.Fatalf("first bootstrap failed: id=%v error=%v", id, err)
	}
	var username, displayName string
	var admin, active bool
	var revision int
	var securityVersion int64
	if err := db.Pool.QueryRow(ctx, `SELECT username,display_name,is_platform_admin,is_active,revision,security_version
	    FROM databrain.users WHERE id=$1`, id).Scan(&username, &displayName, &admin, &active, &revision, &securityVersion); err != nil {
		t.Fatal("cannot read bootstrapped admin")
	}
	if username != "first_admin" || displayName != "First Admin" || !admin || !active || revision != 1 || securityVersion != 1 {
		t.Fatal("bootstrapped admin fields differ from the contract")
	}
	if second, err := BootstrapAdmin(ctx, db.Pool, "second_admin", "Second Admin", "Valid-test-password"); second != uuid.Nil || !errors.Is(err, ErrBootstrapAdminExists) {
		t.Fatalf("second bootstrap was not rejected: id=%v error=%v", second, err)
	}
}

func TestBootstrapAdminConcurrent(t *testing.T) {
	db := bootstrapFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, username := range []string{"admin_one", "admin_two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := BootstrapAdmin(ctx, db.Pool, username, "Concurrent Admin", "Valid-test-password")
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	var succeeded, rejected int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrBootstrapAdminExists):
			rejected++
		default:
			t.Fatalf("unexpected concurrent bootstrap error: %v", err)
		}
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM databrain.users WHERE is_platform_admin AND is_active`).Scan(&count); err != nil || succeeded != 1 || rejected != 1 || count != 1 {
		t.Fatalf("succeeded=%d rejected=%d active admins=%d error=%v", succeeded, rejected, count, err)
	}
}

func TestBootstrapAdminFailureRollsBack(t *testing.T) {
	db := bootstrapFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO databrain.users (id,username,display_name,password_hash)
	    VALUES ($1,'existing_user','Existing User','synthetic')`, uuid.New()); err != nil {
		t.Fatal("cannot prepare duplicate username")
	}
	if id, err := BootstrapAdmin(ctx, db.Pool, "existing_user", "New Admin", "Valid-test-password"); id != uuid.Nil || !errors.Is(err, ErrBootstrapFailed) {
		t.Fatalf("duplicate bootstrap result: id=%v error=%v", id, err)
	}
	var admins int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM databrain.users WHERE is_platform_admin AND is_active`).Scan(&admins); err != nil || admins != 0 {
		t.Fatalf("failed bootstrap left an active admin: count=%d error=%v", admins, err)
	}
}
