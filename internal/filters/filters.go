// Package filters interprets a client's event filter.
//
// A filter is how a client says which parts of a room it wants to see: a timeline window of ten
// events, only `m.room.message`, nobody from a banned sender, and the state compressed to the
// members it does not already know. The rules are deliberately declarative — the client names
// what it wants and the server applies them while building a sync — because the alternative, a
// bespoke query per client screen, would put a room's read path in the client's hands.
package filters

import (
	"encoding/json"
	"strings"
)

// Filter is a whole filter definition, as sent to POST /user/{userID}/filter.
type Filter struct {
	Presence    *EventFilter `json:"presence,omitempty"`
	AccountData *EventFilter `json:"account_data,omitempty"`
	Room        *RoomFilter  `json:"room,omitempty"`

	// raw keeps the definition exactly as it arrived; see [Filter.Raw].
	raw json.RawMessage
}

// RoomFilter is the `room` section: what a sync should say about a room.
type RoomFilter struct {
	State       *EventFilter    `json:"state,omitempty"`
	Timeline    *TimelineFilter `json:"timeline,omitempty"`
	InviteState *EventFilter    `json:"invite_state,omitempty"`
	Ephemeral   *EventFilter    `json:"ephemeral,omitempty"`
}

// TimelineFilter is a room filter's timeline section, which additionally carries the window size.
type TimelineFilter struct {
	EventFilter

	// Limit is how many of the most recent events to return. It is a pointer because "no
	// timeline at all" (limit 0) and "no opinion" are different requests, and a plain int would
	// collapse them.
	Limit *int `json:"limit,omitempty"`

	// UnreadThreadNotifications asks for the room's unread badge to be split per thread. It is
	// not a filter on which events arrive; it is a request about how the unread counts are
	// reported, and it lives in the timeline filter because that is where a client asks about
	// the shape of a room's summary.
	UnreadThreadNotifications bool `json:"unread_thread_notifications,omitempty"`
}

// Embedded returns a timeline filter's own rules. The embedding is a Go detail; the wire format
// is flat, and callers that only want the match rules should not have to know which of the two
// shapes they are holding.
func (f *TimelineFilter) Embedded() *EventFilter {
	if f == nil {
		return nil
	}
	return &f.EventFilter
}

// EventFilter is the include/exclude set the spec reuses for every part of a filter.
type EventFilter struct {
	Limit      *int     `json:"limit,omitempty"`
	Types      []string `json:"types,omitempty"`
	NotTypes   []string `json:"not_types,omitempty"`
	Senders    []string `json:"senders,omitempty"`
	NotSenders []string `json:"not_senders,omitempty"`
	// LazyLoadMembers shrinks a room's state delta to the members the client has not seen. It
	// is the single largest bandwidth win in the specification, and it is off unless asked for.
	LazyLoadMembers bool `json:"lazy_load_members,omitempty"`
}

// Parse reads a filter definition. The original bytes are kept so a stored filter can be handed
// back exactly as it was written: a client that stored a key this server does not act on must
// still read it back, or saving and reloading a filter would silently change a client's config.
func Parse(definition json.RawMessage) (*Filter, error) {
	var filter Filter
	if err := json.Unmarshal(definition, &filter); err != nil {
		return nil, err
	}
	filter.raw = copyRaw(definition)
	return &filter, nil
}

// Raw returns the definition as it arrived, or its re-encoding when it was built in memory.
func (f *Filter) Raw() json.RawMessage {
	if len(f.raw) > 0 {
		return f.raw
	}
	encoded, err := json.Marshal(f)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// Valid reports whether a definition is a JSON object. Nothing else in a filter is rejected:
// unknown keys are a client's business and a future spec revision's, and refusing them would make
// this server the reason a newer client cannot start.
func Valid(definition json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(definition))
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(definition)
}

// TimelineLimit returns the window size a filter asks for, and whether the filter named one.
func (f *Filter) TimelineLimit() (*int, bool) {
	if f == nil || f.Room == nil || f.Room.Timeline == nil || f.Room.Timeline.Limit == nil {
		return nil, false
	}
	limit := *f.Room.Timeline.Limit
	return &limit, true
}

// StateLimits returns a room's per-section limits.
func (f *Filter) StateLimit() (*int, bool) {
	if f == nil || f.Room == nil || f.Room.State == nil || f.Room.State.Limit == nil {
		return nil, false
	}
	limit := *f.Room.State.Limit
	return &limit, true
}

// LazyLoading reports whether the client asked for the state delta to be compressed to the
// members it has not seen.
func (f *Filter) LazyLoading() bool {
	return f != nil && f.Room != nil && f.Room.State != nil && f.Room.State.LazyLoadMembers
}

// IncludeLeave returns the top-level `include_leave` flag, which asks a sync to omit rooms the
// user has left even when something happened in them.
func (f *Filter) IncludeLeave() bool {
	if f == nil {
		return true
	}
	// include_leave defaults to true; only an explicit false turns left rooms off.
	var probe struct {
		IncludeLeave *bool `json:"include_leave"`
	}
	if err := json.Unmarshal(f.Raw(), &probe); err != nil || probe.IncludeLeave == nil {
		return true
	}
	return *probe.IncludeLeave
}

// Allows reports whether an event survives this filter.
//
// The include list wins outright: naming types is how a client says "only these", and applying
// not_types as well would let one list silently cancel the other. With no include list, the
// exclude list decides. Wildcards follow the spec's `*`-suffix form, because that is how clients
// ask for a whole family such as `m.room.*`.
func (f *EventFilter) Allows(eventType, sender string) bool {
	if f == nil {
		return true
	}
	if len(f.Types) > 0 && !matchAny(f.Types, eventType) {
		return false
	}
	if matchAny(f.NotTypes, eventType) {
		return false
	}
	if len(f.Senders) > 0 && !matchAny(f.Senders, sender) {
		return false
	}
	if matchAny(f.NotSenders, sender) {
		return false
	}
	return true
}

// matchAny reports whether a value matches any pattern, where a pattern ending in `*` is a prefix.
func matchAny(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(value, strings.TrimSuffix(pattern, "*")) {
				return true
			}
			continue
		}
		if pattern == value {
			return true
		}
	}
	return false
}

func copyRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}

// Validate checks a filter definition's shape, and reports the first key that is wrong.
//
// Only the keys this version knows about are checked, and unknown keys are accepted: a filter is
// written by a client that may know a newer spec than this server does, and refusing a key because
// this release has not heard of it would make the server the reason a client cannot start. Shape,
// on the other hand, is checkable, and getting it wrong is not something to accept silently:
// a "types" that is a string would match nothing, and the client would see an empty timeline and
// be told its filter worked.
func Validate(definition json.RawMessage) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(definition, &document); err != nil {
		return NewFilterError("a filter must be a JSON object")
	}
	for _, key := range []string{"presence", "account_data", "room"} {
		raw, ok := document[key]
		if !ok {
			continue
		}
		if key != "room" {
			if err := validateSection(key, raw); err != nil {
				return err
			}
			continue
		}
		var room map[string]json.RawMessage
		if err := json.Unmarshal(raw, &room); err != nil {
			return NewFilterError("room must be an object")
		}
		for _, section := range []string{"state", "timeline", "ephemeral", "account_data", "invite_state"} {
			inner, ok := room[section]
			if !ok {
				continue
			}
			if err := validateSection("room."+section, inner); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateSection checks one event-filter section: a limit that is a number, lists that are lists
// of strings, and ID-shaped lists whose entries parse as the ID they name.
func validateSection(name string, raw json.RawMessage) error {
	var section map[string]json.RawMessage
	if err := json.Unmarshal(raw, &section); err != nil {
		return NewFilterError(name + " must be an object")
	}
	for _, key := range []string{"types", "not_types"} {
		if err := validateStringList(name+"."+key, section[key], false); err != nil {
			return err
		}
	}
	for _, key := range []string{"rooms", "not_rooms"} {
		if err := validateStringList(name+"."+key, section[key], true); err != nil {
			return err
		}
	}
	for _, key := range []string{"senders", "not_senders"} {
		if err := validateIDList(name+"."+key, section[key], "@"); err != nil {
			return err
		}
	}
	if raw, ok := section["limit"]; ok {
		var limit float64
		if err := json.Unmarshal(raw, &limit); err != nil || limit < 0 || limit != float64(int64(limit)) {
			return NewFilterError(name + ".limit must be a non-negative integer")
		}
	}
	if raw, ok := section["lazy_load_members"]; ok {
		var flag bool
		if err := json.Unmarshal(raw, &flag); err != nil {
			return NewFilterError(name + ".lazy_load_members must be a boolean")
		}
	}
	return nil
}

// validateStringList checks a list of strings, and when IDs is true that each entry is a room ID.
func validateStringList(name string, raw json.RawMessage, ids bool) error {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return NewFilterError(name + " must be a list of strings")
	}
	if !ids {
		return nil
	}
	for _, entry := range list {
		if !isRoomID(entry) {
			return NewFilterError(name + " must contain room IDs")
		}
	}
	return nil
}

// validateIDList checks a list of user IDs. A `*` is allowed anywhere in an entry because a filter
// may name a whole homeserver or every local user, which is how clients mute a broken instance.
func validateIDList(name string, raw json.RawMessage, prefix string) error {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return NewFilterError(name + " must be a list of user IDs")
	}
	for _, entry := range list {
		if len(entry) < 2 || !strings.HasPrefix(entry, prefix) || !strings.Contains(entry, ":") {
			return NewFilterError(name + " must contain user IDs")
		}
	}
	return nil
}

// isRoomID reports whether a value is shaped like a room ID: an exclamation mark, a local part, a
// colon, and a host.
func isRoomID(value string) bool {
	return len(value) > 2 && value[0] == '!' && strings.Contains(value, ":")
}

// FilterError is a filter that cannot be used. It is its own type so the handler answers with the
// spec's 400 rather than the 500 an unexpected internal failure deserves.
type FilterError struct{ message string }

// NewFilterError builds a [FilterError].
func NewFilterError(message string) *FilterError { return &FilterError{message: message} }

// Error implements error.
func (e *FilterError) Error() string { return e.message }
