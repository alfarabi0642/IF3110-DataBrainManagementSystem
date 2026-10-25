// Package access mendefinisikan principal dan kontrak otorisasi. Validasi session dilakukan oleh auth;
// pemeriksaan grant dilakukan oleh authorizer.
package access

import (
	"context"

	"github.com/google/uuid"
)

type Level string

const (
	ViewOnly Level = "view_only"
	ViewEdit Level = "view_edit"
)

type Action string

const (
	DatabaseView     Action = "database.view"
	MigrationWrite   Action = "migration.write"
	MigrationExecute Action = "migration.execute"
)

type Principal struct {
	UserID          uuid.UUID
	SessionID       uuid.UUID // UUID session internal, bukan token cookie.
	SecurityVersion int64
}

type principalKey struct{}

// WithPrincipal dipakai middleware setelah session valid. Principal dalam context tetap memerlukan
// pemeriksaan Authorizer.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext memeriksa apakah middleware sudah memasang principal.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type Authorizer interface {
	RequireAdmin(ctx context.Context, p Principal) error
	RequireDatabase(ctx context.Context, p Principal, targetID uuid.UUID, action Action) (Level, error)
}
