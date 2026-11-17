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

	// modernc.org/sqlite applies _pragma parameters to every new connection.
	// whatsmeow refuses to start if foreign keys are off.
	dsn := "file:" + filepath.Join(dataDir, sessionDBFile) +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

	container, err := sqlstore.New(ctx, "sqlite", dsn, log)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	return container, nil
}
