package session

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
	"modernc.org/sqlite"
)

// These tests reproduce, on the real session connection string, the
// collision that made a message undecryptable during the initial sync: a
// transaction that reads first and writes afterwards, while another
// connection writes. They touch the disk, so on the homelab they show how the
// hard drive behaves. Set TMPDIR to a directory on that disk when running
// them there.

// holdTime is how long the competing writer keeps the write lock. It is well
// under busy_timeout (5 s), so a transaction that waits finishes.
const holdTime = 500 * time.Millisecond

// SQLite result codes. The driver reports extended codes.
const (
	codeBusy         = 5        // SQLITE_BUSY: another connection holds the write lock
	codeBusySnapshot = 5 | 2<<8 // SQLITE_BUSY_SNAPSHOT: another connection committed after our read
)

func TestOpenStoreWithSessionDSN(t *testing.T) {
	ctx := context.Background()
	container, err := OpenStore(ctx, t.TempDir(), waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer container.Close()

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatalf("GetFirstDevice: %v", err)
	}
	if device.ID != nil {
		t.Errorf("new store has a paired device: %v", device.ID)
	}
}

// TestSessionDSNSettings checks the settings each connection gets from the
// session connection string.
func TestSessionDSNSettings(t *testing.T) {
	db, err := sql.Open("sqlite", sessionDSN(t.TempDir()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	tests := []struct {
		pragma string
		want   string
	}{
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"journal_mode", "wal"},
		{"synchronous", "1"}, // NORMAL
	}
	for _, tt := range tests {
		var got string
		if err := db.QueryRow("PRAGMA " + tt.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", tt.pragma, err)
		}
		if got != tt.want {
			t.Errorf("PRAGMA %s = %s, want %s", tt.pragma, got, tt.want)
		}
	}
}

// TestSessionTransactionsWaitForTheWriteLock checks that, with the session
// connection string, a transaction shaped like whatsmeow's decryption waits
// for another writer instead of failing.
func TestSessionTransactionsWaitForTheWriteLock(t *testing.T) {
	ctx := context.Background()

	t.Run("another connection holds the write lock", func(t *testing.T) {
		db := openTestDB(t, sessionDSN(t.TempDir()))
		committed := holdWriteLock(t, db)

		start := time.Now()
		if err := readThenWrite(ctx, db, nil); err != nil {
			t.Fatalf("transaction failed instead of waiting for the writer: %v", err)
		}
		if waited := time.Since(start); waited < holdTime/2 {
			t.Errorf("transaction finished after %v, before the writer released the lock (%v)", waited, holdTime)
		}
		if err := <-committed; err != nil {
			t.Fatalf("writer commit: %v", err)
		}
	})

	t.Run("another connection writes between the read and the write", func(t *testing.T) {
		db := openTestDB(t, sessionDSN(t.TempDir()))
		otherDone := make(chan error, 1)

		err := readThenWrite(ctx, db, func() {
			go func() {
				_, err := db.ExecContext(ctx, `INSERT INTO t (v) VALUES ('other')`)
				otherDone <- err
			}()
			// Leave the other write time to run, or to start waiting for
			// the lock this transaction already holds.
			time.Sleep(holdTime)
		})
		if err != nil {
			t.Fatalf("transaction failed instead of waiting: %v", err)
		}
		if err := <-otherDone; err != nil {
			t.Fatalf("the other write failed: %v", err)
		}
	})
}

// TestDeferredTransactionsFailAtOnce documents the failure _txlock=immediate
// prevents: with a plain BEGIN, the same transactions get SQLITE_BUSY at once,
// without waiting for busy_timeout. If this test fails after upgrading the
// driver, SQLite changed this behavior and the reason for _txlock is worth
// checking again.
func TestDeferredTransactionsFailAtOnce(t *testing.T) {
	ctx := context.Background()
	deferredDSN := func(dataDir string) string {
		dsn := sessionDSN(dataDir)
		without := strings.Replace(dsn, "&_txlock=immediate", "", 1)
		if without == dsn {
			t.Fatalf("session DSN has no _txlock=immediate to remove: %s", dsn)
		}
		return without
	}

	t.Run("another connection holds the write lock", func(t *testing.T) {
		db := openTestDB(t, deferredDSN(t.TempDir()))
		committed := holdWriteLock(t, db)

		start := time.Now()
		err := readThenWrite(ctx, db, nil)
		elapsed := time.Since(start)
		if got := sqliteCode(err); got != codeBusy {
			t.Errorf("got %v (code %d), want SQLITE_BUSY (%d)", err, got, codeBusy)
		}
		if elapsed >= holdTime/2 {
			t.Errorf("failed after %v; expected it to fail without waiting", elapsed)
		}
		if err := <-committed; err != nil {
			t.Fatalf("writer commit: %v", err)
		}
	})

	t.Run("another connection writes between the read and the write", func(t *testing.T) {
		db := openTestDB(t, deferredDSN(t.TempDir()))

		err := readThenWrite(ctx, db, func() {
			// Nothing holds the write lock, so this commits right away.
			if _, err := db.ExecContext(ctx, `INSERT INTO t (v) VALUES ('other')`); err != nil {
				t.Errorf("the other write failed: %v", err)
			}
		})
		if got := sqliteCode(err); got != codeBusySnapshot {
			t.Errorf("got %v (code %d), want SQLITE_BUSY_SNAPSHOT (%d)", err, got, codeBusySnapshot)
		}
	})
}

// openTestDB opens a connection pool with dsn, as whatsmeow does (no limit on
// open connections), and creates a table to write to.
func openTestDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

// readThenWrite has the shape of whatsmeow's decryption transaction: it reads
// first and writes afterwards. beforeWrite, if set, runs between the two.
func readThenWrite(ctx context.Context, db *sql.DB, beforeWrite func()) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		return err
	}
	if beforeWrite != nil {
		beforeWrite()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO t (v) VALUES ('reader')`); err != nil {
		return err
	}
	return tx.Commit()
}

// holdWriteLock starts a transaction on its own connection, writes, and keeps
// the write lock for holdTime. It returns once the lock is taken; the channel
// receives the result of the commit.
func holdWriteLock(t *testing.T, db *sql.DB) <-chan error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("writer begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO t (v) VALUES ('writer')`); err != nil {
		tx.Rollback()
		t.Fatalf("writer insert: %v", err)
	}
	committed := make(chan error, 1)
	go func() {
		time.Sleep(holdTime)
		committed <- tx.Commit()
	}()
	return committed
}

// sqliteCode returns the SQLite result code of err, or 0 if err is not a
// driver error.
func sqliteCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()
	}
	return 0
}
