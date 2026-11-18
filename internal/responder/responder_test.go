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

var pong = Decision{Outcome: Reply, Keyword: "ping", Reply: "pong"}

type decideCase struct {
	name string
	msg  Message
	want Decision
}

func runDecide(t *testing.T, r *Responder, tests []decideCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.Decide(tt.msg); got != tt.want {
				t.Errorf("Decide() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func with(m Message, change func(*Message)) Message {
	change(&m)
	return m
}

func TestDecideIgnores(t *testing.T) {
	ignore := Decision{Outcome: Ignore}
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"own private text", with(privateText(allowedPhone, "ping"), func(m *Message) { m.FromMe = true }), ignore},
		{"own group tag", with(groupText(enabledGroup, "ping", true), func(m *Message) { m.FromMe = true }), ignore},
		{"own image", with(privateFile(allowedPhone, image()), func(m *Message) { m.FromMe = true }), ignore},
		{"edited private text", with(privateText(allowedPhone, "ping"), func(m *Message) { m.IsEdit = true }), ignore},
		{"edited group tag", with(groupText(enabledGroup, "ping", true), func(m *Message) { m.IsEdit = true }), ignore},
		{"edited image", with(privateFile(allowedPhone, image()), func(m *Message) { m.IsEdit = true }), ignore},
		{"neither text nor file", Message{Private: true, Chat: privateChat, SenderPhone: allowedPhone}, ignore},
		{"text outside private chats and groups", Message{Chat: "status@broadcast", SenderPhone: allowedPhone, IsText: true, Text: "ping"}, ignore},
		{"file outside private chats and groups", Message{Chat: "status@broadcast", SenderPhone: allowedPhone, Media: image()}, ignore},
	})
}

func TestDecidePrivate(t *testing.T) {
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"keyword", privateText(allowedPhone, "ping"), pong},
		{"keyword with case and spaces", privateText(allowedPhone, "  PING \n"), pong},
		// The default reply is only for group tags.
		{"other text", privateText(allowedPhone, "hola, ¿qué tal?"), Decision{Outcome: NoMatch}},
		{"empty text", privateText(allowedPhone, ""), Decision{Outcome: Ignore}},
		{"sender not on the list", privateText(otherPhone, "ping"), Decision{Outcome: Ignore}},
		{"sender number not resolved", privateText("", "ping"), Decision{Outcome: Ignore}},
	})
}

func TestDecideGroup(t *testing.T) {
	defaultReply := Decision{Outcome: Reply, Default: true, Reply: "No entendí."}
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"keyword without tag", groupText(enabledGroup, "ping", false), Decision{Outcome: Ignore}},
		{"other text without tag", groupText(enabledGroup, "hola", false), Decision{Outcome: Ignore}},
		{"tag in a group not enabled", groupText(disabledGroup, " ping", true), Decision{Outcome: GroupNotEnabled}},
		{"tag with keyword", groupText(enabledGroup, " ping", true), pong},
		{"tag with other text", groupText(enabledGroup, " hola", true), defaultReply},
		{"tag alone", groupText(enabledGroup, "", true), defaultReply},
		// Groups do not use the allowed list.
		{"tag from a sender not on the list", with(groupText(enabledGroup, " ping", true), func(m *Message) { m.SenderPhone = otherPhone }), pong},
		{"tag from a sender without a resolved number", with(groupText(enabledGroup, " ping", true), func(m *Message) { m.SenderPhone = "" }), pong},
	})
}

func TestDecideFiles(t *testing.T) {
	imageAck := Decision{Outcome: FileAck, Media: Image, Reply: imageReply}
	documentAck := Decision{Outcome: FileAck, Media: Document, Reply: documentReply}
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"private image", privateFile(allowedPhone, image()), imageAck},
		{"private document", privateFile(allowedPhone, document()), documentAck},
		{"private image from a sender not on the list", privateFile(otherPhone, image()), Decision{Outcome: Ignore}},
		{"group image without tag", groupFile(enabledGroup, image(), false), Decision{Outcome: Ignore}},
		{"group image tagged in a group not enabled", groupFile(disabledGroup, image(), true), Decision{Outcome: GroupNotEnabled, Media: Image}},
		{"group image tagged", groupFile(enabledGroup, image(), true), imageAck},
		{"group document tagged", groupFile(enabledGroup, document(), true), documentAck},
		// A file is confirmed as a file; its text is not compared with the
		// keywords.
		{"file with keyword text", with(privateFile(allowedPhone, image()), func(m *Message) { m.IsText = true; m.Text = "ping" }), imageAck},
	})
}

func TestDecideWithoutDefaultReplyOrTemplate(t *testing.T) {
	runDecide(t, newResponder(t, sparseConfig), []decideCase{
		{"tag with other text", groupText(enabledGroup, " hola", true), Decision{Outcome: NoMatch}},
		{"tag with keyword", groupText(enabledGroup, " ping", true), pong},
		{"private document", privateFile(allowedPhone, document()), Decision{Outcome: NoMatch, Media: Document}},
		{"group document tagged", groupFile(enabledGroup, document(), true), Decision{Outcome: NoMatch, Media: Document}},
		{"private image still confirmed", privateFile(allowedPhone, image()), Decision{Outcome: FileAck, Media: Image, Reply: "Imagen 2 KB"}},
	})
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
