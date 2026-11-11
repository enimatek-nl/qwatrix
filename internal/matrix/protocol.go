// Package matrix additions: protocol constants that are not error codes or identifiers.
package matrix

import (
	"bytes"
	"encoding/json"
)

// Membership values. The spec calls them memberships; they are the state_key'd value of an
// m.room.member event's `membership` field.
const (
	MembershipJoin   = "join"
	MembershipInvite = "invite"
	MembershipLeave  = "leave"
	MembershipBan    = "ban"
	MembershipKnock  = "knock"
	MembershipForget = "forget"
)

// Room visibility values of POST /createRoom.
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public"
)

// Room version 11 and later use these join rule spellings; the older alias is kept for
// readability where the older semantics apply.
const (
	JoinRuleInvite          = "invite"
	JoinRulePublic          = "public"
	JoinRuleKnock           = "knock"
	JoinRuleRestricted      = "restricted"
	JoinRuleKnockRestricted = "knock_restricted"
)

// History visibility values.
const (
	HistoryWorldReadable = "world_readable"
	HistoryShared        = "shared"
	HistoryInvited       = "invited"
	HistoryJoined        = "joined"
)

// Guest access values.
const (
	GuestCanJoin   = "can_join"
	GuestForbidden = "forbidden"
)

// Room event types, spelled as strings rather than referenced from a library so that the
// route tables and state handling read like the specification.
const (
	EventTypeCreate            = "m.room.create"
	EventTypeMember            = "m.room.member"
	EventTypePowerLevels       = "m.room.power_levels"
	EventTypeJoinRules         = "m.room.join_rules"
	EventTypeName              = "m.room.name"
	EventTypeTopic             = "m.room.topic"
	EventTypeAvatar            = "m.room.avatar"
	EventTypeCanonicalAlias    = "m.room.canonical_alias"
	EventTypeGuestAccess       = "m.room.guest_access"
	EventTypeHistoryVisibility = "m.room.history_visibility"
	EventTypeEncryption        = "m.room.encryption"
	EventTypeTombstone         = "m.room.tombstone"
	EventTypeRedaction         = "m.room.redaction"
	EventTypeMessage           = "m.room.message"
	EventTypePinnedEvents      = "m.room.pinned_events"
	EventTypeServerACL         = "m.room.server_acl"
	EventTypeSpaceChild        = "m.space.child"
	EventTypeSpaceParent       = "m.space.parent"
	EventTypeTyping            = "m.typing"
	EventTypeReceipt           = "m.receipt"
	EventTypeReadMarker        = "m.fully_read"
	EventTypeReadReceipt       = "m.read"
	EventTypeDirect            = "m.direct"
	EventTypeTagPrefix         = "m.tag"
	EventTypeIgnoredUserList   = "m.ignored_user_list"
)

// Limitations the specification imposes on room metadata. They are enforced server-side
// because a client that overshoots gets a truncation-on-display rather than a corrupt room.
const (
	// MaxRoomNameLength is the maximum length of m.room.name's `name`.
	MaxRoomNameLength = 255
	// MaxRoomTopicLength is the maximum length of m.room.topic's `topic`.
	MaxRoomTopicLength = 1000
	// MaxRoomAliasLength is the maximum length of an alias local part.
	MaxRoomAliasLength = 255
	// MaxEventTypeLength is the practical cap on a custom event type.
	MaxEventTypeLength = 255
)

// DirectoryVisibility values of /directory/list/room/{roomId}.
const (
	DirectoryVisibilityPrivate  = "private"
	DirectoryVisibilityPublic   = "public"
	DirectoryVisibilityUnlisted = "unlisted"
)

// ServerAclKey is the content key of m.room.server_acl's IP lookalike rule.
const ServerAclKey = "ip_lookbacks"

// RoomTypeSpace is the create-event type that makes a room a space.
const RoomTypeSpace = "m.space"

// ValidateEventContent checks the parts of the event-content contract that Go's JSON decoder
// cannot enforce on its own, and answers M_BAD_JSON, because it is the caller's body that is
// wrong rather than the parameters around it.
//
// The rule that matters is about numbers. Event content travels between homeservers inside
// signed JSON, and a signature commits to the exact bytes: a server that re-serialises a number
// it cannot hold exactly, or that rounds a fraction, has changed an event another server already
// signed. The specification therefore restricts content numbers to integers an IEEE-754 double
// represents exactly — which excludes 1.1 as firmly as it excludes 2^53, even though both look
// harmless in a JSON file. Rejecting them here is the difference between refusing a request and
// producing an event every other server will reject on receipt.
//
// The content must also be a JSON object, which is what an event's content always is.
func ValidateEventContent(content []byte) error {
	if len(content) == 0 {
		return NewBadJSON("the event content must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()

	// The top level has to be an object; scalars and arrays are not content. Token is used
	// rather than Decode because it hands back the opening delimiter and stops there, which
	// is the only way to walk the document without ever turning a number into a float64.
	open, err := decoder.Token()
	if err != nil {
		return DecodeFailure("the event content", err)
	}
	if delim, ok := open.(json.Delim); !ok || delim != '{' {
		return NewBadJSON("the event content must be a JSON object")
	}
	if err := walkContent(decoder, true); err != nil {
		return err
	}
	// Trailing bytes after a complete document mean the body was never one JSON value.
	if decoder.More() {
		return NewBadJSON("the event content is not valid JSON: unexpected data after the top-level object")
	}
	return nil
}

// maxContentInteger is the largest integer an IEEE-754 double holds exactly, 2^53 - 1.
const maxContentInteger = int64(1<<53) - 1

// walkContent reads the remainder of one container, rejecting any number the protocol cannot
// carry. inObject says whether it is an object — whose members are a name and then a value —
// or an array, whose elements are values alone. The two look the same until you try to read a
// member name where an element should be, which silently swallows the next value.
//
// Values are read as tokens rather than decoded into a Go type on purpose: decoding into
// interface{} routes every number through float64, and that conversion is precisely the loss
// being detected. A json.Number keeps the client's own spelling, so the check sees what the
// sender wrote.
func walkContent(decoder *json.Decoder, inObject bool) error {
	for decoder.More() {
		if inObject {
			if _, err := decoder.Token(); err != nil { // the member name
				return DecodeFailure("the event content", err)
			}
		}
		if err := walkContentValue(decoder); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil { // consume the closing } or ]
		return DecodeFailure("the event content", err)
	}
	return nil
}

// walkContentValue checks one value of an event content.
func walkContentValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return DecodeFailure("the event content", err)
	}
	switch value := token.(type) {
	case json.Delim: // '{' or '[' — check inside, then continue with the siblings
		return walkContent(decoder, value == '{')
	case json.Number:
		number, err := value.Int64()
		if err != nil {
			return NewBadJSON("the event content contains " + value.String() +
				", which is not an integer")
		}
		if number > maxContentInteger || number < -maxContentInteger {
			return NewBadJSON("the event content contains " + value.String() +
				", which is outside the range [-2^53+1, 2^53-1] an IEEE-754 double holds exactly")
		}
		return nil
	default:
		return nil
	}
}

// CanKick reports whether a moderated leave (a kick) may be applied to a member whose current
// membership is the given value.
//
// A kick ends a membership, so there must be one: somebody who never arrived has nothing to end,
// and somebody who already left would be made to leave twice. "ban" is excluded because a banned
// user is not in the room either — un-banning is a different transition with its own endpoint.
func CanKick(membership string) bool {
	switch membership {
	case MembershipJoin, MembershipInvite, MembershipKnock:
		return true
	default:
		return false
	}
}
