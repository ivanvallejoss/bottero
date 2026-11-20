package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/ivanvallejoss/bottero/internal/state"
)

func TestWorkerRepliesInPrivate(t *testing.T) {
	h := newHarness(t, testConfig)
	h.receive(h.privateText("3A01", "ping"))
	h.advance(2 * time.Second)

	if next := h.pass(); !next.IsZero() {
		t.Errorf("pass returned %v with nothing left, want zero", next)
	}
	sends := h.sender.all()
	if len(sends) != 1 {
		t.Fatalf("%d sends, want 1", len(sends))
	}
	s := sends[0]
	if s.to != lidJID(allowedLID) || s.msg.GetConversation() != "pong" || s.id == "" {
		t.Errorf("sent %q to %s with id %q", s.msg.GetConversation(), s.to, s.id)
	}
	h.log.wantLine(t, "outcome=replied", "id=3A01", "matched_via=lid_map", "keyword=ping", "default_reply=false",
		"reply_id="+string(s.id), "attempt=1", "waited=2s")
	h.wantNothingOpen()

	// The record stays: a repeated delivery is not answered again.
	h.receive(h.privateText("3A01", "ping"))
	h.log.wantLine(t, "outcome=duplicate", "id=3A01")
}

func TestWorkerQuotesInGroups(t *testing.T) {
	h := newHarness(t, testConfig)
	h.receive(h.groupTag(enabledGroup, "3A02", "ping"))
	h.receive(h.groupTag(enabledGroup, "3A03", "hola"))
	h.pass()

	sends := h.sender.all()
	if len(sends) != 2 {
		t.Fatalf("%d sends, want 2", len(sends))
	}
	for i, want := range []struct{ id, text, quoted string }{
		{"3A02", "pong", "@" + botLID + " ping"},
		{"3A03", "No entendí.", "@" + botLID + " hola"},
	} {
		ext := sends[i].msg.GetExtendedTextMessage()
		ci := ext.GetContextInfo()
		if ext.GetText() != want.text || ci.GetStanzaID() != want.id ||
			ci.GetParticipant() != lidJID(otherLID).String() || ci.GetQuotedMessage().GetConversation() != want.quoted {
			t.Errorf("reply to %s = %v", want.id, sends[i].msg)
		}
	}
	h.log.wantLine(t, "outcome=replied", "id=3A02", "mentioned_as=lid", "keyword=ping")
	h.log.wantLine(t, "outcome=replied", "id=3A03", "keyword=", "default_reply=true")
}

func TestWorkerConfirmsFiles(t *testing.T) {
	h := newHarness(t, testConfig)
	h.receive(h.privateImage("3A04"))
	h.pass()
	sends := h.sender.all()
	if len(sends) != 1 || sends[0].msg.GetConversation() != "Imagen 2 KB" {
		t.Fatalf("sends = %v", sends)
	}
	h.log.wantLine(t, "outcome=file_ack", "id=3A04", "media=image", "attempt=1")
}

func TestWorkerStaysSilentWithNoMatch(t *testing.T) {
	h := newHarness(t, testConfig)
	h.receive(h.privateText("3A05", "hola, ¿qué tal?"))
	h.pass()
	if sends := h.sender.all(); len(sends) != 0 {
		t.Fatalf("sends = %v, want none", sends)
	}
	h.log.wantLine(t, "outcome=no_match", "id=3A05", "matched_via=lid_map")
	h.wantNothingOpen()
}

// A send that can be retried waits 10 s, 30 s, 2 min and 10 min, always with
// the same reply and the same id; after the fifth failure the message closes
// as failed.
func TestWorkerRetriesWithTheSameReply(t *testing.T) {
	h := newHarness(t, testConfig)
	timeout := fmt.Errorf("send: %w", whatsmeow.ErrMessageTimedOut)
	h.sender.errs = []error{timeout, timeout, timeout, timeout, timeout}
	h.receive(h.privateText("3A06", "ping"))

	for i, wait := range retryWaits {
		next := h.pass()
		if want := h.clock().Add(wait); !next.Equal(want) {
			t.Fatalf("after attempt %d, next attempt at %v, want %v", i+1, next, want)
		}
		h.log.wantLine(t, "outcome=retry", "id=3A06", fmt.Sprintf("attempt=%d", i+1))
		// Not due yet: nothing is sent.
		h.advance(wait - time.Second)
		h.pass()
		if n := len(h.sender.all()); n != i+1 {
			t.Fatalf("%d sends before the wait ended, want %d", n, i+1)
		}
		h.advance(time.Second)
	}
	h.pass()
	h.log.wantLine(t, "outcome=failed", "id=3A06", "attempt=5", "timed out")
	h.wantNothingOpen()

	sends := h.sender.all()
	if len(sends) != 5 {
		t.Fatalf("%d sends, want 5", len(sends))
	}
	for _, s := range sends[1:] {
		if s.id != sends[0].id || s.msg.GetConversation() != "pong" {
			t.Errorf("a retry sent %q with id %q; the first attempt had id %q", s.msg.GetConversation(), s.id, sends[0].id)
		}
	}
}

func TestWorkerSucceedsOnARetry(t *testing.T) {
	h := newHarness(t, testConfig)
	h.sender.errs = []error{fmt.Errorf("%w 500", whatsmeow.ErrServerReturnedError)}
	h.receive(h.privateText("3A07", "ping"))
	h.pass()
	h.advance(retryWaits[0])
	h.pass()
	sends := h.sender.all()
	if len(sends) != 2 || sends[0].id != sends[1].id {
		t.Fatalf("sends = %v", sends)
	}
	h.log.wantLine(t, "outcome=replied", "id=3A07", "attempt=2", "reply_id="+string(sends[0].id))
}

func TestWorkerGivesUpOnPermanentErrors(t *testing.T) {
	h := newHarness(t, testConfig)
	h.sender.errs = []error{fmt.Errorf("failed to get group members: %w", whatsmeow.ErrNotInGroup)}
	h.receive(h.groupTag(enabledGroup, "3A08", "ping"))
	h.pass()
	if n := len(h.sender.all()); n != 1 {
		t.Fatalf("%d sends, want 1", n)
	}
	h.log.wantLine(t, "outcome=failed", "id=3A08", "attempt=1")
	h.wantNothingOpen()
}

// Without a connection the attempt does not count: the message waits as it
// was, with its reply and id, until the connection is back.
func TestWorkerWaitsForTheConnection(t *testing.T) {
	h := newHarness(t, testConfig)
	h.sender.errs = []error{whatsmeow.ErrNotConnected}
	h.receive(h.privateText("3A09", "ping"))

	if next := h.pass(); !next.Equal(h.clock().Add(pause)) {
		t.Errorf("after a send with no connection, next look at %v, want %v", next, h.clock().Add(pause))
	}
	h.log.wantLine(t, "outcome=send_deferred", "id=3A09")
	if j := h.openJob(); j.Attempts != 0 || !j.Sending() {
		t.Errorf("after a send with no connection: attempts %d, sending %v", j.Attempts, j.Sending())
	}

	// While disconnected, the worker does not try.
	h.w.SetConnected(false)
	if next := h.pass(); !next.IsZero() {
		t.Errorf("pass while disconnected returned %v, want zero", next)
	}
	if n := len(h.sender.all()); n != 1 {
		t.Fatalf("%d sends while disconnected, want 1", n)
	}

	h.w.SetConnected(true)
	h.pass()
	sends := h.sender.all()
	if len(sends) != 2 || sends[1].id != sends[0].id {
		t.Fatalf("sends = %v", sends)
	}
	h.log.wantLine(t, "outcome=replied", "id=3A09", "attempt=1")
}

func TestWorkerLetsOldMessagesExpire(t *testing.T) {
	h := newHarness(t, testConfig)
	old := h.privateText("3A0A", "ping")
	old.Info.Timestamp = h.clock().Add(-maxAge - time.Minute)
	h.receive(old)
	h.receive(h.privateText("3A0B", "ping"))
	h.pass()

	sends := h.sender.all()
	if len(sends) != 1 {
		t.Fatalf("%d sends, want 1 (only the recent message)", len(sends))
	}
	h.log.wantLine(t, "outcome=expired", "id=3A0A", "age=24h1m0s")
	h.log.wantLine(t, "outcome=replied", "id=3A0B")
	h.wantNothingOpen()
}

// A message waiting for a retry holds back the later ones of its chat, not
// those of other chats.
func TestWorkerKeepsTheOrderOfEachChat(t *testing.T) {
	h := newHarness(t, testConfig)
	h.sender.errs = []error{whatsmeow.ErrMessageTimedOut}
	h.receive(h.privateText("A1", "ping"))
	h.receive(h.privateText("A2", "ping"))
	h.receive(h.groupTag(enabledGroup, "B1", "ping"))

	h.pass()
	h.advance(retryWaits[0])
	h.pass()

	var order []string
	for _, s := range h.sender.all() {
		if q := s.msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID(); q != "" {
			order = append(order, q)
			continue
		}
		order = append(order, "private")
	}
	// A1 fails, B1 goes ahead, then A1 again and only after it A2.
	want := []string{"private", "B1", "private", "private"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("send order = %v, want %v", order, want)
	}
	log := h.log.String()
	first, second := strings.Index(log, "outcome=replied id=A1 "), strings.Index(log, "outcome=replied id=A2 ")
	if first < 0 || second < 0 || first > second {
		t.Errorf("A1 and A2 not replied in order:\n%s", log)
	}
}

// After a restart, a message that was sending goes out with the reply and
// the id built before, even if the configuration changed in between.
func TestWorkerResendsTheStoredReplyAfterARestart(t *testing.T) {
	h := newHarness(t, testConfig)
	h.sender.errs = []error{whatsmeow.ErrMessageTimedOut}
	h.receive(h.privateText("3A0C", "ping"))
	h.pass()
	first := h.sender.all()[0]

	h.restart(`{"keywords": {"ping": "otra respuesta"}, "allowed_people": ["` + allowedPhone + `"]}`)
	h.advance(retryWaits[0])
	h.pass()

	sends := h.sender.all()
	if len(sends) != 2 {
		t.Fatalf("%d sends, want 2", len(sends))
	}
	if sends[1].id != first.id || sends[1].msg.GetConversation() != "pong" {
		t.Errorf("after the restart sent %q with id %q, want %q with id %q",
			sends[1].msg.GetConversation(), sends[1].id, "pong", first.id)
	}
	h.log.wantLine(t, "outcome=replied", "id=3A0C", "keyword=ping", "attempt=2")
}

// Once nothing is due, the WAL is truncated: the content of the closed
// messages is no longer on disk.
func TestWorkerTruncatesTheWALWhenIdle(t *testing.T) {
	h := newHarness(t, testConfig)
	h.receive(h.privateText("3A0D", "ping"))
	h.pass()
	info, err := os.Stat(filepath.Join(h.dir, "state.db-wal"))
	if err == nil && info.Size() != 0 {
		t.Errorf("WAL is %d bytes after the pass, want 0", info.Size())
	}
}

func TestWorkerPrunesOldRecords(t *testing.T) {
	h := newHarness(t, testConfig)
	h.receive(h.privateText("3A0E", "ping"))
	h.pass()
	h.advance(keepRecords + time.Minute)
	h.w.prune(context.Background())
	h.log.wantLine(t, "outcome=pruned", "records=1")
	// The record is gone, so the same id is a new message again.
	h.receive(h.privateText("3A0E", "ping"))
	h.log.wantLine(t, "outcome=queued", "id=3A0E")
}

// The real loop, with real time: a saved message is answered at once, and at
// shutdown the send in progress finishes and is recorded before run returns.
func TestWorkerRunFinishesTheSendInProgress(t *testing.T) {
	h := newHarness(t, testConfig)
	h.w.now = time.Now
	h.in.now = time.Now
	h.in.onSaved = h.w.Wake
	h.sender.block = make(chan struct{})
	h.sender.started = make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.w.run(ctx)
	}()

	evt := h.privateText("3A0F", "ping")
	evt.Info.Timestamp = time.Now()
	h.receive(evt)
	select {
	case <-h.sender.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the saved message was not sent")
	}

	cancel()
	select {
	case <-done:
		t.Fatal("run returned while a send was in progress")
	case <-time.After(50 * time.Millisecond):
	}

	close(h.sender.block)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after the send finished")
	}
	h.log.wantLine(t, "outcome=replied", "id=3A0F")
	h.wantNothingOpen()

	// After run returned, nothing else is taken.
	h.receive(h.privateText("3A10", "ping"))
	time.Sleep(50 * time.Millisecond)
	if n := len(h.sender.all()); n != 1 {
		t.Errorf("%d sends after run returned, want 1", n)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		want failure
	}{
		{whatsmeow.ErrNotConnected, notConnected},
		{fmt.Errorf("failed to send: %w", whatsmeow.ErrNotConnected), notConnected},
		{whatsmeow.ErrNotLoggedIn, notConnected},
		{&whatsmeow.DisconnectedError{Action: "message send"}, notConnected},
		{fmt.Errorf("%w: no LID found", whatsmeow.ErrRecipientNotOnWhatsApp), permanent},
		{fmt.Errorf("failed to get group members: %w", whatsmeow.ErrNotInGroup), permanent},
		{whatsmeow.ErrGroupNotFound, permanent},
		{whatsmeow.ErrNoPermissionInGroup, permanent},
		{whatsmeow.ErrUnknownServer, permanent},
		{whatsmeow.ErrMessageTimedOut, transient},
		{fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError), transient},
		{whatsmeow.ErrIQTimedOut, transient},
		{context.DeadlineExceeded, transient},
		{errors.New("something else"), transient},
	}
	for _, tt := range tests {
		if got := classify(tt.err); got != tt.want {
			t.Errorf("classify(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

// A stored message that cannot be read closes as failed instead of being
// taken again and again.
func TestWorkerClosesUnusableMessages(t *testing.T) {
	h := newHarness(t, testConfig)
	h.receive(h.groupTag(enabledGroup, "3A11", "ping"))
	j := h.openJob()
	if err := h.st.Finish(context.Background(), j.Seq, "test", h.clock()); err != nil {
		t.Fatal(err)
	}
	// A new message whose stored quote is not protobuf.
	saved, _ := encode(savedMessage{Group: true, Text: " ping", Quote: []byte{0xff, 0xff}})
	if _, err := h.st.Save(context.Background(), state.Incoming{
		Chat: enabledGroup, MessageID: "3A12", Sent: h.clock(), Received: h.clock(), Content: saved,
	}); err != nil {
		t.Fatal(err)
	}
	h.pass()
	h.log.wantLine(t, "outcome=failed", "id=3A12", "decode quote")
	h.wantNothingOpen()
	if n := len(h.sender.all()); n != 0 {
		t.Errorf("%d sends, want 0", n)
	}
}

// A reply that goes out 15 minutes or more after the person sent the
// message starts with the delay notice, in the same message.
func TestWorkerAddsTheDelayNoticeToLateReplies(t *testing.T) {
	h := newHarness(t, testConfigWithNotice)
	late := h.privateText("3A20", "ping")
	late.Info.Timestamp = h.clock().Add(-lateAfter)
	onTime := h.privateText("3A21", "ping")
	onTime.Info.Timestamp = h.clock().Add(-lateAfter + time.Second)
	lateInGroup := h.groupTag(enabledGroup, "3A22", "ping")
	lateInGroup.Info.Timestamp = h.clock().Add(-time.Hour)
	lateImage := h.privateImage("3A23")
	lateImage.Info.Timestamp = h.clock().Add(-time.Hour)
	for _, evt := range []*events.Message{late, onTime, lateInGroup, lateImage} {
		h.receive(evt)
	}
	h.pass()

	sends := h.sender.all()
	if len(sends) != 4 {
		t.Fatalf("%d sends, want 4", len(sends))
	}
	texts := []string{
		sends[0].msg.GetConversation(),
		sends[1].msg.GetConversation(),
		sends[2].msg.GetExtendedTextMessage().GetText(),
		sends[3].msg.GetConversation(),
	}
	want := []string{"Perdón la demora.\n\npong", "pong", "Perdón la demora.\n\npong", "Perdón la demora.\n\nImagen 2 KB"}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("reply %d = %q, want %q", i, texts[i], want[i])
		}
	}
	// In the group the late reply still quotes the message.
	if q := sends[2].msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID(); q != "3A22" {
		t.Errorf("late group reply quotes %q, want 3A22", q)
	}
	h.log.wantLine(t, "outcome=replied", "id=3A20", "delay_notice=true")
	h.log.wantLine(t, "outcome=replied", "id=3A21", "delay_notice=false")
	h.log.wantLine(t, "outcome=file_ack", "id=3A23", "delay_notice=true")
}

// The notice is decided at the first attempt. A reply that was on time then
// keeps its text, and its id, even if a retry goes out after 15 minutes.
func TestWorkerKeepsTheReplyOfTheFirstAttempt(t *testing.T) {
	h := newHarness(t, testConfigWithNotice)
	timeout := whatsmeow.ErrMessageTimedOut
	h.sender.errs = []error{timeout, timeout, timeout}
	evt := h.privateText("3A24", "ping")
	evt.Info.Timestamp = h.clock().Add(-14 * time.Minute)
	h.receive(evt)

	h.pass()
	for _, wait := range retryWaits[:3] {
		h.advance(wait)
		h.pass()
	}
	if age := h.clock().Sub(evt.Info.Timestamp); age < lateAfter {
		t.Fatalf("the last attempt went out %v after the message; the test needs it past %v", age, lateAfter)
	}

	sends := h.sender.all()
	if len(sends) != 4 {
		t.Fatalf("%d sends, want 4", len(sends))
	}
	for i, s := range sends {
		if s.msg.GetConversation() != "pong" || s.id != sends[0].id {
			t.Errorf("attempt %d sent %q with id %q", i+1, s.msg.GetConversation(), s.id)
		}
	}
	h.log.wantLine(t, "outcome=replied", "id=3A24", "delay_notice=false", "attempt=4")
}

// Without a notice in the configuration, late replies go as they are.
func TestWorkerWithoutADelayNotice(t *testing.T) {
	h := newHarness(t, testConfig)
	evt := h.privateText("3A25", "ping")
	evt.Info.Timestamp = h.clock().Add(-time.Hour)
	h.receive(evt)
	h.pass()
	if sends := h.sender.all(); len(sends) != 1 || sends[0].msg.GetConversation() != "pong" {
		t.Fatalf("sends = %v", sends)
	}
	h.log.wantLine(t, "outcome=replied", "id=3A25", "delay_notice=false")
}
