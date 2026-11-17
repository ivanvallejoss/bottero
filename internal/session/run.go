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

	"github.com/ivanvallejoss/bottero/internal/eventlog"
)

// ErrNotPaired is returned by Run when the store holds no session.
var ErrNotPaired = errors.New("no session found: run 'bottero pair' first")

// ErrSessionEnded is returned by Run when WhatsApp ends the session in a way
// whatsmeow does not recover from: logged out from the phone, temporary ban,
// another process using the same session, outdated client. Restarting the
// process does not fix any of these.
var ErrSessionEnded = errors.New("session ended and will not reconnect")

// Run connects with the stored session and logs every event until ctx is
// cancelled or the session ends for good.
func Run(ctx context.Context, container *sqlstore.Container, waLogger waLog.Logger, logger *slog.Logger) error {
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

	client.AddEventHandler(eventlog.New(logger, device).Handle)

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
