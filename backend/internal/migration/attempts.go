package migration

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AttemptReceipt identifies a durable attempt without loading its SQL snapshot.
type AttemptReceipt struct {
	ID, TargetID, DraftID, ActorID, RequestKey uuid.UUID
	DraftRevision                              int64
	Version, State                             string
}

type ReserveInput struct {
	ExpectedRevision int64
	RequestKey       uuid.UUID
}

// Reservation grants one claim only to the call that created this attempt.
// Copies share the same single-use permit; receipts read later have no permit.
type Reservation struct {
	Receipt AttemptReceipt
	creator *claimPermit
}

type claimPermit struct {
	store   *AttemptStore
	receipt AttemptReceipt
	used    atomic.Bool
}

func (r Reservation) Created() bool { return r.creator != nil }

// AttemptStore never connects to a target. The caller must authorize execute
// before Reserve and recheck authorization before starting a target transaction.
type AttemptStore struct{ pool *pgxpool.Pool }

func NewAttemptStore(pool *pgxpool.Pool) *AttemptStore { return &AttemptStore{pool: pool} }

func attemptFailure(code problem.Code) error { return &problem.Error{Code: code} }

func attemptStoreError(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.ConstraintName {
		case "one_unresolved_attempt_per_target":
			return attemptFailure(problem.TargetBusy)
		case "one_success_per_draft_revision":
			return attemptFailure(problem.DraftAlreadyExecuted)
		}
	}
	return attemptFailure(problem.PlatformUnavailable)
}

const receiptColumns = `id,target_id,draft_id,executor_id,request_key,draft_revision,version::text,state`

func scanReceipt(row pgx.Row) (*AttemptReceipt, error) {
	var r AttemptReceipt
	err := row.Scan(&r.ID, &r.TargetID, &r.DraftID, &r.ActorID, &r.RequestKey, &r.DraftRevision, &r.Version, &r.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, attemptStoreError(err)
	}
	return &r, nil
}

func lookupReceipt(ctx context.Context, db platformdb.DBTX, targetID, actorID, key uuid.UUID) (*AttemptReceipt, error) {
	return scanReceipt(db.QueryRow(ctx, `SELECT `+receiptColumns+` FROM databrain.migration_attempts
	    WHERE target_id=$1 AND executor_id=$2 AND request_key=$3`, targetID, actorID, key))
}

// Receipt returns nil when this actor has no receipt for this target/key.
// Absence alone does not prove that an earlier request cannot still commit.
// Callers must authorize the target and obtain actorID from the validated session.
func (s *AttemptStore) Receipt(ctx context.Context, targetID, actorID, key uuid.UUID) (*AttemptReceipt, error) {
	if targetID == uuid.Nil || actorID == uuid.Nil || key == uuid.Nil {
		return nil, attemptFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return nil, attemptFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	return lookupReceipt(ctx, lease, targetID, actorID, key)
}

func matchReservation(r AttemptReceipt, draftID uuid.UUID, revision int64) (Reservation, error) {
	if r.DraftID != draftID || r.DraftRevision != revision {
		return Reservation{}, attemptFailure(problem.IdempotencyConflict)
	}
	return Reservation{Receipt: r}, nil
}

func (s *AttemptStore) created(r AttemptReceipt) Reservation {
	return Reservation{Receipt: r, creator: &claimPermit{store: s, receipt: r}}
}

func rollbackAttempt(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// Discard the connection whose commit acknowledgment was lost before looking
// for durable proof on another connection. Never retry the mutation here.
func closeUncertainAttemptConn(lease *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = lease.Hijack().Close(ctx)
}

func (s *AttemptStore) recoverReservation(targetID, draftID, actorID, key, createdID uuid.UUID, revision int64) (Reservation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := s.Receipt(ctx, targetID, actorID, key)
	if err != nil || r == nil {
		return Reservation{}, attemptFailure(problem.PlatformUnavailable)
	}
	reservation, err := matchReservation(*r, draftID, revision)
	if err == nil && r.ID == createdID && r.State == "reserved" {
		reservation = s.created(*r)
	}
	return reservation, err
}

func (s *AttemptStore) Reserve(ctx context.Context, targetID, draftID, actorID uuid.UUID, input ReserveInput) (Reservation, error) {
	if targetID == uuid.Nil || draftID == uuid.Nil || actorID == uuid.Nil || input.RequestKey == uuid.Nil || input.ExpectedRevision < 1 {
		return Reservation{}, attemptFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Replay precedes current draft, target status and stream contention.
	r, err := s.Receipt(ctx, targetID, actorID, input.RequestKey)
	if err != nil {
		return Reservation{}, err
	}
	if r != nil {
		return matchReservation(*r, draftID, input.ExpectedRevision)
	}
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return Reservation{}, attemptFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Reservation{}, attemptFailure(problem.PlatformUnavailable)
	}
	defer rollbackAttempt(tx)
	var version int64
	var busy bool
	err = tx.QueryRow(ctx, `SELECT next_version,active_attempt_id IS NOT NULL
	    FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(&version, &busy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, attemptFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return Reservation{}, attemptStoreError(err)
	}
	// A separate READ COMMITTED command sees the winner after waiting for its
	// stream lock; the lock query's snapshot may predate that winner's commit.
	r, err = lookupReceipt(ctx, tx, targetID, actorID, input.RequestKey)
	if err != nil {
		return Reservation{}, err
	}
	if r != nil {
		return matchReservation(*r, draftID, input.ExpectedRevision)
	}
	if busy {
		return Reservation{}, attemptFailure(problem.TargetBusy)
	}
	draft, err := scanDraft(tx.QueryRow(ctx, `SELECT `+draftColumns+` FROM databrain.migration_drafts
	    WHERE target_id=$1 AND id=$2 FOR UPDATE`, targetID, draftID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, attemptFailure(problem.DraftNotFound)
	}
	if err != nil {
		return Reservation{}, attemptStoreError(err)
	}
	if draft.Revision != input.ExpectedRevision {
		return Reservation{}, attemptFailure(problem.RevisionConflict)
	}
	var targetName string
	var configRevision int64
	err = tx.QueryRow(ctx, `SELECT name,config_revision FROM databrain.database_targets
	    WHERE id=$1 AND is_active FOR SHARE`, targetID).Scan(&targetName, &configRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, attemptFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return Reservation{}, attemptStoreError(err)
	}
	if _, err := ValidateSQL(draft.SQL); err != nil {
		return Reservation{}, err
	}
	var succeeded bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM databrain.migration_attempts
	    WHERE target_id=$1 AND draft_id=$2 AND draft_revision=$3 AND state='succeeded')`, targetID, draftID, draft.Revision).Scan(&succeeded)
	if err != nil {
		return Reservation{}, attemptStoreError(err)
	}
	if succeeded {
		return Reservation{}, attemptFailure(problem.DraftAlreadyExecuted)
	}
	var username string
	if err := tx.QueryRow(ctx, `SELECT username FROM databrain.users WHERE id=$1`, actorID).Scan(&username); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Reservation{}, attemptFailure(problem.InvalidRequest)
		}
		return Reservation{}, attemptStoreError(err)
	}
	id, hash := uuid.New(), sha256.Sum256([]byte(draft.SQL))
	r, err = scanReceipt(tx.QueryRow(ctx, `INSERT INTO databrain.migration_attempts
	    (id,target_id,draft_id,draft_revision,version,title_snapshot,target_name_snapshot,
	     target_config_revision,sql_snapshot,sql_sha256,executor_id,executor_username_snapshot,request_key,state)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'reserved') RETURNING `+receiptColumns,
		id, targetID, draftID, draft.Revision, version, draft.Title, targetName, configRevision, draft.SQL, hash[:], actorID, username, input.RequestKey))
	if err != nil {
		return Reservation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.migration_streams SET next_version=next_version+1,active_attempt_id=$2 WHERE target_id=$1`, targetID, id); err != nil {
		return Reservation{}, attemptStoreError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		closeUncertainAttemptConn(lease)
		return s.recoverReservation(targetID, draftID, actorID, input.RequestKey, id, input.ExpectedRevision)
	}
	return s.created(*r), nil
}

func (s *AttemptStore) recoverClaim(r AttemptReceipt) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stored, err := s.Receipt(ctx, r.TargetID, r.ActorID, r.RequestKey)
	if err != nil || stored == nil || stored.ID != r.ID || stored.State != "running" {
		return false, attemptFailure(problem.PlatformUnavailable)
	}
	return true, nil
}

// Claim returns true only after the creator's reserved -> running transition
// is acknowledged or proved durable. False/error never authorizes target SQL.
// A consumed permit cannot retry, including after an inconclusive commit.
func (s *AttemptStore) Claim(ctx context.Context, reservation Reservation) (bool, error) {
	p := reservation.creator
	if p == nil || p.store != s || !p.used.CompareAndSwap(false, true) {
		return false, nil
	}
	r := p.receipt // Do not trust a caller-modified copy of the public receipt.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return false, attemptFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, attemptFailure(problem.PlatformUnavailable)
	}
	defer rollbackAttempt(tx)
	tag, err := tx.Exec(ctx, `UPDATE databrain.migration_attempts SET state='running',started_at=clock_timestamp()
	    WHERE target_id=$1 AND id=$2 AND state='reserved'`, r.TargetID, r.ID)
	if err != nil {
		return false, attemptStoreError(err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		closeUncertainAttemptConn(lease)
		return s.recoverClaim(r)
	}
	return true, nil
}
