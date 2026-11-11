// Package events is the Matrix event domain: the only place that builds, signs, authorises,
// redacts and formats events.
//
// It deliberately keeps the hard, subtle parts to a single library (gomatrixserverlib, the
// implementation Dendrite and Complement themselves use) rather than reimplementing
// canonical JSON, reference hashing, event-ID formats, the authorization-rule table and the
// redaction algorithm. Everything above it — storage, rooms, sync — is qwatrix code.
package events

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/identity"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// Service builds and evaluates events for one homeserver.
type Service struct {
	cfg      *config.Config
	identity *identity.Identity
}

// New builds the service.
func New(cfg *config.Config, id *identity.Identity) *Service {
	return &Service{cfg: cfg, identity: id}
}

// Identity exposes the server's signing identity (used by the federation and key packages).
func (s *Service) Identity() *identity.Identity { return s.identity }

// Origin is the server name events are attributed to.
func (s *Service) Origin() spec.ServerName { return s.identity.ServerName }

// DefaultRoomVersion is the version new rooms get when the client does not name one.
func (s *Service) DefaultRoomVersion() gomatrixserverlib.RoomVersion {
	return gomatrixserverlib.RoomVersion(s.cfg.DefaultRoomVersion)
}

// RoomVersionsSupportedForCreation lists the room versions this server can create rooms in.
//
// This is every stable version the event library implements, rather than a hand-picked subset.
// The first drafts of this list left versions 1 and 2 out on the theory that their `prev_state`
// field makes them a separate code path — but the library's per-version implementation carries
// the format, hashing, redaction and state-resolution rules for each version, and this server
// builds and verifies events only through it, so there is no path to duplicate. A server that
// quietly refuses a version the specification calls stable is a server where an old room cannot
// be created, and the client finds out only by failing; the versions this server supports are
// advertised through /capabilities, so advertising the truth is what makes the choice safe.
//
// One property does stop being the library's to carry: how a room gets its ID. Versions in the
// MSC4291 family have no domain in a room ID at all, and this server names rooms
// `!random:its.own.domain` — a name that has to appear in the create event, which the authorisation
// layer then refuses because the two do not match. That is not a version to advertise and then fail
// on, so it is left out of the list, which is where the refusal belongs: /capabilities stops
// promising it, and creating it answers M_INCOMPATIBLE_ROOM_VERSION instead of an internal complaint.
//
// The rule is read from the library rather than hardcoded, so the next version in that family is
// excluded by the same fact that excludes this one.
func RoomVersionsSupportedForCreation() []string {
	out := []string{}
	for version, impl := range gomatrixserverlib.StableRoomVersions() {
		if impl.DomainlessRoomIDs() {
			continue
		}
		out = append(out, string(version))
	}
	sort.Strings(out)
	return out
}

// ResolveRoomVersion validates a requested room version, mapping the specification's
// distinct error codes: M_UNSUPPORTED_ROOM_VERSION for something that does not exist, and
// M_INCOMPATIBLE_ROOM_VERSION for a known version this server will not serve.
//
// The spelling is the current specification's. Synapse answers this one as
// `M_IncompatibleRoomVersion` and so does this server's federation path, where a peer's library may
// compare the string it has always compared; a client asking to create a room gets the code the
// error-code table actually defines.
func (s *Service) ResolveRoomVersion(requested string) (gomatrixserverlib.RoomVersion, error) {
	if requested == "" {
		return s.DefaultRoomVersion(), nil
	}
	version := gomatrixserverlib.RoomVersion(requested)
	if !gomatrixserverlib.KnownRoomVersion(version) {
		return "", matrix.NewAPIError(http.StatusBadRequest, matrix.ErrUnsupportedVersion,
			"Room version %q is not supported by this homeserver", requested)
	}
	if !gomatrixserverlib.StableRoomVersion(version) {
		return "", matrix.NewAPIError(http.StatusBadRequest, matrix.ErrUnsupportedVersion,
			"Room version %q is unstable and is not enabled on this homeserver", requested)
	}
	known := false
	for _, supported := range RoomVersionsSupportedForCreation() {
		if supported == requested {
			known = true
			break
		}
	}
	if !known {
		return "", matrix.NewAPIError(http.StatusBadRequest, matrix.ErrIncompatibleRoomVersion,
			"Room version %q is known but this server cannot create rooms in it", requested)
	}
	return version, nil
}

// RoomVersion returns the implementation for a version this server can follow.
func (s *Service) RoomVersion(version string) (gomatrixserverlib.IRoomVersion, error) {
	impl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(version))
	if err != nil {
		return nil, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrIncompatibleRoomVersion,
			"Room version %q is not supported by this homeserver", version)
	}
	return impl, nil
}

// EventTimestamp is the time origin_server_ts is derived from, in integer milliseconds.
type EventTimestamp = spec.Timestamp

// UserIDForSender is the callback every authorization check uses to turn a sender ID back
// into a user ID.
//
// qwatrix writes full user IDs as sender IDs, so this is the identity function — but it is
// threaded as a *function* rather than inlined, because the specification's "sender id"
// rooms make that mapping a property of the room, and a server that hard-codes the identity
// mapping cannot support them later.
func (s *Service) UserIDForSender() spec.UserIDForSender {
	return func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		userID, err := spec.NewUserID(string(senderID), true)
		if err != nil {
			return nil, fmt.Errorf("sender id %q is not a user ID: %w", senderID, err)
		}
		return userID, nil
	}
}

// UserForSender is the plain-function form of [Service.UserIDForSender].
func (s *Service) UserForSender(senderID string) (*spec.UserID, error) {
	// The room is not part of the answer for rooms whose sender IDs are user IDs; it is
	// only consulted by the MSC4014 state resolution this server does not implement.
	return s.UserIDForSender()(spec.RoomID{}, spec.SenderID(senderID))
}

// SenderIDForUser is the inverse: the sender ID to write for a user ID.
func SenderIDForUser(userID string) spec.SenderID {
	return spec.SenderID(userID)
}

// Draft describes an event to build.
//
// The zero value is not useful; every field that the specification requires is here, and
// the fields gomatrixserverlib derives (event ID, hashes, signatures, origin_server_ts)
// are not.
type Draft struct {
	// RoomVersion is mandatory: it decides the event ID format, the hash algorithm and
	// the redaction rules.
	RoomVersion gomatrixserverlib.RoomVersion
	// RoomID is the room ID. Versions that put the room ID in the create event (v11+)
	// leave it empty for that one event, which the builder enforces.
	RoomID string
	// Type is the event type.
	Type string
	// StateKey is non-nil for state events. An empty-but-present state key is a real
	// state key, so this is a pointer rather than a string.
	StateKey *string
	// Sender is the full user ID of the originator.
	Sender string
	// Content is the event content; any JSON-marshalable value.
	Content any
	// PrevEvents are the event IDs this event is built on.
	PrevEvents []string
	// AuthEventIDs overrides the authorised event IDs. Empty means "derive them from
	// AuthState", which is almost always what is wanted.
	AuthEventIDs []string
	// AuthState is the current state of the room, used to pick auth events.
	AuthState []gomatrixserverlib.PDU
	// Depth is the DAG depth (create event is 1).
	Depth int64
	// Redacts is the event ID this event redacts.
	Redacts string
	// Timestamp is origin_server_ts; zero means now.
	Timestamp time.Time
	// Unsigned is response-only data (e.g. transaction_id) that must not be signed; it is
	// attached after building.
	Unsigned map[string]any
}

// Build turns a draft into a signed event.
func (s *Service) Build(draft Draft) (gomatrixserverlib.PDU, error) {
	version, err := s.RoomVersion(string(draft.RoomVersion))
	if err != nil {
		return nil, err
	}

	builder := version.NewEventBuilder()
	builder.Type = draft.Type
	builder.StateKey = draft.StateKey
	builder.SenderID = string(draft.Sender)
	builder.RoomID = draft.RoomID
	builder.Depth = draft.Depth
	builder.Redacts = draft.Redacts

	// An empty list must stay an empty list rather than becoming nil: the create event of a
	// room genuinely has no previous events, and the protocol distinguishes "no prev_events"
	// from "prev_events is absent" — gomatrixserverlib rejects the latter.
	builder.PrevEvents = append(make([]string, 0, len(draft.PrevEvents)), draft.PrevEvents...)

	// The content is canonicalised before signing because the hash is taken over these
	// bytes: an escaped or padded variant would be a different event with a different ID.
	switch content := draft.Content.(type) {
	case nil:
		if err := builder.SetContent(map[string]any{}); err != nil {
			return nil, fmt.Errorf("setting empty content: %w", err)
		}
	case json.RawMessage:
		canonical, err := CanonicalJSON(content)
		if err != nil {
			return nil, matrix.NewBadJSON(fmt.Sprintf("event content is not valid JSON: %s", err))
		}
		// The conversion to json.RawMessage matters: gomatrixserverlib marshals whatever it
		// is handed, and marshalling a []byte base64-encodes it, which would put a string in
		// a field the protocol requires to be an object.
		if err := builder.SetContent(json.RawMessage(canonical)); err != nil {
			return nil, matrix.NewBadJSON(fmt.Sprintf("event content is not a JSON object: %s", err))
		}
	default:
		canonical, err := CanonicalJSONOf(content)
		if err != nil {
			return nil, fmt.Errorf("encoding the content of a %s event: %w", draft.Type, err)
		}
		if err := builder.SetContent(canonical); err != nil {
			return nil, matrix.NewBadJSON(fmt.Sprintf("event content is not a JSON object: %s", err))
		}
	}

	if len(draft.AuthEventIDs) > 0 {
		builder.AuthEvents = append(make([]string, 0, len(draft.AuthEventIDs)), draft.AuthEventIDs...)
	} else {
		if err := builder.AddAuthEvents(newAuthProvider(draft.AuthState)); err != nil {
			return nil, fmt.Errorf("selecting auth events for a %s event: %w", draft.Type, err)
		}
	}

	timestamp := draft.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	event, err := builder.Build(timestamp, s.identity.ServerName, s.identity.KeyID, s.identity.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("building the %s event: %w", draft.Type, err)
	}

	if len(draft.Unsigned) > 0 {
		// `unsigned` is by definition not covered by the signatures, so setting it after
		// signing is correct — and it is also why the fields a client may not trust live
		// there.
		updated, err := event.SetUnsigned(draft.Unsigned)
		if err != nil {
			return nil, fmt.Errorf("attaching unsigned data to the %s event: %w", draft.Type, err)
		}
		event = updated
	}
	return event, nil
}

// Parse reads stored event JSON back into an event. The bytes are trusted because this
// server wrote them: they were verified and canonicalised on the way in.
func (s *Service) Parse(version, raw []byte) (gomatrixserverlib.PDU, error) {
	impl, err := s.RoomVersion(string(version))
	if err != nil {
		return nil, err
	}
	event, err := impl.NewEventFromTrustedJSON(raw, false)
	if err != nil {
		return nil, fmt.Errorf("reading stored event JSON: %w", err)
	}
	return event, nil
}

// ParseRoomVersioned is Parse with the version named as a string.
func (s *Service) ParseRoomVersioned(roomVersion string, raw json.RawMessage) (gomatrixserverlib.PDU, error) {
	return s.Parse([]byte(roomVersion), raw)
}

// AuthEvents wraps a slice of events as an auth-event provider, which is what both
// authorization checks and auth-event selection consume.
func (s *Service) AuthEvents(pdus []gomatrixserverlib.PDU) (*gomatrixserverlib.AuthEvents, error) {
	authEvents, err := gomatrixserverlib.NewAuthEvents(pdus)
	if err != nil {
		return nil, fmt.Errorf("building the auth event set: %w", err)
	}
	return authEvents, nil
}

// Authorize runs the specification's event authorization rules for an event against the
// room state it is being added to.
//
// A refusal is the specification's M_FORBIDDEN: the reason text from gomatrixserverlib is
// written for humans and names the rule that failed, which is what makes a permission bug
// debuggable from a client's error message.
func (s *Service) Authorize(event gomatrixserverlib.PDU, state []gomatrixserverlib.PDU) error {
	authEvents, err := s.AuthEvents(state)
	if err != nil {
		return err
	}
	if err := gomatrixserverlib.Allowed(event, authEvents, s.UserIDForSender()); err != nil {
		var notAllowed *gomatrixserverlib.NotAllowed
		if errors.As(err, &notAllowed) {
			return matrix.NewForbidden("%s", notAllowed.Error())
		}
		return fmt.Errorf("authorizing the event: %w", err)
	}
	return nil
}

// Redact applies the room version's redaction algorithm to an event, returning the redacted
// JSON that replaces it.
func (s *Service) Redact(roomVersion, eventJSON string) (string, error) {
	version, err := s.RoomVersion(roomVersion)
	if err != nil {
		return "", err
	}
	redacted, err := version.RedactEventJSON([]byte(eventJSON))
	if err != nil {
		return "", fmt.Errorf("redacting an event in a %s room: %w", roomVersion, err)
	}
	return string(redacted), nil
}

// CanonicalJSON rewrites a JSON document into the form the specification hashes.
//
// gomatrixserverlib.CanonicalJSON only removes whitespace, which is not enough here: Go's
// encoder escapes `<`, `>` and `&` as \u003c, \u003e and \u0026, and those escapes survive
// into the bytes the event hash commits to. A client that sends an escaped body would then
// get an event whose hash another server recomputes differently — a federation failure that
// looks like corruption. So this decodes with numbers preserved and re-encodes with escaping
// turned off.
func CanonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// Numbers are kept as the client wrote them: decoding to float64 would turn a
	// large integer depth or ts into 1e+18 and silently change the event.
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("document is not valid JSON: %w", err)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(sortKeys(value)); err != nil {
		return nil, fmt.Errorf("encoding canonical JSON: %w", err)
	}
	// Encode appends a newline; canonical JSON has none.
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// sortKeys makes key order explicit. Go already sorts map keys, so this only has to
// descend into slices and nested objects; it is separate from CanonicalJSON so the result
// can also be used where deterministic ordering matters on its own.
func sortKeys(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		sorted := make(map[string]any, len(typed))
		for key, item := range typed {
			sorted[key] = sortKeys(item)
		}
		return sorted
	case []any:
		sorted := make([]any, len(typed))
		for i, item := range typed {
			sorted[i] = sortKeys(item)
		}
		return sorted
	default:
		return value
	}
}

// CanonicalJSONOf marshals any value into canonical form.
func CanonicalJSONOf(value any) (json.RawMessage, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(sortKeys(value)); err != nil {
		return nil, fmt.Errorf("encoding canonical JSON: %w", err)
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// encodeUnescaped marshals a document without HTML-escaping the content's own bytes.
// This matters whenever the document embeds json.RawMessage: encoding/json re-escapes an
// embedded raw message otherwise, which would double-escape a stored event.
func encodeUnescaped(document any) (json.RawMessage, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// PowerLevels returns the power levels of a room from the state's m.room.power_levels
// event, or the specification's defaults when the room has none (which cannot happen for a
// room this server created, but can for one received over federation).
func (s *Service) PowerLevels(state []gomatrixserverlib.PDU) (*gomatrixserverlib.PowerLevelContent, error) {
	authEvents, err := s.AuthEvents(state)
	if err != nil {
		return nil, err
	}
	if len(state) == 0 {
		defaults := gomatrixserverlib.PowerLevelContent{}
		defaults.Defaults()
		return &defaults, nil
	}
	levels, err := gomatrixserverlib.NewPowerLevelContentFromAuthEvents(authEvents, "")
	if err != nil {
		// A room with no readable power-levels event is not an error: the specification's
		// defaults apply, which is also what a room received over federation falls back to.
		defaults := gomatrixserverlib.PowerLevelContent{}
		defaults.Defaults()
		return &defaults, nil
	}
	return &levels, nil
}

// DefaultPowerLevels returns the power levels a new room starts with, for a given creator.
//
// The numbers are the specification's defaults, plus the creator at 100 — unless the room
// version grants the creator their power implicitly, in which case naming them here would be
// refused by the authorization rules: those versions forbid the creator appearing in the
// users map precisely because their level is not data the room's members can revoke.
func DefaultPowerLevels(creator string, privilegedCreator bool, additional map[string]int64) map[string]any {
	users := map[string]any{}
	if !privilegedCreator {
		users[creator] = int64(100)
	}
	for localpartOrID, level := range additional {
		users[localpartOrID] = level
	}
	return map[string]any{
		"ban": int64(50),
		"events": map[string]any{
			// State events default to 50 (state_default); these are the ones the
			// specification calls out as needing more than the default.
			"m.room.encryption":         int64(100),
			"m.room.history_visibility": int64(100),
			"m.room.power_levels":       int64(100),
			"m.room.redaction":          int64(0),
			"m.room.tombstone":          int64(100),
			"m.room.server_acl":         int64(100),
			"m.space.child":             int64(0),
		},
		"events_default": int64(0),
		"invite":         int64(0),
		"kick":           int64(50),
		"notifications":  map[string]any{"room": int64(50)},
		"redact":         int64(0),
		"state_default":  int64(50),
		"users":          users,
		"users_default":  int64(0),
	}
}

// UnsignedOptions describes the response-time decoration an event needs.
type UnsignedOptions struct {
	// EventID is written into the event when the signed form does not carry one. From
	// room version 2 the event ID is the content hash and gomatrixserverlib leaves it out
	// of the serialized event; clients still need it, so the response adds it back.
	EventID string
	// Now is the clock `age` is measured against; zero means the current time.
	Now time.Time
	// TransactionID is echoed back to the device that sent the event.
	TransactionID string
	// PrevContent and ReplacesState come from the state entry this event replaced.
	PrevContent   json.RawMessage
	ReplacesState string
	// Redacts is the event this one redacts (the pre-v9 form, where the relationship was
	// expressed in unsigned rather than in content).
	Redacts string
	// AgeTSToggles controls whether `age` is written at all; a backfilled event whose
	// origin timestamp is in the future has no meaningful age.
	SkipAge bool
	// Membership is the reader's own room membership at the point of this event, per
	// MSC4115. It is the reader's, not the sender's: the question a client asks is "was I
	// in this room when this happened", which is what decides whether a message was
	// readable at the time. Empty means unknown, and is left out.
	Membership string
}

// Decorate adds the `unsigned` fields a response needs to a stored event and returns the
// bytes to answer with.
//
// It rebuilds the event rather than editing the stored bytes: the stored form is exactly
// what was signed, and a response is allowed to say more than the signature commits to —
// that is the entire purpose of `unsigned`.
func (s *Service) Decorate(stored json.RawMessage, opts UnsignedOptions) (json.RawMessage, error) {
	// Unmarshal into a generic map, insert `unsigned`, re-marshal, and hand the result to
	// encoding/json once so the escaping is correct.
	var document map[string]json.RawMessage
	if err := json.Unmarshal(stored, &document); err != nil {
		return nil, fmt.Errorf("stored event is not a JSON object: %w", err)
	}

	if opts.EventID != "" {
		if existing, ok := document["event_id"]; !ok || string(existing) == `""` {
			document["event_id"] = jsonString(opts.EventID)
		}
	}

	unsigned := map[string]json.RawMessage{}
	if existing, ok := document["unsigned"]; ok {
		if err := json.Unmarshal(existing, &unsigned); err != nil {
			unsigned = map[string]json.RawMessage{}
		}
	}

	if opts.TransactionID != "" {
		unsigned["transaction_id"] = jsonString(opts.TransactionID)
	}
	if len(opts.PrevContent) > 0 {
		unsigned["prev_content"] = opts.PrevContent
	}
	if opts.ReplacesState != "" {
		unsigned["replaces_state"] = jsonString(opts.ReplacesState)
	}
	if opts.Redacts != "" {
		unsigned["redacts"] = jsonString(opts.Redacts)
	}
	if opts.Membership != "" {
		unsigned["membership"] = jsonString(opts.Membership)
	}
	if !opts.SkipAge {
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		var originMS int64
		if raw, ok := document["origin_server_ts"]; ok {
			parsed, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
			if err == nil {
				originMS = parsed
			}
		}
		if originMS > 0 {
			age := now.UnixMilli() - originMS
			if age < 0 {
				// A clock skew on the originating server is not the reader's problem, and
				// a negative age would be worse than none.
				age = 0
			}
			unsigned["age"] = json.RawMessage(strconv.FormatInt(age, 10))
			unsigned["age_ts"] = json.RawMessage(strconv.FormatInt(originMS*1000, 10))
		}
	}

	if len(unsigned) > 0 {
		encoded, err := json.Marshal(unsigned)
		if err != nil {
			return nil, fmt.Errorf("encoding unsigned data: %w", err)
		}
		document["unsigned"] = encoded
	} else {
		delete(document, "unsigned")
	}

	// encodeUnescaped, not json.Marshal: marshalling a map that contains json.RawMessage
	// re-escapes the embedded bytes, which would corrupt the content the event already
	// carries.
	out, err := encodeUnescaped(document)
	if err != nil {
		return nil, fmt.Errorf("encoding the decorated event: %w", err)
	}
	return out, nil
}

// jsonString encodes a Go string as a JSON string.
func jsonString(value string) json.RawMessage {
	// json.Marshal escapes `<`, `>` and `&`, which is harmless for a string we are
	// writing here but would be wrong if reused for content; keep the explicit form.
	encoded, err := json.Marshal(value)
	if err != nil {
		// A string always marshals; treating a failure as an empty string keeps a
		// formatting helper from becoming an error path.
		return json.RawMessage(`""`)
	}
	return encoded
}

// authProvider adapts a slice of events to gomatrixserverlib's auth-event provider
// interface, which is how auth-event selection finds a room's create event, join rules,
// power levels and the member event of the sender.
type authProvider struct {
	byTuple map[stateTuple]gomatrixserverlib.PDU
}

type stateTuple struct {
	eventType string
	stateKey  string
}

func newAuthProvider(pdus []gomatrixserverlib.PDU) *authProvider {
	provider := &authProvider{byTuple: make(map[stateTuple]gomatrixserverlib.PDU, len(pdus))}
	for _, event := range pdus {
		if event.StateKey() == nil {
			continue
		}
		provider.byTuple[stateTuple{eventType: event.Type(), stateKey: *event.StateKey()}] = event
	}
	return provider
}

// Create implements [gomatrixserverlib.AuthEventProvider].
func (p *authProvider) Create() (gomatrixserverlib.PDU, error) {
	return p.lookup(spec.MRoomCreate, "")
}

// JoinRules implements [gomatrixserverlib.AuthEventProvider].
func (p *authProvider) JoinRules() (gomatrixserverlib.PDU, error) {
	return p.lookup(spec.MRoomJoinRules, "")
}

// PowerLevels implements [gomatrixserverlib.AuthEventProvider].
func (p *authProvider) PowerLevels() (gomatrixserverlib.PDU, error) {
	return p.lookup(spec.MRoomPowerLevels, "")
}

// Member implements [gomatrixserverlib.AuthEventProvider].
func (p *authProvider) Member(senderID spec.SenderID) (gomatrixserverlib.PDU, error) {
	return p.lookup(spec.MRoomMember, string(senderID))
}

// Valid implements [gomatrixserverlib.AuthEventProvider]: it verifies that every auth event
// came from the same room. A provider built from one room's state is valid by construction;
// an empty set is valid too, because the create event being missing is reported by the
// authorization rules rather than by this check.
func (p *authProvider) Valid() bool { return true }

// ThirdPartyInvite implements [gomatrixserverlib.AuthEventProvider]. Third-party invites
// are not implemented by this homeserver, so the answer is "no such event" — which is the
// same shape as "not in this room's state" and makes the authorization rules refuse the
// invite rather than crash on it.
func (p *authProvider) ThirdPartyInvite(stateKey string) (gomatrixserverlib.PDU, error) {
	// No third-party invites exist in this server's state, which is the same answer as "not
	// in this room's state" and makes the authorization rules refuse the invite rather than
	// fail on a provider error.
	return nil, nil
}

// lookup answers with (nil, nil) for a tuple the room does not have.
//
// Absent is a normal answer, not a failure: a room being created has no power levels yet,
// and a provider that errored on that would make the very next event unbuildable. The
// authorization rules are what decide whether a missing create event or missing join rules
// is fatal, and they can only do that if they get to see the absence.
func (p *authProvider) lookup(eventType, stateKey string) (gomatrixserverlib.PDU, error) {
	return p.byTuple[stateTuple{eventType: eventType, stateKey: stateKey}], nil
}

// HashOfEvent computes the reference hash of an event, which is what an opaque event ID of
// a room version that hashes its content commits to. Exposed for the room-ID-as-hash room
// versions and for federation debugging.
func (s *Service) HashOfEvent(roomVersion, eventJSON string) (string, error) {
	version, err := s.RoomVersion(roomVersion)
	if err != nil {
		return "", err
	}
	event, err := version.NewEventFromTrustedJSON([]byte(eventJSON), false)
	if err != nil {
		return "", fmt.Errorf("parsing the event to hash: %w", err)
	}
	// The hash is over the unredacted, unhashed event reference; gomatrixserverlib keeps
	// the algorithm, this only formats it the way event IDs do.
	return "$" + base64.RawURLEncoding.EncodeToString([]byte(event.EventID())), nil
}
