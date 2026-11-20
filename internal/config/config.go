// Package config loads the bot's configuration: the routes, which say which
// chats the bot answers and with which rules, and the defaults every route
// inherits. The file is read once at startup; changes apply on restart.
//
// A route has a name, the chats it covers (people, for private chats with
// them, and groups) and its rules: keywords and their fixed replies, the
// default reply, the confirmations for images and documents, and the notice
// for late replies. A field the route leaves out comes from defaults; a text
// field set to "" turns it off. The route's keywords are added to those of
// defaults, and a keyword in both takes the route's reply.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Placeholders each confirmation template may use. The responder fills them
// in with the metadata the message carries.
var (
	ImagePlaceholders    = []string{"mimetype", "size", "width", "height"}
	DocumentPlaceholders = []string{"mimetype", "size", "name", "pages"}
)

// ErrInvalid wraps every error Load returns. A missing or invalid file is not
// fixed by restarting, so the caller exits with its own code.
var ErrInvalid = errors.New("invalid configuration")

// Config is the parsed configuration.
type Config struct {
	// rules maps a route name to its rules, with what the route inherits
	// from defaults already applied.
	rules map[string]*Rules
	// people and groups map each chat to the name of its route: a phone
	// number (digits only) stands for the private chat with that person, a
	// group JID for that group. A chat is in one route at most.
	people map[string]string
	groups map[string]string
	// routes describes each route for the startup log, sorted by name.
	routes []RouteInfo
}

// Rules is what the bot answers with in one route.
type Rules struct {
	// keywords maps a normalized keyword to its reply.
	keywords map[string]string
	// defaultReply answers a text without a keyword; "" means silence.
	defaultReply string
	// imageAck and documentAck are the confirmation templates; "" means the
	// bot does not confirm that kind of file.
	imageAck    string
	documentAck string
	// delayNotice goes before a reply that is late; "" means no notice.
	delayNotice string
}

// Source says where a text field of a route comes from.
type Source string

const (
	// Own: the route sets the field.
	Own Source = "own"
	// Inherited: the field comes from defaults.
	Inherited Source = "inherited"
	// Off: the field is empty, wherever it comes from.
	Off Source = "off"
)

// RouteInfo describes a route for the startup log, without the phone numbers
// and group JIDs themselves.
type RouteInfo struct {
	Name   string
	People int
	Groups int
	// Keywords counts the route's keywords, inherited ones included;
	// OwnKeywords, the ones the route itself sets (also those that replace
	// an inherited reply).
	Keywords    int
	OwnKeywords int

	DefaultReply Source
	ImageAck     Source
	DocumentAck  Source
	DelayNotice  Source
}

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := check(data); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	// check already rejected unknown fields, wrong types, null, repeated keys
	// and data after the object; the decoder still refuses unknown fields
	// in case the two ever disagree.
	var raw file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	cfg, err := build(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return cfg, nil
}

// PersonRoute returns the route of the private chat with the person whose
// phone number (digits only) is phone.
func (c *Config) PersonRoute(phone string) (string, bool) {
	route, ok := c.people[phone]
	return route, ok
}

// GroupRoute returns the route of the group whose JID (e.g. "1203...@g.us")
// is chat.
func (c *Config) GroupRoute(chat string) (string, bool) {
	route, ok := c.groups[chat]
	return route, ok
}

// Rules returns the rules of the route named route, or false if no route
// has that name.
func (c *Config) Rules(route string) (*Rules, bool) {
	rules, ok := c.rules[route]
	return rules, ok
}

// Routes describes every route, sorted by name.
func (c *Config) Routes() []RouteInfo {
	return slices.Clone(c.routes)
}

// NormalizeKeyword is the form used to compare a message with a keyword:
// surrounding spaces removed, lower case.
func NormalizeKeyword(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Reply returns the reply for a message whose whole text matches a keyword.
func (r *Rules) Reply(text string) (keyword, reply string, ok bool) {
	keyword = NormalizeKeyword(text)
	reply, ok = r.keywords[keyword]
	return keyword, reply, ok
}

// DefaultReply is the reply to a text that matches no keyword. "" means the
// bot stays silent.
func (r *Rules) DefaultReply() string { return r.defaultReply }

// ImageAck is the confirmation template for images. "" means no confirmation.
func (r *Rules) ImageAck() string { return r.imageAck }

// DocumentAck is the confirmation template for documents. "" means no
// confirmation.
func (r *Rules) DocumentAck() string { return r.documentAck }

// DelayNotice is the text that goes before a late reply, in the same
// message. "" means late replies go without a notice.
func (r *Rules) DelayNotice() string { return r.delayNotice }

// file, fileRoute and fileRules mirror the JSON layout. A text field that is
// nil was left out of the file.
type file struct {
	Defaults *fileRules           `json:"defaults"`
	Routes   map[string]fileRoute `json:"routes"`
}

type fileRoute struct {
	People []string   `json:"people"`
	Groups []string   `json:"groups"`
	Rules  *fileRules `json:"rules"`
}

type fileRules struct {
	Keywords     map[string]string `json:"keywords"`
	DefaultReply *string           `json:"default_reply"`
	ImageAck     *string           `json:"image_ack"`
	DocumentAck  *string           `json:"document_ack"`
	DelayNotice  *string           `json:"delay_notice"`
}

// level is one level of rules, defaults or a route, as the file sets it:
// keywords normalized, texts trimmed, nil for a text left out.
type level struct {
	keywords     map[string]string
	defaultReply *string
	imageAck     *string
	documentAck  *string
	delayNotice  *string
}

var routeNameRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

// build validates the decoded file and applies the inheritance.
func build(raw file) (*Config, error) {
	defaults, err := parseLevel("defaults", raw.Defaults)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		rules:  make(map[string]*Rules, len(raw.Routes)),
		people: make(map[string]string),
		groups: make(map[string]string),
	}
	names := make([]string, 0, len(raw.Routes))
	for name := range raw.Routes {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		if !routeNameRE.MatchString(name) {
			return nil, fmt.Errorf("routes: %q is not a valid route name (lowercase letters, digits, - and _)", name)
		}
		route := raw.Routes[name]
		loc := "routes." + name

		for _, phone := range route.People {
			if !isPhone(phone) {
				return nil, fmt.Errorf("%s.people: %q is not a phone number (digits only, international format, e.g. 549...)", loc, phone)
			}
			if err := claim(cfg.people, phone, name, loc+".people"); err != nil {
				return nil, err
			}
		}
		for _, group := range route.Groups {
			if !strings.HasSuffix(group, "@g.us") || len(group) == len("@g.us") {
				return nil, fmt.Errorf("%s.groups: %q is not a group JID (expected ...@g.us)", loc, group)
			}
			if err := claim(cfg.groups, group, name, loc+".groups"); err != nil {
				return nil, err
			}
		}

		own, err := parseLevel(loc+".rules", route.Rules)
		if err != nil {
			return nil, err
		}
		rules, info := inherit(defaults, own)
		if len(rules.keywords) == 0 && rules.defaultReply == "" && rules.imageAck == "" && rules.documentAck == "" {
			return nil, fmt.Errorf("%s: the route has nothing to answer with: no keywords, default_reply, image_ack or document_ack, counting what it inherits from defaults", loc)
		}
		info.Name = name
		info.People = len(route.People)
		info.Groups = len(route.Groups)
		cfg.rules[name] = rules
		cfg.routes = append(cfg.routes, info)
	}
	return cfg, nil
}

// claim records that chat belongs to route, or fails if the configuration
// already has it.
func claim(chats map[string]string, chat, route, loc string) error {
	switch other, taken := chats[chat]; {
	case !taken:
		chats[chat] = route
		return nil
	case other == route:
		return fmt.Errorf("%s: %q is repeated", loc, chat)
	default:
		return fmt.Errorf("%s: %q is also in route %q (each chat goes in one route)", loc, chat, other)
	}
}

// parseLevel validates one level of rules. loc is where it is in the file,
// for the errors.
func parseLevel(loc string, raw *fileRules) (level, error) {
	if raw == nil {
		return level{}, nil
	}
	lv := level{
		keywords:     make(map[string]string, len(raw.Keywords)),
		defaultReply: trimmed(raw.DefaultReply),
		imageAck:     trimmed(raw.ImageAck),
		documentAck:  trimmed(raw.DocumentAck),
		delayNotice:  trimmed(raw.DelayNotice),
	}
	if err := checkPlaceholders(loc+".image_ack", lv.imageAck, ImagePlaceholders); err != nil {
		return level{}, err
	}
	if err := checkPlaceholders(loc+".document_ack", lv.documentAck, DocumentPlaceholders); err != nil {
		return level{}, err
	}

	// In file order would be nicer, but a map has none; sorted, the same file
	// always fails with the same error.
	keys := make([]string, 0, len(raw.Keywords))
	for keyword := range raw.Keywords {
		keys = append(keys, keyword)
	}
	slices.Sort(keys)
	for _, keyword := range keys {
		reply := raw.Keywords[keyword]
		norm := NormalizeKeyword(keyword)
		if norm == "" {
			return level{}, fmt.Errorf("%s.keywords: empty keyword", loc)
		}
		if strings.TrimSpace(reply) == "" {
			return level{}, fmt.Errorf("%s.keywords: %q has an empty reply", loc, keyword)
		}
		if _, dup := lv.keywords[norm]; dup {
			return level{}, fmt.Errorf("%s.keywords: %q is repeated (keywords ignore case and surrounding spaces)", loc, keyword)
		}
		lv.keywords[norm] = reply
	}
	return lv, nil
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

// inherit builds a route's rules from defaults and what the route sets.
func inherit(defaults, own level) (*Rules, RouteInfo) {
	rules := &Rules{keywords: make(map[string]string, len(defaults.keywords)+len(own.keywords))}
	for k, v := range defaults.keywords {
		rules.keywords[k] = v
	}
	for k, v := range own.keywords {
		rules.keywords[k] = v
	}
	info := RouteInfo{Keywords: len(rules.keywords), OwnKeywords: len(own.keywords)}
	rules.defaultReply, info.DefaultReply = pick(own.defaultReply, defaults.defaultReply)
	rules.imageAck, info.ImageAck = pick(own.imageAck, defaults.imageAck)
	rules.documentAck, info.DocumentAck = pick(own.documentAck, defaults.documentAck)
	rules.delayNotice, info.DelayNotice = pick(own.delayNotice, defaults.delayNotice)
	return rules, info
}

// pick returns a text field of a route and where it comes from: the route's
// own value if it sets one (even ""), otherwise the one in defaults.
func pick(own, inherited *string) (string, Source) {
	switch {
	case own != nil && *own != "":
		return *own, Own
	case own != nil:
		return "", Off
	case inherited != nil && *inherited != "":
		return *inherited, Inherited
	default:
		return "", Off
	}
}

var placeholderRE = regexp.MustCompile(`\{[^{}]*\}`)

// checkPlaceholders rejects a template that uses a placeholder its kind
// does not have, so a typo such as {mime} fails at startup.
func checkPlaceholders(loc string, tmpl *string, allowed []string) error {
	if tmpl == nil {
		return nil
	}
	for _, ph := range placeholderRE.FindAllString(*tmpl, -1) {
		if !slices.Contains(allowed, strings.Trim(ph, "{}")) {
			return fmt.Errorf("%s: unknown placeholder %s (allowed: {%s})", loc, ph, strings.Join(allowed, "}, {"))
		}
	}
	return nil
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

// The layout check. encoding/json accepts without a word several things
// that are mistakes in a configuration: a key repeated in an object (the
// last one wins), null (it reads as a field left out, which for a rule means
// inheriting it), and anything after the top-level object. Its errors for
// unknown fields and wrong types do not say where they are, either. check
// walks the document against the layout below before it is decoded, and its
// errors name where they are: routes.familia.rules.image_ack.

type kind int

const (
	kindText kind = iota
	kindTexts
	kindObject // fixed fields
	kindDict   // any key, every value of the same layout
)

type layout struct {
	kind   kind
	fields map[string]*layout // kindObject
	value  *layout            // kindDict
	// textField is set for the text fields of a level of rules, for the
	// message about null.
	textField bool
}

var (
	ruleTextLayout = &layout{kind: kindText, textField: true}
	rulesLayout    = &layout{kind: kindObject, fields: map[string]*layout{
		"keywords":      {kind: kindDict, value: &layout{kind: kindText}},
		"default_reply": ruleTextLayout,
		"image_ack":     ruleTextLayout,
		"document_ack":  ruleTextLayout,
		"delay_notice":  ruleTextLayout,
	}}
	routeLayout = &layout{kind: kindObject, fields: map[string]*layout{
		"people": {kind: kindTexts},
		"groups": {kind: kindTexts},
		"rules":  rulesLayout,
	}}
	documentLayout = &layout{kind: kindObject, fields: map[string]*layout{
		"defaults": rulesLayout,
		"routes":   {kind: kindDict, value: routeLayout},
	}}
)

func check(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err == io.EOF {
		return errors.New("the file is empty")
	}
	if err != nil {
		return err
	}
	if err := walk(dec, tok, documentLayout, ""); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("there is more after the configuration object")
	}
	return nil
}

// walk checks the value that starts with tok against l. path is where the
// value is in the document.
func walk(dec *json.Decoder, tok json.Token, l *layout, path string) error {
	if tok == nil {
		hint := "leave the field out instead"
		if l.textField {
			hint = `leave the field out to inherit it, or write "" to turn it off`
		}
		return fmt.Errorf("%s: null is not allowed (%s)", where(path), hint)
	}
	switch l.kind {
	case kindText:
		if _, ok := tok.(string); !ok {
			return fmt.Errorf("%s: must be a text", where(path))
		}
		return nil
	case kindTexts:
		if tok != json.Delim('[') {
			return fmt.Errorf("%s: must be a list of texts", where(path))
		}
		for i := 0; dec.More(); i++ {
			elem, err := next(dec)
			if err != nil {
				return err
			}
			if err := walk(dec, elem, &layout{kind: kindText}, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		_, err := next(dec)
		return err
	}

	if tok != json.Delim('{') {
		return fmt.Errorf("%s: must be an object", where(path))
	}
	seen := make(map[string]bool)
	for dec.More() {
		keyTok, err := next(dec)
		if err != nil {
			return err
		}
		key := keyTok.(string)
		if seen[key] {
			return fmt.Errorf("%s: %q is repeated", where(path), key)
		}
		seen[key] = true

		value := l.value
		if l.kind == kindObject {
			value = l.fields[key]
			if value == nil {
				return fmt.Errorf("%s: unknown field %q (allowed: %s)", where(path), key, fieldNames(l))
			}
		}
		valTok, err := next(dec)
		if err != nil {
			return err
		}
		if err := walk(dec, valTok, value, join(path, key)); err != nil {
			return err
		}
	}
	_, err := next(dec)
	return err
}

// next returns the next token; the end of the file in the middle of the
// document is an error.
func next(dec *json.Decoder) (json.Token, error) {
	tok, err := dec.Token()
	if err == io.EOF {
		return nil, io.ErrUnexpectedEOF
	}
	return tok, err
}

func where(path string) string {
	if path == "" {
		return "the configuration"
	}
	return path
}

var plainKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// join adds key to path: routes.familia, or keywords["buen día"] for a key
// that is not a plain word.
func join(path, key string) string {
	if !plainKeyRE.MatchString(key) {
		return path + "[" + strconv.Quote(key) + "]"
	}
	if path == "" {
		return key
	}
	return path + "." + key
}

func fieldNames(l *layout) string {
	names := make([]string, 0, len(l.fields))
	for name := range l.fields {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
