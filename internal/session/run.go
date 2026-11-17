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

// Run connects with the stored session and logs every event until ctx is
// cancelled or WhatsApp ends the session in a way whatsmeow will not recover
// from (logged out, banned, replaced by another process, outdated client).
func Run(ctx context.Context, container *sqlstore.Container, waLogger waLog.Logger, logger *slog.Logger) error {
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("load device: %w", err)
	}
	if device.ID == nil {
		return ErrNotPaired
	}

	client := whatsmeow.NewClient(device, waLogger.Sub("Client"))
	client.AddEventHandler(eventlog.New(logger, device).Handle)

	// whatsmeow reconnects on its own after ordinary disconnects. Events that
	// implement PermanentDisconnect are the ones after which it does not.
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
		return fmt.Errorf("session ended and will not reconnect: %s", reason)
	}
}
