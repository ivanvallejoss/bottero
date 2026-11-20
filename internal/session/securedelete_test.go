package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// secureDeleteParam is the parameter that turns on secure_delete in a
// connection string.
const secureDeleteParam = "&_pragma=secure_delete(1)"

// bufferMessages is how many messages of each size TestSessionBufferWrites
// saves and clears with each connection string.
const bufferMessages = 300

// plaintextMark starts every plaintext TestSessionBufferWrites saves, so the
// database files can be searched for what is left of it.
const plaintextMark = "texto-descifrado-"

// TestSessionBufferWrites runs whatsmeow's buffer of decrypted messages as
// the bot uses it (EnableDecryptedEventBuffer): the plaintext is saved inside
// the decryption transaction, and set to NULL once the handlers finish. It
// does so on the session connection string with and without secure_delete,
// logs how long each write takes, and reports where the plaintext is left.
//
// whatsmeow's real decryption transaction also reads and writes the Signal
// session; this one only saves the plaintext. It measures what secure_delete
// adds to these writes, not the whole decryption. On the homelab, set TMPDIR
// to a directory on the hard drive.
func TestSessionBufferWrites(t *testing.T) {
	variants := []struct {
		name   string
		dsn    func(dataDir string) string
		secure bool
	}{
		{"without secure_delete", func(dir string) string {
			return strings.Replace(sessionDSN(dir), secureDeleteParam, "", 1)
		}, false},
		{"with secure_delete", func(dir string) string {
			if dsn := sessionDSN(dir); strings.Contains(dsn, secureDeleteParam) {
				return dsn
			}
			return sessionDSN(dir) + secureDeleteParam
		}, true},
	}
	// A text message, and one the size of an image with its thumbnail, which
	// does not fit in a page and goes to overflow pages.
	sizes := []struct {
		name  string
		bytes int
	}{{"text", 120}, {"image", 20_000}}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			dsn := v.dsn(dir)
			buffer := openEventBuffer(t, dsn)
			wantSecureDelete(t, dsn, v.secure)

			for _, size := range sizes {
				puts := make([]time.Duration, bufferMessages)
				clears := make([]time.Duration, bufferMessages)
				for i := range bufferMessages {
					hash := sha256.Sum256(fmt.Appendf(nil, "%s-%d", size.name, i))
					plaintext := plaintextOf(size.bytes, i)

					start := time.Now()
					err := buffer.DoDecryptionTxn(ctx, func(ctx context.Context) error {
						return buffer.PutBufferedEvent(ctx, hash, plaintext, time.Now())
					})
					puts[i] = time.Since(start)
					if err != nil {
						t.Fatalf("save %s %d: %v", size.name, i, err)
					}

					start = time.Now()
					err = buffer.ClearBufferedEventPlaintext(ctx, hash)
					clears[i] = time.Since(start)
					if err != nil {
						t.Fatalf("clear %s %d: %v", size.name, i, err)
					}
				}
				logTimes(t, size.name+": save in the decryption transaction", puts)
				logTimes(t, size.name+": clear after the handlers", clears)
			}

			inWAL := fileHas(t, filepath.Join(dir, sessionDBFile+"-wal"), plaintextMark)
			checkpointTruncate(t, dsn)
			inDB := fileHas(t, filepath.Join(dir, sessionDBFile), plaintextMark)
			t.Logf("plaintext left: in session.db after a checkpoint %v; in the WAL before it %v", inDB, inWAL)
			if v.secure && inDB {
				t.Errorf("with secure_delete, cleared plaintext is still in %s", sessionDBFile)
			}
		})
	}
}

// openEventBuffer opens a session store on dsn, with a device stored so the
// buffer's rows have the device they belong to, and returns its buffer.
func openEventBuffer(t *testing.T, dsn string) store.EventBuffer {
	t.Helper()
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite", dsn, waLog.Noop)
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	t.Cleanup(func() { container.Close() })
	device := container.NewDevice()
	device.ID = &types.JID{User: botPhone, Device: 2, Server: types.DefaultUserServer}
	// Pairing fills in the account identity; the table requires its fields.
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{0},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := container.PutDevice(ctx, device); err != nil {
		t.Fatalf("store device: %v", err)
	}
	return device.EventBuffer
}

// wantSecureDelete checks the secure_delete setting a connection opened with
// dsn gets.
func wantSecureDelete(t *testing.T, dsn string, want bool) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got int
	if err := db.QueryRow("PRAGMA secure_delete").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if (got == 1) != want {
		t.Fatalf("PRAGMA secure_delete = %d with %s", got, dsn)
	}
}

// plaintextOf is a plaintext of n bytes made of the mark, so that every page
// it fills, overflow pages included, carries the mark.
func plaintextOf(n, i int) []byte {
	unit := fmt.Sprintf("%s%04d ", plaintextMark, i)
	return []byte(strings.Repeat(unit, n/len(unit)+1)[:n])
}

// checkpointTruncate copies the WAL into the database and empties it, on
// its own connection.
func checkpointTruncate(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var busy, logFrames, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatal(err)
	}
	if busy != 0 {
		t.Fatalf("checkpoint blocked by another connection")
	}
}

// fileHas reports whether the file at path contains mark. A missing file
// contains nothing.
func fileHas(t *testing.T, path, mark string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(data, []byte(mark))
}
