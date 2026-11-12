package roomserver

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// authorisedViaKey is the membership content field MSC3083 defines for the user whose membership in
// another room authorised this join.
const authorisedViaKey = "join_authorised_via_users_server"

// AuthorisedViaForJoin works out whether this server can authorise a local user's join to a restricted
// room, and if so which of its own users signs for that authorisation.
//
// A `restricted` room admits people who are already in one of the rooms its `allow` list names. The
// rule lives in the room being joined, but the evidence for it lives somewhere else, so the join event
// has to carry the name of a user of *this* server who is in the joined room and entitled to invite —
// the join is then signed by the server that can vouch for that user, which is what makes the claim
// worth anything to another homeserver later.
//
// The decision is the library's, deliberately: gomatrixserverlib's `CheckRestrictedJoin` is the same
// code path a remote server uses to authorise a federated join, and a homegrown copy of those rules is
// how one server ends up letting people into rooms another server still refuses. An empty answer is not
// an error — it means the room does not need an authorisation (public, invite-only with an invite
// pending, or the caller simply has no claim to make), and the ordinary event authorisation decides.
func (s *Service) AuthorisedViaForJoin(ctx context.Context, roomID, userID string) (string, error) {
	version, err := s.RoomVersionFor(ctx, roomID)
	if err != nil {
		// A room this server does not hold is joined by asking a server that does, and that server
		// authors the join; a room whose version cannot be resolved cannot be authorised here
		// either. Both end the same way: this function declines to answer, and the event
		// authoriser, which has stricter information, makes the decision.
		return "", nil
	}
	roomSpec, err := spec.NewRoomID(roomID)
	if err != nil {
		return "", nil
	}
	via, err := version.CheckRestrictedJoin(ctx, spec.ServerName(s.cfg.ServerName),
		restrictedJoinQuerier{service: s, targetRoomID: roomID, targetSender: userID},
		*roomSpec, spec.SenderID(userID))
	slog.Debug("roomserver: asked whether this server authorises a join",
		"room_id", roomID, "user_id", userID, "authorised_via", via, "error", err)
	if err != nil {
		// Both answers the library gives are refusals to authorise, not faults in this server: one
		// says "you are not joined to any matching room", the other "I cannot tell, ask a server
		// that is still in the room". Neither is a reason to fail the join here — the event
		// authoriser applies the rule and reports it to the client with the right error code.
		return "", nil
	}
	return via, nil
}

// restrictedJoinQuerier answers gomatrixserverlib's MSC3083 questions out of this server's store.
//
// Three things have to be answerable: the room's own state, whether an invite is pending for the
// joining user, and — the interesting one — whether this server's view of another room's memberships
// can still be trusted. That last question is why restricted joins cannot be decided by reading a
// membership table blindly: a server that left the authorising room years ago still has the rows, and
// honouring them would let a room's rule be satisfied by a membership that stopped being maintained.
type restrictedJoinQuerier struct {
	service *Service
	// targetRoomID is the room being joined. A member of the allowed room is only a usable
	// authoriser if this server can also produce their membership in the room being entered:
	// the join event is checked against that room's state, and a name whose membership cannot be
	// found there is not an authorisation, it is an unfindable reference. The joiner is excluded
	// for the same reason a witness cannot testify on their own behalf — naming themselves would
	// make every membership in the allowed room a self-service key.
	targetRoomID string
	targetSender string
}

// CurrentStateEvent reads one state event of a room. A missing event is reported as no event rather
// than as an error, because "this room has no join-rules event" is a fact the caller acts on: no join
// rules means nothing restricts membership.
func (q restrictedJoinQuerier) CurrentStateEvent(ctx context.Context, roomID spec.RoomID,
	eventType string, stateKey string) (gomatrixserverlib.PDU, error) {
	entry, err := q.service.store.State.Get(ctx, roomID.String(), eventType, stateKey)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	row, err := q.service.store.Events.ByEventID(ctx, entry.EventID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	version, err := q.service.RoomVersionFor(ctx, roomID.String())
	if err != nil {
		return nil, err
	}
	return version.NewEventFromTrustedJSON(json.RawMessage(row.JSON), false)
}

// InvitePending reports a live invitation, which is the other way into a restricted room and needs no
// authorising user because the invite itself is the credential.
func (q restrictedJoinQuerier) InvitePending(ctx context.Context, roomID spec.RoomID,
	senderID spec.SenderID) (bool, error) {
	member, err := q.membership(ctx, roomID.String(), senderID)
	if err != nil {
		return false, err
	}
	return member != nil && member.Membership == matrix.MembershipInvite, nil
}

// RestrictedRoomJoinInfo reports whether this server is still a member of the room whose membership is
// being used as grounds for a join, whether the joining user is in it, and which of this server's own
// users could sign for the authorisation.
//
// `JoinedUsers` is deliberately limited to this server's own users. The authorising user is not a
// witness who is called to court, only a name in a field: what is verified is the signature over the
// join event, made by the server that user's ID names. Offering a remote user would produce a join this
// server has signed but cannot be attributed to that user's server, and the receiving server would be
// right to reject it.
func (q restrictedJoinQuerier) RestrictedRoomJoinInfo(ctx context.Context, roomID spec.RoomID,
	senderID spec.SenderID, localServerName spec.ServerName) (*gomatrixserverlib.RestrictedRoomJoinInfo,
	error) {
	target, err := q.membership(ctx, roomID.String(), senderID)
	if err != nil {
		return nil, err
	}
	members, err := q.service.store.Memberships.List(ctx, roomID.String(),
		[]string{matrix.MembershipJoin}, nil)
	if err != nil {
		return nil, err
	}
	info := &gomatrixserverlib.RestrictedRoomJoinInfo{
		UserJoinedToRoom: target != nil && target.Membership == matrix.MembershipJoin,
	}
	version, err := q.service.RoomVersionFor(ctx, roomID.String())
	if err != nil {
		// Without a room version a membership cannot be turned into the signed event the caller
		// needs, which is the same as having no joined users to offer.
		return info, nil
	}
	for _, member := range members {
		if _, domain, err := matrix.ParseUserID(member.UserID, string(localServerName)); err != nil ||
			domain != string(localServerName) {
			continue
		}
		info.LocalServerInRoom = true
		if !info.UserJoinedToRoom {
			// The joining user is not in this room, so this room cannot be the grounds for the
			// join and there is nobody to name.
			continue
		}
		if member.UserID == q.targetSender {
			continue
		}
		// The authoriser has to be somebody in the room being joined as well, whose membership
		// this server can hand to the authoriser as proof.
		if witness, err := q.service.store.Memberships.Get(ctx, q.targetRoomID, member.Localpart); err != nil ||
			witness.Membership != matrix.MembershipJoin {
			continue
		}
		// The membership table is a routing index: it says who is in the room and does not keep the
		// event that says so. An authorisation has to be a signed event, so it is read from the
		// room's current state, which is where a membership event lives anyway.
		entry, err := q.service.store.State.Get(ctx, roomID.String(), matrix.EventTypeMember, member.UserID)
		if err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		row, err := q.service.store.Events.ByEventID(ctx, entry.EventID)
		if err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		event, err := version.NewEventFromTrustedJSON(json.RawMessage(row.JSON), false)
		if err != nil {
			// A membership this server cannot re-read is not an authorisation. Skipping it is
			// safe in both directions: it can only make the authorisation harder to obtain.
			continue
		}
		info.JoinedUsers = append(info.JoinedUsers, event)
	}
	slog.Debug("roomserver: reported a room's memberships as grounds for a restricted join",
		"room_id", roomID.String(), "local_server_in_room", info.LocalServerInRoom,
		"user_joined", info.UserJoinedToRoom, "authorising_users", len(info.JoinedUsers))
	return info, nil
}

// membership reads one user's membership in a room, tolerating both forms the caller can hand over:
// MSC3083 speaks of senders, and in the room versions that have restricted joins a sender is a user ID.
// A user this server has never heard of has no row, which is reported as no membership rather than as
// a failure — "not in the room" is an answer.
func (q restrictedJoinQuerier) membership(ctx context.Context, roomID string,
	senderID spec.SenderID) (*store.Membership, error) {
	// The row key is a local part for this server's own users and a whole user ID for anybody
	// else's — the same rule every other membership read uses, and what stops two homeservers'
	// "bob" from answering for each other here.
	member, err := q.service.store.Memberships.Get(ctx, roomID,
		matrix.MembershipKey(string(senderID), string(q.service.cfg.ServerName)))
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return member, nil
}
