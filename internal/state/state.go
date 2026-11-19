// Package state keeps the messages the bot has to answer, from the moment
// the event handler receives them until the reply is sent or given up, and a
// short record of the ones already attended so a repeated delivery is not
// answered twice.
//
// A message goes through three states:
//
//   - pending: saved by the handler, waiting for the worker.
//   - sending: the worker built the reply and fixed its id. If the process
//     dies here, the same reply goes out again, with the same id, at start.
//   - closed: replied, given up or expired. Its content is erased in the same
//     write that closes it; the row stays, without content, as the record
//     that the message was attended, until Prune deletes it.
//
// The store does not look inside a message: content and the built reply are
// opaque bytes that the session encodes.
//
// It lives in its own SQLite file, separate from the whatsmeow session, so
// its writes never wait for whatsmeow's write lock.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite driver, registered under the name "sqlite".
	_ "modernc.org/sqlite"
)

// stateDBFile is the bot's own state database inside the data directory.
const stateDBFile = "state.db"

// ErrNotOpen is returned when a change refers to a message that is not
// pending or sending (it does not exist or it is already closed).
var ErrNotOpen = errors.New("state: no open message with that seq")

// Store is the bot's state database.
type Store struct {
	db *sql.DB
}

// Incoming is a message the handler saves.
type Incoming struct {
	Chat      string
	MessageID string
	// Sent is when the person sent the message (the server's timestamp).
	Sent time.Time
	// Received is when the bot saved it. The first attempt is due then.
	Received time.Time
	// Content is what the worker needs to answer.
	Content []byte
}

// Job is an open message, as the worker takes it.
type Job struct {
	Seq       int64
	Chat      string
	MessageID string
	Sent      time.Time
	Received  time.Time
	// Attempts counts the failed sends so far.
	Attempts int
	Content  []byte
	// ReplyID and Reply are set once the message is sending: the reply as it
	// was built at the first attempt, which every later attempt sends again.
	ReplyID string
	Reply   []byte
}

// Sending reports whether the reply was already built and its id fixed.
func (j Job) Sending() bool { return j.ReplyID != "" }

// Open opens (and creates or migrates, if needed) the state database in
// dataDir.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(dataDir))
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	// One connection for everything: the handler's saves and the worker's
	// changes take turns inside the process, and never meet in SQLite the way
	// whatsmeow's connections did in the session database.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// dsn is the connection string of the state database in dataDir.
//
// synchronous(FULL): a commit waits until the WAL is on disk. The handler
// tells whatsmeow a message was handled only after saving it, and whatsmeow
// then confirms it to the server, which will not deliver it again; so the
// save has to survive a power cut. Measured on the homelab's hard drive: about
// 45 ms per save, under 100 ms in 99% of them.
//
// secure_delete(1): erased content is overwritten with zeros in the database
// file, instead of staying in the free space of its pages. Older copies of
// the pages stay in the WAL until Checkpoint truncates it.
//
// _txlock=immediate: a transaction takes the write lock when it begins. With
// a single connection nothing competes for it today; it keeps a read-then-
// write transaction from failing at once if that ever changes.
func dsn(dataDir string) string {
	return "file:" + filepath.Join(dataDir, stateDBFile) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=secure_delete(1)&_txlock=immediate"
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrations bring the schema from version i to i+1. The version lives in
// PRAGMA user_version; a database from before this package has version 0.
var migrations = []func(ctx context.Context, tx *sql.Tx) error{
	migrateToMessages,
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate state database: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read state schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("state database has schema version %d, newer than this binary (%d)", version, len(migrations))
	}
	if version == len(migrations) {
		return nil
	}
	for v := version; v < len(migrations); v++ {
		if err := migrations[v](ctx, tx); err != nil {
			return fmt.Errorf("migrate state schema to version %d: %w", v+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return fmt.Errorf("set state schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate state database: %w", err)
	}
	return nil
}

// migrateToMessages creates the messages table. Times are Unix milliseconds.
// If the database has the answered table of the dedup package, its rows
// become closed records with outcome "answered", so a message answered before
// the upgrade is still recognized, and the table is dropped.
func migrateToMessages(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE messages (
	seq             INTEGER PRIMARY KEY,
	chat            TEXT    NOT NULL,
	message_id      TEXT    NOT NULL,
	state           TEXT    NOT NULL CHECK (state IN ('pending', 'sending', 'closed')),
	sent_at         INTEGER NOT NULL,
	received_at     INTEGER NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL,
	content         BLOB,
	reply_id        TEXT,
	reply           BLOB,
	outcome         TEXT,
	closed_at       INTEGER,
	UNIQUE (chat, message_id)
);
CREATE INDEX messages_open ON messages (chat, seq) WHERE state <> 'closed';
CREATE INDEX messages_closed ON messages (closed_at) WHERE state = 'closed';
`); err != nil {
		return err
	}

	var hasAnswered int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'answered'`,
	).Scan(&hasAnswered); err != nil {
		return err
	}
	if hasAnswered == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO messages (chat, message_id, state, sent_at, received_at, next_attempt_at, outcome, closed_at)
SELECT chat, message_id, 'closed', t, t, t, 'answered', t
FROM (SELECT chat, message_id, coalesce(CAST(strftime('%s', answered_at) AS INTEGER) * 1000, ?) AS t FROM answered)`,
		now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DROP TABLE answered`)
	return err
}

// Save stores a received message as pending. It returns false if the
// message (same chat and id) is already there, pending, sending or closed:
// a repeated delivery. A single statement does the check and the insert, so
// two deliveries of the same message at once cannot both save it.
func (s *Store) Save(ctx context.Context, in Incoming) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO messages (chat, message_id, state, sent_at, received_at, next_attempt_at, content)
VALUES (?, ?, 'pending', ?, ?, ?, ?)
ON CONFLICT (chat, message_id) DO NOTHING`,
		in.Chat, in.MessageID, in.Sent.UnixMilli(), in.Received.UnixMilli(), in.Received.UnixMilli(), in.Content)
	if err != nil {
		return false, fmt.Errorf("save %s: %w", in.MessageID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("save %s: %w", in.MessageID, err)
	}
	return n == 1, nil
}

// headOfChat keeps, among the open messages, the oldest of each chat: a later
// message waits until the earlier ones of its chat are closed.
const headOfChat = `state <> 'closed' AND NOT EXISTS (
	SELECT 1 FROM messages o WHERE o.chat = m.chat AND o.state <> 'closed' AND o.seq < m.seq)`

// Next returns the oldest open message that is the first of its chat and
// whose next attempt is due at now. It returns false if there is none.
func (s *Store) Next(ctx context.Context, now time.Time) (Job, bool, error) {
	var (
		j                  Job
		sent, received     int64
		replyID            sql.NullString
		content, replyBody []byte
	)
	err := s.db.QueryRowContext(ctx, `
SELECT seq, chat, message_id, sent_at, received_at, attempts, content, reply_id, reply
FROM messages m
WHERE `+headOfChat+` AND next_attempt_at <= ?
ORDER BY seq
LIMIT 1`, now.UnixMilli()).Scan(&j.Seq, &j.Chat, &j.MessageID, &sent, &received, &j.Attempts, &content, &replyID, &replyBody)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("next message: %w", err)
	}
	j.Sent = time.UnixMilli(sent)
	j.Received = time.UnixMilli(received)
	j.Content = content
	j.ReplyID = replyID.String
	j.Reply = replyBody
	return j, true, nil
}

// NextAttempt returns when the earliest attempt among the messages Next could
// return is due. It returns false if no message is open.
func (s *Store) NextAttempt(ctx context.Context) (time.Time, bool, error) {
	var at sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT min(next_attempt_at) FROM messages m WHERE `+headOfChat,
	).Scan(&at); err != nil {
		return time.Time{}, false, fmt.Errorf("next attempt: %w", err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(at.Int64), true, nil
}

// MarkSending stores the reply built for a pending message and the id it
// goes out with. From here on, every attempt sends this same reply.
func (s *Store) MarkSending(ctx context.Context, seq int64, replyID string, reply []byte) error {
	return s.change(ctx, "mark sending", seq,
		`UPDATE messages SET state = 'sending', reply_id = ?, reply = ? WHERE seq = ? AND state = 'pending'`,
		replyID, reply, seq)
}

// Retry records a failed attempt: the count of failed attempts so far and
// when the next one is due.
func (s *Store) Retry(ctx context.Context, seq int64, attempts int, at time.Time) error {
	return s.change(ctx, "retry", seq,
		`UPDATE messages SET attempts = ?, next_attempt_at = ? WHERE seq = ? AND state <> 'closed'`,
		attempts, at.UnixMilli(), seq)
}

// Finish closes an open message with its outcome and erases its content and
// its reply in the same write. The id of the reply, if any, stays.
func (s *Store) Finish(ctx context.Context, seq int64, outcome string, at time.Time) error {
	return s.change(ctx, "finish", seq,
		`UPDATE messages SET state = 'closed', outcome = ?, closed_at = ?, content = NULL, reply = NULL
		 WHERE seq = ? AND state <> 'closed'`,
		outcome, at.UnixMilli(), seq)
}

func (s *Store) change(ctx context.Context, what string, seq int64, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s %d: %w", what, seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %d: %w", what, seq, err)
	}
	if n == 0 {
		return fmt.Errorf("%s %d: %w", what, seq, ErrNotOpen)
	}
	return nil
}

// Prune deletes the records of messages closed before cutoff and returns how
// many. Open messages are never deleted.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE state = 'closed' AND closed_at < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune: %w", err)
	}
	return res.RowsAffected()
}

// Checkpoint copies the WAL into the database file and truncates the WAL to
// zero bytes. The WAL keeps the earlier versions of every page it holds,
// with the content of messages already closed; truncating it is what removes
// them.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if busy != 0 {
		return errors.New("checkpoint: database busy, WAL not truncated")
	}
	return nil
}
