package access

import (
	"context"
	"errors"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestAuthorizerRejectsMissingDependenciesAndPrincipal(t *testing.T) {
	if authorizer, err := NewAuthorizer(nil); authorizer != nil || err == nil {
		t.Fatal("nil platform pool accepted")
	}
	if validPrincipal(Principal{}) {
		t.Fatal("empty principal accepted")
	}
	if !validPrincipal(Principal{UserID: uuid.New(), SessionID: uuid.New(), SecurityVersion: 1}) {
		t.Fatal("complete principal rejected")
	}
}

type adminCheck struct {
	platformdb.DBTX
	valid, admin bool
	err          error
}

func (db adminCheck) QueryRow(context.Context, string, ...any) pgx.Row { return db }
func (db adminCheck) Scan(dest ...any) error {
	*(dest[0].(*bool)), *(dest[1].(*bool)) = db.valid, db.admin
	return db.err
}

func TestRequireAdminInFailsClosed(t *testing.T) {
	p := Principal{UserID: uuid.New(), SessionID: uuid.New(), SecurityVersion: 1}
	for _, test := range []struct {
		db   platformdb.DBTX
		p    Principal
		want problem.Code
	}{
		{nil, Principal{}, problem.SessionInvalid},
		{nil, p, problem.InternalError},
		{adminCheck{}, p, problem.SessionInvalid},
		{adminCheck{valid: true}, p, problem.AdminRequired},
		{adminCheck{valid: true, admin: true}, p, ""},
		{adminCheck{valid: true, admin: true, err: errors.New("PRIVATE")}, p, problem.PlatformUnavailable},
	} {
		err := RequireAdminIn(context.Background(), test.db, test.p)
		if test.want == "" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var value *problem.Error
		if !errors.As(err, &value) || value.Code != test.want {
			t.Fatalf("error=%v; want %s", err, test.want)
		}
	}
}
