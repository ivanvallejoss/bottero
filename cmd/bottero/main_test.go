package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ivanvallejoss/bottero/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// check logs one line per route, with where each text comes from, and no
// phone numbers or group JIDs.
func TestCheckDescribesTheRoutes(t *testing.T) {
	path := writeConfig(t, `{
		"defaults": {"keywords": {"ping": "pong"}, "default_reply": "No entendí.", "delay_notice": "Perdón la demora."},
		"routes": {
			"personal": {"people": ["5493624000001"], "rules": {"delay_notice": ""}},
			"familia": {"groups": ["120363000000000001@g.us"], "rules": {"keywords": {"cena": "A las 21."}, "image_ack": "Foto {size}"}}
		}
	}`)
	var out bytes.Buffer
	if err := checkCmd(slog.New(slog.NewTextHandler(&out, nil)), []string{"-config", path}); err != nil {
		t.Fatalf("check: %v", err)
	}
	log := out.String()
	for _, want := range []string{
		`msg="configuration loaded" component=bottero routes=2`,
		`msg="route loaded" component=bottero route=familia people=0 groups=1 keywords=2 own_keywords=1 default_reply=inherited image_ack=own document_ack=off delay_notice=inherited`,
		`msg="route loaded" component=bottero route=personal people=1 groups=0 keywords=1 own_keywords=0 default_reply=inherited image_ack=off document_ack=off delay_notice=off`,
		`msg="configuration is valid"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("no line with %q in:\n%s", want, log)
		}
	}
	for _, private := range []string{"5493624000001", "120363000000000001"} {
		if strings.Contains(log, private) {
			t.Errorf("the log shows %s:\n%s", private, log)
		}
	}
}

// An invalid configuration makes check fail the same way run does, so main
// exits with exitConfig.
func TestCheckRejectsAnInvalidConfiguration(t *testing.T) {
	path := writeConfig(t, `{"routes": {"personal": {"people": ["5493624000001"], "rules": {"delay_notice": null}}}}`)
	var out bytes.Buffer
	err := checkCmd(slog.New(slog.NewTextHandler(&out, nil)), []string{"-config", path})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("check error = %v, want ErrInvalid", err)
	}
	if strings.Contains(out.String(), "configuration is valid") {
		t.Errorf("check said the configuration is valid:\n%s", out.String())
	}
}
