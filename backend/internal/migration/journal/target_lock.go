package journal

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Ownership target memakai kunci advisory session dengan namespace aplikasi tetap di dalam database
// fisik, bukan UUID registrasi target. Dua registrasi alias ke database yang sama karena itu berbagi
// kunci yang sama.
var (
	ErrTargetLockHeld = errors.New("target ownership lock is already held on this connection")
	ErrTargetLockLost = errors.New("target ownership lock is not held")
)

// TargetLock adalah kunci ownership pada satu koneksi target. Kunci harus diperoleh pada koneksi yang
// sama dengan SQL dan bertahan melewati transaksi antar batch. Menutup koneksi juga melepas kunci;
// nilai ini tidak aman dipakai bersamaan dari beberapa goroutine.
type TargetLock struct {
	conn *pgx.Conn
	held bool
}

// TryTargetLock mencoba mengambil ownership tanpa menunggu. false berarti worker lain memegang
// database fisik yang sama; pemanggil harus menjadwalkan ulang, bukan menjalankan SQL.
func TryTargetLock(ctx context.Context, conn *pgx.Conn) (*TargetLock, bool, error) {
	if conn == nil {
		return nil, false, errors.New("target ownership lock requires a dedicated target connection")
	}
	var already bool
	err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
	    WHERE locktype='advisory' AND pid=pg_backend_pid() AND classid=3110 AND objid=2
	    AND objsubid=2 AND granted)`).Scan(&already)
	if err != nil {
		return nil, false, err
	}
	if already {
		return nil, false, ErrTargetLockHeld
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(3110,2)`).Scan(&acquired); err != nil {
		return nil, false, err
	}
	if !acquired {
		return nil, false, nil
	}
	return &TargetLock{conn: conn, held: true}, true, nil
}

// Release melepas kunci secara eksplisit. Release setelah kunci dilepas adalah no-op. Jika koneksi
// sudah tertutup, pemanggil harus menganggap kunci hilang; menutup koneksi tetap melepasnya.
func (l *TargetLock) Release(ctx context.Context) error {
	if l == nil || !l.held {
		return nil
	}
	var released bool
	if err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock(3110,2)`).Scan(&released); err != nil {
		return err
	}
	l.held = false
	if !released {
		return ErrTargetLockLost
	}
	return nil
}
