package migration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func requireDraftCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) || p.Code != want || p.Message != "" {
		t.Fatalf("expected safe %s error, got %v", want, err)
	}
}

func TestValidateDraft(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input DraftInput
		valid bool
	}{
		{"invalid SQL accepted verbatim", DraftInput{" 猫 migration \t", " \r\nNOT SQL; '猫'\n\t "}, true},
		{"whitespace SQL accepted", DraftInput{"title", " \t\r\n"}, true},
		{"Unicode title at character limit", DraftInput{strings.Repeat("猫", 120), "?"}, true},
		{"SQL at byte limit", DraftInput{"title", strings.Repeat("猫", MaxSQLBytes/3) + "aa"}, true},
		{"empty title", DraftInput{"", "?"}, false},
		{"blank title", DraftInput{" \t\r\n", "?"}, false},
		{"long title", DraftInput{strings.Repeat("猫", 121), "?"}, false},
		{"invalid title encoding", DraftInput{string([]byte{0xff}), "?"}, false},
		{"NUL title", DraftInput{"a\x00b", "?"}, false},
		{"empty SQL", DraftInput{"title", ""}, false},
		{"long SQL", DraftInput{"title", strings.Repeat("a", MaxSQLBytes+1)}, false},
		{"invalid SQL encoding", DraftInput{"title", string([]byte{0xff})}, false},
		{"NUL SQL", DraftInput{"title", "SELECT '\x00'"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDraft(tc.input)
			if tc.valid {
				if err != nil {
					t.Fatal("valid decoded input rejected")
				}
			} else {
				requireDraftCode(t, err, problem.ValidationFailed)
			}
		})
	}
}

func TestDraftArgumentsRejectedBeforeDatabase(t *testing.T) {
	s := NewDraftStore(nil)
	ctx := context.Background()
	id, actor := uuid.New(), uuid.New()
	input := DraftInput{Title: "title", SQL: "?"}
	for _, ids := range [][2]uuid.UUID{{uuid.Nil, actor}, {id, uuid.Nil}} {
		_, err := s.Create(ctx, ids[0], ids[1], input)
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	_, err := s.Create(ctx, id, actor, DraftInput{})
	requireDraftCode(t, err, problem.ValidationFailed)
	for _, ids := range [][2]uuid.UUID{{uuid.Nil, id}, {id, uuid.Nil}} {
		_, err := s.Get(ctx, ids[0], ids[1])
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	for _, args := range [][2]int{{0, 0}, {201, 0}, {1, -1}, {1, 100001}} {
		_, _, err := s.List(ctx, id, args[0], args[1])
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	_, _, err = s.List(ctx, uuid.Nil, 50, 0)
	requireDraftCode(t, err, problem.InvalidRequest)
	for _, ids := range [][3]uuid.UUID{{uuid.Nil, id, actor}, {id, uuid.Nil, actor}, {id, id, uuid.Nil}} {
		_, err := s.Save(ctx, ids[0], ids[1], ids[2], SaveDraftInput{input, 1})
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	for _, revision := range []int64{0, -1} {
		_, err := s.Save(ctx, id, id, actor, SaveDraftInput{input, revision})
		requireDraftCode(t, err, problem.InvalidRequest)
	}
	_, err = s.Save(ctx, id, id, actor, SaveDraftInput{ExpectedRevision: 1})
	requireDraftCode(t, err, problem.ValidationFailed)
}

func TestDraftStoreErrorsAreSafe(t *testing.T) {
	for _, code := range []string{"23503", "23514", "22001", "22021", "22003", "XX000"} {
		want := problem.ValidationFailed
		if code == "XX000" {
			want = problem.PlatformUnavailable
		}
		err := draftStoreError(&pgconn.PgError{Code: code, Message: "private SQL", Detail: "private credentials"})
		requireDraftCode(t, err, want)
	}
	requireDraftCode(t, draftStoreError(errors.New("private DSN")), problem.PlatformUnavailable)
}
