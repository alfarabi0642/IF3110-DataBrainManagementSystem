package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func lifecycleTx(t *testing.T, service *Service, ctx context.Context) pgx.Tx {
	t.Helper()
	tx, err := service.pool.Begin(ctx)
	if err != nil {
		t.Fatal("cannot begin lifecycle transaction")
	}
	t.Cleanup(func() { rollbackAuth(tx) })
	return tx
}

func authenticatedFixture(t *testing.T, service *Service, ctx context.Context, user uuid.UUID) (Session, string) {
	t.Helper()
	state, token, err := service.create(ctx, AuthenticatedSession, user, 1)
	if err != nil {
		t.Fatal("cannot create lifecycle session")
	}
	return state, token
}

func execLifecycle(t *testing.T, db platformdb.DBTX, ctx context.Context, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, query, args...); err != nil {
		t.Fatal("cannot prepare/check lifecycle fixture:", err)
	}
}

func TestRevokeSessionTransactionAndAliases(t *testing.T) {
	service, ctx := sessionFixture(t)
	user := insertSessionUser(t, service, ctx, false)
	state, alias := authenticatedFixture(t, service, ctx, user)
	_, otherToken := authenticatedFixture(t, service, ctx, user)
	execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET last_rotated_at=clock_timestamp()-interval '16 minutes' WHERE id=$1", state.ID)
	_, canonical, err := service.Validate(ctx, alias, false)
	if err != nil || canonical == "" {
		t.Fatal("cannot prepare rotation")
	}
	tx := lifecycleTx(t, service, ctx)
	if err := service.RevokeSession(ctx, tx, state.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Validate(ctx, canonical, false); err != nil {
		t.Fatal("revocation escaped rollback")
	}
	tx = lifecycleTx(t, service, ctx)
	if err := service.RevokeSession(ctx, tx, state.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{alias, canonical} {
		_, _, err := service.Validate(ctx, token, true)
		requireProblem(t, err, problem.SessionInvalid)
	}
	if _, _, err := service.Validate(ctx, otherToken, false); err != nil {
		t.Fatal("logout revoked another family")
	}
	var before, after time.Time
	if err := service.pool.QueryRow(ctx, "SELECT revoked_at FROM databrain.sessions WHERE id=$1", state.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx = lifecycleTx(t, service, ctx)
	if err := service.RevokeSession(ctx, tx, state.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.pool.QueryRow(ctx, "SELECT revoked_at FROM databrain.sessions WHERE id=$1", state.ID).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatal("repeated revoke changed retention time")
	}
}

func TestReauthenticateAndRevokeUsersAtomically(t *testing.T) {
	service, ctx := sessionFixture(t)
	user, other := insertSessionUser(t, service, ctx, false), insertSessionUser(t, service, ctx, false)
	first, oldToken := authenticatedFixture(t, service, ctx, user)
	second, secondToken := authenticatedFixture(t, service, ctx, user)
	_, otherToken := authenticatedFixture(t, service, ctx, other)
	var lastSeen time.Time
	if err := service.pool.QueryRow(ctx, "SELECT last_seen_at FROM databrain.sessions WHERE id=$1", first.ID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	tx := lifecycleTx(t, service, ctx)
	execLifecycle(t, tx, ctx, "SELECT singleton FROM databrain.security_guard WHERE singleton FOR UPDATE")
	execLifecycle(t, tx, ctx, "UPDATE databrain.users SET is_platform_admin=true WHERE id=$1", user)
	if err := service.ReauthenticateUsers(ctx, tx, []uuid.UUID{user, user}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, replacement, err := service.Validate(ctx, oldToken, false); err != nil || replacement != "" {
		t.Fatal("reauth escaped rollback")
	}
	if err := service.pool.QueryRow(ctx, "SELECT last_seen_at FROM databrain.sessions WHERE id=$1", first.ID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	tx = lifecycleTx(t, service, ctx)
	if err := service.ReauthenticateUsers(ctx, tx, []uuid.UUID{user, user}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var expires, seen time.Time
	var version, count int
	if err := service.pool.QueryRow(ctx, `SELECT absolute_expires_at,last_seen_at,
	    (SELECT security_version FROM databrain.users WHERE id=$2),
	    (SELECT count(*) FROM databrain.session_tokens WHERE session_id=$1)
	    FROM databrain.sessions WHERE id=$1`, first.ID, user).Scan(&expires, &seen, &version, &count); err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(first.AbsoluteExpiresAt) || !seen.Equal(lastSeen) || version != 1 || count != 2 {
		t.Fatal("reauth extended lifetime, changed version, or rotated duplicate IDs twice")
	}
	_, canonical, err := service.Validate(ctx, oldToken, false)
	if !errors.Is(err, ErrReauthRequired) || canonical == "" {
		t.Fatal("old alias gained protected access or lost repair")
	}
	if _, _, err := service.Validate(ctx, canonical, true); err != nil {
		t.Fatal("reauth cannot log in/logout")
	}
	if _, _, err := service.Validate(ctx, secondToken, false); !errors.Is(err, ErrReauthRequired) {
		t.Fatal("second family not affected")
	}
	if _, _, err := service.Validate(ctx, otherToken, false); err != nil {
		t.Fatal("unrelated user affected")
	}
	tx = lifecycleTx(t, service, ctx)
	if err := service.RevokeUsers(ctx, tx, []uuid.UUID{user, user}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Validate(ctx, canonical, true); err != nil {
		t.Fatal("bulk revoke escaped rollback")
	}
	tx = lifecycleTx(t, service, ctx)
	if err := service.RevokeUsers(ctx, tx, []uuid.UUID{user, user}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{oldToken, canonical, secondToken} {
		_, _, err := service.Validate(ctx, token, true)
		requireProblem(t, err, problem.SessionInvalid)
	}
	if err := service.pool.QueryRow(ctx, "SELECT security_version FROM databrain.users WHERE id=$1", user).Scan(&version); err != nil || version != 2 {
		t.Fatal("security version not incremented exactly once")
	}
	tx = lifecycleTx(t, service, ctx)
	if err := service.ReauthenticateUsers(ctx, tx, []uuid.UUID{user}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	if err := service.pool.QueryRow(ctx, "SELECT revoked_at IS NOT NULL FROM databrain.sessions WHERE id=$1", second.ID).Scan(&revoked); err != nil || !revoked {
		t.Fatal("reauth revived a revoked session")
	}
}

func TestCleanupBoundedAndCanonicalSafe(t *testing.T) {
	service, ctx := sessionFixture(t)
	user := insertSessionUser(t, service, ctx, false)
	live, token := authenticatedFixture(t, service, ctx, user)
	anonymous, _, err := service.CreateAnonymous(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recent, _ := authenticatedFixture(t, service, ctx, user)
	old, _ := authenticatedFixture(t, service, ctx, user)
	execLifecycle(t, service.pool, ctx, `UPDATE databrain.sessions SET created_at=clock_timestamp()-interval '1 hour',
	    absolute_expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, anonymous.ID)
	execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET revoked_at=clock_timestamp() WHERE id=$1", recent.ID)
	execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET revoked_at=clock_timestamp()-interval '8 days' WHERE id=$1", old.ID)
	execLifecycle(t, service.pool, ctx, `INSERT INTO databrain.session_tokens(token_hash,session_id,valid_until)
	    SELECT decode(md5('cleanup'||g)||md5('alias'||g),'hex'),$1,clock_timestamp()-interval '1 minute'
	    FROM generate_series(1,$2::int) g`, live.ID, cleanupBatchSize+5)
	execLifecycle(t, service.pool, ctx, `INSERT INTO databrain.login_windows(key_type,key_hash,window_start,attempts)
	    SELECT 'username',decode(md5('cleanup'||g)||md5('window'||g),'hex'),clock_timestamp()-interval '31 minutes',1
	    FROM generate_series(1,$1::int) g`, cleanupBatchSize+5)
	if err := service.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var aliases, windows, sessions int
	if err := service.pool.QueryRow(ctx, `SELECT
	    (SELECT count(*) FROM databrain.session_tokens WHERE session_id=$1),
	    (SELECT count(*) FROM databrain.login_windows),
	    (SELECT count(*) FROM databrain.sessions)`, live.ID).Scan(&aliases, &windows, &sessions); err != nil {
		t.Fatal(err)
	}
	if aliases != 6 || windows != 5 || sessions != 2 {
		t.Fatalf("cleanup counts: tokens=%d windows=%d sessions=%d", aliases, windows, sessions)
	}
	if err := service.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Validate(ctx, token, false); err != nil {
		t.Fatal("cleanup removed a live canonical token")
	}
	if err := service.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.session_tokens").Scan(&aliases); err != nil || aliases != 2 {
		t.Fatal("cleanup damaged retained canonical tokens or failed to cascade old families")
	}
}

func TestReauthenticationDoesNotReviveInvalidFamilies(t *testing.T) {
	service, ctx := sessionFixture(t)
	user := insertSessionUser(t, service, ctx, false)
	var ids []uuid.UUID
	for _, condition := range []string{
		"revoked_at=clock_timestamp()",
		"last_seen_at=clock_timestamp()-interval '31 minutes'",
		"created_at=clock_timestamp()-interval '2 hours',absolute_expires_at=clock_timestamp()-interval '1 minute'",
		"security_version=2",
	} {
		state, _ := authenticatedFixture(t, service, ctx, user)
		ids = append(ids, state.ID)
		execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET "+condition+" WHERE id=$1", state.ID)
	}
	tx := lifecycleTx(t, service, ctx)
	if err := service.ReauthenticateUsers(ctx, tx, []uuid.UUID{user}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var tokens, reauth int
	if err := service.pool.QueryRow(ctx, `SELECT
	    (SELECT count(*) FROM databrain.session_tokens WHERE session_id=ANY($1::uuid[])),
	    (SELECT count(*) FROM databrain.sessions WHERE id=ANY($1::uuid[]) AND reauth_required)`, ids).Scan(&tokens, &reauth); err != nil {
		t.Fatal(err)
	}
	if tokens != len(ids) || reauth != 0 {
		t.Fatal("invalid families were rotated or marked live")
	}
}

func TestCleanupRetentionAndLockedFamilies(t *testing.T) {
	service, ctx := sessionFixture(t)
	user := insertSessionUser(t, service, ctx, false)
	oldAbsolute, _ := authenticatedFixture(t, service, ctx, user)
	oldIdle, _ := authenticatedFixture(t, service, ctx, user)
	recentExpired, _ := authenticatedFixture(t, service, ctx, user)
	execLifecycle(t, service.pool, ctx, `UPDATE databrain.sessions SET created_at=clock_timestamp()-interval '9 days',
	    absolute_expires_at=clock_timestamp()-interval '8 days' WHERE id=$1`, oldAbsolute.ID)
	execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET last_seen_at=clock_timestamp()-interval '8 days' WHERE id=$1", oldIdle.ID)
	execLifecycle(t, service.pool, ctx, `UPDATE databrain.sessions SET created_at=clock_timestamp()-interval '2 days',
	    absolute_expires_at=clock_timestamp()-interval '1 day' WHERE id=$1`, recentExpired.ID)
	tx := lifecycleTx(t, service, ctx)
	execLifecycle(t, tx, ctx, "SELECT id FROM databrain.sessions WHERE id=$1 FOR UPDATE", oldAbsolute.ID)
	if err := service.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := service.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.sessions").Scan(&count); err != nil || count != 2 {
		t.Fatal("cleanup removed a locked/recent family or retained an old idle family")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.sessions WHERE id=$1", recentExpired.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("recently expired family lost its retention period")
	}
}

func TestValidationRechecksAfterConcurrentRevoke(t *testing.T) {
	service, ctx := sessionFixture(t)
	user := insertSessionUser(t, service, ctx, false)
	state, token := authenticatedFixture(t, service, ctx, user)
	execLifecycle(t, service.pool, ctx, "UPDATE databrain.sessions SET last_rotated_at=clock_timestamp()-interval '16 minutes' WHERE id=$1", state.ID)
	tx := lifecycleTx(t, service, ctx)
	if err := service.RevokeSession(ctx, tx, state.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, _, err := service.Validate(ctx, token, false); result <- err }()
	waitForFamilyLock(t, ctx, tx, result)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	requireProblem(t, <-result, problem.SessionInvalid)
	var count int
	if err := service.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.session_tokens WHERE session_id=$1", state.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("revoked waiter issued a successor")
	}
}

func TestValidationRechecksTokenExpiryAfterLockWait(t *testing.T) {
	service, ctx := sessionFixture(t)
	user := insertSessionUser(t, service, ctx, false)
	state, token := authenticatedFixture(t, service, ctx, user)
	tx := lifecycleTx(t, service, ctx)
	execLifecycle(t, tx, ctx, "SELECT id FROM databrain.sessions WHERE id=$1 FOR UPDATE", state.ID)
	raw, err := decodeToken(token)
	if err != nil {
		t.Fatal(err)
	}
	hash := tokenHash(raw)
	execLifecycle(t, tx, ctx, "UPDATE databrain.session_tokens SET valid_until=clock_timestamp()-interval '1 second' WHERE token_hash=$1", hash[:])
	result := make(chan error, 1)
	go func() { _, _, err := service.Validate(ctx, token, false); result <- err }()
	waitForFamilyLock(t, ctx, tx, result)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	requireProblem(t, <-result, problem.SessionInvalid)
}

func waitForFamilyLock(t *testing.T, ctx context.Context, tx pgx.Tx, result <-chan error) {
	t.Helper()
	// Wait for a real database lock waiter, not a timing guess.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
		    WHERE locktype='transactionid' AND NOT granted AND transactionid=(pg_current_xact_id()::text)::xid)`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("validation did not wait for family lock: %v", err)
		case <-ctx.Done():
			t.Fatal("validation did not reach family lock")
		case <-ticker.C:
		}
	}
}
