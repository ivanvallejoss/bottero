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
	secondPhone   = "5493624000003"
	enabledGroup  = "120363000000000001@g.us"
	disabledGroup = "120363000000000002@g.us"
	secondGroup   = "120363000000000003@g.us"
	privateChat   = "123456789012345@lid"
)

// fullConfig has two routes. main has every reply and template set, for
// allowedPhone and enabledGroup; otra, for secondPhone and secondGroup, only
// answers ping, with its own reply. otherPhone and disabledGroup are in no
// route.
const fullConfig = `{
	"routes": {
		"main": {
			"people": ["5493624000001"],
			"groups": ["120363000000000001@g.us"],
			"rules": {
				"keywords": {"ping": "pong"},
				"default_reply": "No entendí.",
				"image_ack": "Imagen {mimetype} {width}x{height} {size}",
				"document_ack": "Archivo {name} {mimetype} {size} {pages}"
			}
		},
		"otra": {
			"people": ["5493624000003"],
			"groups": ["120363000000000003@g.us"],
			"rules": {"keywords": {"ping": "pong desde otra"}}
		}
	}
}`

// sparseConfig has no default reply and no document confirmation.
const sparseConfig = `{
	"routes": {
		"main": {
			"people": ["5493624000001"],
			"groups": ["120363000000000001@g.us"],
			"rules": {"keywords": {"ping": "pong"}, "image_ack": "Imagen {size}"}
		}
	}
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

// result is what the bot does with a message: Admit's verdict and route and,
// for an admitted message, what Answer decides.
type result struct {
	Admission Admission
	Route     string
	Decision  Decision
}

var (
	ignored      = result{Admission: Ignored}
	noRoute      = result{Admission: NoRoute}
	noMatch      = answered("main", Decision{Outcome: NoMatch})
	pong         = answered("main", Decision{Outcome: Reply, Keyword: "ping", Reply: "pong"})
	defaultReply = answered("main", Decision{Outcome: Reply, Default: true, Reply: "No entendí."})
	otraPong     = answered("otra", Decision{Outcome: Reply, Keyword: "ping", Reply: "pong desde otra"})
)

func answered(route string, d Decision) result {
	return result{Admission: Admitted, Route: route, Decision: d}
}

type decideCase struct {
	name string
	msg  Message
	want result
}

// runDecide runs each message through Admit and, if it is admitted, through
// Answer with the route Admit chose, the way the session does.
func runDecide(t *testing.T, r *Responder, tests []decideCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got result
			got.Admission, got.Route = r.Admit(tt.msg)
			if got.Admission == Admitted {
				msg := tt.msg
				msg.Route = got.Route
				got.Decision = r.Answer(msg)
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
		// The default reply also answers in private chats.
		{"other text", privateText(allowedPhone, "hola, ¿qué tal?"), defaultReply},
		{"blank text", privateText(allowedPhone, "  "), defaultReply},
		{"empty text", privateText(allowedPhone, ""), ignored},
		{"sender in no route", privateText(otherPhone, "ping"), ignored},
		{"sender number not resolved", privateText("", "ping"), ignored},
		// The number chooses the route.
		{"sender in another route", privateText(secondPhone, "ping"), otraPong},
		{"other text in a route without a default reply", privateText(secondPhone, "hola"), answered("otra", Decision{Outcome: NoMatch})},
	})
}

func TestGroup(t *testing.T) {
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"keyword without tag", groupText(enabledGroup, "ping", false), ignored},
		{"other text without tag", groupText(enabledGroup, "hola", false), ignored},
		{"tag in a group in no route", groupText(disabledGroup, " ping", true), noRoute},
		{"tag with keyword", groupText(enabledGroup, " ping", true), pong},
		{"tag with other text", groupText(enabledGroup, " hola", true), defaultReply},
		{"tag alone", groupText(enabledGroup, "", true), defaultReply},
		{"tag in the other route's group", groupText(secondGroup, " ping", true), otraPong},
		// In groups the route is the group's, whoever writes.
		{"tag from a sender in no route", with(groupText(enabledGroup, " ping", true), func(m *Message) { m.SenderPhone = otherPhone }), pong},
		{"tag from a sender without a resolved number", with(groupText(enabledGroup, " ping", true), func(m *Message) { m.SenderPhone = "" }), pong},
		{"tag from a sender in another route", with(groupText(enabledGroup, " ping", true), func(m *Message) { m.SenderPhone = secondPhone }), pong},
		{"tag in no route from a sender in a route", with(groupText(disabledGroup, " ping", true), func(m *Message) { m.SenderPhone = allowedPhone }), noRoute},
	})
}

func TestFiles(t *testing.T) {
	imageAck := answered("main", Decision{Outcome: FileAck, Reply: imageReply})
	documentAck := answered("main", Decision{Outcome: FileAck, Reply: documentReply})
	runDecide(t, newResponder(t, fullConfig), []decideCase{
		{"private image", privateFile(allowedPhone, image()), imageAck},
		{"private document", privateFile(allowedPhone, document()), documentAck},
		{"private image from a sender in no route", privateFile(otherPhone, image()), ignored},
		{"private image in a route that does not confirm images", privateFile(secondPhone, image()), answered("otra", Decision{Outcome: NoMatch})},
		{"group image without tag", groupFile(enabledGroup, image(), false), ignored},
		{"group image tagged in a group in no route", groupFile(disabledGroup, image(), true), noRoute},
		{"group image tagged", groupFile(enabledGroup, image(), true), imageAck},
		{"group document tagged", groupFile(enabledGroup, document(), true), documentAck},
		// A file is confirmed as a file; its text is not compared with the
		// keywords, and the default reply does not answer it.
		{"file with keyword text", with(privateFile(allowedPhone, image()), func(m *Message) { m.IsText = true; m.Text = "ping" }), imageAck},
	})
}

func TestWithoutDefaultReplyOrTemplate(t *testing.T) {
	runDecide(t, newResponder(t, sparseConfig), []decideCase{
		{"private other text", privateText(allowedPhone, "hola"), noMatch},
		{"tag with other text", groupText(enabledGroup, " hola", true), noMatch},
		{"tag with keyword", groupText(enabledGroup, " ping", true), pong},
		{"private document", privateFile(allowedPhone, document()), noMatch},
		{"group document tagged", groupFile(enabledGroup, document(), true), noMatch},
		{"private image still confirmed", privateFile(allowedPhone, image()), answered("main", Decision{Outcome: FileAck, Reply: "Imagen 2 KB"})},
	})
}

// Who the bot attends is decided only by Admit. Answer replies with the
// rules of the route the message carries, and does not look at who sent it.
func TestAnswerUsesTheRouteOfTheMessage(t *testing.T) {
	r := newResponder(t, fullConfig)
	routed := func(m Message, route string) Message { m.Route = route; return m }
	tests := []struct {
		name string
		msg  Message
		want Decision
	}{
		{"private text from a sender in no route", routed(privateText(otherPhone, "ping"), "main"), pong.Decision},
		{"tag in a group in no route", routed(groupText(disabledGroup, " ping", true), "otra"), otraPong.Decision},
		{"group text without a tag", routed(groupText(enabledGroup, " hola", false), "main"), defaultReply.Decision},
		// The route is gone from the configuration, or the message never
		// had one.
		{"route not in the configuration", routed(privateText(allowedPhone, "ping"), "borrada"), Decision{Outcome: RouteMissing}},
		{"no route", privateText(allowedPhone, "ping"), Decision{Outcome: RouteMissing}},
		{"file with no route", privateFile(allowedPhone, image()), Decision{Outcome: RouteMissing}},
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
	r := newResponder(t, `{
		"defaults": {"keywords": {"ping": "pong"}, "delay_notice": "Perdón la demora."},
		"routes": {
			"con": {"people": ["5493624000001"]},
			"propio": {"people": ["5493624000002"], "rules": {"delay_notice": "Tarde, pero llego."}},
			"sin": {"people": ["5493624000003"], "rules": {"delay_notice": ""}}
		}
	}`)
	reply := Decision{Outcome: Reply, Keyword: "ping", Reply: "pong"}
	ack := Decision{Outcome: FileAck, Reply: "Imagen 2 KB"}
	none := Decision{Outcome: NoMatch}
	missing := Decision{Outcome: RouteMissing}

	tests := []struct {
		name  string
		route string
		in    Decision
		want  Decision
	}{
		{"keyword reply", "con", reply, Decision{Outcome: Reply, Keyword: "ping", DelayNotice: true, Reply: "Perdón la demora.\n\npong"}},
		{"file confirmation", "con", ack, Decision{Outcome: FileAck, DelayNotice: true, Reply: "Perdón la demora.\n\nImagen 2 KB"}},
		{"the route's own notice", "propio", reply, Decision{Outcome: Reply, Keyword: "ping", DelayNotice: true, Reply: "Tarde, pero llego.\n\npong"}},
		{"nothing to reply", "con", none, none},
		{"route not in the configuration", "con", missing, missing},
		{"notice turned off in the route", "sin", reply, reply},
		{"unknown route", "borrada", reply, reply},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.WithDelayNotice(tt.route, tt.in); got != tt.want {
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
		// Sizes that would round up to 1024 of a unit go in the next one.
		{mb - kb/2 - 1, "1023 KB"},
		{mb - kb/2, "1.0 MB"},
		{mb - 1, "1.0 MB"},
		{mb, "1.0 MB"},
		{mb + mb/2, "1.5 MB"},
		{1073636966, "1023.9 MB"},
		{gb - 1, "1.0 GB"},
		{gb, "1.0 GB"},
		{2*gb + gb/2, "2.5 GB"},
	}
	for _, tt := range tests {
		if got := formatSize(tt.bytes); got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
