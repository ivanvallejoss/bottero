// Package eventlog writes one readable line per whatsmeow event.
//
// The log is the main output of the exploration stage. It records what
// whatsmeow delivers, with identifiers exactly as they arrive (phone-number
// JID or LID). It logs metadata for every message; the text is added only
// for the messages the includeText filter accepts.
package eventlog

import (
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Logger turns whatsmeow events into log lines.
type Logger struct {
	log    *slog.Logger
	device *store.Device

	// includeText decides whether a message line also carries the text.
	// nil means never.
	includeText func(*events.Message) bool

	// offlineSync is true between OfflineSyncPreview and OfflineSyncCompleted,
	// while the server delivers the events missed during downtime. whatsmeow
	// handles incoming nodes in order, so messages in between are the
	// catch-up ones.
	offlineSync atomic.Bool
}

// New returns a Logger that writes through base. device is the bot's own
// device store, used to log the bot's identifiers on every connection.
// includeText decides which messages are logged with their text; nil means
// metadata only.
func New(base *slog.Logger, device *store.Device, includeText func(*events.Message) bool) *Logger {
	return &Logger{log: base.With("component", "event"), device: device, includeText: includeText}
}

// Handle logs a single event. Its signature matches whatsmeow.EventHandler,
// so it can be passed directly to Client.AddEventHandler.
//
// The line's own time attribute is when the bot received the event.
func (l *Logger) Handle(rawEvt any) {
	name := eventName(rawEvt)

	switch evt := rawEvt.(type) {
	case *events.Message:
		l.message(name, evt)

	case *events.UndecryptableMessage:
		l.undecryptable(name, evt)

	case *events.Receipt:
		l.log.Info("event", "type", name,
			"scope", scope(evt.Chat),
			"chat", jid(evt.Chat),
			"sender", jid(evt.Sender),
			"sender_alt", jid(evt.SenderAlt),
			"receipt_type", receiptType(evt.Type),
			"message_count", len(evt.MessageIDs),
			"msg_time", evt.Timestamp,
		)

	case *events.Connected:
		l.offlineSync.Store(false)
		l.log.Info("event", "type", name,
			"self_pn", jidPtr(l.device.ID),
			"self_lid", jid(l.device.LID),
		)

	case *events.Disconnected:
		l.offlineSync.Store(false)
		l.log.Info("event", "type", name)

	case *events.OfflineSyncPreview:
		l.offlineSync.Store(true)
		l.log.Info("event", "type", name,
			"total", evt.Total,
			"messages", evt.Messages,
			"notifications", evt.Notifications,
			"receipts", evt.Receipts,
			"app_data_changes", evt.AppDataChanges,
		)

	case *events.OfflineSyncCompleted:
		l.offlineSync.Store(false)
		l.log.Info("event", "type", name, "count", evt.Count)

	case *events.HistorySync:
		l.log.Info("event", "type", name,
			"sync_type", evt.Data.GetSyncType().String(),
			"conversations", len(evt.Data.GetConversations()),
			"chunk", evt.Data.GetChunkOrder(),
			"progress", evt.Data.GetProgress(),
		)

	case *events.KeepAliveTimeout:
		l.log.Warn("event", "type", name,
			"error_count", evt.ErrorCount,
			"last_success", evt.LastSuccess,
		)

	case *events.JoinedGroup:
		l.log.Info("event", "type", name,
			"chat", jid(evt.JID),
			"addressing", string(evt.AddressingMode),
			"sender", jidPtr(evt.Sender),
			"sender_pn", jidPtr(evt.SenderPN),
			"reason", evt.Reason,
			"join_type", evt.Type,
		)

	case *events.GroupInfo:
		l.log.Info("event", "type", name,
			"chat", jid(evt.JID),
			"sender", jidPtr(evt.Sender),
			"sender_pn", jidPtr(evt.SenderPN),
		)

	// LoggedOut, StreamReplaced, ClientOutdated, TemporaryBan, ConnectFailure
	// and CATRefreshError: whatsmeow will not reconnect after these.
	case events.PermanentDisconnect:
		l.log.Warn("event", "type", name, "reason", evt.PermanentDisconnectDescription())

	default:
		l.log.Info("event", "type", name)
	}
}

func (l *Logger) message(name string, evt *events.Message) {
	info := evt.Info
	attrs := append(l.infoAttrs(name, info), "kind", kind(evt.Message))
	if info.Edit != "" {
		attrs = append(attrs, "edit", string(info.Edit))
	}
	if mentioned := contextInfo(evt.Message).GetMentionedJID(); len(mentioned) > 0 {
		attrs = append(attrs, "mentions", strings.Join(mentioned, ","))
	}
	if l.includeText != nil && l.includeText(evt) {
		if text := Text(evt.Message); text != "" {
			attrs = append(attrs, "text", text)
		}
	}
	l.log.Info("event", attrs...)
}

// Text returns the text of a plain or extended text message, or "".
func Text(msg *waE2E.Message) string {
	if text := msg.GetConversation(); text != "" {
		return text
	}
	return msg.GetExtendedTextMessage().GetText()
}

// undecryptable logs a message whatsmeow received but could not decrypt.
// whatsmeow asks the sender to retry, so the same id may show up again
// later as a regular Message.
func (l *Logger) undecryptable(name string, evt *events.UndecryptableMessage) {
	attrs := append(l.infoAttrs(name, evt.Info),
		"is_unavailable", evt.IsUnavailable,
		"decrypt_fail_mode", string(evt.DecryptFailMode),
	)
	l.log.Warn("event", attrs...)
}

// infoAttrs returns the metadata shared by every event that carries a
// MessageInfo, with identifiers exactly as whatsmeow delivers them.
func (l *Logger) infoAttrs(name string, info types.MessageInfo) []any {
	return []any{
		"type", name,
		"scope", scope(info.Chat),
		"chat", jid(info.Chat),
		"sender", jid(info.Sender),
		"sender_alt", jid(info.SenderAlt),
		"recipient_alt", jid(info.RecipientAlt),
		"addressing", string(info.AddressingMode),
		"from_me", info.IsFromMe,
		"id", info.ID,
		"msg_time", info.Timestamp,
		"offline_sync", l.offlineSync.Load(),
		"wa_type", info.Type,
		"media_type", info.MediaType,
	}
}

// eventName turns *events.Message into "Message".
func eventName(evt any) string {
	name := fmt.Sprintf("%T", evt)
	name = strings.TrimPrefix(name, "*")
	return strings.TrimPrefix(name, "events.")
}

// scope classifies a chat by its JID server.
func scope(chat types.JID) string {
	switch chat.Server {
	case types.DefaultUserServer, types.HiddenUserServer:
		return "private"
	case types.GroupServer:
		return "group"
	case types.BroadcastServer:
		if chat.User == types.StatusBroadcastJID.User {
			return "status"
		}
		return "broadcast"
	case types.NewsletterServer:
		return "newsletter"
	default:
		return "other"
	}
}

// kind names the part of the message proto that carries the content.
// "text" is a plain conversation message; "extended_text" is text with
// context (mentions, replies, link previews).
func kind(msg *waE2E.Message) string {
	switch {
	case msg == nil:
		return "none"
	case msg.GetConversation() != "":
		return "text"
	case msg.GetExtendedTextMessage() != nil:
		return "extended_text"
	case msg.GetImageMessage() != nil:
		return "image"
	case msg.GetPtvMessage() != nil:
		return "video_note"
	case msg.GetVideoMessage() != nil:
		return "video"
	case msg.GetAudioMessage() != nil:
		return "audio"
	case msg.GetDocumentMessage() != nil:
		return "document"
	case msg.GetStickerMessage() != nil:
		return "sticker"
	case msg.GetReactionMessage() != nil, msg.GetEncReactionMessage() != nil:
		return "reaction"
	case msg.GetProtocolMessage() != nil:
		return "protocol"
	case msg.GetContactMessage() != nil:
		return "contact"
	case msg.GetLocationMessage() != nil, msg.GetLiveLocationMessage() != nil:
		return "location"
	case msg.GetPollCreationMessage() != nil, msg.GetPollCreationMessageV3() != nil:
		return "poll"
	case msg.GetPollUpdateMessage() != nil:
		return "poll_vote"
	case msg.GetSenderKeyDistributionMessage() != nil:
		return "sender_key"
	default:
		return "other"
	}
}

// contextInfo returns the context info (mentions, quoted message) of the
// message types that carry one, or nil.
func contextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	switch {
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetContextInfo()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo()
	case msg.GetPtvMessage() != nil:
		return msg.GetPtvMessage().GetContextInfo()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetContextInfo()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetContextInfo()
	default:
		return nil
	}
}

func receiptType(t types.ReceiptType) string {
	if t == types.ReceiptTypeDelivered {
		return "delivered"
	}
	return string(t)
}

func jid(j types.JID) string {
	if j.IsEmpty() {
		return ""
	}
	return j.String()
}

func jidPtr(j *types.JID) string {
	if j == nil {
		return ""
	}
	return jid(*j)
}
