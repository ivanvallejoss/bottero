package session

import (
	"encoding/json"
	"fmt"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/ivanvallejoss/bottero/internal/eventlog"
	"github.com/ivanvallejoss/bottero/internal/responder"
)

// savedMessage is what the worker needs to answer a message the inbox
// saved: what Answer reads, what a group reply quotes, and the attributes of
// the decision log. It is stored as JSON in state.db while the message is
// open, and erased when it closes. The sender's phone number and the rest of
// the message are not kept: Admit already used them to choose the route.
type savedMessage struct {
	// Route is the route Admit chose. The worker looks it up by name in the
	// configuration it has when it takes the message.
	Route string           `json:"route,omitempty"`
	Group bool             `json:"group,omitempty"`
	Text  string           `json:"text,omitempty"`
	Media *responder.Media `json:"media,omitempty"`
	// Recognized is the pair of decision-log attributes that says how the
	// sender (matched_via, private chats) or the tag (mentioned_as, groups)
	// was recognized.
	Recognized []string `json:"recognized,omitempty"`
	// Author and Quote are, in groups, the author of the message and the
	// part of it the reply quotes (a waE2E.Message in protobuf).
	Author string `json:"author,omitempty"`
	Quote  []byte `json:"quote,omitempty"`
}

// newSavedMessage keeps from an admitted message what the worker needs.
func newSavedMessage(evt *events.Message, msg responder.Message, how []any) (savedMessage, error) {
	s := savedMessage{Route: msg.Route, Group: msg.Group, Text: msg.Text, Media: msg.Media}
	for _, v := range how {
		s.Recognized = append(s.Recognized, fmt.Sprint(v))
	}
	if msg.Group {
		quote, err := proto.Marshal(quotable(evt.Message, eventlog.Text(evt.Message)))
		if err != nil {
			return savedMessage{}, fmt.Errorf("encode quote: %w", err)
		}
		s.Author = evt.Info.Sender.ToNonAD().String()
		s.Quote = quote
	}
	return s, nil
}

// message is what Answer receives: the same fields reduce gave it when the
// message arrived.
func (s savedMessage) message() responder.Message {
	return responder.Message{Private: !s.Group, Group: s.Group, Text: s.Text, Media: s.Media, Route: s.Route}
}

// how returns the attributes of the decision log that say how the sender or
// the tag was recognized.
func (s savedMessage) how() []any {
	out := make([]any, len(s.Recognized))
	for i, v := range s.Recognized {
		out[i] = v
	}
	return out
}

// reply builds the message that answers this one with text: in groups it
// quotes the message, so it is clear whom it answers; in private chats it is
// plain text.
func (s savedMessage) reply(messageID, text string) (*waE2E.Message, error) {
	if !s.Group {
		return &waE2E.Message{Conversation: &text}, nil
	}
	quoted := &waE2E.Message{}
	if err := proto.Unmarshal(s.Quote, quoted); err != nil {
		return nil, fmt.Errorf("decode quote: %w", err)
	}
	return quotedReply(text, messageID, s.Author, quoted), nil
}

// builtReply is a reply as the worker built it at the first attempt. It is
// stored as JSON with the message while it is sending, so every attempt
// sends this same reply with the same id, even after a restart or a change
// in the configuration.
type builtReply struct {
	// Message is the waE2E.Message to send, in protobuf.
	Message []byte `json:"message"`
	// Outcome is the decision-log outcome once it is sent: replied or
	// file_ack. Keyword and Default go with replied.
	Outcome string `json:"outcome"`
	Keyword string `json:"keyword,omitempty"`
	Default bool   `json:"default,omitempty"`
	// DelayNotice is true when the reply starts with the delay notice.
	DelayNotice bool `json:"delay_notice,omitempty"`
}

func encode(v any) ([]byte, error) { return json.Marshal(v) }

func decodeSaved(data []byte) (savedMessage, error) {
	var s savedMessage
	if err := json.Unmarshal(data, &s); err != nil {
		return savedMessage{}, fmt.Errorf("decode saved message: %w", err)
	}
	return s, nil
}

func decodeBuilt(data []byte) (builtReply, error) {
	var b builtReply
	if err := json.Unmarshal(data, &b); err != nil {
		return builtReply{}, fmt.Errorf("decode built reply: %w", err)
	}
	return b, nil
}
