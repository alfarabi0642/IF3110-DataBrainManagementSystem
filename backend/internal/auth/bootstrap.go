package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidUsername      = errors.New("username must match ^[a-z][a-z0-9_]{2,31}$")
	ErrInvalidDisplayName   = errors.New("display name must contain 1 to 80 characters")
	ErrBootstrapAdminExists = errors.New("an active platform admin already exists")
	ErrBootstrapFailed      = errors.New("bootstrap admin failed")

	usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)
)

func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return ErrInvalidUsername
	}
	return nil
}

func NormalizeLoginUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

func NormalizeDisplayName(displayName string) (string, error) {
	displayName = strings.TrimSpace(displayName)
	if !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) < 1 ||
		utf8.RuneCountInString(displayName) > 80 || strings.ContainsRune(displayName, 0) {
		return "", ErrInvalidDisplayName
	}
	return displayName, nil
}

// BootstrapAdmin creates the first active platform admin. The password is
// hashed before the transaction acquires the global security lock.
func BootstrapAdmin(ctx context.Context, pool *pgxpool.Pool, username, displayName, password string) (uuid.UUID, error) {
	if err := ValidateUsername(username); err != nil {
		return uuid.Nil, ErrInvalidUsername
	}
	var err error
	displayName, err = NormalizeDisplayName(displayName)
	if err != nil {
		return uuid.Nil, ErrInvalidDisplayName
	}
	passwordHash, err := HashPassword(ctx, password)
	if err != nil {
		return uuid.Nil, err
	}
	if pool == nil {
		return uuid.Nil, ErrBootstrapFailed
	}
	userID, err := uuid.NewRandom()
	if err != nil {
		return uuid.Nil, ErrBootstrapFailed
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, ErrBootstrapFailed
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var guard bool
	if err := tx.QueryRow(ctx, `SELECT singleton FROM databrain.security_guard WHERE singleton FOR UPDATE`).Scan(&guard); err != nil || !guard {
		return uuid.Nil, ErrBootstrapFailed
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
	    SELECT 1 FROM databrain.users WHERE is_platform_admin AND is_active
	)`).Scan(&exists); err != nil {
		return uuid.Nil, ErrBootstrapFailed
	}
	if exists {
		return uuid.Nil, ErrBootstrapAdminExists
	}
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.users
	    (id,username,display_name,password_hash,is_platform_admin)
	    VALUES ($1,$2,$3,$4,true)`, userID, username, displayName, passwordHash); err != nil {
		return uuid.Nil, ErrBootstrapFailed
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, ErrBootstrapFailed
	}
	return userID, nil
}
