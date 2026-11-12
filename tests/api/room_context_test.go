package api_test

// `GET /rooms/{roomID}/context/{eventID}` — the neighbourhood of one event.
//
// This is what a client calls after it has landed on an event from anywhere: a search result, a push
// notification, the reply chain of a message, or a timestamp lookup that pointed at a moment rather than a
// message. It asks for the event, some events on each side of it, and tokens to keep walking in either
// direction.
//
// What these tests pin is the part a shortcut gets wrong: the two arrays are ordered *away* from the
// centre, because the client renders them outward — `events_before` newest-first, `events_after`
// oldest-first — and the tokens pick up exactly where the response stopped, so an event is neither served
// twice nor skipped between one page and the next. The tokens are also the whole value of the endpoint:
// an event with no way to walk on from it is a dead end, which is what a client already had before it
// asked.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

type contextAnswer struct {
	Event        json.RawMessage   `json:"event"`
	EventsBefore []json.RawMessage `json:"events_before"`
	EventsAfter  []json.RawMessage `json:"events_after"`
	Start        string            `json:"start"`
	End          string            `json:"end"`
	State        []json.RawMessage `json:"state"`
}

func askContext(t *testing.T, server *harness.Server, account *harness.Account,
	roomID, eventID string, limit int) *harness.Response {
	t.Helper()
	path := "/_matrix/client/v1/rooms/" + harness.PathEscape(roomID) + "/context/" +
		harness.PathEscape(eventID)
	if limit >= 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	return account.Do(t, server, harness.Request{Method: http.MethodGet, Path: path})
}

// askMessages pages this room with the given token, and returns the event IDs the chunk carried.
func askMessages(t *testing.T, server *harness.Server, account *harness.Account,
	roomID, dir, from string) []string {
	t.Helper()
	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/messages?dir=" + dir +
		"&limit=50&from=" + harness.PathEscape(from)
	response := account.Do(t, server, harness.Request{Method: http.MethodGet, Path: path})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("paging %s from %s: got %d, want 200\nbody: %s", dir, from, response.StatusCode, response.Body)
	}
	var page struct {
		Chunk []struct {
			EventID string `json:"event_id"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal([]byte(response.Body), &page); err != nil {
		t.Fatalf("paging %s from %s: %v (%s)", dir, from, err, response.Body)
	}
	ids := make([]string, 0, len(page.Chunk))
	for _, event := range page.Chunk {
		ids = append(ids, event.EventID)
	}
	return ids
}

// fiveMessages is a room whose history has an obvious middle, sent with pauses wide enough that the
// events cannot share a timestamp.
func fiveMessages(t *testing.T, server *harness.Server, alice *harness.Account) (string, []string) {
	t.Helper()
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	var ids []string
	for _, body := range []string{"one", "two", "three", "four", "five"} {
		ids = append(ids, alice.MustSendText(t, server, roomID, body))
	}
	return roomID, ids
}

func TestTheContextOfAnEventReachesEitherSideOfIt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "centre", "pw-centre", false)
	roomID, ids := fiveMessages(t, server, alice)
	middle := ids[2]

	response := askContext(t, server, alice, roomID, middle, 4)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reading context: got %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	var answer contextAnswer
	if err := json.Unmarshal([]byte(response.Body), &answer); err != nil {
		t.Fatalf("the context cannot be read: %v (%s)", err, response.Body)
	}

	var event struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(answer.Event, &event); err != nil || event.EventID != middle {
		t.Fatalf("the response is not about the event it was asked for: %s", response.Body)
	}

	// Each array holds the two events nearest the centre — the limit covers the two arrays together —
	// and each is ordered away from the centre, because that is the direction the client is rendering.
	before := idsOf(answer.EventsBefore)
	if len(before) != 2 || before[0] != ids[1] || before[1] != ids[0] {
		t.Errorf("events_before = %v, want [%s %s] (nearest first, walking back)", before, ids[1], ids[0])
	}
	after := idsOf(answer.EventsAfter)
	if len(after) != 2 || after[0] != ids[3] || after[1] != ids[4] {
		t.Errorf("events_after = %v, want [%s %s] (nearest first, walking on)", after, ids[3], ids[4])
	}

	if answer.Start == "" || answer.End == "" {
		t.Errorf("the context carries no tokens to walk on from (start %q, end %q)", answer.Start, answer.End)
	}
	// The state travels with the neighbourhood: a client that has jumped into a room it has never
	// rendered needs the room's membership to draw anything at all.
	if len(answer.State) == 0 {
		t.Error("the context carries no state, so a client cannot render the events it was given")
	}
}

func TestTheContextTokensPickUpWhereTheResponseStopped(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "centre", "pw-centre", false)
	roomID, ids := fiveMessages(t, server, alice)
	second := ids[1]

	response := askContext(t, server, alice, roomID, second, 2)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reading context: got %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	var answer contextAnswer
	if err := json.Unmarshal([]byte(response.Body), &answer); err != nil {
		t.Fatalf("the context cannot be read: %v (%s)", err, response.Body)
	}
	before, after := idsOf(answer.EventsBefore), idsOf(answer.EventsAfter)
	if len(before) != 1 || before[0] != ids[0] {
		t.Fatalf("events_before = %v, want just %s", before, ids[0])
	}
	if len(after) != 1 || after[0] != ids[2] {
		t.Fatalf("events_after = %v, want just %s", after, ids[2])
	}

	// Forward from `end` continues the history: the events after the ones the context already carried,
	// and not those events again. A client that got the same page twice has no way to tell that the
	// history is not repeating itself.
	forward := askMessages(t, server, alice, roomID, "f", answer.End)
	if !hasID(forward, ids[3]) || !hasID(forward, ids[4]) {
		t.Errorf("forward from the context's end token gave %v, want %s and %s", forward, ids[3], ids[4])
	}
	for _, id := range append(before, after...) {
		if hasID(forward, id) {
			t.Errorf("forward from the context's end token served %s again", id)
		}
	}

	// Backward from `start` reaches the events older than everything the context carried. Here that is
	// the room's own creation state: the context already gave the one older message there was, so a page
	// that repeats it has skipped nothing but taught the client the history is repeating itself, and a
	// page that jumped forward has ignored the direction.
	backward := askMessages(t, server, alice, roomID, "b", answer.Start)
	for _, id := range append(before, after...) {
		if hasID(backward, id) {
			t.Errorf("backward from the context's start token served %s again", id)
		}
	}
	// `ids[0]` is legitimately absent — the context carried it — so what must not appear is anything
	// *newer* than the centre, which would mean the token pointed the wrong way.
	for _, id := range append([]string{second}, ids[2:]...) {
		if hasID(backward, id) {
			t.Errorf("backward from the context's start token reached forward to %s", id)
		}
	}
	if len(backward) == 0 {
		t.Error("backward from the context's start token found nothing, though the room has a beginning to reach")
	}
}

func TestTheContextOfAnEventThatIsNotHereIsNotFound(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "centre", "pw-centre", false)
	roomID, ids := fiveMessages(t, server, alice)
	otherRoomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	// An event that does not exist, and an event that exists in a room the reader is not asking about,
	// get the same answer. Confirming which room an event lives in would be a way to walk a list of event
	// IDs and find the private rooms among them.
	askContext(t, server, alice, roomID, "$notanevent:test.local", -1).
		MustError(t, http.StatusNotFound, "M_NOT_FOUND")
	askContext(t, server, alice, otherRoomID, ids[0], -1).
		MustError(t, http.StatusNotFound, "M_NOT_FOUND")
}

func TestTheContextOfARoomTheReaderIsNotInIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "owner", "pw-owner", false)
	outsider := server.Register(t, "outsider", "pw-outsider", false)
	roomID, ids := fiveMessages(t, server, alice)

	askContext(t, server, outsider, roomID, ids[2], -1).MustError(t, http.StatusForbidden, "M_FORBIDDEN")
}

// idsOf reads the event IDs out of a list of raw events, in the order they were given — the order is
// what these tests assert, so it must survive the decoding.
func idsOf(events []json.RawMessage) []string {
	ids := make([]string, 0, len(events))
	for _, raw := range events {
		var event struct {
			EventID string `json:"event_id"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			panic("a context array carried something that is not an event: " + err.Error())
		}
		ids = append(ids, event.EventID)
	}
	return ids
}

func hasID(haystack []string, needle string) bool {
	for _, value := range haystack {
		if value == needle {
			return true
		}
	}
	return false
}
