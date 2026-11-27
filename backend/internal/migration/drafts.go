package migration

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	MaxIrreversibleReasonRunes = 500
)

// DraftSummary tidak memuat SQL pada hasil daftar.
type DraftSummary struct {
	ID, TargetID, CreatedBy, UpdatedBy uuid.UUID
	Title, UpdatedByUsername           string
	Revision                           int64
	Frozen                             bool
	CreatedAt, UpdatedAt               time.Time
}

type Draft struct {
	DraftSummary
	SQL                string
	DownSQL            string
	Irreversible       bool
	IrreversibleReason string
}

// DraftInput mempertahankan nilai form yang sudah di-decode tanpa normalisasi. SQL adalah arah up.
type DraftInput struct {
	Title, SQL, DownSQL string
	Irreversible        bool
	IrreversibleReason  string
}

// DefinitionInput adalah isi immutable yang divalidasi saat submission. Reversible membutuhkan down
// yang valid; irreversible membutuhkan alasan nonblank dan down kosong.
type DefinitionInput struct {
	UpSQL, DownSQL     string
	Irreversible       bool
	IrreversibleReason string
}

type Definition struct {
	ID, TargetID, DraftID uuid.UUID
	Revision              int64
	UpSQL, DownSQL        string
	Irreversible          bool
	IrreversibleReason    string
	UpSHA256, DownSHA256  []byte
	Source                string
	CreatedAt             time.Time
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

func draftReason(reason string) any {
	if reason == "" {
		return nil
	}
	return reason
}

func validateDraft(input DraftInput) error {
	if !utf8.ValidString(input.Title) || strings.TrimSpace(input.Title) == "" ||
		utf8.RuneCountInString(input.Title) > 120 || strings.ContainsRune(input.Title, 0) ||
		!utf8.ValidString(input.SQL) || len(input.SQL) < 1 || len(input.SQL) > sqlpolicy.MaxSQLBytes ||
		strings.ContainsRune(input.SQL, 0) {
		return draftFailure(problem.ValidationFailed)
	}
	if input.DownSQL != "" && (!utf8.ValidString(input.DownSQL) || len(input.DownSQL) > sqlpolicy.MaxSQLBytes ||
		strings.ContainsRune(input.DownSQL, 0)) {
		return draftFailure(problem.ValidationFailed)
	}
	if input.IrreversibleReason != "" && (!utf8.ValidString(input.IrreversibleReason) ||
		utf8.RuneCountInString(input.IrreversibleReason) > MaxIrreversibleReasonRunes ||
		strings.ContainsRune(input.IrreversibleReason, 0)) {
		return draftFailure(problem.ValidationFailed)
	}
	return nil
}

// ValidateDefinition memvalidasi definisi saat submission, bukan saat simpan draf. Down divalidasi
// dengan kebijakan parser yang sama seperti up; eksekusi target tetap ditutup oleh gate journal.
func ValidateDefinition(input DefinitionInput) error {
	if !utf8.ValidString(input.UpSQL) || len(input.UpSQL) < 1 || len(input.UpSQL) > sqlpolicy.MaxSQLBytes ||
		strings.ContainsRune(input.UpSQL, 0) {
		return draftFailure(problem.ValidationFailed)
	}
	if _, err := sqlpolicy.ValidateSQL(input.UpSQL); err != nil {
		return err
	}
	if input.Irreversible {
		if input.DownSQL != "" {
			return draftFailure(problem.ValidationFailed)
		}
		if !utf8.ValidString(input.IrreversibleReason) || strings.TrimSpace(input.IrreversibleReason) == "" ||
			utf8.RuneCountInString(input.IrreversibleReason) > MaxIrreversibleReasonRunes ||
			strings.ContainsRune(input.IrreversibleReason, 0) {
			return draftFailure(problem.ValidationFailed)
		}
		return nil
	}
	if strings.TrimSpace(input.IrreversibleReason) != "" || strings.TrimSpace(input.DownSQL) == "" {
		return draftFailure(problem.ValidationFailed)
	}
	if !utf8.ValidString(input.DownSQL) || len(input.DownSQL) > sqlpolicy.MaxSQLBytes || strings.ContainsRune(input.DownSQL, 0) {
		return draftFailure(problem.ValidationFailed)
	}
	if _, err := sqlpolicy.ValidateSQL(input.DownSQL); err != nil {
		return err
	}
	return nil
}

func draftStoreError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503", "23514", "22001", "22021", "22003":
			return draftFailure(problem.ValidationFailed)
		case "P0001":
			return draftFailure(problem.DraftFrozen)
		}
	}
	return draftFailure(problem.PlatformUnavailable)
}

const draftSummaryColumns = `id,target_id,title,revision,created_by,updated_by,created_at,updated_at`
const draftColumns = draftSummaryColumns + `,sql,down_sql,irreversible,coalesce(irreversible_reason,'')`
const frozenDraftExpression = `(EXISTS (SELECT 1 FROM databrain.migration_attempts a
    WHERE a.target_id=d.target_id AND a.draft_id=d.id)
    OR EXISTS (SELECT 1 FROM databrain.migration_definitions x
    WHERE x.target_id=d.target_id AND x.draft_id=d.id))`

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	err := row.Scan(&d.ID, &d.TargetID, &d.Title, &d.Revision, &d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt,
		&d.SQL, &d.DownSQL, &d.Irreversible, &d.IrreversibleReason)
	if err != nil {
		return Draft{}, err
	}
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return d, nil
}

func scanDraftWithFrozen(row pgx.Row) (Draft, error) {
	var d Draft
	err := row.Scan(&d.ID, &d.TargetID, &d.Title, &d.Revision, &d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt,
		&d.SQL, &d.DownSQL, &d.Irreversible, &d.IrreversibleReason, &d.Frozen)
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
	    (id,target_id,title,sql,down_sql,irreversible,irreversible_reason,created_by,updated_by)
	    SELECT $1,id,$3,$4,$5,$6,$7,$8,$8 FROM databrain.database_targets WHERE id=$2
	    RETURNING `+draftColumns, uuid.New(), targetID, input.Title, input.SQL, input.DownSQL,
		input.Irreversible, draftReason(input.IrreversibleReason), actorID))
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
	rows, err := lease.Query(ctx, `SELECT `+draftSummaryColumns+`,`+frozenDraftExpression+`
	    FROM databrain.migration_drafts d
	    WHERE d.target_id=$1 ORDER BY d.updated_at DESC,d.id LIMIT $2 OFFSET $3`, targetID, limit+1, offset)
	if err != nil {
		return nil, false, draftStoreError(err)
	}
	defer rows.Close()
	items := make([]DraftSummary, 0, limit)
	for rows.Next() {
		var d DraftSummary
		if err := rows.Scan(&d.ID, &d.TargetID, &d.Title, &d.Revision, &d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt, &d.Frozen); err != nil {
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
	d, err := scanDraftWithFrozen(lease.QueryRow(ctx, `SELECT `+draftColumns+`,`+frozenDraftExpression+`
	    FROM databrain.migration_drafts d WHERE d.target_id=$1 AND d.id=$2`, targetID, draftID))
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
	    SET title=$4,sql=$5,down_sql=$6,irreversible=$7,irreversible_reason=$8,updated_by=$9,
	        updated_at=now(),revision=revision+1
	    WHERE target_id=$1 AND id=$2 AND revision::bigint=$3 RETURNING `+draftColumns,
		targetID, draftID, input.ExpectedRevision, input.Title, input.SQL, input.DownSQL,
		input.Irreversible, draftReason(input.IrreversibleReason), actorID))
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

const definitionColumns = `id,target_id,draft_id,revision,up_sql,down_sql,irreversible,
    coalesce(irreversible_reason,''),up_sha256,down_sha256,source,created_at`

func scanDefinition(row pgx.Row) (Definition, error) {
	var d Definition
	err := row.Scan(&d.ID, &d.TargetID, &d.DraftID, &d.Revision, &d.UpSQL, &d.DownSQL, &d.Irreversible,
		&d.IrreversibleReason, &d.UpSHA256, &d.DownSHA256, &d.Source, &d.CreatedAt)
	if err != nil {
		return Definition{}, err
	}
	d.CreatedAt = d.CreatedAt.UTC()
	return d, nil
}

func definitionMatchesDraft(d Draft, def Definition) bool {
	return def.UpSQL == d.SQL && def.DownSQL == d.DownSQL && def.Irreversible == d.Irreversible &&
		def.IrreversibleReason == d.IrreversibleReason
}

// SnapshotDefinition menyimpan definisi immutable pada revision draft saat submission. Pemanggil
// sudah mengotorisasi target; row lock draft membuat snapshot bersaing dengan Save/Reserve. Setelah
// definisi ada, trigger database menolak perubahan draft.
func (s *DraftStore) SnapshotDefinition(ctx context.Context, targetID, draftID uuid.UUID, expectedRevision int64) (Definition, error) {
	if targetID == uuid.Nil || draftID == uuid.Nil || expectedRevision < 1 {
		return Definition{}, draftFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return Definition{}, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Definition{}, draftFailure(problem.PlatformUnavailable)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	def, err := snapshotDefinitionTx(ctx, tx, targetID, draftID, expectedRevision)
	if err != nil {
		return Definition{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Definition{}, draftFailure(problem.PlatformUnavailable)
	}
	return def, nil
}

// snapshotDefinitionTx dipakai submission agar definition, request, dan required groups commit atomik.
func snapshotDefinitionTx(ctx context.Context, tx pgx.Tx, targetID, draftID uuid.UUID, expectedRevision int64) (Definition, error) {
	if targetID == uuid.Nil || draftID == uuid.Nil || expectedRevision < 1 {
		return Definition{}, draftFailure(problem.InvalidRequest)
	}
	d, err := scanDraftWithFrozen(tx.QueryRow(ctx, `SELECT `+draftColumns+`,`+frozenDraftExpression+`
	    FROM databrain.migration_drafts d WHERE d.target_id=$1 AND d.id=$2 FOR UPDATE`, targetID, draftID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Definition{}, draftFailure(problem.DraftNotFound)
	}
	if err != nil {
		return Definition{}, draftStoreError(err)
	}
	if d.Revision != expectedRevision {
		return Definition{}, draftFailure(problem.RevisionConflict)
	}
	if d.Frozen {
		existing, findErr := scanDefinition(tx.QueryRow(ctx, `SELECT `+definitionColumns+`
		    FROM databrain.migration_definitions WHERE target_id=$1 AND draft_id=$2 AND revision=$3`,
			targetID, draftID, d.Revision))
		if findErr == nil && definitionMatchesDraft(d, existing) {
			return existing, nil
		}
		return Definition{}, draftFailure(problem.DraftFrozen)
	}
	input := DefinitionInput{UpSQL: d.SQL, DownSQL: d.DownSQL, Irreversible: d.Irreversible, IrreversibleReason: d.IrreversibleReason}
	if err := ValidateDefinition(input); err != nil {
		return Definition{}, err
	}
	upHash := sha256.Sum256([]byte(d.SQL))
	var downHash []byte
	var reason any
	if d.Irreversible {
		reason = d.IrreversibleReason
	} else {
		sum := sha256.Sum256([]byte(d.DownSQL))
		downHash = sum[:]
	}
	return scanDefinition(tx.QueryRow(ctx, `INSERT INTO databrain.migration_definitions
	    (id,target_id,draft_id,revision,up_sql,down_sql,irreversible,irreversible_reason,up_sha256,down_sha256,source)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'submission') RETURNING `+definitionColumns,
		uuid.New(), targetID, draftID, d.Revision, d.SQL, d.DownSQL, d.Irreversible, reason, upHash[:], downHash))
}
