package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
)

// TestAPeerMayEndAMembershipByTheLegacyLeaveEndpoint serves the federation API as it was written
// before room v11 moved leaves into the ordinary event stream.
//
// `PUT /_matrix/federation/v1/send_leave/{roomID}/{eventID}` is the older door: one event, delivered
// on its own rather than inside a transaction, and an empty answer. It is what a homeserver that has
// not been updated, and complement's reference federation server, still use to say "this user is out".
// Refusing it with M_UNRECOGNIZED does not make the leave not happen — it leaves the two servers
// disagreeing about who is in the room, with the peer holding a leave event this server never took.
//
// What it must not become is a second `/send` with less checking on it. The endpoint names the room
// and the event in the path, and each of those is a place a peer can lie.
func TestAPeerMayEndAMembershipByTheLegacyLeaveEndpoint(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	remote := "@charlie:" + string(peer.serverNo)

	if members := roomMembers(t, alice, server, roomID); !members[remote] {
		t.Fatalf("the fixture is wrong: %s is not in the room before the leave (%v)", remote, members)
	}

	state := remoteStateEventIDs(t, server, alice, roomID)
	leave := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.member",
		StateKey:   strPtr(remote),
		Depth:      join.Depth() + 1,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			state["m.room.create\x00"],
			state["m.room.power_levels\x00"],
			join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"membership": "leave"})),
	})

	path := "/_matrix/federation/v1/send_leave/" + harness.PathEscape(roomID, leave.EventID())
	response := signedFederationPut(t, server, peer, path, json.RawMessage(leave.JSON()))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a peer's leave was refused with %d (%s), want 200",
			response.StatusCode, federationBody(t, response))
	}
	if body := string(federationBody(t, response)); gjson.Parse(body).Get("errcode").Exists() {
		t.Errorf("the answer to a leave is an empty object, got %s", body)
	}

	// The point of accepting the event is that the room's membership changes. A leave that is
	// stored but not applied leaves a member list with a ghost in it.
	if members := roomMembers(t, alice, server, roomID); members[remote] {
		t.Errorf("%s still holds a membership after the leave the peer sent", remote)
	}
}

// TestTheLegacyLeaveEndpointRefusesWhatItCannotUse keeps the endpoint narrower than `/send`.
//
// It takes one event and promises a membership change, so anything else is either a mistake or an
// attempt to use a lightly-checked door to push an arbitrary event. The room and the event ID are in
// the path, so both are checked against the event: a body naming another room would let a peer who
// is trusted for one room write state into a room this server never admitted it to.
func TestTheLegacyLeaveEndpointRefusesWhatItCannotUse(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)
	authEvents := []string{
		state["m.room.create\x00"],
		state["m.room.power_levels\x00"],
		join.EventID(),
	}

	t.Run("an event that is not a membership", func(t *testing.T) {
		message := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
			RoomID:     roomID,
			SenderID:   remote,
			Type:       "m.room.message",
			Depth:      join.Depth() + 1,
			PrevEvents: []string{join.EventID()},
			AuthEvents: authEvents,
			Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "not a leave"})),
		})
		response := signedFederationPut(t, server, peer,
			"/_matrix/federation/v1/send_leave/"+harness.PathEscape(roomID, message.EventID()),
			json.RawMessage(message.JSON()))
		if response.StatusCode < 400 {
			body := federationBody(t, response)
			t.Errorf("a message sent to the leave endpoint was accepted with %d (%s)", response.StatusCode, body)
		}
	})

	t.Run("an event for another room", func(t *testing.T) {
		other := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
		leave := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
			RoomID:     other,
			SenderID:   remote,
			Type:       "m.room.member",
			StateKey:   strPtr(remote),
			Depth:      join.Depth() + 1,
			PrevEvents: []string{join.EventID()},
			AuthEvents: authEvents,
			Content:    spec.RawJSON(mustMarshal(t, map[string]any{"membership": "leave"})),
		})
		// The path names the room this server knows the peer in; the event names another one.
		response := signedFederationPut(t, server, peer,
			"/_matrix/federation/v1/send_leave/"+harness.PathEscape(roomID, leave.EventID()),
			json.RawMessage(leave.JSON()))
		if response.StatusCode < 400 {
			body := federationBody(t, response)
			t.Errorf("a leave for a room the path does not name was accepted with %d (%s)",
				response.StatusCode, body)
		}
	})
}

// TestAPeerReadsTheAuthChainOfAnEvent is the read a joining server uses to check an event it is
// told about but cannot see the rules for.
//
// `GET /_matrix/federation/v1/event_auth/{roomID}/{eventID}` answers with the closure of auth events
// behind one event — the set of state that decided whether it was allowed. It is what a peer needs to
// verify an event whose own auth events it does not hold, and without it a server cannot check a
// history someone is showing it, so it either believes the peer or refuses the room.
//
// The answer is the chain and nothing else: this is a room the requester is already in, and the auth
// chain is a small, fixed set. Returning the whole room's state instead is the mistake the test at
// the far end of this is named after — a peer gets more than it asked for and this server has
// disclosed state it was not asked about.
func TestAPeerReadsTheAuthChainOfAnEvent(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, roomVersion := joinedRoom(t, server, alice, peer, "public_chat")

	status, body := federationGetSigned(t, server, peer,
		"/_matrix/federation/v1/event_auth/"+harness.PathEscape(roomID, join.EventID()))
	if status != http.StatusOK {
		t.Fatalf("a peer asked for an auth chain and got %d (%s)", status, body)
	}
	answer := gjson.GetBytes(body, "auth_chain")
	if !answer.IsArray() {
		t.Fatalf("the answer has no auth_chain array: %s", body)
	}
	// The peer identifies each event by recomputing its ID from the JSON under the room's own
	// version, because from room v4 on the ID is not written inside the event. A server that
	// returned an event whose ID does not recompute would hand back a chain that names nothing.
	impl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(roomVersion))
	if err != nil {
		t.Fatalf("room version %q: %v", roomVersion, err)
	}
	version := impl
	ids := map[string]bool{}
	for _, event := range answer.Array() {
		raw := json.RawMessage(event.Raw)
		parsed, err := version.NewEventFromUntrustedJSON(raw)
		if err != nil {
			t.Fatalf("an event of the chain cannot be read under the room's version: %v", err)
		}
		if parsed.RoomID().String() != roomID {
			t.Errorf("the chain holds an event of another room (%s)", parsed.RoomID().String())
		}
		ids[parsed.EventID()] = true
	}
	if len(ids) == 0 {
		t.Fatalf("the auth chain of an event is empty: %s", body)
	}
	// The chain is the closure of what authorised the event, so at minimum it holds the event's own
	// auth events: the create, the power levels, the join rules.
	for _, want := range join.AuthEventIDs() {
		if !ids[want] {
			t.Errorf("the chain does not include %s, which authorised the event", want)
		}
	}

	// A peer that asks about an event this server has never seen is told so, in the same shape it
	// would get for a room it is not in: the answer must not confirm which event IDs are real.
	status, body = federationGetSigned(t, server, peer,
		"/_matrix/federation/v1/event_auth/"+harness.PathEscape(roomID, "$nothinghere"))
	if status == http.StatusOK {
		t.Errorf("an unknown event ID answered 200 with %s", body)
	}
	if status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("an unknown event ID answered %d, want a refusal that names nothing about why", status)
	}
}

// TestTheAuthChainReadNeedsTheRoom keeps the read behind the room. An auth chain is a slice of a
// room's history, so a server that hands it out to any peer is a history reader for anybody who can
// name a room and an event in it.
func TestTheAuthChainReadNeedsTheRoom(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})

	// The peer has never been told about this room, so it cannot ask about an event in it — and the
	// refusal cannot be a 404 naming the room, which would confirm the room exists.
	status, body := federationGetSigned(t, server, peer,
		"/_matrix/federation/v1/event_auth/"+harness.PathEscape(roomID, "$whatever"))
	if status == http.StatusOK {
		t.Errorf("an auth chain was handed out for a room the peer is not in: %s", body)
	}
	if status < 400 {
		t.Errorf("status %d, want a refusal (%s)", status, body)
	}
}

func strPtr(value string) *string { return &value }
