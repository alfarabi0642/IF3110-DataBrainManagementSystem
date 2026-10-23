package auth

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
)

func sessionFixture(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db := bootstrapFixture(t)
	secrets, err := secret.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal("cannot create session fixture secrets")
	}
	service, err := NewService(db.Pool, secrets, Policy{AppEnv: "test", AppOrigin: "http://localhost:8080"}, nil)
	if err != nil {
		t.Fatal("cannot create session fixture service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return service, ctx
}

func insertSessionUser(t *testing.T, service *Service, ctx context.Context, admin bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := service.pool.Exec(ctx, `INSERT INTO databrain.users
	    (id,username,display_name,password_hash,is_platform_admin)
	    VALUES ($1,$2,'Session User','fixture-not-an-account-hash',$3)`, id, "session_"+id.String()[:8], admin)
	if err != nil {
		t.Fatal("cannot insert session fixture user")
	}
	return id
}

func requireProblem(t *testing.T, err error, code problem.Code) {
	t.Helper()
	if !problemIs(err, code) {
		t.Fatalf("error = %v; want %s", err, code)
	}
}

func TestSessionDatabaseLifecycle(t *testing.T) {
	service, ctx := sessionFixture(t)
	userID := insertSessionUser(t, service, ctx, false)

	state, token, err := service.CreateAnonymous(ctx)
	if err != nil || state.Kind != AnonymousSession || state.ID == uuid.Nil || token == "" {
		t.Fatalf("anonymous session = %+v, token=%t, error=%v", state, token != "", err)
	}
	validated, replacement, err := service.Validate(ctx, token, false)
	if err != nil || validated.ID != state.ID || replacement != "" {
		t.Fatalf("anonymous validation = %+v, replacement=%q, error=%v", validated, replacement, err)
	}

	authenticated, original, err := service.create(ctx, AuthenticatedSession, userID, 1)
	if err != nil {
		t.Fatal("cannot create authenticated session")
	}
	if _, err := service.pool.Exec(ctx, `UPDATE databrain.sessions
	    SET last_rotated_at=clock_timestamp()-interval '15 minutes' WHERE id=$1`, authenticated.ID); err != nil {
		t.Fatal("cannot prepare session rotation")
	}

	type result struct {
		state Session
		token string
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			current, next, err := service.Validate(ctx, original, false)
			results <- result{current, next, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var canonical string
	for result := range results {
		if result.err != nil || result.state.ID != authenticated.ID || result.token == "" {
			t.Fatalf("concurrent rotation = %+v, token=%t, error=%v", result.state, result.token != "", result.err)
		}
		if canonical == "" {
			canonical = result.token
		} else if result.token != canonical {
			t.Fatal("concurrent requests produced different canonical tokens")
		}
	}
	var tokenCount int
	if err := service.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.session_tokens WHERE session_id=$1", authenticated.ID).Scan(&tokenCount); err != nil || tokenCount != 2 {
		t.Fatalf("token family count=%d error=%v", tokenCount, err)
	}
	if _, repaired, err := service.Validate(ctx, original, false); err != nil || repaired != canonical {
		t.Fatalf("alias repair token=%q error=%v", repaired, err)
	}

	if _, err := service.pool.Exec(ctx, "UPDATE databrain.sessions SET reauth_required=true WHERE id=$1", authenticated.ID); err != nil {
		t.Fatal("cannot prepare reauthentication")
	}
	reauth, _, err := service.Validate(ctx, canonical, false)
	if !errors.Is(err, ErrReauthRequired) || !reauth.ReauthRequired || reauth.Principal.UserID != userID {
		t.Fatalf("reauth validation = %+v, error=%v", reauth, err)
	}
	if _, _, err := service.Validate(ctx, canonical, true); err != nil {
		t.Fatalf("allow-reauth validation failed: %v", err)
	}
	if _, err := service.pool.Exec(ctx, "UPDATE databrain.sessions SET revoked_at=clock_timestamp() WHERE id=$1", authenticated.ID); err != nil {
		t.Fatal("cannot revoke session fixture")
	}
	_, _, err = service.Validate(ctx, canonical, true)
	requireProblem(t, err, problem.SessionInvalid)
}

func TestAuthorizerDatabaseContract(t *testing.T) {
	service, ctx := sessionFixture(t)
	userID := insertSessionUser(t, service, ctx, false)
	state, _, err := service.create(ctx, AuthenticatedSession, userID, 1)
	if err != nil {
		t.Fatal("cannot create authorizer session")
	}
	authorizer, err := access.NewAuthorizer(service.pool)
	if err != nil {
		t.Fatal("cannot create authorizer")
	}

	requireProblem(t, authorizer.RequireAdmin(ctx, state.Principal), problem.AdminRequired)
	targetID := uuid.New()
	_, err = service.pool.Exec(ctx, `INSERT INTO databrain.database_targets
	    (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
	    VALUES ($1,'Authorizer target','127.0.0.1',55432,$2,'fixture_role',$3,'disable',$4)`,
		targetID, "authorizer_"+targetID.String(), bytes.Repeat([]byte{0x24}, 30), userID)
	if err != nil {
		t.Fatal("cannot insert authorizer target")
	}
	_, err = authorizer.RequireDatabase(ctx, state.Principal, targetID, access.DatabaseView)
	requireProblem(t, err, problem.DatabaseNotFound)

	if _, err := service.pool.Exec(ctx, "UPDATE databrain.users SET is_platform_admin=true WHERE id=$1", userID); err != nil {
		t.Fatal("cannot promote authorizer fixture user")
	}
	if err := authorizer.RequireAdmin(ctx, state.Principal); err != nil {
		t.Fatalf("active admin rejected: %v", err)
	}
	_, err = authorizer.RequireDatabase(ctx, state.Principal, targetID, access.DatabaseView)
	requireProblem(t, err, problem.DatabaseNotFound)

	viewGroup, editGroup := uuid.New(), uuid.New()
	_, err = service.pool.Exec(ctx, "INSERT INTO databrain.groups(id,name) VALUES ($1,'Authorizer viewers'),($2,'Authorizer editors')", viewGroup, editGroup)
	if err != nil {
		t.Fatal("cannot insert authorizer groups")
	}
	if _, err := service.pool.Exec(ctx, "INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$3),($2,$3)", viewGroup, editGroup, userID); err != nil {
		t.Fatal("cannot insert authorizer memberships")
	}
	if _, err := service.pool.Exec(ctx, `INSERT INTO databrain.database_grants(group_id,target_id,access_level)
	    VALUES ($1,$2,'view_only')`, viewGroup, targetID); err != nil {
		t.Fatal("cannot insert authorizer view grant")
	}
	level, err := authorizer.RequireDatabase(ctx, state.Principal, targetID, access.DatabaseView)
	if err != nil || level != access.ViewOnly {
		t.Fatalf("view grant = %q, error=%v", level, err)
	}
	_, err = authorizer.RequireDatabase(ctx, state.Principal, targetID, access.MigrationWrite)
	requireProblem(t, err, problem.DatabaseAccessDenied)
	if _, err := service.pool.Exec(ctx, `INSERT INTO databrain.database_grants(group_id,target_id,access_level)
	    VALUES ($1,$2,'view_edit')`, editGroup, targetID); err != nil {
		t.Fatal("cannot insert edit grant")
	}
	level, err = authorizer.RequireDatabase(ctx, state.Principal, targetID, access.MigrationExecute)
	if err != nil || level != access.ViewEdit {
		t.Fatalf("mixed grant = %q, error=%v", level, err)
	}

	if _, err := service.pool.Exec(ctx, "UPDATE databrain.database_targets SET is_active=false WHERE id=$1", targetID); err != nil {
		t.Fatal("cannot deactivate target")
	}
	_, err = authorizer.RequireDatabase(ctx, state.Principal, targetID, access.DatabaseView)
	requireProblem(t, err, problem.DatabaseNotFound)
	if _, err := service.pool.Exec(ctx, "UPDATE databrain.database_targets SET is_active=true WHERE id=$1", targetID); err != nil {
		t.Fatal("cannot reactivate target")
	}
	if _, err := service.pool.Exec(ctx, "UPDATE databrain.sessions SET reauth_required=true WHERE id=$1", state.ID); err != nil {
		t.Fatal("cannot require reauthentication")
	}
	requireProblem(t, authorizer.RequireAdmin(ctx, state.Principal), problem.SessionInvalid)
	_, err = authorizer.RequireDatabase(ctx, state.Principal, targetID, access.DatabaseView)
	requireProblem(t, err, problem.SessionInvalid)
}
