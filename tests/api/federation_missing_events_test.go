package api_test

// A peer asking this server for the history it is missing.
//
// This is the question a federating server asks constantly, and the one whose refusal is loudest later: a
// peer that cannot fill a gap refuses the event that pointed at it, and the two servers carry on with
// different histories of the same room. So the answers below are checked for the shape a peer has to be
// able to use — oldest first, neither named end included, and only events from the room it asked about —
// as well as for the two things this server is allowed to do about it: bound the answer by the asking
// server's own place in the room, and refuse outright when that place is nowhere.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/gjson"
)

// askForMissingEvents posts a get_missing_events request as the peer and returns the status and body.
func askForMissingEvents(t *testing.T, server *harness.Server, peer *fakePeer, roomID string,
	body map[string]any) (int, string) {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}
	path := "/_matrix/federation/v1/get_missing_events/" + harness.PathEscape(roomID)
	response := peerRequest(t, server, peer, http.MethodPost, path, json.RawMessage(raw))
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode, string(federationBody(t, response))
}

// unsignedFederationPost sends a gap request with no signature, which is how a request that did not come
// from the server it claims to come from looks.
func unsignedFederationPost(t *testing.T, server *harness.Server, path string, body map[string]any) int {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}
	response, err := server.FederationClient(t).Post(server.FederationServer(t).URL+path,
		"application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

// roomPeerJoinsAfter builds a room where alice says two things and only then lets the peer's user join,
// which is the situation a peer's gap request comes from: everything between the room's creation and that
// join is history the peer was never handed.
func roomPeerJoinsAfter(t *testing.T, server *harness.Server, alice *harness.Account, peer *fakePeer) (string,
	gomatrixserverlib.PDU, []string, string) {
	t.Helper()

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	first := alice.MustSendText(t, server, roomID, "before the peer arrived")
	second := alice.MustSendText(t, server, roomID, "and then this")

	remote := "@charlie:" + string(peer.serverNo)
	join, version := buildJoin(t, server, peer, roomID, remote, nil)
	response := signedFederationPut(t, server, peer,
		"/_matrix/federation/v2/send_join/"+harness.PathEscape(roomID)+"/"+harness.PathEscape(join.EventID()),
		json.RawMessage(join.JSON()))
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the peer's join was answered with %d, want 200: %s",
			response.StatusCode, federationBody(t, response))
	}
	return roomID, join, []string{first, second}, version
}

// eventIDOf computes the identifier a returned event carries, which from room version 2 onwards is not
// inside the event at all but a hash of it. A peer computes it the same way, which is why an answer that
// cannot be parsed as an event of this room version is an answer that names nothing.
func eventIDOf(t *testing.T, version string, raw json.RawMessage) string {
	t.Helper()

	impl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(version))
	if err != nil {
		t.Fatalf("room version %q: %v", version, err)
	}
	event, err := impl.NewEventFromUntrustedJSON(raw)
	if err != nil {
		t.Fatalf("a returned event cannot be parsed as a %s event: %v\n%s", version, err, raw)
	}
	return event.EventID()
}

func TestAPeerAsksForTheHistoryItNeverGot(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)

	// Everything from the room's creation up to the peer's own join is, from the peer's side, missing
	// history: it was told about the room at its join and handed none of what came before.
	roomID, join, sent, version := roomPeerJoinsAfter(t, server, alice, peer)
	create := remoteStateEventIDs(t, server, alice, roomID)["m.room.create\x00"]
	first, second := sent[0], sent[1]

	status, body := askForMissingEvents(t, server, peer, roomID, map[string]any{
		"earliest_events": []string{create},
		"latest_events":   []string{join.EventID()},
		"limit":           10,
		"min_depth":       1,
	})
	if status != http.StatusOK {
		t.Fatalf("a signed gap request got %d, want 200: %s", status, body)
	}
	events := gjson.Get(body, "events")
	if !events.IsArray() || len(events.Array()) == 0 {
		t.Fatalf("the answer carries no events, so the peer's gap stays open:\n%s", body)
	}

	got := map[string]int{}
	order := []string{}
	var lastDepth int64 = -1
	for i, raw := range events.Array() {
		var event struct {
			Type   string `json:"type"`
			RoomID string `json:"room_id"`
			Depth  int64  `json:"depth"`
		}
		if err := json.Unmarshal([]byte(raw.Raw), &event); err != nil {
			t.Fatalf("event %d cannot be read: %v\n%s", i, err, raw.Raw)
		}
		if event.RoomID != roomID {
			t.Fatalf("event %d belongs to %q, not %s", i, event.RoomID, roomID)
		}
		if event.Depth < lastDepth {
			t.Fatalf("event %d (%s) is shallower than the one before it: the peer would attach a room's "+
				"history backwards", i, event.Type)
		}
		lastDepth = event.Depth
		got[eventIDOf(t, version, json.RawMessage(raw.Raw))] = i
		order = append(order, event.Type)
	}

	// The two ends the peer named are its own, and sending them back would be resend traffic it would
	// have to discard.
	if _, included := got[create]; included {
		t.Errorf("the answer includes the earliest event the peer already named")
	}
	if _, included := got[join.EventID()]; included {
		t.Errorf("the answer includes the latest event the peer already named")
	}
	// The messages it was never told about are the point of the request.
	for _, eventID := range []string{first, second} {
		if _, found := got[eventID]; !found {
			t.Errorf("the answer skipped %s, which happened between the two ends:\n%s", eventID, body)
		}
	}
	if got[first] > got[second] {
		t.Errorf("the two messages came back the wrong way round: %v", order)
	}

	// A limit is a limit. A peer that asks for one event is paging, and a server that answers with
	// everything turns its loop into a download.
	status, body = askForMissingEvents(t, server, peer, roomID, map[string]any{
		"earliest_events": []string{create},
		"latest_events":   []string{join.EventID()},
		"limit":           1,
	})
	if status != http.StatusOK {
		t.Fatalf("a limited gap request got %d, want 200: %s", status, body)
	}
	if n := len(gjson.Get(body, "events").Array()); n != 1 {
		t.Fatalf("a request with limit 1 was answered with %d events:\n%s", n, body)
	}
}

func TestAPeerCannotAskForTheHistoryOfARoomItsUsersAreNotIn(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)

	// A room the peer's user never joined, and a room they did: the same peer asking about the first is
	// asking for a conversation it is not in.
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	alice.MustSendText(t, server, roomID, "not for the peer's eyes")
	joined, join, _ := joinedRoom(t, server, alice, peer, "public_chat")
	if joined == roomID {
		t.Fatalf("the two rooms are the same (%s)", roomID)
	}
	create := remoteStateEventIDs(t, server, alice, roomID)["m.room.create\x00"]

	status, body := askForMissingEvents(t, server, peer, roomID, map[string]any{
		"earliest_events": []string{create},
		"latest_events":   []string{join.EventID()},
		"limit":           10,
	})
	if status == http.StatusOK {
		t.Fatalf("a peer was handed the history of a room it is not in:\n%s", body)
	}

	// An unknown room is answered as unknown rather than as an empty gap, which would tell the peer the
	// history it is looking for existed and has since been deleted.
	status, body = askForMissingEvents(t, server, peer, "!nosuchroom:example.invalid", map[string]any{
		"earliest_events": []string{"$a"},
		"latest_events":   []string{join.EventID()},
	})
	if status == http.StatusOK {
		t.Fatalf("an unknown room answered as a closed gap: %s", body)
	}

	// Unsigned, the request is not from a server at all.
	unsigned := unsignedFederationPost(t, server, "/_matrix/federation/v1/get_missing_events/"+
		harness.PathEscape(joined), map[string]any{
		"earliest_events": []string{"$a"},
		"latest_events":   []string{join.EventID()},
	})
	if unsigned == http.StatusOK {
		t.Fatalf("an unsigned gap request was answered; any client that can reach this port could then " +
			"read a private room's history")
	}
}
