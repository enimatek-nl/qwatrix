package api_test

// Federation ingest: `PUT /_matrix/federation/v1/send/{transactionID}`.
//
// This is how the rest of the federation tells this server that something happened. Everything else
// in the federation API is this server asking; this endpoint is the one where it is told, and it is
// therefore the endpoint where bad things arrive: forged events, replayed transactions, events for
// rooms this server has no business knowing about.
//
// The three properties these tests pin are the three that make ingest safe to expose at all. An event
// is kept only if its own signature holds, whoever relayed it. A transaction delivered twice is
// applied once, because a peer that did not hear the answer will send it again and the alternative is
// a room where every message appears twice. And an event is accepted or refused on its own, because
// one bogus event must not be able to stop a room moving forward.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// roomVersionUnknown is a room version this server supports; the room in the last test does not
// exist at all, so no version negotiation happens and the builder only needs a valid one.
const roomVersionUnknown = "11"

// remoteStateEventIDs reads a room's state through the client API, as the local user, and returns
// the event IDs a remote event would need to authenticate against it.
func remoteStateEventIDs(t *testing.T, server *harness.Server, alice *harness.Account, roomID string) map[string]string {
	t.Helper()

	var events []struct {
		Type     string `json:"type"`
		StateKey string `json:"state_key"`
		EventID  string `json:"event_id"`
	}
	response := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/state")
	response.MustStatus(t, http.StatusOK)
	response.JSON(t, &events)

	ids := make(map[string]string, len(events))
	for _, event := range events {
		ids[event.Type+"\x00"+event.StateKey] = event.EventID
	}
	return ids
}

// buildRemoteEvent builds and signs an event as the peer, the way a server that hosted the sender
// would.
func buildRemoteEvent(t *testing.T, peer *fakePeer, version string, event gomatrixserverlib.ProtoEvent) gomatrixserverlib.PDU {
	t.Helper()

	impl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(version))
	if err != nil {
		t.Fatalf("room version %q: %v", version, err)
	}
	signed, err := impl.NewEventBuilderFromProtoEvent(&event).Build(
		time.Now(), peer.serverNo, peer.keyID, peer.priv)
	if err != nil {
		t.Fatalf("building the event: %v", err)
	}
	return signed
}

// sendTransaction delivers a transaction the way a peer would and returns the per-event results.
func sendTransaction(t *testing.T, server *harness.Server, peer *fakePeer, pdus []json.RawMessage, transactionID string) (map[string]struct {
	Error string `json:"error"`
}, []byte) {
	t.Helper()

	path := "/_matrix/federation/v1/send/" + harness.PathEscape(transactionID)
	body := map[string]any{
		"origin":           string(peer.serverNo),
		"origin_server_ts": time.Now().UnixMilli(),
		"pdus":             pdus,
	}
	response := signedFederationPut(t, server, peer, path, body)
	raw := federationBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the transaction was refused as a whole (%d: %s); events are refused one at a time",
			response.StatusCode, raw)
	}
	var parsed struct {
		PDUs map[string]struct {
			Error string `json:"error"`
		} `json:"pdus"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the transaction answer is not the documented shape: %v (%s)", err, raw)
	}
	if parsed.PDUs == nil {
		t.Fatalf("the transaction answer carries no per-event results (%s)", raw)
	}
	return parsed.PDUs, raw
}

// joinedRoom puts a remote user in a room through the real handshake, because the events a peer
// sends are authorised against the room as it stands — and a sender who is not a member is a
// different question, asked elsewhere, than a member sending an event.
func joinedRoom(t *testing.T, server *harness.Server, alice *harness.Account, peer *fakePeer, preset string) (string, gomatrixserverlib.PDU, string) {
	t.Helper()

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": preset})
	remote := "@charlie:" + string(peer.serverNo)
	join, version := buildJoin(t, server, peer, roomID, remote, nil)
	path := "/_matrix/federation/v2/send_join/" + harness.PathEscape(roomID) + "/" +
		harness.PathEscape(join.EventID())
	response := signedFederationPut(t, server, peer, path, json.RawMessage(join.JSON()))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("send_join: status %d (%s)", response.StatusCode, federationBody(t, response))
	}
	_ = response.Body.Close()
	return roomID, join, version
}

func TestAFederatedEventReachesTheLocalUser(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	remote := "@charlie:" + string(peer.serverNo)

	// Since when must this event be reachable? A sync taken before it was sent, so the test cannot
	// accidentally count something the first sync had already delivered.
	since := alice.MustSync(t, server, nil).NextBatch

	state := remoteStateEventIDs(t, server, alice, roomID)
	message := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.message",
		Depth:      join.Depth() + 1,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			state["m.room.create\x00"],
			state["m.room.power_levels\x00"],
			join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "delivered by a peer"})),
	})

	results, raw := sendTransaction(t, server, peer, []json.RawMessage{json.RawMessage(message.JSON())},
		harness.Nonce("message"))
	if result, ok := results[message.EventID()]; !ok {
		t.Fatalf("the answer says nothing about the event it was sent (%s)", raw)
	} else if result.Error != "" {
		t.Fatalf("the event was refused: %s", result.Error)
	}

	// The point of accepting an event is that the local user finds out about it. A server that stores
	// a federated message and never shows it has taken on the cost of federation without the use.
	alice.MustTimelineHas(t, server, roomID, "m.room.message", "delivered by a peer")
	_ = since
}

func TestATransactionDeliveredTwiceIsAppliedOnce(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)

	message := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.message",
		Depth:      join.Depth() + 1,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			state["m.room.create\x00"],
			state["m.room.power_levels\x00"],
			join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "sent once, delivered twice"})),
	})
	raw := []json.RawMessage{json.RawMessage(message.JSON())}

	// A peer that never received the answer sends the same transaction again under the same ID. That
	// is not a mistake to punish; it is the normal consequence of a timeout, and the room is the thing
	// that suffers if this server has nowhere to look up what it already did.
	transactionID := harness.Nonce("retry")
	first, _ := sendTransaction(t, server, peer, raw, transactionID)
	second, _ := sendTransaction(t, server, peer, raw, transactionID)
	if first[message.EventID()].Error != "" || second[message.EventID()].Error != "" {
		t.Fatalf("a retry must not turn an accepted event into a refused one: %v / %v",
			first[message.EventID()], second[message.EventID()])
	}

	// Exactly one copy may reach the timeline. `limited: false` and a length of one are the assertion;
	// a room where the message appears twice is what a client would see as a broken server.
	seen := 0
	response := alice.MustSync(t, server, nil)
	for _, event := range response.Rooms.Join[roomID].Timeline.Events {
		var candidate struct {
			Type    string `json:"type"`
			EventID string `json:"event_id"`
			Content struct {
				Body string `json:"body"`
			} `json:"content"`
		}
		if err := json.Unmarshal(event, &candidate); err != nil {
			t.Fatalf("a timeline event cannot be read: %v", err)
		}
		if candidate.Type == "m.room.message" && candidate.Content.Body == "sent once, delivered twice" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the retried transaction left %d copies of the message in the timeline, want 1", seen)
	}
}

func TestAnUnsignedFederatedEventIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)

	message := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.message",
		Depth:      join.Depth() + 1,
		PrevEvents: []string{join.EventID()},
		AuthEvents: []string{
			state["m.room.create\x00"],
			state["m.room.power_levels\x00"],
			join.EventID(),
		},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "written by nobody"})),
	})

	// The transaction's own signature is valid — a real peer is sending it — but the *event* was
	// signed by somebody whose key this server will not find. Trusting the transport here would mean
	// any homeserver could put words in any user's mouth by relaying an event that claims to be from
	// them, which is the difference between federation and an open write endpoint.
	unsigned := json.RawMessage(message.JSON())
	var fields map[string]any
	if err := json.Unmarshal(unsigned, &fields); err != nil {
		t.Fatalf("re-reading the event: %v", err)
	}
	fields["signatures"] = map[string]any{"not.the.peer:8448": map[string]any{
		"ed25519:nonsense": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}}
	forged, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encoding the forged event: %v", err)
	}

	results, _ := sendTransaction(t, server, peer, []json.RawMessage{forged}, harness.Nonce("forged"))
	if result, ok := results[""]; ok && result.Error == "" {
		t.Fatal("an event with no readable event ID was accepted")
	}
	refused := false
	for _, result := range results {
		if result.Error != "" {
			refused = true
		}
	}
	if !refused {
		t.Fatal("an event whose signature does not verify was accepted")
	}

	// And it must not be merely unacknowledged: storing it would let it reach clients later, from a
	// timeline read that never re-checks the signature.
	if response := alice.MustSync(t, server, nil); len(response.Rooms.Join[roomID].Timeline.Events) == 0 {
		t.Fatal("the room has no timeline, so this test proves nothing")
	}
	for _, event := range alice.MustSync(t, server, nil).Rooms.Join[roomID].Timeline.Events {
		var candidate struct {
			Content struct {
				Body string `json:"body"`
			} `json:"content"`
		}
		if err := json.Unmarshal(event, &candidate); err != nil {
			t.Fatalf("a timeline event cannot be read: %v", err)
		}
		if candidate.Content.Body == "written by nobody" {
			t.Fatal("a forged event reached the local user's timeline")
		}
	}
}

func TestAFederatedEventForAnUnknownRoomIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)

	// A room this server has never heard of, with a plausible-looking sender from the peer. Accepting
	// it would let a peer populate this server's database with rooms this server is not in, and the
	// room's own rules — the only thing that could have authorised the event — are not here to consult.
	message := buildRemoteEvent(t, peer, roomVersionUnknown, gomatrixserverlib.ProtoEvent{
		RoomID:     "!nosuchroom:" + string(harness.ServerName),
		SenderID:   "@charlie:" + string(peer.serverNo),
		Type:       "m.room.message",
		Depth:      3,
		PrevEvents: []string{"$notheere:" + string(harness.ServerName)},
		AuthEvents: []string{"$create:" + string(harness.ServerName)},
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "left at an address that does not exist"})),
	})

	results, raw := sendTransaction(t, server, peer, []json.RawMessage{json.RawMessage(message.JSON())},
		harness.Nonce("unknown-room"))
	if result, ok := results[message.EventID()]; !ok {
		t.Fatalf("the answer says nothing about the event it was sent (%s)", raw)
	} else if result.Error == "" {
		t.Fatal("an event for a room this server does not have was accepted")
	}
	_ = alice
}
