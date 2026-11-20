// Package responder decides what the bot answers, in two steps. Admit says
// whether the bot attends a message at all; Answer says what it replies to a
// message Admit accepted. It knows nothing about whatsmeow or the
// connection: it receives a message already reduced to the fields that
// matter.
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
	// Chat is the chat JID. For groups it is compared with the enabled list.
	Chat string
	// FromMe is true for messages sent by the bot's own number.
	FromMe bool
	// SenderPhone is the sender's phone number, digits only, or "" if it
	// could not be resolved. Used in private chats.
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
	// sender not on the list, empty private text, group message without a
	// tag, other kinds of messages). Not logged as a decision.
	Ignored Admission = iota
	// Admitted: the bot attends the message. Answer decides what to reply.
	Admitted
	// NotEnabled: the bot was tagged in a group that is not on the enabled
	// list. Silence.
	NotEnabled
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

// Admit reports whether the bot attends m. In private chats it attends
// people on the allowed list, for texts and files; in groups, any member who
// tags the bot, if the group is on the enabled list.
func (r *Responder) Admit(m Message) Admission {
	if m.FromMe || m.IsEdit {
		return Ignored
	}
	if m.Media == nil && !m.IsText {
		return Ignored
	}
	switch {
	case m.Private:
		// A text needs something to compare with the keywords. A file is
		// confirmed whatever its caption says.
		if m.Media == nil && m.Text == "" {
			return Ignored
		}
		if !r.cfg.IsAllowedPerson(m.SenderPhone) {
			return Ignored
		}
		return Admitted
	case m.Group:
		if !m.Mentioned {
			return Ignored
		}
		if !r.cfg.IsEnabledGroup(m.Chat) {
			return NotEnabled
		}
		return Admitted
	default:
		return Ignored
	}
}

// Answer decides what to reply to a message Admit admitted: the keyword's
// reply if the whole text matches one; in groups, otherwise the default
// reply; for an image or document, its confirmation. A file's caption is not
// matched against the keywords.
func (r *Responder) Answer(m Message) Decision {
	if m.Media != nil {
		return r.answerFile(*m.Media)
	}
	if keyword, reply, ok := r.cfg.Reply(m.Text); ok {
		return Decision{Outcome: Reply, Keyword: keyword, Reply: reply}
	}
	// The default reply is only for group tags.
	if m.Group {
		if reply := r.cfg.DefaultReply(); reply != "" {
			return Decision{Outcome: Reply, Default: true, Reply: reply}
		}
	}
	return Decision{Outcome: NoMatch}
}

// WithDelayNotice returns d for a reply that goes out late: the configured
// delay notice first, a blank line, and then the reply, all in one message.
// With no notice configured, or nothing to reply, d is returned as it is.
func (r *Responder) WithDelayNotice(d Decision) Decision {
	notice := r.cfg.DelayNotice()
	if notice == "" || d.Outcome == NoMatch {
		return d
	}
	d.Reply = notice + "\n\n" + d.Reply
	d.DelayNotice = true
	return d
}

// answerFile confirms an image or document with its template, filled with
// the file's metadata.
func (r *Responder) answerFile(media Media) Decision {
	tmpl := r.cfg.DocumentAck()
	if media.Kind == Image {
		tmpl = r.cfg.ImageAck()
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

// formatSize writes a byte count the way WhatsApp shows file sizes.
func formatSize(n uint64) string {
	const kb, mb, gb = 1024, 1024 * 1024, 1024 * 1024 * 1024
	switch {
	case n == 0:
		return "?"
	case n < kb:
		return fmt.Sprintf("%d B", n)
	case n < mb:
		return fmt.Sprintf("%d KB", (n+kb/2)/kb)
	case n < gb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	}
}
