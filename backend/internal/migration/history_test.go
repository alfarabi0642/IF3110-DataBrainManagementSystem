package migration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func TestHistoryArguments(t *testing.T) {
	s := NewAttemptStore(nil)
	ctx := context.Background()
	id := uuid.New()
	for _, ids := range [][2]uuid.UUID{{uuid.Nil, id}, {id, uuid.Nil}} {
		_, err := s.Get(ctx, ids[0], ids[1])
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	for _, page := range [][2]int{{0, 0}, {-1, 0}, {201, 0}, {50, -1}, {50, 100001}} {
		_, _, err := s.List(ctx, id, page[0], page[1])
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	_, _, err := s.List(ctx, uuid.Nil, 50, 0)
	requireDraftCode(t, err, problem.InvalidRequest)
}

func TestHistoryPublicFailure(t *testing.T) {
	if publicAttemptFailure(ExecutionFailure{}) != nil {
		t.Fatal("successful or pending attempt should have no failure")
	}
	for _, code := range []string{"CONNECTION_FAILED", "ACCESS_REVOKED", "SQL_ERROR", "SQL_TIMEOUT", "LOCK_TIMEOUT", "CANCELED", "COMMIT_UNCERTAIN", "OUTCOME_PERSISTENCE_FAILED", "PROCESS_INTERRUPTED", "PRIVATE_CODE"} {
		f := publicAttemptFailure(ExecutionFailure{Code: code, Message: "PRIVATE_SQL_DSN", SQLState: "23505", StatementIndex: 50})
		if f == nil || f.Message == "" || len(f.Message) > 240 || strings.Contains(f.Message, "PRIVATE") || strings.Contains(f.Code, "PRIVATE") || f.SQLState != "23505" || f.StatementIndex != 50 {
			t.Fatal("public failure exposed private text or lost safe diagnostics")
		}
	}
	for _, state := range []string{"", "bad!", "privt", "<sql>", "23505X", "猫猫"} {
		f := publicAttemptFailure(ExecutionFailure{Code: "SQL_ERROR", SQLState: state, StatementIndex: 51})
		if f.SQLState != "" || f.StatementIndex != 0 {
			t.Fatal("invalid diagnostics must be omitted")
		}
	}
	when := time.Date(2026, 10, 24, 12, 30, 0, 0, time.FixedZone("fixture", 7*60*60))
	a := AttemptSummary{CreatedAt: when, StartedAt: &when}
	utcAttemptTimes(&a)
	if a.CreatedAt.Location() != time.UTC || a.StartedAt.Location() != time.UTC || a.FinishedAt != nil || !a.CreatedAt.Equal(when) {
		t.Fatal("UTC conversion changed the instant or lost nullable times")
	}
}
