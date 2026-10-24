package admin

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func adminFixture(t *testing.T) (*Service, access.Principal) {
	t.Helper()
	raw := os.Getenv("DATABRAIN_TEST_ADMIN_URL")
	if raw == "" {
		t.Skip("set DATABRAIN_TEST_ADMIN_URL for admin PostgreSQL integration tests")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.User == nil ||
		u.User.Username() != "databrain_fixture_owner" || u.Path != "/postgres" {
		t.Fatal("admin tests require the repository fixture owner on 127.0.0.1:55432/postgres")
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
	if err != nil || filepath.Clean(directory) != filepath.Clean(expected) || role != "databrain_fixture_owner" || database != "postgres" {
		t.Fatal("fixture directory or identity does not match this repository")
	}
	name := "databrain_admin_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER databrain_migrator"); err != nil {
		t.Fatal("cannot create isolated admin test database")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var actualOwner string
		if err := owner.QueryRow(cleanup, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", name).Scan(&actualOwner); err != nil || actualOwner != "databrain_migrator" {
			t.Error("cleanup refused: admin test database ownership changed")
			return
		}
		if _, err := owner.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
			t.Error("cannot drop isolated admin test database")
		}
	})
	if _, err := owner.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC; GRANT CONNECT ON DATABASE "+quoted+" TO databrain_runtime"); err != nil {
		t.Fatal("cannot isolate admin test database")
	}
	u.Path = "/" + name
	u.User = url.UserPassword("databrain_migrator", os.Getenv("DATABRAIN_MIGRATOR_PASSWORD"))
	if _, err := migrations.Up(u.String()); err != nil {
		t.Fatal("admin fixture migration failed")
	}
	u.User = url.UserPassword("databrain_runtime", os.Getenv("DATABRAIN_RUNTIME_PASSWORD"))
	db, err := platformdb.Open(ctx, u.String())
	if err != nil {
		t.Fatal("cannot open admin runtime pool")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := db.Close(cleanup); err != nil {
			t.Error("cannot close admin runtime pool")
		}
	})
	id, err := auth.BootstrapAdmin(ctx, db.Pool, "admin", "Fixture admin", "Fixture-admin-password")
	if err != nil {
		t.Fatal("cannot bootstrap fixture admin")
	}
	secrets, err := secret.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewService(db.Pool, secrets, auth.Policy{AppEnv: "test", AppOrigin: "http://localhost:8080"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := sessions.CreateAnonymous(ctx)
	if err != nil {
		t.Fatal("cannot seed fixture family")
	}
	// Fixture-only authentication until POST /login exists. The stored family
	// and canonical token still use the real schema and encryption helpers.
	if _, err := db.Pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1
	    WHERE id=$1`, state.ID, id); err != nil {
		t.Fatal("cannot seed fixture identity")
	}
	service, err := NewService(db.Pool, sessions)
	if err != nil {
		t.Fatal(err)
	}
	return service, access.Principal{UserID: id, SessionID: state.ID, SecurityVersion: 1}
}

func TestAdminCreateAndListDatabase(t *testing.T) {
	s, actor := adminFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	password := " 猫猫猫猫 "
	input := CreateUserInput{Username: "new_admin", DisplayName: "  新しい管理者  ", Password: password, IsPlatformAdmin: true}
	id, err := s.CreateUser(ctx, actor, input)
	if err != nil {
		t.Fatal(err)
	}
	var hash, display string
	var active, admin bool
	var revision, version int64
	if err := s.pool.QueryRow(ctx, `SELECT password_hash,display_name,is_active,is_platform_admin,revision,security_version
	    FROM databrain.users WHERE id=$1`, id).Scan(&hash, &display, &active, &admin, &revision, &version); err != nil {
		t.Fatal("cannot inspect created user")
	}
	if valid, err := auth.VerifyPassword(ctx, password, hash); err != nil || !valid || display != "新しい管理者" || !active || !admin || revision != 1 || version != 1 {
		t.Fatal("created user violates password, profile or initial-state contract")
	}
	if duplicate, err := s.CreateUser(ctx, actor, input); duplicate != uuid.Nil || !errors.Is(err, ErrUsernameTaken) {
		t.Fatal("duplicate username was not rejected safely")
	}
	group, err := s.CreateGroup(ctx, actor, CreateGroupInput{Name: "  Engineering  "})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := s.CreateGroup(ctx, actor, CreateGroupInput{Name: "engineering"}); duplicate != uuid.Nil || !errors.Is(err, ErrGroupNameTaken) {
		t.Fatal("case-insensitive group uniqueness failed")
	}
	if _, err := s.pool.Exec(ctx, "UPDATE databrain.users SET is_active=false WHERE id=$1", id); err != nil {
		t.Fatal("cannot seed inactive user")
	}
	if _, err := s.pool.Exec(ctx, "UPDATE databrain.groups SET is_active=false WHERE id=$1", group); err != nil {
		t.Fatal("cannot seed inactive group")
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO databrain.memberships(group_id,user_id) VALUES ($1,$2)", group, id); err != nil {
		t.Fatal("cannot seed user membership")
	}
	users, more, err := s.ListUsers(ctx, actor, 1, 0)
	if err != nil || !more || len(users) != 1 || users[0].Username != "admin" {
		t.Fatal("first user page is not stable or has_more is wrong")
	}
	users, more, err = s.ListUsers(ctx, actor, 1, 1)
	if err != nil || more || len(users) != 1 || users[0].ID != id.String() || users[0].IsActive {
		t.Fatal("inactive user or final page missing")
	}
	groups, more, err := s.ListGroups(ctx, actor, 50, 0)
	if err != nil || more || len(groups) != 1 || groups[0].Name != "Engineering" || groups[0].IsActive || groups[0].Revision != 1 {
		t.Fatal("group list/default revision incorrect")
	}
	if groups, more, err := s.ListGroups(ctx, actor, 50, 50); err != nil || more || len(groups) != 0 {
		t.Fatal("empty group page incorrect")
	}
	profile, memberships, err := s.User(ctx, actor, id)
	if err != nil || profile.ID != id.String() || profile.Username != "new_admin" || len(memberships) != 1 ||
		memberships[0].ID != group.String() || memberships[0].IsActive {
		t.Fatal("user detail did not return the safe profile and inactive membership")
	}
	var untouched bool
	if err := s.pool.QueryRow(ctx, `SELECT u.security_version=1 AND u.revision=1 AND s.revoked_at IS NULL AND NOT s.reauth_required
	    AND (SELECT count(*) FROM databrain.session_tokens WHERE session_id=s.id)=1
	    FROM databrain.users u JOIN databrain.sessions s ON s.user_id=u.id WHERE s.id=$1`, actor.SessionID).Scan(&untouched); err != nil || !untouched {
		t.Fatal("creation changed existing actor privileges or session")
	}
	targetSession, _, err := s.sessions.(*auth.Service).CreateAnonymous(ctx)
	if err != nil {
		t.Fatal("cannot seed reset target family")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1
	    WHERE id=$1`, targetSession.ID, id); err != nil {
		t.Fatal("cannot authenticate reset target fixture")
	}
	actorMustLogin, err := s.ResetPassword(ctx, actor, id, ResetPasswordInput{ExpectedRevision: 1, Password: "Replacement-password"})
	if err != nil || actorMustLogin {
		t.Fatalf("password reset = actor_login=%v error=%v", actorMustLogin, err)
	}
	if _, err := s.ResetPassword(ctx, actor, id, ResetPasswordInput{ExpectedRevision: 1, Password: "Another-password"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal("stale password reset did not report revision conflict")
	}
	var replacementHash string
	var resetRevision, resetVersion int64
	var targetRevoked bool
	if err := s.pool.QueryRow(ctx, `SELECT u.password_hash,u.revision,u.security_version,s.revoked_at IS NOT NULL
	    FROM databrain.users u JOIN databrain.sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.id=$2`, id, targetSession.ID).
		Scan(&replacementHash, &resetRevision, &resetVersion, &targetRevoked); err != nil {
		t.Fatal("cannot inspect password reset")
	}
	if valid, err := auth.VerifyPassword(ctx, "Replacement-password", replacementHash); err != nil || !valid ||
		resetRevision != 2 || resetVersion != 2 || !targetRevoked {
		t.Fatal("password reset did not atomically update revision/version and revoke sessions")
	}
	if _, err := s.pool.Exec(ctx, "UPDATE databrain.users SET is_platform_admin=false WHERE id=$1", actor.UserID); err != nil {
		t.Fatal("cannot seed nonadmin actor")
	}
	_, _, err = s.ListUsers(ctx, actor, 50, 0)
	requireAdminCode(t, err, problem.AdminRequired)
	_, err = s.CreateGroup(ctx, actor, CreateGroupInput{Name: "Denied"})
	requireAdminCode(t, err, problem.AdminRequired)
}

func TestAdminEditUserUnambiguousTransitions(t *testing.T) {
	s, actor := adminFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID, err := s.CreateUser(ctx, actor, CreateUserInput{
		Username: "edit_user", DisplayName: "Before", Password: "Fixture-edit-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.sessions.(*auth.Service).CreateAnonymous(ctx)
	if err != nil {
		t.Fatal("cannot seed edited user family")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1
	    WHERE id=$1`, state.ID, userID); err != nil {
		t.Fatal("cannot authenticate edited user fixture")
	}

	if actorLogin, err := s.EditUser(ctx, actor, userID, EditUserInput{
		ExpectedRevision: 1, DisplayName: "  Renamed  ", IsActive: true,
	}); err != nil || actorLogin {
		t.Fatalf("name edit = actor_login=%v error=%v", actorLogin, err)
	}
	var name string
	var active, admin, revoked, reauth bool
	var revision, version, tokens int64
	if err := s.pool.QueryRow(ctx, `SELECT u.display_name,u.is_active,u.is_platform_admin,u.revision,u.security_version,
	    s.revoked_at IS NOT NULL,s.reauth_required,(SELECT count(*) FROM databrain.session_tokens st WHERE st.session_id=s.id)
	    FROM databrain.users u JOIN databrain.sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.id=$2`, userID, state.ID).
		Scan(&name, &active, &admin, &revision, &version, &revoked, &reauth, &tokens); err != nil ||
		name != "Renamed" || !active || admin || revision != 2 || version != 1 || revoked || reauth || tokens != 1 {
		t.Fatal("name-only edit changed security state")
	}

	if actorLogin, err := s.EditUser(ctx, actor, userID, EditUserInput{
		ExpectedRevision: 2, DisplayName: "Renamed admin", IsActive: true, IsPlatformAdmin: true,
	}); err != nil || actorLogin {
		t.Fatalf("promotion = actor_login=%v error=%v", actorLogin, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT u.revision,u.security_version,s.revoked_at IS NOT NULL,
	    s.reauth_required,(SELECT count(*) FROM databrain.session_tokens st WHERE st.session_id=s.id)
	    FROM databrain.users u JOIN databrain.sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.id=$2`, userID, state.ID).
		Scan(&revision, &version, &revoked, &reauth, &tokens); err != nil || revision != 3 || version != 1 || revoked || !reauth || tokens != 2 {
		t.Fatal("promotion did not rotate and require reauthentication")
	}

	if _, err := s.EditUser(ctx, actor, userID, EditUserInput{
		ExpectedRevision: 3, DisplayName: "Renamed admin", IsActive: true,
	}); err != nil {
		t.Fatal("demotion failed:", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT u.revision,u.security_version,s.revoked_at IS NOT NULL
	    FROM databrain.users u JOIN databrain.sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.id=$2`, userID, state.ID).
		Scan(&revision, &version, &revoked); err != nil || revision != 4 || version != 2 || !revoked {
		t.Fatal("demotion did not revoke the edited user")
	}
	if _, err := s.EditUser(ctx, actor, userID, EditUserInput{
		ExpectedRevision: 3, DisplayName: "Stale", IsActive: true,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal("stale user edit did not report revision conflict")
	}

	state, _, err = s.sessions.(*auth.Service).CreateAnonymous(ctx)
	if err != nil {
		t.Fatal("cannot seed deactivation family")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=2
	    WHERE id=$1`, state.ID, userID); err != nil {
		t.Fatal("cannot authenticate deactivation fixture")
	}
	if _, err := s.EditUser(ctx, actor, userID, EditUserInput{
		ExpectedRevision: 4, DisplayName: "Disabled", IsActive: false,
	}); err != nil {
		t.Fatal("deactivation failed:", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT u.is_active,u.revision,u.security_version,s.revoked_at IS NOT NULL
	    FROM databrain.users u JOIN databrain.sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.id=$2`, userID, state.ID).
		Scan(&active, &revision, &version, &revoked); err != nil || active || revision != 5 || version != 3 || !revoked {
		t.Fatal("deactivation did not atomically revoke the edited user")
	}
	if _, err := s.EditUser(ctx, actor, userID, EditUserInput{
		ExpectedRevision: 5, DisplayName: "Reactivated", IsActive: true,
	}); err != nil {
		t.Fatal("reactivation failed:", err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT is_active,revision,security_version FROM databrain.users WHERE id=$1", userID).
		Scan(&active, &revision, &version); err != nil || !active || revision != 6 || version != 3 {
		t.Fatal("reactivation did not use the privilege-increase effect")
	}

	combinedID, err := s.CreateUser(ctx, actor, CreateUserInput{
		Username: "combined_user", DisplayName: "Combined", Password: "Fixture-combined-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditUser(ctx, actor, combinedID, EditUserInput{
		ExpectedRevision: 1, DisplayName: "Combined", IsActive: false, IsPlatformAdmin: true,
	}); err != nil {
		t.Fatal("combined deactivation and promotion failed:", err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT is_active,is_platform_admin,revision,security_version FROM databrain.users WHERE id=$1", combinedID).
		Scan(&active, &admin, &revision, &version); err != nil || active || !admin || revision != 2 || version != 2 {
		t.Fatal("deactivation did not dominate the combined role promotion")
	}
	if _, err := s.EditUser(ctx, actor, actor.UserID, EditUserInput{
		ExpectedRevision: 1, DisplayName: "Fixture admin", IsActive: true,
	}); !errors.Is(err, ErrLastAdmin) {
		t.Fatal("last active admin demotion was accepted")
	}
}

func TestAdminCreationCapsConcurrent(t *testing.T) {
	s, actor := adminFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx, `INSERT INTO databrain.users(id,username,display_name,password_hash,is_active)
	    SELECT md5('cap-user-'||n)::uuid,'seed_'||n,'Seed','fixture-not-a-login-hash',false FROM generate_series(1,998) n`); err != nil {
		t.Fatal("cannot seed user cap")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO databrain.groups(id,name,is_active)
	    SELECT md5('cap-group-'||n)::uuid,'Seed '||n,false FROM generate_series(1,99) n`); err != nil {
		t.Fatal("cannot seed group cap")
	}
	for _, kind := range []string{"user", "group"} {
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, name := range []string{"race_one", "race_two"} {
			go func() {
				<-start
				var err error
				if kind == "user" {
					_, err = s.CreateUser(ctx, actor, CreateUserInput{Username: name, DisplayName: name, Password: "Fixture-race-password"})
				} else {
					_, err = s.CreateGroup(ctx, actor, CreateGroupInput{Name: name})
				}
				results <- err
			}()
		}
		close(start)
		a, b := <-results, <-results
		if !(a == nil && errors.Is(b, ErrPlatformLimitReached) || b == nil && errors.Is(a, ErrPlatformLimitReached)) {
			t.Fatalf("%s cap did not serialize concurrent creates: %v, %v", kind, a, b)
		}
	}
	var users, groups int
	if err := s.pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM databrain.users),(SELECT count(*) FROM databrain.groups)").Scan(&users, &groups); err != nil || users != 1000 || groups != 100 {
		t.Fatal("creation exceeded platform caps")
	}
}

func TestAdminCreationRechecksAfterLockWait(t *testing.T) {
	s, actor := adminFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, "SELECT singleton FROM databrain.security_guard WHERE singleton FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE databrain.sessions SET revoked_at=clock_timestamp() WHERE id=$1", actor.SessionID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := s.CreateGroup(ctx, actor, CreateGroupInput{Name: "Must not exist"}); result <- err }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
		    WHERE locktype='transactionid' AND NOT granted AND transactionid=(pg_current_xact_id()::text)::xid)`).Scan(&waiting)
		if err != nil {
			t.Fatal("cannot inspect lock waiter")
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("creation did not reach security lock: %v", err)
		case <-ctx.Done():
			t.Fatal("creation never waited for security lock")
		case <-ticker.C:
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	requireAdminCode(t, <-result, problem.SessionInvalid)
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.groups").Scan(&count); err != nil || count != 0 {
		t.Fatal("revoked actor created a group")
	}
}

func TestAdminGroupRelationsDatabase(t *testing.T) {
	s, actor := adminFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID, err := s.CreateUser(ctx, actor, CreateUserInput{
		Username: "relation_user", DisplayName: "Relation user", Password: "Fixture-relation-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	groupID, err := s.CreateGroup(ctx, actor, CreateGroupInput{Name: "Readers"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := s.Group(ctx, actor, groupID)
	if err != nil || profile.ID != groupID.String() || profile.Revision != 1 || !profile.IsActive {
		t.Fatalf("group profile = %+v, %v", profile, err)
	}
	if actorLogin, err := s.EditGroup(ctx, actor, groupID, EditGroupInput{ExpectedRevision: 1, Name: "Readers renamed", IsActive: true}); err != nil || actorLogin {
		t.Fatalf("name edit = actor_login=%v error=%v", actorLogin, err)
	}

	targetSession, _, err := s.sessions.(*auth.Service).CreateAnonymous(ctx)
	if err != nil {
		t.Fatal("cannot seed relation target family")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1
	    WHERE id=$1`, targetSession.ID, userID); err != nil {
		t.Fatal("cannot authenticate relation target fixture")
	}
	if actorLogin, err := s.AddMember(ctx, actor, groupID, userID); err != nil || actorLogin {
		t.Fatalf("add member = actor_login=%v error=%v", actorLogin, err)
	}
	var tokenCount int
	var reauth bool
	if err := s.pool.QueryRow(ctx, `SELECT s.reauth_required,(SELECT count(*) FROM databrain.session_tokens st WHERE st.session_id=s.id)
	    FROM databrain.sessions s WHERE s.id=$1`, targetSession.ID).Scan(&reauth, &tokenCount); err != nil || !reauth || tokenCount != 2 {
		t.Fatal("membership addition did not rotate and require reauthentication")
	}
	if _, err := s.AddMember(ctx, actor, groupID, userID); err != nil {
		t.Fatal("repeated membership addition failed")
	}
	var repeatedTokens int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.session_tokens WHERE session_id=$1", targetSession.ID).Scan(&repeatedTokens); err != nil || repeatedTokens != tokenCount {
		t.Fatal("repeated membership addition changed the family")
	}

	targetID := uuid.New()
	if _, err := s.pool.Exec(ctx, `INSERT INTO databrain.database_targets
	    (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
	    VALUES ($1,'Relations target','127.0.0.1',5432,'relations','runtime',$2,'disable',$3)`,
		targetID, bytes.Repeat([]byte{0x31}, 30), actor.UserID); err != nil {
		t.Fatal("cannot seed grant target")
	}
	if _, err := s.SetGrant(ctx, actor, groupID, targetID, access.ViewOnly); err != nil {
		t.Fatal(err)
	}
	detail, members, memberChoices, grants, databaseChoices, err := s.GroupDetails(ctx, actor, groupID)
	if err != nil || detail.ID != groupID.String() || len(members) != 1 || members[0].ID != userID.String() ||
		len(memberChoices) != 1 || memberChoices[0].ID != actor.UserID.String() || len(grants) != 1 ||
		grants[0].DatabaseID != targetID.String() || grants[0].Access != access.ViewOnly ||
		len(databaseChoices) != 1 || databaseChoices[0].ID != targetID.String() {
		t.Fatalf("group detail = %+v members=%+v choices=%+v grants=%+v databases=%+v error=%v",
			detail, members, memberChoices, grants, databaseChoices, err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.session_tokens WHERE session_id=$1", targetSession.ID).Scan(&tokenCount); err != nil || tokenCount != 3 {
		t.Fatal("grant addition did not rotate the family")
	}
	if _, err := s.SetGrant(ctx, actor, groupID, targetID, access.ViewOnly); err != nil {
		t.Fatal("identical grant set failed")
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM databrain.session_tokens WHERE session_id=$1", targetSession.ID).Scan(&repeatedTokens); err != nil || repeatedTokens != tokenCount {
		t.Fatal("identical grant set changed the family")
	}
	if _, err := s.SetGrant(ctx, actor, groupID, targetID, access.ViewEdit); err != nil {
		t.Fatal("grant upgrade failed")
	}
	if _, err := s.SetGrant(ctx, actor, groupID, targetID, access.ViewOnly); err != nil {
		t.Fatal("grant downgrade failed")
	}
	var version int64
	var revoked bool
	if err := s.pool.QueryRow(ctx, `SELECT u.security_version,s.revoked_at IS NOT NULL FROM databrain.users u
	    JOIN databrain.sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.id=$2`, userID, targetSession.ID).Scan(&version, &revoked); err != nil || version != 2 || !revoked {
		t.Fatal("grant downgrade did not revoke the affected user")
	}
	if _, err := s.RemoveGrant(ctx, actor, groupID, targetID); err != nil {
		t.Fatal("grant removal failed")
	}
	if _, err := s.RemoveGrant(ctx, actor, groupID, targetID); err != nil {
		t.Fatal("repeated grant removal failed")
	}
	if err := s.pool.QueryRow(ctx, "SELECT security_version FROM databrain.users WHERE id=$1", userID).Scan(&version); err != nil || version != 3 {
		t.Fatal("grant removal was not applied exactly once")
	}

	if _, err := s.EditGroup(ctx, actor, groupID, EditGroupInput{ExpectedRevision: 2, Name: "Readers renamed", IsActive: false}); err != nil {
		t.Fatal("group deactivation failed")
	}
	if _, err := s.EditGroup(ctx, actor, groupID, EditGroupInput{ExpectedRevision: 3, Name: "Readers renamed", IsActive: false}); err != nil {
		t.Fatal("identical group edit failed")
	}
	if err := s.pool.QueryRow(ctx, "SELECT security_version FROM databrain.users WHERE id=$1", userID).Scan(&version); err != nil || version != 4 {
		t.Fatal("group deactivation or no-op session effect is incorrect")
	}
	reactivatedSession, _, err := s.sessions.(*auth.Service).CreateAnonymous(ctx)
	if err != nil {
		t.Fatal("cannot seed group activation family")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=$3
	    WHERE id=$1`, reactivatedSession.ID, userID, version); err != nil {
		t.Fatal("cannot authenticate group activation fixture")
	}
	if _, err := s.EditGroup(ctx, actor, groupID, EditGroupInput{ExpectedRevision: 3, Name: "Readers renamed", IsActive: true}); err != nil {
		t.Fatal("group activation failed")
	}
	if err := s.pool.QueryRow(ctx, "SELECT reauth_required FROM databrain.sessions WHERE id=$1", reactivatedSession.ID).Scan(&reauth); err != nil || !reauth {
		t.Fatal("group activation did not require reauthentication")
	}
	if _, err := s.RemoveMember(ctx, actor, groupID, userID); err != nil {
		t.Fatal("membership removal failed")
	}
	if _, err := s.RemoveMember(ctx, actor, groupID, userID); err != nil {
		t.Fatal("repeated membership removal failed")
	}
	if err := s.pool.QueryRow(ctx, "SELECT security_version FROM databrain.users WHERE id=$1", userID).Scan(&version); err != nil || version != 5 {
		t.Fatal("membership removal was not applied exactly once")
	}
}
