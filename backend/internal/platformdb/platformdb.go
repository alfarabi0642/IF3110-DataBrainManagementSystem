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
	cfg, err := runtimePoolConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lock, err := openProcessLock(startup, cfg.ConnConfig)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = lock.Close(cleanup)
		}
	}()
	pool, err := openPool(startup, cfg)
	if err != nil {
		return nil, err
	}
	monitor, stop := context.WithCancel(context.Background())
	db := &DB{Pool: pool, lock: lock, lost: make(chan struct{}), stop: stop, done: make(chan struct{}), closing: make(chan struct{}), closed: make(chan struct{})}
	go db.monitor(monitor)
	return db, nil
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
