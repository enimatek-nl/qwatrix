package roomserver

// Write paths: sending messages, setting state, changing membership, redacting.
//
// Every one of them funnels into [eventWriter.append], so there is exactly one place where
// an event becomes part of a room. That is deliberate: the checks that must not be
// bypassed — authorization, state updates, denormalised caches, waking pollers — are all in
// that one function.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// SendMessage appends a message event, honouring the client's transaction ID.
//
// Idempotency is checked before the transaction opens rather than inside it: the whole
// point is that a retry costs one read instead of producing a second event, and reading
// outside the write lock keeps a slow client from holding the writer.
func (s *Service) SendMessage(ctx context.Context, sender Sender, roomID, eventType, txnID string, content json.RawMessage) (eventID string, replayed bool, err error) {
	if eventType == "" {
		return "", false, matrix.NewInvalidParam("an event type is required")
	}
	// An event is state because it carries a state key, and because of nothing else — the
	// type only says which state definition applies *if* there is a key. So a
	// PUT /send/m.room.join_rules/{txn} is a legitimate timeline event that happens to share
	// its name with a state type, and refusing it would be refusing a request the protocol
	// permits. Clients send these; complement's own helpers do it to put an arbitrary event in
	// a room. The timeline and the state cannot disagree about it, because this event is not
	// part of the state at all.
	if err := validateEventType(eventType); err != nil {
		return "", false, err
	}
	// A state type written here would be a timeline event pretending to be a room rule, so it is
	// refused rather than persisted: see stateOnlyEventTypes.
	if err := validateNotStateType(eventType); err != nil {
		return "", false, err
	}

	if txnID != "" {
		// A transaction ID is a device's own numbering, scoped to the room the client is
		// addressing. Replaying it answers with the event the first attempt made — including
		// when the body differs, because the request that got through already decided what
		// happened, and a client retrying a lost response is not going to reconstruct the
		// body it sent. Rejecting a *different* body would turn the one case idempotency
		// exists for (an SDK that regenerates the event on retry) into a failure.
		existing, lookupErr := s.store.Txns.Get(ctx, sender.Localpart, sender.DeviceID, roomID, txnID)
		switch {
		case lookupErr == nil:
			return existing.EventID, true, nil
		case !store.IsNotFound(lookupErr):
			return "", false, lookupErr
		}
	}

	err = s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		writer := &eventWriter{service: s, tx: tx, ctx: ctx}
		row, appendErr := writer.append(txAppend{
			roomID:    roomID,
			eventType: eventType,
			isState:   false,
			sender:    sender.UserID,
			content:   json.RawMessage(content),
			txnID:     txnID,
			deviceID:  sender.DeviceID,
		})
		if appendErr != nil {
			return appendErr
		}
		eventID = row.EventID
		if txnID != "" {
			if err := s.store.Txns.Put(ctx, tx, &store.TxnID{
				Localpart:   sender.Localpart,
				DeviceID:    sender.DeviceID,
				RoomID:      roomID,
				TxnID:       txnID,
				EventID:     row.EventID,
				EventType:   eventType,
				PayloadHash: hashPayload(content),
			}); err != nil && !store.IsAlreadyExists(err) {
				// A duplicate here means a concurrent retry of the same transaction won
				// the race; the event it created is the one the client should have, and
				// the event we just created would be a fork in the room's history. It is
				// not: the write lock serialises appends, so reaching here means the
				// client retried while we were writing, and the event we built is the
				// correct answer either way.
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}

	s.notifier.Publish()
	return eventID, false, nil
}

// SendStateEvent sets one state tuple of a room.
func (s *Service) SendStateEvent(ctx context.Context, sender Sender, roomID, eventType, stateKey string, content json.RawMessage) (string, error) {
	if err := validateEventType(eventType); err != nil {
		return "", err
	}
	if eventType == matrix.EventTypeCanonicalAlias {
		// The room's own claim about what it is called has to be true, or it is a lie no
		// client can check.
		if err := s.validateAliasClaim(ctx, roomID, json.RawMessage(content)); err != nil {
			return "", err
		}
	}
	if eventType == matrix.EventTypeCreate {
		// The create event is the room's identity: rewriting it would let a client change
		// which room a room ID refers to.
		return "", matrix.NewForbidden("%q may not be changed after a room is created", eventType)
	}

	var eventID string
	err := s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		writer := &eventWriter{service: s, tx: tx, ctx: ctx}
		row, err := writer.append(txAppend{
			roomID:    roomID,
			eventType: eventType,
			stateKey:  stateKey,
			isState:   true,
			sender:    sender.UserID,
			content:   json.RawMessage(content),
		})
		if err != nil {
			return err
		}
		eventID = row.EventID
		return nil
	})
	if err != nil {
		return "", err
	}
	s.notifier.Publish()
	return eventID, nil
}

// MembershipRequest describes a membership change.
type MembershipRequest struct {
	Sender Sender
	RoomID string
	// TargetLocalpart is whose membership changes. It is the sender's own local part for
	// joins, leaves and knocks.
	TargetLocalpart string
	// Membership is the value to write.
	Membership string
	// Reason is the optional audit string the spec carries in content.reason.
	Reason string
	// Extra carries content keys the client sent alongside the ones this request type
	// defines. A join body is not only a reason: the specification lets a client put
	// anything in the resulting member event, and clients do use it to attach their own
	// metadata to the fact that they joined. Keys the server owns — membership above all —
	// are not overridable, so this map is merged before those are written.
	Extra map[string]json.RawMessage
}

// UpdateMembership writes an m.room.member event.
//
// The rules of who may do what — a moderator kicking, a room's invite-only gate, a knock
// against a room that allows it — are applied by the authorization layer like every other
// event, because they are the same rules a remote server applies to the same event and
// implementing them twice is how two implementations end up disagreeing about who is in a
// room.
func (s *Service) UpdateMembership(ctx context.Context, req MembershipRequest) (string, error) {
	target := req.TargetLocalpart
	if target == "" {
		target = req.Sender.Localpart
	}
	switch req.Membership {
	case matrix.MembershipJoin, matrix.MembershipLeave, matrix.MembershipInvite,
		matrix.MembershipBan, matrix.MembershipKnock:
	case matrix.MembershipForget:
		// "forget" is not a membership that appears in an event: it is the client's way of
		// asking for the room to disappear from their list, and it is carried by the
		// membership row's flag instead.
		return "", s.Forget(ctx, target, req.RoomID)
	default:
		return "", matrix.NewInvalidParam("unknown membership %q", req.Membership)
	}

	// Only three membership changes can target somebody other than the sender: an invite, a
	// ban, and a kick (which the spec spells as a leave event written by somebody else).
	// Anything else means the client asked for something the protocol does not have, and
	// saying so here keeps the mistake from turning into a puzzling authorization failure.
	actingForSomeoneElse := target != req.Sender.Localpart
	if actingForSomeoneElse && req.Membership != matrix.MembershipInvite &&
		req.Membership != matrix.MembershipBan && req.Membership != matrix.MembershipLeave {
		return "", matrix.NewForbidden("a user may only invite, ban or kick somebody else")
	}

	// A client's own keys come first, so that everything below — membership, the reason, the
	// inviter's profile — wins over a client that sent a key of the same name. The server
	// owns those fields; the rest of the document belongs to the sender.
	content := map[string]any{}
	for key, value := range req.Extra {
		content[key] = value
	}
	content["membership"] = req.Membership
	if req.Reason != "" {
		content["reason"] = req.Reason
	}
	if req.Membership == matrix.MembershipInvite {
		// An invited client renders "Alice invited you" from the invite event alone, so the
		// inviter's profile travels in its content.
		if inviter, err := s.store.Users.Get(ctx, req.Sender.Localpart); err == nil {
			if inviter.DisplayName != "" {
				content["display_name"] = inviter.DisplayName
			}
			if inviter.AvatarURL != "" {
				content["avatar_url"] = inviter.AvatarURL
			}
		}
		// A local target that does not exist is a typo worth reporting. A remote user ID is
		// not this server's to check — and must not be, or inviting across federation would
		// depend on this server being able to reach the other side.
		if targetUser, err := s.store.Users.Get(ctx, target); err == nil {
			if targetUser.DisplayName != "" {
				content["display_name"] = targetUser.DisplayName
			}
		} else if !store.IsNotFound(err) {
			return "", err
		} else if isLocalLocalpart(target) {
			return "", matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"User %q does not exist on this homeserver", matrix.UserID(target, s.cfg.ServerName))
		}
	}

	var eventID string
	err := s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		writer := &eventWriter{service: s, tx: tx, ctx: ctx}
		row, err := writer.append(txAppend{
			roomID:    req.RoomID,
			eventType: matrix.EventTypeMember,
			// The state key of an m.room.member event is a user ID, not a local part; the
			// service API accepts local parts for a caller's own convenience and this is the
			// one place that turns them back into the protocol's form.
			stateKey:   memberStateKey(target, s.cfg.ServerName),
			isState:    true,
			sender:     req.Sender.UserID,
			content:    content,
			membership: req.Membership,
		})
		if err != nil {
			return err
		}
		eventID = row.EventID
		return nil
	})
	if err != nil {
		return "", err
	}
	s.notifier.Publish()
	return eventID, nil
}

// RedactEvent redacts an event: it writes an m.room.redaction event and replaces the redacted
// event's content with the room version's redacted form.
//
// The original event is not deleted. Redaction is a statement that the content should not be
// read, not a claim that it never existed — other servers hold it, and clients that saw it
// keep it. Deleting bytes here would make this server's copy of history disagree with
// everybody else's, which in a distributed system is worse than the problem redaction was
// asked to solve.
func (s *Service) RedactEvent(ctx context.Context, sender Sender, roomID, eventID, txnID, reason string) (string, error) {
	target, err := s.store.Events.ByEventID(ctx, eventID)
	if err != nil {
		if store.IsNotFound(err) {
			return "", matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Event %q does not exist", eventID)
		}
		return "", err
	}
	if target.RoomID != roomID {
		return "", matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"Event %q is not in room %q", eventID, roomID)
	}

	var redactionID string
	err = s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		writer := &eventWriter{service: s, tx: tx, ctx: ctx}

		// Room version 11 moved `redacts` into the content; earlier versions carry it as a
		// top-level event property. The library writes the right one for the room version,
		// so the content here mirrors it for the newer versions and the top-level field is
		// set for the older ones.
		content := map[string]any{}
		if reason != "" {
			content["reason"] = reason
		}
		room, err := s.store.Rooms.Get(ctx, roomID)
		if err != nil {
			return err
		}
		if redactsInContent(room.Version) {
			content["redacts"] = eventID
		}

		row, err := writer.append(txAppend{
			roomID:    roomID,
			eventType: matrix.EventTypeRedaction,
			isState:   false,
			sender:    sender.UserID,
			content:   content,
			redacts:   eventID,
			txnID:     txnID,
			deviceID:  sender.DeviceID,
		})
		if err != nil {
			return err
		}
		redactionID = row.EventID

		redacted, err := s.events.Redact(room.Version, target.JSON)
		if err != nil {
			return err
		}
		return s.store.Events.ReplaceJSON(ctx, eventID, redacted)
	})
	if err != nil {
		return "", err
	}
	s.notifier.Publish()
	return redactionID, nil
}

// Forget drops a room from a user's list without changing their membership.
//
// The specification is explicit that this does not remove the user from the room and does
// not delete the room's history: it only stops the server telling that user about it.
func (s *Service) Forget(ctx context.Context, localpart, roomID string) error {
	err := s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		membership, err := s.store.Memberships.Get(ctx, roomID, localpart)
		if err != nil {
			if store.IsNotFound(err) {
				// Forgetting a room the user was never in succeeds: the desired end state
				// already holds, and an error would make every client's cleanup loop fail
				// on rooms it never joined.
				return nil
			}
			return err
		}
		if membership.Membership != matrix.MembershipLeave && membership.Membership != matrix.MembershipBan &&
			membership.Membership != matrix.MembershipInvite {
			// Forgetting a room you are still in is refused with 400: the caller is not
			// describing a stale room list, it is still a member, and the answer to that is
			// to leave first. 404 would say the room is unknown, which is a different
			// problem and sends a client down the wrong path.
			return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrUnknown,
				"You are currently joined to the room")
		}
		return s.store.Memberships.MarkForgotten(ctx, roomID, localpart, true)
	})
	if err != nil {
		return err
	}
	s.notifier.Publish()
	return nil
}

// LeaveRoom is the join/leave/kick/ban/knock entry point used by the room-member routes.
func (s *Service) LeaveRoom(ctx context.Context, sender Sender, roomID, targetLocalpart, reason string) (string, error) {
	return s.UpdateMembership(ctx, MembershipRequest{
		Sender:          sender,
		RoomID:          roomID,
		TargetLocalpart: targetLocalpart,
		Membership:      matrix.MembershipLeave,
		Reason:          reason,
	})
}

// JoinRoom makes a user join a room they are allowed to join.
func (s *Service) JoinRoom(ctx context.Context, sender Sender, roomID, reason string,
	extra map[string]json.RawMessage) (string, error) {
	return s.UpdateMembership(ctx, MembershipRequest{
		Sender:          sender,
		RoomID:          roomID,
		TargetLocalpart: sender.Localpart,
		Membership:      matrix.MembershipJoin,
		Reason:          reason,
		Extra:           extra,
	})
}

// InviteToRoom invites a user, given their user ID.
func (s *Service) InviteToRoom(ctx context.Context, sender Sender, roomID, targetUserID, reason string) (string, error) {
	localpart, _, err := matrix.ParseUserID(targetUserID, s.cfg.ServerName)
	if err != nil {
		return "", err
	}
	return s.UpdateMembership(ctx, MembershipRequest{
		Sender:          sender,
		RoomID:          roomID,
		TargetLocalpart: localpart,
		Membership:      matrix.MembershipInvite,
		Reason:          reason,
	})
}

// KickFromRoom removes a user from the room without banning them, which the spec spells as
// a leave event sent by somebody else.
func (s *Service) KickFromRoom(ctx context.Context, sender Sender, roomID, targetUserID, reason string) (string, error) {
	localpart, _, err := matrix.ParseUserID(targetUserID, s.cfg.ServerName)
	if err != nil {
		return "", err
	}
	// A kick acts on a membership, so there has to be one to act on. Answering 200 with a
	// leave event for somebody who was never here invents history for a room they are not in,
	// and for somebody who already left it re-announces a departure the room has moved on
	// from. Both are answered the way the spec answers a moderated transition that does not
	// apply: M_FORBIDDEN.
	if membership, err := s.store.Memberships.Get(ctx, roomID, localpart); err != nil {
		if store.IsNotFound(err) {
			return "", matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
				"User %q is not a member of this room", targetUserID)
		}
		return "", err
	} else if !matrix.CanKick(membership.Membership) {
		return "", matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
			"User %q cannot be kicked from this room: they are %s",
			targetUserID, membership.Membership)
	}
	return s.UpdateMembership(ctx, MembershipRequest{
		Sender:          sender,
		RoomID:          roomID,
		TargetLocalpart: localpart,
		Membership:      matrix.MembershipLeave,
		Reason:          reason,
	})
}

// BanUser bans a user from the room.
func (s *Service) BanUser(ctx context.Context, sender Sender, roomID, targetUserID, reason string) (string, error) {
	localpart, _, err := matrix.ParseUserID(targetUserID, s.cfg.ServerName)
	if err != nil {
		return "", err
	}
	return s.UpdateMembership(ctx, MembershipRequest{
		Sender:          sender,
		RoomID:          roomID,
		TargetLocalpart: localpart,
		Membership:      matrix.MembershipBan,
		Reason:          reason,
	})
}

// UnbanUser removes a ban, which is a leave event for a banned user.
func (s *Service) UnbanUser(ctx context.Context, sender Sender, roomID, targetUserID string) (string, error) {
	localpart, _, err := matrix.ParseUserID(targetUserID, s.cfg.ServerName)
	if err != nil {
		return "", err
	}
	membership, err := s.store.Memberships.Get(ctx, roomID, localpart)
	if err != nil {
		if store.IsNotFound(err) {
			return "", matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"User %q is not banned from this room", targetUserID)
		}
		return "", err
	}
	if membership.Membership != matrix.MembershipBan {
		return "", matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"User %q is not banned from this room", targetUserID)
	}
	return s.UpdateMembership(ctx, MembershipRequest{
		Sender:          sender,
		RoomID:          roomID,
		TargetLocalpart: localpart,
		Membership:      matrix.MembershipLeave,
	})
}

// redactsInContent reports whether a room version puts m.room.redaction's `redacts` field
// in the content rather than at the top level (room version 11 and later).
func redactsInContent(roomVersion string) bool {
	value := 0
	for _, r := range roomVersion {
		if r < '0' || r > '9' {
			break
		}
		value = value*10 + int(r-'0')
	}
	return value >= 11
}

// stateOnlyEventTypes are the event types the specification defines as state events — the ones whose
// meaning lives in a room's state, addressed by `(type, state_key)`.
//
// Writing one through `PUT /state` sets the room's rule. Writing the same type through `POST /send`
// creates a *timeline* event with no state key, which changes nothing and yet reads as the real thing to
// any client that walks events instead of resolving state. So a room member could drop an event shaped
// exactly like the room's power rules in front of everybody in the room, and the room's actual rules
// would be somewhere else entirely. That is a forgery of the room's appearance rather than a change to
// it, and the honest response is to refuse the write and name the endpoint that means what the caller
// evidently wanted.
var stateOnlyEventTypes = map[string]bool{
	"m.room.create":             true,
	"m.room.member":             true,
	"m.room.power_levels":       true,
	"m.room.join_rules":         true,
	"m.room.history_visibility": true,
	"m.room.guest_access":       true,
	"m.room.topic":              true,
	"m.room.name":               true,
	"m.room.avatar":             true,
	"m.room.canonical_alias":    true,
	"m.room.encryption":         true,
	"m.room.server_acl":         true,
	"m.room.tombstone":          true,
	"m.room.pinned_events":      true,
	"m.space.child":             true,
	"m.space.parent":            true,
}

// validateNotStateType refuses a state event type arriving on the timeline path.
func validateNotStateType(eventType string) error {
	if stateOnlyEventTypes[eventType] {
		return matrix.NewInvalidParam(
			"event type %q is a state event; write it with PUT /_matrix/client/v3/rooms/{roomID}/state/%s",
			eventType, eventType)
	}
	return nil
}

// validateEventType enforces the parts of the event-type grammar that protect this server: a type is
// required, bounded, and made of characters that survive a URL path, a database key and a filename.
//
// It deliberately does *not* enforce the specification's recommendation that a custom type carry a
// reverse-domain prefix. The specification says such a type MUST be namespaced and defines no error for
// the violation, and no other homeserver refuses one — so refusing would not keep anybody safe, it would
// only mean this server's users cannot send anything to anybody. A type is opaque bytes to a homeserver;
// namespaces are a courtesy between clients, and a courtesy is enforced by asking, not by breaking the
// person who did not read the fine print.
func validateEventType(eventType string) error {
	if eventType == "" {
		return matrix.NewInvalidParam("an event type is required")
	}
	if len(eventType) > matrix.MaxEventTypeLength {
		return matrix.NewInvalidParam("an event type must be at most %d characters", matrix.MaxEventTypeLength)
	}
	if strings.HasPrefix(eventType, "m.") {
		return nil
	}
	for _, r := range eventType {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '~':
		default:
			return matrix.NewInvalidParam("event type %q contains invalid characters", eventType)
		}
	}
	return nil
}

// hashPayload fingerprints a request body for transaction replay detection.
func hashPayload(content json.RawMessage) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// isLocalLocalpart reports whether a local part could only belong to this server: one
// without a domain component is local by construction.
func isLocalLocalpart(target string) bool {
	return !strings.Contains(target, ":") && !strings.HasPrefix(target, "@")
}

// ErrNotImplemented marks a call whose semantics the specification defines but this server
// does not implement yet, so that a handler returns the spec's error rather than a wrong
// answer.
var ErrNotImplemented = errors.New("roomserver: not implemented")

// TODOs for the write paths that follow in later milestones:
//   - /rooms/{roomID}/upgrade (writes m.room.tombstone and creates the successor room).
//   - knocking and knock_state (room versions 7+).
//   - restricted join rules against space parents.

// validateAliasClaim checks an m.room.canonical_alias event before it is written.
//
// The event is the room naming itself, and it is the only name a client has when it meets the
// room without context — in a search result, a room picker, or a link somebody pasted. An alias
// in it that does not resolve back to this room therefore makes the room describe itself
// falsely, which no client downstream can detect, so the server refuses it.
//
// Two mistakes are distinguished because they have different fixes. A malformed alias is a
// parameter error: the caller did not name an alias at all. A well-formed alias that is not
// this room's is M_BAD_ALIAS: the syntax is fine and the claim is wrong, and a client told the
// first would retry the same false claim with tidied punctuation.
//
// A remote alias is refused as well. Whether it resolves is not this server's to know, and a
// canonical alias it cannot verify is exactly the unguessable claim this check exists to stop.
func (s *Service) validateAliasClaim(ctx context.Context, roomID string, content json.RawMessage) error {
	var claim struct {
		Alias      string   `json:"alias"`
		AltAliases []string `json:"alt_aliases"`
	}
	if err := json.Unmarshal(content, &claim); err != nil {
		// A body this cannot read is left to the ordinary content validation; this check is
		// about claims, and an unreadable body makes none.
		return nil
	}
	for _, alias := range append([]string{claim.Alias}, claim.AltAliases...) {
		if alias == "" {
			// Clearing the canonical alias is always allowed: a room may stop claiming a
			// name at any time.
			continue
		}
		localpart, domain, err := matrix.ParseAlias(alias)
		if err != nil {
			return matrix.NewInvalidParam("%s is not a valid room alias", alias)
		}
		if domain != s.cfg.ServerName {
			return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrBadAlias,
				"Alias %q is not on this homeserver, so its target cannot be verified", alias)
		}
		target, err := s.store.Aliases.Get(ctx, matrix.Alias(localpart, domain))
		if err != nil {
			if store.IsNotFound(err) {
				return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrBadAlias,
					"Alias %q does not exist", alias)
			}
			return err
		}
		if target.RoomID != roomID {
			return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrBadAlias,
				"Alias %q points to a different room", alias)
		}
	}
	return nil
}
