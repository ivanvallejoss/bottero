package session

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/ivanvallejoss/bottero/internal/config"
	"github.com/ivanvallejoss/bottero/internal/dedup"
	"github.com/ivanvallejoss/bottero/internal/eventlog"
	"github.com/ivanvallejoss/bottero/internal/responder"
)

// sendTimeout bounds sending one reply, so a dead connection does not hold
// whatsmeow's message queue for long.
const sendTimeout = 30 * time.Second

// replier connects the responder to the WhatsApp client: it reduces incoming
// messages to what the responder needs, records answered messages and sends
// the replies. What to answer is decided by the responder alone.
type replier struct {
	ctx       context.Context
	client    *whatsmeow.Client
	cfg       *config.Config
	responder *responder.Responder
	answered  *dedup.Store
	log       *slog.Logger
}

func newReplier(ctx context.Context, client *whatsmeow.Client, cfg *config.Config, answered *dedup.Store, logger *slog.Logger) *replier {
	return &replier{
		ctx:       ctx,
		client:    client,
		cfg:       cfg,
		responder: responder.New(cfg),
		answered:  answered,
		log:       logger.With("component", "responder"),
	}
}

// handle is registered with AddEventHandlerWithSuccessStatus. Returning false
// tells whatsmeow the message was not handled: it does not confirm it to the
// server and keeps the decrypted copy, so the server delivers it again on the
// next connection and the reply is retried.
func (r *replier) handle(rawEvt any) bool {
	evt, ok := rawEvt.(*events.Message)
	if !ok {
		return true
	}
	info := evt.Info
	device := r.client.Store
	text := eventlog.Text(evt.Message)

	msg := responder.Message{
		Private: isPrivate(info.Chat),
		Group:   info.Chat.Server == types.GroupServer,
		Chat:    info.Chat.String(),
		FromMe:  info.IsFromMe,
		IsText:  evt.Message.GetConversation() != "" || evt.Message.GetExtendedTextMessage() != nil,
		Text:    text,
		IsEdit:  evt.IsEdit || info.Edit != "",
	}

	// Attributes that say how the sender or the tag was recognized.
	var how []any
	if msg.Private {
		phone, via := senderPhone(r.ctx, device.LIDs, info)
		msg.SenderPhone = phone
		how = []any{"matched_via", via}
	}
	if msg.Group {
		mentionedAs := selfMention(device, evt.Message)
		msg.Mentioned = mentionedAs != ""
		msg.Text = stripSelfMentions(text, device)
		how = []any{"mentioned_as", mentionedAs}
	}

	decision := r.responder.Decide(msg)
	base := []any{"id", info.ID, "chat", info.Chat.String()}

	switch decision.Outcome {
	case responder.Ignore:
		return true
	case responder.NoMatch:
		r.log.Info("decision", concat([]any{"outcome", "no_match"}, base, how)...)
		return true
	case responder.GroupNotEnabled:
		r.log.Info("decision", concat([]any{"outcome", "group_not_enabled"}, base, how)...)
		return true
	}

	// Claim before sending: if the message is already recorded, it was
	// answered before and this is a repeated delivery.
	claimed, err := r.answered.Claim(r.ctx, info.ID, info.Chat.String(), info.Sender.String())
	if err != nil {
		r.log.Error("decision", concat([]any{"outcome", "dedup_error"}, base, []any{"err", err})...)
		return false
	}
	if !claimed {
		r.log.Info("decision", concat([]any{"outcome", "duplicate"}, base)...)
		return true
	}

	// In groups the reply quotes the message that tagged the bot, so it is
	// clear whom it answers. In private chats it is plain text.
	var out *waE2E.Message
	if msg.Group {
		out = quotedReply(decision.Reply, info, text)
	} else {
		reply := decision.Reply
		out = &waE2E.Message{Conversation: &reply}
	}

	sendCtx, cancel := context.WithTimeout(r.ctx, sendTimeout)
	defer cancel()
	resp, err := r.client.SendMessage(sendCtx, info.Chat, out)
	if err != nil {
		// Release the claim so the next delivery can try again. The run
		// context may already be cancelled (shutdown), so don't use it here.
		if relErr := r.answered.Release(context.WithoutCancel(r.ctx), info.ID); relErr != nil {
			r.log.Error("decision", concat([]any{"outcome", "release_error"}, base, []any{"err", relErr})...)
		}
		r.log.Warn("decision", concat([]any{"outcome", "send_failed"}, base, []any{"err", err})...)
		return false
	}

	r.log.Info("decision", concat(
		[]any{"outcome", "replied"}, base, how,
		[]any{"keyword", decision.Keyword, "default_reply", decision.Default, "reply_id", resp.ID},
	)...)
	return true
}

// includeText is the event log's content filter: private messages from
// people on the allowed list, and every message in an enabled group, are
// logged with their text.
func (r *replier) includeText(evt *events.Message) bool {
	info := evt.Info
	if info.Chat.Server == types.GroupServer {
		return r.cfg.IsEnabledGroup(info.Chat.String())
	}
	if info.IsFromMe || !isPrivate(info.Chat) {
		return false
	}
	phone, _ := senderPhone(r.ctx, r.client.Store.LIDs, info)
	return r.cfg.IsAllowedPerson(phone)
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
// text message: "lid", "pn", or "" if it is not mentioned. Only text
// messages count; a tag in an image or file caption is handled separately.
func selfMention(device *store.Device, msg *waE2E.Message) string {
	for _, raw := range msg.GetExtendedTextMessage().GetContextInfo().GetMentionedJID() {
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
		text = strings.ReplaceAll(text, "@"+device.LID.User, "")
	}
	if device.ID != nil {
		text = strings.ReplaceAll(text, "@"+device.ID.User, "")
	}
	return text
}

// quotedReply builds a text reply that quotes the message it answers.
func quotedReply(reply string, info types.MessageInfo, quotedText string) *waE2E.Message {
	id := info.ID
	author := info.Sender.ToNonAD().String()
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: &reply,
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      &id,
				Participant:   &author,
				QuotedMessage: &waE2E.Message{Conversation: &quotedText},
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
