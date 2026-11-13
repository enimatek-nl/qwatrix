package roomserver

// Knocking: letting a stranger ask to be let in, in a form the room can answer.
//
// A knock is a membership event like any other, which is the whole reason it works. It travels over
// federation like one, appears in the room's state like one, and is refused by the room's own
// authorization rules like one — this server adds no knock-specific permission of its own, because a
// second implementation of "may this user knock" is a second opportunity to disagree with the server on
// the other side of the border about who is in a room.
//
// What this file does own is the two things the authorization rules cannot say:
//
// A room whose version predates the membership (before version 7) is refused with its own error, not with
// a forbidden. "You may not" and "this room is not built out of the kind of event you are asking for" are
// different facts, and a client that hears the first will look for a permission to be granted.
//
// And a room this server does not hold is not this server's to knock on locally. Storing a knock in a
// room it has never seen would give the user a private picture of a room whose members were never told,
// which is the failure mode this feature exists to prevent.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// firstRoomVersionWithKnock is the room version that introduced the `knock` membership (MSC2403).
const firstRoomVersionWithKnock = 7

// RemoteKnocker carries a knock to a server that holds the room. It is installed by the composition root
// when this server federates; without it, a knock on a room this server does not hold is refused rather
// than quietly recorded at home.
type FederationKnocker interface {
	// KnockRemoteRoom runs `make_knock` and `send_knock` towards one of `servers` and returns what the
	// peer answered. It is transport only: what arrives is checked and stored here, because this is
	// where the rules of this server's own users live.
	KnockRemoteRoom(ctx context.Context, userID, roomID, reason string,
		servers []string) (*RemoteKnock, error)
}

// RemoteKnock is a peer's answer to a knock this server asked for.
type RemoteKnock struct {
	// Origin names the server that answered, recorded so a failure can be blamed on somebody.
	Origin string
	// RoomVersion is the version the peer says the room follows, which decides how every event below
	// is parsed.
	RoomVersion string
	// KnockEvent is this server's own knock: built here from the peer's template and signed here. It
	// needs no verification, which is the difference between this handshake and a federated join.
	KnockEvent gomatrixserverlib.PDU
	// RoomState is what the peer offers of the room to somebody it has not let in. It is unsigned by
	// design (MSC2403 sends a stripped set; MSC4311 proposes full events), so it is kept under the
	// same narrow rule the knock screen itself applies.
	RoomState []json.RawMessage
}

// WithFederationKnocker installs the transport a knock on a remote room needs.
func (s *Service) WithFederationKnocker(knocker FederationKnocker) *Service {
	s.federationKnocker = knocker
	return s
}

// KnockRoomVia answers `POST /_matrix/client/v3/rooms/{roomID}/knock` and its alias spelling.
//
// `servers` names the servers the client suggests knocking through, which for a room this server holds is
// noise and for one it does not is the only way in.
func (s *Service) KnockRoomVia(ctx context.Context, sender Sender, roomID, reason string,
	extra map[string]json.RawMessage, servers []string) (string, error) {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err == nil {
		// A room this server is not in is not this server's to knock on locally — even when it holds a
		// description of it. That is the case for a room the user knocked on before, and for one they
		// left: what sits in the local database is a *report* of a room, kept so the user's knock screen
		// has something in it, and its state is whatever a peer chose to send. Writing a second knock
		// against that would both trust the report and let the room's members go untold, so the knock is
		// taken to a server that holds the room, which is where a repeat knock belongs.
		if room.Left && s.federationKnocker != nil {
			return s.knockRemoteRoom(ctx, sender, roomID, reason, servers)
		}
		return s.knockLocalRoom(ctx, sender, roomID, room.Version, reason, extra)
	}
	if !store.IsNotFound(err) {
		return "", err
	}

	// A room ID whose home server is this one and which this server does not have is an absent room,
	// not a remote one: knocking on it would be asking a peer to vouch for an identity this server owns.
	if _, domain, err := matrix.ParseRoomID(roomID); err == nil && domain == s.cfg.ServerName {
		return "", matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
	}
	if s.federationKnocker == nil {
		return "", matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
	}
	return s.knockRemoteRoom(ctx, sender, roomID, reason, servers)
}

// knockRemoteRoom knocks on a room held elsewhere, and keeps what that produced.
//
// Only two things are stored: this server's own knock event, and the peer's account of the room's front
// door. The room is recorded as one this server has *left*, which is the flag that keeps it out of the
// directory, out of `/hierarchy`, and out of every question of the form "are we in this room?" — a
// knocked room is a room whose members have not said yes yet, and nothing this server does may read that
// as membership. What it does give the knocking user is the receipt of their knock in their own sync, so
// the request is visibly outstanding rather than vanished.
func (s *Service) knockRemoteRoom(ctx context.Context, sender Sender, roomID, reason string,
	servers []string) (string, error) {
	remote, err := s.federationKnocker.KnockRemoteRoom(ctx, sender.UserID, roomID, reason, servers)
	if err != nil {
		return "", err
	}
	if remote == nil || remote.KnockEvent == nil {
		return "", matrix.NewUnknown("the answer to the federated knock carried no knock event")
	}
	if knocked := remote.KnockEvent.RoomID().String(); knocked != roomID {
		return "", matrix.NewUnknown(fmt.Sprintf(
			"a peer answered a knock for room %q when this server asked for %q", knocked, roomID))
	}
	if err := s.persistFederatedKnock(ctx, roomID, remote); err != nil {
		return "", err
	}
	s.notifier.Publish()
	return remote.KnockEvent.EventID(), nil
}

// persistFederatedKnock stores a knock on a room this server is not in.
func (s *Service) persistFederatedKnock(ctx context.Context, roomID string, remote *RemoteKnock) error {
	version, err := s.events.RoomVersion(remote.RoomVersion)
	if err != nil {
		return unsupportedRoomVersionForKnock(remote.RoomVersion)
	}
	return s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.Rooms.Get(ctx, roomID); err != nil {
			if !store.IsNotFound(err) {
				return err
			}
			if err := s.store.Rooms.Create(ctx, &store.Room{
				ID:      roomID,
				Version: remote.RoomVersion,
				// `Left` is the honest flag: this server holds a description of a room it is
				// not in, and every question about whether it participates must answer no.
				Left:      true,
				CreatedMS: time.Now().UnixMilli(),
			}); err != nil {
				return err
			}
		}

		writer := &eventWriter{service: s, tx: tx, ctx: ctx}
		for _, raw := range remote.RoomState {
			event, err := version.NewEventFromUntrustedJSON(raw)
			if err != nil {
				slog.Debug("roomserver: a peer's knock state could not be read as an event",
					"room_id", roomID, "error", err)
				continue
			}
			if event.RoomID().String() != roomID || event.StateKey() == nil {
				// State from another room describes rules that were never made for this
				// one, and a non-state event has no place in a knock screen at all.
				continue
			}
			if !isKnockStateType(event.Type()) {
				continue
			}
			if err := writer.appendPrebuilt(remoteAppendRequest(roomID, event), event); err != nil {
				slog.Debug("roomserver: a peer's knock state could not be stored",
					"room_id", roomID, "event_type", event.Type(), "error", err)
				continue
			}
		}
		// Last, so the reader's own receipt is the newest thing in the room.
		return writer.appendPrebuilt(remoteAppendRequest(roomID, remote.KnockEvent), remote.KnockEvent)
	})
}

// unsupportedRoomVersionForKnock is the refusal for a peer that names a room version this server cannot
// parse events for.
func unsupportedRoomVersionForKnock(version string) error {
	return matrix.NewIncompatibleRoomVersion(version)
}

// knockLocalRoom writes the knock into a room this server holds.
func (s *Service) knockLocalRoom(ctx context.Context, sender Sender, roomID, version, reason string,
	extra map[string]json.RawMessage) (string, error) {
	if !roomVersionSupportsKnock(version) {
		return "", matrix.NewIncompatibleRoomVersion(version)
	}
	// `membership` is written by the membership request itself and a client cannot override it; a
	// `reason` is the client's, and anything else in the body travels to the event unchanged, as it
	// does for a join.
	return s.UpdateMembership(ctx, MembershipRequest{
		Sender:     sender,
		RoomID:     roomID,
		Membership: matrix.MembershipKnock,
		Reason:     reason,
		Extra:      extra,
	})
}

// roomVersionSupportsKnock reports whether a room version has the `knock` membership in it at all.
//
// The version names in the specification are the integers from "1" upwards, so that is what is compared.
// A name that is not a number — an experiment named after its MSC, which some deployments create rooms
// under — is not refused here: this server does not know what such a room contains, and inventing a
// refusal would turn an unknown room version into a room that cannot be knocked on. The event
// authoriser, which reads the version's actual rules, still has the last word.
func roomVersionSupportsKnock(version string) bool {
	number, err := strconv.Atoi(version)
	if err != nil {
		return true
	}
	return number >= firstRoomVersionWithKnock
}

// knockStateTypes are the events a room shows to somebody who has knocked on it: the events that
// describe the door and nothing behind it.
//
// This is a deliberately narrow set, and it is the whole of the protocol's answer to an uncomfortable
// position. A knocking user is not in the room. They have no right to its messages, its member list, or
// its power structure — and yet a client has to show them *something*, or the knock they sent looks like
// a request that vanished. So the room hands over its name, its picture, the alias it answers to, the
// rule that decides who may enter, and the visibility promise that governs what they would see if they
// were let in: the front of the house, and the receipt of their own knock.
//
// Note what is not here: any other user's membership event. A room's member list is the fact a knocking
// user most wants and least is entitled to — it is who to ask, and it is other people's presence in a
// room they have not been admitted to.
var knockStateTypes = map[string]struct{}{
	string(matrix.EventTypeCreate):            {},
	string(matrix.EventTypeJoinRules):         {},
	string(matrix.EventTypeName):              {},
	string(matrix.EventTypeAvatar):            {},
	string(matrix.EventTypeCanonicalAlias):    {},
	string(matrix.EventTypeHistoryVisibility): {},
}

// KnockState answers the `knock_state` of one room for one of this server's users.
//
// It reads the room's state directly rather than through the room's read rules, and that is the point: a
// knocking user fails the room's read rules by design — they are not a member — and the set below is not
// what those rules protect. The filter, not the gate, is what keeps this honest: anything that is not the
// front-door set, and any membership event that is not the reader's own, is not returned.
func isKnockStateType(eventType string) bool {
	_, ok := knockStateTypes[eventType]
	return ok
}

func (s *Service) KnockState(ctx context.Context, roomID, localpart string) ([]json.RawMessage, error) {
	return s.knockState(ctx, roomID, memberStateKey(localpart, s.cfg.ServerName))
}

// KnockStateForUser answers the same question for a full user ID, which is what a peer names in
// `send_knock` when it asks to be let in: the answer it gets is that user's own receipt and the room's
// front door, and nothing else.
func (s *Service) KnockStateForUser(ctx context.Context, roomID, userID string) ([]json.RawMessage, error) {
	return s.knockState(ctx, roomID, userID)
}

func (s *Service) knockState(ctx context.Context, roomID, own string) ([]json.RawMessage, error) {
	// The reader's own receipt is not in the room's state, because a knock is not a state change —
	// it is the ask. The membership row is what points at it, being the one place this server records
	// that somebody knocked.
	// The room's own record of the ask, newest wins: a user who knocked twice has their latest
	// knock as their receipt, and the room's members see that same one.
	ownRow := ""
	ownPosition := int64(-1)
	members, err := s.store.Events.Range(ctx, store.EventQuery{
		RoomIDs: []string{roomID},
		Types:   []string{string(matrix.EventTypeMember)},
	})
	if err != nil {
		return nil, err
	}
	for _, row := range members {
		if row.StateKey == nil || *row.StateKey != own || row.RejectedReason != "" {
			continue
		}
		if gjson.Get(row.JSON, "content.membership").String() != matrix.MembershipKnock {
			continue
		}
		if int64(row.ID) > ownPosition {
			ownPosition, ownRow = int64(row.ID), row.EventID
		}
	}
	state, err := s.store.State.All(ctx, roomID)
	if err != nil {
		return nil, err
	}
	rows := map[string]store.EventRow{}
	ids := make([]string, 0, len(state)+1)
	if ownRow != "" {
		ids = append(ids, ownRow)
	}
	for _, entry := range state {
		if _, ok := knockStateTypes[entry.Type]; ok {
			ids = append(ids, entry.EventID)
		}
	}
	if len(ids) == 0 {
		return []json.RawMessage{}, nil
	}
	found, err := s.store.Events.ByEventIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range found {
		rows[row.EventID] = row
	}

	// Ordered by (type, state_key) like every other state answer, with the reader's own membership last:
	// it is the event the client is looking for, and putting it in a fixed place means a client does not
	// have to know the alphabet to find their receipt.
	out := make([]json.RawMessage, 0, len(ids))
	render := func(eventID string) error {
		row, ok := rows[eventID]
		if !ok {
			return nil
		}
		event, err := s.Render(ctx, &row, RenderOptions{SkipTransactionID: true})
		if err != nil {
			return err
		}
		out = append(out, event)
		return nil
	}
	for _, entry := range state {
		if entry.EventID == ownRow {
			continue
		}
		if _, ok := knockStateTypes[entry.Type]; !ok {
			continue
		}
		if err := render(entry.EventID); err != nil {
			return nil, err
		}
	}
	if ownRow != "" {
		if err := render(ownRow); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MakeKnockTemplate answers `GET /_matrix/federation/v1/make_knock/{roomID}/{userID}` and its v2 — the
// draft a peer signs before it hands the knock to `send_knock`.
//
// Unlike a leave, no membership in this room is required, because the whole point of the handshake is a
// user the room has never seen. That is also what makes it worth a moment's thought: this answer tells a
// stranger where this room's graph ends and which events authorise a change in it, about a room its users
// have not been admitted to. It gives away nothing that a room's public behaviour does not already — the
// peer learns the room's version and the shape of its history, and gets to *ask*; whether the knock means
// anything is decided by `send_knock`, where the room's own join rule is read and its members are told.
func (s *Service) MakeKnockTemplate(ctx context.Context, roomID, userID string,
	requested []string) (*MembershipTemplate, error) {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewForbidden("this server cannot take a knock on that room")
		}
		return nil, err
	}
	if !roomVersionSupportsKnock(room.Version) {
		return nil, matrix.NewIncompatibleRoomVersion(room.Version)
	}
	// `?ver=` is the peer saying which versions it can speak about this room. Answering a draft in a
	// version the asking server cannot parse would be an invitation to fail at the next step, so a
	// list that does not include this room's version is refused here, while the answer is still cheap.
	if len(requested) > 0 {
		allowed := false
		for _, want := range requested {
			if want == room.Version {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, matrix.NewIncompatibleRoomVersion(room.Version)
		}
	}
	return s.draftMembership(ctx, roomID, userID, matrix.MembershipKnock, false)
}

// CheckFederationKnock is the door check a peer's knock has to pass before this server admits it.
//
// The membership-shaped refusals are 400s and the identity-shaped ones 403s, for the reason every
// membership door here uses: an event that was built wrong is a thing a peer can rebuild, and a server
// submitting a membership for a user it does not hold is not a mistake that a retry can fix.
//
// The identity rules are the whole of the trust here. A knock arrives signed by a server, and it claims
// that a user knocked. If the two did not have to agree, one badly behaved peer could have every user on
// every server appear to knock on any room it liked — and a knock is a message to that room's members,
// which makes it worth forging.
func CheckFederationKnock(event gomatrixserverlib.PDU, roomID, origin string) error {
	if event.Type() != string(matrix.EventTypeMember) {
		return matrix.NewInvalidParam("send_knock requires an m.room.member event, got %q", event.Type())
	}
	stateKey := event.StateKey()
	if stateKey == nil || *stateKey == "" {
		return matrix.NewInvalidParam("a knock event must name the user it knocks")
	}
	if string(event.SenderID()) != *stateKey {
		return matrix.NewForbidden("a knock may only speak for the user whose server signed it")
	}
	sender, err := spec.NewUserID(string(event.SenderID()), true)
	if err != nil {
		return matrix.NewForbidden("%q is not a user ID", event.SenderID())
	}
	if string(sender.Domain()) != origin {
		return matrix.NewForbidden("%q may not knock as %s: that user belongs to %s",
			origin, *stateKey, sender.Domain())
	}
	if event.RoomID().String() != roomID {
		return matrix.NewForbidden("the event is for room %q, not %q", event.RoomID(), roomID)
	}
	var content struct {
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(event.Content(), &content); err != nil {
		return matrix.NewInvalidParam("the knock event's content cannot be read")
	}
	if content.Membership != string(matrix.MembershipKnock) {
		return matrix.NewInvalidParam("send_knock carries membership %q, not knock", content.Membership)
	}
	return nil
}

// CheckKnock is the door check `send_knock` runs before it admits a peer's knock. It is a method so the
// transport reaches it through the same seam as every other membership decision.
func (s *Service) CheckKnock(ctx context.Context, roomID, origin string,
	event gomatrixserverlib.PDU) error {
	_ = ctx
	return CheckFederationKnock(event, roomID, origin)
}
