// Package responder decides what the bot answers. It knows nothing about
// whatsmeow or the connection: it receives a message already reduced to the
// fields that matter and returns a decision. The fixed replies can later be
// replaced (for example by a language model) without touching the session.
package responder

import "github.com/ivanvallejoss/bottero/internal/config"

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
	// IsEdit is true for an edit of an earlier message.
	IsEdit bool
	// Mentioned is true when the message explicitly mentions the bot.
	Mentioned bool
}

// Outcome is the kind of decision.
type Outcome int

const (
	// Ignore: not something the bot considers (own message, edit, no text,
	// private sender not on the list, group message without a tag). Not
	// logged as a decision.
	Ignore Outcome = iota
	// NoMatch: the bot would answer, but no keyword matches and there is no
	// reply to fall back on. Silence.
	NoMatch
	// Reply: answer with Decision.Reply.
	Reply
	// GroupNotEnabled: the bot was tagged in a group that is not on the
	// enabled list. Silence.
	GroupNotEnabled
)

// Decision is the result of Decide.
type Decision struct {
	Outcome Outcome
	// Keyword is the normalized keyword that matched, or "" when Default.
	Keyword string
	// Default is true when Reply is the configured default reply.
	Default bool
	// Reply is the text to send (Reply only).
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
	if m.FromMe || m.IsEdit || !m.IsText {
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
