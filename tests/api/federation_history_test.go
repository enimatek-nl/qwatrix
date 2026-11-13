package api_test

// Fetching a room's past from a server that was there for it.
//
// Joining a room buys its current state and none of its history: that is what a join is. Everything a
// user of the joining server then learns about the past — jumping to a date, paging up out of the top of
// the screen — is a question this server asks a resident peer, and the answer has to be kept somewhere
// that is honest about its status. These tests are the whole of that round trip between two real
// homeservers, because the interesting failure is not "the request failed" but "the server remembered the
// wrong thing".
//
// Two properties are worth this much scaffolding. The first is that the fetched history is reachable: a
// server that fetched an event and then says it has never heard of it is worse than one that never asked.
// The second is that it is *not* delivered as news: a client that opens a room must not have last year's
// messages pushed at it as though they had just happened, and a device that was asleep for a year must not
// wake to a thousand notifications for events it has already seen.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

func TestAServerFetchesHistoryThatHappenedBeforeItJoined(t *testing.T) {
	// The server that holds the room, and its history, from the beginning.
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, hs1, map[string]any{"preset": "public_chat", "name": "the past"})

	// A boundary after the room's own creation, so that a search forward from it cannot be answered by
	// one of the creation events.
	before := time.Now().UnixMilli()
	time.Sleep(10 * time.Millisecond)
	older := alice.MustSendText(t, hs1, roomID, "written before anyone else joined")
	newer := alice.MustSendText(t, hs1, roomID, "the newest thing in the room")
	after := time.Now().UnixMilli()

	// The server that joins afterwards. It is told the room's state, and nothing else.
	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	charlie := hs2.Register(t, "charlie", "pw-charlie", false)
	charlie.MustPostJSON(t, hs2, "/_matrix/client/v3/join/"+url.PathEscape(roomID)+
		"?server_name="+url.QueryEscape(hs1.ServerName), map[string]any{}).MustStatus(t, http.StatusOK)
	hs1.DrainFederation(t)
	hs2.DrainFederation(t)

	// The premise of everything below: this server really does not have that history. If delivery ever
	// starts sending it, the assertions that follow would pass for the wrong reason, so the premise is
	// checked rather than assumed.
	for _, body := range messageBodies(messagesOf(t, charlie, hs2, roomID)) {
		if body == "written before anyone else joined" {
			t.Fatalf("the joining server was handed the room's history by the join, so this test would prove nothing")
		}
	}

	// The question a user jumping to a date asks, on the server that cannot answer it locally.
	asked := charlie.MustGet(t, hs2, "/_matrix/client/v1/rooms/"+harness.PathEscape(roomID)+
		"/timestamp_to_event?ts="+strconv.FormatInt(before, 10)+"&dir=f")
	if asked.StatusCode != http.StatusOK {
		t.Fatalf("asking the joining server where the room was: got %d, want 200\nbody: %s",
			asked.StatusCode, asked.Body)
	}
	var found struct {
		EventID string `json:"event_id"`
	}
	asked.JSON(t, &found)
	if found.EventID != older {
		t.Fatalf("the joining server answered %s, want the event that only the other server holds (%s)\nHS1 LOG:\n%s\nHS2 LOG:\n%s",
			found.EventID, older, hs1.Logs(), hs2.Logs())
	}

	// Asked twice, the same answer: the question must not be settled by which peer answered first, and a
	// server that fetched an event has to keep meaning the same event when it asks again.
	secondResponse := charlie.Do(t, hs2, harness.Request{Method: http.MethodGet,
		Path: "/_matrix/client/v1/rooms/" + harness.PathEscape(roomID) +
			"/timestamp_to_event?ts=" + strconv.FormatInt(before, 10) + "&dir=f"})
	if secondResponse.StatusCode != http.StatusOK {
		t.Fatalf("the second asking: got %d\nbody: %s\nHS1 LOG:\n%s\nHS2 LOG:\n%s",
			secondResponse.StatusCode, secondResponse.Body, hs1.Logs(), hs2.Logs())
	}
	second := secondResponse
	var repeated struct {
		EventID string `json:"event_id"`
	}
	second.JSON(t, &repeated)
	if repeated.EventID != older {
		t.Errorf("asking twice gave %s the second time, want %s both times", repeated.EventID, older)
	}

	// The event the user landed on is the newest one, and what they do next is page up. A server that
	// fetched one event and nothing around it would answer the question and leave the client with nowhere
	// to go, which is why the specification tells the server to backfill around what it fetched.
	landed := charlie.Do(t, hs2, harness.Request{Method: http.MethodGet,
		Path: "/_matrix/client/v1/rooms/" + harness.PathEscape(roomID) +
			"/timestamp_to_event?ts=" + strconv.FormatInt(after, 10) + "&dir=b"})
	if landed.StatusCode != http.StatusOK {
		t.Fatalf("the dir=b asking: %d\n%s", landed.StatusCode, landed.Body)
	}
	var landedAnswer struct {
		EventID string `json:"event_id"`
	}
	landed.JSON(t, &landedAnswer)
	if landedAnswer.EventID != newer {
		t.Fatalf("looking backward from after the room landed on %s, want %s", landedAnswer.EventID, newer)
	}
	context := charlie.Do(t, hs2, harness.Request{Method: http.MethodGet,
		Path: "/_matrix/client/v1/rooms/" + harness.PathEscape(roomID) + "/context/" +
			harness.PathEscape(newer) + "?limit=0"})
	if context.StatusCode != http.StatusOK {
		t.Fatalf("asking for the context of a fetched event: got %d, want 200\nbody: %s",
			context.StatusCode, context.Body)
	}
	var neighbours struct {
		Start string `json:"start"`
	}
	context.JSON(t, &neighbours)
	if neighbours.Start == "" {
		t.Fatalf("the context of a fetched event carries no token to page from: %s", context.Body)
	}
	paged := charlie.MustGet(t, hs2, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/messages?dir=b&limit=100&from="+url.QueryEscape(neighbours.Start))
	if paged.StatusCode != http.StatusOK {
		t.Fatalf("paging back from a fetched event: got %d, want 200\nbody: %s", paged.StatusCode, paged.Body)
	}
	bodies := messageBodies(messagesOf(t, charlie, hs2, roomID))
	foundOlder := false
	for _, body := range bodies {
		if body == "written before anyone else joined" {
			foundOlder = true
		}
	}
	if !foundOlder {
		t.Errorf("the room's history never became reachable on the joining server, which holds %v\n"+
			"HS1 LOG:\n%s\nHS2 LOG:\n%s", bodies, hs1.Logs(), hs2.Logs())
	}

	// And the part that must not happen: history is not news. A client opening this room must not be told
	// that last year's messages have just arrived, because they have not, and a user who reads to the top
	// of a room should not accumulate unread counts for the past they just looked at.
	// A sync from the beginning is where fetched history would surface if it were going to: it delivers
	// whatever this server has for the room. Then an incremental one, which must be empty of it too.
	full := charlie.Do(t, hs2, harness.Request{Method: http.MethodGet, Path: "/_matrix/client/v3/sync?timeout=0"})
	if bodies := syncBodies(t, full.Body, roomID); containsBody(bodies, "written before anyone else joined") {
		t.Errorf("a full sync delivered the fetched history as news: %v", bodies)
	}
	var token struct {
		NextBatch string `json:"next_batch"`
	}
	if err := json.Unmarshal(full.Body, &token); err != nil {
		t.Fatalf("the sync token cannot be read: %v", err)
	}
	incremental := charlie.Do(t, hs2, harness.Request{Method: http.MethodGet,
		Path: "/_matrix/client/v3/sync?since=" + url.QueryEscape(token.NextBatch) + "&timeout=0"})
	if bodies := syncBodies(t, incremental.Body, roomID); containsBody(bodies, "written before anyone else joined") {
		t.Errorf("an incremental sync delivered the fetched history as news: %v", bodies)
	}
}

// syncBodies lists the message bodies one /sync response delivered for a room.
func syncBodies(t *testing.T, body []byte, roomID string) []string {
	t.Helper()
	var document struct {
		Rooms struct {
			Join map[string]struct {
				Timeline struct {
					Events []json.RawMessage `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("the sync response cannot be read: %v", err)
	}
	bodies := []string{}
	for id, joined := range document.Rooms.Join {
		if id != roomID {
			continue
		}
		for _, raw := range joined.Timeline.Events {
			var event struct {
				Type    string `json:"type"`
				Content struct {
					Body string `json:"body"`
				} `json:"content"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				continue
			}
			if event.Type == "m.room.message" {
				bodies = append(bodies, event.Content.Body)
			}
		}
	}
	return bodies
}

func containsBody(bodies []string, want string) bool {
	for _, body := range bodies {
		if body == want {
			return true
		}
	}
	return false
}
