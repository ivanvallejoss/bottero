package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/ivanvallejoss/bottero/internal/config"
	"github.com/ivanvallejoss/bottero/internal/responder"
)

// These measurements show how long the handler takes per message and how
// much of it is the admission: everything before the save in state.db. They
// were written before the routing by chat, to compare the same numbers before
// and after it. On the homelab, set TMPDIR to a directory on the hard drive;
// elsewhere they only show that the measurement runs.

// timedMessages is how many messages TestHandlerTimes passes through the
// handler. At about 45 ms per save on the homelab's hard drive, it takes some
// ten seconds there.
const timedMessages = 200

// TestHandlerTimes passes messages through the whole handler, with state.db
// on disk, and logs p50, p99 and max: private texts and group tags, half and
// half. The worker does not run, so the saves do not wait for it; the state
// package measures that case.
func TestHandlerTimes(t *testing.T) {
	h := newHarness(t, testConfig)
	times := make([]time.Duration, timedMessages)
	for i := range timedMessages {
		var evt *events.Message
		if i%2 == 0 {
			evt = privateFromPhone(h.clock(), fmt.Sprintf("3AP%04d", i), "ping")
		} else {
			evt = h.groupTag(enabledGroup, fmt.Sprintf("3AG%04d", i), "ping")
		}
		start := time.Now()
		if !h.in.handle(evt) {
			t.Fatalf("handler did not handle %s", evt.Info.ID)
		}
		times[i] = time.Since(start)
	}
	if n := h.log.count("outcome=queued"); n != timedMessages {
		t.Fatalf("%d messages queued, want %d", n, timedMessages)
	}
	logTimes(t, "whole handler", times)
}

// BenchmarkAdmission measures what the bot does with a message before
// saving it: the event log's text filter, reducing the message, Admit, and
// encoding what is saved. The handler adds the save to this.
func BenchmarkAdmission(b *testing.B) {
	path := filepath.Join(b.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(testConfig), 0o600); err != nil {
		b.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		b.Fatal(err)
	}
	// No store: the benchmark stops before the save.
	in := newInbox(testDevice(), cfg, responder.New(cfg), nil, slog.New(slog.DiscardHandler), func() {})
	ctx := context.Background()
	// groupTag only reads the harness clock.
	events0 := &harness{now: t0}
	cases := []struct {
		name string
		evt  *events.Message
	}{
		{"private", privateFromPhone(t0, "3AP0001", "ping")},
		{"group", events0.groupTag(enabledGroup, "3AG0001", "ping")},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				in.includeText(c.evt)
				msg, how := reduce(ctx, in.device, c.evt)
				in.responder.Admit(msg)
				saved, err := newSavedMessage(c.evt, msg, how)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := encode(saved); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// privateFromPhone is a private text as it arrives from a phone: the sender
// as its LID and the phone number in SenderAlt, the way every private
// message arrived in the exploration stage.
func privateFromPhone(now time.Time, id, text string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:      lidJID(allowedLID),
				Sender:    types.JID{User: allowedLID, Device: 0, Server: types.HiddenUserServer},
				SenderAlt: pnJID(allowedPhone),
			},
			ID:        types.MessageID(id),
			Timestamp: now.Add(-time.Second),
		},
		Message: &waE2E.Message{Conversation: ptr(text)},
	}
}

// logTimes logs the p50, p99 and maximum of times.
func logTimes(t *testing.T, what string, times []time.Duration) {
	t.Helper()
	sorted := slices.Clone(times)
	slices.Sort(sorted)
	n := len(sorted)
	t.Logf("%s (%d): p50 %v  p99 %v  max %v", what, n,
		sorted[n/2].Round(time.Microsecond), sorted[n*99/100].Round(time.Microsecond), sorted[n-1].Round(time.Microsecond))
}
