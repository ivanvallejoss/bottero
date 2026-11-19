package session

import (
	"context"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/ivanvallejoss/bottero/internal/eventlog"
	"github.com/ivanvallejoss/bottero/internal/responder"
)

// reduce turns a whatsmeow message into what the responder needs. It also
// returns the attributes that say how the sender or the tag was recognized,
// for the decision log: matched_via in private chats, mentioned_as in
// groups.
func reduce(ctx context.Context, device *store.Device, evt *events.Message) (responder.Message, []any) {
	info := evt.Info
	text := eventlog.Text(evt.Message)

	msg := responder.Message{
		Private: isPrivate(info.Chat),
		Group:   info.Chat.Server == types.GroupServer,
		Chat:    info.Chat.String(),
		FromMe:  info.IsFromMe,
		IsText:  evt.Message.GetConversation() != "" || evt.Message.GetExtendedTextMessage() != nil,
		Text:    text,
		Media:   mediaOf(evt.Message),
		IsEdit:  evt.IsEdit || info.Edit != "",
	}

	var how []any
	if msg.Private {
		phone, via := senderPhone(ctx, device.LIDs, info)
		msg.SenderPhone = phone
		how = []any{"matched_via", via}
	}
	if msg.Group {
		mentionedAs := selfMention(device, evt.Message)
		msg.Mentioned = mentionedAs != ""
		msg.Text = stripSelfMentions(text, device)
		how = []any{"mentioned_as", mentionedAs}
	}
	return msg, how
}

// mediaOf describes an image or document by the metadata in the message,
// or returns nil for any other kind of message.
func mediaOf(msg *waE2E.Message) *responder.Media {
	if img := msg.GetImageMessage(); img != nil {
		return &responder.Media{
			Kind:     responder.Image,
			Mimetype: img.GetMimetype(),
			Size:     img.GetFileLength(),
			Width:    img.GetWidth(),
			Height:   img.GetHeight(),
		}
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return &responder.Media{
			Kind:     responder.Document,
			Mimetype: doc.GetMimetype(),
			Size:     doc.GetFileLength(),
			Name:     doc.GetFileName(),
			Pages:    doc.GetPageCount(),
		}
	}
	return nil
}

// quotable is the part of a message a reply quotes: the image or document
// itself (so the quote shows it), or the text.
func quotable(msg *waE2E.Message, text string) *waE2E.Message {
	if img := msg.GetImageMessage(); img != nil {
		return &waE2E.Message{ImageMessage: img}
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return &waE2E.Message{DocumentMessage: doc}
	}
	return &waE2E.Message{Conversation: &text}
}

// senderPhone returns the sender's phone number (digits only) and which
// identifier it came from: the sender itself when it arrives as a phone
// number, the alternate identifier otherwise, and whatsmeow's LID-to-number
// mapping as a last resort. It returns "", "" if none of them has it.
func senderPhone(ctx context.Context, lids store.LIDStore, info types.MessageInfo) (phone, via string) {
	if info.Sender.Server == types.DefaultUserServer {
		return info.Sender.User, "sender"
	}
	if info.SenderAlt.Server == types.DefaultUserServer {
		return info.SenderAlt.User, "sender_alt"
	}
	if info.Sender.Server == types.HiddenUserServer && lids != nil {
		pn, err := lids.GetPNForLID(ctx, info.Sender.ToNonAD())
		if err == nil && !pn.IsEmpty() {
			return pn.User, "lid_map"
		}
	}
	return "", ""
}

// selfMention returns how the bot appears among the explicit mentions of a
// text message or of an image or document caption: "lid", "pn", or "" if
// it is not mentioned.
func selfMention(device *store.Device, msg *waE2E.Message) string {
	var mentions []string
	switch {
	case msg.GetExtendedTextMessage() != nil:
		mentions = msg.GetExtendedTextMessage().GetContextInfo().GetMentionedJID()
	case msg.GetImageMessage() != nil:
		mentions = msg.GetImageMessage().GetContextInfo().GetMentionedJID()
	case msg.GetDocumentMessage() != nil:
		mentions = msg.GetDocumentMessage().GetContextInfo().GetMentionedJID()
	}
	for _, raw := range mentions {
		mentioned, err := types.ParseJID(raw)
		if err != nil {
			continue
		}
		if as := eventlog.SelfIdentity(device, mentioned); as != "" {
			return as
		}
	}
	return ""
}

// stripSelfMentions removes the bot's "@..." tags from a message text, in
// both forms, so the rest can be compared with the keywords.
func stripSelfMentions(text string, device *store.Device) string {
	if !device.LID.IsEmpty() {
		text = removeTag(text, "@"+device.LID.User)
	}
	if device.ID != nil {
		text = removeTag(text, "@"+device.ID.User)
	}
	return text
}

// removeTag removes every occurrence of tag that is not followed by a digit.
// When a digit follows, the text mentions someone else whose identifier
// starts with the same digits, and it stays as it is.
func removeTag(text, tag string) string {
	var b strings.Builder
	for {
		i := strings.Index(text, tag)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := i + len(tag)
		if end < len(text) && text[end] >= '0' && text[end] <= '9' {
			b.WriteString(text[:end])
		} else {
			b.WriteString(text[:i])
		}
		text = text[end:]
	}
}

// quotedReply builds a text reply that quotes the message it answers: its
// id, its author (without the device number) and the quoted part.
func quotedReply(reply, id, author string, quoted *waE2E.Message) *waE2E.Message {
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: &reply,
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      &id,
				Participant:   &author,
				QuotedMessage: quoted,
			},
		},
	}
}

// isPrivate reports whether chat is a one-to-one chat.
func isPrivate(chat types.JID) bool {
	return chat.Server == types.DefaultUserServer || chat.Server == types.HiddenUserServer
}

// concat joins slog attribute lists.
func concat(parts ...[]any) []any {
	var out []any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
