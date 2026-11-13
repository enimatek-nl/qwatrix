package roomserver

// Offering a peer the place in this room's graph where its user's leave belongs.
//
// This is the handshake in front of `send_leave`: a server whose user is leaving — most often a user
// rejecting an invitation, who never joined and so holds none of the room's history to build on — asks the
// server that hosts the room for a draft event, signs it with its own key, and hands it back. Serving the
// draft is not a permission slip: the peer still signs, and `send_leave` still refuses an event whose
// sender does not belong to the server that signed it. What this server supplies is the one thing the
// leaving server cannot know: where the room's history currently ends.
//
// A leave for a user this room has never heard of is refused rather than drafted. Anybody who can reach
// this endpoint could then obtain a plausible event about a stranger, signed-off by a server that has no
// idea who they are, and a room's member list is not a thing to offer opinions about by accident.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/matrix-org/gomatrixserverlib"
)

// LeaveTemplate is the draft event `make_leave` answers with.
type LeaveTemplate struct {
	RoomVersion gomatrixserverlib.RoomVersion `json:"room_version"`
	Event       gomatrixserverlib.ProtoEvent  `json:"event"`
}

// MakeLeaveTemplate answers `GET /_matrix/federation/v1/make_leave/{roomID}/{userID}` and its v2.
func (s *Service) MakeLeaveTemplate(ctx context.Context, roomID, userID string) (*LeaveTemplate, error) {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			// Worded the way the join handshake words its refusal: whether a room ID exists here is not
			// this server's to publish, and a peer that is not in it learns nothing by asking twice.
			return nil, matrix.NewForbidden("this server cannot leave that room")
		}
		return nil, err
	}
	version, err := s.events.RoomVersion(string(gomatrixserverlib.RoomVersion(room.Version)))
	if err != nil || version == nil {
		return nil, matrix.NewIncompatibleRoomVersion(room.Version)
	}
	parsed, err := s.parseUser(userID)
	if err != nil {
		return nil, err
	}
	member, err := s.membershipOf(ctx, roomID, parsed)
	if err != nil {
		return nil, err
	}
	if member == nil {
		return nil, matrix.NewForbidden("this room has no member %q to leave", userID)
	}

	// The same authorising set a join is drafted with — the room's create event, its rules, and the
	// user's own membership — because what is changing is the membership, and the events that say a
	// membership may change are the same ones either way.
	authEventIDs, err := s.joinAuthEventIDs(ctx, roomID, parsed)
	if err != nil {
		return nil, err
	}
	tip, err := s.store.Events.Tip(ctx, roomID)
	if err != nil && !store.IsNotFound(err) {
		return nil, err
	}
	prevEvents := []string{}
	depth := int64(1)
	if tip != nil {
		prevEvents = append(prevEvents, tip.EventID)
		depth = tip.Depth + 1
	}
	content, err := json.Marshal(map[string]any{"membership": string(matrix.MembershipLeave)})
	if err != nil {
		return nil, fmt.Errorf("encoding the leave template content: %w", err)
	}

	return &LeaveTemplate{
		RoomVersion: gomatrixserverlib.RoomVersion(room.Version),
		Event: gomatrixserverlib.ProtoEvent{
			Type:     string(matrix.EventTypeMember),
			StateKey: stateKeyOf(userID),
			RoomID:   roomID,
			Depth:    depth,
			// `sender`, `origin` and `origin_server_ts` are left to the peer for the same reason a join
			// template leaves them: the peer says who the event is from, when its clock said so, and signs
			// the result. This server offers a place in the graph and nothing else.
			PrevEvents: prevEvents,
			AuthEvents: authEventIDs,
			Content:    content,
		},
	}, nil
}

// membershipOf returns the room's current membership row for a user ID, or nil when the room has never
// held one.
//
// The comparison is against the full user ID rather than the localpart: two homeservers hand out `alice`
// as freely as each other, and a leave drafted for the wrong Alice is a statement that a real person left
// a room they were never in.
func (s *Service) membershipOf(ctx context.Context, roomID, userID string) (*store.Membership, error) {
	members, err := s.store.Memberships.List(ctx, roomID, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("reading this room's members: %w", err)
	}
	for i := range members {
		if strings.EqualFold(members[i].UserID, userID) {
			return &members[i], nil
		}
	}
	return nil, nil
}
