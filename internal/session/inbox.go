package session

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/ivanvallejoss/bottero/internal/config"
	"github.com/ivanvallejoss/bottero/internal/responder"
	"github.com/ivanvallejoss/bottero/internal/state"
)

// saver is the part of the state database the inbox uses.
type saver interface {
	Save(ctx context.Context, in state.Incoming) (bool, error)
}

// inbox is the event handler that receives messages. It decides only
// whether the bot attends a message (the responder's Admit) and, if it does,
// saves the message in the state database before telling whatsmeow it was
// handled. Answering is the worker's job, so a slow reply never holds
// whatsmeow's queue of incoming events.
type inbox struct {
	device    *store.Device
	cfg       *config.Config
	responder *responder.Responder
	store     saver
	log       *slog.Logger
	now       func() time.Time
	// onSaved is called after each new message is saved, to wake the worker.
	onSaved func()

	mu     sync.RWMutex
	closed bool
}

func newInbox(device *store.Device, cfg *config.Config, resp *responder.Responder, st saver, logger *slog.Logger, onSaved func()) *inbox {
	return &inbox{
		device:    device,
		cfg:       cfg,
		responder: resp,
		store:     st,
		log:       logger.With("component", "responder"),
		now:       time.Now,
		onSaved:   onSaved,
	}
}

// handle is registered with AddEventHandlerWithSuccessStatus. Returning
// false tells whatsmeow the message was not handled: it does not confirm it
// to the server and keeps the decrypted copy, so the server delivers it
// again on the next connection. That happens only when the message could not
// be saved: the state database failed, the handler panicked, or the bot is
// shutting down.
func (in *inbox) handle(rawEvt any) (handled bool) {
	evt, ok := rawEvt.(*events.Message)
	if !ok {
		return true
	}

	// close waits for a handler in progress; after it, deliveries are refused
	// and come back on the next connection.
	in.mu.RLock()
	defer in.mu.RUnlock()
	if in.closed {
		return false
	}

	info := evt.Info
	base := []any{"id", info.ID, "chat", info.Chat.String()}
	var media []any

	// whatsmeow counts a panic in a handler as success and confirms the
	// message, which would then be lost. As a failure, it comes back on the
	// next connection.
	defer func() {
		if p := recover(); p != nil {
			in.log.Error("decision", concat([]any{"outcome", "handler_panic"}, base,
				[]any{"panic", fmt.Sprint(p), "stack", string(debug.Stack())})...)
			handled = false
		}
	}()

	// Not the run context: a message that arrives while the bot shuts down
	// is still saved, and answered at the next start.
	ctx := context.Background()

	msg, how := reduce(ctx, in.device, evt)
	if msg.Media != nil {
		media = []any{"media", string(msg.Media.Kind)}
	}

	admission, route := in.responder.Admit(msg)
	switch admission {
	case responder.Ignored:
		return true
	case responder.NoRoute:
		in.log.Info("decision", concat([]any{"outcome", "no_route"}, base, media, how)...)
		return true
	}
	msg.Route = route
	base = concat(base, []any{"route", route}, media)

	saved, err := newSavedMessage(evt, msg, how)
	if err != nil {
		in.log.Error("decision", concat([]any{"outcome", "save_error"}, base, []any{"err", err})...)
		return false
	}
	content, err := encode(saved)
	if err != nil {
		in.log.Error("decision", concat([]any{"outcome", "save_error"}, base, []any{"err", err})...)
		return false
	}

	isNew, err := in.store.Save(ctx, state.Incoming{
		Chat:      info.Chat.String(),
		MessageID: info.ID,
		Sent:      info.Timestamp,
		Received:  in.now(),
		Content:   content,
	})
	if err != nil {
		in.log.Error("decision", concat([]any{"outcome", "save_error"}, base, []any{"err", err})...)
		return false
	}
	if !isNew {
		in.log.Info("decision", concat([]any{"outcome", "duplicate"}, base)...)
		return true
	}
	in.log.Info("decision", concat([]any{"outcome", "queued"}, base, how)...)
	in.onSaved()
	return true
}

// close waits for a handler in progress to finish and makes the later ones
// refuse their message.
func (in *inbox) close() {
	in.mu.Lock()
	in.closed = true
	in.mu.Unlock()
}

// includeText is the event log's content filter: private messages from
// people who are in a route, and every message in a group that is in a
// route, are logged with their text. It looks up the route the same way
// Admit does.
func (in *inbox) includeText(evt *events.Message) bool {
	info := evt.Info
	if info.Chat.Server == types.GroupServer {
		_, ok := in.cfg.GroupRoute(info.Chat.String())
		return ok
	}
	if info.IsFromMe || !isPrivate(info.Chat) {
		return false
	}
	phone, _ := senderPhone(context.Background(), in.device.LIDs, info)
	_, ok := in.cfg.PersonRoute(phone)
	return ok
}
