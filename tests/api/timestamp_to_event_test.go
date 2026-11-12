package api_test

// `GET /rooms/{roomID}/timestamp_to_event` — the endpoint that answers "what was happening at
// <timestamp>?", which is how a client jumps into the middle of a room it has never opened.
//
// The rules are the interesting part, and they are the ones a naive implementation gets wrong:
//
//   - The direction is not a hint, it is the answer's definition. Forward means "the earliest event at or
//     after this moment"; backward means "the latest event at or before it". A room whose events all share
//     one millisecond therefore has *two* right answers, and returning whichever the store happened to
//     yield first is wrong half the time — complement tests exactly that with an application service
//     placing events at identical timestamps.
//   - Running past the end of a room finds nothing. An implementation that clamps to the oldest event
//     would answer a question about the year 1970 with the room's first message, which is a confident
//     statement about a moment the room knows nothing about.
//   - A stranger may not ask at all, even about a public room. The endpoint is a read of history, and the
//     room's history rule is what governs it — the same rule `/messages` applies, so the two cannot
//     disagree about who may see the room's past.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

type timestampAnswer struct {
	OriginServerTS int64  `json:"origin_server_ts"`
	EventID        string `json:"event_id"`
}

// askTimestampToEvent asks the question and returns the raw response, leaving every verdict to the
// caller: a status code is part of what these tests pin, so decoding cannot quietly absorb it.
func askTimestampToEvent(t *testing.T, server *harness.Server, account *harness.Account,
	roomID, dir string, ts int64) *harness.Response {
	t.Helper()
	path := "/_matrix/client/v1/rooms/" + harness.PathEscape(roomID) + "/timestamp_to_event?dir=" +
		url.QueryEscape(dir) + "&ts=" + url.QueryEscape(strconv.FormatInt(ts, 10))
	return account.Do(t, server, harness.Request{Method: http.MethodGet, Path: path})
}

func TestATimestampFindsTheEventOnEitherSideOfIt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "jumper", "pw-jumper", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	first := alice.MustSendText(t, server, roomID, "the first message")
	// A moment strictly between the two messages: the pause has to outlast the millisecond the events
	// are stamped with, or "before the second message" and "the first message" stop being different
	// questions.
	time.Sleep(5 * time.Millisecond)
	beforeSecond := time.Now().UnixMilli()
	time.Sleep(5 * time.Millisecond)
	second := alice.MustSendText(t, server, roomID, "the second message")

	cases := []struct {
		name string
		dir  string
		ts   int64
		want string
	}{
		{"forward from just before the second message finds it", "f", beforeSecond, second},
		{"backward from just before the second message finds the first", "b", beforeSecond, first},
		// The other end of a room is not a tie, so it can be asserted by name: one message is the
		// newest thing in here, and looking back from the far future can only find it.
		{"backward from the end of time finds the last message", "b", 4102444800000, second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := askTimestampToEvent(t, server, alice, roomID, tc.dir, tc.ts)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("looking for %q: got %d, want 200\nbody: %s", tc.name, response.StatusCode, response.Body)
			}
			var answer timestampAnswer
			if err := json.Unmarshal([]byte(response.Body), &answer); err != nil {
				t.Fatalf("the answer cannot be read: %v (%s)", err, response.Body)
			}
			if answer.EventID != tc.want {
				t.Errorf("looking for %q found %s, want %s", tc.name, answer.EventID, tc.want)
			}
			// The timestamp is not decoration: a client paginating from here needs the position it was
			// actually given, and an answer that disagrees with the event it names is worse than silent.
			if answer.OriginServerTS <= 0 {
				t.Errorf("the answer names %s but carries origin_server_ts %d", answer.EventID, answer.OriginServerTS)
			}
		})
	}
}

// eventTimestamp reads the stamp an event actually carries, straight from the room's history.
func eventTimestamp(t *testing.T, server *harness.Server, account *harness.Account,
	roomID, eventID string) int64 {
	t.Helper()
	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/event/" +
		harness.PathEscape(eventID)
	response := account.Do(t, server, harness.Request{Method: http.MethodGet, Path: path})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reading %s: got %d, want 200\nbody: %s", eventID, response.StatusCode, response.Body)
	}
	var stamped struct {
		OriginServerTS int64 `json:"origin_server_ts"`
	}
	if err := json.Unmarshal([]byte(response.Body), &stamped); err != nil {
		t.Fatalf("reading %s: %v (%s)", eventID, err, response.Body)
	}
	return stamped.OriginServerTS
}

// TestATimestampAtTheBeginningOfTimeFindsTheOldestEvent is about the beginning of a room, which is not
// one event but a handful written in the same millisecond — the create event, the creator's membership,
// the power levels, the join rule. Any of them is a correct answer to "what was happening at the epoch",
// and which one it is depends on the order this server happened to write them in. What must hold, and
// what a clamping or an ordering bug would break, is that the answer carries the *smallest* timestamp in
// the room: it must never reach past the room's own beginning.
func TestATimestampAtTheBeginningOfTimeFindsTheOldestEvent(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "jumper", "pw-jumper", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	first := alice.MustSendText(t, server, roomID, "the first message")

	answer := askTimestampToEvent(t, server, alice, roomID, "f", 1)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("looking for the room's beginning: got %d, want 200\nbody: %s",
			answer.StatusCode, answer.Body)
	}
	var found timestampAnswer
	if err := json.Unmarshal([]byte(answer.Body), &found); err != nil {
		t.Fatalf("the answer cannot be read: %v (%s)", err, answer.Body)
	}

	// The room's own beginning has to be reachable from the epoch, and the first message is the oldest
	// event a client could ask for by name — so an answer stamped later than it has walked too far.
	if found.OriginServerTS > eventTimestamp(t, server, alice, roomID, first) {
		t.Errorf("looking forward from the epoch found an event stamped %d, after the room's first message at %d",
			found.OriginServerTS, eventTimestamp(t, server, alice, roomID, first))
	}
	if found.OriginServerTS <= 0 {
		t.Errorf("the answer names %s with no usable timestamp (%d)", found.EventID, found.OriginServerTS)
	}
}

func TestATimestampBeyondTheEndsOfARoomFindsNothing(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "jumper", "pw-jumper", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSendText(t, server, roomID, "a message in the present")
	time.Sleep(5 * time.Millisecond)
	afterEverything := time.Now().UnixMilli()

	// Past the last event, forward finds nothing; before the first, backward finds nothing. Both are
	// "the room has no answer", and an implementation that clamps would rather invent one.
	for _, tc := range []struct {
		name string
		dir  string
		ts   int64
	}{
		{"forward from after the last event", "f", afterEverything + 60_000},
		{"backward from before the room existed", "b", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := askTimestampToEvent(t, server, alice, roomID, tc.dir, tc.ts)
			// 404 M_NOT_FOUND is what the specification gives, and what a client distinguishes from
			// "you may not ask" — the first is a fact about the room, the second about the reader.
			response.MustError(t, http.StatusNotFound, "M_NOT_FOUND")
		})
	}
}

func TestATimestampOfARoomTheReaderIsNotInIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "owner", "pw-owner", false)
	outsider := server.Register(t, "outsider", "pw-outsider", false)
	// Public, and readable by its members: the room is discoverable, which is a different thing from
	// being allowed to ask what was said in it at a given moment.
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSendText(t, server, roomID, "a message only members may locate")

	response := askTimestampToEvent(t, server, outsider, roomID, "f", time.Now().UnixMilli())
	response.MustError(t, http.StatusForbidden, "M_FORBIDDEN")
}

func TestATimestampQuestionThatCannotBeAnsweredIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "jumper", "pw-jumper", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSendText(t, server, roomID, "a message to point at")
	escaped := harness.PathEscape(roomID)

	// The direction is required: "f" and "b" are opposite questions, and guessing one would answer a
	// question the client did not ask.
	for _, tc := range []struct {
		name string
		path string
	}{
		{"no direction at all", "/_matrix/client/v1/rooms/" + escaped + "/timestamp_to_event?ts=1000"},
		{"a direction that is not one", "/_matrix/client/v1/rooms/" + escaped + "/timestamp_to_event?ts=1000&dir=sideways"},
		{"a timestamp that is not a number", "/_matrix/client/v1/rooms/" + escaped + "/timestamp_to_event?ts=soon&dir=f"},
		{"no timestamp at all", "/_matrix/client/v1/rooms/" + escaped + "/timestamp_to_event?dir=f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alice.Do(t, server, harness.Request{Method: http.MethodGet, Path: tc.path}).
				MustError(t, http.StatusBadRequest, "M_INVALID_PARAM")
		})
	}
}
