// Package responder decides what the bot answers, in two steps. Admit says
// whether the bot attends a message at all and, if it does, which route of
// the configuration it belongs to; Answer says what it replies to a message
// Admit accepted, with the rules of that route. It knows nothing about
// whatsmeow or the connection: it receives a message already reduced to the
// fields that matter.
//
// Admit only reads the configuration and is instant, so the session can run
// it as soon as a message arrives. Answer is the part that can later be
// replaced (for example by a language model) without touching the session.
package responder

import (
	"fmt"
	"strings"

	"github.com/ivanvallejoss/bottero/internal/config"
)

// Message is an incoming message reduced to what the decision needs.
type Message struct {
	// Private is true for a one-to-one chat; Group for a group chat.
	Private bool
	Group   bool
	// Chat is the chat JID. For groups it chooses the route.
	Chat string
	// FromMe is true for messages sent by the bot's own number.
	FromMe bool
	// SenderPhone is the sender's phone number, digits only, or "" if it
	// could not be resolved. In private chats it chooses the route.
	SenderPhone string
	// IsText is true for a plain or extended text message.
	IsText bool
	// Text is the message text. In groups, the bot's own mentions are
	// already removed from it.
	Text string
	// Media is set for an image or a document.
	Media *Media
	// IsEdit is true for an edit of an earlier message.
	IsEdit bool
	// Mentioned is true when the message (or the file's caption) explicitly
	// mentions the bot.
	Mentioned bool
	// Route is the route Admit chose. Answer replies with its rules.
	Route string
}

// MediaKind is the kind of file the bot confirms.
type MediaKind string

const (
	Image    MediaKind = "image"
	Document MediaKind = "document"
)

// Media is an image or document described by the metadata the message
// carries. The bot never downloads the file. Zero values mean the message
// did not carry that field.
type Media struct {
	Kind     MediaKind
	Mimetype string
	Size     uint64
	Width    uint32
	Height   uint32
	Name     string
	Pages    uint32
}

// Admission is the result of Admit.
type Admission int

const (
	// Ignored: not something the bot considers (own message, edit, private
	// sender in no route, empty private text, group message without a tag,
	// other kinds of messages). Not logged as a decision.
	Ignored Admission = iota
	// Admitted: the bot attends the message. Answer decides what to reply.
	Admitted
	// NoRoute: the bot was tagged in a group that is in no route. Silence.
	NoRoute
)

// Outcome is the kind of answer.
type Outcome int

const (
	// NoMatch: there is nothing to answer with (no keyword matches and no
	// default reply, or an empty confirmation template). Silence.
	NoMatch Outcome = iota
	// Reply: answer a text message with Decision.Reply.
	Reply
	// FileAck: confirm an image or document with Decision.Reply.
	FileAck
	// RouteMissing: the message's route is not in the configuration (it was
	// removed or renamed after the message was saved). Silence.
	RouteMissing
)

// Decision is the result of Answer.
type Decision struct {
	Outcome Outcome
	// Keyword is the normalized keyword that matched (Reply only; "" when
	// Default).
	Keyword string
	// Default is true when Reply is the configured default reply.
	Default bool
	// DelayNotice is true when Reply starts with the delay notice.
	DelayNotice bool
	// Reply is the text to send (Reply and FileAck).
	Reply string
}

// Responder answers with the fixed replies from the configuration.
type Responder struct {
	cfg *config.Config
}

// New returns a Responder that uses cfg.
func New(cfg *config.Config) *Responder {
	return &Responder{cfg: cfg}
}

// Admit reports whether the bot attends m and, if it does, the route it
// belongs to. In private chats it attends the people who are in a route, for
// texts and files; in groups, any member who tags the bot, if the group is in
// a route. The route of a private chat is chosen by the person's number; that
// of a group, by the group alone.
func (r *Responder) Admit(m Message) (Admission, string) {
	if m.FromMe || m.IsEdit {
		return Ignored, ""
	}
	if m.Media == nil && !m.IsText {
		return Ignored, ""
	}
	switch {
	case m.Private:
		// A text needs something to compare with the keywords. A file is
		// confirmed whatever its caption says.
		if m.Media == nil && m.Text == "" {
			return Ignored, ""
		}
		route, ok := r.cfg.PersonRoute(m.SenderPhone)
		if !ok {
			return Ignored, ""
		}
		return Admitted, route
	case m.Group:
		if !m.Mentioned {
			return Ignored, ""
		}
		route, ok := r.cfg.GroupRoute(m.Chat)
		if !ok {
			return NoRoute, ""
		}
		return Admitted, route
	default:
		return Ignored, ""
	}
}

// Answer decides what to reply to a message Admit admitted, with the rules
// of its route: the keyword's reply if the whole text matches one, otherwise
// the default reply; for an image or document, its confirmation. A file's
// caption is not matched against the keywords.
func (r *Responder) Answer(m Message) Decision {
	rules, ok := r.cfg.Rules(m.Route)
	if !ok {
		return Decision{Outcome: RouteMissing}
	}
	if m.Media != nil {
		return answerFile(rules, *m.Media)
	}
	if keyword, reply, ok := rules.Reply(m.Text); ok {
		return Decision{Outcome: Reply, Keyword: keyword, Reply: reply}
	}
	if reply := rules.DefaultReply(); reply != "" {
		return Decision{Outcome: Reply, Default: true, Reply: reply}
	}
	return Decision{Outcome: NoMatch}
}

// WithDelayNotice returns d for a reply that goes out late: the delay notice
// of route first, a blank line, and then the reply, all in one message. With
// no notice in the route, or nothing to reply, d is returned as it is.
func (r *Responder) WithDelayNotice(route string, d Decision) Decision {
	if d.Outcome != Reply && d.Outcome != FileAck {
		return d
	}
	rules, ok := r.cfg.Rules(route)
	if !ok || rules.DelayNotice() == "" {
		return d
	}
	d.Reply = rules.DelayNotice() + "\n\n" + d.Reply
	d.DelayNotice = true
	return d
}

// answerFile confirms an image or document with its template, filled with
// the file's metadata.
func answerFile(rules *config.Rules, media Media) Decision {
	tmpl := rules.DocumentAck()
	if media.Kind == Image {
		tmpl = rules.ImageAck()
	}
	if tmpl == "" {
		return Decision{Outcome: NoMatch}
	}
	return Decision{Outcome: FileAck, Reply: render(tmpl, media)}
}

// render fills a confirmation template with the file's metadata. A value
// the message did not carry shows as "?". The configuration already
// rejected placeholders the template's kind does not have.
func render(tmpl string, m Media) string {
	return strings.NewReplacer(
		"{mimetype}", orUnknown(m.Mimetype),
		"{size}", formatSize(m.Size),
		"{width}", number(m.Width),
		"{height}", number(m.Height),
		"{name}", orUnknown(m.Name),
		"{pages}", number(m.Pages),
	).Replace(tmpl)
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func number(n uint32) string {
	if n == 0 {
		return "?"
	}
	return fmt.Sprint(n)
}

// formatSize writes a byte count the way WhatsApp shows file sizes. A size
// that would round up to 1024 of a unit is written in the next one: "1.0 MB",
// not "1024 KB".
func formatSize(n uint64) string {
	const kb, mb, gb = 1024, 1024 * 1024, 1024 * 1024 * 1024
	switch {
	case n == 0:
		return "?"
	case n < kb:
		return fmt.Sprintf("%d B", n)
	case n < mb-kb/2:
		return fmt.Sprintf("%d KB", (n+kb/2)/kb)
	case float64(n)/mb < 1023.95:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	}
}
