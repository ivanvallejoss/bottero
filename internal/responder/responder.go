// Package responder decides what the bot answers. It knows nothing about
// whatsmeow or the connection: it receives a message already reduced to the
// fields that matter and returns a decision. The fixed replies can later be
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

// Outcome is the kind of decision.
type Outcome int

const (
	// Ignore: not something the bot considers (own message, edit, private
	// sender not on the list, group message without a tag, other kinds of
	// messages). Not logged as a decision.
	Ignore Outcome = iota
	// NoMatch: the bot would answer, but there is nothing to answer with (no
	// keyword matches and no default reply, or an empty confirmation
	// template). Silence.
	NoMatch
	// Reply: answer a text message with Decision.Reply.
	Reply
	// FileAck: confirm an image or document with Decision.Reply.
	FileAck
	// GroupNotEnabled: the bot was tagged in a group that is not on the
	// enabled list. Silence.
	GroupNotEnabled
)

// Decision is the result of Decide.
type Decision struct {
	Outcome Outcome
	// Keyword is the normalized keyword that matched (Reply only; "" when
	// Default).
	Keyword string
	// Default is true when Reply is the configured default reply.
	Default bool
	// Media is the kind of file confirmed (FileAck, or NoMatch for a file).
	Media MediaKind
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

// Decide returns what to do with m.
func (r *Responder) Decide(m Message) Decision {
	if m.FromMe || m.IsEdit {
		return Decision{Outcome: Ignore}
	}
	if m.Media != nil {
		return r.decideFile(m)
	}
	if !m.IsText {
		return Decision{Outcome: Ignore}
	}
	switch {
	case m.Private:
		return r.decidePrivate(m)
	case m.Group:
		return r.decideGroup(m)
	default:
		return Decision{Outcome: Ignore}
	}
}

// decidePrivate answers people on the allowed list whose whole text matches
// a keyword.
func (r *Responder) decidePrivate(m Message) Decision {
	if m.Text == "" || !r.cfg.IsAllowedPerson(m.SenderPhone) {
		return Decision{Outcome: Ignore}
	}
	if keyword, reply, ok := r.cfg.Reply(m.Text); ok {
		return Decision{Outcome: Reply, Keyword: keyword, Reply: reply}
	}
	return Decision{Outcome: NoMatch}
}

// decideGroup answers any member who tags the bot in an enabled group: the
// keyword's reply if the rest of the text matches one, otherwise the
// default reply, or silence if there is none.
func (r *Responder) decideGroup(m Message) Decision {
	if !m.Mentioned {
		return Decision{Outcome: Ignore}
	}
	if !r.cfg.IsEnabledGroup(m.Chat) {
		return Decision{Outcome: GroupNotEnabled}
	}
	if keyword, reply, ok := r.cfg.Reply(m.Text); ok {
		return Decision{Outcome: Reply, Keyword: keyword, Reply: reply}
	}
	if reply := r.cfg.DefaultReply(); reply != "" {
		return Decision{Outcome: Reply, Default: true, Reply: reply}
	}
	return Decision{Outcome: NoMatch}
}

// decideFile confirms an image or document with the same rules as text:
// people on the allowed list in private chats, and a tag in the caption in
// enabled groups. The caption is not matched against the keywords.
func (r *Responder) decideFile(m Message) Decision {
	switch {
	case m.Private:
		if !r.cfg.IsAllowedPerson(m.SenderPhone) {
			return Decision{Outcome: Ignore}
		}
	case m.Group:
		if !m.Mentioned {
			return Decision{Outcome: Ignore}
		}
		if !r.cfg.IsEnabledGroup(m.Chat) {
			return Decision{Outcome: GroupNotEnabled, Media: m.Media.Kind}
		}
	default:
		return Decision{Outcome: Ignore}
	}

	tmpl := r.cfg.DocumentAck()
	if m.Media.Kind == Image {
		tmpl = r.cfg.ImageAck()
	}
	if tmpl == "" {
		return Decision{Outcome: NoMatch, Media: m.Media.Kind}
	}
	return Decision{Outcome: FileAck, Media: m.Media.Kind, Reply: render(tmpl, *m.Media)}
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
