// Package access defines the identity and authorization contracts shared by features.
// Session validation and grant checks are implemented by auth and the authorizer.
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
	SessionID       uuid.UUID // Internal session UUID, never the cookie token.
	SecurityVersion int64
}

type principalKey struct{}

// WithPrincipal is used by auth middleware only after validating the session.
// Storing a principal does not grant permissions or replace an Authorizer check.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext reports whether auth middleware installed a principal.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type Authorizer interface {
	RequireAdmin(ctx context.Context, p Principal) error
	RequireDatabase(ctx context.Context, p Principal, targetID uuid.UUID, action Action) (Level, error)
}
