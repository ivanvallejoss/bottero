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
	"github.com/ivanvallejoss/bottero/internal/dedup"
	"github.com/ivanvallejoss/bottero/internal/eventlog"
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
func Run(ctx context.Context, container *sqlstore.Container, waLogger waLog.Logger, logger *slog.Logger, cfg *config.Config, answered *dedup.Store) error {
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
	// server delivers it again before that (the bot crashed, or a handler
	// reported failure), whatsmeow hands over the same message again instead
	// of the encryption layer discarding it as already used.
	client.EnableDecryptedEventBuffer = true

	reply := newReplier(ctx, client, cfg, answered, logger)

	// Handlers run in order for every event.
	client.AddEventHandler(eventlog.New(logger, device, reply.includeText).Handle)

	// Events that implement PermanentDisconnect are the ones after which
	// whatsmeow does not reconnect.
	stopped := make(chan string, 1)
	client.AddEventHandler(func(evt any) {
		if pd, ok := evt.(events.PermanentDisconnect); ok {
			select {
			case stopped <- pd.PermanentDisconnectDescription():
			default:
			}
		}
	})

	// Registered last: when it reports failure, whatsmeow skips the handlers
	// after it and does not confirm the message to the server.
	client.AddEventHandlerWithSuccessStatus(reply.handle)

	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Disconnect()

	select {
	case <-ctx.Done():
		logger.Info("shutting down", "component", "bottero")
		return nil
	case reason := <-stopped:
		return fmt.Errorf("%w: %s", ErrSessionEnded, reason)
	}
}
