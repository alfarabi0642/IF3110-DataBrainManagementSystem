package explorer

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxDefinitionBytes = 64 << 10
	maxMetadataBytes   = 4 << 20
	userSchemaSQL      = `n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`
)

type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // r: tabel biasa; p: tabel berpartisi.
}

type Column struct {
	Ordinal   int     `json:"ordinal"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Nullable  bool    `json:"nullable"`
	Default   *string `json:"default"`   // nil berarti tanpa ekspresi default.
	Identity  string  `json:"identity"`  // Kode PostgreSQL: a (always), d (by default), atau kosong.
	Generated string  `json:"generated"` // Kode PostgreSQL: s (stored), atau kosong.
}

type Constraint struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"` // Kode contype PostgreSQL.
	Columns    []string `json:"columns"`
	Definition string   `json:"definition"`
}

type Index struct {
	Name       string `json:"name"`
	Method     string `json:"method"`
	Unique     bool   `json:"unique"`
	Primary    bool   `json:"primary"`
	Valid      bool   `json:"valid"`
	Definition string `json:"definition"`
}

// Catalog berisi schema, tabel, atau metadata satu tabel sesuai pilihan. Credential koneksi dan data
// baris tidak disertakan.
type Catalog struct {
	Schemas     []string
	Tables      []Table
	Table       *Table
	Columns     []Column
	Constraints []Constraint
	Indexes     []Index
}

type Explorer struct {
	provider *targets.Provider
}

func New(provider *targets.Provider) *Explorer {
	return &Explorer{provider: provider}
}

// Catalog membaca satu pilihan dalam transaksi khusus read-only. Schema kosong menampilkan schema
// user; tabel kosong menampilkan daftar tabel. Pemanggil wajib mengotorisasi akses.
func (e *Explorer) Catalog(ctx context.Context, targetID uuid.UUID, schema, table string) (Catalog, error) {
	if targetID == uuid.Nil || (schema != "" && !validName(schema)) ||
		(table != "" && (!validName(table) || schema == "")) {
		return Catalog{}, failure(problem.InvalidRequest)
	}
	if schema != "" && !userSchema(schema) {
		return Catalog{}, failure(problem.RelationNotFound)
	}
	var result Catalog
	err := e.read(ctx, targetID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = readCatalog(ctx, tx, schema, table)
		return err
	})
	if err != nil {
		return Catalog{}, err
	}
	return result, nil
}

// Pembacaan katalog dan baris memakai transaksi khusus read-only dengan batas waktu.
func (e *Explorer) read(ctx context.Context, targetID uuid.UUID, query func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := e.provider.OpenExplorer(ctx, targetID)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return catalogError(ctx, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	_, err = tx.Exec(ctx, `SET LOCAL statement_timeout='3s'; SET LOCAL lock_timeout='1s'; SET LOCAL search_path=pg_catalog;
	    SET LOCAL TimeZone='UTC'; SET LOCAL DateStyle='ISO, YMD'; SET LOCAL bytea_output='hex'`)
	if err != nil {
		return catalogError(ctx, err)
	}
	if err := query(ctx, tx); err != nil {
		return catalogError(ctx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return catalogError(ctx, err)
	}
	return nil
}

func validName(name string) bool {
	return len(name) >= 1 && len(name) <= 63 && utf8.ValidString(name) && !strings.ContainsRune(name, 0)
}

func userSchema(name string) bool {
	return name != "information_schema" && !strings.HasPrefix(name, "pg_")
}

func readCatalog(ctx context.Context, tx pgx.Tx, schema, table string) (Catalog, error) {
	var result Catalog
	if schema == "" {
		rows, err := tx.Query(ctx, `WITH data AS MATERIALIZED (
		    SELECT n.nspname::text COLLATE "C" AS name FROM pg_catalog.pg_namespace n WHERE `+userSchemaSQL+`
		), budget AS (SELECT coalesce(sum(octet_length(name)),0)>$1 AS oversized FROM data)
		SELECT NULL::text AS name FROM budget WHERE oversized
		UNION ALL SELECT name FROM data,budget WHERE NOT oversized ORDER BY name`, maxMetadataBytes)
		if err != nil {
			return Catalog{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var name *string
			if err := rows.Scan(&name); err != nil {
				return Catalog{}, err
			}
			if name == nil {
				return Catalog{}, failure(problem.ResultTooLarge)
			}
			result.Schemas = append(result.Schemas, *name)
		}
		return result, rows.Err()
	}
	var namespace uint32
	if err := tx.QueryRow(ctx, `SELECT n.oid FROM pg_catalog.pg_namespace n WHERE n.nspname=$1 AND `+userSchemaSQL, schema).Scan(&namespace); err != nil {
		return Catalog{}, err
	}
	if table == "" {
		rows, err := tx.Query(ctx, `WITH data AS MATERIALIZED (
		    SELECT c.relname::text COLLATE "C" AS name,c.relkind::text AS kind FROM pg_catalog.pg_class c
		    WHERE c.relnamespace=$1 AND c.relkind IN ('r','p')
		), budget AS (SELECT coalesce(sum(octet_length(name)+octet_length(kind)),0)>$2 AS oversized FROM data)
		SELECT NULL::text AS name,NULL::text AS kind FROM budget WHERE oversized
		UNION ALL SELECT name,kind FROM data,budget WHERE NOT oversized ORDER BY name`, namespace, maxMetadataBytes)
		if err != nil {
			return Catalog{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var name, kind *string
			if err := rows.Scan(&name, &kind); err != nil {
				return Catalog{}, err
			}
			if name == nil {
				return Catalog{}, failure(problem.ResultTooLarge)
			}
			result.Tables = append(result.Tables, Table{Schema: schema, Name: *name, Kind: *kind})
		}
		return result, rows.Err()
	}
	var oid uint32
	var kind string
	if err := resolveTable(ctx, tx, schema, table, &oid, &kind); err != nil {
		return Catalog{}, err
	}
	if err := lockTable(ctx, tx, schema, table, oid); err != nil {
		return Catalog{}, err
	}
	result.Table = &Table{Schema: schema, Name: table, Kind: kind}
	remaining := maxMetadataBytes
	if err := readColumns(ctx, tx, oid, &remaining, &result); err != nil {
		return Catalog{}, err
	}
	if err := readConstraints(ctx, tx, oid, &remaining, &result); err != nil {
		return Catalog{}, err
	}
	if err := readIndexes(ctx, tx, oid, &remaining, &result); err != nil {
		return Catalog{}, err
	}
	return result, nil
}

func resolveTable(ctx context.Context, tx pgx.Tx, schema, table string, oid *uint32, kind *string) error {
	return tx.QueryRow(ctx, `SELECT c.oid,c.relkind::text FROM pg_catalog.pg_class c
	    JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
	    WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind IN ('r','p') AND `+userSchemaSQL, schema, table).Scan(oid, kind)
}

// Cari objek sebelum lock, lalu periksa ulang OID agar drop/recreate atau rename bersamaan tidak
// memilih objek pengganti. ONLY menghindari lock partisi.
func lockTable(ctx context.Context, tx pgx.Tx, schema, table string, oid uint32) error {
	if _, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{schema, table}.Sanitize()+" IN ACCESS SHARE MODE"); err != nil {
		return err
	}
	var current uint32
	var kind string
	if err := resolveTable(ctx, tx, schema, table, &current, &kind); err != nil {
		return err
	}
	if current != oid {
		return failure(problem.RelationChanged)
	}
	return nil
}

// Query metadata mengukur definisi dan total ukuran di target. Hasil terlalu besar hanya mengirim
// penanda NULL, tanpa teks aslinya.
func readColumns(ctx context.Context, tx pgx.Tx, oid uint32, remaining *int, result *Catalog) error {
	rows, err := tx.Query(ctx, `WITH data AS MATERIALIZED (
	    SELECT a.attnum::int AS ordinal,a.attname::text AS name,pg_catalog.format_type(a.atttypid,a.atttypmod) AS postgres_type,
	        NOT a.attnotnull AS nullable,pg_catalog.pg_get_expr(d.adbin,d.adrelid,false) AS default_sql,
	        a.attidentity::text AS identity,a.attgenerated::text AS generated
	    FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
	    WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped
	), sized AS MATERIALIZED (
	    SELECT *,octet_length(name)+octet_length(postgres_type)+coalesce(octet_length(default_sql),0)+8 AS bytes FROM data
	), budget AS (
	    SELECT coalesce(max(octet_length(default_sql)),0)>$2 OR coalesce(sum(bytes),0)>$3 AS oversized FROM sized
	)
	SELECT NULL::int AS ordinal,NULL::text,NULL::text,NULL::boolean,NULL::text,NULL::text,NULL::text,0 AS bytes FROM budget WHERE oversized
	UNION ALL SELECT ordinal,name,postgres_type,nullable,default_sql,identity,generated,bytes FROM sized,budget WHERE NOT oversized
	ORDER BY ordinal`, oid, maxDefinitionBytes, *remaining)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var column Column
		var bytes int
		// Periksa penanda NULL sebelum melakukan scan ke tipe konkret.
		if rows.RawValues()[0] == nil {
			return failure(problem.ResultTooLarge)
		}
		if err := rows.Scan(&column.Ordinal, &column.Name, &column.Type, &column.Nullable, &column.Default, &column.Identity, &column.Generated, &bytes); err != nil {
			return err
		}
		*remaining -= bytes
		result.Columns = append(result.Columns, column)
	}
	return rows.Err()
}

func readConstraints(ctx context.Context, tx pgx.Tx, oid uint32, remaining *int, result *Catalog) error {
	rows, err := tx.Query(ctx, `WITH data AS MATERIALIZED (
	    SELECT c.oid,c.conname::text COLLATE "C" AS name,c.contype::text AS kind,
	        ARRAY(SELECT a.attname::text FROM unnest(c.conkey) WITH ORDINALITY k(num,pos)
	            JOIN pg_catalog.pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.num ORDER BY k.pos) AS column_names,
	        pg_catalog.pg_get_constraintdef(c.oid,false) AS definition
	    FROM pg_catalog.pg_constraint c WHERE c.conrelid=$1
	), sized AS MATERIALIZED (
	    SELECT *,octet_length(name)+octet_length(kind)+octet_length(definition)+
	        coalesce((SELECT sum(octet_length(v)) FROM unnest(column_names) v),0) AS bytes FROM data
	), budget AS (
	    SELECT coalesce(max(octet_length(definition)),0)>$2 OR coalesce(sum(bytes),0)>$3 AS oversized FROM sized
	)
	SELECT NULL::text AS name,NULL::text,NULL::text[],NULL::text,0::bigint AS bytes,0::oid AS oid FROM budget WHERE oversized
	UNION ALL SELECT name,kind,column_names,definition,bytes,oid FROM sized,budget WHERE NOT oversized
	ORDER BY name,oid`, oid, maxDefinitionBytes, *remaining)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if rows.RawValues()[0] == nil {
			return failure(problem.ResultTooLarge)
		}
		var constraint Constraint
		var bytes int
		var ignored uint32
		if err := rows.Scan(&constraint.Name, &constraint.Kind, &constraint.Columns, &constraint.Definition, &bytes, &ignored); err != nil {
			return err
		}
		*remaining -= bytes
		result.Constraints = append(result.Constraints, constraint)
	}
	return rows.Err()
}

func readIndexes(ctx context.Context, tx pgx.Tx, oid uint32, remaining *int, result *Catalog) error {
	rows, err := tx.Query(ctx, `WITH data AS MATERIALIZED (
	    SELECT i.indexrelid AS oid,c.relname::text COLLATE "C" AS name,a.amname::text AS method,
	        i.indisunique AS is_unique,i.indisprimary AS is_primary,i.indisvalid AS is_valid,
	        pg_catalog.pg_get_indexdef(i.indexrelid,0,false) AS definition
	    FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indexrelid
	    JOIN pg_catalog.pg_am a ON a.oid=c.relam WHERE i.indrelid=$1
	), sized AS MATERIALIZED (
	    SELECT *,octet_length(name)+octet_length(method)+octet_length(definition)+3 AS bytes FROM data
	), budget AS (
	    SELECT coalesce(max(octet_length(definition)),0)>$2 OR coalesce(sum(bytes),0)>$3 AS oversized FROM sized
	)
	SELECT NULL::text AS name,NULL::text,NULL::boolean,NULL::boolean,NULL::boolean,NULL::text,0 AS bytes,0::oid AS oid FROM budget WHERE oversized
	UNION ALL SELECT name,method,is_unique,is_primary,is_valid,definition,bytes,oid FROM sized,budget WHERE NOT oversized
	ORDER BY name,oid`, oid, maxDefinitionBytes, *remaining)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if rows.RawValues()[0] == nil {
			return failure(problem.ResultTooLarge)
		}
		var index Index
		var bytes int
		var ignored uint32
		if err := rows.Scan(&index.Name, &index.Method, &index.Unique, &index.Primary, &index.Valid, &index.Definition, &bytes, &ignored); err != nil {
			return err
		}
		*remaining -= bytes
		result.Indexes = append(result.Indexes, index)
	}
	return rows.Err()
}

func failure(code problem.Code) error { return &problem.Error{Code: code} }

func catalogError(ctx context.Context, err error) error {
	var safe *problem.Error
	if errors.As(err, &safe) {
		return failure(safe.Code)
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return failure(problem.RequestTimeout)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return failure(problem.RelationNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42P01", "3F000", "42704":
			return failure(problem.RelationNotFound)
		case "55P03", "0A000":
			return failure(problem.RelationChanged)
		case "57014":
			return failure(problem.RequestTimeout)
		}
	}
	return failure(problem.TargetUnavailable)
}
