package access

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestPrincipalContext(t *testing.T) {
	base := context.Background()
	if _, ok := FromContext(base); ok {
		t.Fatal("missing principal reported as present")
	}
	p := Principal{UserID: uuid.New(), SessionID: uuid.New(), SecurityVersion: 3}
	ctx := WithPrincipal(base, p)
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if got, ok := FromContext(child); !ok || got != p {
		t.Fatalf("derived context lost principal: %+v, %v", got, ok)
	}
	p.SecurityVersion++
	if got, ok := FromContext(WithPrincipal(child, p)); !ok || got != p {
		t.Fatalf("replacement principal unavailable: %+v, %v", got, ok)
	}
	if _, ok := FromContext(base); ok {
		t.Fatal("principal leaked into parent context")
	}
}
