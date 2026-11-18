package eventlog

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// The bot's identities as whatsmeow stores them: both carry the device number
// of the linked device.
const (
	botPhone = "5493624000009"
	botLID   = "123456789012345"
)

func botDevice() *store.Device {
	pn := types.JID{User: botPhone, Device: 2, Server: types.DefaultUserServer}
	return &store.Device{
		ID:  &pn,
		LID: types.JID{User: botLID, Device: 2, Server: types.HiddenUserServer},
	}
}

func ptr[T any](v T) *T { return &v }

func TestSelfIdentity(t *testing.T) {
	device := botDevice()
	tests := []struct {
		name string
		jid  types.JID
		want string
	}{
		{"LID without device", types.NewJID(botLID, types.HiddenUserServer), "lid"},
		{"LID with the bot's device", device.LID, "lid"},
		{"LID with another device", types.JID{User: botLID, Device: 7, Server: types.HiddenUserServer}, "lid"},
		{"phone number without device", types.NewJID(botPhone, types.DefaultUserServer), "pn"},
		{"phone number with another device", types.JID{User: botPhone, Device: 7, Server: types.DefaultUserServer}, "pn"},
		{"someone else's LID", types.NewJID("987654321098765", types.HiddenUserServer), ""},
		{"someone else's number", types.NewJID("5493624000001", types.DefaultUserServer), ""},
		{"LID digits on the phone-number server", types.NewJID(botLID, types.DefaultUserServer), ""},
		{"phone digits on the LID server", types.NewJID(botPhone, types.HiddenUserServer), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SelfIdentity(device, tt.jid); got != tt.want {
				t.Errorf("SelfIdentity(%s) = %q, want %q", tt.jid, got, tt.want)
			}
		})
	}
}

func TestSelfIdentityWithoutIdentities(t *testing.T) {
	// Before pairing the device has neither identity.
	device := &store.Device{}
	for _, j := range []types.JID{
		types.NewJID(botLID, types.HiddenUserServer),
		types.NewJID(botPhone, types.DefaultUserServer),
	} {
		if got := SelfIdentity(device, j); got != "" {
			t.Errorf("SelfIdentity(%s) = %q on a device without identities", j, got)
		}
	}
}

func TestText(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"plain text", &waE2E.Message{Conversation: ptr("hola")}, "hola"},
		{"extended text", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ptr("@" + botLID + " ping")}}, "@" + botLID + " ping"},
		// A caption is not the message text.
		{"image with caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: ptr("ping")}}, ""},
		{"nil message", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.msg); got != tt.want {
				t.Errorf("Text() = %q, want %q", got, tt.want)
			}
		})
	}
}
