// Package config loads the bot's configuration: keywords and their fixed
// replies, the people the bot answers in private chats and the groups where
// it is enabled. The file is read once at startup; changes apply on restart.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrInvalid wraps every error Load returns. A missing or invalid file is not
// fixed by restarting, so the caller exits with its own code.
var ErrInvalid = errors.New("invalid configuration")

// Config is the parsed configuration.
type Config struct {
	// keywords maps a normalized keyword to its reply.
	keywords map[string]string
	// defaultReply answers a group tag without a keyword. Unused until the
	// groups capability.
	defaultReply string
	// allowedPeople holds phone numbers, digits only.
	allowedPeople map[string]bool
	// enabledGroups holds group JIDs. Unused until the groups capability.
	enabledGroups map[string]bool
}

// file mirrors the JSON layout. Unknown fields are rejected, so a typo in a
// field name fails at startup instead of being silently ignored.
type file struct {
	Keywords      map[string]string `json:"keywords"`
	DefaultReply  string            `json:"default_reply"`
	AllowedPeople []string          `json:"allowed_people"`
	EnabledGroups []string          `json:"enabled_groups"`
}

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	defer f.Close()

	var raw file
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}

	cfg := &Config{
		keywords:      make(map[string]string, len(raw.Keywords)),
		defaultReply:  strings.TrimSpace(raw.DefaultReply),
		allowedPeople: make(map[string]bool, len(raw.AllowedPeople)),
		enabledGroups: make(map[string]bool, len(raw.EnabledGroups)),
	}

	for keyword, reply := range raw.Keywords {
		norm := NormalizeKeyword(keyword)
		if norm == "" {
			return nil, fmt.Errorf("%w: keywords: empty keyword", ErrInvalid)
		}
		if strings.TrimSpace(reply) == "" {
			return nil, fmt.Errorf("%w: keywords: %q has an empty reply", ErrInvalid, keyword)
		}
		if _, dup := cfg.keywords[norm]; dup {
			return nil, fmt.Errorf("%w: keywords: %q is repeated (keywords ignore case and surrounding spaces)", ErrInvalid, keyword)
		}
		cfg.keywords[norm] = reply
	}

	for _, phone := range raw.AllowedPeople {
		if !isPhone(phone) {
			return nil, fmt.Errorf("%w: allowed_people: %q is not a phone number (digits only, international format, e.g. 549...)", ErrInvalid, phone)
		}
		cfg.allowedPeople[phone] = true
	}

	for _, group := range raw.EnabledGroups {
		if !strings.HasSuffix(group, "@g.us") || len(group) == len("@g.us") {
			return nil, fmt.Errorf("%w: enabled_groups: %q is not a group JID (expected ...@g.us)", ErrInvalid, group)
		}
		cfg.enabledGroups[group] = true
	}

	return cfg, nil
}

// NormalizeKeyword is the form used to compare a message with a keyword:
// surrounding spaces removed, lower case.
func NormalizeKeyword(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Reply returns the reply for a message whose whole text matches a keyword.
func (c *Config) Reply(text string) (keyword, reply string, ok bool) {
	keyword = NormalizeKeyword(text)
	reply, ok = c.keywords[keyword]
	return keyword, reply, ok
}

// IsAllowedPerson reports whether phone (digits only) is on the allowed list.
func (c *Config) IsAllowedPerson(phone string) bool {
	return phone != "" && c.allowedPeople[phone]
}

// Summary describes the loaded configuration for the startup log line,
// without the phone numbers themselves.
func (c *Config) Summary() (keywords, people, groups int) {
	return len(c.keywords), len(c.allowedPeople), len(c.enabledGroups)
}

func isPhone(s string) bool {
	if len(s) < 8 || len(s) > 15 || s[0] == '0' {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
