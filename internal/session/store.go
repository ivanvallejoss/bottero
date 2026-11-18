// Package session owns the WhatsApp connection: the whatsmeow session store,
// pairing and the connected client. It does not decide what to answer.
package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	// Pure-Go SQLite driver, registered under the name "sqlite".
	_ "modernc.org/sqlite"
)

// sessionDBFile is the whatsmeow session database inside the data directory.
const sessionDBFile = "session.db"

// OpenStore opens (and creates or migrates, if needed) the whatsmeow session
// database in dataDir.
func OpenStore(ctx context.Context, dataDir string, log waLog.Logger) (*sqlstore.Container, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	container, err := sqlstore.New(ctx, "sqlite", sessionDSN(dataDir), log)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	return container, nil
}

// sessionDSN is the connection string of the session database in dataDir.
//
// modernc.org/sqlite applies the _pragma parameters to every new connection.
// whatsmeow refuses to start if foreign keys are off.
//
// _txlock=immediate makes the driver start every transaction with BEGIN
// IMMEDIATE, which takes the write lock before the first read. whatsmeow
// decrypts each message in a transaction that first reads the Signal session
// and then writes it back. Started with a plain BEGIN, that transaction is
// already reading when it asks for the write lock, and SQLite then returns
// SQLITE_BUSY at once instead of waiting for busy_timeout: when another
// connection holds the write lock, and also when another connection committed
// after the read. During the initial sync, whatsmeow stores contact names one
// by one on another connection, and a message failed to decrypt that way.
// With BEGIN IMMEDIATE the transaction waits for the lock under busy_timeout,
// like any single write. Transactions opened as read-only keep a plain BEGIN.
//
// synchronous(NORMAL): in WAL mode, a commit no longer waits for an fsync of
// the hard drive; the WAL is synced at checkpoints. With _txlock=immediate
// each decryption waits for its turn to write, and NORMAL keeps that turn
// short. The database is not corrupted by a power cut, and commits survive a
// crash or restart of the process; a power cut can roll back the last commits
// that the system had not yet written to disk. The state database keeps
// synchronous(FULL) for the reason given in the dedup package.
func sessionDSN(dataDir string) string {
	return "file:" + filepath.Join(dataDir, sessionDBFile) +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
}
