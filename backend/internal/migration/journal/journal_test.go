package journal

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDeriveOwnerSecret(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	idA := uuid.New()
	idB := uuid.New()

	s1, err := DeriveOwnerSecret(key, idA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s2, err := DeriveOwnerSecret(key, idA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s1 != s2 {
		t.Fatal("owner secret must be deterministic for the same key and root execution ID")
	}
	sB, err := DeriveOwnerSecret(key, idB)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s1 == sB {
		t.Fatal("distinct execution IDs must produce distinct owner secrets")
	}
	h1 := HashJournalSecret(s1)
	if len(h1) != 32 || bytes.Equal(h1, s1[:]) {
		t.Fatal("HashJournalSecret must return a 32-byte digest distinct from the raw secret")
	}

	if _, err := DeriveOwnerSecret(key[:16], idA); err == nil {
		t.Fatal("expected short key to fail")
	}
	if _, err := DeriveOwnerSecret(key, uuid.Nil); err == nil {
		t.Fatal("expected nil execution ID to fail")
	}
}

func TestHasLockedSearchPath(t *testing.T) {
	if !hasLockedSearchPath([]string{"search_path=pg_catalog, databrain_runtime"}) {
		t.Fatal("expected exact locked search_path to pass")
	}
	for _, cfg := range [][]string{
		nil,
		{},
		{"search_path=public, databrain_runtime"},
		{"search_path=databrain_runtime, pg_catalog"},
		{"search_path=pg_catalog, databrain_runtime, public"},
		{"work_mem=4MB"},
	} {
		if hasLockedSearchPath(cfg) {
			t.Fatalf("unsafe proconfig accepted: %v", cfg)
		}
	}
}

func TestJournalInputValidation(t *testing.T) {
	ctx := context.Background()
	if err := VerifyTargetJournal(ctx, nil); !errors.Is(err, ErrTargetConnectionRequired) {
		t.Fatalf("expected ErrTargetConnectionRequired, got %v", err)
	}
	if _, err := ReadJournalOwner(ctx, nil); !errors.Is(err, ErrTargetConnectionRequired) {
		t.Fatalf("expected ErrTargetConnectionRequired, got %v", err)
	}
	if _, err := ReadJournalCommits(ctx, nil, uuid.New()); !errors.Is(err, ErrTargetConnectionRequired) {
		t.Fatalf("expected ErrTargetConnectionRequired, got %v", err)
	}
	var zeroSecret [32]byte
	if err := AcquireJournalOwner(ctx, nil, uuid.New(), uuid.New(), strings.Repeat("a", 64), zeroSecret); !errors.Is(err, ErrTargetConnectionRequired) {
		t.Fatalf("expected ErrTargetConnectionRequired, got %v", err)
	}
	if _, err := ArmJournalTx(ctx, nil, uuid.New(), uuid.New(), strings.Repeat("a", 64)); !errors.Is(err, ErrTargetConnectionRequired) {
		t.Fatalf("expected ErrTargetConnectionRequired, got %v", err)
	}
	var nilGuard *JournalTxGuard
	if err := nilGuard.RecordCommit(ctx, nil, 0, "apply", 1, nil); !errors.Is(err, ErrJournalAuth) {
		t.Fatalf("expected ErrJournalAuth for nil guard, got %v", err)
	}
	consumed := &JournalTxGuard{consumed: true}
	if err := consumed.RecordCommit(ctx, nil, 0, "apply", 1, nil); !errors.Is(err, ErrJournalAuth) {
		t.Fatalf("expected ErrJournalAuth for consumed guard, got %v", err)
	}
}
