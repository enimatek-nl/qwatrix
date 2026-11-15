package slidingsync

// Choosing the state a room is sent with, and noticing when the client just asked for more of it.
//
// A client's `required_state` is a set expression — include this, exclude that, members lazily —
// evaluated against the room's current state. Two things make it more than a filter:
//
//   - rooms are sent as deltas, so a state entry is only worth sending when it changed since this
//     connection last saw the room, or when the config widened to cover it; and
//   - state cannot be deleted in Matrix, only replaced by an event that says nothing, so "the room
//     used to have a name" has to be reported as a stub — `{type, state_key}` with no content — or
//     the client keeps a name that was taken away.

import (
	"encoding/json"
	"sort"
	"strings"
)

// stateSelector is a room config's required_state, ready to be evaluated.
type stateSelector struct {
	Include     []StatePattern `json:"include,omitempty"`
	Exclude     []StatePattern `json:"exclude,omitempty"`
	LazyMembers bool           `json:"lazy_members,omitempty"`
}

// selectorFor turns a request's required_state into an evaluator. A request that sent none means
// "no state", which is what the MSC implies when the field is required and empty: a client that
// wants everything says so with `{}`.
func selectorFor(request *RequiredStateRequest) stateSelector {
	if request == nil {
		return stateSelector{}
	}
	return stateSelector{Include: request.Include, Exclude: request.Exclude, LazyMembers: request.LazyMembers}
}

// matches reports whether a state entry should be returned.
//
// An element matches by omission: no `type` matches every type, no `state_key` matches every state
// key. `"*"` is accepted as a wildcard too, which costs nothing and saves an MSC3575-shaped client
// from silently being given no state — the failure mode of a literal-string comparison is a client
// with an empty room, and no clue why.
func (sel stateSelector) matches(eventType, stateKey string) bool {
	if len(sel.Include) == 0 {
		// No include list means nothing is required. The MSC requires the field, so this is a
		// client that sent `"required_state": {}`; sending the room's whole state to such a client
		// would be the one interpretation that is certainly wrong.
		return false
	}
	matched := false
	for _, pattern := range sel.Include {
		if pattern.matches(eventType, stateKey) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, pattern := range sel.Exclude {
		if pattern.matches(eventType, stateKey) {
			// Memberships asked for by name cannot be excluded by the lazy-membership rule below,
			// but an explicit exclude beats an explicit include: the MSC applies `exclude` after
			// `include`, and a client that wrote both means the exclude.
			return false
		}
	}
	if sel.LazyMembers && eventType == memberEventType && !sel.namedMember(stateKey) {
		// Lazy members: m.room.member events are chosen by who is in the timeline instead, except
		// for state keys the client named, and except for the wildcard case where it asked for all
		// memberships and then asked for laziness — in which case laziness wins, and the client's own
		// membership is still worth having if it asked for it by name.
		return false
	}
	return true
}

// namedMember reports whether the client asked for this particular membership by state key.
func (sel stateSelector) namedMember(stateKey string) bool {
	for _, pattern := range sel.Include {
		if pattern.StateKey != nil && *pattern.StateKey == stateKey && !isWildcard(*pattern.StateKey) {
			return true
		}
	}
	return false
}

// wantsAllMembers reports whether this config asks for membership events and is not lazy about them.
// Combining two configs has to know the difference: a union is lazy only if every config in it is.
func (sel stateSelector) wantsAllMembers() bool {
	if sel.LazyMembers || len(sel.Include) == 0 {
		return false
	}
	for _, pattern := range sel.Include {
		if !pattern.matches(memberEventType, "") {
			continue
		}
		// The exclude list decides it: a client that excludes all memberships is not asking for
		// them, and reading that as "all members wanted" would defeat its own exclude.
		for _, exclude := range sel.Exclude {
			if exclude.matches(memberEventType, "") {
				return false
			}
		}
		return true
	}
	return false
}

// matches reports whether one include/exclude element covers a state entry.
func (p StatePattern) matches(eventType, stateKey string) bool {
	if p.Type != nil && !isWildcard(*p.Type) && *p.Type != eventType {
		return false
	}
	if p.StateKey != nil && !isWildcard(*p.StateKey) && *p.StateKey != stateKey {
		return false
	}
	return true
}

// isWildcard reads the two spellings of "anything" an element can use: an omitted field, or "*".
func isWildcard(value string) bool { return value == "*" }

// merge combines the room configs of every rule a room matched, taking the superset as the MSC
// requires: the widest timeline, and the union of the state sets.
//
// The lazy-membership rule is the one worth reading twice. A union is lazy only if it does not also
// contain a config that wants every membership: a client with a room list of `{}` + lazy and a
// subscribed room of `{}` without laziness has asked, somewhere in this request, for all the
// members of that room.
func (sel stateSelector) merge(other stateSelector) stateSelector {
	merged := stateSelector{
		Include: append(append([]StatePattern{}, sel.Include...), other.Include...),
		Exclude: append(append([]StatePattern{}, sel.Exclude...), other.Exclude...),
		// Laziness survives a union only if no rule in it asked for the whole member list: a client
		// with a lazy room list and one non-lazy subscribed room has asked, somewhere in this
		// request, for that room's members.
		LazyMembers: (sel.LazyMembers || other.LazyMembers) &&
			!sel.wantsAllMembers() && !other.wantsAllMembers(),
	}
	return merged.normalised()
}

// normalised sorts and de-duplicates the element lists so that two requests asking for the same
// state in a different order produce the same stored config.
//
// This matters because the config is how the server decides whether a client has been shown a room's
// state before: an order-sensitive comparison turns every re-ordering into an expansion, and a client
// that walks its room list would be resent the state of every room it touched.
func (sel stateSelector) normalised() stateSelector {
	return stateSelector{
		Include:     normalisePatterns(sel.Include),
		Exclude:     normalisePatterns(sel.Exclude),
		LazyMembers: sel.LazyMembers,
	}
}

func normalisePatterns(patterns []StatePattern) []StatePattern {
	if len(patterns) == 0 {
		return nil
	}
	out := make([]StatePattern, len(patterns))
	copy(out, patterns)
	sort.Slice(out, func(i, j int) bool { return patternKey(out[i]) < patternKey(out[j]) })
	deduped := out[:1]
	for _, pattern := range out[1:] {
		if patternKey(pattern) != patternKey(deduped[len(deduped)-1]) {
			deduped = append(deduped, pattern)
		}
	}
	return deduped
}

// patternKey is a total order over elements that is also their identity for de-duplication. Omitted
// and "*" both mean "any" and are written the same way, so `{}` and `{"type":"*"}` are one element.
func patternKey(pattern StatePattern) string {
	key := "*"
	if pattern.Type != nil {
		key = *pattern.Type
	}
	stateKey := "*"
	if pattern.StateKey != nil {
		stateKey = *pattern.StateKey
	}
	if isWildcard(key) {
		key = "*"
	}
	if isWildcard(stateKey) {
		stateKey = "*"
	}
	return key + "\x00" + stateKey
}

// encodes renders the config for storage; the empty selector is stored as an empty object rather
// than an empty string so that "the client asked for no state" decodes as that, not as a missing
// record.
func (sel stateSelector) encodes() string {
	document, err := json.Marshal(sel)
	if err != nil {
		// A selector is built from JSON that was already decoded, so it cannot fail to re-encode.
		// Falling back to the empty config is the honest answer if it ever did: the next answer
		// resends state it did not need to, rather than skipping state the client needs.
		return "{}"
	}
	return string(document)
}

// decodeSelector reads a stored config. An empty string is "nothing was recorded", which callers
// treat as a first send rather than as a config of nothing.
func decodeSelector(stored string) (stateSelector, bool) {
	if strings.TrimSpace(stored) == "" {
		return stateSelector{}, false
	}
	var sel stateSelector
	if err := json.Unmarshal([]byte(stored), &sel); err != nil {
		return stateSelector{}, false
	}
	return sel, true
}

// memberEventType is the state type lazily loaded membership is about.
const memberEventType = "m.room.member"
