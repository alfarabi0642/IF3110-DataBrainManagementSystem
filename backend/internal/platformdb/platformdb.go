// Package platformdb mengelola pool runtime platform dan process lock server.
package platformdb

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaVersion harus sesuai dengan migration internal terbaru yang didukung.
const SchemaVersion int64 = 1

// DBTX dipakai bersama oleh query langsung dan transaksi pgx.
type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

var (
	_ DBTX = (*pgxpool.Pool)(nil)
	_ DBTX = (pgx.Tx)(nil)

	ErrUnavailable = errors.New("platform database unavailable")
	ErrSchema      = errors.New("platform schema is missing, dirty, or unsupported; run the supported migrations manually")
	ErrProcessLock = errors.New("another server holds the platform process lock")
	ErrRuntimeRole = errors.New("platform connection requires a restricted runtime role")
	ErrLockLost    = errors.New("platform process lock lost")
)

type DB struct {
	Pool   *pgxpool.Pool
	lock   *pgx.Conn
	lost   chan struct{}
	stop   context.CancelFunc
	done   chan struct{}
	lockMu sync.Mutex // Koneksi pgx tidak boleh dipakai bersamaan.

	closeOnce sync.Once
	closing   chan struct{}
	closed    chan struct{}
}

// Open memeriksa schema serta hak runtime sebelum mengambil lock koneksi khusus. Fungsi ini tidak
// menjalankan migration atau mengubah state attempt.
func Open(ctx context.Context, databaseURL string) (_ *DB, err error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrUnavailable
	}
	cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns = 10, 0, 0
	cfg.PingTimeout = 2 * time.Second
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "databrain-platform"
	cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog,databrain"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lockConfig := cfg.ConnConfig.Copy()
	lockConfig.RuntimeParams["application_name"] = "databrain-process-lock"
	lock, err := pgx.ConnectConfig(startup, lockConfig)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = lock.Close(cleanup)
		}
	}()
	if err := checkSchema(startup, lock); err != nil {
		return nil, err
	}
	var safeRole bool
	if err := lock.QueryRow(startup, `SELECT NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
	    AND NOT has_schema_privilege(current_user,'databrain','CREATE')
	    AND NOT has_table_privilege(current_user,'databrain.migration_attempts','DELETE,TRUNCATE')
	    FROM pg_roles WHERE rolname=current_user`).Scan(&safeRole); err != nil {
		return nil, ErrUnavailable
	}
	if !safeRole {
		return nil, ErrRuntimeRole
	}
	var acquired bool
	if err := lock.QueryRow(startup, "SELECT pg_try_advisory_lock(3110,1)").Scan(&acquired); err != nil {
		return nil, ErrUnavailable
	}
	if !acquired {
		return nil, ErrProcessLock
	}
	pool, err := pgxpool.NewWithConfig(startup, cfg)
	if err != nil {
		return nil, ErrUnavailable
	}
	conn, err := Acquire(startup, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	err = conn.Ping(startup)
	conn.Release()
	if err != nil {
		pool.Close()
		return nil, ErrUnavailable
	}
	monitor, stop := context.WithCancel(context.Background())
	db := &DB{Pool: pool, lock: lock, lost: make(chan struct{}), stop: stop, done: make(chan struct{}), closing: make(chan struct{}), closed: make(chan struct{})}
	go db.monitor(monitor)
	return db, nil
}

// Acquire membatasi penantian koneksi pool; pemanggil wajib Release. Batas query/transaksi tetap
// berasal dari context operasi.
func Acquire(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	acquire, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := pool.Acquire(acquire)
	if err != nil {
		return nil, ErrUnavailable
	}
	return conn, nil
}

func checkSchema(ctx context.Context, query DBTX) error {
	var version int64
	var dirty bool
	err := query.QueryRow(ctx, `SELECT version,dirty FROM public.schema_migrations
	    WHERE (SELECT count(*) FROM public.schema_migrations)=1`).Scan(&version, &dirty)
	if err != nil || version != SchemaVersion || dirty {
		return ErrSchema
	}
	return nil
}

func (db *DB) Lost() <-chan struct{} { return db.lost }

// WithProcessLock menjalankan startup/maintenance pada koneksi pemegang process lock. Koneksi hilang
// membatalkan transaksi sekaligus melepas lock agar server baru tidak tumpang tindih dengan transaksi
// lama. Callback tidak boleh melepas lock atau menyimpan koneksi ini.
func (db *DB) WithProcessLock(ctx context.Context, work func(*pgx.Conn) error) error {
	db.lockMu.Lock()
	defer db.lockMu.Unlock()
	if !db.available() {
		return ErrLockLost
	}
	var held bool
	if err := db.lock.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory'
        AND pid=pg_backend_pid() AND classid=3110 AND objid=1 AND objsubid=2 AND granted)`).Scan(&held); err != nil || !held {
		return ErrLockLost
	}
	return work(db.lock)
}

func (db *DB) monitor(ctx context.Context) {
	defer close(db.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			db.lockMu.Lock()
			check, cancel := context.WithTimeout(ctx, 2*time.Second)
			var held bool
			err := db.lock.QueryRow(check, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory'
			    AND pid=pg_backend_pid() AND classid=3110 AND objid=1 AND objsubid=2 AND granted)`).Scan(&held)
			db.lockMu.Unlock()
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil || !held {
				close(db.lost)
				return
			}
		}
	}
}

// Ready memeriksa konektivitas, schema, dan kepemilikan process lock saat ini. Lock yang hilang tidak
// diambil ulang.
func (db *DB) Ready(ctx context.Context) bool {
	if !db.available() {
		return false
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := Acquire(check, db.Pool)
	if err != nil {
		return false
	}
	defer conn.Release()
	return checkSchema(check, conn) == nil && db.available()
}

func (db *DB) available() bool {
	select {
	case <-db.lost:
		return false
	case <-db.closing:
		return false
	default:
		return true
	}
}

// Close menghentikan monitor dan melepas lock serta pool idle. Pemanggil wajib menutup rows/transaksi
// dan mengembalikan koneksi. Context membatasi shutdown jika koneksi masih dipinjam.
func (db *DB) Close(ctx context.Context) error {
	db.closeOnce.Do(func() {
		close(db.closing)
		db.stop()
		go func() {
			defer close(db.closed)
			<-db.done
			db.Pool.Close()
			db.lockMu.Lock()
			defer db.lockMu.Unlock()
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = db.lock.Close(cleanup)
		}()
	})
	select {
	case <-db.closed:
		return nil
	case <-ctx.Done():
		return ErrUnavailable
	}
}
