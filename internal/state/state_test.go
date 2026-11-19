package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests open real SQLite databases in t.TempDir(). On the homelab, set
// TMPDIR to a directory on the hard drive to run them against it.

// t0 is an arbitrary start time; tests move forward from it.
var t0 = time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func incoming(chat, id string, at time.Time, content string) Incoming {
	return Incoming{Chat: chat, MessageID: id, Sent: at.Add(-time.Second), Received: at, Content: []byte(content)}
}

func mustSave(t *testing.T, s *Store, in Incoming) {
	t.Helper()
	saved, err := s.Save(context.Background(), in)
	if err != nil {
		t.Fatalf("Save(%s): %v", in.MessageID, err)
	}
	if !saved {
		t.Fatalf("Save(%s) = false, want true", in.MessageID)
	}
}

// mustNext returns the message Next gives at now, or fails if there is none.
func mustNext(t *testing.T, s *Store, now time.Time) Job {
	t.Helper()
	j, ok, err := s.Next(context.Background(), now)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !ok {
		t.Fatalf("Next(%v) found nothing", now)
	}
	return j
}

func wantNone(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	j, ok, err := s.Next(context.Background(), now)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ok {
		t.Fatalf("Next(%v) = %s, want nothing", now, j.MessageID)
	}
}

func TestOpenSettings(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := context.Background()
	for _, tt := range []struct {
		pragma, want string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "2"}, // FULL
		{"secure_delete", "1"},
		{"user_version", fmt.Sprint(len(migrations))},
	} {
		var got string
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+tt.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", tt.pragma, err)
		}
		if got != tt.want {
			t.Errorf("PRAGMA %s = %s, want %s", tt.pragma, got, tt.want)
		}
	}
}

func TestOpenTwiceKeepsData(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	mustSave(t, s, incoming("a@lid", "A1", t0, "hola"))
	s.Close()

	s = openStore(t, dir)
	if j := mustNext(t, s, t0); j.MessageID != "A1" || string(j.Content) != "hola" {
		t.Errorf("after reopening, Next = %+v", j)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if s, err := Open(context.Background(), dir); err == nil {
		s.Close()
		t.Fatal("Open accepted a schema newer than the binary")
	}
}

// The state database of earlier versions had only the answered table of the
// dedup package. Its rows become closed records.
func TestMigrateAnsweredTable(t *testing.T) {
	dir := t.TempDir()
	old, err := sql.Open("sqlite", dsn(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS answered (
			message_id  TEXT PRIMARY KEY,
			chat        TEXT NOT NULL,
			sender      TEXT NOT NULL,
			answered_at TEXT NOT NULL
		)`,
		`INSERT INTO answered VALUES ('OLD1', 'a@lid', 'a@lid', '2026-11-18T10:00:00-03:00')`,
		`INSERT INTO answered VALUES ('OLD2', '120363000000000001@g.us', 'b@lid', 'not a date')`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	s := openStore(t, dir)
	ctx := context.Background()

	var tables int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'answered'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Error("the answered table is still there")
	}

	var closedAt int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT closed_at FROM messages WHERE chat = 'a@lid' AND message_id = 'OLD1' AND state = 'closed' AND outcome = 'answered'`,
	).Scan(&closedAt); err != nil {
		t.Fatalf("migrated record OLD1: %v", err)
	}
	if want := time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC); !time.UnixMilli(closedAt).Equal(want) {
		t.Errorf("OLD1 closed at %v, want %v", time.UnixMilli(closedAt).UTC(), want)
	}
	// A date that does not parse becomes the time of the migration, so the
	// record is pruned 14 days later instead of breaking the upgrade.
	if err := s.db.QueryRowContext(ctx, `SELECT closed_at FROM messages WHERE message_id = 'OLD2'`).Scan(&closedAt); err != nil {
		t.Fatalf("migrated record OLD2: %v", err)
	}
	if age := time.Since(time.UnixMilli(closedAt)); age < 0 || age > time.Minute {
		t.Errorf("OLD2 closed %v ago, want about now", age)
	}

	// A message answered before the upgrade is still a repeated delivery.
	saved, err := s.Save(ctx, incoming("a@lid", "OLD1", t0, "ping"))
	if err != nil || saved {
		t.Errorf("Save of a migrated message = %v, %v; want false, nil", saved, err)
	}
	wantNone(t, s, t0.Add(time.Hour))
}

func TestSaveRecognizesRepeatedDeliveries(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := context.Background()
	mustSave(t, s, incoming("a@lid", "M1", t0, "ping"))

	saved, err := s.Save(ctx, incoming("a@lid", "M1", t0.Add(time.Minute), "ping"))
	if err != nil || saved {
		t.Errorf("second Save of a pending message = %v, %v; want false, nil", saved, err)
	}

	// The same id in another chat is another message.
	mustSave(t, s, incoming("b@lid", "M1", t0, "ping"))

	// Still recognized once sending and once closed.
	j := mustNext(t, s, t0)
	if err := s.MarkSending(ctx, j.Seq, "R1", []byte("pong")); err != nil {
		t.Fatal(err)
	}
	if saved, _ := s.Save(ctx, incoming("a@lid", "M1", t0, "ping")); saved {
		t.Error("Save of a sending message = true")
	}
	if err := s.Finish(ctx, j.Seq, "replied", t0); err != nil {
		t.Fatal(err)
	}
	if saved, _ := s.Save(ctx, incoming("a@lid", "M1", t0, "ping")); saved {
		t.Error("Save of a closed message = true")
	}
}

// Two deliveries of the same message can be handled at once: the handler of
// a connection that dropped may still be running while the new connection
// delivers the message again. Exactly one save wins.
func TestSaveSameMessageAtOnce(t *testing.T) {
	s := openStore(t, t.TempDir())
	const n = 20
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		saved int
		errs  []error
	)
	for range n {
		wg.Go(func() {
			ok, err := s.Save(context.Background(), incoming("a@lid", "SAME", t0, "ping"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				saved++
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("Save errors: %v", errs)
	}
	if saved != 1 {
		t.Errorf("%d of %d concurrent saves won, want 1", saved, n)
	}
}

func TestNextKeepsTheOrderOfEachChat(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := context.Background()
	mustSave(t, s, incoming("a@lid", "A1", t0, ""))
	mustSave(t, s, incoming("a@lid", "A2", t0.Add(time.Second), ""))
	mustSave(t, s, incoming("b@lid", "B1", t0.Add(2*time.Second), ""))
	now := t0.Add(3 * time.Second)

	a1 := mustNext(t, s, now)
	if a1.MessageID != "A1" {
		t.Fatalf("first Next = %s, want A1", a1.MessageID)
	}

	// A1 waits for a retry: B1 goes ahead, A2 waits behind A1.
	retryAt := now.Add(time.Minute)
	if err := s.Retry(ctx, a1.Seq, 1, retryAt); err != nil {
		t.Fatal(err)
	}
	b1 := mustNext(t, s, now)
	if b1.MessageID != "B1" {
		t.Fatalf("Next while A1 waits = %s, want B1", b1.MessageID)
	}
	if err := s.Finish(ctx, b1.Seq, "replied", now); err != nil {
		t.Fatal(err)
	}
	wantNone(t, s, now)

	at, ok, err := s.NextAttempt(ctx)
	if err != nil || !ok || !at.Equal(retryAt) {
		t.Fatalf("NextAttempt = %v, %v, %v; want %v", at, ok, err, retryAt)
	}

	a1 = mustNext(t, s, retryAt)
	if a1.MessageID != "A1" || a1.Attempts != 1 {
		t.Fatalf("Next after the wait = %+v, want A1 with 1 attempt", a1)
	}
	if err := s.Finish(ctx, a1.Seq, "failed", retryAt); err != nil {
		t.Fatal(err)
	}
	if a2 := mustNext(t, s, retryAt); a2.MessageID != "A2" {
		t.Fatalf("Next after A1 closed = %s, want A2", a2.MessageID)
	}
}

func TestNextAttemptWithNothingOpen(t *testing.T) {
	s := openStore(t, t.TempDir())
	if _, ok, err := s.NextAttempt(context.Background()); err != nil || ok {
		t.Errorf("NextAttempt on an empty store = %v, %v; want false, nil", ok, err)
	}
}

// The reply built at the first attempt, and its id, survive a restart: the
// worker sends them again instead of building another reply.
func TestSendingSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	mustSave(t, s, incoming("a@lid", "M1", t0, "ping"))
	j := mustNext(t, s, t0)
	if j.Sending() {
		t.Fatal("a just-saved message is already sending")
	}
	if err := s.MarkSending(ctx, j.Seq, "R1", []byte("pong")); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(ctx, j.Seq, 2, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A message can be marked sending only once.
	if err := s.MarkSending(ctx, j.Seq, "R2", []byte("otra")); !errors.Is(err, ErrNotOpen) {
		t.Errorf("second MarkSending = %v, want ErrNotOpen", err)
	}
	s.Close()

	s = openStore(t, dir)
	j = mustNext(t, s, t0.Add(time.Minute))
	if !j.Sending() || j.ReplyID != "R1" || string(j.Reply) != "pong" || j.Attempts != 2 || string(j.Content) != "ping" {
		t.Errorf("after restart, Next = %+v", j)
	}
	if !j.Sent.Equal(t0.Add(-time.Second)) || !j.Received.Equal(t0) {
		t.Errorf("times after restart: sent %v, received %v", j.Sent, j.Received)
	}
}

func TestChangesNeedAnOpenMessage(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := context.Background()
	mustSave(t, s, incoming("a@lid", "M1", t0, ""))
	j := mustNext(t, s, t0)
	if err := s.Finish(ctx, j.Seq, "no_match", t0); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"MarkSending": s.MarkSending(ctx, j.Seq, "R1", nil),
		"Retry":       s.Retry(ctx, j.Seq, 1, t0),
		"Finish":      s.Finish(ctx, j.Seq, "failed", t0),
		"unknown seq": s.Finish(ctx, 9999, "failed", t0),
	} {
		if !errors.Is(err, ErrNotOpen) {
			t.Errorf("%s on a closed message = %v, want ErrNotOpen", name, err)
		}
	}
}

func TestFinishErasesContentAndReply(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := context.Background()
	mustSave(t, s, incoming("a@lid", "M1", t0, "ping"))
	j := mustNext(t, s, t0)
	if err := s.MarkSending(ctx, j.Seq, "R1", []byte("pong")); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, j.Seq, "replied", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	var (
		content, reply []byte
		replyID        sql.NullString
		outcome        string
		closedAt       int64
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT content, reply, reply_id, outcome, closed_at FROM messages WHERE seq = ?`, j.Seq,
	).Scan(&content, &reply, &replyID, &outcome, &closedAt); err != nil {
		t.Fatal(err)
	}
	if content != nil || reply != nil {
		t.Errorf("closed message keeps content %q and reply %q", content, reply)
	}
	if replyID.String != "R1" || outcome != "replied" || closedAt != t0.Add(time.Second).UnixMilli() {
		t.Errorf("closed record = reply_id %q, outcome %q, closed_at %d", replyID.String, outcome, closedAt)
	}
}

// After closing a message and checkpointing, nothing of its content or its
// reply is left in the database files: secure_delete zeroes it in the
// database and the checkpoint truncates the WAL, which holds the earlier
// copies of the pages.
func TestNoContentLeftInFiles(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	ctx := context.Background()

	const textMark, replyMark = "texto-que-tiene-que-desaparecer", "respuesta-que-tiene-que-desaparecer"
	// One small message, and one larger than a page, whose content goes to
	// overflow pages that are freed when it is erased.
	small := incoming("a@lid", "SMALL", t0, textMark)
	large := incoming("b@lid", "LARGE", t0, strings.Repeat(textMark+" ", 600))
	for _, in := range []Incoming{small, large} {
		mustSave(t, s, in)
		j := mustNext(t, s, t0)
		if err := s.MarkSending(ctx, j.Seq, "R-"+in.MessageID, []byte(strings.Repeat(replyMark+" ", 400))); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, j.Seq, "replied", t0); err != nil {
			t.Fatal(err)
		}
	}

	// Before the checkpoint, the WAL still has the content. This is what the
	// checkpoint is for, and it shows the search below works.
	if found := filesContaining(t, dir, textMark); !slices.Contains(found, stateDBFile+"-wal") {
		t.Fatalf("before the checkpoint the WAL should still hold the content; found in %v", found)
	}

	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mark := range []string{textMark, replyMark} {
		if found := filesContaining(t, dir, mark); len(found) > 0 {
			t.Errorf("%q still in %v after closing and checkpointing", mark, found)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, stateDBFile+"-wal")); err == nil && info.Size() != 0 {
		t.Errorf("WAL is %d bytes after the checkpoint, want 0", info.Size())
	}

	// The records are still there for repeated deliveries.
	if saved, _ := s.Save(ctx, small); saved {
		t.Error("a closed message was saved again after the checkpoint")
	}
}

// filesContaining returns the names of the files in dir that contain mark.
func filesContaining(t *testing.T, dir, mark string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(mark)) {
			found = append(found, e.Name())
		}
	}
	return found
}

func TestPrune(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := context.Background()
	for _, id := range []string{"OLD", "RECENT", "OPEN"} {
		mustSave(t, s, incoming("a@"+id, id, t0, ""))
	}
	closeAt := map[string]time.Time{"OLD": t0, "RECENT": t0.Add(10 * 24 * time.Hour)}
	for range 2 {
		j := mustNext(t, s, t0)
		if err := s.Finish(ctx, j.Seq, "replied", closeAt[j.MessageID]); err != nil {
			t.Fatal(err)
		}
	}

	// Fourteen days and an hour after t0, the cutoff is an hour after t0:
	// OLD (closed at t0) goes, RECENT (closed ten days later) stays.
	now := t0.Add(14*24*time.Hour + time.Hour)
	n, err := s.Prune(ctx, now.Add(-14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Prune deleted %d records, want 1", n)
	}
	// The pruned message is no longer recognized; the recent one is.
	if saved, _ := s.Save(ctx, incoming("a@OLD", "OLD", t0, "")); !saved {
		t.Error("a pruned record still blocks the message")
	}
	if saved, _ := s.Save(ctx, incoming("a@RECENT", "RECENT", t0, "")); saved {
		t.Error("a recent record was pruned")
	}
	// An open message is never pruned, however old.
	if _, err := s.Prune(ctx, t0.Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if j := mustNext(t, s, t0); j.MessageID != "OPEN" {
		t.Errorf("Next after pruning = %s, want OPEN", j.MessageID)
	}
}

// The handler saves while the worker takes, marks, finishes and checkpoints,
// as they will in the bot. Nothing fails, nothing is lost, and the log shows
// how long a save waits on this disk while the worker writes.
func TestHandlerAndWorkerTogether(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := context.Background()
	const n = 200

	saveTimes := make([]time.Duration, n)
	handlerDone := make(chan struct{})
	var handlerErr error
	go func() {
		defer close(handlerDone)
		for i := range n {
			start := time.Now()
			saved, err := s.Save(ctx, incoming(fmt.Sprintf("chat%d@lid", i%7), fmt.Sprintf("M%03d", i), t0, "ping"))
			saveTimes[i] = time.Since(start)
			if err != nil || !saved {
				handlerErr = fmt.Errorf("save %d: %v, %v", i, saved, err)
				return
			}
		}
	}()

	finished := 0
	for finished < n {
		j, ok, err := s.Next(ctx, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			select {
			case <-handlerDone:
				if handlerErr != nil {
					t.Fatal(handlerErr)
				}
			default:
			}
			if err := s.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
			continue
		}
		if err := s.MarkSending(ctx, j.Seq, "R"+j.MessageID, []byte("pong")); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, j.Seq, "replied", t0); err != nil {
			t.Fatal(err)
		}
		finished++
	}
	<-handlerDone
	if handlerErr != nil {
		t.Fatal(handlerErr)
	}

	slices.Sort(saveTimes)
	t.Logf("save while the worker writes: p50 %v  p99 %v  max %v",
		saveTimes[n/2].Round(time.Microsecond), saveTimes[n*99/100].Round(time.Microsecond), saveTimes[n-1].Round(time.Microsecond))
}
