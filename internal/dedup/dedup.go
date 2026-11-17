// Package dedup records the messages the bot has answered, so a message
// delivered again (after a reconnection or a crash) is not answered twice.
//
// It lives in its own SQLite file, separate from the whatsmeow session, so
// its writes never wait for whatsmeow's write lock.
package dedup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite driver, registered under the name "sqlite".
	_ "modernc.org/sqlite"
)

// stateDBFile is the bot's own state database inside the data directory.
const stateDBFile = "state.db"

const schema = `
CREATE TABLE IF NOT EXISTS answered (
	message_id  TEXT PRIMARY KEY,
	chat        TEXT NOT NULL,
	sender      TEXT NOT NULL,
	answered_at TEXT NOT NULL
)`

// Store is the record of answered messages.
type Store struct {
	db *sql.DB
}

// Open opens (and creates, if needed) the state database in dataDir.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// Unlike the session database, this one keeps synchronous(FULL): a claim
	// must be on disk before the reply goes out, so a power cut right after
	// sending cannot erase it. The bot writes here only when it answers, so
	// waiting for the disk costs nothing noticeable.
	dsn := "file:" + filepath.Join(dataDir, stateDBFile) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create state schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Claim records that the bot is about to answer messageID. It returns false
// if the message was already recorded, i.e. it was answered before. The
// primary key makes this atomic even if two deliveries race.
func (s *Store) Claim(ctx context.Context, messageID, chat, sender string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO answered (message_id, chat, sender, answered_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (message_id) DO NOTHING`,
		messageID, chat, sender, time.Now().Format(time.RFC3339))
	if err != nil {
		return false, fmt.Errorf("claim %s: %w", messageID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim %s: %w", messageID, err)
	}
	return n == 1, nil
}

// Release removes the record of messageID, used when sending the reply
// failed, so a later delivery of the same message can try again.
func (s *Store) Release(ctx context.Context, messageID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM answered WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("release %s: %w", messageID, err)
	}
	return nil
}
