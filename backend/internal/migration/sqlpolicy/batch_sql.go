package sqlpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/jackc/pgx/v5"
)

const (
	// DefaultBatchSize dipakai bila konfigurasi tidak menyebut ukuran batch.
	DefaultBatchSize = 1000
	MinBatchSize     = 1
	MaxBatchSize     = 10000
)

// BatchConfig adalah resep batch v1 yang didukung: satu tabel ordinary schema-qualified, primary
// key bigint tunggal, satu kolom nilai bigint NOT NULL, dan delta bigint konstan.
type BatchConfig struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	KeyColumn   string `json:"key_column"`
	ValueColumn string `json:"value_column"`
	Delta       int64  `json:"delta"`
	BatchSize   int    `json:"batch_size"`
}

// BatchPlan memuat template SQL immutable yang dihasilkan dari konfigurasi tervalidasi. Pemanggil
// tidak boleh menerima template dari client; VerifyBatchPlan menolak substitusi.
type BatchPlan struct {
	Config       BatchConfig
	UpSQL        string
	DownSQL      string
	TemplateHash string
}

func batchFailure(code problem.Code) error { return &problem.Error{Code: code} }

// Normalize memvalidasi konfigurasi batch dan mengisi default BatchSize.
func (c BatchConfig) Normalize() (BatchConfig, error) {
	if !userSchema(c.Schema) || !sqlIdentifier(c.Table) || !sqlIdentifier(c.KeyColumn) ||
		!sqlIdentifier(c.ValueColumn) || c.KeyColumn == c.ValueColumn || c.Delta == 0 {
		return BatchConfig{}, batchFailure(problem.ValidationFailed)
	}
	if c.BatchSize == 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.BatchSize < MinBatchSize || c.BatchSize > MaxBatchSize {
		return BatchConfig{}, batchFailure(problem.ValidationFailed)
	}
	return c, nil
}

// BuildBatchPlan menghasilkan template up/down yang dapat direview. Identifiers di-quote oleh pgx;
// delta dan daftar key selalu menjadi parameter, tidak pernah bagian dari template teks.
func BuildBatchPlan(config BatchConfig) (BatchPlan, error) {
	config, err := config.Normalize()
	if err != nil {
		return BatchPlan{}, err
	}
	table := pgx.Identifier{config.Schema, config.Table}.Sanitize()
	value := pgx.Identifier{config.ValueColumn}.Sanitize()
	key := pgx.Identifier{config.KeyColumn}.Sanitize()
	plan := BatchPlan{
		Config:  config,
		UpSQL:   "UPDATE " + table + " SET " + value + "=" + value + "+$2 WHERE " + key + "=ANY($1)",
		DownSQL: "UPDATE " + table + " SET " + value + "=" + value + "-$2 WHERE " + key + "=ANY($1)",
	}
	plan.TemplateHash = batchTemplateHash(plan)
	return plan, nil
}

// VerifyBatchPlan memastikan template dan hash masih berasal dari konfigurasi yang sama. Dipakai
// sebelum eksekusi untuk menolak substitusi template oleh client.
func VerifyBatchPlan(plan BatchPlan) error {
	expected, err := BuildBatchPlan(plan.Config)
	if err != nil {
		return err
	}
	if plan.UpSQL != expected.UpSQL || plan.DownSQL != expected.DownSQL || plan.TemplateHash != expected.TemplateHash {
		return batchFailure(problem.SQLUnsupported)
	}
	return nil
}

func batchTemplateHash(plan BatchPlan) string {
	hash := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(part)))
			hash.Write(size[:])
			hash.Write([]byte(part))
		}
	}
	write("databrain-batch-v1", plan.Config.Schema, plan.Config.Table, plan.Config.KeyColumn,
		plan.Config.ValueColumn, strconv.FormatInt(plan.Config.Delta, 10),
		strconv.Itoa(plan.Config.BatchSize), plan.UpSQL, plan.DownSQL)
	return hex.EncodeToString(hash.Sum(nil))
}

type batchColumn struct {
	number              int32
	bigint, notNull     bool
	generated, identity string
}

// ValidateBatchRecipe memeriksa konfigurasi struktural lalu katalog target pada koneksi yang
// diberikan. Fungsi ini hanya membaca katalog; caller tetap mengotorisasi akses target dan memakai
// koneksi khusus yang sama dengan eksekusi nanti.
func ValidateBatchRecipe(ctx context.Context, db platformdb.DBTX, config BatchConfig) (BatchPlan, error) {
	plan, err := BuildBatchPlan(config)
	if err != nil {
		return BatchPlan{}, err
	}
	if db == nil {
		return BatchPlan{}, batchFailure(problem.PlatformUnavailable)
	}
	recipe := plan.Config
	var kind, persistence string
	var heap, partition, inheritance, rls, forceRLS, trigger, rule bool
	err = db.QueryRow(ctx, `SELECT c.relkind::text,(c.relam=(SELECT oid FROM pg_am WHERE amname='heap')),c.relispartition,
	    c.relhassubclass,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,
	    EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgrelid=c.oid),
	    EXISTS (SELECT 1 FROM pg_rewrite w WHERE w.ev_class=c.oid)
	    FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
	    WHERE n.nspname=$1 AND c.relname=$2`, recipe.Schema, recipe.Table).
		Scan(&kind, &heap, &partition, &inheritance, &persistence, &rls, &forceRLS, &trigger, &rule)
	if errors.Is(err, pgx.ErrNoRows) {
		return BatchPlan{}, batchFailure(problem.SQLUnsupported)
	}
	if err != nil {
		return BatchPlan{}, batchFailure(problem.PlatformUnavailable)
	}
	if kind != "r" || !heap || partition || inheritance || persistence != "p" || rls || forceRLS || trigger || rule {
		return BatchPlan{}, batchFailure(problem.SQLUnsupported)
	}
	rows, err := db.Query(ctx, `SELECT a.attname,a.attnum::int4,(a.atttypid='int8'::regtype),
	    a.attnotnull,a.attgenerated::text,a.attidentity::text
	    FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid
	    JOIN pg_namespace n ON n.oid=c.relnamespace
	    WHERE n.nspname=$1 AND c.relname=$2 AND a.attname::text=ANY($3) AND NOT a.attisdropped`,
		recipe.Schema, recipe.Table, []string{recipe.KeyColumn, recipe.ValueColumn})
	if err != nil {
		return BatchPlan{}, batchFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	columns := make(map[string]batchColumn, 2)
	for rows.Next() {
		var name string
		var column batchColumn
		if err := rows.Scan(&name, &column.number, &column.bigint, &column.notNull, &column.generated, &column.identity); err != nil {
			return BatchPlan{}, batchFailure(problem.PlatformUnavailable)
		}
		columns[name] = column
	}
	if rows.Err() != nil {
		return BatchPlan{}, batchFailure(problem.PlatformUnavailable)
	}
	key, keyOK := columns[recipe.KeyColumn]
	value, valueOK := columns[recipe.ValueColumn]
	if !keyOK || !valueOK || !key.bigint || key.generated != "" || !value.bigint || !value.notNull ||
		value.generated != "" || value.identity != "" {
		return BatchPlan{}, batchFailure(problem.SQLUnsupported)
	}
	var valid, plain bool
	var keyCount int32
	var firstKey int32
	err = db.QueryRow(ctx, `SELECT i.indisvalid,i.indnkeyatts::int4,(i.indexprs IS NULL),i.indkey[0]::int4
	    FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid
	    JOIN pg_namespace n ON n.oid=c.relnamespace
	    WHERE n.nspname=$1 AND c.relname=$2 AND i.indisprimary`, recipe.Schema, recipe.Table).
		Scan(&valid, &keyCount, &plain, &firstKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return BatchPlan{}, batchFailure(problem.SQLUnsupported)
	}
	if err != nil {
		return BatchPlan{}, batchFailure(problem.PlatformUnavailable)
	}
	if !valid || !plain || keyCount != 1 || firstKey != key.number {
		return BatchPlan{}, batchFailure(problem.SQLUnsupported)
	}
	return plan, nil
}
