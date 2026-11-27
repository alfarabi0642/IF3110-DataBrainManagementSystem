package migration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/journal"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/jackc/pgx/v5"
)

func secondTargetConn(t *testing.T, f draftFixture) *pgx.Conn {
	t.Helper()
	cfg := f.target.Config().Copy()
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal("cannot open a second target connection")
	}
	t.Cleanup(func() { closeDraftTestConn(conn) })
	return conn
}

func acquireTargetLock(t *testing.T, ctx context.Context, conn *pgx.Conn) *journal.TargetLock {
	t.Helper()
	lock, acquired, err := journal.TryTargetLock(ctx, conn)
	if err != nil || !acquired || lock == nil {
		t.Fatalf("target ownership not acquired: %v", err)
	}
	return lock
}

func TestTargetLockPostgres(t *testing.T) {
	f := newDraftFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("exclusive ownership, reentrant misuse and release", func(t *testing.T) {
		second := secondTargetConn(t, f)
		lock := acquireTargetLock(t, ctx, f.target)
		defer func() { _ = lock.Release(ctx) }()
		other, acquired, err := journal.TryTargetLock(ctx, second)
		if err != nil || acquired || other != nil {
			t.Fatalf("second connection acquired the same target ownership: %v", err)
		}
		if _, _, err := journal.TryTargetLock(ctx, f.target); !errors.Is(err, journal.ErrTargetLockHeld) {
			t.Fatalf("reentrant acquire was not identified: %v", err)
		}
		if err := lock.Release(ctx); err != nil {
			t.Fatal(err)
		}
		if err := lock.Release(ctx); err != nil {
			t.Fatalf("released lock was not idempotent: %v", err)
		}
		next := acquireTargetLock(t, ctx, second)
		if err := next.Release(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("session ownership survives transaction boundaries", func(t *testing.T) {
		second := secondTargetConn(t, f)
		lock := acquireTargetLock(t, ctx, f.target)
		defer func() { _ = lock.Release(ctx) }()
		tx, err := f.target.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if other, acquired, err := journal.TryTargetLock(ctx, second); err != nil || acquired || other != nil {
			t.Fatalf("transaction boundary released target ownership: %v", err)
		}
	})

	t.Run("closing the connection releases ownership", func(t *testing.T) {
		owner := secondTargetConn(t, f)
		_ = acquireTargetLock(t, ctx, owner)
		if err := owner.Close(ctx); err != nil {
			t.Fatal(err)
		}
		replacement := secondTargetConn(t, f)
		deadline := time.Now().Add(2 * time.Second)
		for {
			next, acquired, err := journal.TryTargetLock(ctx, replacement)
			if err != nil {
				t.Fatal(err)
			}
			if acquired {
				if err := next.Release(ctx); err != nil {
					t.Fatal(err)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("closed connection retained target ownership")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("different physical database does not contend", func(t *testing.T) {
		lock := acquireTargetLock(t, ctx, f.target)
		defer func() { _ = lock.Release(ctx) }()
		lease, err := platformdb.Acquire(ctx, f.db.Pool)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		other := acquireTargetLock(t, ctx, lease.Conn())
		if err := other.Release(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
