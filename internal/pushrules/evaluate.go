package pushrules

// Push rules are not a lookup table; they are a small pattern language whose whole purpose is to
// answer one question about one event for one person: is this worth interrupting them for, and is it
// worth more than ordinary attention?
//
// The language is deliberately dumb so that a person can reason about it from a settings screen. A
// rule states conditions and actions; the server walks the kinds in a fixed order and the rules
// within each kind by priority, and the *first* rule whose conditions all hold decides. Nothing is
// combined, nothing is scored, so "mute this person" cannot be quietly out-voted by a broader rule
// written later.
//
// This file is the evaluation half; pushrules.go is the storage half that lets a client see and edit
// the same data. Both matter: rules that cannot be read and edited are decoration, and rules that
// cannot be evaluated are a settings screen that lies.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Event is the view of a timeline event that the rule language can address.
//
// It is a struct rather than a stored row because the conditions are written against the *event*,
// including fields a rule may address that the storage layer keeps elsewhere (the sender's display
// name, for instance). Content is decoded rather than raw: every condition that looks inside content
// does so by a dotted path, and re-decoding per condition would be the same work done many times.
type Event struct {
	// RoomID is used by the conditions that need the room's own configuration.
	RoomID string
	// Type, Sender and StateKey are the event's own identity. StateKey is nil for a message.
	Type     string
	Sender   string
	StateKey *string
	// Content is the event's content object.
	Content map[string]any
	// SenderDisplayName is looked up for `contains_display_name`-style comparisons about the
	// *other* party; it is not needed by the default rules and may be empty.
	SenderDisplayName string
}

// Evaluation is what one event is worth to one person.
type Evaluation struct {
	// Notifies is true when the winning rule asks for the event to be brought to the person's
	// attention.
	Notifies bool
	// Highlights is true when that rule also asks for the special attention a mention gets.
	Highlights bool
	// RuleID names the rule that decided, for logs and for a client debugging its own rules.
	RuleID string
	// Matched reports that some rule matched at all. Master short-circuits with no actions, so
	// this is not the same as Notifies.
	Matched bool
}

// receiver is the person whose rules are being run, with the few things about them that the default
// rules need: their user ID (to be mentioned by) and their display name (to be named by).
type receiver struct {
	localpart   string
	userID      string
	displayName string
}

// Evaluate runs one account's rules over one event and returns the first rule's verdict.
//
// An event sent by the person themselves is never worth interrupting them for. That exclusion lives
// here rather than at the callers because every caller — a badge, a pusher, a "notify me" check —
// would otherwise have to remember it, and one that forgets tells the user their own words are
// waiting for them.
func (s *Service) Evaluate(ctx context.Context, localpart string, ev Event) (*Evaluation, error) {
	userID := "@" + localpart + ":" + s.serverName
	if ev.Sender == userID {
		return &Evaluation{}, nil
	}
	who, err := s.receiver(ctx, localpart)
	if err != nil {
		return nil, err
	}
	return s.evaluateWith(ctx, who, ev)
}

// Notifies is the badge-shaped answer to Evaluate: whether the event is worth a notification, and
// whether it is worth a highlight.
func (s *Service) Notifies(ctx context.Context, localpart string, ev Event) (notifies, highlights bool, err error) {
	outcome, err := s.Evaluate(ctx, localpart, ev)
	if err != nil {
		return false, false, err
	}
	return outcome.Notifies, outcome.Highlights, nil
}

func (s *Service) receiver(ctx context.Context, localpart string) (receiver, error) {
	who := receiver{localpart: localpart, userID: "@" + localpart + ":" + s.serverName}
	user, err := s.store.Users.Get(ctx, localpart)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// An account with no user row has no display name to be named by; its other
			// rules still apply.
			return who, nil
		}
		return who, err
	}
	who.displayName = user.DisplayName
	return who, nil
}

func (s *Service) evaluateWith(ctx context.Context, who receiver, ev Event) (*Evaluation, error) {
	byKind, err := s.Rules(ctx, who.localpart)
	if err != nil {
		return nil, err
	}
	for _, kind := range kindOrder {
		for _, rule := range byKind[string(kind)] {
			if !rule.Enabled {
				// A rule the person switched off is not a rule; it is a note about
				// something they do not want to hear about.
				continue
			}
			if rule.RuleID == RuleMaster {
				// Master is the switch that says "stop asking". It has no conditions,
				// and while it is enabled no event notifies at all.
				return &Evaluation{Matched: true, RuleID: rule.RuleID}, nil
			}
			matched, err := s.matches(ctx, who, rule, ev)
			if err != nil {
				return nil, err
			}
			if !matched {
				continue
			}
			notifies, highlights := readActions(rule.Actions)
			return &Evaluation{
				Notifies: notifies, Highlights: highlights, RuleID: rule.RuleID, Matched: true,
			}, nil
		}
	}
	// Nothing matched. Silence is the answer: a server that invented a notification here would be
	// deciding, on the user's behalf, that something unnamed was worth their attention.
	return &Evaluation{}, nil
}

// readActions interprets a rule's actions.
//
// Actions are a list mixing bare strings (`notify`, `dont_notify`, `coalesce`) with objects
// (`{"set_tweak":"highlight","value":true}` and, since Matrix 1.13, the combined
// `{"notify":true,"set_tweak":"highlight"}`). `dont_notify` wins outright because it is the action a
// person chooses when they want a rule to mean "nothing".
func readActions(actions []any) (notifies, highlights bool) {
	for _, action := range actions {
		switch value := action.(type) {
		case string:
			switch value {
			case "notify":
				notifies = true
			case "dont_notify":
				return false, false
			}
		case map[string]any:
			if notify, ok := value["notify"].(bool); ok {
				// The Matrix 1.13 combined form: the object says both whether to
				// notify and what kind of attention it is.
				if !notify {
					return false, false
				}
				notifies = true
			}
			if tweak, _ := value["set_tweak"].(string); tweak == "highlight" {
				notifies = true
				highlights = tweakIsOn(value["value"])
			}
		}
	}
	return notifies, highlights
}

// tweakIsOn reads a `set_tweak` value. An absent value means "on": a highlight tweak that names no
// value is a client saying "this one matters", not "this one is worth nothing".
func tweakIsOn(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return typed
	case string:
		return typed != "" && typed != "false"
	default:
		return true
	}
}

// matches reports whether every condition of a rule holds. A rule with no conditions matches
// everything, which is why the default rules that should be narrow carry conditions and the
// catch-all is a rule a person can see and turn off.
func (s *Service) matches(ctx context.Context, who receiver, rule Rule, ev Event) (bool, error) {
	if rule.Kind == KindContent {
		// A content rule is the shorthand form: one pattern, matched against content.body. It is
		// stored as a pattern rather than a condition because that is the shape the API exposes.
		if rule.Pattern == "" {
			return false, nil
		}
		return matchGlob(bodyOf(ev), rule.Pattern), nil
	}
	if len(rule.Conditions) == 0 {
		return true, nil
	}
	for _, raw := range rule.Conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return false, nil
		}
		met, err := s.conditionHolds(ctx, who, condition, ev)
		if err != nil {
			return false, err
		}
		if !met {
			return false, nil
		}
	}
	return true, nil
}

// conditionHolds evaluates one condition of one rule.
func (s *Service) conditionHolds(ctx context.Context, who receiver, condition map[string]any, ev Event) (bool, error) {
	kind, _ := condition["kind"].(string)
	switch kind {
	case "event_match":
		key, _ := condition["key"].(string)
		value, found := lookup(ev, key)
		if !found {
			return false, nil
		}
		pattern, hasPattern := condition["pattern"].(string)
		if !hasPattern || pattern == "" {
			// No pattern means "the key exists", which is how the tombstone rule asks
			// "did this room replace another one" without caring which.
			return true, nil
		}
		return matchGlob(value, pattern), nil

	case "contains_display_name":
		// Being named in words is a different class of interruption than being mentioned in
		// markup, so this matches the person's own display name as a word in the body.
		if who.displayName == "" {
			return false, nil
		}
		return matchGlob(bodyOf(ev), who.displayName), nil

	case "room_member_count":
		is, _ := condition["is"].(string)
		count, err := s.memberCount(ctx, ev.RoomID)
		if err != nil {
			return false, err
		}
		return compareCount(count, is), nil

	case "sender_notification_permission":
		key, _ := condition["key"].(string)
		return s.senderMayNotify(ctx, key, ev)

	case "member_count", "notification_content":
		// Both of these ask the server for figures it only has when identity-server
		// integration and MSC3847 counters exist, which this server does not do. They are
		// treated as satisfied rather than failed because they appear in *this server's own*
		// default rules, where the rule author is the server and the narrower reading would
		// silently disable a rule the user can see and expect to work. A condition invented by
		// a client is a different matter, and falls through to the default below.
		return true, nil

	default:
		// A condition this server cannot read is a condition it cannot confirm, so the rule
		// does not fire. Firing on incomprehension would mean a rule written for a newer
		// server silently interrupts this server's users instead.
		return false, nil
	}
}

// bodyOf reads content.body, which almost every textual condition is about.
func bodyOf(ev Event) string {
	value, _ := lookup(ev, "content.body")
	return value
}

// lookup resolves a dotted path against the event. The specification's keys are of the form
// `content.body`, `type`, `sender`, `state_key`, and `content.m\.relates_to.rel_type`, where a
// backslash escapes a literal dot inside a key name.
func lookup(ev Event, path string) (string, bool) {
	segments := splitPath(path)
	if len(segments) == 0 {
		return "", false
	}
	head := segments[0]
	rest := segments[1:]

	switch head {
	case "type":
		return ev.Type, ev.Type != ""
	case "sender":
		return ev.Sender, ev.Sender != ""
	case "state_key":
		if ev.StateKey == nil {
			return "", false
		}
		return *ev.StateKey, true
	case "room_id":
		return ev.RoomID, ev.RoomID != ""
	case "content":
		return lookupContent(ev.Content, rest)
	default:
		// Older clients write conditions against the event root, e.g. `content` alone.
		return lookupContent(ev.Content, segments)
	}
}

func lookupContent(content map[string]any, path []string) (string, bool) {
	var current any = content
	for _, segment := range path {
		asMap, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = asMap[segment]
		if !ok {
			return "", false
		}
	}
	switch typed := current.(type) {
	case []any:
		// An array is read as its elements joined, which is what makes a rule about
		// `content.m\.mentions.user_ids` work: the mention list is an array, and the pattern
		// is one user ID. The word-boundary matching around a pattern keeps "@bob:hs" from
		// matching inside "@bob:hs2".
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				parts = append(parts, text)
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, " "), true
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case nil:
		return "", false
	default:
		// A condition against an object or array is asking whether it is there, which the
		// caller reads off the `found` flag.
		return "<present>", true
	}
}

// splitPath splits a condition key on unescaped dots.
func splitPath(path string) []string {
	var segments []string
	var current strings.Builder
	for index := 0; index < len(path); index++ {
		switch character := path[index]; {
		case character == '\\' && index+1 < len(path):
			index++
			current.WriteByte(path[index])
		case character == '.':
			segments = append(segments, current.String())
			current.Reset()
		default:
			current.WriteByte(character)
		}
	}
	segments = append(segments, current.String())
	return segments
}

// matchGlob reports whether a value matches a rule pattern.
//
// Patterns are not regular expressions; they are the small glob the specification defines, plus the
// two things that make a rule about words behave like a person expects. A pattern containing `\w` is
// anchored at those markers to word boundaries; a pattern used against a body without any `\w` is
// looked for *inside* the text, because "alice" is meant to match "hi alice, are you there" and a
// rule that only matched the whole body would never fire in a conversation.
func matchGlob(value, pattern string) bool {
	if value == "" {
		return false
	}
	expression := globPattern(value, pattern)
	matched, err := regexp.MatchString(expression, value)
	if err != nil {
		// A pattern this server cannot compile has already been rejected on the way in for
		// the default rules; for a client-supplied one the safe answer is "no match".
		return false
	}
	return matched
}

func globPattern(value, pattern string) string {
	var out strings.Builder
	out.WriteString("(?i)")
	if strings.Contains(pattern, `\w`) {
		// `\w` is the specification's word-boundary marker, used by the default rules so that
		// a user named "ali" is not highlighted by "algorithm".
		out.WriteString(strings.ReplaceAll(regexp.QuoteMeta(
			strings.ReplaceAll(pattern, `\w`, "\x00")), "\x00", `\b`))
		return out.String()
	}
	out.WriteString("(?:^|[^\\p{L}\\p{N}_])(?:")
	out.WriteString(globToRegex(pattern))
	out.WriteString(")(?:$|[^\\p{L}\\p{N}_])")
	return out.String()
}

// globToRegex translates the glob metacharacters, leaving everything else literal.
func globToRegex(pattern string) string {
	var out strings.Builder
	for index := 0; index < len(pattern); index++ {
		switch character := pattern[index]; character {
		case '*':
			out.WriteString(".*")
		case '?':
			out.WriteString(".")
		default:
			out.WriteString(regexp.QuoteMeta(string(character)))
		}
	}
	return out.String()
}

// compareCount reads the `is` comparator of a room_member_count condition: an exact number, a
// `<`/`>` comparison, or a `a..b` range (with either end open).
func compareCount(count int64, comparator string) bool {
	comparator = strings.TrimSpace(comparator)
	if comparator == "" {
		return false
	}
	if low, high, ok := strings.Cut(comparator, ".."); ok {
		lower, lowerOK := parseCount(low)
		upper, upperOK := parseCount(high)
		switch {
		case lowerOK && upperOK:
			return count >= lower && count <= upper
		case lowerOK:
			return count >= lower
		case upperOK:
			return count <= upper
		default:
			return false
		}
	}
	for _, prefix := range []struct {
		token string
		test  func(count, bound int64) bool
	}{{">=", func(a, b int64) bool { return a >= b }},
		{"<=", func(a, b int64) bool { return a <= b }},
		{">", func(a, b int64) bool { return a > b }},
		{"<", func(a, b int64) bool { return a < b }}} {
		if rest, found := strings.CutPrefix(comparator, prefix.token); found {
			bound, err := parseCountOnly(rest)
			return err == nil && prefix.test(count, bound)
		}
	}
	bound, err := parseCountOnly(comparator)
	return err == nil && count == bound
}

func parseCount(raw string) (int64, bool) {
	value, err := parseCountOnly(raw)
	return value, err == nil
}

func parseCountOnly(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, errors.New("empty count")
	}
	return strconv.ParseInt(trimmed, 10, 64)
}

// senderMayNotify answers `sender_notification_permission`: does the sender hold the power level the
// room requires for this kind of notification (typically `room`, for `@room` broadcasts)?
func (s *Service) senderMayNotify(ctx context.Context, key string, ev Event) (bool, error) {
	if key == "" {
		key = "room"
	}
	level, err := s.senderPowerLevel(ctx, ev)
	if err != nil {
		return false, err
	}
	required, err := s.notificationPowerLevel(ctx, ev.RoomID, key)
	if err != nil {
		return false, err
	}
	return level >= required, nil
}

func (s *Service) senderPowerLevel(ctx context.Context, ev Event) (int64, error) {
	levels, err := s.powerLevels(ctx, ev.RoomID)
	if err != nil {
		return 0, err
	}
	users := map[string]any{}
	if raw, ok := levels["users"].(map[string]any); ok {
		users = raw
	}
	if level, ok := users[ev.Sender]; ok {
		return asInt(level), nil
	}
	return asInt(levels["users_default"]), nil
}

func (s *Service) notificationPowerLevel(ctx context.Context, roomID, key string) (int64, error) {
	levels, err := s.powerLevels(ctx, roomID)
	if err != nil {
		return 0, err
	}
	notifications, _ := levels["notifications"].(map[string]any)
	if notifications == nil {
		// With no stated requirement, `@room` belongs to whoever may change the power levels
		// of the room — which is how the specification reads it.
		return asInt(levels["events_default"]) + 1, nil
	}
	if level, ok := notifications[key]; ok {
		return asInt(level), nil
	}
	return asInt(levels["events_default"]) + 1, nil
}

func (s *Service) powerLevels(ctx context.Context, roomID string) (map[string]any, error) {
	if roomID == "" {
		return map[string]any{}, nil
	}
	row, err := s.store.State.Get(ctx, roomID, "m.room.power_levels", "")
	if errors.Is(err, store.ErrNotFound) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	event, err := s.store.Events.ByEventID(ctx, row.EventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var content map[string]any
	if err := json.Unmarshal([]byte(event.JSON), &content); err != nil {
		return nil, fmt.Errorf("reading the power levels of %s: %w", roomID, err)
	}
	// The event's content is nested under `content` in the stored event JSON.
	if inner, ok := content["content"].(map[string]any); ok {
		return inner, nil
	}
	return content, nil
}

func (s *Service) memberCount(ctx context.Context, roomID string) (int64, error) {
	if roomID == "" {
		return 0, nil
	}
	members, err := s.store.Memberships.List(ctx, roomID, []string{"join"}, nil)
	if err != nil {
		return 0, err
	}
	return int64(len(members)), nil
}

func asInt(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// EventFromRow builds the rule language's view of a stored event. It is exported because the badge
// counter, the pusher, and anything else that asks "was this worth telling somebody about" all need
// the same decoding of the same rows.
func EventFromRow(row *store.EventRow) (Event, error) {
	ev := Event{Type: row.Type, Sender: row.Sender, RoomID: row.RoomID}
	if row.StateKey != nil {
		key := *row.StateKey
		ev.StateKey = &key
	}
	if len(row.JSON) == 0 {
		return ev, nil
	}
	var event struct {
		Content  json.RawMessage `json:"content"`
		Unsigned json.RawMessage `json:"unsigned"`
	}
	if err := json.Unmarshal([]byte(row.JSON), &event); err != nil {
		return ev, fmt.Errorf("decoding %s: %w", row.EventID, err)
	}
	if len(event.Content) > 0 {
		if err := json.Unmarshal(event.Content, &ev.Content); err != nil {
			return ev, fmt.Errorf("decoding the content of %s: %w", row.EventID, err)
		}
	}
	if ev.Content == nil {
		ev.Content = map[string]any{}
	}
	return ev, nil
}

// RelType reports the relation an event declares, which is how a thread reply and an edit are told
// apart from an ordinary message.
func (ev Event) RelType() string {
	relates, _ := ev.Content["m.relates_to"].(map[string]any)
	if relates == nil {
		return ""
	}
	relType, _ := relates["rel_type"].(string)
	return relType
}

// ThreadRoot is the root event of the thread this event replies to, or "" when it is not a thread
// reply. `m.thread` relations name the thread by its root event ID.
func (ev Event) ThreadRoot() string {
	if ev.RelType() != "m.thread" {
		return ""
	}
	relates, _ := ev.Content["m.relates_to"].(map[string]any)
	root, _ := relates["event_id"].(string)
	return root
}
