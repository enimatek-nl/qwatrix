package api_test

// The handshake in front of a leave: a peer asking where this room's history ends so it can sign a leave
// that attaches to it.
//
// The case that needs it is a user rejecting an invitation. They never joined, so they hold none of the
// room's history to build an event on, and without this endpoint the rejection has nowhere to be placed —
// which leaves the inviter looking at an invitation that can never go away, and the invitee's server
// holding a membership its own user has already refused.
//
// The test that matters is the round trip: this server hands out a draft, the peer signs it, and this
// server's own `send_leave` accepts it back. A draft that its own door refuses is worse than no draft,
// because the peer has now done everything right and been refused for it.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
)

// makeMembershipTemplate asks this server for a leave draft, as a peer would, and returns the room version and
// the unsigned event.
func makeMembershipTemplate(t *testing.T, server *harness.Server, peer *fakePeer, roomID, userID string,
	versioned bool) (gomatrixserverlib.ProtoEvent, string) {
	t.Helper()

	path := "/_matrix/federation/v1/make_leave/" + harness.PathEscape(roomID) + "/" +
		harness.PathEscape(userID)
	if versioned {
		path = "/_matrix/federation/v2/make_leave/" + harness.PathEscape(roomID) + "/" +
			harness.PathEscape(userID)
	}
	status, body := federationGetSigned(t, server, peer, path)
	if status != http.StatusOK {
		t.Fatalf("make_leave got %d, want 200: %s", status, body)
	}
	return templateOf(t, body)
}

// templateOf reads a make_join-shaped answer: the room version and the draft event.
func templateOf(t *testing.T, body []byte) (gomatrixserverlib.ProtoEvent, string) {
	t.Helper()

	var answer struct {
		RoomVersion string                       `json:"room_version"`
		Event       gomatrixserverlib.ProtoEvent `json:"event"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer is not the documented shape: %v\n%s", err, body)
	}
	return answer.Event, answer.RoomVersion
}

// countPredecessors reads a draft's predecessors, which a room version writes either as event IDs or, in
// the two oldest versions, as objects — so it is read through JSON rather than asserted about.
func countPredecessors(t *testing.T, predecessors any) int {
	t.Helper()

	if predecessors == nil {
		return 0
	}
	encoded, err := json.Marshal(predecessors)
	if err != nil {
		t.Fatalf("the draft's predecessors cannot be read: %v", err)
	}
	var ids []json.RawMessage
	if err := json.Unmarshal(encoded, &ids); err != nil {
		t.Fatalf("the draft's predecessors are not a list: %s", encoded)
	}
	return len(ids)
}

// signTemplate fills in what a draft deliberately leaves out — who it is from, and when the sending server's
// clock said so — and signs it, which is the whole of what the leaving server contributes.
func signTemplate(t *testing.T, event gomatrixserverlib.ProtoEvent, version string, peer *fakePeer,
	userID string) gomatrixserverlib.PDU {
	t.Helper()

	event.SenderID = userID
	impl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(version))
	if err != nil {
		t.Fatalf("room version %q: %v", version, err)
	}
	signed, err := impl.NewEventBuilderFromProtoEvent(&event).
		Build(time.Now(), peer.serverNo, peer.keyID, peer.priv)
	if err != nil {
		t.Fatalf("signing the draft: %v", err)
	}
	return signed
}

func TestAPeerPlacesItsUsersLeaveWhereThisRoomCanTakeIt(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, _ := joinedRoom(t, server, alice, peer, "public_chat")
	remote := "@charlie:" + string(peer.serverNo)

	event, version := makeMembershipTemplate(t, server, peer, roomID, remote, false)
	if event.Type != "m.room.member" {
		t.Fatalf("the draft is a %q, not a membership", event.Type)
	}
	if event.StateKey == nil || *event.StateKey != remote {
		t.Fatalf("the draft is about %v, want %s", event.StateKey, remote)
	}
	if event.RoomID != roomID {
		t.Fatalf("the draft names room %q, want %s", event.RoomID, roomID)
	}
	var content struct {
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(event.Content, &content); err != nil || content.Membership != "leave" {
		t.Fatalf("the draft carries content %s, want a leave", event.Content)
	}
	// The place in the graph is the point of the answer: without a predecessor and a depth the peer
	// cannot attach anything.
	if countPredecessors(t, event.PrevEvents) == 0 {
		t.Fatalf("the draft names no predecessor, so the leave would be an orphan:\n%v", event)
	}
	if event.Depth < join.Depth() {
		t.Fatalf("the draft sits at depth %d, before the join at %d", event.Depth, join.Depth())
	}
	// Identity is left to the signer: a draft carrying `sender` would be this server describing a user
	// it has no authority over, and a peer that copies the field verbatim would sign a claim from the
	// wrong mouth.
	if event.SenderID != "" {
		t.Fatalf("the draft names a sender %q; the leaving server says who is leaving", event.SenderID)
	}

	// The round trip: sign what this server handed out and put it back through the door of the same name.
	signed := signTemplate(t, event, version, peer, remote)
	response := signedFederationPut(t, server, peer,
		"/_matrix/federation/v2/send_leave/"+harness.PathEscape(roomID)+"/"+harness.PathEscape(signed.EventID()),
		json.RawMessage(signed.JSON()))
	defer func() { _ = response.Body.Close() }()
	body := federationBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("this server refused the leave it drafted itself with %d: %s", response.StatusCode, body)
	}

	// And the room agrees: charlie is no longer in it.
	members := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/joined_members")
	if containsMember(string(members.Body), remote) {
		t.Fatalf("%s is still listed after a leave this server drafted and accepted:\n%s", remote, members.Body)
	}
}

func TestAMakeLeaveDraftIsRefusedWhereTheRoomHasNoSuchMember(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, _, _ := joinedRoom(t, server, alice, peer, "public_chat")

	// A user this room has never heard of. Handing out a draft about them would be this server offering
	// a plausible membership event for a stranger, endorsed by a server that cannot know them.
	stranger := "@nobody:" + string(peer.serverNo)
	path := "/_matrix/federation/v1/make_leave/" + harness.PathEscape(roomID) + "/" +
		harness.PathEscape(stranger)
	if status, body := federationGetSigned(t, server, peer, path); status == http.StatusOK {
		t.Fatalf("a leave was drafted for %s, who this room never had: %s", stranger, body)
	}

	// A room this server has never heard of is refused the same way the join handshake refuses it, so
	// that make_leave cannot be used to ask whether a room ID exists here.
	unknown := "/_matrix/federation/v1/make_leave/" + harness.PathEscape("!nosuchroom:example.invalid") + "/" +
		harness.PathEscape(stranger)
	if status, body := federationGetSigned(t, server, peer, unknown); status == http.StatusOK {
		t.Fatalf("an unknown room offered a leave template: %s", body)
	}

	// Unsigned, the request is not from a server at all.
	unsigned, err := server.FederationClient(t).Get(server.FederationServer(t).URL +
		"/_matrix/federation/v1/make_leave/" + harness.PathEscape(roomID) + "/" +
		harness.PathEscape("@charlie:"+string(peer.serverNo)))
	if err != nil {
		t.Fatalf("GET make_leave: %v", err)
	}
	defer func() { _ = unsigned.Body.Close() }()
	if unsigned.StatusCode == http.StatusOK {
		t.Fatalf("an unsigned make_leave was answered")
	}
}
