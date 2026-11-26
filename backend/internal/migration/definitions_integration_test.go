package migration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/legacy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

func TestDefinitionSnapshotPostgres(t *testing.T) {
	f := newDraftFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := f.store
	t.Run("reversible snapshot freezes the draft and replays by revision", func(t *testing.T) {
		d, err := s.Create(ctx, f.a, f.actor, DraftInput{
			Title: "Reversible", SQL: "CREATE TABLE public.definition_up(id int)",
			DownSQL: "DROP TABLE public.definition_up",
		})
		if err != nil {
			t.Fatal(err)
		}
		def, err := s.SnapshotDefinition(ctx, f.a, d.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if def.ID == uuid.Nil || def.TargetID != f.a || def.DraftID != d.ID || def.Revision != 1 ||
			def.UpSQL != d.SQL || def.DownSQL != d.DownSQL || def.Irreversible || def.IrreversibleReason != "" ||
			def.Source != "submission" || !bytes.Equal(def.UpSHA256, sha256Bytes(d.SQL)) || !bytes.Equal(def.DownSHA256, sha256Bytes(d.DownSQL)) {
			t.Fatalf("definition snapshot wrong: %+v", def)
		}
		read, err := s.Get(ctx, f.a, d.ID)
		if err != nil || !read.Frozen || read.SQL != d.SQL || read.DownSQL != d.DownSQL || read.Irreversible {
			t.Fatalf("draft did not become frozen: %+v %v", read, err)
		}
		_, err = s.Save(ctx, f.a, d.ID, f.otherActor, SaveDraftInput{DraftInput: DraftInput{Title: "changed", SQL: "SELECT 1"}, ExpectedRevision: 1})
		RequireDraftCode(t, err, problem.DraftFrozen)
		replay, err := s.SnapshotDefinition(ctx, f.a, d.ID, 1)
		if err != nil || replay.ID != def.ID {
			t.Fatalf("identical submission did not replay: %+v %v", replay, err)
		}
		if _, err := f.db.Pool.Exec(ctx, `INSERT INTO databrain.migration_streams (target_id) VALUES ($1) ON CONFLICT (target_id) DO NOTHING`, f.a); err != nil {
			t.Fatal("cannot initialize stream fixture")
		}
		if _, err := legacy.NewAttemptStore(s.Pool()).Reserve(ctx, f.a, d.ID, f.actor, legacy.ReserveInput{ExpectedRevision: 1, RequestKey: uuid.New()}); err == nil {
			t.Fatal("M1 route executed a submitted definition")
		} else {
			RequireDraftCode(t, err, problem.DraftFrozen)
		}
	})
	t.Run("irreversible snapshot omits down and stores the reason", func(t *testing.T) {
		d, err := s.Create(ctx, f.a, f.actor, DraftInput{
			Title: "Irreversible", SQL: "CREATE TABLE public.definition_irreversible(id int)",
			Irreversible: true, IrreversibleReason: "No safe down.",
		})
		if err != nil {
			t.Fatal(err)
		}
		def, err := s.SnapshotDefinition(ctx, f.a, d.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !def.Irreversible || def.DownSQL != "" || def.IrreversibleReason != "No safe down." || def.DownSHA256 != nil {
			t.Fatalf("irreversible definition wrong: %+v", def)
		}
	})
	t.Run("incomplete definitions fail without freezing", func(t *testing.T) {
		d, err := s.Create(ctx, f.a, f.actor, DraftInput{Title: "No down", SQL: "CREATE TABLE public.definition_missing(id int)"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.SnapshotDefinition(ctx, f.a, d.ID, 1)
		RequireDraftCode(t, err, problem.ValidationFailed)
		read, err := s.Get(ctx, f.a, d.ID)
		if err != nil || read.Frozen {
			t.Fatalf("invalid submission froze draft: %+v %v", read, err)
		}
		_, err = s.SnapshotDefinition(ctx, f.a, d.ID, 2)
		RequireDraftCode(t, err, problem.RevisionConflict)
		_, err = s.SnapshotDefinition(ctx, f.b, d.ID, 1)
		RequireDraftCode(t, err, problem.DraftNotFound)
		irreversible, err := s.Create(ctx, f.a, f.actor, DraftInput{
			Title: "Blank reason", SQL: "CREATE TABLE public.definition_blank(id int)", Irreversible: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.SnapshotDefinition(ctx, f.a, irreversible.ID, 1)
		RequireDraftCode(t, err, problem.ValidationFailed)
	})
	t.Run("accepted attempt freezes without a definition", func(t *testing.T) {
		d, err := s.Create(ctx, f.a, f.actor, DraftInput{Title: "Attempted", SQL: "CREATE TABLE public.definition_attempt(id int)"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Pool.Exec(ctx, `INSERT INTO databrain.migration_streams (target_id) VALUES ($1) ON CONFLICT (target_id) DO NOTHING`, f.a); err != nil {
			t.Fatal("cannot initialize stream fixture")
		}
		if _, err := legacy.NewAttemptStore(s.Pool()).Reserve(ctx, f.a, d.ID, f.actor, legacy.ReserveInput{ExpectedRevision: 1, RequestKey: uuid.New()}); err != nil {
			t.Fatal(err)
		}
		read, err := s.Get(ctx, f.a, d.ID)
		if err != nil || !read.Frozen {
			t.Fatalf("attempt did not freeze draft: %+v %v", read, err)
		}
		_, err = s.Save(ctx, f.a, d.ID, f.actor, SaveDraftInput{DraftInput: DraftInput{Title: "changed", SQL: "SELECT 1"}, ExpectedRevision: 1})
		RequireDraftCode(t, err, problem.DraftFrozen)
		_, err = s.SnapshotDefinition(ctx, f.a, d.ID, 1)
		RequireDraftCode(t, err, problem.DraftFrozen)
	})
}

func sha256Bytes(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
