package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
// and after it; BenchmarkAnswer measures the worker's side of the routing. On
// the homelab, set TMPDIR to a directory on the hard drive; elsewhere they
// only show that the measurement runs.

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
// saving it: the event log's text filter, reducing the message, Admit with
// its choice of route, and encoding what is saved. The handler adds the save
// to this. It runs with testConfig's single route and with fifty more routes
// of ten chats each, to show the choice of route does not grow with the
// configuration.
func BenchmarkAdmission(b *testing.B) {
	events0 := &harness{now: t0} // groupTag only reads the harness clock
	messages := []struct {
		name string
		evt  *events.Message
	}{
		{"private", privateFromPhone(t0, "3AP0001", "ping")},
		{"group", events0.groupTag(enabledGroup, "3AG0001", "ping")},
	}
	ctx := context.Background()
	for _, m := range messages {
		for _, size := range benchSizes() {
			b.Run(m.name+"/"+size.name, func(b *testing.B) {
				cfg := benchLoad(b, size.body)
				// No store: the benchmark stops before the save.
				in := newInbox(testDevice(), cfg, responder.New(cfg), nil, slog.New(slog.DiscardHandler), func() {})
				for b.Loop() {
					in.includeText(m.evt)
					msg, how := reduce(ctx, in.device, m.evt)
					admission, route := in.responder.Admit(msg)
					if admission != responder.Admitted {
						b.Fatalf("message not admitted: %v", admission)
					}
					msg.Route = route
					saved, err := newSavedMessage(m.evt, msg, how)
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
}

// BenchmarkAnswer measures what the worker does with a saved message before
// building the reply: reading what was saved, finding the rules of its
// route and choosing the answer.
func BenchmarkAnswer(b *testing.B) {
	content, err := encode(savedMessage{Route: "main", Text: "ping", Recognized: []string{"matched_via", "sender_alt"}})
	if err != nil {
		b.Fatal(err)
	}
	for _, size := range benchSizes() {
		b.Run(size.name, func(b *testing.B) {
			resp := responder.New(benchLoad(b, size.body))
			for b.Loop() {
				saved, err := decodeSaved(content)
				if err != nil {
					b.Fatal(err)
				}
				if d := resp.Answer(saved.message()); d.Outcome != responder.Reply {
					b.Fatalf("answer = %+v", d)
				}
			}
		})
	}
}

// benchSizes are the configurations the benchmarks run with: testConfig,
// and testConfig's route plus fifty routes of five people and five groups.
func benchSizes() []struct{ name, body string } {
	var routes strings.Builder
	for r := range 50 {
		people, groups := make([]string, 5), make([]string, 5)
		for c := range 5 {
			people[c] = fmt.Sprintf(`"549362%07d"`, r*5+c)
			groups[c] = fmt.Sprintf(`"120363%012d@g.us"`, 100+r*5+c)
		}
		fmt.Fprintf(&routes, `"r%02d": {"people": [%s], "groups": [%s], "rules": {"keywords": {"ping": "pong %d"}}},`,
			r, strings.Join(people, ","), strings.Join(groups, ","), r)
	}
	large := `{"routes": {` + routes.String() + `
		"main": {
			"people": ["` + allowedPhone + `"],
			"groups": ["` + enabledGroup + `"],
			"rules": {"keywords": {"ping": "pong"}, "default_reply": "No entendí.", "image_ack": "Imagen {size}"}
		}
	}}`
	return []struct{ name, body string }{{"1-route", testConfig}, {"51-routes", large}}
}

func benchLoad(b *testing.B, body string) *config.Config {
	b.Helper()
	path := filepath.Join(b.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		b.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		b.Fatal(err)
	}
	return cfg
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
