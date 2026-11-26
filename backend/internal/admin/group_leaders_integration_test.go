package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/google/uuid"
)

func createAdminTestUser(t *testing.T, s *Service, actor access.Principal, username string) uuid.UUID {
	t.Helper()
	id, err := s.CreateUser(context.Background(), actor, CreateUserInput{
		Username: username, DisplayName: username, Password: "Leader-user-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func createAdminTestTarget(t *testing.T, s *Service, actor access.Principal, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO databrain.database_targets
	    (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
	    VALUES ($1,$2,'localhost',5432,$4,'role',decode(repeat('31',30),'hex'),'disable',$3)`,
		id, name, actor.UserID, "target_"+strings.ReplaceAll(id.String(), "-", "")); err != nil {
		t.Fatal(err)
	}
	return id
}

func groupRevision(t *testing.T, s *Service, groupID uuid.UUID) (int64, string) {
	t.Helper()
	var revision int64
	var leader string
	if err := s.pool.QueryRow(context.Background(), `SELECT revision,coalesce(leader_id::text,'')
	    FROM databrain.groups WHERE id=$1`, groupID).Scan(&revision, &leader); err != nil {
		t.Fatal(err)
	}
	return revision, leader
}

func TestGroupLeadersAndPolicyGenerationPostgres(t *testing.T) {
	s, actor := adminFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leader := createAdminTestUser(t, s, actor, "group_leader")
	member := createAdminTestUser(t, s, actor, "group_member")
	outsider := createAdminTestUser(t, s, actor, "group_outsider")
	inactive := createAdminTestUser(t, s, actor, "group_inactive")
	var inactiveRevision int64
	if err := s.pool.QueryRow(ctx, `SELECT revision FROM databrain.users WHERE id=$1`, inactive).Scan(&inactiveRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditUser(ctx, actor, inactive, EditUserInput{inactiveRevision, "group_inactive", false, false}); err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateGroup(ctx, actor, CreateGroupInput{Name: "Required group", LeaderID: leader})
	if err != nil {
		t.Fatal(err)
	}
	revision, leaderID := groupRevision(t, s, group)
	if revision != 1 || leaderID != leader.String() {
		t.Fatalf("initial leader or revision wrong: %d %q", revision, leaderID)
	}
	var memberCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM databrain.memberships WHERE group_id=$1 AND user_id=$2`, group, leader).Scan(&memberCount); err != nil || memberCount != 1 {
		t.Fatalf("initial leader membership missing: %d %v", memberCount, err)
	}
	targetA := createAdminTestTarget(t, s, actor, "Leader target A")
	if _, err := s.SetGrant(ctx, actor, group, targetA, access.ViewOnly); err != nil {
		t.Fatal(err)
	}
	generationA, err := access.PolicyGeneration(ctx, s.pool, targetA)
	if err != nil {
		t.Fatal(err)
	}
	required, err := access.RequiredGroups(ctx, s.pool, targetA)
	if err != nil || len(required) != 1 || required[0].GroupID != group || required[0].Access != access.ViewOnly ||
		required[0].LeaderID != leader || required[0].IncompleteReason != "" {
		t.Fatalf("required view-only group wrong: %+v %v", required, err)
	}
	// A second target/group proves unrelated changes do not advance another target's generation.
	otherGroup, err := s.CreateGroup(ctx, actor, CreateGroupInput{Name: "Other group", LeaderID: member})
	if err != nil {
		t.Fatal(err)
	}
	targetB := createAdminTestTarget(t, s, actor, "Leader target B")
	if _, err := s.SetGrant(ctx, actor, otherGroup, targetB, access.ViewEdit); err != nil {
		t.Fatal(err)
	}
	generationB, err := access.PolicyGeneration(ctx, s.pool, targetB)
	if err != nil {
		t.Fatal(err)
	}
	if generationA != 2 || generationB != 2 {
		t.Fatalf("initial grant generation wrong: A=%d B=%d", generationA, generationB)
	}
	// A leaderless required group remains visible with a repairable reason.
	leaderless, err := s.CreateGroup(ctx, actor, CreateGroupInput{Name: "Leaderless group"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGrant(ctx, actor, leaderless, targetA, access.ViewOnly); err != nil {
		t.Fatal(err)
	}
	required, err = access.RequiredGroups(ctx, s.pool, targetA)
	if err != nil || len(required) != 2 {
		t.Fatalf("leaderless group omitted: %+v %v", required, err)
	}
	var sawLeaderless bool
	for _, item := range required {
		if item.GroupID == leaderless && item.IncompleteReason == access.ReasonNoLeader {
			sawLeaderless = true
		}
	}
	if !sawLeaderless {
		t.Fatalf("leaderless reason missing: %+v", required)
	}
	generationA, _ = access.PolicyGeneration(ctx, s.pool, targetA)
	// Add the replacement candidate as a member, then replace and restore the leader.
	if _, err := s.AddMember(ctx, actor, group, member); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssignLeader(ctx, actor, group, member, 1); err != nil {
		t.Fatal(err)
	}
	revision, leaderID = groupRevision(t, s, group)
	generationAfterReplace, _ := access.PolicyGeneration(ctx, s.pool, targetA)
	if revision != 2 || leaderID != member.String() || generationAfterReplace != generationA+1 {
		t.Fatalf("leader replacement wrong: rev=%d leader=%q gen=%d", revision, leaderID, generationAfterReplace)
	}
	if _, err := s.AssignLeader(ctx, actor, group, leader, 2); err != nil {
		t.Fatal(err)
	}
	revision, leaderID = groupRevision(t, s, group)
	generationAfterRestore, _ := access.PolicyGeneration(ctx, s.pool, targetA)
	if revision != 3 || leaderID != leader.String() || generationAfterRestore != generationAfterReplace+1 {
		t.Fatalf("leader restoration wrong: rev=%d leader=%q gen=%d", revision, leaderID, generationAfterRestore)
	}
	generationBAfter, _ := access.PolicyGeneration(ctx, s.pool, targetB)
	if generationBAfter != generationB {
		t.Fatalf("unrelated target generation changed: %d -> %d", generationB, generationBAfter)
	}
	// Idempotent assignment does not advance revision or generation.
	if _, err := s.AssignLeader(ctx, actor, group, leader, 3); err != nil {
		t.Fatal(err)
	}
	revision, _ = groupRevision(t, s, group)
	generationNoop, _ := access.PolicyGeneration(ctx, s.pool, targetA)
	if revision != 3 || generationNoop != generationAfterRestore {
		t.Fatalf("no-op leader assignment changed state: rev=%d gen=%d", revision, generationNoop)
	}
	// Denied candidates never replace the leader.
	if _, err := s.AssignLeader(ctx, actor, group, outsider, 3); err == nil {
		t.Fatal("nonmember was assigned")
	}
	if _, err := s.AssignLeader(ctx, actor, group, inactive, 3); err == nil {
		t.Fatal("inactive user was assigned")
	}
	if _, err := s.AssignLeader(ctx, actor, group, member, 1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision returned %v", err)
	}
	revision, leaderID = groupRevision(t, s, group)
	if revision != 3 || leaderID != leader.String() {
		t.Fatalf("denied assignment changed group: rev=%d leader=%q", revision, leaderID)
	}
	if _, err := s.EditGroup(ctx, actor, group, EditGroupInput{3, "Required group", true}); err != nil {
		t.Fatal(err)
	}
	generationNameOnly, _ := access.PolicyGeneration(ctx, s.pool, targetA)
	if generationNameOnly != generationAfterRestore {
		t.Fatalf("name-only edit advanced policy: %d -> %d", generationAfterRestore, generationNameOnly)
	}
	// Deactivating a leader clears the assignment and advances affected targets.
	var userRevision int64
	if err := s.pool.QueryRow(ctx, `SELECT revision FROM databrain.users WHERE id=$1`, leader).Scan(&userRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditUser(ctx, actor, leader, EditUserInput{userRevision, "group_leader", false, false}); err != nil {
		t.Fatal(err)
	}
	_, leaderID = groupRevision(t, s, group)
	generationDeactivate, _ := access.PolicyGeneration(ctx, s.pool, targetA)
	if leaderID != "" || generationDeactivate != generationNameOnly+1 {
		t.Fatalf("user deactivation did not clear leader/advance: leader=%q gen=%d", leaderID, generationDeactivate)
	}
	required, err = access.RequiredGroups(ctx, s.pool, targetA)
	if err != nil {
		t.Fatal(err)
	}
	var sawInactiveLeader bool
	for _, item := range required {
		if item.GroupID == group && item.IncompleteReason == access.ReasonNoLeader {
			sawInactiveLeader = true
		}
	}
	if !sawInactiveLeader {
		t.Fatalf("deactivated leader group has no repair reason: %+v", required)
	}
	// Group deactivation removes it from the required set; reactivation requires repair.
	revision, _ = groupRevision(t, s, group)
	if _, err := s.EditGroup(ctx, actor, group, EditGroupInput{revision, "Required group", false}); err != nil {
		t.Fatal(err)
	}
	generationGroupOff, _ := access.PolicyGeneration(ctx, s.pool, targetA)
	required, _ = access.RequiredGroups(ctx, s.pool, targetA)
	for _, item := range required {
		if item.GroupID == group {
			t.Fatal("inactive group remained required")
		}
	}
	revision, _ = groupRevision(t, s, group)
	if _, err := s.EditGroup(ctx, actor, group, EditGroupInput{revision, "Required group", true}); err != nil {
		t.Fatal(err)
	}
	generationGroupOn, _ := access.PolicyGeneration(ctx, s.pool, targetA)
	if generationGroupOn != generationGroupOff+1 {
		t.Fatalf("group reactivation did not advance: %d -> %d", generationGroupOff, generationGroupOn)
	}
}

func TestAssignLeaderRacePostgres(t *testing.T) {
	s, actor := adminFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	initial := createAdminTestUser(t, s, actor, "race_leader_initial")
	first := createAdminTestUser(t, s, actor, "race_leader_one")
	second := createAdminTestUser(t, s, actor, "race_leader_two")
	group, err := s.CreateGroup(ctx, actor, CreateGroupInput{Name: "Race group", LeaderID: initial})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, actor, group, second); err != nil {
		t.Fatal(err)
	}
	target := createAdminTestTarget(t, s, actor, "Race target")
	if _, err := s.SetGrant(ctx, actor, group, target, access.ViewEdit); err != nil {
		t.Fatal(err)
	}
	before, err := access.PolicyGeneration(ctx, s.pool, target)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, userID := range []uuid.UUID{first, second} {
		userID := userID
		go func() {
			<-start
			_, err := s.AssignLeader(ctx, actor, group, userID, 1)
			results <- err
		}()
	}
	close(start)
	errOne, errTwo := <-results, <-results
	var succeeded, conflicted int
	for _, err := range []error{errOne, errTwo} {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrRevisionConflict):
			conflicted++
		default:
			t.Fatalf("unexpected race result: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("race results succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	revision, leader := groupRevision(t, s, group)
	after, err := access.PolicyGeneration(ctx, s.pool, target)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 2 || (leader != first.String() && leader != second.String()) || after != before+1 {
		t.Fatalf("race state wrong: rev=%d leader=%q generation=%d->%d", revision, leader, before, after)
	}
}
