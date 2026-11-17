package session

import (
	"context"
	"log/slog"
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
	phone, via := senderPhone(r.ctx, r.client.Store.LIDs, info)

	decision := r.responder.Decide(responder.Message{
		Private:     isPrivate(info.Chat),
		FromMe:      info.IsFromMe,
		SenderPhone: phone,
		Text:        eventlog.Text(evt.Message),
		IsEdit:      evt.IsEdit || info.Edit != "",
	})

	switch decision.Outcome {
	case responder.Ignore:
		return true
	case responder.NoMatch:
		r.log.Info("decision", "outcome", "no_match", "id", info.ID, "chat", info.Chat.String(), "matched_via", via)
		return true
	}

	// Claim before sending: if the message is already recorded, it was
	// answered before and this is a repeated delivery.
	claimed, err := r.answered.Claim(r.ctx, info.ID, info.Chat.String(), info.Sender.String())
	if err != nil {
		r.log.Error("decision", "outcome", "dedup_error", "id", info.ID, "err", err)
		return false
	}
	if !claimed {
		r.log.Info("decision", "outcome", "duplicate", "id", info.ID, "chat", info.Chat.String())
		return true
	}

	sendCtx, cancel := context.WithTimeout(r.ctx, sendTimeout)
	defer cancel()
	text := decision.Reply
	resp, err := r.client.SendMessage(sendCtx, info.Chat, &waE2E.Message{Conversation: &text})
	if err != nil {
		// Release the claim so the next delivery can try again. The run
		// context may already be cancelled (shutdown), so don't use it here.
		if relErr := r.answered.Release(context.WithoutCancel(r.ctx), info.ID); relErr != nil {
			r.log.Error("decision", "outcome", "release_error", "id", info.ID, "err", relErr)
		}
		r.log.Warn("decision", "outcome", "send_failed", "id", info.ID, "chat", info.Chat.String(), "keyword", decision.Keyword, "err", err)
		return false
	}

	r.log.Info("decision", "outcome", "replied",
		"id", info.ID,
		"chat", info.Chat.String(),
		"keyword", decision.Keyword,
		"matched_via", via,
		"reply_id", resp.ID,
	)
	return true
}

// includeText is the event log's content filter: only private messages
// from people on the allowed list are logged with their text.
func (r *replier) includeText(evt *events.Message) bool {
	if evt.Info.IsFromMe || !isPrivate(evt.Info.Chat) {
		return false
	}
	phone, _ := senderPhone(r.ctx, r.client.Store.LIDs, evt.Info)
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

// isPrivate reports whether chat is a one-to-one chat.
func isPrivate(chat types.JID) bool {
	return chat.Server == types.DefaultUserServer || chat.Server == types.HiddenUserServer
}
