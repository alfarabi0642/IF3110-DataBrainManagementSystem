package admin

import (
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
)

func TestInactiveRolePersistence(t *testing.T) {
	s, actor := adminFixture(t)
	ctx := t.Context()
	id, err := s.CreateUser(ctx, actor, CreateUserInput{Username: "inactive_role", DisplayName: "Inactive", Password: "Inactive-fixture-password"})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.sessions.(*auth.Service).CreateAnonymous(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE databrain.sessions SET kind='authenticated',user_id=$2,security_version=1 WHERE id=$1`, state.ID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditUser(ctx, actor, id, EditUserInput{ExpectedRevision: 1, DisplayName: "Inactive"}); err != nil {
		t.Fatal(err)
	}
	for i, wantAdmin := range []bool{true, false} {
		revision := int64(i + 2)
		mustLogin, err := s.EditUser(ctx, actor, id, EditUserInput{ExpectedRevision: revision, DisplayName: "Inactive", IsPlatformAdmin: wantAdmin})
		if err != nil || mustLogin {
			t.Fatalf("role edit failed: %v", err)
		}
		var active, admin bool
		var gotRevision, version, live, tokens int64
		err = s.pool.QueryRow(ctx, `SELECT is_active,is_platform_admin,revision,security_version,
            (SELECT count(*) FROM databrain.sessions WHERE user_id=$1 AND revoked_at IS NULL),
            (SELECT count(*) FROM databrain.session_tokens WHERE session_id=$2)
            FROM databrain.users WHERE id=$1`, id, state.ID).Scan(&active, &admin, &gotRevision, &version, &live, &tokens)
		if err != nil || active || admin != wantAdmin || gotRevision != revision+1 || version != 2 || live != 0 || tokens != 1 {
			t.Fatalf("inactive edit did not persist or changed sessions: err=%v active=%t admin=%t revision=%d version=%d live=%d tokens=%d", err, active, admin, gotRevision, version, live, tokens)
		}
		// Pengiriman ulang nilai yang sama tidak menaikkan revision.
		if _, err := s.EditUser(ctx, actor, id, EditUserInput{ExpectedRevision: gotRevision, DisplayName: "Inactive", IsPlatformAdmin: wantAdmin}); err != nil {
			t.Fatal(err)
		}
		var after int64
		if err := s.pool.QueryRow(ctx, "SELECT revision FROM databrain.users WHERE id=$1", id).Scan(&after); err != nil || after != gotRevision {
			t.Fatal("no-op changed revision")
		}
	}
}
