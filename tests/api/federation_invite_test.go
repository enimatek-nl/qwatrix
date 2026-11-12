package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
)

// An invite is signed by two servers, and this is the one people forget.
//
// The event says "user X is invited to this room". The inviter's signature proves the inviter asked
// for it; the invited user's own server has to agree that it is willing to hand that invitation to
// its user, because the invitee will act on the event — decrypting with it, showing the room, joining
// from it — and the only party standing between the invitee and an event about them is the server
// that user trusts. So the specification asks both servers to sign, and a receiving server that sees
// an invite with one signature refuses it.
//
// `PUT /_matrix/federation/v2/invite/{roomID}/{eventID}` is how the inviter asks for that second
// signature: it hands over the event, the room version, and enough stripped state for the invitee's
// server to describe a room it may never have heard of. The answer is the same event, now carrying the
// invited server's signature too.
//
// Without this endpoint a server cannot invite anybody who lives elsewhere. The event reaches the
// peer inside an ordinary transaction and is refused there as having the wrong number of signatures —
// a refusal the sender reads as a peer problem, since the transaction itself was accepted.
func TestTheInviteEndpointSignsAnInviteForItsOwnUser(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	inviter := "@charlie:" + string(peer.serverNo)

	// An invitation for a member would be an odd thing to authorise, and this is the sequence that
	// really happens: alice leaves, and is invited back.
	alice.MustLeaveRoom(t, server, roomID)

	state := remoteStateEventIDs(t, server, alice, roomID)
	invite := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   inviter,
		Type:       "m.room.member",
		StateKey:   strPtr(alice.UserID),
		Depth:      join.Depth() + 2,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			state["m.room.create\x00"],
			state["m.room.power_levels\x00"],
			join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"membership": "invite"})),
	})

	response := signedFederationPut(t, server, peer,
		"/_matrix/federation/v2/invite/"+harness.PathEscape(roomID, invite.EventID()),
		json.RawMessage(mustMarshal(t, map[string]any{
			"event":             json.RawMessage(invite.JSON()),
			"room_version":      version,
			"invite_room_state": []any{},
		})))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the invite to %s was refused with %d (%s), want 200",
			alice.UserID, response.StatusCode, federationBody(t, response))
	}

	body := federationBody(t, response)
	// The signature is the whole answer. A peer cannot use an event that is merely acknowledged.
	signed := gjson.GetBytes(body, "event.signatures")
	if !signed.Get(gjson.Escape(string(server.ServerName))).Exists() {
		t.Errorf("the answer carries no signature from this server, so the inviter cannot deliver the "+
			"invite: signatures=%s", signed.Raw)
	}
	if !signed.Get(gjson.Escape(string(peer.serverNo))).Exists() {
		t.Errorf("the co-signed invite lost the inviter's signature: signatures=%s", signed.Raw)
	}
	if got := gjson.GetBytes(body, "room_version").String(); got != version {
		t.Errorf("the answer named room version %q; the inviter needs the room's own version to parse "+
			"the event it gets back, asked for %q", got, version)
	}

	// Signing is not the only thing the invited server does with an invitation: it tells its user.
	// The room is one this server already holds, so the invite belongs in alice's sync from now on.
	synced := alice.MustGet(t, server, "/_matrix/client/v3/sync?timeout=0")
	invites := gjson.GetBytes(synced.Body, "rooms.invite")
	if !invites.Get(gjson.Escape(roomID)).Exists() {
		t.Errorf("alice was never shown the invitation to %s: rooms.invite=%s", roomID, invites.Raw)
	}
}

// TestTheInviteEndpointSignsNobodyElsesUser keeps the endpoint from becoming a signing oracle.
//
// The signature this server adds says "we are willing to give this invitation to our user". Asked
// about a user this server does not have, it would be attesting to something it cannot know — and any
// peer could arrange that, since the request needs only the inviter's signature.
func TestTheInviteEndpointSignsNobodyElsesUser(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	inviter := "@charlie:" + string(peer.serverNo)
	foreigner := "@someone:somewhere.example"

	state := remoteStateEventIDs(t, server, alice, roomID)
	invite := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   inviter,
		Type:       "m.room.member",
		StateKey:   strPtr(foreigner),
		Depth:      join.Depth() + 1,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			state["m.room.create\x00"],
			state["m.room.power_levels\x00"],
			join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"membership": "invite"})),
	})

	response := signedFederationPut(t, server, peer,
		"/_matrix/federation/v2/invite/"+harness.PathEscape(roomID, invite.EventID()),
		json.RawMessage(mustMarshal(t, map[string]any{
			"event":        json.RawMessage(invite.JSON()),
			"room_version": version,
		})))
	if response.StatusCode == http.StatusOK {
		t.Errorf("this server signed an invitation for %q, a user it does not have: %s",
			foreigner, federationBody(t, response))
	}
}

// TestTheInviteEndpointRefusesAnInviteTheInviterDidNotSign checks the order of operations: this server
// signs an invite only when another server has already said it issued it.
//
// The inviter's signature is what makes the event an instruction rather than a guess, and an endpoint
// that added its own signature to an unsigned event would let anyone put an invitation in front of a
// user with the name of any willing member of the room in it.
func TestTheInviteEndpointRefusesAnInviteTheInviterDidNotSign(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")

	// A different peer's key, with the first peer's user in the sender field: the signature is
	// well-formed and names a domain that did not sign this event.
	other := newFakePeer(t)
	state := remoteStateEventIDs(t, server, alice, roomID)
	invite := buildRemoteEvent(t, other, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   "@" + "charlie:" + string(peer.serverNo),
		Type:       "m.room.member",
		StateKey:   strPtr(alice.UserID),
		Depth:      join.Depth() + 1,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			state["m.room.create\x00"],
			state["m.room.power_levels\x00"],
			join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"membership": "invite"})),
	})

	response := signedFederationPut(t, server, peer,
		"/_matrix/federation/v2/invite/"+harness.PathEscape(roomID, invite.EventID()),
		json.RawMessage(mustMarshal(t, map[string]any{
			"event":        json.RawMessage(invite.JSON()),
			"room_version": version,
		})))
	if response.StatusCode == http.StatusOK {
		t.Errorf("an invite signed by some other server was co-signed anyway: %s",
			federationBody(t, response))
	}
}

// TestAnInviteOfARemoteUserArrivesCoSigned drives the same rule from the other side: a server that
// invites somebody who lives elsewhere has to go and get that user's server to sign, because a peer
// refuses the event otherwise.
//
// The sequence is the one that keeps failing in complement's `TestUnbanViaInvite`. Alice joins a room
// on another homeserver, leaves it, and is invited back. Her own server holds the room and so can
// accept an invitation into it — everything that goes wrong happens before that, on the way out: the
// invitation is built, queued, and relayed as an ordinary event, and the peer declines it for having
// one signature where two are needed. Since a `/send` transaction answers 200 whatever it refused
// inside, the sending side has historically read that as success.
func TestAnInviteOfARemoteUserArrivesCoSigned(t *testing.T) {
	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	bob := hs2.Register(t, "bob", "pw-bob", false)
	roomID := bob.MustCreateRoom(t, hs2, map[string]any{"preset": "public_chat"})

	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	alice.MustPostJSON(t, hs1, "/_matrix/client/v3/join/"+url.PathEscape(roomID)+
		"?server_name="+url.QueryEscape(hs2.ServerName), map[string]any{}).MustStatus(t, 200)
	hs1.DrainFederation(t)
	alice.MustLeaveRoom(t, hs1, roomID)
	// The leave is alice's server's event, so it is alice's server that owes hs2 the copy; without
	// this the room on hs2 still holds alice as joined and an invitation there is a rule violation
	// rather than a re-invitation.
	hs1.DrainFederation(t)
	hs2.DrainFederation(t)

	invite := bob.Do(t, hs2, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/invite",
		Body:   map[string]any{"user_id": alice.UserID},
	})
	if invite.StatusCode != http.StatusOK {
		t.Fatalf("inviting a remote member answered %d: %s", invite.StatusCode, invite.Body)
	}

	for attempt := 0; attempt < 6; attempt++ {
		hs2.DrainFederation(t)
		hs1.DrainFederation(t)
		synced := alice.MustGet(t, hs1, "/_matrix/client/v3/sync?timeout=0")
		entry := gjson.GetBytes(synced.Body, "rooms.invite."+gjson.Escape(roomID))
		if entry.Exists() {
			// The stripped state is the invitee's only view of a room they cannot read.
			if events := entry.Get("invite_state.events"); !events.IsArray() || len(events.Array()) == 0 {
				t.Errorf("the invitation arrived with no stripped state, so alice is invited to a room "+
					"she cannot see anything about: %s", entry.Raw)
			}
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("the invitation never reached alice's server: it is refused there for carrying one "+
		"signature where two are needed (hs2 %q invites %s into %s)", hs2.ServerName, alice.UserID, roomID)
}
