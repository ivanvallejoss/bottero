package session

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/ivanvallejoss/bottero/internal/responder"
	"github.com/ivanvallejoss/bottero/internal/state"
)

// openJob returns the message the worker would take now, or fails.
func (h *harness) openJob() state.Job {
	h.t.Helper()
	j, ok, err := h.st.Next(context.Background(), h.clock())
	if err != nil || !ok {
		h.t.Fatalf("Next = %v, %v; want an open message", ok, err)
	}
	return j
}

func (h *harness) wantNothingOpen() {
	h.t.Helper()
	if j, ok, err := h.st.Next(context.Background(), h.clock().Add(365*24*time.Hour)); err != nil || ok {
		h.t.Fatalf("Next = %s, %v, %v; want nothing open", j.MessageID, ok, err)
	}
}

func TestInboxSavesAPrivateMessage(t *testing.T) {
	h := newHarness(t, testConfig)
	evt := h.privateText("3A01", "ping")
	h.receive(evt)

	if h.wakes != 1 {
		t.Errorf("worker woken %d times, want 1", h.wakes)
	}
	h.log.wantLine(t, "outcome=queued", "id=3A01", "matched_via=lid_map")

	j := h.openJob()
	if j.Chat != lidJID(allowedLID).String() || j.MessageID != "3A01" {
		t.Errorf("saved as %s / %s", j.Chat, j.MessageID)
	}
	if !j.Sent.Equal(evt.Info.Timestamp) || !j.Received.Equal(h.clock()) {
		t.Errorf("times: sent %v, received %v", j.Sent, j.Received)
	}
	saved, err := decodeSaved(j.Content)
	if err != nil {
		t.Fatal(err)
	}
	// Only what Answer and the log need: no phone number, no quote.
	want := savedMessage{Text: "ping", Recognized: []string{"matched_via", "lid_map"}}
	if !reflect.DeepEqual(saved, want) {
		t.Errorf("saved content = %+v, want %+v", saved, want)
	}
}

func TestInboxSavesAGroupMessageWithItsQuote(t *testing.T) {
	h := newHarness(t, testConfig)
	evt := h.groupTag(enabledGroup, "3A02", "ping")
	h.receive(evt)
	h.log.wantLine(t, "outcome=queued", "id=3A02", "mentioned_as=lid")

	saved, err := decodeSaved(h.openJob().Content)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Group || saved.Text != " ping" {
		t.Errorf("saved content = %+v", saved)
	}
	// The author of the quote goes without the device number.
	if want := lidJID(otherLID).String(); saved.Author != want {
		t.Errorf("author = %q, want %q", saved.Author, want)
	}
	quote := &waE2E.Message{}
	if err := proto.Unmarshal(saved.Quote, quote); err != nil {
		t.Fatal(err)
	}
	if quote.GetConversation() != "@"+botLID+" ping" {
		t.Errorf("quote = %v, want the original text", quote)
	}
}

func TestInboxDoesNotSaveWhatTheBotDoesNotAttend(t *testing.T) {
	h := newHarness(t, testConfig)
	stranger := h.privateText("3A10", "ping")
	stranger.Info.Sender, stranger.Info.Chat = lidJID(otherLID), lidJID(otherLID)
	untagged := h.groupTag(enabledGroup, "3A11", "ping")
	untagged.Message = &waE2E.Message{Conversation: ptr("ping")}
	own := h.privateText("3A12", "ping")
	own.Info.IsFromMe = true

	for _, evt := range []any{stranger, untagged, own, "not a message"} {
		if !h.in.handle(evt) {
			t.Errorf("handle(%v) = false, want true", evt)
		}
	}
	if h.log.String() != "" {
		t.Errorf("logged:\n%s", h.log.String())
	}

	if !h.in.handle(h.groupTag(disabledGroup, "3A13", "ping")) {
		t.Error("tag in a group not enabled: handle = false")
	}
	h.log.wantLine(t, "outcome=group_not_enabled", "id=3A13")
	h.wantNothingOpen()
	if h.wakes != 0 {
		t.Errorf("worker woken %d times, want 0", h.wakes)
	}
}

func TestInboxRecognizesARepeatedDelivery(t *testing.T) {
	h := newHarness(t, testConfig)
	h.receive(h.privateText("3A20", "ping"))
	h.receive(h.privateText("3A20", "ping"))
	h.log.wantLine(t, "outcome=duplicate", "id=3A20")
	if h.wakes != 1 {
		t.Errorf("worker woken %d times, want 1", h.wakes)
	}
}

// fakeSaver fails, or panics, instead of saving.
type fakeSaver struct {
	err      error
	panicMsg string
	block    chan struct{}
	entered  chan struct{}
}

func (f *fakeSaver) Save(context.Context, state.Incoming) (bool, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	return f.err == nil, f.err
}

func inboxWith(t *testing.T, st saver) (*inbox, *syncBuffer) {
	t.Helper()
	cfg := loadConfig(t, testConfig)
	log := &syncBuffer{}
	return newInbox(testDevice(), cfg, responder.New(cfg), st, slog.New(slog.NewTextHandler(log, nil)), func() {}), log
}

// A message that could not be saved is reported as not handled, so the
// server delivers it again on the next connection.
func TestInboxReportsFailureWhenItCannotSave(t *testing.T) {
	in, log := inboxWith(t, &fakeSaver{err: errors.New("disk I/O error")})
	h := newHarness(t, testConfig)
	if in.handle(h.privateText("3A30", "ping")) {
		t.Error("handle = true after a failed save")
	}
	log.wantLine(t, "outcome=save_error", "id=3A30", "disk I/O error")
}

// whatsmeow would count a panic as success and confirm the message; the
// inbox turns it into a failure.
func TestInboxReportsFailureOnPanic(t *testing.T) {
	in, log := inboxWith(t, &fakeSaver{panicMsg: "boom"})
	h := newHarness(t, testConfig)
	if in.handle(h.privateText("3A31", "ping")) {
		t.Error("handle = true after a panic")
	}
	log.wantLine(t, "outcome=handler_panic", "id=3A31", "panic=boom")
}

func TestInboxRefusesAfterClose(t *testing.T) {
	h := newHarness(t, testConfig)
	h.in.close()
	if h.in.handle(h.privateText("3A40", "ping")) {
		t.Error("handle = true after close")
	}
	h.wantNothingOpen()
}

// close waits for a handler in progress: the databases close after it.
func TestInboxCloseWaitsForAHandlerInProgress(t *testing.T) {
	saver := &fakeSaver{block: make(chan struct{}), entered: make(chan struct{})}
	in, _ := inboxWith(t, saver)
	h := newHarness(t, testConfig)

	handled := make(chan bool)
	go func() { handled <- in.handle(h.privateText("3A41", "ping")) }()
	<-saver.entered

	closed := make(chan struct{})
	go func() {
		in.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned while a handler was saving")
	case <-time.After(50 * time.Millisecond):
	}

	close(saver.block)
	if !<-handled {
		t.Error("the handler in progress did not finish its save")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not return after the handler finished")
	}
}

func TestInboxHandlersAtOnceSaveOnce(t *testing.T) {
	h := newHarness(t, testConfig)
	evt := h.privateText("3A50", "ping")
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if !h.in.handle(evt) {
				t.Error("handle = false")
			}
		})
	}
	wg.Wait()
	if n := h.log.count("outcome=queued"); n != 1 {
		t.Errorf("%d deliveries queued, want 1", n)
	}
	if n := h.log.count("outcome=duplicate"); n != 9 {
		t.Errorf("%d deliveries recognized as repeated, want 9", n)
	}
}
