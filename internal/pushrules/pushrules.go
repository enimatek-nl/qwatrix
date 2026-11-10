// Package pushrules holds the per-account notification rules.
//
// Push rules are the one piece of user configuration that decides whether a message reaches a
// person at all, so the server cannot treat them as opaque: it has to know the shape of a rule to
// order kinds correctly, and it has to ship the defaults that make invitations and mentions notify,
// since an account with no rules would silently receive nothing.
//
// The rules are also delivered as `m.push_rules` account data in /sync, which is how a client that
// has just come back online learns what it should be notifying for. That means a rule change has to
// be visible to an in-flight sync, the same way a receipt is — see [Service.touch].
package pushrules

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Scope is the only scope the specification defines.
const Scope = "global"

// EventType is the account-data type a client reads the rules from.
const EventType = "m.push_rules"

// Kind names one of the rule classes. The order matters: kinds are evaluated in this order, and a
// matching override rule stops evaluation entirely.
type Kind string

const (
	KindOverride  Kind = "override"
	KindContent   Kind = "content"
	KindRoom      Kind = "room"
	KindSender    Kind = "sender"
	KindUnderride Kind = "underride"
)

// RuleMaster is the switch a person uses to stop all notifications. It carries no conditions: while
// it is enabled, nothing notifies, and a rule that could be narrowed would be a different feature.
const RuleMaster = ".m.rule.master"

// RuleIsUserMention is the override rule that makes a mention worth a highlight. It is first among
// the override rules on purpose: a mention inside a notice, or inside anything else a person has
// chosen to dim, is still somebody asking them specifically.
const RuleIsUserMention = ".m.rule.is_user_mention"

// kindOrder is the evaluation order, and so the display order in a response.
var kindOrder = []Kind{KindOverride, KindContent, KindRoom, KindSender, KindUnderride}

// ValidKind reports whether kind is one the specification knows.
func ValidKind(kind string) bool {
	for _, candidate := range kindOrder {
		if string(candidate) == kind {
			return true
		}
	}
	return false
}

// Rule is the wire form of a push rule.
type Rule struct {
	RuleID     string `json:"rule_id"`
	Kind       Kind   `json:"kind"`
	Default    bool   `json:"default"`
	Enabled    bool   `json:"enabled"`
	Conditions []any  `json:"conditions,omitempty"`
	Pattern    string `json:"pattern,omitempty"`
	Actions    []any  `json:"actions"`
}

// Service is the push-rule service.
type Service struct {
	store      *store.Store
	notifier   *notifier.Notifier
	serverName string
}

// Options configures a [Service].
type Options struct {
	// ServerName builds the user ID the default invite rule matches.
	ServerName string
}

// New builds the push-rule service.
func New(st *store.Store, n *notifier.Notifier, opts Options) *Service {
	return &Service{store: st, notifier: n, serverName: opts.ServerName}
}

// ensureSeeded writes an account's default rules the first time anything touches them.
//
// Seeding has to happen before a rule is looked up, not merely before one is listed, or the very
// first request an account makes — "overwrite .m.rule.master" — finds no rule to protect and happily
// creates a server-owned one. The defaults are also what makes an untouched account notify at all, so
// they cannot be written lazily only on the read path.
func (s *Service) ensureSeeded(ctx context.Context, localpart string) error {
	// The reconciliation reads tombstones too, and has to: an account that removed every default
	// it was given has no rules to list, and seeding it from scratch would push back every rule
	// that person deleted.
	rows, err := s.store.PushRules.ListIncludingRemoved(ctx, localpart, Scope)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return s.seed(ctx, localpart)
	}
	return s.backfillDefaults(ctx, localpart, rows)
}

// backfillDefaults writes the default rules an account is missing.
//
// Without this, a server upgrade that adds a default — a new kind of mention, say — would apply to
// accounts created afterwards and silently not to everybody else's, which is the kind of difference
// a person cannot see or explain. Defaults the account already has are left alone, including one it
// has switched off: `default` means "the server wrote this", not "the server may rewrite this".
func (s *Service) backfillDefaults(ctx context.Context, localpart string, rows []store.PushRule) error {
	present := make(map[string]store.PushRule, len(rows))
	for _, row := range rows {
		present[string(row.Kind)+"/"+row.RuleID] = row
	}
	changed := false
	for index, rule := range s.defaultRules(localpart) {
		existing, found := present[string(rule.kind)+"/"+rule.ruleID]
		if found {
			// One repair, for a server that once shipped `.m.rule.master` with a
			// condition on it. Master with a condition is not master: it would
			// selectively mute the message types it names while claiming to be the
			// all-or-nothing switch the client renders as a single toggle.
			if rule.ruleID == RuleMaster && existing.Conditions != "" {
				existing.Conditions = ""
				existing.Actions = "[]"
				if err := s.store.PushRules.Put(ctx, &existing); err != nil {
					return err
				}
				changed = true
			}
			continue
		}
		row := rule.row(localpart, priorityFor(index))
		if err := s.store.PushRules.Put(ctx, &row); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		// Bumping the position on every request would wake every client's /sync forever: the
		// rules are only worth delivering again when they actually changed.
		return nil
	}
	return s.touch(ctx, localpart)
}

// Rules returns an account's rules, seeded with the defaults on first use, grouped by kind in the
// order the kinds are evaluated. Every kind is present, even when empty: a client that has to tell
// "no room rules" from "no such key" is a client doing the server's job.
func (s *Service) Rules(ctx context.Context, localpart string) (map[string][]Rule, error) {
	if err := s.ensureSeeded(ctx, localpart); err != nil {
		return nil, err
	}
	rows, err := s.store.PushRules.List(ctx, localpart, Scope)
	if err != nil {
		return nil, err
	}
	grouped := map[string][]Rule{}
	for _, kind := range kindOrder {
		grouped[string(kind)] = []Rule{}
	}
	for _, row := range rows {
		rule, err := decode(row)
		if err != nil {
			slog.Warn("push rule is unreadable and was skipped", "localpart", localpart,
				"rule_id", row.RuleID, "error", err)
			continue
		}
		grouped[string(rule.Kind)] = append(grouped[string(rule.Kind)], rule)
	}
	return grouped, nil
}

// Rule returns one rule.
func (s *Service) Rule(ctx context.Context, localpart, kind, ruleID string) (*Rule, error) {
	if err := s.ensureSeeded(ctx, localpart); err != nil {
		return nil, err
	}
	// Reading a rule under a kind this server has never heard of is answered as "no such rule",
	// the same as the enable and actions sub-resources: the answer is true either way, and a client
	// that probes for rules a particular server may not ship treats 404 as the ordinary negative
	// and 400 as a server error. Arguing about the client's vocabulary is the one outcome worse
	// than admitting the rule is not here.
	if !ValidKind(kind) {
		return nil, matrix.NewNotFound("push rule %q does not exist", ruleID)
	}
	row, err := s.store.PushRules.Get(ctx, localpart, Scope, kind, ruleID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewNotFound("push rule %q does not exist", ruleID)
		}
		return nil, err
	}
	rule, err := decode(*row)
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// SetActions creates or replaces a rule's actions, which is how a rule is created at all: a
// client sends `{"actions": [...]}` and the rule springs into existence with them.
func (s *Service) SetActions(ctx context.Context, localpart, kind, ruleID string, actions []any) error {
	if err := s.ensureSeeded(ctx, localpart); err != nil {
		return err
	}
	if !ValidKind(kind) {
		return matrix.NewInvalidParam("unknown push rule kind %q", kind)
	}
	if actions == nil {
		return matrix.NewMissingParam("actions")
	}
	if err := s.guardDefault(ctx, localpart, kind, ruleID); err != nil {
		return err
	}
	encoded, err := json.Marshal(actions)
	if err != nil {
		return matrix.NewBadJSON(err.Error())
	}
	row, err := s.store.PushRules.Get(ctx, localpart, Scope, kind, ruleID)
	if err != nil {
		if !store.IsNotFound(err) {
			return err
		}
		row = &store.PushRule{Localpart: localpart, Scope: Scope, Kind: kind, RuleID: ruleID,
			Enabled: true, Priority: int64(len(kindOrder) * 1000)}
	}
	row.Actions = string(encoded)
	row.Removed = false
	if err := s.store.PushRules.Put(ctx, row); err != nil {
		return err
	}
	return s.changed(ctx, localpart)
}

// SetEnabled turns a rule on or off. A default rule cannot be deleted but can always be disabled,
// which is the whole reason the defaults are stored rather than computed: "off" has to be a fact
// about the account, not a re-derivation at read time.
func (s *Service) SetEnabled(ctx context.Context, localpart, kind, ruleID string, enabled bool) error {
	if err := s.ensureSeeded(ctx, localpart); err != nil {
		return err
	}
	// A kind this server has no notion of has no rules to enable or disable, and "there is no such
	// rule" is the true answer. Refusing with "bad request" would be arguing about a client's
	// vocabulary, and clients that probe for rules a particular server may not ship treat 404 as
	// the ordinary negative.
	if !ValidKind(kind) {
		return matrix.NewNotFound("push rule %q does not exist", ruleID)
	}
	row, err := s.store.PushRules.Get(ctx, localpart, Scope, kind, ruleID)
	if err != nil {
		if !store.IsNotFound(err) {
			return err
		}
		return matrix.NewNotFound("push rule %q does not exist", ruleID)
	}
	row.Enabled = enabled
	if err := s.store.PushRules.Put(ctx, row); err != nil {
		return err
	}
	return s.changed(ctx, localpart)
}

// Delete removes a rule. A default rule is marked deleted rather than removed, so the next seed
// does not hand it back to an account that got rid of it.
func (s *Service) Delete(ctx context.Context, localpart, kind, ruleID string) error {
	if err := s.ensureSeeded(ctx, localpart); err != nil {
		return err
	}
	if !ValidKind(kind) {
		return matrix.NewInvalidParam("unknown push rule kind %q", kind)
	}
	row, err := s.store.PushRules.Get(ctx, localpart, Scope, kind, ruleID)
	if err != nil {
		if !store.IsNotFound(err) {
			return err
		}
		return matrix.NewNotFound("push rule %q does not exist", ruleID)
	}
	if !row.Default {
		if err := s.store.PushRules.Delete(ctx, localpart, Scope, kind, ruleID); err != nil {
			return err
		}
	} else {
		// The rule stays on the shelf and is marked gone. Dropping the row would look like the
		// same thing today and a different thing tomorrow: the defaults are re-derived for an
		// account that appears to have none, so a deleted default would be quietly restored and
		// the user would get back a rule they threw away.
		row.Removed = true
		row.Enabled = false
		if err := s.store.PushRules.Put(ctx, row); err != nil {
			return err
		}
	}
	return s.changed(ctx, localpart)
}

// AccountData renders the `m.push_rules` document a client expects in its account data.
func (s *Service) AccountData(ctx context.Context, localpart string) (json.RawMessage, error) {
	grouped, err := s.Rules(ctx, localpart)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"global": grouped})
}

// LastPosition reports when the account's rules last changed, for the /sync delta.
func (s *Service) LastPosition(ctx context.Context, localpart string) (int64, error) {
	return s.store.PushRules.LastPosition(ctx, localpart, Scope)
}

// seed writes the default rules an account starts with.
//
// These are the rules the specification names, and they are what makes a fresh account usable:
// invitations and mentions notify, notices and edits do not, and everything else falls through to
// the underride rules. A server that shipped no defaults would answer a client's rule screen with
// an empty list and quietly never notify anybody.
// defaultRule is one of the rules this server ships for every account.
type defaultRule struct {
	kind       Kind
	ruleID     string
	enabled    bool
	actions    []any
	conditions []any
	// pattern is only used by a content rule, the one kind written as a bare glob.
	pattern string
}

func (d defaultRule) row(localpart string, priority int64) store.PushRule {
	return store.PushRule{
		Localpart: localpart, Scope: Scope, Kind: string(d.kind), RuleID: d.ruleID,
		Default: true, Enabled: d.enabled, Priority: priority,
		Actions:    string(mustJSON(d.actions)),
		Conditions: string(mustJSON(d.conditions)),
		Pattern:    d.pattern,
	}
}

// match is one `event_match` condition: a dotted key and the glob a value has to fit.
func match(key, pattern string) map[string]any {
	return map[string]any{"kind": "event_match", "key": key, "pattern": pattern}
}

// sound and highlight are the two refinements a notification can carry: a noise to make, and the
// special attention of a mention. They are written next to a `notify` because an action list that
// names neither is a rule that says "notice this" without saying what to do.
var (
	sound     = map[string]any{"set_tweak": "sound", "value": "default"}
	highlight = map[string]any{"set_tweak": "highlight"}
)

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		// The values that go through here are literals in this file, so a failure is a bug in
		// the rule set rather than anything a client or a database could cause.
		panic("pushrules: encoding a default rule: " + err.Error())
	}
	return raw
}

// defaultRules builds the shipped rule set for one account.
//
// It is per account because two of the defaults are about that account specifically: the invite rule
// matches the account's own user ID, and so does the rule that turns a mention into a highlight. They
// are stored rather than re-derived at read time because a rule a client can see and edit has to be
// data, or an edit to it would vanish on the next restart.
//
// The order is the evaluation order, so it is worth reading as a set of decisions:
//
//   - a mention outranks everything that dims things, so being asked specifically still reaches a
//     person who has muted a room's bots;
//   - an invitation is worth a noise, a membership change is not;
//   - notices are dimmed because they are what a bot says when it does not want an answer;
//   - reactions are dimmed because acknowledging somebody's message is not itself news;
//   - and `.m.rule.fallback` catches everything else that reaches a person's devices: this server
//     tells them when something they can see has been said.
func (s *Service) defaultRules(localpart string) []defaultRule {
	userID := "@" + localpart + ":" + s.serverName
	return []defaultRule{
		{kind: KindOverride, ruleID: RuleIsUserMention, enabled: true,
			actions: []any{map[string]any{"notify": true, "set_tweak": "highlight"}},
			conditions: []any{
				map[string]any{"kind": "event_match", "key": `content.m\.mentions.user_ids`,
					"pattern": userID}}},
		{kind: KindOverride, ruleID: RuleMaster, enabled: false, actions: []any{}},
		{kind: KindOverride, ruleID: ".m.rule.suppress_notices", enabled: true,
			actions:    []any{"dont_notify"},
			conditions: []any{match("content.msgtype", "m.notice")}},
		{kind: KindOverride, ruleID: ".m.rule.invite_for_me", enabled: true,
			actions: []any{"notify", sound, highlight},
			conditions: []any{
				match("type", "m.room.member"),
				match("content.membership", "invite"),
				match("state_key", userID)}},
		{kind: KindOverride, ruleID: ".m.rule.member_event", enabled: true,
			actions:    []any{"dont_notify"},
			conditions: []any{match("type", "m.room.member")}},
		{kind: KindOverride, ruleID: ".m.rule.contains_display_name", enabled: true,
			actions:    []any{"notify", sound, highlight},
			conditions: []any{map[string]any{"kind": "contains_display_name"}}},
		{kind: KindOverride, ruleID: ".m.rule.roomnotif", enabled: true,
			actions: []any{"notify", sound, highlight},
			conditions: []any{
				match("content.body", "@room"),
				map[string]any{"kind": "sender_notification_permission", "key": "room"}}},
		{kind: KindOverride, ruleID: ".m.rule.tombstone", enabled: true,
			actions: []any{"dont_notify"},
			conditions: []any{
				match("type", "m.room.create"),
				map[string]any{"kind": "event_match", "key": "content.predecessor.event_id"}}},
		{kind: KindOverride, ruleID: ".m.rule.reaction", enabled: true,
			actions:    []any{"dont_notify"},
			conditions: []any{match("type", "m.reaction")}},
		{kind: KindContent, ruleID: ".m.rule.contains_user_name", enabled: true,
			actions: []any{"notify", sound, highlight},
			pattern: localpart},
		{kind: KindUnderride, ruleID: ".m.rule.call", enabled: true,
			actions:    []any{"notify", sound, highlight},
			conditions: []any{match("type", "m.call.invite")}},
		{kind: KindUnderride, ruleID: ".m.rule.encryption_encrypted_room_message", enabled: true,
			actions:    []any{"notify"},
			conditions: []any{match("type", "m.room.encrypted")}},
		{kind: KindUnderride, ruleID: ".m.rule.fallback", enabled: true,
			actions: []any{"notify"}},
	}
}

// priorityFor orders the defaults inside their kind. Gaps of 100 leave room for a client to move one
// of its own rules between two defaults without renumbering anything.
func priorityFor(index int) int64 { return int64((index + 1) * 100) }

// seed writes a new account's defaults, and counts as a change so that the rules reach the client
// that has never seen them.
func (s *Service) seed(ctx context.Context, localpart string) error {
	for index, rule := range s.defaultRules(localpart) {
		row := rule.row(localpart, priorityFor(index))
		if err := s.store.PushRules.Put(ctx, &row); err != nil {
			return err
		}
	}
	// Seeding counts as a change. An account that has never had its rules read has no position,
	// and without one there is nothing for a /sync to compare against — the defaults would exist
	// but never be delivered.
	return s.touch(ctx, localpart)
}

func (s *Service) changed(ctx context.Context, localpart string) error {
	if err := s.touch(ctx, localpart); err != nil {
		return err
	}
	if s.notifier != nil {
		s.notifier.Publish()
	}
	return nil
}

// touch records "the rules moved, as of now".
func (s *Service) touch(ctx context.Context, localpart string) error {
	position, err := s.store.Events.MaxPosition(ctx)
	if err != nil {
		return err
	}
	// Rules are account data, and account data is emitted when its position is past the client's
	// token — but a change made while a sync is in flight happens *at* the tip, so the stamp has
	// to be one past it. Anything else answers a client that is waiting with "nothing happened"
	// for a change it caused itself.
	return s.store.PushRules.Touch(ctx, localpart, Scope, position+1)
}

// guardDefault refuses to rewrite a rule the server owns.
//
// The defaults are the reason a fresh account notifies for invitations and mentions at all. Letting
// a client overwrite their conditions would mean the set of "rules I never configured but that
// decide whether my phone buzzes" is editable into something that silently notifies for nothing —
// and the client would have no way to get back what it removed. Enabling and disabling stays open,
// which is the operation people actually want.
func (s *Service) guardDefault(ctx context.Context, localpart, kind, ruleID string) error {
	row, err := s.store.PushRules.Get(ctx, localpart, Scope, kind, ruleID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil // A new rule is the caller's to define.
		}
		return err
	}
	if row.Default {
		return matrix.NewInvalidParam("push rule %q is a default rule and cannot be modified; it can be enabled, disabled, or deleted", ruleID)
	}
	return nil
}

// decode turns a stored row into the wire form.
func decode(row store.PushRule) (Rule, error) {
	rule := Rule{
		RuleID:  row.RuleID,
		Kind:    Kind(row.Kind),
		Default: row.Default,
		Enabled: row.Enabled,
		Pattern: row.Pattern,
		Actions: []any{},
	}
	if row.Actions != "" {
		if err := json.Unmarshal([]byte(row.Actions), &rule.Actions); err != nil {
			return rule, fmt.Errorf("actions of push rule %q: %w", row.RuleID, err)
		}
	}
	if row.Conditions != "" {
		if err := json.Unmarshal([]byte(row.Conditions), &rule.Conditions); err != nil {
			return rule, fmt.Errorf("conditions of push rule %q: %w", row.RuleID, err)
		}
	}
	return rule, nil
}

// SetPattern stores a content rule's match string. A content rule is defined by what it looks for,
// so this is the rule's substance rather than metadata: without it the rule matches nothing and the
// client that set it believes it set something.
func (s *Service) SetPattern(ctx context.Context, localpart, kind, ruleID, pattern string) error {
	if !ValidKind(kind) {
		return matrix.NewInvalidParam("unknown push rule kind %q", kind)
	}
	if err := s.guardDefault(ctx, localpart, kind, ruleID); err != nil {
		return err
	}
	row, err := s.store.PushRules.Get(ctx, localpart, Scope, kind, ruleID)
	if err != nil {
		if !store.IsNotFound(err) {
			return err
		}
		row = &store.PushRule{Localpart: localpart, Scope: Scope, Kind: kind, RuleID: ruleID,
			Enabled: true, Actions: "[]", Priority: int64(len(kindOrder) * 1000)}
	}
	row.Pattern = pattern
	row.Removed = false
	if err := s.store.PushRules.Put(ctx, row); err != nil {
		return err
	}
	return s.changed(ctx, localpart)
}

// Move places a rule before or after another one of the same kind.
//
// Order is the whole meaning of a rule list — the first match wins — so `?before=` and `?after=`
// are not a hint a server may drop: a client that moves a rule and gets an answer of 200 has been
// told its rule now outranks another. Priorities are rewritten as a dense sequence so that a client
// moving a rule a hundred times cannot run the integers out.
func (s *Service) Move(ctx context.Context, localpart, kind, ruleID, before, after string) error {
	if before == "" && after == "" {
		return nil
	}
	if before != "" && after != "" {
		return matrix.NewInvalidParam("only one of before and after may be given")
	}
	rows, err := s.store.PushRules.List(ctx, localpart, Scope)
	if err != nil {
		return err
	}
	var ordered []string
	var moving *store.PushRule
	for _, row := range rows {
		if row.Kind != kind {
			continue
		}
		if row.RuleID == ruleID {
			moving = &store.PushRule{}
			*moving = row
			continue
		}
		ordered = append(ordered, row.RuleID)
	}
	if moving == nil {
		return matrix.NewNotFound("push rule %q does not exist", ruleID)
	}
	anchor := before
	if anchor == "" {
		anchor = after
	}
	at := len(ordered)
	if anchor != "" {
		at = -1
		for i, id := range ordered {
			if id == anchor {
				at = i
				break
			}
		}
		if at < 0 {
			return matrix.NewInvalidParam("there is no rule %q to place %q against", anchor, ruleID)
		}
		if after != "" {
			at++
		}
	}
	ordered = append(ordered[:at], append([]string{ruleID}, ordered[at:]...)...)

	for _, row := range rows {
		if row.Kind != kind || row.RuleID == ruleID {
			continue
		}
		for position, id := range ordered {
			if id != row.RuleID {
				continue
			}
			row.Priority = int64((position + 1) * 100)
			if err := s.store.PushRules.Put(ctx, &row); err != nil {
				return err
			}
			break
		}
	}
	// The moved rule takes the slot it was placed in, which is the one point in the sequence its
	// own ID now occupies.
	for position, id := range ordered {
		if id != ruleID {
			continue
		}
		moving.Priority = int64((position + 1) * 100)
		break
	}
	if err := s.store.PushRules.Put(ctx, moving); err != nil {
		return err
	}
	return s.changed(ctx, localpart)
}
