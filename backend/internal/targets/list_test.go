package targets

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func TestListInputValidation(t *testing.T) {
	p := &Provider{}
	for _, page := range [][2]int{{0, 0}, {-1, 0}, {201, 0}, {50, -1}, {50, 100001}} {
		_, _, err := p.ListForUser(context.Background(), uuid.New(), page[0], page[1])
		checkCode(t, err, problem.InvalidRequest)
		_, _, err = p.ListAdmin(context.Background(), page[0], page[1])
		checkCode(t, err, problem.InvalidRequest)
	}
	_, _, err := p.ListForUser(context.Background(), uuid.Nil, 50, 0)
	checkCode(t, err, problem.InvalidRequest)
}

func TestDatabaseLists(t *testing.T) {
	f := fixture(t) // Fixture memeriksa identitas sebelum membuat database unik.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := f.p
	first := mustCreate(t, f, f.input)
	second := mustCreate(t, f, f.second)
	for _, target := range []*Target{&first, &second} {
		edit := editOf(*target)
		edit.Name = "Same name <b>text</b>"
		updated, err := p.Edit(ctx, target.ID, edit)
		if err != nil {
			t.Fatal("cannot prepare tied target names")
		}
		*target = updated
	}
	hidden, unrelated := uuid.New(), uuid.New()
	// Record sintetis menguji filter daftar platform tanpa membuka endpoint target atau mengubah database
	// target.
	for _, target := range []struct {
		id     uuid.UUID
		name   string
		active bool
	}{
		{hidden, "Hidden", false}, {unrelated, "Unrelated", true},
	} {
		sealed, err := p.secrets.SealTarget(target.id, []byte("SENSITIVE-fixture-password"))
		if err != nil {
			t.Fatal("cannot seal synthetic fixture credential")
		}
		_, err = p.pool.Exec(ctx, `INSERT INTO databrain.database_targets
		    (id,name,host,port,database_name,username,password_sealed,ssl_mode,is_active,created_by)
		    VALUES ($1,$2,'127.0.0.1',55432,$3,'fixture_role',$4,'disable',$5,$6)`,
			target.id, target.name, "list_fixture_"+target.id.String(), sealed, target.active, f.actor)
		if err != nil {
			t.Fatal("cannot insert platform list fixture")
		}
		if _, err := p.pool.Exec(ctx, "INSERT INTO databrain.migration_streams(target_id) VALUES ($1)", target.id); err != nil {
			t.Fatal("cannot insert list fixture stream")
		}
	}
	admin, noGrant, other, inactive := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_, err := p.pool.Exec(ctx, `INSERT INTO databrain.users(id,username,display_name,password_hash,is_active,is_platform_admin) VALUES
	    ($1,'admin_no_grant','Admin','fixture-not-an-account-hash',true,true),
	    ($2,'no_grant','No grant','fixture-not-an-account-hash',true,false),
	    ($3,'other_user','Other','fixture-not-an-account-hash',true,false),
	    ($4,'inactive_user','Inactive','fixture-not-an-account-hash',false,false)`, admin, noGrant, other, inactive)
	if err != nil {
		t.Fatal("cannot insert list user fixtures")
	}
	viewGroup, editGroup, disabledGroup, otherGroup := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_, err = p.pool.Exec(ctx, `INSERT INTO databrain.groups(id,name,is_active) VALUES
	    ($1,'Viewers',true),($2,'Editors',true),($3,'Disabled',false),($4,'Other',true)`, viewGroup, editGroup, disabledGroup, otherGroup)
	if err != nil {
		t.Fatal("cannot insert list group fixtures")
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO databrain.memberships(group_id,user_id) VALUES
	    ($1,$5),($2,$5),($3,$5),($1,$6),($4,$7)`, viewGroup, editGroup, disabledGroup, otherGroup, f.actor, inactive, other)
	if err != nil {
		t.Fatal("cannot insert list membership fixtures")
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO databrain.database_grants(group_id,target_id,access_level) VALUES
	    ($1,$5,'view_only'),($1,$6,'view_only'),($1,$7,'view_only'),
	    ($2,$5,'view_edit'),($3,$6,'view_edit'),($4,$8,'view_only')`,
		viewGroup, editGroup, disabledGroup, otherGroup, first.ID, second.ID, hidden, unrelated)
	if err != nil {
		t.Fatal("cannot insert list grant fixtures")
	}

	draft, attempt := uuid.New(), uuid.New()
	if _, err := p.pool.Exec(ctx, `INSERT INTO databrain.migration_drafts(id,target_id,title,sql,created_by,updated_by)
	    VALUES ($1,$2,'List fixture','SELECT 1',$3,$3)`, draft, first.ID, f.actor); err != nil {
		t.Fatal("cannot insert active attempt draft fixture")
	}
	hash := sha256.Sum256([]byte("SELECT 1"))
	_, err = p.pool.Exec(ctx, `INSERT INTO databrain.migration_attempts
	    (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,target_config_revision,
	    sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
	    VALUES ($1,$2,$3,1,1,'List fixture',$4,$5,'SELECT 1',$6,$7,'fixture_editor',$8,'reserved')`,
		attempt, first.ID, draft, first.Name, first.ConfigRevision, hash[:], f.actor, uuid.New())
	if err != nil {
		t.Fatal("cannot insert active attempt fixture")
	}
	if _, err := p.pool.Exec(ctx, "UPDATE databrain.migration_streams SET active_attempt_id=$2,next_version=2 WHERE target_id=$1", first.ID, attempt); err != nil {
		t.Fatal("cannot link active attempt fixture")
	}

	t.Run("effective grants and safe projection", func(t *testing.T) {
		items, more, err := p.ListForUser(ctx, f.actor, 50, 0)
		if err != nil || more || len(items) != 2 {
			t.Fatal("list did not filter inactive/foreign targets or deduplicate grants")
		}
		for _, item := range items {
			if item.Name != first.Name {
				t.Fatal("list changed the stored target name")
			}
			if item.ID == first.ID {
				if item.EffectiveAccess != access.ViewEdit || item.ActiveAttemptID == nil || *item.ActiveAttemptID != attempt || item.ActiveAttemptState != "reserved" || item.ActiveAttemptVersion != "1" {
					t.Fatal("highest active grant or active attempt is wrong")
				}
			} else if item.ID == second.ID {
				if item.EffectiveAccess != access.ViewOnly || item.ActiveAttemptID != nil || item.ActiveAttemptState != "" || item.ActiveAttemptVersion != "" {
					t.Fatal("inactive group's grant affected access or an attempt leaked across targets")
				}
			} else {
				t.Fatal("foreign target appeared in user list")
			}
		}
		for _, sensitive := range []string{f.input.Password, f.input.Host, f.input.Username, "SENSITIVE", "SELECT 1"} {
			if strings.Contains(fmt.Sprintf("%+v", items), sensitive) {
				t.Fatal("user list exposes connection credentials or SQL")
			}
		}
		for _, userID := range []uuid.UUID{admin, noGrant, inactive, uuid.New()} {
			items, more, err := p.ListForUser(ctx, userID, 50, 0)
			if err != nil || more || len(items) != 0 {
				t.Fatal("admin without grant, inactive, unknown or ungranted user received target data")
			}
		}
		items, more, err = p.ListForUser(ctx, other, 50, 0)
		if err != nil || more || len(items) != 1 || items[0].ID != unrelated {
			t.Fatal("user identity did not scope membership lookup")
		}
	})

	t.Run("stable pagination and admin list", func(t *testing.T) {
		tied := []uuid.UUID{first.ID, second.ID}
		sort.Slice(tied, func(i, j int) bool { return tied[i].String() < tied[j].String() })
		for offset, want := range tied {
			items, more, err := p.ListForUser(ctx, f.actor, 1, offset)
			if err != nil || len(items) != 1 || items[0].ID != want || more != (offset == 0) {
				t.Fatal("user pagination lost its name/id tie-breaker or has_more")
			}
		}
		items, more, err := p.ListAdmin(ctx, 200, 0)
		if err != nil || more || len(items) != 4 || items[0].ID != hidden || items[0].IsActive || items[1].ID != tied[0] || items[2].ID != tied[1] || items[3].ID != unrelated {
			t.Fatal("admin selector list incorrectly used user grants or excluded inactive targets")
		}
		for _, item := range items {
			if !item.PasswordStored || item.Host == "" || item.Username == "" {
				t.Fatal("admin metadata missing connection fields or stored-password indicator")
			}
			if strings.Contains(fmt.Sprintf("%+v", item), f.input.Password) || strings.Contains(fmt.Sprintf("%+v", item), "SENSITIVE") {
				t.Fatal("admin list exposes stored credentials")
			}
		}
		page, more, err := p.ListAdmin(ctx, 2, 1)
		if err != nil || !more || len(page) != 2 || page[0].ID != tied[0] || page[1].ID != tied[1] {
			t.Fatal("admin pagination or has_more is wrong")
		}
		page, more, err = p.ListAdmin(ctx, 2, 3)
		if err != nil || more || len(page) != 1 || page[0].ID != unrelated {
			t.Fatal("admin final page is wrong")
		}
		if items, more, err := p.ListForUser(ctx, f.actor, 200, 100000); err != nil || more || len(items) != 0 {
			t.Fatal("empty user page is wrong")
		}
		if items, more, err := p.ListAdmin(ctx, 200, 100000); err != nil || more || len(items) != 0 {
			t.Fatal("empty admin page is wrong")
		}
	})

	t.Run("grant changes are read afresh", func(t *testing.T) {
		if _, err := p.pool.Exec(ctx, "DELETE FROM databrain.memberships WHERE group_id=$1 AND user_id=$2", editGroup, f.actor); err != nil {
			t.Fatal("cannot remove fixture membership")
		}
		items, _, err := p.ListForUser(ctx, f.actor, 50, 0)
		if err != nil || len(items) != 2 {
			t.Fatal("membership change hid an unrelated grant")
		}
		for _, item := range items {
			if item.EffectiveAccess != access.ViewOnly {
				t.Fatal("list reused a stale edit grant")
			}
		}
		if _, err := p.pool.Exec(ctx, "UPDATE databrain.groups SET is_active=false WHERE id=$1", viewGroup); err != nil {
			t.Fatal("cannot disable fixture group")
		}
		items, _, err = p.ListForUser(ctx, f.actor, 50, 0)
		if err != nil || len(items) != 0 {
			t.Fatal("list retained access through inactive groups")
		}
	})

	t.Run("safe cancellation and resource cleanup", func(t *testing.T) {
		canceled, stop := context.WithCancel(ctx)
		stop()
		items, more, err := p.ListForUser(canceled, f.actor, 50, 0)
		checkCode(t, err, problem.PlatformUnavailable)
		if items != nil || more {
			t.Fatal("canceled query returned partial user data")
		}
		adminItems, more, err := p.ListAdmin(canceled, 50, 0)
		checkCode(t, err, problem.PlatformUnavailable)
		if adminItems != nil || more || p.pool.Stat().AcquiredConns() != 0 {
			t.Fatal("canceled query returned partial metadata or leaked a platform lease")
		}
	})
}
