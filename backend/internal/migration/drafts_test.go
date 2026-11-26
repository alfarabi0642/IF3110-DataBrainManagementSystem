package migration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func RequireDraftCode(t *testing.T, err error, want problem.Code) {
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
		{"invalid SQL accepted verbatim", DraftInput{Title: " 猫 migration \t", SQL: " \r\nNOT SQL; '猫'\n\t "}, true},
		{"whitespace SQL accepted", DraftInput{Title: "title", SQL: " \t\r\n"}, true},
		{"Unicode title at character limit", DraftInput{Title: strings.Repeat("猫", 120), SQL: "?"}, true},
		{"SQL at byte limit", DraftInput{Title: "title", SQL: strings.Repeat("猫", sqlpolicy.MaxSQLBytes/3) + "aa"}, true},
		{"empty title", DraftInput{Title: "", SQL: "?"}, false},
		{"blank title", DraftInput{Title: " \t\r\n", SQL: "?"}, false},
		{"long title", DraftInput{Title: strings.Repeat("猫", 121), SQL: "?"}, false},
		{"invalid title encoding", DraftInput{Title: string([]byte{0xff}), SQL: "?"}, false},
		{"NUL title", DraftInput{Title: "a\x00b", SQL: "?"}, false},
		{"empty SQL", DraftInput{Title: "title", SQL: ""}, false},
		{"long SQL", DraftInput{Title: "title", SQL: strings.Repeat("a", sqlpolicy.MaxSQLBytes+1)}, false},
		{"invalid SQL encoding", DraftInput{Title: "title", SQL: string([]byte{0xff})}, false},
		{"NUL SQL", DraftInput{Title: "title", SQL: "SELECT '\x00'"}, false},
		{"down saved while incomplete", DraftInput{Title: "title", SQL: "?", DownSQL: "DROP TABLE public.never_created"}, true},
		{"irreversible with down is saveable", DraftInput{Title: "title", SQL: "?", DownSQL: "DROP TABLE x", Irreversible: true}, true},
		{"down at byte limit", DraftInput{Title: "title", SQL: "?", DownSQL: strings.Repeat("a", sqlpolicy.MaxSQLBytes)}, true},
		{"down over byte limit", DraftInput{Title: "title", SQL: "?", DownSQL: strings.Repeat("a", sqlpolicy.MaxSQLBytes+1)}, false},
		{"invalid down encoding", DraftInput{Title: "title", SQL: "?", DownSQL: string([]byte{0xff})}, false},
		{"NUL down", DraftInput{Title: "title", SQL: "?", DownSQL: "DROP TABLE \x00"}, false},
		{"reason at rune limit", DraftInput{Title: "title", SQL: "?", Irreversible: true, IrreversibleReason: strings.Repeat("猫", MaxIrreversibleReasonRunes)}, true},
		{"reason over rune limit", DraftInput{Title: "title", SQL: "?", Irreversible: true, IrreversibleReason: strings.Repeat("猫", MaxIrreversibleReasonRunes+1)}, false},
		{"invalid reason encoding", DraftInput{Title: "title", SQL: "?", Irreversible: true, IrreversibleReason: string([]byte{0xff})}, false},
		{"NUL reason", DraftInput{Title: "title", SQL: "?", Irreversible: true, IrreversibleReason: "no \x00 down"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDraft(tc.input)
			if tc.valid {
				if err != nil {
					t.Fatal("valid decoded input rejected")
				}
			} else {
				RequireDraftCode(t, err, problem.ValidationFailed)
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
		RequireDraftCode(t, err, problem.InvalidRequest)
	}
	_, err := s.Create(ctx, id, actor, DraftInput{})
	RequireDraftCode(t, err, problem.ValidationFailed)
	for _, ids := range [][2]uuid.UUID{{uuid.Nil, id}, {id, uuid.Nil}} {
		_, err := s.Get(ctx, ids[0], ids[1])
		RequireDraftCode(t, err, problem.InvalidRequest)
	}
	for _, args := range [][2]int{{0, 0}, {201, 0}, {1, -1}, {1, 100001}} {
		_, _, err := s.List(ctx, id, args[0], args[1])
		RequireDraftCode(t, err, problem.InvalidRequest)
	}
	_, _, err = s.List(ctx, uuid.Nil, 50, 0)
	RequireDraftCode(t, err, problem.InvalidRequest)
	for _, ids := range [][3]uuid.UUID{{uuid.Nil, id, actor}, {id, uuid.Nil, actor}, {id, id, uuid.Nil}} {
		_, err := s.Save(ctx, ids[0], ids[1], ids[2], SaveDraftInput{DraftInput: input, ExpectedRevision: 1})
		RequireDraftCode(t, err, problem.InvalidRequest)
	}
	for _, revision := range []int64{0, -1} {
		_, err := s.Save(ctx, id, id, actor, SaveDraftInput{DraftInput: input, ExpectedRevision: revision})
		RequireDraftCode(t, err, problem.InvalidRequest)
	}
	_, err = s.Save(ctx, id, id, actor, SaveDraftInput{ExpectedRevision: 1})
	RequireDraftCode(t, err, problem.ValidationFailed)
}

func TestValidateDefinition(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input DefinitionInput
		want  problem.Code
	}{
		{"reversible", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", DownSQL: "DROP TABLE public.a"}, ""},
		{"irreversible", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", Irreversible: true, IrreversibleReason: "No safe down"}, ""},
		{"missing up", DefinitionInput{DownSQL: "DROP TABLE public.a"}, problem.ValidationFailed},
		{"invalid up", DefinitionInput{UpSQL: "CREAT TABLE public.a(id int)", DownSQL: "DROP TABLE public.a"}, problem.SQLInvalid},
		{"unsupported up", DefinitionInput{UpSQL: "BEGIN; CREATE TABLE public.a(id int); COMMIT;", DownSQL: "DROP TABLE public.a"}, problem.SQLUnsupported},
		{"reversible without down", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)"}, problem.ValidationFailed},
		{"reversible whitespace down", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", DownSQL: " \t\n"}, problem.ValidationFailed},
		{"reversible invalid down", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", DownSQL: "CREAT TABLE public.a(id int)"}, problem.SQLInvalid},
		{"reversible unsupported down", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", DownSQL: "BEGIN; DROP TABLE public.a; COMMIT;"}, problem.SQLUnsupported},
		{"reversible with reason", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", DownSQL: "DROP TABLE public.a", IrreversibleReason: "reason"}, problem.ValidationFailed},
		{"irreversible with down", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", DownSQL: "DROP TABLE public.a", Irreversible: true, IrreversibleReason: "No safe down"}, problem.ValidationFailed},
		{"irreversible blank reason", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", Irreversible: true, IrreversibleReason: " \t\n"}, problem.ValidationFailed},
		{"irreversible long reason", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", Irreversible: true, IrreversibleReason: strings.Repeat("猫", MaxIrreversibleReasonRunes+1)}, problem.ValidationFailed},
		{"oversized down", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", DownSQL: strings.Repeat("a", sqlpolicy.MaxSQLBytes+1)}, problem.ValidationFailed},
		{"invalid down encoding", DefinitionInput{UpSQL: "CREATE TABLE public.a(id int)", DownSQL: string([]byte{0xff})}, problem.ValidationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDefinition(tc.input)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid definition rejected: %v", err)
				}
				return
			}
			RequireDraftCode(t, err, tc.want)
		})
	}
}

func TestDraftStoreErrorsAreSafe(t *testing.T) {
	for _, code := range []string{"23503", "23514", "22001", "22021", "22003", "XX000", "P0001"} {
		want := problem.ValidationFailed
		if code == "XX000" {
			want = problem.PlatformUnavailable
		}
		if code == "P0001" {
			want = problem.DraftFrozen
		}
		err := draftStoreError(&pgconn.PgError{Code: code, Message: "private SQL", Detail: "private credentials"})
		RequireDraftCode(t, err, want)
	}
	RequireDraftCode(t, draftStoreError(errors.New("private DSN")), problem.PlatformUnavailable)
}
