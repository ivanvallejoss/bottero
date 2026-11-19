package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/ivanvallejoss/bottero/internal/config"
	"github.com/ivanvallejoss/bottero/internal/responder"
)

// The bot's identities as whatsmeow stores them (with the linked device's
// number), and the people and chats the tests use.
const (
	botPhone      = "5493624000009"
	botLID        = "123456789012345"
	allowedPhone  = "5493624000001"
	allowedLID    = "111111111111111"
	otherPhone    = "5493624000002"
	otherLID      = "987654321098765"
	enabledGroup  = "120363000000000001@g.us"
	disabledGroup = "120363000000000002@g.us"
)

func ptr[T any](v T) *T { return &v }

func pnJID(user string) types.JID  { return types.NewJID(user, types.DefaultUserServer) }
func lidJID(user string) types.JID { return types.NewJID(user, types.HiddenUserServer) }

func botDevice() *store.Device {
	pn := types.JID{User: botPhone, Device: 2, Server: types.DefaultUserServer}
	return &store.Device{
		ID:  &pn,
		LID: types.JID{User: botLID, Device: 2, Server: types.HiddenUserServer},
	}
}

// fakeLIDs answers GetPNForLID from a map. Any other LIDStore method panics
// through the nil embedded interface.
type fakeLIDs struct {
	store.LIDStore
	pn  map[types.JID]types.JID
	err error
}

func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	if f.err != nil {
		return types.EmptyJID, f.err
	}
	return f.pn[lid], nil
}

func TestIsPrivate(t *testing.T) {
	tests := []struct {
		chat types.JID
		want bool
	}{
		{pnJID(allowedPhone), true},
		{lidJID(allowedLID), true},
		{types.NewJID("120363000000000001", types.GroupServer), false},
		{types.StatusBroadcastJID, false},
		{types.NewJID("123", types.BroadcastServer), false},
		{types.NewJID("120363000000000003", types.NewsletterServer), false},
	}
	for _, tt := range tests {
		if got := isPrivate(tt.chat); got != tt.want {
			t.Errorf("isPrivate(%s) = %v, want %v", tt.chat, got, tt.want)
		}
	}
}

func TestSenderPhone(t *testing.T) {
	ctx := context.Background()
	lids := fakeLIDs{pn: map[types.JID]types.JID{lidJID(allowedLID): pnJID(allowedPhone)}}
	source := func(sender, alt types.JID) types.MessageInfo {
		return types.MessageInfo{MessageSource: types.MessageSource{Chat: sender.ToNonAD(), Sender: sender, SenderAlt: alt}}
	}

	tests := []struct {
		name      string
		lids      store.LIDStore
		info      types.MessageInfo
		wantPhone string
		wantVia   string
	}{
		{"sender is a phone number", lids, source(pnJID(allowedPhone), types.EmptyJID), allowedPhone, "sender"},
		{"sender is a phone number with a device", lids,
			source(types.JID{User: allowedPhone, Device: 3, Server: types.DefaultUserServer}, types.EmptyJID), allowedPhone, "sender"},
		{"sender is a LID with the number in sender_alt", lids, source(lidJID(otherLID), pnJID(otherPhone)), otherPhone, "sender_alt"},
		{"sender is a LID found in the LID map", lids, source(lidJID(allowedLID), types.EmptyJID), allowedPhone, "lid_map"},
		// The LID map is keyed without the device number.
		{"sender is a LID with a device found in the LID map", lids,
			source(types.JID{User: allowedLID, Device: 4, Server: types.HiddenUserServer}, types.EmptyJID), allowedPhone, "lid_map"},
		{"sender is a LID not in the LID map", lids, source(lidJID(otherLID), types.EmptyJID), "", ""},
		{"LID map error", fakeLIDs{err: errors.New("db closed")}, source(lidJID(allowedLID), types.EmptyJID), "", ""},
		{"no LID store", nil, source(lidJID(allowedLID), types.EmptyJID), "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phone, via := senderPhone(ctx, tt.lids, tt.info)
			if phone != tt.wantPhone || via != tt.wantVia {
				t.Errorf("senderPhone() = %q, %q; want %q, %q", phone, via, tt.wantPhone, tt.wantVia)
			}
		})
	}
}

func mentioning(jids ...string) *waE2E.ContextInfo {
	return &waE2E.ContextInfo{MentionedJID: jids}
}

func TestSelfMention(t *testing.T) {
	device := botDevice()
	botByLID := lidJID(botLID).String()
	botByPhone := pnJID(botPhone).String()
	someone := lidJID(otherLID).String()

	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"text mentioning the bot's LID",
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ptr("@" + botLID + " ping"), ContextInfo: mentioning(botByLID)}}, "lid"},
		{"text mentioning the bot's number",
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ptr("@" + botPhone + " ping"), ContextInfo: mentioning(botByPhone)}}, "pn"},
		{"text mentioning someone else",
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ptr("@" + otherLID), ContextInfo: mentioning(someone)}}, ""},
		{"text mentioning someone else and the bot",
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{ContextInfo: mentioning(someone, botByLID)}}, "lid"},
		{"invalid JID before the bot's mention",
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{ContextInfo: mentioning("1.x@lid", botByLID)}}, "lid"},
		{"extended text without context", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ptr("@" + botLID)}}, ""},
		{"plain text with the bot's LID written", &waE2E.Message{Conversation: ptr("@" + botLID + " ping")}, ""},
		{"image caption mentioning the bot", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: ptr("@" + botLID), ContextInfo: mentioning(botByLID)}}, "lid"},
		{"document caption mentioning the bot", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: ptr("@" + botPhone), ContextInfo: mentioning(botByPhone)}}, "pn"},
		// Only text, images and documents are checked for a tag today.
		{"video caption mentioning the bot", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: ptr("@" + botLID), ContextInfo: mentioning(botByLID)}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selfMention(device, tt.msg); got != tt.want {
				t.Errorf("selfMention() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripSelfMentions(t *testing.T) {
	device := botDevice()
	tests := []struct {
		name, text, want string
	}{
		{"LID tag", "@" + botLID + " ping", " ping"},
		{"number tag", "@" + botPhone + " ping", " ping"},
		{"repeated tags in both forms", "@" + botLID + " ping @" + botPhone, " ping "},
		{"other mentions stay", "@" + otherLID + " ping @" + botLID, "@" + otherLID + " ping "},
		{"tag at the end", "ping @" + botLID, "ping "},
		{"tag followed by punctuation", "@" + botLID + ", ping", ", ping"},
		{"LID that starts with the bot's digits stays", "@" + botLID + "7 ping", "@" + botLID + "7 ping"},
		{"number that starts with the bot's digits stays", "@" + botPhone + "1 ping", "@" + botPhone + "1 ping"},
		{"look-alike and the bot's tag", "@" + botLID + "7 ping @" + botLID, "@" + botLID + "7 ping "},
		{"no tag", "ping", "ping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripSelfMentions(tt.text, device); got != tt.want {
				t.Errorf("stripSelfMentions(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}

	// Without identities there is nothing to remove.
	text := "@" + botLID + " ping"
	if got := stripSelfMentions(text, &store.Device{}); got != text {
		t.Errorf("stripSelfMentions on a device without identities = %q", got)
	}
}

func TestReduce(t *testing.T) {
	ctx := context.Background()
	device := botDevice()
	device.LIDs = fakeLIDs{pn: map[types.JID]types.JID{lidJID(allowedLID): pnJID(allowedPhone)}}
	group, _ := types.ParseJID(enabledGroup)
	botByLID := lidJID(botLID).String()
	botByPhone := pnJID(botPhone).String()

	private := func(sender types.JID, msg *waE2E.Message) *events.Message {
		return &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: sender.ToNonAD(), Sender: sender}},
			Message: msg,
		}
	}
	inGroup := func(msg *waE2E.Message) *events.Message {
		return &events.Message{
			Info: types.MessageInfo{MessageSource: types.MessageSource{
				Chat: group, Sender: lidJID(otherLID), SenderAlt: pnJID(otherPhone), IsGroup: true,
			}},
			Message: msg,
		}
	}
	change := func(evt *events.Message, f func(*events.Message)) *events.Message {
		f(evt)
		return evt
	}
	ping := func() *waE2E.Message { return &waE2E.Message{Conversation: ptr("ping")} }
	tagByLID := func() *waE2E.Message {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: ptr("@" + botLID + " ping"), ContextInfo: mentioning(botByLID),
		}}
	}
	allowedChat := lidJID(allowedLID).String()

	tests := []struct {
		name    string
		evt     *events.Message
		want    responder.Message
		wantHow []any
	}{
		{"private text, sender found in the LID map",
			private(lidJID(allowedLID), ping()),
			responder.Message{Private: true, Chat: allowedChat, SenderPhone: allowedPhone, IsText: true, Text: "ping"},
			[]any{"matched_via", "lid_map"}},
		{"private text, sender as a phone number",
			private(pnJID(allowedPhone), ping()),
			responder.Message{Private: true, Chat: pnJID(allowedPhone).String(), SenderPhone: allowedPhone, IsText: true, Text: "ping"},
			[]any{"matched_via", "sender"}},
		{"private text, sender not resolved",
			private(lidJID(otherLID), ping()),
			responder.Message{Private: true, Chat: lidJID(otherLID).String(), IsText: true, Text: "ping"},
			[]any{"matched_via", ""}},
		// Only groups look for the bot's tag and remove it from the text.
		{"private text with the bot's tag",
			private(lidJID(allowedLID), tagByLID()),
			responder.Message{Private: true, Chat: allowedChat, SenderPhone: allowedPhone, IsText: true, Text: "@" + botLID + " ping"},
			[]any{"matched_via", "lid_map"}},
		{"private document",
			private(pnJID(allowedPhone), &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				Mimetype: ptr("application/pdf"), FileLength: ptr[uint64](4096), FileName: ptr("informe.pdf"), PageCount: ptr[uint32](4),
			}}),
			responder.Message{Private: true, Chat: pnJID(allowedPhone).String(), SenderPhone: allowedPhone, Media: &responder.Media{
				Kind: responder.Document, Mimetype: "application/pdf", Size: 4096, Name: "informe.pdf", Pages: 4,
			}},
			[]any{"matched_via", "sender"}},
		{"own private message",
			change(private(lidJID(allowedLID), ping()), func(e *events.Message) {
				e.Info.Sender = types.JID{User: botLID, Device: 2, Server: types.HiddenUserServer}
				e.Info.IsFromMe = true
			}),
			responder.Message{Private: true, Chat: allowedChat, FromMe: true, IsText: true, Text: "ping"},
			[]any{"matched_via", ""}},
		{"edit flagged by the event",
			change(private(lidJID(allowedLID), ping()), func(e *events.Message) { e.IsEdit = true }),
			responder.Message{Private: true, Chat: allowedChat, SenderPhone: allowedPhone, IsText: true, Text: "ping", IsEdit: true},
			[]any{"matched_via", "lid_map"}},
		{"edit flagged in the message info",
			change(private(lidJID(allowedLID), ping()), func(e *events.Message) { e.Info.Edit = types.EditAttributeMessageEdit }),
			responder.Message{Private: true, Chat: allowedChat, SenderPhone: allowedPhone, IsText: true, Text: "ping", IsEdit: true},
			[]any{"matched_via", "lid_map"}},
		{"group text tagging the bot's LID",
			inGroup(tagByLID()),
			responder.Message{Group: true, Chat: enabledGroup, IsText: true, Text: " ping", Mentioned: true},
			[]any{"mentioned_as", "lid"}},
		{"group text without a tag",
			inGroup(ping()),
			responder.Message{Group: true, Chat: enabledGroup, IsText: true, Text: "ping"},
			[]any{"mentioned_as", ""}},
		// The caption is not part of the text the keywords are compared with.
		{"group image tagging the bot's number in the caption",
			inGroup(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Caption: ptr("@" + botPhone + " mirá"), ContextInfo: mentioning(botByPhone),
				Mimetype: ptr("image/jpeg"), FileLength: ptr[uint64](2048), Width: ptr[uint32](800), Height: ptr[uint32](600),
			}}),
			responder.Message{Group: true, Chat: enabledGroup, Mentioned: true, Media: &responder.Media{
				Kind: responder.Image, Mimetype: "image/jpeg", Size: 2048, Width: 800, Height: 600,
			}},
			[]any{"mentioned_as", "pn"}},
		{"status broadcast",
			&events.Message{
				Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID, Sender: pnJID(allowedPhone)}},
				Message: ping(),
			},
			responder.Message{Chat: types.StatusBroadcastJID.String(), IsText: true, Text: "ping"},
			nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, how := reduce(ctx, device, tt.evt)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("reduce() message = %+v, want %+v", got, tt.want)
			}
			if !reflect.DeepEqual(how, tt.wantHow) {
				t.Errorf("reduce() attributes = %v, want %v", how, tt.wantHow)
			}
		})
	}
}

func TestMediaOf(t *testing.T) {
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Mimetype: ptr("image/jpeg"), FileLength: ptr[uint64](2048), Width: ptr[uint32](800), Height: ptr[uint32](600),
		Caption: ptr("pie"),
	}}
	if got, want := mediaOf(img), (responder.Media{Kind: responder.Image, Mimetype: "image/jpeg", Size: 2048, Width: 800, Height: 600}); got == nil || *got != want {
		t.Errorf("mediaOf(image) = %+v, want %+v", got, want)
	}

	doc := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Mimetype: ptr("application/pdf"), FileLength: ptr[uint64](4096), FileName: ptr("informe.pdf"), PageCount: ptr[uint32](4),
	}}
	if got, want := mediaOf(doc), (responder.Media{Kind: responder.Document, Mimetype: "application/pdf", Size: 4096, Name: "informe.pdf", Pages: 4}); got == nil || *got != want {
		t.Errorf("mediaOf(document) = %+v, want %+v", got, want)
	}

	for name, msg := range map[string]*waE2E.Message{
		"text":    {Conversation: ptr("ping")},
		"video":   {VideoMessage: &waE2E.VideoMessage{Mimetype: ptr("video/mp4")}},
		"audio":   {AudioMessage: &waE2E.AudioMessage{Mimetype: ptr("audio/ogg; codecs=opus")}},
		"sticker": {StickerMessage: &waE2E.StickerMessage{Mimetype: ptr("image/webp")}},
	} {
		if got := mediaOf(msg); got != nil {
			t.Errorf("mediaOf(%s) = %+v, want nil", name, got)
		}
	}
}

func TestQuotedReply(t *testing.T) {
	info := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:   types.NewJID("120363000000000001", types.GroupServer),
			Sender: types.JID{User: otherLID, Device: 3, Server: types.HiddenUserServer},
		},
		ID: "3A0000000000000000AA",
	}
	img := &waE2E.ImageMessage{Mimetype: ptr("image/jpeg")}
	doc := &waE2E.DocumentMessage{FileName: ptr("informe.pdf")}

	tests := []struct {
		name  string
		msg   *waE2E.Message
		text  string
		check func(*waE2E.Message) bool
	}{
		{"image", &waE2E.Message{ImageMessage: img}, "", func(q *waE2E.Message) bool { return q.GetImageMessage() == img && q.GetDocumentMessage() == nil }},
		{"document", &waE2E.Message{DocumentMessage: doc}, "", func(q *waE2E.Message) bool { return q.GetDocumentMessage() == doc && q.GetImageMessage() == nil }},
		{"text", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ptr("@" + botLID + " ping")}}, "@" + botLID + " ping",
			func(q *waE2E.Message) bool { return q.GetConversation() == "@"+botLID+" ping" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := quotedReply("pong", info, quotable(tt.msg, tt.text))
			ext := out.GetExtendedTextMessage()
			if ext.GetText() != "pong" {
				t.Errorf("reply text = %q, want %q", ext.GetText(), "pong")
			}
			ci := ext.GetContextInfo()
			if ci.GetStanzaID() != info.ID {
				t.Errorf("quoted id = %q, want %q", ci.GetStanzaID(), info.ID)
			}
			// The author goes without the device number.
			if want := lidJID(otherLID).String(); ci.GetParticipant() != want {
				t.Errorf("quoted author = %q, want %q", ci.GetParticipant(), want)
			}
			if !tt.check(ci.GetQuotedMessage()) {
				t.Errorf("quoted message = %v", ci.GetQuotedMessage())
			}
		})
	}
}

func TestIncludeText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"allowed_people": ["` + allowedPhone + `"], "enabled_groups": ["` + enabledGroup + `"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	device := botDevice()
	device.LIDs = fakeLIDs{pn: map[types.JID]types.JID{lidJID(allowedLID): pnJID(allowedPhone)}}
	// A client that never connects: includeText only reads its store.
	r := &replier{ctx: context.Background(), cfg: cfg, client: &whatsmeow.Client{Store: device}}

	group := func(chat string, fromMe bool) *events.Message {
		jid, _ := types.ParseJID(chat)
		return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{
			Chat: jid, Sender: lidJID(otherLID), SenderAlt: pnJID(otherPhone), IsGroup: true, IsFromMe: fromMe,
		}}}
	}
	private := func(sender, alt types.JID, fromMe bool) *events.Message {
		return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{
			Chat: sender, Sender: sender, SenderAlt: alt, IsFromMe: fromMe,
		}}}
	}

	tests := []struct {
		name string
		evt  *events.Message
		want bool
	}{
		{"enabled group, sender not on the list", group(enabledGroup, false), true},
		{"enabled group, own message", group(enabledGroup, true), true},
		{"group not enabled", group(disabledGroup, false), false},
		{"private, allowed by sender_alt", private(lidJID(otherLID), pnJID(allowedPhone), false), true},
		{"private, allowed by the LID map", private(lidJID(allowedLID), types.EmptyJID, false), true},
		{"private, not on the list", private(lidJID(otherLID), pnJID(otherPhone), false), false},
		// Own messages never carry text in private, whatever the sender resolves to.
		{"private, own message", private(lidJID(allowedLID), pnJID(allowedPhone), true), false},
		{"status broadcast from an allowed number", &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{
			Chat: types.StatusBroadcastJID, Sender: pnJID(allowedPhone),
		}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.includeText(tt.evt); got != tt.want {
				t.Errorf("includeText() = %v, want %v", got, tt.want)
			}
		})
	}
}
