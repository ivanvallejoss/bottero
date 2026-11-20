package responder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ivanvallejoss/bottero/internal/config"
)

// Identifiers used by the test configurations below.
const (
	allowedPhone  = "5493624000001"
	otherPhone    = "5493624000002"
	enabledGroup  = "120363000000000001@g.us"
	disabledGroup = "120363000000000002@g.us"
	privateChat   = "123456789012345@lid"
)

// fullConfig has every reply and template set. allowed_people and
// enabled_groups hold allowedPhone and enabledGroup.
const fullConfig = `{
	"keywords": {"ping": "pong"},
	"default_reply": "No entendí.",
	"image_ack": "Imagen {mimetype} {width}x{height} {size}",
	"document_ack": "Archivo {name} {mimetype} {size} {pages}",
	"allowed_people": ["5493624000001"],
	"enabled_groups": ["120363000000000001@g.us"]
}`

// sparseConfig has no default reply and no document confirmation.
const sparseConfig = `{
	"keywords": {"ping": "pong"},
	"image_ack": "Imagen {size}",
	"allowed_people": ["5493624000001"],
	"enabled_groups": ["120363000000000001@g.us"]
}`

func newResponder(t *testing.T, body string) *Responder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return New(cfg)
}

func image() *Media {
	return &Media{Kind: Image, Mimetype: "image/jpeg", Size: 2048, Width: 800, Height: 600}
}

func document() *Media {
	return &Media{Kind: Document, Mimetype: "application/pdf", Size: 3 * 1024 * 1024, Name: "informe.pdf", Pages: 4}
}

func privateText(phone, text string) Message {
	return Message{Private: true, Chat: privateChat, SenderPhone: phone, IsText: true, Text: text}
}

func privateFile(phone string, media *Media) Message {
	return Message{Private: true, Chat: privateChat, SenderPhone: phone, Media: media}
}

// groupText is a group text message. text is what is left after the session
// removes the bot's mentions.
func groupText(chat, text string, mentioned bool) Message {
	return Message{Group: true, Chat: chat, SenderPhone: otherPhone, IsText: true, Text: text, Mentioned: mentioned}
}

func groupFile(chat string, media *Media, mentioned bool) Message {
	return Message{Group: true, Chat: chat, SenderPhone: otherPhone, Media: media, Mentioned: mentioned}
}

const (
	imageReply    = "Imagen image/jpeg 800x600 2 KB"
	documentReply = "Archivo informe.pdf application/pdf 3.0 MB 4"
)

// result is what the bot does with a message: Admit's verdict and, for an
// admitted message, what Answer decides.
type result struct {
	Admission Admission
	Decision  Decision
}

var (
	ignored    = result{Admission: Ignored}
	notEnabled = result{Admission: NotEnabled}
	noMatch    = answered(Decision{Outcome: NoMatch})
	pong       = answered(Decision{Outcome: Reply, Keyword: "ping", Reply: "pong"})
)

func answered(d Decision) result { return result{Admission: Admitted, Decision: d} }

type decideCase struct {
	name string
	msg  Message
	want result
}

// runDecide runs each message through Admit and, if it is admitted, through
// Answer, the way the session does.
func runDecide(t *testing.T, r *Responder, tests []decideCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := result{Admission: r.Admit(tt.msg)}
			if got.Admission == Admitted {
				got.Decision = r.Answer(tt.msg)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func with(m Message, change func(*Message)) Message {
	change(&m)
	return m
}

func TestIgnored(t *testing.T) {
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"own private text", with(privateText(allowedPhone, "ping"), func(m *Message) { m.FromMe = true }), ignored},
		{"own group tag", with(groupText(enabledGroup, "ping", true), func(m *Message) { m.FromMe = true }), ignored},
		{"own image", with(privateFile(allowedPhone, image()), func(m *Message) { m.FromMe = true }), ignored},
		{"edited private text", with(privateText(allowedPhone, "ping"), func(m *Message) { m.IsEdit = true }), ignored},
		{"edited group tag", with(groupText(enabledGroup, "ping", true), func(m *Message) { m.IsEdit = true }), ignored},
		{"edited image", with(privateFile(allowedPhone, image()), func(m *Message) { m.IsEdit = true }), ignored},
		{"neither text nor file", Message{Private: true, Chat: privateChat, SenderPhone: allowedPhone}, ignored},
		{"text outside private chats and groups", Message{Chat: "status@broadcast", SenderPhone: allowedPhone, IsText: true, Text: "ping"}, ignored},
		{"file outside private chats and groups", Message{Chat: "status@broadcast", SenderPhone: allowedPhone, Media: image()}, ignored},
	})
}

func TestPrivate(t *testing.T) {
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"keyword", privateText(allowedPhone, "ping"), pong},
		{"keyword with case and spaces", privateText(allowedPhone, "  PING \n"), pong},
		// The default reply is only for group tags.
		{"other text", privateText(allowedPhone, "hola, ¿qué tal?"), noMatch},
		{"empty text", privateText(allowedPhone, ""), ignored},
		{"sender not on the list", privateText(otherPhone, "ping"), ignored},
		{"sender number not resolved", privateText("", "ping"), ignored},
	})
}

func TestGroup(t *testing.T) {
	defaultReply := answered(Decision{Outcome: Reply, Default: true, Reply: "No entendí."})
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"keyword without tag", groupText(enabledGroup, "ping", false), ignored},
		{"other text without tag", groupText(enabledGroup, "hola", false), ignored},
		{"tag in a group not enabled", groupText(disabledGroup, " ping", true), notEnabled},
		{"tag with keyword", groupText(enabledGroup, " ping", true), pong},
		{"tag with other text", groupText(enabledGroup, " hola", true), defaultReply},
		{"tag alone", groupText(enabledGroup, "", true), defaultReply},
		// Groups do not use the allowed list.
		{"tag from a sender not on the list", with(groupText(enabledGroup, " ping", true), func(m *Message) { m.SenderPhone = otherPhone }), pong},
		{"tag from a sender without a resolved number", with(groupText(enabledGroup, " ping", true), func(m *Message) { m.SenderPhone = "" }), pong},
	})
}

func TestFiles(t *testing.T) {
	imageAck := answered(Decision{Outcome: FileAck, Reply: imageReply})
	documentAck := answered(Decision{Outcome: FileAck, Reply: documentReply})
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"private image", privateFile(allowedPhone, image()), imageAck},
		{"private document", privateFile(allowedPhone, document()), documentAck},
		{"private image from a sender not on the list", privateFile(otherPhone, image()), ignored},
		{"group image without tag", groupFile(enabledGroup, image(), false), ignored},
		{"group image tagged in a group not enabled", groupFile(disabledGroup, image(), true), notEnabled},
		{"group image tagged", groupFile(enabledGroup, image(), true), imageAck},
		{"group document tagged", groupFile(enabledGroup, document(), true), documentAck},
		// A file is confirmed as a file; its text is not compared with the
		// keywords.
		{"file with keyword text", with(privateFile(allowedPhone, image()), func(m *Message) { m.IsText = true; m.Text = "ping" }), imageAck},
	})
}

func TestWithoutDefaultReplyOrTemplate(t *testing.T) {
	runDecide(t, newResponder(t, sparseConfig), []decideCase{
		{"tag with other text", groupText(enabledGroup, " hola", true), noMatch},
		{"tag with keyword", groupText(enabledGroup, " ping", true), pong},
		{"private document", privateFile(allowedPhone, document()), noMatch},
		{"group document tagged", groupFile(enabledGroup, document(), true), noMatch},
		{"private image still confirmed", privateFile(allowedPhone, image()), answered(Decision{Outcome: FileAck, Reply: "Imagen 2 KB"})},
	})
}

// Who the bot attends is decided only by Admit. Answer trusts that the
// message was admitted and does not look at the lists again.
func TestAnswerDoesNotCheckAdmission(t *testing.T) {
	r := newResponder(t, fullConfig)
	tests := []struct {
		name string
		msg  Message
		want Decision
	}{
		{"private text from a sender not on the list", privateText(otherPhone, "ping"), pong.Decision},
		{"tag in a group not enabled", groupText(disabledGroup, " ping", true), pong.Decision},
		{"group text without a tag", groupText(enabledGroup, " hola", false), Decision{Outcome: Reply, Default: true, Reply: "No entendí."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.Answer(tt.msg); got != tt.want {
				t.Errorf("Answer() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestWithDelayNotice(t *testing.T) {
	withNotice := newResponder(t, `{"delay_notice": "Perdón la demora."}`)
	without := newResponder(t, `{}`)
	reply := Decision{Outcome: Reply, Keyword: "ping", Reply: "pong"}
	ack := Decision{Outcome: FileAck, Reply: "Imagen 2 KB"}
	none := Decision{Outcome: NoMatch}

	tests := []struct {
		name string
		r    *Responder
		in   Decision
		want Decision
	}{
		{"keyword reply", withNotice, reply, Decision{Outcome: Reply, Keyword: "ping", DelayNotice: true, Reply: "Perdón la demora.\n\npong"}},
		{"file confirmation", withNotice, ack, Decision{Outcome: FileAck, DelayNotice: true, Reply: "Perdón la demora.\n\nImagen 2 KB"}},
		{"nothing to reply", withNotice, none, none},
		{"no notice configured", without, reply, reply},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.WithDelayNotice(tt.in); got != tt.want {
				t.Errorf("WithDelayNotice() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRender(t *testing.T) {
	const tmpl = "{mimetype}|{size}|{width}|{height}|{name}|{pages}"

	full := Media{Mimetype: "image/png", Size: 5 * 1024, Width: 10, Height: 20, Name: "a.pdf", Pages: 3}
	if got, want := render(tmpl, full), "image/png|5 KB|10|20|a.pdf|3"; got != want {
		t.Errorf("render(full) = %q, want %q", got, want)
	}
	if got, want := render(tmpl, Media{}), "?|?|?|?|?|?"; got != want {
		t.Errorf("render(empty) = %q, want %q", got, want)
	}
	if got, want := render("Recibido.", full), "Recibido."; got != want {
		t.Errorf("render(no placeholders) = %q, want %q", got, want)
	}
}

func TestFormatSize(t *testing.T) {
	const kb, mb, gb = 1024, 1024 * 1024, 1024 * 1024 * 1024
	tests := []struct {
		bytes uint64
		want  string
	}{
		{0, "?"},
		{1, "1 B"},
		{kb - 1, "1023 B"},
		{kb, "1 KB"},
		{kb + kb/2 - 1, "1 KB"},
		{kb + kb/2, "2 KB"},
		{10 * kb, "10 KB"},
		{mb, "1.0 MB"},
		{mb + mb/2, "1.5 MB"},
		{gb, "1.0 GB"},
		{2*gb + gb/2, "2.5 GB"},
	}
	for _, tt := range tests {
		if got := formatSize(tt.bytes); got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
