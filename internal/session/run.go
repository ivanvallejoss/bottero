package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/ivanvallejoss/bottero/internal/config"
	"github.com/ivanvallejoss/bottero/internal/eventlog"
	"github.com/ivanvallejoss/bottero/internal/responder"
	"github.com/ivanvallejoss/bottero/internal/state"
)

// ErrNotPaired is returned by Run when the store holds no session.
var ErrNotPaired = errors.New("no session found: run 'bottero pair' first")

// ErrSessionEnded is returned by Run when WhatsApp ends the session in a way
// whatsmeow does not recover from: logged out from the phone, temporary ban,
// another process using the same session, outdated client. Restarting the
// process does not fix any of these.
var ErrSessionEnded = errors.New("session ended and will not reconnect")

// Run connects with the stored session, logs every event and answers
// messages until ctx is cancelled or the session ends for good.
//
// The inbox receives each message inside whatsmeow's event handlers and saves
// the ones the bot attends; the worker answers them in its own goroutine.
// When Run stops, it does so in this order: the worker takes no new message
// and finishes the send in progress; the client disconnects; the inbox
// waits for a handler in progress and refuses later deliveries, which the
// server sends again on the next connection. The caller closes the databases
// afterwards.
func Run(ctx context.Context, container *sqlstore.Container, waLogger waLog.Logger, logger *slog.Logger, cfg *config.Config, st *state.Store) error {
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("load device: %w", err)
	}
	if device.ID == nil {
		return ErrNotPaired
	}

	client := whatsmeow.NewClient(device, waLogger.Sub("Client"))

	// whatsmeow reconnects on its own after a disconnect, waiting 2 more
	// seconds after each failed attempt. InitialAutoReconnect extends that to
	// the first connection, so starting without network (the Wi-Fi coming
	// back after a power cut) keeps retrying instead of exiting.
	client.InitialAutoReconnect = true

	// Keep each decrypted message until the handlers finish with it. If the
	// server delivers it again before that (the bot crashed before saving it,
	// or the inbox reported failure), whatsmeow hands over the same message
	// again instead of the encryption layer discarding it as already used.
	client.EnableDecryptedEventBuffer = true

	resp := responder.New(cfg)
	work := newWorker(st, resp, client, logger)
	in := newInbox(device, cfg, resp, st, logger, work.Wake)

	// Handlers run in order for every event.
	client.AddEventHandler(eventlog.New(logger, device, in.includeText).Handle)

	// Events that implement PermanentDisconnect are the ones after which
	// whatsmeow does not reconnect.
	stopped := make(chan string, 1)
	client.AddEventHandler(func(evt any) {
		switch e := evt.(type) {
		case *events.Connected:
			work.SetConnected(true)
		case *events.Disconnected:
			work.SetConnected(false)
		case events.PermanentDisconnect:
			work.SetConnected(false)
			select {
			case stopped <- e.PermanentDisconnectDescription():
			default:
			}
		}
	})

	// Registered last: when it reports failure, whatsmeow skips the handlers
	// after it and does not confirm the message to the server.
	client.AddEventHandlerWithSuccessStatus(in.handle)

	workCtx, stopWork := context.WithCancel(ctx)
	workDone := make(chan struct{})
	go func() {
		defer close(workDone)
		work.run(workCtx)
	}()
	shutdown := func() {
		stopWork()
		<-workDone
		client.Disconnect()
		in.close()
	}

	if err := client.Connect(); err != nil {
		shutdown()
		return fmt.Errorf("connect: %w", err)
	}

	var result error
	select {
	case <-ctx.Done():
		logger.Info("shutting down", "component", "bottero")
	case reason := <-stopped:
		result = fmt.Errorf("%w: %s", ErrSessionEnded, reason)
	}
	shutdown()
	return result
}
