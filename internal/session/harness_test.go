package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/ivanvallejoss/bottero/internal/config"
	"github.com/ivanvallejoss/bottero/internal/responder"
	"github.com/ivanvallejoss/bottero/internal/state"
)

// testConfig answers "ping" in private and in the enabled group, has a
// default reply and confirms images.
const testConfig = `{
	"keywords": {"ping": "pong"},
	"default_reply": "No entendí.",
	"image_ack": "Imagen {size}",
	"allowed_people": ["` + allowedPhone + `"],
	"enabled_groups": ["` + enabledGroup + `"]
}`

func loadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// testDevice is the bot's device, with whatsmeow's LID-to-number mapping
// knowing the allowed person.
func testDevice() *store.Device {
	device := botDevice()
	device.LIDs = fakeLIDs{pn: map[types.JID]types.JID{lidJID(allowedLID): pnJID(allowedPhone)}}
	return device
}

// syncBuffer collects log lines written from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// lineWith returns the first log line that contains every part, or "".
func (s *syncBuffer) lineWith(parts ...string) string {
	for _, line := range strings.Split(s.String(), "\n") {
		ok := line != ""
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			return line
		}
	}
	return ""
}

func (s *syncBuffer) wantLine(t *testing.T, parts ...string) string {
	t.Helper()
	line := s.lineWith(parts...)
	if line == "" {
		t.Errorf("no log line with %q in:\n%s", parts, s.String())
	}
	return line
}

func (s *syncBuffer) count(parts ...string) int {
	n := 0
	for _, line := range strings.Split(s.String(), "\n") {
		ok := line != ""
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			n++
		}
	}
	return n
}

// sent is one call to SendMessage.
type sent struct {
	to  types.JID
	msg *waE2E.Message
	id  types.MessageID
}

// fakeSender stands in for whatsmeow.Client. SendMessage returns the queued
// errors in order and then succeeds. If block is set, each send waits for
// it (or for its context) before returning.
type fakeSender struct {
	mu      sync.Mutex
	sends   []sent
	errs    []error
	ids     int
	block   chan struct{}
	started chan struct{}
}

func (f *fakeSender) GenerateMessageID() types.MessageID {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids++
	return types.MessageID(fmt.Sprintf("3EB0REPLY%04d", f.ids))
}

func (f *fakeSender) SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	var id types.MessageID
	if len(extra) > 0 {
		id = extra[0].ID
	}
	f.mu.Lock()
	f.sends = append(f.sends, sent{to: to, msg: msg, id: id})
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	block, started := f.block, f.started
	f.mu.Unlock()

	if block != nil {
		if started != nil {
			started <- struct{}{}
		}
		select {
		case <-block:
		case <-ctx.Done():
			return whatsmeow.SendResponse{}, ctx.Err()
		}
	}
	if err != nil {
		return whatsmeow.SendResponse{}, err
	}
	return whatsmeow.SendResponse{ID: id, Timestamp: time.Now()}, nil
}

func (f *fakeSender) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sends...)
}

// harness is an inbox and a worker over a real state database, with a fake
// sender and a clock the test moves.
type harness struct {
	t      *testing.T
	dir    string
	st     *state.Store
	in     *inbox
	w      *worker
	sender *fakeSender
	log    *syncBuffer
	wakes  int

	mu  sync.Mutex
	now time.Time
}

func newHarness(t *testing.T, cfgBody string) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), sender: &fakeSender{}, log: &syncBuffer{}, now: t0}
	h.open(cfgBody)
	return h
}

// open opens the state database in the harness directory and builds a new
// inbox and worker on it, as a start of the bot does.
func (h *harness) open(cfgBody string) {
	h.t.Helper()
	st, err := state.Open(context.Background(), h.dir)
	if err != nil {
		h.t.Fatalf("state.Open: %v", err)
	}
	h.t.Cleanup(func() { st.Close() })
	h.st = st

	cfg := loadConfig(h.t, cfgBody)
	resp := responder.New(cfg)
	logger := slog.New(slog.NewTextHandler(h.log, nil))
	device := testDevice()

	h.w = newWorker(st, resp, h.sender, logger)
	h.w.now = h.clock
	h.w.connected.Store(true)
	h.in = newInbox(device, cfg, resp, st, logger, func() { h.wakes++ })
	h.in.now = h.clock
}

// restart closes the database and opens everything again with cfgBody.
func (h *harness) restart(cfgBody string) {
	h.t.Helper()
	h.st.Close()
	h.open(cfgBody)
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// receive hands evt to the inbox, as whatsmeow does, and fails if the inbox
// reports it as not handled.
func (h *harness) receive(evt *events.Message) {
	h.t.Helper()
	if !h.in.handle(evt) {
		h.t.Fatalf("inbox did not handle %s", evt.Info.ID)
	}
}

func (h *harness) pass() time.Time {
	return h.w.pass(context.Background())
}

// t0 is an arbitrary start time; tests move forward from it.
var t0 = time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)

// Messages as whatsmeow delivers them, sent a second before the harness
// clock.

func (h *harness) privateText(id, text string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: lidJID(allowedLID), Sender: lidJID(allowedLID)},
			ID:            types.MessageID(id),
			Timestamp:     h.clock().Add(-time.Second),
		},
		Message: &waE2E.Message{Conversation: ptr(text)},
	}
}

func (h *harness) privateImage(id string) *events.Message {
	evt := h.privateText(id, "")
	evt.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Mimetype: ptr("image/jpeg"), FileLength: ptr[uint64](2048), Width: ptr[uint32](800), Height: ptr[uint32](600),
	}}
	return evt
}

// groupTag is a message in chat that tags the bot by its LID, from a member
// on their phone (device 3).
func (h *harness) groupTag(chat, id, text string) *events.Message {
	jid, _ := types.ParseJID(chat)
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat: jid, IsGroup: true,
				Sender:    types.JID{User: otherLID, Device: 3, Server: types.HiddenUserServer},
				SenderAlt: pnJID(otherPhone),
			},
			ID:        types.MessageID(id),
			Timestamp: h.clock().Add(-time.Second),
		},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: ptr("@" + botLID + " " + text), ContextInfo: mentioning(lidJID(botLID).String()),
		}},
	}
}
