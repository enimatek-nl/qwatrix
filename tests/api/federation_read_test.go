package api_test

// The read side of federation: a peer asking this server for something it does not have.
//
// These are the endpoints that make a homeserver useful to another homeserver rather than merely
// talkative. Sending is the easy half — an event pushed at a peer can be answered on its own terms.
// Fetching is the half a room's history depends on: a server that joined a room yesterday has today's
// state and none of yesterday's messages, and everything it can show a user is a question it asks a
// resident server about.
//
// The permission rule is the same everywhere and is worth stating once: **a server may read what its
// users are in.** A peer with no user in the room gets a refusal even for an event whose ID it happens
// to know, because event IDs are not secrets and "may I have that event?" is a way to read rooms the
// answer would not otherwise admit exist.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// bodyOf reads the "body" string out of a raw event, which is how these tests recognise an event: a
// federated PDU carries no event ID of its own.
func bodyOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var event struct {
		Content struct {
			Body string `json:"body"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("the answer carried something that is not an event: %v (%s)", err, raw)
	}
	return event.Content.Body
}

// TestAPeerFetchesAnEventItDoesNotHold is the smallest of these reads: one event, by ID.
func TestAPeerFetchesAnEventItDoesNotHold(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, _, _ := joinedRoom(t, server, alice, peer, "public_chat")
	eventID := alice.MustSendText(t, server, roomID, "a message a peer has not seen")

	status, body := federationGetSigned(t, server, peer, "/_matrix/federation/v1/event/"+harness.PathEscape(eventID))
	if status != http.StatusOK {
		t.Fatalf("a resident peer asked for an event in its room: got %d, want 200\nbody: %s", status, body)
	}
	var answer struct {
		Origin         string            `json:"origin"`
		OriginServerTS int64             `json:"origin_server_ts"`
		PDUs           []json.RawMessage `json:"pdus"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer cannot be read: %v (%s)", err, body)
	}
	if len(answer.PDUs) != 1 {
		t.Fatalf("the answer carries %d events, want exactly the one asked for (%s)", len(answer.PDUs), body)
	}
	if answer.Origin != server.ServerName {
		t.Errorf("origin = %q, want this server %q", answer.Origin, server.ServerName)
	}
	if !json.Valid(answer.PDUs[0]) || len(answer.PDUs[0]) == 0 {
		t.Fatalf("the event was not carried: %s", body)
	}
	// The PDU is asserted on its content rather than its ID: from room version 2 onwards an event's ID
	// is a hash *of* the event, so it is not carried inside it, and a peer that wanted to check the ID
	// would recompute it.
	if body := bodyOf(t, answer.PDUs[0]); body != "a message a peer has not seen" {
		t.Errorf("the event fetched carries body %q, want the message asked for\n%s", body, answer.PDUs[0])
	}

	// A server with nobody in the room may not fetch it, and an event that does not exist is said so.
	outsider := newFakePeer(t)
	outsiderStatus, outsiderBody := federationGetSigned(t, server, outsider, "/_matrix/federation/v1/event/"+harness.PathEscape(eventID))
	if outsiderStatus == http.StatusOK {
		t.Fatalf("a server with no user in the room was handed the event: %s", outsiderBody)
	}
	if status, _ := federationGetSigned(t, server, peer, "/_matrix/federation/v1/event/"+harness.PathEscape("$nonsuch")); status != http.StatusNotFound {
		t.Errorf("fetching an event that does not exist: got %d, want 404", status)
	}
}

// TestAPeerAsksWhereTheRoomWasAtAMoment is the federation half of MSC3059: the question a server asks
// another server when its user jumps to a date in a room it does not hold the history of.
func TestAPeerAsksWhereTheRoomWasAtAMoment(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, _, _ := joinedRoom(t, server, alice, peer, "public_chat")

	// The moment the peer will ask about, one millisecond after everything that has happened so far —
	// which includes the peer's own join, and a search forward from a boundary that touches an event
	// includes it. Real tests of this endpoint sleep for the same reason.
	before := time.Now().UnixMilli() + 1
	time.Sleep(10 * time.Millisecond)
	alice.MustSendText(t, server, roomID, "the first thing a peer looks for")
	const lastBody = "the last thing in the room"
	alice.MustSendText(t, server, roomID, lastBody)

	// The event is identified by its body: a federated PDU carries no event ID, because from room
	// version 2 onwards the ID is a hash of the event rather than a field inside it.
	ask := func(t *testing.T, ts int64, direction string) (int, []byte, string) {
		t.Helper()
		path := "/_matrix/federation/v1/timestamp_to_event/" + harness.PathEscape(roomID) +
			"?ts=" + strconv.FormatInt(ts, 10) + "&dir=" + direction
		status, body := federationGetSigned(t, server, peer, path)
		var answer struct {
			Origin         string          `json:"origin"`
			OriginServerTS int64           `json:"origin_server_ts"`
			PDU            json.RawMessage `json:"pdu"`
		}
		_ = json.Unmarshal(body, &answer)
		return status, body, bodyOf(t, answer.PDU)
	}

	// Forward from before the message finds the message; back from after the room finds its last
	// event. The directions are the whole point of the endpoint, so both are asked.
	status, body, found := ask(t, before, "f")
	if status != http.StatusOK {
		t.Fatalf("asking where the room was: got %d, want 200\nbody: %s", status, body)
	}
	if found != "the first thing a peer looks for" {
		t.Errorf("looking forward found %q, want the message sent first", found)
	}
	if _, _, found := ask(t, time.Now().UnixMilli(), "b"); found != lastBody {
		t.Errorf("looking backward found %q, want the newest message", found)
	}

	// Asking before the room began is not answered by clamping to the oldest event: the peer is about
	// to be told there is nothing there, which is a fact it can act on, rather than a lie about 1970.
	if status, body, _ := ask(t, 1, "b"); status != http.StatusNotFound {
		t.Errorf("looking backward before the room began: got %d, want 404\nbody: %s", status, body)
	}
	// A direction that is not one of the two is a client error, not a guess.
	if status, _ := federationGetSigned(t, server, peer, "/_matrix/federation/v1/timestamp_to_event/"+
		harness.PathEscape(roomID)+"?ts=1&dir=sideways"); status != http.StatusBadRequest {
		t.Errorf("an unknown direction: got %d, want 400", status)
	}
	// And a server with no user in the room is refused, as with every other read here.
	outsider := newFakePeer(t)
	if status, _ := federationGetSigned(t, server, outsider, "/_matrix/federation/v1/timestamp_to_event/"+
		harness.PathEscape(roomID)+"?ts=1&dir=f"); status == http.StatusOK {
		t.Fatal("a server with no user in the room was told where the room was")
	}
}
