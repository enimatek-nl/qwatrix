package api_test

// The membership endpoints take one membership each, and refuse every other event.
//
// Both endpoints exchange a single membership for a reason: `send_join` answers with the room's state, so
// a server that handed it something else would be paid in state for an event it was not entitled to;
// `send_leave` answers with nothing at all, and it is the narrower door — no state is exchanged, no
// handshake, no co-signing. A peer that could push an *invite* through the door meant for a leave would
// be writing membership state by a route that never went near the invite handshake, and a peer that could
// push a message through either one has a second `/send` with fewer checks behind it.
//
// So the rule is exact rather than generous, and it matches the specification and Synapse: `send_join`
// accepts a join and `send_leave` accepts a leave. A ban is state and travels in a transaction; a knock
// has its own endpoints; neither belongs here.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// putMembershipEvent puts one signed event at a membership endpoint and returns the status and body.
func putMembershipEvent(t *testing.T, server *harness.Server, peer *fakePeer, base, roomID string,
	event gomatrixserverlib.PDU) (int, string) {
	t.Helper()

	path := base + "/" + harness.PathEscape(roomID) + "/" + harness.PathEscape(event.EventID())
	response := signedFederationPut(t, server, peer, path, json.RawMessage(event.JSON()))
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode, string(federationBody(t, response))
}

// wrongMemberships are every event a membership endpoint has been asked to swallow that it must refuse:
// the two ways an event is not a membership at all, and the memberships that belong to other endpoints.
func wrongMemberships(t *testing.T, peer *fakePeer, version string, roomID,
	sender string, depth int64, state map[string]string, joinID, accept string) []gomatrixserverlib.PDU {
	t.Helper()

	events := []gomatrixserverlib.PDU{
		buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
			RoomID: roomID, SenderID: sender, Type: "m.room.message", Depth: depth,
			PrevEvents: []string{joinID},
			AuthEvents: []string{state["m.room.create\x00"], state["m.room.power_levels\x00"], joinID},
			Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "not a membership"})),
		}),
	}
	// A membership event with the key missing entirely, then each of the memberships that belong
	// elsewhere. A `state_key` of the sender's own is what makes each of these a well-formed membership
	// event, so the refusal cannot be blamed on a malformed one.
	for _, membership := range []string{"", "join", "invite", "knock", "ban"} {
		if membership == accept {
			// The one membership this endpoint exists for, tested separately as the positive case: a
			// refusal-only test is satisfied by an endpoint that refuses everything.
			continue
		}
		content := map[string]any{"membership": membership}
		if membership == "" {
			content = map[string]any{}
		}
		events = append(events, buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
			RoomID: roomID, SenderID: sender, Type: "m.room.member", Depth: depth,
			StateKey:   &sender,
			PrevEvents: []string{joinID},
			AuthEvents: []string{state["m.room.create\x00"], state["m.room.power_levels\x00"], joinID},
			Content:    spec.RawJSON(mustMarshal(t, content)),
		}))
	}
	return events
}

func TestSendJoinAcceptsOnlyAJoin(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)

	for _, event := range wrongMemberships(t, peer, version, roomID, remote, join.Depth()+1, state,
		join.EventID(), "join") {
		status, body := putMembershipEvent(t, server, peer, "/_matrix/federation/v2/send_join", roomID, event)
		if status == http.StatusOK {
			t.Errorf("send_join accepted a %q with membership %q:\n%s", event.Type(),
				membershipIn(t, event), body)
			continue
		}
		// Refused means 400, which is what tells a peer that its *event* was wrong rather than that
		// something is broken here: a 403 would send the peer back the same way forever, and a 500
		// would look like a fault in this server.
		if status != http.StatusBadRequest {
			t.Errorf("send_join answered a %q with %d, want 400: %s", membershipIn(t, event), status, body)
		}
	}

	// And the door still opens for what it is for: a second remote user joining the same room. Without
	// this, a test that only checked refusals could be satisfied by an endpoint that refuses everything.
	other := "@dana:" + string(peer.serverNo)
	dana, _ := buildJoin(t, server, peer, roomID, other, nil)
	response := signedFederationPut(t, server, peer,
		"/_matrix/federation/v2/send_join/"+harness.PathEscape(roomID)+"/"+harness.PathEscape(dana.EventID()),
		json.RawMessage(dana.JSON()))
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Errorf("send_join refused a real join with %d: %s", response.StatusCode,
			federationBody(t, response))
	}
}

func TestSendLeaveAcceptsOnlyALeave(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)

	for _, event := range wrongMemberships(t, peer, version, roomID, remote, join.Depth()+1, state,
		join.EventID(), "leave") {
		status, body := putMembershipEvent(t, server, peer, "/_matrix/federation/v1/send_leave", roomID, event)
		if status == http.StatusOK {
			t.Errorf("send_leave accepted a %q with membership %q:\n%s", event.Type(),
				membershipIn(t, event), body)
			continue
		}
		if status != http.StatusBadRequest {
			t.Errorf("send_leave answered a %q with %d, want 400: %s", membershipIn(t, event), status, body)
		}
	}

	// Charlie leaves, the way the older API does it, and this server says so.
	left := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID: roomID, SenderID: remote, Type: "m.room.member", Depth: join.Depth() + 1,
		StateKey:   &remote,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			state["m.room.create\x00"], state["m.room.power_levels\x00"],
			state["m.room.join_rules\x00"], join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"membership": "leave"})),
	})
	status, body := putMembershipEvent(t, server, peer, "/_matrix/federation/v1/send_leave", roomID, left)
	if status != http.StatusOK {
		t.Fatalf("a real leave was answered with %d, want 200: %s", status, body)
	}

	// The membership has to have happened: a client asking who is in the room must not still be told
	// about the member whose leave this server was handed.
	members := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/joined_members")
	if got := string(members.Body); containsMember(got, remote) {
		t.Fatalf("%s is still a member after a leave signed by its own server:\n%s", remote, got)
	}
}

// membershipIn reads the membership out of an event's content, which is what the endpoint is being asked
// about.
func membershipIn(t *testing.T, event gomatrixserverlib.PDU) string {
	t.Helper()

	var content struct {
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(event.Content(), &content); err != nil {
		t.Fatalf("the event's content cannot be read: %v", err)
	}
	if event.Type() != "m.room.member" {
		return ""
	}
	return content.Membership
}

// containsMember looks for a user in a /joined_members answer.
func containsMember(body, userID string) bool {
	var answer struct {
		Joined map[string]json.RawMessage `json:"joined"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		return true // unreadable: assume the member is listed, so the test fails rather than passes quietly
	}
	_, found := answer.Joined[userID]
	return found
}

// TestAMembershipEndpointRefusesAnEventAboutAnotherRoom keeps the path authoritative after the checks were
// reordered: the room and event ID in the URL are what the peer was trusted with, and a body that names
// something else is a peer trying to write where it was not admitted.
func TestAMembershipEndpointRefusesAnEventAboutAnotherRoom(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	var elsewhere string
	var version string
	roomID, join, _ := joinedRoom(t, server, alice, peer, "public_chat")
	elsewhereRoom, _, elsewhereVersion := joinedRoom(t, server, alice, peer, "public_chat")
	if elsewhereRoom == roomID {
		t.Fatalf("the two rooms are the same (%s)", roomID)
	}
	elsewhere = elsewhereRoom
	remote := "@charlie:" + string(peer.serverNo)
	// A leave for the *other* room — built against that room's version and events — addressed at this
	// one, which is the only way this can be assembled.
	elsewhereState := remoteStateEventIDs(t, server, alice, elsewhere)
	version = elsewhereVersion
	leave := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID: elsewhere, SenderID: remote, Type: "m.room.member", Depth: join.Depth() + 1,
		StateKey:   &remote,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			elsewhereState["m.room.create\x00"], elsewhereState["m.room.power_levels\x00"],
			join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"membership": "leave"})),
	})
	status, body := putMembershipEvent(t, server, peer, "/_matrix/federation/v1/send_leave", roomID, leave)
	if status == http.StatusOK {
		t.Fatalf("a leave naming %q was accepted at the door for %q\n%s", elsewhere, roomID, body)
	}
}
