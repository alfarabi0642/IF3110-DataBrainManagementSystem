package journal

import (
	"context"
	"errors"
	"testing"
)

func TestTargetLockRejectsMissingConnection(t *testing.T) {
	lock, acquired, err := TryTargetLock(context.Background(), nil)
	if err == nil || acquired || lock != nil {
		t.Fatal("target ownership without a connection was accepted")
	}
	var missing *TargetLock
	if err := missing.Release(context.Background()); err != nil {
		t.Fatalf("nil lock release must be a no-op: %v", err)
	}
}

func TestTargetLockErrorsAreDistinct(t *testing.T) {
	if errors.Is(ErrTargetLockHeld, ErrTargetLockLost) {
		t.Fatal("reentrant and lost lock errors must stay distinguishable")
	}
}
