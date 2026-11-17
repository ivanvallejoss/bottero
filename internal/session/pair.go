package session

import (
	"context"
	"errors"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// ErrAlreadyPaired is returned by Pair when the store already holds a session.
var ErrAlreadyPaired = errors.New("a session already exists")

// pairDisplayName is sent with the pairing-code request. WhatsApp validates it
// and only accepts the "Browser (OS)" form with common values.
const pairDisplayName = "Chrome (Linux)"

// Pair links the dedicated number as a companion device using a pairing code
// instead of a QR code. showCode receives the code to type on the phone.
//
// It returns as soon as the phone accepts the code. whatsmeow saves the
// session before reporting success, so nothing is lost by exiting here. The
// first real connection, which uploads the initial batch of encryption keys
// and can take a while on a slow disk, happens on the next 'bottero run'.
func Pair(ctx context.Context, container *sqlstore.Container, phone string, log waLog.Logger, showCode func(code string)) error {
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("load device: %w", err)
	}
	if device.ID != nil {
		return fmt.Errorf("%w (%s); to pair again, unlink it from the phone and delete the session database", ErrAlreadyPaired, device.ID)
	}

	client := whatsmeow.NewClient(device, log.Sub("Client"))

	// The QR channel has to be requested before connecting. Its first item
	// means the login socket is ready; the QR codes themselves are ignored.
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("get QR channel: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Disconnect()

	codeRequested := false
	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if codeRequested {
				continue
			}
			codeRequested = true
			code, err := client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, pairDisplayName)
			if err != nil {
				return fmt.Errorf("request pairing code: %w", err)
			}
			showCode(code)
		case whatsmeow.QRChannelSuccess.Event:
			return nil
		case whatsmeow.QRChannelTimeout.Event:
			return errors.New("pairing timed out before the code was entered")
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("pairing failed: %w", item.Error)
		case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
			return fmt.Errorf("WhatsApp asked for passkey verification (%s), which pairing from a terminal does not support", item.Event)
		default:
			return fmt.Errorf("pairing failed: %s", item.Event)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("pairing ended without success")
}
