package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes body to a config file in a temporary directory and
// returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func mustRules(t *testing.T, cfg *Config, route string) *Rules {
	t.Helper()
	rules, ok := cfg.Rules(route)
	if !ok {
		t.Fatalf("Rules(%q) not found", route)
	}
	return rules
}

// texts is the four text fields of a route's rules, for comparing them at
// once.
type texts struct{ defaultReply, imageAck, documentAck, delayNotice string }

func textsOf(r *Rules) texts {
	return texts{r.DefaultReply(), r.ImageAck(), r.DocumentAck(), r.DelayNotice()}
}

// fullConfig has defaults and two routes: personal inherits almost
// everything, familia sets its own keyword and confirmations.
const fullConfig = `{
	"defaults": {
		"keywords": {"hola": "¡Hola!", "ping": "pong", "ayuda": "Probá con hola o ping."},
		"default_reply": "No entendí.",
		"image_ack": "Imagen {size}",
		"document_ack": "Archivo {name}",
		"delay_notice": "Perdón la demora."
	},
	"routes": {
		"personal": {
			"people": ["5493624000001", "5493624000002"],
			"groups": ["120363000000000001@g.us"],
			"rules": {"delay_notice": ""}
		},
		"familia": {
			"groups": ["120363000000000002@g.us"],
			"rules": {
				"keywords": {"Ayuda": "En familia: hola, ping, cena.", "cena": "A las 21."},
				"image_ack": "Foto {width}x{height}",
				"document_ack": "  "
			}
		}
	}
}`

func TestChatsChooseTheirRoute(t *testing.T) {
	cfg := mustLoad(t, fullConfig)

	for phone, want := range map[string]string{
		"5493624000001": "personal",
		"5493624000002": "personal",
		"5493624000003": "",
		"":              "",
	} {
		got, ok := cfg.PersonRoute(phone)
		if got != want || ok != (want != "") {
			t.Errorf("PersonRoute(%q) = %q, %v; want %q", phone, got, ok, want)
		}
	}
	for group, want := range map[string]string{
		"120363000000000001@g.us": "personal",
		"120363000000000002@g.us": "familia",
		"120363000000000003@g.us": "",
	} {
		got, ok := cfg.GroupRoute(group)
		if got != want || ok != (want != "") {
			t.Errorf("GroupRoute(%q) = %q, %v; want %q", group, got, ok, want)
		}
	}
	// A number is not a group and a group is not a number.
	if _, ok := cfg.GroupRoute("5493624000001"); ok {
		t.Error("a person's number chose a group route")
	}
	if _, ok := cfg.PersonRoute("120363000000000001@g.us"); ok {
		t.Error("a group JID chose a person route")
	}
	if _, ok := cfg.Rules("otra"); ok {
		t.Error(`Rules("otra") found a route that is not in the file`)
	}
}

// A route takes from defaults every field it leaves out; "" turns a text
// off.
func TestRoutesInheritTheirTexts(t *testing.T) {
	cfg := mustLoad(t, fullConfig)

	personal := textsOf(mustRules(t, cfg, "personal"))
	if want := (texts{"No entendí.", "Imagen {size}", "Archivo {name}", ""}); personal != want {
		t.Errorf("personal = %+v, want %+v", personal, want)
	}
	// A blank text counts as "" and turns the field off too.
	familia := textsOf(mustRules(t, cfg, "familia"))
	if want := (texts{"No entendí.", "Foto {width}x{height}", "", "Perdón la demora."}); familia != want {
		t.Errorf("familia = %+v, want %+v", familia, want)
	}
}

// A route's keywords are added to those of defaults; a keyword in both
// takes the route's reply, even written differently.
func TestRoutesMergeTheirKeywords(t *testing.T) {
	cfg := mustLoad(t, fullConfig)
	tests := []struct {
		route, text, want string
	}{
		{"personal", "ping", "pong"},
		{"personal", "ayuda", "Probá con hola o ping."},
		{"personal", "cena", ""},
		{"familia", "ping", "pong"},
		{"familia", "hola", "¡Hola!"},
		{"familia", "AYUDA", "En familia: hola, ping, cena."},
		{"familia", "cena", "A las 21."},
	}
	for _, tt := range tests {
		_, reply, ok := mustRules(t, cfg, tt.route).Reply(tt.text)
		if reply != tt.want || ok != (tt.want != "") {
			t.Errorf("%s: Reply(%q) = %q, %v; want %q", tt.route, tt.text, reply, ok, tt.want)
		}
	}
}

func TestRoutesDescribeThemselves(t *testing.T) {
	cfg := mustLoad(t, fullConfig)
	want := []RouteInfo{
		{Name: "familia", People: 0, Groups: 1, Keywords: 4, OwnKeywords: 2,
			DefaultReply: Inherited, ImageAck: Own, DocumentAck: Off, DelayNotice: Inherited},
		{Name: "personal", People: 2, Groups: 1, Keywords: 3, OwnKeywords: 0,
			DefaultReply: Inherited, ImageAck: Inherited, DocumentAck: Inherited, DelayNotice: Off},
	}
	got := cfg.Routes()
	if len(got) != len(want) {
		t.Fatalf("Routes() = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Routes()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A route without rules takes all of defaults, and one without defaults
// has only its own.
func TestRoutesWithoutRulesOrWithoutDefaults(t *testing.T) {
	cfg := mustLoad(t, `{
		"defaults": {"keywords": {"ping": "pong"}},
		"routes": {"todo": {"people": ["5493624000001"]}}
	}`)
	if _, reply, _ := mustRules(t, cfg, "todo").Reply("ping"); reply != "pong" {
		t.Errorf("route without rules: Reply(ping) = %q", reply)
	}

	cfg = mustLoad(t, `{"routes": {"solo": {"people": ["5493624000001"], "rules": {"image_ack": "Imagen"}}}}`)
	if got := textsOf(mustRules(t, cfg, "solo")); got != (texts{imageAck: "Imagen"}) {
		t.Errorf("route without defaults = %+v", got)
	}
	if info := cfg.Routes()[0]; info.ImageAck != Own || info.DefaultReply != Off || info.Keywords != 0 {
		t.Errorf("route without defaults described as %+v", info)
	}
}

// A configuration without routes, and a route without chats, are valid: the
// bot answers nobody, or nobody in that route.
func TestRoutesMayBeEmpty(t *testing.T) {
	for _, body := range []string{`{}`, `{"routes": {}}`, `{"defaults": {"keywords": {"ping": "pong"}}}`} {
		if cfg := mustLoad(t, body); len(cfg.Routes()) != 0 {
			t.Errorf("%s: Routes() = %+v, want none", body, cfg.Routes())
		}
	}
	cfg := mustLoad(t, `{"routes": {"pausada": {"rules": {"keywords": {"ping": "pong"}}}}}`)
	if info := cfg.Routes()[0]; info.People != 0 || info.Groups != 0 {
		t.Errorf("route without chats described as %+v", info)
	}
}

func TestKeywordsIgnoreCaseAndSurroundingSpaces(t *testing.T) {
	cfg := mustLoad(t, `{"routes": {"r": {"people": ["5493624000001"], "rules": {"keywords": {"  Buen Día ": "  ¡Buen día!  "}}}}}`)
	rules := mustRules(t, cfg, "r")

	for _, text := range []string{"buen día", "BUEN DÍA", "  Buen Día\n"} {
		keyword, reply, ok := rules.Reply(text)
		if !ok {
			t.Errorf("Reply(%q) did not match", text)
			continue
		}
		if keyword != "buen día" {
			t.Errorf("Reply(%q) keyword = %q, want the normalized form", text, keyword)
		}
		if reply != "  ¡Buen día!  " {
			t.Errorf("Reply(%q) reply = %q, want it as written in the file", text, reply)
		}
	}

	// Inner spaces and any other character count.
	for _, text := range []string{"buen  día", "buendía", "buen día!"} {
		if _, _, ok := rules.Reply(text); ok {
			t.Errorf("Reply(%q) matched", text)
		}
	}
}

func TestTextsAreTrimmed(t *testing.T) {
	cfg := mustLoad(t, `{
		"defaults": {"default_reply": "  No entendí.  ", "image_ack": " {size} ", "document_ack": "\t{name}\n", "delay_notice": " Perdón la demora.\n"},
		"routes": {"r": {"people": ["5493624000001"]}}
	}`)
	if got, want := textsOf(mustRules(t, cfg, "r")), (texts{"No entendí.", "{size}", "{name}", "Perdón la demora."}); got != want {
		t.Errorf("texts = %+v, want %+v", got, want)
	}
}

func TestTemplatesAcceptTheirPlaceholders(t *testing.T) {
	mustLoad(t, `{
		"defaults": {"image_ack": "{mimetype} {size} {width} {height}", "document_ack": "{mimetype} {size} {name} {pages}"},
		"routes": {"r": {"rules": {"image_ack": "{width}", "document_ack": "{pages}"}}}
	}`)
}

func TestPhoneLengthLimits(t *testing.T) {
	cfg := mustLoad(t, `{"routes": {"r": {"people": ["12345678", "123456789012345"], "rules": {"default_reply": "x"}}}}`)
	for _, phone := range []string{"12345678", "123456789012345"} {
		if _, ok := cfg.PersonRoute(phone); !ok {
			t.Errorf("PersonRoute(%q) not found", phone)
		}
	}
}

// route is a route with chats and something to answer with, for the error
// cases to change one thing.
func route(fields string) string {
	return `{"routes": {"r": {` + fields + `}}}`
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	const answer = `"rules": {"default_reply": "x"}`
	tests := []struct {
		name string
		body string
		// want is part of the error: where in the file it is and what.
		want string
	}{
		{"empty file", ``, "the file is empty"},
		{"broken JSON", `{"routes": {}`, "unexpected end"},
		{"not an object", `[]`, "the configuration: must be an object"},
		{"data after the object", `{} {}`, "there is more after the configuration object"},
		{"text after the object", `{}x`, "there is more after the configuration object"},

		// The previous layout fails at its first field.
		{"previous layout", `{"keywords": {"ping": "pong"}, "allowed_people": ["5493624000001"]}`,
			`the configuration: unknown field "keywords" (allowed: defaults, routes)`},
		{"unknown field in a route", route(`"peple": ["5493624000001"]`), `routes.r: unknown field "peple" (allowed: groups, people, rules)`},
		{"unknown field in the rules", route(`"rules": {"keyword": {}}`), `routes.r.rules: unknown field "keyword"`},
		{"unknown field in defaults", `{"defaults": {"delay": "x"}}`, `defaults: unknown field "delay"`},

		{"wrong type for people", route(`"people": "5493624000001"`), "routes.r.people: must be a list of texts"},
		{"wrong type in people", route(`"people": [5493624000001]`), "routes.r.people[0]: must be a text"},
		{"wrong type for keywords", `{"defaults": {"keywords": ["ping"]}}`, "defaults.keywords: must be an object"},
		{"wrong type for a reply", `{"defaults": {"keywords": {"ping": 1}}}`, "defaults.keywords.ping: must be a text"},
		{"wrong type for routes", `{"routes": []}`, "routes: must be an object"},

		{"null text in a route", route(`"rules": {"delay_notice": null}`),
			`routes.r.rules.delay_notice: null is not allowed (leave the field out to inherit it, or write "" to turn it off)`},
		{"null text in defaults", `{"defaults": {"image_ack": null}}`, "defaults.image_ack: null is not allowed"},
		{"null people", route(`"people": null`), "routes.r.people: null is not allowed (leave the field out instead)"},
		{"null rules", route(`"rules": null`), "routes.r.rules: null is not allowed"},
		{"null route", `{"routes": {"r": null}}`, "routes.r: null is not allowed"},
		{"null keyword reply", `{"defaults": {"keywords": {"buen día": null}}}`, `defaults.keywords["buen día"]: null is not allowed`},

		{"keyword repeated exactly", `{"defaults": {"keywords": {"ping": "a", "ping": "b"}}}`, `defaults.keywords: "ping" is repeated`},
		{"route repeated", `{"routes": {"r": {}, "r": {}}}`, `routes: "r" is repeated`},
		{"field repeated", route(`"people": [], "people": []`), `routes.r: "people" is repeated`},
		{"keywords equal after normalizing", `{"defaults": {"keywords": {"Ping": "a", "ping ": "b"}}}`,
			`defaults.keywords: "ping " is repeated (keywords ignore case and surrounding spaces)`},
		{"empty keyword", `{"defaults": {"keywords": {"  ": "pong"}}}`, "defaults.keywords: empty keyword"},
		{"empty reply", route(`"rules": {"keywords": {"ping": "  "}}`), `routes.r.rules.keywords: "ping" has an empty reply`},

		{"route name with upper case", `{"routes": {"Familia": {}}}`, `routes: "Familia" is not a valid route name`},
		{"route name with a space", `{"routes": {"mi grupo": {}}}`, `routes: "mi grupo" is not a valid route name`},
		{"empty route name", `{"routes": {"": {}}}`, `routes: "" is not a valid route name`},

		{"example phone", route(`"people": ["549XXXXXXXXXX"], ` + answer), `routes.r.people: "549XXXXXXXXXX" is not a phone number`},
		{"phone with plus sign", route(`"people": ["+5493624000001"], ` + answer), "is not a phone number"},
		{"phone too short", route(`"people": ["1234567"], ` + answer), "is not a phone number"},
		{"phone too long", route(`"people": ["1234567890123456"], ` + answer), "is not a phone number"},
		{"phone starting with zero", route(`"people": ["03624000001"], ` + answer), "is not a phone number"},
		{"group without suffix", route(`"groups": ["120363000000000001"], ` + answer), `routes.r.groups: "120363000000000001" is not a group JID`},
		{"group suffix only", route(`"groups": ["@g.us"], ` + answer), "is not a group JID"},
		{"private chat as group", route(`"groups": ["5493624000001@s.whatsapp.net"], ` + answer), "is not a group JID"},
		{"LID as group", route(`"groups": ["123456789012345@lid"], ` + answer), "is not a group JID"},

		{"person repeated in a route", route(`"people": ["5493624000001", "5493624000001"], ` + answer), `routes.r.people: "5493624000001" is repeated`},
		{"group repeated in a route", route(`"groups": ["120363000000000001@g.us", "120363000000000001@g.us"], ` + answer), "is repeated"},
		// Routes are checked in name order, so the second one reports it.
		{"person in two routes", `{"routes": {
			"a": {"people": ["5493624000001"], "rules": {"default_reply": "x"}},
			"b": {"people": ["5493624000001"], "rules": {"default_reply": "x"}}}}`,
			`routes.b.people: "5493624000001" is also in route "a"`},
		{"group in two routes", `{"routes": {
			"a": {"groups": ["120363000000000001@g.us"], "rules": {"default_reply": "x"}},
			"b": {"groups": ["120363000000000001@g.us"], "rules": {"default_reply": "x"}}}}`,
			`routes.b.groups: "120363000000000001@g.us" is also in route "a"`},

		{"route with nothing to answer", route(`"people": ["5493624000001"]`), "routes.r: the route has nothing to answer with"},
		{"route that turns off all it inherits", `{"defaults": {"default_reply": "x", "image_ack": "y"},
			"routes": {"r": {"rules": {"default_reply": "", "image_ack": ""}}}}`, "routes.r: the route has nothing to answer with"},
		// The delay notice is not an answer.
		{"route with only a delay notice", route(`"rules": {"delay_notice": "Perdón."}`), "nothing to answer with"},

		{"document placeholder in image_ack", `{"defaults": {"image_ack": "{name}"}}`, "defaults.image_ack: unknown placeholder {name}"},
		{"image placeholder in document_ack", route(`"rules": {"document_ack": "{width}"}`), "routes.r.rules.document_ack: unknown placeholder {width}"},
		{"misspelled placeholder", `{"defaults": {"image_ack": "{mime}"}}`, "unknown placeholder {mime}"},
		{"empty placeholder", `{"defaults": {"document_ack": "{}"}}`, "unknown placeholder {}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load error = %v, want ErrInvalid wrapping os.ErrNotExist", err)
	}
}

// The example in the repo carries a placeholder number, so it is rejected
// until a real one replaces it, as the README says.
func TestLoadRejectsExampleConfig(t *testing.T) {
	_, err := Load(filepath.Join("..", "..", "deploy", "config.example.json"))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `.people: "549XXXXXXXXXX" is not a phone number`) {
		t.Errorf("Load error = %v, want ErrInvalid about the placeholder number", err)
	}
}

// With the placeholder replaced, the example loads.
func TestExampleConfigLoadsWithARealNumber(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	mustLoad(t, strings.ReplaceAll(string(data), "549XXXXXXXXXX", "5493624000001"))
}
