package targets

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Target is safe for an authorized admin page. It contains no password or
// ciphertext. User-facing database lists must use their own smaller projection.
type Target struct {
	ID, CreatedBy                      uuid.UUID
	Name, Host, DatabaseName, Username string
	SSLMode                            string
	Port                               int
	IsActive, PasswordStored           bool
	ConfigRevision                     int64
	CreatedAt, UpdatedAt               time.Time
}

type storedTarget struct {
	Target
	sealed []byte
}

const targetColumns = `id,name,host,port,database_name,username,ssl_mode,is_active,config_revision,created_by,created_at,updated_at,password_sealed`

func scanTarget(row pgx.Row) (storedTarget, error) {
	var s storedTarget
	err := row.Scan(&s.ID, &s.Name, &s.Host, &s.Port, &s.DatabaseName, &s.Username,
		&s.SSLMode, &s.IsActive, &s.ConfigRevision, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt, &s.sealed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storedTarget{}, failure(problem.DatabaseNotFound)
		}
		return storedTarget{}, storeError(err)
	}
	s.PasswordStored = len(s.sealed) > 0
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	return s, nil
}

func storeError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return failure(problem.TargetConflict)
		case "23503", "23514", "22001":
			return failure(problem.ValidationFailed)
		}
	}
	return failure(problem.PlatformUnavailable)
}

func rollback(tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(cleanup)
}

func (p *Provider) load(ctx context.Context, targetID uuid.UUID) (storedTarget, error) {
	if targetID == uuid.Nil {
		return storedTarget{}, failure(problem.InvalidRequest)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(bounded, p.pool)
	if err != nil {
		return storedTarget{}, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	return scanTarget(lease.QueryRow(bounded, "SELECT "+targetColumns+" FROM databrain.database_targets WHERE id=$1", targetID))
}

func (p *Provider) credentials(s storedTarget) (CreateInput, error) {
	password, err := p.secrets.OpenTarget(s.ID, s.sealed)
	if err != nil {
		return CreateInput{}, failure(problem.TargetUnavailable)
	}
	return p.validate(CreateInput{Name: s.Name, Host: s.Host, Port: s.Port, DatabaseName: s.DatabaseName,
		Username: s.Username, Password: string(password), SSLMode: s.SSLMode})
}

func (p *Provider) Get(ctx context.Context, targetID uuid.UUID) (Target, error) {
	s, err := p.load(ctx, targetID)
	return s.Target, err
}

// Create probes before acquiring a platform transaction. Target and stream are
// inserted atomically; the unique identity also covers inactive records.
func (p *Provider) Create(ctx context.Context, actorID uuid.UUID, input CreateInput) (Target, error) {
	if actorID == uuid.Nil {
		return Target{}, failure(problem.InvalidRequest)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	in, err := p.validate(input)
	if err != nil {
		return Target{}, err
	}
	if _, err = p.Probe(bounded, in); err != nil {
		return Target{}, err
	}
	id := uuid.New()
	sealed, err := p.secrets.SealTarget(id, []byte(in.Password))
	if err != nil {
		return Target{}, failure(problem.InternalError)
	}
	lease, err := platformdb.Acquire(bounded, p.pool)
	if err != nil {
		return Target{}, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.Begin(bounded)
	if err != nil {
		return Target{}, failure(problem.PlatformUnavailable)
	}
	defer rollback(tx)
	s, err := scanTarget(tx.QueryRow(bounded, `INSERT INTO databrain.database_targets
	    (id,name,host,port,database_name,username,password_sealed,ssl_mode,created_by)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+targetColumns,
		id, in.Name, in.Host, in.Port, in.DatabaseName, in.Username, sealed, in.SSLMode, actorID))
	if err != nil {
		return Target{}, err
	}
	if _, err = tx.Exec(bounded, "INSERT INTO databrain.migration_streams(target_id) VALUES ($1)", id); err != nil {
		return Target{}, storeError(err)
	}
	if err = tx.Commit(bounded); err != nil {
		return Target{}, failure(problem.PlatformUnavailable)
	}
	return s.Target, nil
}

// Edit probes changed credentials/TLS and reactivation before taking locks.
// The subsequent stream lock and revision recheck reject races without holding
// a platform transaction across any target network operation.
func (p *Provider) Edit(ctx context.Context, targetID uuid.UUID, input EditInput) (Target, error) {
	if input.ExpectedRevision < 1 {
		return Target{}, failure(problem.InvalidRequest)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	previous, err := p.load(bounded, targetID)
	if err != nil {
		return Target{}, err
	}
	if previous.ConfigRevision != input.ExpectedRevision {
		return Target{}, failure(problem.RevisionConflict)
	}
	old, err := p.credentials(previous)
	if err != nil {
		return Target{}, err
	}
	in := old
	in.Name, in.Username, in.SSLMode = strings.TrimSpace(input.Name), input.Username, input.SSLMode
	if input.Password != "" {
		in.Password = input.Password
	}
	in, err = p.validate(in)
	if err != nil {
		return Target{}, err
	}
	connectionChanged := in.Username != old.Username || in.Password != old.Password || in.SSLMode != old.SSLMode
	if connectionChanged || !previous.IsActive && input.IsActive {
		if _, err = p.Probe(bounded, in); err != nil {
			return Target{}, err
		}
	}
	sealed := previous.sealed
	if in.Password != old.Password {
		sealed, err = p.secrets.SealTarget(targetID, []byte(in.Password))
		if err != nil {
			return Target{}, failure(problem.InternalError)
		}
	}
	lease, err := platformdb.Acquire(bounded, p.pool)
	if err != nil {
		return Target{}, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.Begin(bounded)
	if err != nil {
		return Target{}, failure(problem.PlatformUnavailable)
	}
	defer rollback(tx)
	var busy bool
	err = tx.QueryRow(bounded, "SELECT active_attempt_id IS NOT NULL FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE", targetID).Scan(&busy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, failure(problem.DatabaseNotFound)
	}
	if err != nil {
		return Target{}, storeError(err)
	}
	if busy {
		return Target{}, failure(problem.TargetBusy)
	}
	current, err := scanTarget(tx.QueryRow(bounded, "SELECT "+targetColumns+" FROM databrain.database_targets WHERE id=$1 FOR UPDATE", targetID))
	if err != nil {
		return Target{}, err
	}
	if current.ConfigRevision != input.ExpectedRevision {
		return Target{}, failure(problem.RevisionConflict)
	}
	if in.Name != old.Name || connectionChanged || input.IsActive != current.IsActive {
		current, err = scanTarget(tx.QueryRow(bounded, `UPDATE databrain.database_targets SET
	        name=$2,username=$3,password_sealed=$4,ssl_mode=$5,is_active=$6,
	        config_revision=config_revision+1,updated_at=now() WHERE id=$1 AND config_revision=$7 RETURNING `+targetColumns,
			targetID, in.Name, in.Username, sealed, in.SSLMode, input.IsActive, input.ExpectedRevision))
		if err != nil {
			return Target{}, err
		}
	}
	if err = tx.Commit(bounded); err != nil {
		return Target{}, failure(problem.PlatformUnavailable)
	}
	return current.Target, nil
}
