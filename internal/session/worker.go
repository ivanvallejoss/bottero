package session

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/ivanvallejoss/bottero/internal/responder"
	"github.com/ivanvallejoss/bottero/internal/state"
)

const (
	// sendTimeout bounds one attempt to send a reply. At shutdown, the send
	// in progress gets this long to finish.
	sendTimeout = 30 * time.Second

	// maxAge is how long after the person sent it a message is still
	// answered. Older ones close as expired.
	maxAge = 24 * time.Hour

	// keepRecords is how long the record of a closed message stays, so a
	// repeated delivery is recognized. The WhatsApp server keeps undelivered
	// events for up to 14 days.
	keepRecords = 14 * 24 * time.Hour
	pruneEvery  = 12 * time.Hour

	// pause is the wait before trying again after the state database failed
	// or a send found no connection.
	pause = 5 * time.Second
)

// retryWaits are the waits after each failed attempt that can be retried.
// After a failure past the last wait, the message closes as failed: five
// attempts in all.
var retryWaits = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// sender is the part of whatsmeow.Client the worker uses.
type sender interface {
	GenerateMessageID() types.MessageID
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

var _ sender = (*whatsmeow.Client)(nil)

// worker answers the messages the inbox saved, one at a time and in the
// order they arrived within each chat. It is the only part of the bot that
// sends.
type worker struct {
	store     *state.Store
	responder *responder.Responder
	client    sender
	log       *slog.Logger
	now       func() time.Time
	wake      chan struct{}
	// connected follows whatsmeow's Connected and Disconnected events. The
	// worker sends only while it is true.
	connected atomic.Bool
}

func newWorker(st *state.Store, resp *responder.Responder, client sender, logger *slog.Logger) *worker {
	return &worker{
		store:     st,
		responder: resp,
		client:    client,
		log:       logger.With("component", "responder"),
		now:       time.Now,
		wake:      make(chan struct{}, 1),
	}
}

// Wake makes the worker look for messages now; the inbox calls it after
// saving one.
func (w *worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// SetConnected tells the worker whether whatsmeow is connected and logged
// in. Coming up, it also wakes the worker for the messages that waited.
func (w *worker) SetConnected(up bool) {
	w.connected.Store(up)
	if up {
		w.Wake()
	}
}

// run answers messages until ctx is cancelled. A send in progress at that
// moment finishes (within sendTimeout) and is recorded before run returns;
// no new message is taken.
func (w *worker) run(ctx context.Context) {
	var nextPrune time.Time
	for ctx.Err() == nil {
		if now := w.now(); !now.Before(nextPrune) {
			w.prune(ctx)
			nextPrune = now.Add(pruneEvery)
		}
		var until time.Time
		if w.connected.Load() {
			until = w.pass(ctx)
		}
		w.sleep(ctx, until, nextPrune)
	}
}

// sleep waits until the earlier of until (if set) and prune, a call to Wake,
// or the end of ctx.
func (w *worker) sleep(ctx context.Context, until, prune time.Time) {
	deadline := prune
	if !until.IsZero() && until.Before(deadline) {
		deadline = until
	}
	timer := time.NewTimer(max(deadline.Sub(w.now()), 0))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-w.wake:
	case <-timer.C:
	}
}

// result is how attending one message ended.
type result int

const (
	attended result = iota
	// noConnection: the send found no connection; the message waits as it
	// was, without counting the attempt.
	noConnection
	// stateFailed: the state database failed; the message waits as it was.
	stateFailed
)

// pass attends every message that is due, in order, and returns when to
// look again: when the next attempt is due, after a pause if something
// failed, or zero to wait for Wake. It stops early when ctx ends or the
// connection drops. After attending at least one message, and with nothing
// else due, it truncates the WAL so the erased content leaves the disk.
func (w *worker) pass(ctx context.Context) time.Time {
	db := context.WithoutCancel(ctx)
	attendedAny := false
	for ctx.Err() == nil {
		if !w.connected.Load() {
			return time.Time{}
		}
		job, ok, err := w.store.Next(db, w.now())
		if err != nil {
			w.log.Error("state", "outcome", "state_error", "err", err)
			return w.now().Add(pause)
		}
		if !ok {
			break
		}
		attendedAny = true
		if r := w.attend(ctx, job); r != attended {
			return w.now().Add(pause)
		}
	}
	if ctx.Err() != nil {
		return time.Time{}
	}
	if attendedAny {
		if err := w.store.Checkpoint(db); err != nil {
			w.log.Error("state", "outcome", "state_error", "err", err)
		}
	}
	at, ok, err := w.store.NextAttempt(db)
	if err != nil {
		w.log.Error("state", "outcome", "state_error", "err", err)
		return w.now().Add(pause)
	}
	if !ok {
		return time.Time{}
	}
	return at
}

// attend takes one message through to its outcome, or to its next attempt.
// The writes to the state database, and the send itself, do not stop when
// ctx ends: a message is never left halfway.
func (w *worker) attend(ctx context.Context, job state.Job) result {
	db := context.WithoutCancel(ctx)
	base := []any{"id", job.MessageID, "chat", job.Chat}

	saved, err := decodeSaved(job.Content)
	if err != nil {
		return w.finish(db, job, "failed", base, []any{"err", err})
	}
	if saved.Media != nil {
		base = append(base, "media", string(saved.Media.Kind))
	}
	how := saved.how()

	if age := w.now().Sub(job.Sent); age > maxAge {
		return w.finish(db, job, "expired", base, how, []any{"age", age.Round(time.Second).String()})
	}

	built, ok, err := w.prepare(db, &job, saved)
	var unusable unusableError
	if errors.As(err, &unusable) {
		return w.finish(db, job, "failed", base, how, []any{"err", err})
	}
	if err != nil {
		w.log.Error("decision", concat([]any{"outcome", "state_error"}, base, []any{"err", err})...)
		return stateFailed
	}
	if !ok {
		return w.finish(db, job, "no_match", base, how)
	}

	out := &waE2E.Message{}
	if err := proto.Unmarshal(built.Message, out); err != nil {
		return w.finish(db, job, "failed", base, how, []any{"err", err})
	}
	chat, err := types.ParseJID(job.Chat)
	if err != nil {
		return w.finish(db, job, "failed", base, how, []any{"err", err})
	}

	sendCtx, cancel := context.WithTimeout(db, sendTimeout)
	resp, err := w.client.SendMessage(sendCtx, chat, out, whatsmeow.SendRequestExtra{ID: job.ReplyID})
	cancel()
	attempt := job.Attempts + 1

	if err == nil {
		attrs := []any{"reply_id", resp.ID, "attempt", attempt, "waited", w.now().Sub(job.Received).Round(time.Millisecond).String()}
		if built.Outcome == "replied" {
			attrs = append([]any{"keyword", built.Keyword, "default_reply", built.Default}, attrs...)
		}
		return w.finish(db, job, built.Outcome, base, how, attrs)
	}

	failure := []any{"reply_id", job.ReplyID, "attempt", attempt, "err", err}
	switch classify(err) {
	case notConnected:
		w.log.Warn("decision", concat([]any{"outcome", "send_deferred"}, base, []any{"reply_id", job.ReplyID, "err", err})...)
		return noConnection
	case permanent:
		return w.finish(db, job, "failed", base, how, failure)
	}
	if attempt > len(retryWaits) {
		return w.finish(db, job, "failed", base, how, failure)
	}
	next := w.now().Add(retryWaits[attempt-1])
	if err := w.store.Retry(db, job.Seq, attempt, next); err != nil {
		w.log.Error("decision", concat([]any{"outcome", "state_error"}, base, []any{"err", err})...)
		return stateFailed
	}
	w.log.Warn("decision", concat([]any{"outcome", "retry"}, base, failure, []any{"next_attempt", next.Format(time.RFC3339)})...)
	return attended
}

// unusableError is a stored message or reply that cannot be decoded or
// built. Trying again would fail the same way, so the message closes as
// failed.
type unusableError struct{ err error }

func (e unusableError) Error() string { return e.err.Error() }
func (e unusableError) Unwrap() error { return e.err }

// prepare returns the reply to send for job, and false if there is nothing
// to answer. The first time, it asks the responder, builds the reply, fixes
// its id and stores both before anything is sent. Later attempts, also after
// a restart, get the stored ones. Any error other than an unusableError
// comes from the state database.
func (w *worker) prepare(db context.Context, job *state.Job, saved savedMessage) (builtReply, bool, error) {
	if job.Sending() {
		built, err := decodeBuilt(job.Reply)
		if err != nil {
			return builtReply{}, false, unusableError{err}
		}
		return built, true, nil
	}

	decision := w.responder.Answer(saved.message())
	if decision.Outcome == responder.NoMatch {
		return builtReply{}, false, nil
	}
	out, err := saved.reply(job.MessageID, decision.Reply)
	if err != nil {
		return builtReply{}, false, unusableError{err}
	}
	body, err := proto.Marshal(out)
	if err != nil {
		return builtReply{}, false, unusableError{err}
	}
	built := builtReply{Message: body, Outcome: "replied", Keyword: decision.Keyword, Default: decision.Default}
	if decision.Outcome == responder.FileAck {
		built = builtReply{Message: body, Outcome: "file_ack"}
	}
	enc, err := encode(built)
	if err != nil {
		return builtReply{}, false, unusableError{err}
	}
	id := w.client.GenerateMessageID()
	if err := w.store.MarkSending(db, job.Seq, id, enc); err != nil {
		return builtReply{}, false, err
	}
	job.ReplyID = id
	return built, true, nil
}

// finish closes job with outcome, erasing its content, and logs the
// decision.
func (w *worker) finish(db context.Context, job state.Job, outcome string, attrs ...[]any) result {
	if err := w.store.Finish(db, job.Seq, outcome, w.now()); err != nil {
		w.log.Error("decision", "outcome", "state_error", "id", job.MessageID, "chat", job.Chat, "err", err)
		return stateFailed
	}
	line := concat(append([][]any{{"outcome", outcome}}, attrs...)...)
	switch outcome {
	case "failed":
		w.log.Warn("decision", line...)
	default:
		w.log.Info("decision", line...)
	}
	return attended
}

// prune deletes the records of messages closed more than keepRecords ago.
func (w *worker) prune(ctx context.Context) {
	n, err := w.store.Prune(context.WithoutCancel(ctx), w.now().Add(-keepRecords))
	if err != nil {
		w.log.Error("state", "outcome", "state_error", "err", err)
		return
	}
	if n > 0 {
		w.log.Info("state", "outcome", "pruned", "records", n)
	}
}

// failure is the kind of error a send returned.
type failure int

const (
	// transient: may work on a later attempt, or may even have arrived
	// (the server's confirmation did not come in time).
	transient failure = iota
	// notConnected: there was no connection to send through.
	notConnected
	// permanent: retrying will not help.
	permanent
)

func classify(err error) failure {
	var disconnected *whatsmeow.DisconnectedError
	switch {
	case errors.Is(err, whatsmeow.ErrNotConnected),
		errors.Is(err, whatsmeow.ErrNotLoggedIn),
		errors.As(err, &disconnected):
		return notConnected
	case errors.Is(err, whatsmeow.ErrRecipientNotOnWhatsApp),
		errors.Is(err, whatsmeow.ErrNotInGroup),
		errors.Is(err, whatsmeow.ErrGroupNotFound),
		errors.Is(err, whatsmeow.ErrNoPermissionInGroup),
		errors.Is(err, whatsmeow.ErrUnknownServer),
		errors.Is(err, whatsmeow.ErrRecipientADJID),
		errors.Is(err, whatsmeow.ErrBroadcastListUnsupported):
		return permanent
	}
	return transient
}
