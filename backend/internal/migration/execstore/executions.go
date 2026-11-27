package execstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ExecutionFailure hanya berisi pesan publik tetap dan diagnostic terbatas. Error driver dan body SQL
// tidak boleh disimpan di sini.
type ExecutionFailure struct {
	Code, Message, SQLState string
	StatementIndex          int
}

const (
	DirectionApply      = "apply"
	DirectionRollback   = "rollback"
	DirectionForward    = "forward"
	DirectionResume     = "resume"
	DirectionCompensate = "compensate"

	ModeNormal = "normal"
	ModeBatch  = "batch"

	// maxSnapshotBytes sama dengan batas sql_snapshot pada schema platform.
	maxSnapshotBytes = 131072
	// target_config_revision adalah integer PostgreSQL.
	maxTargetConfigRevision = int64(1)<<31 - 1
)

var validDirections = map[string]struct{}{
	DirectionApply: {}, DirectionRollback: {}, DirectionForward: {},
	DirectionResume: {}, DirectionCompensate: {},
}

// ApprovedOperation adalah plan immutable yang sudah disetujui. Lapisan enqueue tidak menafsirkan
// approval, head, atau eligibility: caller wajib memvalidasi dan mengonsumsi approval pada transaksi
// yang sama sebelum memanggil Enqueue.
type ApprovedOperation struct {
	RequestID, DefinitionID, TargetID, ActorID uuid.UUID
	Revision, TargetConfigRevision             int64
	Direction                                  string
	Mode                                       string
	PlanHash                                   string
	SQLSnapshot, DownSnapshot                  string
	ExpectedHeadID, SourceExecutionID          *uuid.UUID
	Batch                                      *sqlpolicy.BatchConfig
}

// PlanHash menghitung hash kanonik plan versi 1: field length-prefixed berurutan dan byte SQL asli.
// Map tidak pernah di-hash; admission dan worker memakai fungsi yang sama untuk mengikat intent.
func PlanHash(op ApprovedOperation) string {
	hash := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(part)))
			hash.Write(size[:])
			hash.Write([]byte(part))
		}
	}
	write("databrain-plan-v1")
	write(op.TargetID.String(), op.ActorID.String(), op.RequestID.String(), op.DefinitionID.String())
	write(strconv.FormatInt(op.Revision, 10), strconv.FormatInt(op.TargetConfigRevision, 10))
	write(op.Direction, op.Mode)
	write(optionalUUIDString(op.ExpectedHeadID), optionalUUIDString(op.SourceExecutionID))
	write(op.SQLSnapshot, op.DownSnapshot)
	if op.Batch != nil {
		write("databrain-batch-config-v1", op.Batch.Schema, op.Batch.Table, op.Batch.KeyColumn,
			op.Batch.ValueColumn, strconv.FormatInt(op.Batch.Delta, 10), strconv.Itoa(op.Batch.BatchSize))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func optionalUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func (op ApprovedOperation) validate(requestKey uuid.UUID) error {
	if requestKey == uuid.Nil || op.TargetID == uuid.Nil || op.ActorID == uuid.Nil ||
		op.RequestID == uuid.Nil || op.DefinitionID == uuid.Nil ||
		op.Revision < 1 || op.TargetConfigRevision < 1 || op.TargetConfigRevision > maxTargetConfigRevision {
		return enqueueFailure(problem.InvalidRequest)
	}
	if (op.ExpectedHeadID != nil && *op.ExpectedHeadID == uuid.Nil) ||
		(op.SourceExecutionID != nil && *op.SourceExecutionID == uuid.Nil) {
		return enqueueFailure(problem.InvalidRequest)
	}
	if _, ok := validDirections[op.Direction]; !ok {
		return enqueueFailure(problem.InvalidRequest)
	}
	switch op.Mode {
	case ModeNormal:
		if op.Batch != nil {
			return enqueueFailure(problem.InvalidRequest)
		}
	case ModeBatch:
		if op.Batch == nil {
			return enqueueFailure(problem.InvalidRequest)
		}
		if _, err := sqlpolicy.BuildBatchPlan(*op.Batch); err != nil {
			return enqueueFailure(problem.InvalidRequest)
		}
	default:
		return enqueueFailure(problem.InvalidRequest)
	}
	if (op.Direction == DirectionResume || op.Direction == DirectionCompensate) && op.SourceExecutionID == nil {
		return enqueueFailure(problem.InvalidRequest)
	}
	if !validSnapshot(op.SQLSnapshot, 1) || !validSnapshot(op.DownSnapshot, 0) {
		return enqueueFailure(problem.InvalidRequest)
	}
	if op.PlanHash != PlanHash(op) {
		return enqueueFailure(problem.InvalidRequest)
	}
	return nil
}

func validSnapshot(sql string, minimum int) bool {
	size := len(sql)
	return size >= minimum && size <= maxSnapshotBytes && utf8.ValidString(sql) && !strings.ContainsRune(sql, 0)
}

func MarshalBatchConfig(cfg *sqlpolicy.BatchConfig) ([]byte, error) {
	if cfg == nil {
		return nil, nil
	}
	norm, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	return json.Marshal(norm)
}

func UnmarshalBatchConfig(raw []byte) (*sqlpolicy.BatchConfig, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var cfg sqlpolicy.BatchConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	norm, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	return &norm, nil
}

func insertExecution(ctx context.Context, tx pgx.Tx, id uuid.UUID, op ApprovedOperation) error {
	var batchJSON any
	if op.Batch != nil {
		raw, err := MarshalBatchConfig(op.Batch)
		if err != nil {
			return enqueueFailure(problem.InvalidRequest)
		}
		batchJSON = raw
	}
	_, err := tx.Exec(ctx, `INSERT INTO databrain.migration_executions
	    (id,target_id,actor_id,request_id,definition_id,revision,target_config_revision,
	     direction,mode,plan_hash,sql_snapshot,down_snapshot,expected_head_id,source_execution_id,batch_config)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		id, op.TargetID, op.ActorID, op.RequestID, op.DefinitionID, op.Revision,
		op.TargetConfigRevision, op.Direction, op.Mode, op.PlanHash, op.SQLSnapshot,
		op.DownSnapshot, op.ExpectedHeadID, op.SourceExecutionID, batchJSON)
	return err
}

// ExecutionView adalah proyeksi aman untuk detail eksekusi. SQL, request/definition, credential,
// sebelum-image, dan bukti privat tidak termasuk.
type ExecutionView struct {
	ID, TargetID                             uuid.UUID
	Direction, Mode, Phase                   string
	Outcome                                  *string
	CommittedRows, CommittedBatches          int64
	TotalRows                                *int64
	HasPartialCommits, ReconciliationPending bool
	Failure                                  *ExecutionFailure
	CreatedAt                                time.Time
	StartedAt, FinishedAt                    *time.Time
}

func LoadExecution(ctx context.Context, db platformdb.DBTX, targetID, executionID uuid.UUID) (*ExecutionView, error) {
	var view ExecutionView
	var outcome, failureCode, failureMessage, failureSQLState string
	var totalRows pgtype.Int8
	err := db.QueryRow(ctx, `SELECT id,target_id,direction,mode,phase,COALESCE(outcome,''),
	    committed_rows,committed_batches,total_rows,has_partial_commits,reconciliation_pending,
	    COALESCE(failure_code,''),COALESCE(failure_message,''),COALESCE(failure_sqlstate,''),
	    created_at,started_at,finished_at
	    FROM databrain.migration_executions WHERE target_id=$1 AND id=$2`, targetID, executionID).
		Scan(&view.ID, &view.TargetID, &view.Direction, &view.Mode, &view.Phase, &outcome,
			&view.CommittedRows, &view.CommittedBatches, &totalRows,
			&view.HasPartialCommits, &view.ReconciliationPending,
			&failureCode, &failureMessage, &failureSQLState,
			&view.CreatedAt, &view.StartedAt, &view.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, enqueueFailure(problem.PlatformUnavailable)
	}
	if outcome != "" {
		view.Outcome = &outcome
	}
	if totalRows.Valid {
		total := totalRows.Int64
		view.TotalRows = &total
	}
	if failureCode != "" {
		view.Failure = &ExecutionFailure{Code: failureCode, Message: failureMessage, SQLState: failureSQLState}
	}
	view.CreatedAt = view.CreatedAt.UTC()
	for _, value := range []*time.Time{view.StartedAt, view.FinishedAt} {
		if value != nil {
			*value = value.UTC()
		}
	}
	return &view, nil
}
