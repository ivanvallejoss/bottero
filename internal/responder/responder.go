// Package responder decides what the bot answers. It knows nothing about
// whatsmeow or the connection: it receives a message already reduced to the
// fields that matter and returns a decision. The fixed replies can later be
// replaced (for example by a language model) without touching the session.
package responder

import "github.com/ivanvallejoss/bottero/internal/config"

// Message is an incoming message reduced to what the decision needs.
type Message struct {
	// Private is true for a one-to-one chat.
	Private bool
	// FromMe is true for messages sent by the bot's own number.
	FromMe bool
	// SenderPhone is the sender's phone number, digits only, or "" if it
	// could not be resolved.
	SenderPhone string
	// Text is the message text, or "" if the message carries no text.
	Text string
	// IsEdit is true for an edit of an earlier message.
	IsEdit bool
}

// Outcome is the kind of decision.
type Outcome int

const (
	// Ignore: not something the bot answers (group, own message, sender not
	// on the list, no text, edit). Not logged as a decision.
	Ignore Outcome = iota
	// NoMatch: a message the bot would answer, but no keyword matches.
	NoMatch
	// Reply: answer with Decision.Reply.
	Reply
)

// Decision is the result of Decide.
type Decision struct {
	Outcome Outcome
	// Keyword is the normalized keyword that matched (Reply only).
	Keyword string
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

// Eligible reports whether the bot answers this kind of message at all:
// private chat, not sent by the bot's own number, sender on the allowed
// list, plain text, not an edit.
func (r *Responder) Eligible(m Message) bool {
	return m.Private &&
		!m.FromMe &&
		r.cfg.IsAllowedPerson(m.SenderPhone) &&
		m.Text != "" &&
		!m.IsEdit
}

// Decide returns what to do with m. A message is answered only if it is
// eligible and its whole text matches a keyword.
func (r *Responder) Decide(m Message) Decision {
	if !r.Eligible(m) {
		return Decision{Outcome: Ignore}
	}
	keyword, reply, ok := r.cfg.Reply(m.Text)
	if !ok {
		return Decision{Outcome: NoMatch}
	}
	return Decision{Outcome: Reply, Keyword: keyword, Reply: reply}
}
