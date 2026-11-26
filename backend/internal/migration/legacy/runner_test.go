package legacy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRunnerArguments(t *testing.T) {
	if _, err := NewRunner(nil, nil, nil); err == nil {
		t.Fatal("missing dependencies accepted")
	}
	r := &Runner{}
	id := uuid.New()
	for _, args := range []struct {
		target, draft uuid.UUID
		input         ReserveInput
	}{
		{uuid.Nil, id, ReserveInput{1, id}}, {id, uuid.Nil, ReserveInput{1, id}}, {id, id, ReserveInput{0, id}}, {id, id, ReserveInput{1, uuid.Nil}},
	} {
		_, err := r.Execute(context.Background(), access.Principal{}, args.target, args.draft, args.input)
		requireProblemCode(t, err, problem.InvalidRequest)
	}
}

func TestRunnerFailureClassification(t *testing.T) {
	for _, tc := range []struct{ state, code string }{{"23505", "SQL_ERROR"}, {"55P03", "LOCK_TIMEOUT"}, {"57014", "SQL_TIMEOUT"}, {"bad!", "SQL_ERROR"}} {
		f := StatementFailure(context.Background(), &pgconn.PgError{Code: tc.state, Message: "PRIVATE_SQL", Detail: "PRIVATE_DSN", Hint: "PRIVATE_HINT"}, 2)
		if f.Code != tc.code || f.StatementIndex != 2 || len(f.Message) > 240 || strings.Contains(f.Message, "PRIVATE") {
			t.Fatal("unsafe statement diagnostic")
		}
		if tc.state == "bad!" && f.SQLState != "" {
			t.Fatal("invalid SQLSTATE accepted")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	f := StatementFailure(canceled, errors.New("PRIVATE_DSN"), 1)
	if f.Code != "CANCELED" || notStartedExecutionError(canceled, "CONNECTION_FAILED", "unused").Failure.Code != "CANCELED" {
		t.Fatal("cancellation misclassified")
	}
	if got := CommitOutcome(nil, nil); got.State != "succeeded" || got.Evidence != "commit_ack" {
		t.Fatal("commit acknowledgment not recognized")
	}
	if got := CommitOutcome(nil, pgx.ErrTxCommitRollback); got.State != "failed" || got.Evidence != "server_rollback" {
		t.Fatal("definitive rollback not recognized")
	}
	for _, err := range []error{errors.New("PRIVATE_DSN"), context.Canceled, &pgconn.PgError{Code: "40003", Severity: "ERROR", Message: "PRIVATE_SQL"}, &pgconn.PgError{Code: "XX000", Severity: "FATAL"}} {
		if got := CommitOutcome(nil, err); got.State != "unknown" || got.Evidence != "" || got.Failure.Code != "COMMIT_UNCERTAIN" || strings.Contains(got.Failure.Message, "PRIVATE") {
			t.Fatal("unproved commit classified as terminal")
		}
	}
}
