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

func TestLoadFullConfig(t *testing.T) {
	cfg := mustLoad(t, `{
		"keywords": {"hola": "¡Hola!", "ping": "pong"},
		"default_reply": "No entendí.",
		"image_ack": "Imagen {mimetype} {width}x{height} {size}",
		"document_ack": "Archivo {name} {mimetype} {size} {pages}",
		"delay_notice": "Perdón la demora.",
		"allowed_people": ["5493624000001", "5493624000002"],
		"enabled_groups": ["120363000000000001@g.us"]
	}`)

	keywords, people, groups := cfg.Summary()
	if keywords != 2 || people != 2 || groups != 1 {
		t.Errorf("Summary() = %d keywords, %d people, %d groups; want 2, 2, 1", keywords, people, groups)
	}
	if keyword, reply, ok := cfg.Reply("ping"); !ok || keyword != "ping" || reply != "pong" {
		t.Errorf(`Reply("ping") = %q, %q, %v; want "ping", "pong", true`, keyword, reply, ok)
	}
	if got := cfg.DefaultReply(); got != "No entendí." {
		t.Errorf("DefaultReply() = %q", got)
	}
	if got := cfg.ImageAck(); got != "Imagen {mimetype} {width}x{height} {size}" {
		t.Errorf("ImageAck() = %q", got)
	}
	if got := cfg.DocumentAck(); got != "Archivo {name} {mimetype} {size} {pages}" {
		t.Errorf("DocumentAck() = %q", got)
	}
	if got := cfg.DelayNotice(); got != "Perdón la demora." {
		t.Errorf("DelayNotice() = %q", got)
	}

	for phone, want := range map[string]bool{
		"5493624000001": true,
		"5493624000002": true,
		"5493624000003": false,
		"":              false,
	} {
		if got := cfg.IsAllowedPerson(phone); got != want {
			t.Errorf("IsAllowedPerson(%q) = %v, want %v", phone, got, want)
		}
	}
	for group, want := range map[string]bool{
		"120363000000000001@g.us": true,
		"120363000000000002@g.us": false,
	} {
		if got := cfg.IsEnabledGroup(group); got != want {
			t.Errorf("IsEnabledGroup(%q) = %v, want %v", group, got, want)
		}
	}
}

func TestLoadEmptyObject(t *testing.T) {
	cfg := mustLoad(t, `{}`)

	if keywords, people, groups := cfg.Summary(); keywords != 0 || people != 0 || groups != 0 {
		t.Errorf("Summary() = %d, %d, %d; want all 0", keywords, people, groups)
	}
	if _, _, ok := cfg.Reply("ping"); ok {
		t.Error(`Reply("ping") matched with no keywords`)
	}
	if cfg.DefaultReply() != "" || cfg.ImageAck() != "" || cfg.DocumentAck() != "" || cfg.DelayNotice() != "" {
		t.Errorf("replies, templates and notice should be empty: %q, %q, %q, %q",
			cfg.DefaultReply(), cfg.ImageAck(), cfg.DocumentAck(), cfg.DelayNotice())
	}
}

func TestKeywordsIgnoreCaseAndSurroundingSpaces(t *testing.T) {
	cfg := mustLoad(t, `{"keywords": {"  Buen Día ": "  ¡Buen día!  "}}`)

	for _, text := range []string{"buen día", "BUEN DÍA", "  Buen Día\n"} {
		keyword, reply, ok := cfg.Reply(text)
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
		if _, _, ok := cfg.Reply(text); ok {
			t.Errorf("Reply(%q) matched", text)
		}
	}
}

func TestRepliesAndTemplatesAreTrimmed(t *testing.T) {
	cfg := mustLoad(t, `{"default_reply": "  No entendí.  ", "image_ack": " {size} ", "document_ack": "\t{name}\n", "delay_notice": " Perdón la demora.\n"}`)

	if got := cfg.DefaultReply(); got != "No entendí." {
		t.Errorf("DefaultReply() = %q", got)
	}
	if got := cfg.ImageAck(); got != "{size}" {
		t.Errorf("ImageAck() = %q", got)
	}
	if got := cfg.DocumentAck(); got != "{name}" {
		t.Errorf("DocumentAck() = %q", got)
	}
	if got := cfg.DelayNotice(); got != "Perdón la demora." {
		t.Errorf("DelayNotice() = %q", got)
	}
}

func TestBlankRepliesAndTemplatesMeanSilence(t *testing.T) {
	cfg := mustLoad(t, `{"default_reply": "   ", "image_ack": " ", "document_ack": "\n", "delay_notice": "  "}`)

	if cfg.DefaultReply() != "" || cfg.ImageAck() != "" || cfg.DocumentAck() != "" || cfg.DelayNotice() != "" {
		t.Errorf("blank values should load as empty: %q, %q, %q, %q", cfg.DefaultReply(), cfg.ImageAck(), cfg.DocumentAck(), cfg.DelayNotice())
	}
}

func TestTemplatesAcceptTheirPlaceholders(t *testing.T) {
	mustLoad(t, `{
		"image_ack": "{mimetype} {size} {width} {height}",
		"document_ack": "{mimetype} {size} {name} {pages}"
	}`)
}

func TestPhoneLengthLimits(t *testing.T) {
	cfg := mustLoad(t, `{"allowed_people": ["12345678", "123456789012345"]}`)

	for _, phone := range []string{"12345678", "123456789012345"} {
		if !cfg.IsAllowedPerson(phone) {
			t.Errorf("IsAllowedPerson(%q) = false", phone)
		}
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty file", ``},
		{"broken JSON", `{"keywords": {"ping": "pong"}`},
		{"wrong type", `{"keywords": ["ping"]}`},
		{"unknown field", `{"keyword": {"ping": "pong"}}`},
		{"empty keyword", `{"keywords": {"  ": "pong"}}`},
		{"empty reply", `{"keywords": {"ping": "  "}}`},
		// The error names either keyword, depending on map order, so only
		// the error kind is checked.
		{"keywords equal after normalizing", `{"keywords": {"Ping": "a", "ping ": "b"}}`},
		{"example phone", `{"allowed_people": ["549XXXXXXXXXX"]}`},
		{"phone with plus sign", `{"allowed_people": ["+5493624000001"]}`},
		{"phone too short", `{"allowed_people": ["1234567"]}`},
		{"phone too long", `{"allowed_people": ["1234567890123456"]}`},
		{"phone starting with zero", `{"allowed_people": ["03624000001"]}`},
		{"group without suffix", `{"enabled_groups": ["120363000000000001"]}`},
		{"group suffix only", `{"enabled_groups": ["@g.us"]}`},
		{"private chat as group", `{"enabled_groups": ["5493624000001@s.whatsapp.net"]}`},
		{"LID as group", `{"enabled_groups": ["123456789012345@lid"]}`},
		{"document placeholder in image_ack", `{"image_ack": "{name}"}`},
		{"image placeholder in document_ack", `{"document_ack": "{width}"}`},
		{"misspelled placeholder", `{"image_ack": "{mime}"}`},
		{"empty placeholder", `{"document_ack": "{}"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Load error = %v, want ErrInvalid", err)
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
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "allowed_people") {
		t.Errorf("Load error = %v, want ErrInvalid about allowed_people", err)
	}
}
