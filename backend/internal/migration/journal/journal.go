// Package journal mengelola target journal databrain_runtime dan advisory lock (3110, 2).
package journal

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// TargetJournalSchema adalah namespace terproteksi di database target untuk bukti commit dan
	// kepemilikan eksekusi lintas-alias.
	TargetJournalSchema    = "databrain_runtime"
	TargetJournalOwnerRole = "databrain_journal_owner"
	TargetJournalVersion   = 1

	ownerSecretDomain = "databrain:target-journal-owner:v1:"
)

var (
	ErrTargetConnectionRequired = errors.New("target journal requires a dedicated target connection")
	ErrJournalMissing           = errors.New("target journal schema is missing")
	ErrJournalUnsafe            = errors.New("target journal privilege separation failed")
	ErrJournalVersion           = errors.New("target journal schema version mismatch")
	ErrJournalOwned             = errors.New("target journal owned by another unresolved execution")
	ErrJournalAuth              = errors.New("target journal authorization failed")
	ErrJournalLockRequired      = errors.New("target ownership advisory lock is required")

	hexPlanHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// IsValidPlanHash memeriksa apakah hash rencana berupa 64 karakter hex lowercase.
func IsValidPlanHash(hash string) bool {
	return hexPlanHashRE.MatchString(hash)
}

// JournalOwner merepresentasikan pemilik eksekusi yang belum selesai pada database target.
type JournalOwner struct {
	RootExecutionID   uuid.UUID
	ActionExecutionID uuid.UUID
	PlanHash          string
	AcquiredAt        time.Time
	UpdatedAt         time.Time
}

// JournalCommit adalah bukti atomik satu transaksi commit pada database target.
type JournalCommit struct {
	RootExecutionID   uuid.UUID
	BatchOrdinal      int
	Direction         string
	ActionExecutionID uuid.UUID
	PlanHash          string
	RowsAffected      int64
	CursorEnd         *string
	CommittedAt       time.Time
}

// JournalTxGuard menyimpan nonce satu kali pakai per transaksi di memori Go. Nonce mentah hanya
// dikirim ke PostgreSQL setelah seluruh SQL pengguna selesai.
type JournalTxGuard struct {
	rootExecutionID   uuid.UUID
	actionExecutionID uuid.UUID
	planHash          string
	nonce             [32]byte
	consumed          bool
}

// DeriveOwnerSecret menghasilkan rahasia 32-byte deterministik dari APP_SECRET_KEY dan root execution
// ID. Hanya hash SHA-256 yang disimpan di database target sehingga SQL pengguna tidak dapat menghapus
// atau mengambil alih kepemilikan, sementara worker pengganti tetap dapat merekonsiliasi setelah crash.
func DeriveOwnerSecret(appSecret []byte, rootExecutionID uuid.UUID) ([32]byte, error) {
	var out [32]byte
	if len(appSecret) != 32 || rootExecutionID == uuid.Nil {
		return out, errors.New("owner secret requires a 32-byte application secret and non-nil execution ID")
	}
	mac := hmac.New(sha256.New, appSecret)
	mac.Write([]byte(ownerSecretDomain))
	mac.Write(rootExecutionID[:])
	copy(out[:], mac.Sum(nil))
	return out, nil
}

// HashJournalSecret mengembalikan digest SHA-256 dari rahasia 32-byte untuk dikirim ke fungsi
// penyimpanan hash di database target.
func HashJournalSecret(secret [32]byte) []byte {
	sum := sha256.Sum256(secret[:])
	return sum[:]
}

var requiredJournalTables = []string{
	"journal_meta",
	"unresolved_owner",
	"tx_nonces",
	"execution_commits",
	"batch_scope",
}

var requiredJournalFunctions = []string{
	"read_meta()",
	"read_owner()",
	"read_commits(uuid)",
	"acquire_owner(uuid,uuid,text,bytea)",
	"transfer_owner(uuid,uuid,text,bytea)",
	"clear_owner(uuid,bytea)",
	"arm_tx(uuid,uuid,text,bytea)",
	"record_commit(uuid,uuid,text,integer,text,bigint,text,bytea)",
	"record_batch_scope(uuid,uuid,text,integer,bigint[],bigint[],bigint[])",
	"read_batch_scope_meta(uuid)",
	"read_batch_scope(uuid,integer)",
}

var internalJournalFunctions = []string{
	"_protect_immutable()",
	"_require_target_lock()",
}

// VerifyTargetJournal memeriksa secara fail-closed bahwa schema databrain_runtime tersedia pada versi
// yang didukung, dimiliki role NOLOGIN terpisah, tidak dapat diubah atau dihapus oleh current_user,
// dan semua fungsi SECURITY DEFINER mengunci search_path.
func VerifyTargetJournal(ctx context.Context, conn *pgx.Conn) error {
	if conn == nil {
		return ErrTargetConnectionRequired
	}
	var (
		ownerName      string
		ownerUnsafe    bool
		inJournalOwner bool
		ownsDB         bool
		canCreate      bool
		canUsage       bool
	)
	err := conn.QueryRow(ctx, `
		SELECT r.rolname,
		       r.rolcanlogin OR r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls,
		       pg_catalog.pg_has_role(current_user, n.nspowner, 'MEMBER'),
		       pg_catalog.pg_has_role(current_user, d.datdba, 'MEMBER'),
		       pg_catalog.has_schema_privilege(current_user, n.oid, 'CREATE'),
		       pg_catalog.has_schema_privilege(current_user, n.oid, 'USAGE')
		  FROM pg_catalog.pg_namespace n
		  JOIN pg_catalog.pg_roles r ON r.oid = n.nspowner
		  JOIN pg_catalog.pg_database d ON d.datname = pg_catalog.current_database()
		 WHERE n.nspname = $1`, TargetJournalSchema).Scan(
		&ownerName, &ownerUnsafe, &inJournalOwner, &ownsDB, &canCreate, &canUsage,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrJournalMissing
	}
	if err != nil {
		return ErrJournalUnsafe
	}
	if ownerName != TargetJournalOwnerRole || ownerUnsafe || inJournalOwner || ownsDB || canCreate || !canUsage {
		return ErrJournalUnsafe
	}

	for _, table := range requiredJournalTables {
		var (
			relKind        string
			tableOwner     string
			hasDirectGrant bool
		)
		err := conn.QueryRow(ctx, `
			SELECT c.relkind::text,
			       pg_catalog.pg_get_userbyid(c.relowner),
			       pg_catalog.has_table_privilege(current_user, c.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			  FROM pg_catalog.pg_class c
			  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relname = $2`, TargetJournalSchema, table).Scan(
			&relKind, &tableOwner, &hasDirectGrant,
		)
		if err != nil || relKind != "r" || tableOwner != TargetJournalOwnerRole || hasDirectGrant {
			return ErrJournalUnsafe
		}
	}

	for _, trig := range []struct{ table, name string }{
		{"journal_meta", "protect_journal_meta"},
		{"execution_commits", "protect_execution_commits"},
		{"batch_scope", "protect_batch_scope"},
	} {
		var enabled string
		err := conn.QueryRow(ctx, `
			SELECT t.tgenabled::text
			  FROM pg_catalog.pg_trigger t
			  JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid
			  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relname = $2 AND t.tgname = $3 AND NOT t.tgisinternal`,
			TargetJournalSchema, trig.table, trig.name).Scan(&enabled)
		if err != nil || (enabled != "O" && enabled != "A") {
			return ErrJournalUnsafe
		}
	}

	for _, sig := range requiredJournalFunctions {
		var (
			fnOwner    string
			secDef     bool
			canExec    bool
			configList []string
		)
		err := conn.QueryRow(ctx, `
			SELECT pg_catalog.pg_get_userbyid(p.proowner),
			       p.prosecdef,
			       pg_catalog.has_function_privilege(current_user, p.oid, 'EXECUTE'),
			       coalesce(p.proconfig, ARRAY[]::text[])
			  FROM pg_catalog.pg_proc p
			  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = $1
			   AND p.proname || '(' || pg_catalog.oidvectortypes(p.proargtypes) || ')' = $2`,
			TargetJournalSchema, normalizeSigForLookup(sig)).Scan(&fnOwner, &secDef, &canExec, &configList)
		if err != nil || fnOwner != TargetJournalOwnerRole || !secDef || !canExec || !hasLockedSearchPath(configList) {
			return ErrJournalUnsafe
		}
	}

	for _, sig := range internalJournalFunctions {
		var (
			fnOwner string
			canExec bool
		)
		err := conn.QueryRow(ctx, `
			SELECT pg_catalog.pg_get_userbyid(p.proowner),
			       pg_catalog.has_function_privilege(current_user, p.oid, 'EXECUTE')
			  FROM pg_catalog.pg_proc p
			  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = $1
			   AND p.proname || '(' || pg_catalog.oidvectortypes(p.proargtypes) || ')' = $2`,
			TargetJournalSchema, normalizeSigForLookup(sig)).Scan(&fnOwner, &canExec)
		if err != nil || fnOwner != TargetJournalOwnerRole || canExec {
			return ErrJournalUnsafe
		}
	}

	rows, err := conn.Query(ctx, `SELECT schema_version FROM databrain_runtime.read_meta()`)
	if err != nil {
		return ErrJournalUnsafe
	}
	defer rows.Close()
	count := 0
	var version int
	for rows.Next() {
		if err := rows.Scan(&version); err != nil {
			return ErrJournalUnsafe
		}
		count++
	}
	if rows.Err() != nil {
		return ErrJournalUnsafe
	}
	if count != 1 || version != TargetJournalVersion {
		return ErrJournalVersion
	}
	return nil
}

// normalizeSigForLookup menyesuaikan tipe ringkas ke format keluaran oidvectortypes PostgreSQL
// (memisahkan argumen dengan ", ").
func normalizeSigForLookup(sig string) string {
	return strings.ReplaceAll(sig, ",", ", ")
}

func hasLockedSearchPath(proconfig []string) bool {
	for _, entry := range proconfig {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "search_path") {
			continue
		}
		parts := strings.Split(v, ",")
		if len(parts) != 2 {
			continue
		}
		if strings.TrimSpace(parts[0]) == "pg_catalog" && strings.TrimSpace(parts[1]) == TargetJournalSchema {
			return true
		}
	}
	return false
}

func classifyJournalError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		msg := strings.ToLower(pgErr.Message)
		switch {
		case strings.Contains(msg, "advisory lock"):
			return ErrJournalLockRequired
		case strings.Contains(msg, "owned by another unresolved execution"):
			return ErrJournalOwned
		case strings.Contains(msg, "authorization failed") || strings.Contains(msg, "commit nonce") || strings.Contains(msg, "owner mismatch"):
			return ErrJournalAuth
		}
	}
	return err
}

// ReadJournalOwner membaca pemilik eksekusi aktif di database target, atau nil bila kosong.
func ReadJournalOwner(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (*JournalOwner, error) {
	if q == nil {
		return nil, ErrTargetConnectionRequired
	}
	var owner JournalOwner
	err := q.QueryRow(ctx, `SELECT root_execution_id, action_execution_id, plan_hash, acquired_at, updated_at
		FROM databrain_runtime.read_owner()`).Scan(
		&owner.RootExecutionID, &owner.ActionExecutionID, &owner.PlanHash, &owner.AcquiredAt, &owner.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, classifyJournalError(err)
	}
	owner.AcquiredAt = owner.AcquiredAt.UTC()
	owner.UpdatedAt = owner.UpdatedAt.UTC()
	return &owner, nil
}

// ReadJournalCommits membaca seluruh bukti commit yang tercatat untuk satu root execution ID.
func ReadJournalCommits(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, rootExecutionID uuid.UUID) ([]JournalCommit, error) {
	if q == nil {
		return nil, ErrTargetConnectionRequired
	}
	if rootExecutionID == uuid.Nil {
		return nil, errors.New("root execution ID is required")
	}
	rows, err := q.Query(ctx, `SELECT root_execution_id, batch_ordinal, direction, action_execution_id,
		plan_hash, rows_affected, cursor_end, committed_at
		FROM databrain_runtime.read_commits($1)`, rootExecutionID)
	if err != nil {
		return nil, classifyJournalError(err)
	}
	defer rows.Close()
	commits := make([]JournalCommit, 0)
	for rows.Next() {
		var c JournalCommit
		if err := rows.Scan(&c.RootExecutionID, &c.BatchOrdinal, &c.Direction, &c.ActionExecutionID,
			&c.PlanHash, &c.RowsAffected, &c.CursorEnd, &c.CommittedAt); err != nil {
			return nil, classifyJournalError(err)
		}
		c.CommittedAt = c.CommittedAt.UTC()
		commits = append(commits, c)
	}
	if rows.Err() != nil {
		return nil, classifyJournalError(rows.Err())
	}
	return commits, nil
}

// AcquireJournalOwner mendaftarkan atau memverifikasi pemilik eksekusi tunggal pada target di bawah
// advisory lock (3110,2). Hanya hash SHA-256 dari ownerSecret yang dikirim ke database target.
func AcquireJournalOwner(ctx context.Context, conn *pgx.Conn, rootExecutionID, actionExecutionID uuid.UUID, planHash string, ownerSecret [32]byte) error {
	if conn == nil {
		return ErrTargetConnectionRequired
	}
	if rootExecutionID == uuid.Nil || actionExecutionID == uuid.Nil || !hexPlanHashRE.MatchString(planHash) {
		return errors.New("invalid journal owner arguments")
	}
	_, err := conn.Exec(ctx, `SELECT databrain_runtime.acquire_owner($1, $2, $3, $4)`,
		rootExecutionID, actionExecutionID, planHash, HashJournalSecret(ownerSecret))
	return classifyJournalError(err)
}

// TransferJournalOwner memindahkan kepemilikan aktif dari sumber yang belum selesai ke aksi pemulihan
// yang sah pada root execution yang sama.
func TransferJournalOwner(ctx context.Context, conn *pgx.Conn, rootExecutionID, newActionExecutionID uuid.UUID, newPlanHash string, ownerSecret [32]byte) error {
	if conn == nil {
		return ErrTargetConnectionRequired
	}
	if rootExecutionID == uuid.Nil || newActionExecutionID == uuid.Nil || !hexPlanHashRE.MatchString(newPlanHash) {
		return errors.New("invalid journal transfer arguments")
	}
	_, err := conn.Exec(ctx, `SELECT databrain_runtime.transfer_owner($1, $2, $3, $4)`,
		rootExecutionID, newActionExecutionID, newPlanHash, ownerSecret[:])
	return classifyJournalError(err)
}

// ClearJournalOwner menghapus kepemilikan unresolved setelah finalisasi platform terkonfirmasi.
// Bukti commit pada execution_commits tidak pernah dihapus.
func ClearJournalOwner(ctx context.Context, conn *pgx.Conn, rootExecutionID uuid.UUID, ownerSecret [32]byte) error {
	if conn == nil {
		return ErrTargetConnectionRequired
	}
	if rootExecutionID == uuid.Nil {
		return errors.New("root execution ID is required")
	}
	_, err := conn.Exec(ctx, `SELECT databrain_runtime.clear_owner($1, $2)`, rootExecutionID, ownerSecret[:])
	return classifyJournalError(err)
}

// ArmJournalTx menanam hash nonce satu kali pakai di awal transaksi target sebelum SQL pengguna
// dijalankan.
func ArmJournalTx(ctx context.Context, tx pgx.Tx, rootExecutionID, actionExecutionID uuid.UUID, planHash string) (*JournalTxGuard, error) {
	if tx == nil {
		return nil, ErrTargetConnectionRequired
	}
	if rootExecutionID == uuid.Nil || actionExecutionID == uuid.Nil || !hexPlanHashRE.MatchString(planHash) {
		return nil, errors.New("invalid journal transaction arguments")
	}
	guard := &JournalTxGuard{
		rootExecutionID:   rootExecutionID,
		actionExecutionID: actionExecutionID,
		planHash:          planHash,
	}
	if _, err := rand.Read(guard.nonce[:]); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT databrain_runtime.arm_tx($1, $2, $3, $4)`,
		rootExecutionID, actionExecutionID, planHash, HashJournalSecret(guard.nonce)); err != nil {
		clear(guard.nonce[:])
		return nil, classifyJournalError(err)
	}
	return guard, nil
}

// RecordCommit mencatat bukti commit atomik dalam transaksi target yang sama memakai nonce yang
// sebelumnya ditanam oleh ArmJournalTx.
func (g *JournalTxGuard) RecordCommit(ctx context.Context, tx pgx.Tx, batchOrdinal int, direction string, rowsAffected int64, cursorEnd *string) error {
	if g == nil || g.consumed {
		return ErrJournalAuth
	}
	if tx == nil {
		return ErrTargetConnectionRequired
	}
	if batchOrdinal < 0 || rowsAffected < 0 {
		return errors.New("invalid commit evidence counters")
	}
	switch direction {
	case "apply", "rollback", "forward", "resume", "compensate":
	default:
		return errors.New("invalid commit evidence direction")
	}
	defer func() {
		clear(g.nonce[:])
		g.consumed = true
	}()
	_, err := tx.Exec(ctx, `SELECT databrain_runtime.record_commit($1, $2, $3, $4, $5, $6, $7, $8)`,
		g.rootExecutionID, g.actionExecutionID, g.planHash, batchOrdinal, direction, rowsAffected, cursorEnd, g.nonce[:])
	return classifyJournalError(err)
}

// BatchScopeRow menyimpan satu baris cakupan batch (key + before/after value) untuk satu batch
// terbatas (<= 10.000 baris) pada database target. Tidak pernah dikirim ke platform DB atau API.
type BatchScopeRow struct {
	Key         int64
	BeforeValue int64
	AfterValue  int64
}

// RecordBatchScope mencatat cakupan satu batch ke databrain_runtime.batch_scope secara idempoten.
func RecordBatchScope(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, rootExecutionID, actionExecutionID uuid.UUID,
	planHash string, batchOrdinal int, keys, beforeVals, afterVals []int64) error {
	if q == nil {
		return ErrTargetConnectionRequired
	}
	if rootExecutionID == uuid.Nil || actionExecutionID == uuid.Nil || !hexPlanHashRE.MatchString(planHash) ||
		batchOrdinal < 1 || len(keys) == 0 || len(keys) != len(beforeVals) || len(keys) != len(afterVals) {
		return errors.New("invalid batch scope arguments")
	}
	_, err := q.Exec(ctx, `SELECT databrain_runtime.record_batch_scope($1, $2, $3, $4, $5, $6, $7)`,
		rootExecutionID, actionExecutionID, planHash, int32(batchOrdinal), keys, beforeVals, afterVals)
	return classifyJournalError(err)
}

// ReadBatchScopeMeta membaca jumlah baris dan jumlah batch cakupan yang sudah direkam.
func ReadBatchScopeMeta(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, rootExecutionID uuid.UUID) (int64, int, error) {
	if q == nil {
		return 0, 0, ErrTargetConnectionRequired
	}
	var totalRows int64
	var totalBatches int
	if err := q.QueryRow(ctx, `SELECT total_rows, total_batches FROM databrain_runtime.read_batch_scope_meta($1)`,
		rootExecutionID).Scan(&totalRows, &totalBatches); err != nil {
		return 0, 0, classifyJournalError(err)
	}
	return totalRows, totalBatches, nil
}

// ReadBatchScope membaca daftar baris (maksimal satu batch) untuk satu ordinal batch.
func ReadBatchScope(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, rootExecutionID uuid.UUID, batchOrdinal int) ([]BatchScopeRow, error) {
	if q == nil {
		return nil, ErrTargetConnectionRequired
	}
	rows, err := q.Query(ctx, `SELECT row_key, before_value, after_value FROM databrain_runtime.read_batch_scope($1, $2)`,
		rootExecutionID, int32(batchOrdinal))
	if err != nil {
		return nil, classifyJournalError(err)
	}
	defer rows.Close()
	out := make([]BatchScopeRow, 0)
	for rows.Next() {
		var r BatchScopeRow
		if err := rows.Scan(&r.Key, &r.BeforeValue, &r.AfterValue); err != nil {
			return nil, classifyJournalError(err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
