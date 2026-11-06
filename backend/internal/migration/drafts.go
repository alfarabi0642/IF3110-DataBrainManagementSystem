package migration

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MaxSQLBytes = 128 << 10

// DraftSummary tidak memuat SQL pada hasil daftar.
type DraftSummary struct {
	ID, TargetID, CreatedBy, UpdatedBy uuid.UUID
	Title, UpdatedByUsername           string
	Revision                           int64
	CreatedAt, UpdatedAt               time.Time
}

type Draft struct {
	DraftSummary
	SQL string
}

// DraftInput mempertahankan nilai form yang sudah di-decode tanpa normalisasi.
type DraftInput struct {
	Title, SQL string
}

type SaveDraftInput struct {
	DraftInput
	ExpectedRevision int64
}

// DraftStore hanya memakai pool platform. Pemanggil wajib mengotorisasi MigrationWrite pada target dan
// mengambil actorID dari session.
type DraftStore struct {
	pool *pgxpool.Pool
}

func NewDraftStore(pool *pgxpool.Pool) *DraftStore { return &DraftStore{pool: pool} }

func draftFailure(code problem.Code) error { return &problem.Error{Code: code} }

func validateDraft(input DraftInput) error {
	if !utf8.ValidString(input.Title) || strings.TrimSpace(input.Title) == "" ||
		utf8.RuneCountInString(input.Title) > 120 || strings.ContainsRune(input.Title, 0) ||
		!utf8.ValidString(input.SQL) || len(input.SQL) < 1 || len(input.SQL) > MaxSQLBytes ||
		strings.ContainsRune(input.SQL, 0) {
		return draftFailure(problem.ValidationFailed)
	}
	return nil
}

func draftStoreError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503", "23514", "22001", "22021", "22003":
			return draftFailure(problem.ValidationFailed)
		}
	}
	return draftFailure(problem.PlatformUnavailable)
}

const draftSummaryColumns = `id,target_id,title,revision,created_by,updated_by,created_at,updated_at`
const draftColumns = draftSummaryColumns + `,sql`

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	err := row.Scan(&d.ID, &d.TargetID, &d.Title, &d.Revision, &d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt, &d.SQL)
	if err != nil {
		return Draft{}, err
	}
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return d, nil
}

func (s *DraftStore) Create(ctx context.Context, targetID, actorID uuid.UUID, input DraftInput) (Draft, error) {
	if targetID == uuid.Nil || actorID == uuid.Nil {
		return Draft{}, draftFailure(problem.InvalidRequest)
	}
	if err := validateDraft(input); err != nil {
		return Draft{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return Draft{}, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	d, err := scanDraft(lease.QueryRow(ctx, `INSERT INTO databrain.migration_drafts
	    (id,target_id,title,sql,created_by,updated_by)
	    SELECT $1,id,$3,$4,$5,$5 FROM databrain.database_targets WHERE id=$2
	    RETURNING `+draftColumns, uuid.New(), targetID, input.Title, input.SQL, actorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, draftFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return Draft{}, draftStoreError(err)
	}
	return d, nil
}

// List memakai urutan updated_at DESC/id dan limit+1 untuk menandai halaman berikutnya. Default
// pagination ditentukan pemanggil; limit nol tidak dianggap default.
func (s *DraftStore) List(ctx context.Context, targetID uuid.UUID, limit, offset int) ([]DraftSummary, bool, error) {
	if targetID == uuid.Nil || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return nil, false, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	rows, err := lease.Query(ctx, `SELECT `+draftSummaryColumns+` FROM databrain.migration_drafts
	    WHERE target_id=$1 ORDER BY updated_at DESC,id LIMIT $2 OFFSET $3`, targetID, limit+1, offset)
	if err != nil {
		return nil, false, draftStoreError(err)
	}
	defer rows.Close()
	items := make([]DraftSummary, 0, limit)
	for rows.Next() {
		var d DraftSummary
		if err := rows.Scan(&d.ID, &d.TargetID, &d.Title, &d.Revision, &d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, false, draftStoreError(err)
		}
		d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
		items = append(items, d)
	}
	if rows.Err() != nil {
		return nil, false, draftStoreError(rows.Err())
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}

func (s *DraftStore) Get(ctx context.Context, targetID, draftID uuid.UUID) (Draft, error) {
	if targetID == uuid.Nil || draftID == uuid.Nil {
		return Draft{}, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return Draft{}, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	d, err := scanDraft(lease.QueryRow(ctx, `SELECT `+draftColumns+` FROM databrain.migration_drafts WHERE target_id=$1 AND id=$2`, targetID, draftID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, draftFailure(problem.DraftNotFound)
	}
	if err != nil {
		return Draft{}, draftStoreError(err)
	}
	return d, nil
}

// Save memperbarui revision yang diharapkan secara atomik, termasuk untuk teks identik. Penulis dengan
// revision lama tidak menimpa data baru. SQL tidak diparsing atau dijalankan.
func (s *DraftStore) Save(ctx context.Context, targetID, draftID, actorID uuid.UUID, input SaveDraftInput) (Draft, error) {
	if targetID == uuid.Nil || draftID == uuid.Nil || actorID == uuid.Nil || input.ExpectedRevision < 1 {
		return Draft{}, draftFailure(problem.InvalidRequest)
	}
	if err := validateDraft(input.DraftInput); err != nil {
		return Draft{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return Draft{}, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	d, err := scanDraft(lease.QueryRow(ctx, `UPDATE databrain.migration_drafts
	    SET title=$4,sql=$5,updated_by=$6,updated_at=now(),revision=revision+1
	    WHERE target_id=$1 AND id=$2 AND revision::bigint=$3 RETURNING `+draftColumns,
		targetID, draftID, input.ExpectedRevision, input.Title, input.SQL, actorID))
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := lease.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM databrain.migration_drafts WHERE target_id=$1 AND id=$2)`, targetID, draftID).Scan(&exists); err != nil {
			return Draft{}, draftStoreError(err)
		}
		if !exists {
			return Draft{}, draftFailure(problem.DraftNotFound)
		}
		return Draft{}, draftFailure(problem.RevisionConflict)
	}
	if err != nil {
		return Draft{}, draftStoreError(err)
	}
	return d, nil
}
