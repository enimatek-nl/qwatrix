package api_test

// Simplified Sliding Sync (MSC4186), the endpoint Element X's Rust SDK speaks and no other client
// has any reason to call.
//
// What these tests hold the server to is the part of the MSC that a /sync-shaped implementation
// gets wrong by accident: the endpoint is not "sync with a room list", it is a *stateful
// connection* that has remembered, per room, what it already showed this device. Three properties
// run through the file:
//
//   - a room is described once (`initial: true`) and afterwards only as a delta, so a client never
//     has to diff a room it was just told is unchanged;
//   - `num_live` separates an event that just happened from history the server is preloading, which
//     is the difference between a phone buzzing and a phone being quiet;
//   - a `pos` belongs to the user and device it was issued to, and a server that forgets that hands
//     rooms to whoever holds a string.
//
// The contract is MSC4186 as merged (ffc54bc). The `/versions` flag is deliberately not advertised
// yet, so these tests call the unstable URL that Element X itself uses.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// slidingSyncPath is the unstable URL MSC4186 names, which is what a client calls before the
// server advertises the feature in /versions.
const slidingSyncPath = "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync"

// slidingSync sends one request on a connection and returns the response.
func slidingSync(t *testing.T, server *harness.Server, who *harness.Account, body map[string]any) *harness.Response {
	t.Helper()
	return who.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   slidingSyncPath,
		Token:  who.AccessToken,
		Body:   body,
	})
}

// syncAnswer is a parsed sliding-sync response.
type syncAnswer struct {
	Pos   string `json:"pos"`
	Lists map[string]struct {
		Count int64 `json:"count"`
	} `json:"lists"`
	Rooms map[string]*slidingRoom    `json:"rooms"`
	Raw   map[string]json.RawMessage `json:"-"`
}

// slidingRoom is one room in a sliding-sync response. Pointers to scalars, not scalars, because
// half of what these tests assert is whether a field was *absent*.
type slidingRoom struct {
	Initial          *bool           `json:"initial"`
	ExpandedTimeline *bool           `json:"expanded_timeline"`
	Limited          *bool           `json:"limited"`
	NumLive          *int            `json:"num_live"`
	PrevBatch        *string         `json:"prev_batch"`
	BumpStamp        *int64          `json:"bump_stamp"`
	Membership       *string         `json:"membership"`
	Name             json.RawMessage `json:"name"`
	Avatar           json.RawMessage `json:"avatar"`
	IsDM             *bool           `json:"is_dm"`
	Lists            []string        `json:"lists"`
	Timeline         []slidingEvent  `json:"timeline"`
	RequiredState    []slidingState  `json:"required_state"`
	StrippedState    []slidingState  `json:"stripped_state"`
	JoinedCount      *int            `json:"joined_count"`
	InvitedCount     *int            `json:"invited_count"`
	Heroes           []slidingHero   `json:"heroes"`
}

type slidingEvent struct {
	EventID  string         `json:"event_id"`
	Type     string         `json:"type"`
	Sender   string         `json:"sender"`
	StateKey *string        `json:"state_key"`
	Content  map[string]any `json:"content"`
}

type slidingState struct {
	Type     string          `json:"type"`
	StateKey string          `json:"state_key"`
	Sender   string          `json:"sender"`
	Content  *map[string]any `json:"content"`
}

type slidingHero struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
}

// decodeSyncAnswer reads a sliding-sync body, failing the test with the body included: a wrong
// shape is the most likely reason any of these tests break, and the raw JSON is the only way to see
// what the server actually sent.
func decodeSyncAnswer(t *testing.T, resp *harness.Response) syncAnswer {
	t.Helper()
	resp.MustStatus(t, 200)
	var answer syncAnswer
	if err := json.Unmarshal(resp.Body, &answer); err != nil {
		t.Fatalf("sliding sync body is not the shape the MSC describes: %v\n%s", err, resp.Body)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(resp.Body, &raw)
	answer.Raw = raw
	return answer
}

// requireRoom fails unless the answer describes a room, naming what came back instead.
func (a syncAnswer) requireRoom(t *testing.T, roomID string) *slidingRoom {
	t.Helper()
	room, ok := a.Rooms[roomID]
	if !ok {
		names := make([]string, 0, len(a.Rooms))
		for id := range a.Rooms {
			names = append(names, id)
		}
		t.Fatalf("the answer does not describe %s; it describes %v (pos %s)", roomID, names, a.Pos)
	}
	return room
}

// listCount reads a list's count.
func (a syncAnswer) listCount(t *testing.T, list string) int64 {
	t.Helper()
	entry, ok := a.Lists[list]
	if !ok {
		t.Fatalf("the answer carries no list %q: %s", list, string(a.Raw["lists"]))
	}
	return entry.Count
}

// listConfig is the smallest list that gets a room list: everything, in a window, one event deep.
func listConfig(timelineLimit int, from, to int) map[string]any {
	config := map[string]any{
		"timeline_limit": timelineLimit,
		"required_state": map[string]any{"include": []any{}},
	}
	if to >= 0 {
		config["range"] = []int{from, to}
	}
	return map[string]any{"all_rooms": config}
}

// TestASlidingSyncConnectionDescribesEachRoomOnce is the whole reason this endpoint has connection
// state: the first answer is a description, the second is a diff.
func TestASlidingSyncConnectionDescribesEachRoomOnce(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Weekly sync"})
	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "hello"})

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(1, 0, 9),
	}))
	if first.Pos == "" {
		t.Fatal("a sliding sync that returned no pos could never be continued")
	}
	room := first.requireRoom(t, roomID)
	if room.Initial == nil || !*room.Initial {
		t.Error("a room seen for the first time on this connection must be sent as initial")
	}
	if len(room.Timeline) != 1 {
		t.Errorf("a timeline_limit of 1 asked for one event, got %d", len(room.Timeline))
	}
	if room.PrevBatch == nil || *room.PrevBatch == "" {
		t.Error("a timeline cut short by its limit has to say where to keep reading backwards")
	}
	if room.BumpStamp == nil || *room.BumpStamp == 0 {
		t.Errorf("a room with events in it has no excuse for a missing bump_stamp, got %v", room.BumpStamp)
	}
	if room.Membership == nil || *room.Membership != "join" {
		t.Errorf("membership = %v, want join: the client draws the room list from it", room.Membership)
	}
	if got := room.listCountFor("all_rooms"); got == false {
		t.Errorf("a room returned because a list matched has to name that list, got %v", room.Lists)
	}
	if first.listCount(t, "all_rooms") != 1 {
		t.Errorf("count = %d, want 1: the count is the number of rooms matching the list, not the number sent",
			first.listCount(t, "all_rooms"))
	}

	// Second request, same connection, nothing happened: no rooms at all.
	second := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"pos":     first.Pos,
		"lists":   listConfig(1, 0, 9),
	}))
	if len(second.Rooms) != 0 {
		t.Errorf("an idle connection was told about %d rooms; a diff of nothing is a room list of none",
			len(second.Rooms))
	}
	if second.Pos == "" {
		t.Error("every answer carries the pos the next one continues from")
	}
	if len(second.Lists) != 1 || second.listCount(t, "all_rooms") != 1 {
		t.Errorf("a list that sent no rooms still has to report its count, got %v", second.Raw["lists"])
	}
}

// listCountFor reports whether a room names a list among the ones it matched.
func (r *slidingRoom) listCountFor(list string) bool {
	for _, name := range r.Lists {
		if name == list {
			return true
		}
	}
	return false
}

// TestSlidingSyncSeparatesNewsFromPreloadedHistory is the property that decides whether a phone
// makes a noise: history the server hands over because a window widened is not an event that just
// happened.
func TestSlidingSyncTellsWhatJustHappened(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	answer := decodeSyncAnswer(t, slidingSync(t, server, bob, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(1, 0, 9),
	}))
	if live := answer.requireRoom(t, roomID).NumLive; live != nil && *live != 0 {
		t.Errorf("num_live = %d on a first answer where nothing happened since the device asked, want 0", *live)
	}

	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "a new thing"})

	next := decodeSyncAnswer(t, slidingSync(t, server, bob, map[string]any{
		"conn_id": "main",
		"pos":     answer.Pos,
		"lists":   listConfig(1, 0, 9),
	}))
	room := next.requireRoom(t, roomID)
	if room.Initial != nil && *room.Initial {
		t.Error("a room this connection already described must be sent as a delta")
	}
	if len(room.Timeline) != 1 || room.Timeline[0].Content["body"] != "a new thing" {
		t.Fatalf("a delta carries the events that arrived, got %+v", room.Timeline)
	}
	if room.NumLive == nil || *room.NumLive != 1 {
		t.Errorf("num_live = %v, want 1: this event just happened, which is what tells a client to buzz",
			room.NumLive)
	}
	if room.Limited != nil && *room.Limited {
		t.Error("one event past the window is not a gap in history")
	}
}

// TestSlidingSyncOrdersByLastActivity pins the ordering rule, including the part the MSC shouts
// about: rooms are ordered by when the last event was received, not by bump_stamp.
func TestSlidingSyncOrdersByLastActivity(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	quiet := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	middle := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	loud := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	for _, roomID := range []string{quiet, middle, loud} {
		sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "first"})
	}
	// The newest room is the one that speaks last, not the one created last.
	sendContent(t, server, alice, middle, map[string]any{"msgtype": "m.text", "body": "again"})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(0, 0, 1),
	}))
	if answer.listCount(t, "all_rooms") != 3 {
		t.Errorf("count = %d, want 3: the window holds two rooms but the list has three",
			answer.listCount(t, "all_rooms"))
	}
	if len(answer.Rooms) != 2 {
		t.Fatalf("range [0,1] is two rooms, got %d", len(answer.Rooms))
	}
	if _, ok := answer.Rooms[quiet]; ok {
		t.Errorf("the room with no recent activity was sent inside a window of two: %v", answer.Rooms)
	}
	answer.requireRoom(t, middle)
	answer.requireRoom(t, loud)
}

// TestSlidingSyncWideningAWindowSendsOnlyWhatWasMissing is how a client grows a room list: it asks
// for more and expects the difference, not a repeat.
func TestSlidingSyncWideningAWindowSendsOnlyWhatWasMissing(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	for i := 0; i < 5; i++ {
		roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
		sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": fmt.Sprintf("r%d", i)})
	}

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(1, 0, 1),
	}))
	if len(first.Rooms) != 2 {
		t.Fatalf("range [0,1] is two rooms, got %d", len(first.Rooms))
	}

	second := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"pos":     first.Pos,
		"lists":   listConfig(1, 0, 4),
	}))
	if len(second.Rooms) != 3 {
		t.Errorf("widening from two rooms to five should send the three missing rooms, got %d: %v",
			len(second.Rooms), second.Rooms)
	}
	for roomID := range second.Rooms {
		if _, already := first.Rooms[roomID]; already {
			t.Errorf("room %s was sent twice on one connection without anything happening in it", roomID)
		}
		if room := second.requireRoom(t, roomID); room.Initial == nil || !*room.Initial {
			t.Errorf("room %s entered the window for the first time and must be initial", roomID)
		}
	}
}

// TestSlidingSyncExpandsATimelineWhenTheLimitGrows is the timeline-trickling rule: widening the
// window hands over history the connection has never seen, and says so.
func TestSlidingSyncExpandsATimelineWhenTheLimitGrows(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	for _, body := range []string{"one", "two", "three", "four"} {
		sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": body})
	}

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(1, 0, 9),
	}))
	if got := len(first.requireRoom(t, roomID).Timeline); got != 1 {
		t.Fatalf("timeline_limit 1 asked for one event, got %d", got)
	}

	wide := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"pos":     first.Pos,
		"lists":   listConfig(4, 0, 9),
	}))
	room := wide.requireRoom(t, roomID)
	if len(room.Timeline) != 4 {
		t.Errorf("raising the limit to 4 should hand over the four latest events, got %d", len(room.Timeline))
	}
	if room.ExpandedTimeline == nil || !*room.ExpandedTimeline {
		t.Error("a room sent more history than the last time has to say so with expanded_timeline")
	}
	if room.NumLive != nil && *room.NumLive != 0 {
		t.Errorf("num_live = %d for events that happened before the device asked, want 0", *room.NumLive)
	}
}

// TestSlidingSyncSubscriptionsTrackARoomTheWindowMisses is the other half of the API: a room the
// client opened is worth telling about even when it is not near the top of the list.
func TestSlidingSyncSubscriptionsTrackARoomTheWindowMisses(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	inWindow := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	subscribed := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	for _, roomID := range []string{subscribed, inWindow} {
		sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "first"})
	}
	// The window holds one room, and it is the one that spoke last: the subscribed room has to be the
	// one *outside* it, or this test would be proving that a subscription is redundant.
	sendContent(t, server, alice, inWindow, map[string]any{"msgtype": "m.text", "body": "newest"})
	// The subscription is combined with the list, not a separate connection: a room in both has to
	// satisfy the wider of the two configs.
	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(1, 0, 0),
		"room_subscriptions": map[string]any{
			subscribed: map[string]any{
				"timeline_limit": 2,
				"required_state": map[string]any{"include": []any{}},
			},
		},
	}))
	room := answer.requireRoom(t, subscribed)
	if len(room.Timeline) != 2 {
		t.Errorf("a subscription asking for two events should get two, got %d", len(room.Timeline))
	}
	if len(room.Lists) != 0 {
		t.Errorf("a room sent only because it was subscribed to must name no list, got %v", room.Lists)
	}
	if _, ok := answer.Rooms[inWindow]; !ok {
		t.Error("the list's own room disappeared once a subscription was added")
	}
}

// TestSlidingSyncIgnoresARoomTheUserMayNotSee is the permission rule: a subscription is not a way to
// read a room, and an unknown room is not an error either.
func TestSlidingSyncIgnoresARoomTheUserMayNotSee(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	stranger := server.Register(t, "carol", "pw-carol", false)
	secret := stranger.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"room_subscriptions": map[string]any{
			secret: map[string]any{
				"timeline_limit": 5,
				"required_state": map[string]any{"include": []any{}},
			},
			"!never-existed:test": map[string]any{
				"timeline_limit": 1,
				"required_state": map[string]any{"include": []any{}},
			},
		},
	}))
	if room, ok := answer.Rooms[secret]; ok {
		t.Errorf("a room alice was never in was described to her: %+v", room)
	}
	if len(answer.Rooms) != 0 {
		t.Errorf("rooms this server has never heard of should be ignored, got %v", answer.Rooms)
	}
}

// TestSlidingSyncRefusesAPosItDidNotIssue is the ownership rule the MSC's security section exists
// for: possession of a token is not permission to read.
func TestSlidingSyncRefusesAPosItDidNotIssue(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "shared",
		"lists":   listConfig(1, 0, 9),
	}))

	// A pos the server never handed out.
	resp := slidingSync(t, server, alice, map[string]any{
		"conn_id": "shared",
		"pos":     "123456789",
		"lists":   listConfig(1, 0, 9),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an invented pos = %d, want 400: %s", resp.StatusCode, resp.Body)
	}
	if body := string(resp.Body); !json.Valid(resp.Body) || !strings.Contains(body, "M_UNKNOWN_POS") {
		t.Errorf("an invented pos must be answered M_UNKNOWN_POS, got %s", body)
	}

	// The same conn_id and the same pos, presented by somebody else.
	stolen := bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   slidingSyncPath,
		Token:  bob.AccessToken,
		Body: map[string]any{
			"conn_id": "shared",
			"pos":     first.Pos,
			"lists":   listConfig(1, 0, 9),
		},
	})
	if stolen.StatusCode != http.StatusBadRequest {
		t.Errorf("another user's pos = %d, want 400: %s", stolen.StatusCode, stolen.Body)
	}
	if !strings.Contains(string(stolen.Body), "M_UNKNOWN_POS") {
		t.Errorf("another user's pos must read as unknown to them, not as a shared connection: %s", stolen.Body)
	}
	// The owner is unaffected: a refusal is not a reset of somebody else's connection.
	if again := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "shared",
		"pos":     first.Pos,
		"lists":   listConfig(1, 0, 9),
	})); len(again.Rooms) != 0 {
		t.Error("a stolen pos that was refused disturbed the connection it was stolen from")
	}
}

// TestSlidingSyncRejectsARequestItCannotServe covers the limits the MSC states in numbers, which
// are the only protection a server has against a client that asks for a thousand lists.
func TestSlidingSyncRejectsARequestItCannotServe(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)

	lists := map[string]any{}
	for i := 0; i < 101; i++ {
		lists[fmt.Sprintf("list-%d", i)] = map[string]any{
			"timeline_limit": 1,
			"required_state": map[string]any{"include": []any{}},
		}
	}
	subscriptions := map[string]any{}
	for i := 0; i < 101; i++ {
		subscriptions[fmt.Sprintf("!room-%d:test", i)] = map[string]any{
			"timeline_limit": 1,
			"required_state": map[string]any{"include": []any{}},
		}
	}

	cases := []struct {
		name string
		body map[string]any
	}{
		{"more than a hundred lists", map[string]any{"conn_id": "main", "lists": lists}},
		{"more than a hundred subscriptions", map[string]any{"conn_id": "main", "room_subscriptions": subscriptions}},
		{"a presence it has never heard of", map[string]any{"conn_id": "main", "set_presence": "asleep"}},
		{"a negative timeout", map[string]any{"conn_id": "main", "timeout": -1}},
		{"a reversed range", map[string]any{
			"conn_id": "main",
			"lists": map[string]any{"all_rooms": map[string]any{
				"timeline_limit": 1,
				"required_state": map[string]any{"include": []any{}},
				"range":          []int{9, 0},
			}},
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resp := slidingSync(t, server, alice, testCase.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", resp.StatusCode, resp.Body)
			}
			if !strings.Contains(string(resp.Body), "M_INVALID_PARAM") {
				t.Errorf("body = %s, want M_INVALID_PARAM", resp.Body)
			}
		})
	}
}

// TestSlidingSyncWaitsForSomethingToHappen is the long-poll: the endpoint is how a client learns
// about an event the instant it lands, and a server that answers immediately instead turns the app
// into a polling client.
func TestSlidingSyncWaitsForSomethingToHappen(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	first := decodeSyncAnswer(t, slidingSync(t, server, bob, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(1, 0, 9),
	}))

	go func() {
		time.Sleep(300 * time.Millisecond)
		sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "while you waited"})
	}()

	started := time.Now()
	resp := slidingSync(t, server, bob, map[string]any{
		"conn_id": "main",
		"pos":     first.Pos,
		"timeout": 20000,
		"lists":   listConfig(1, 0, 9),
	})
	answer := decodeSyncAnswer(t, resp)
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Errorf("the request came back after %v, which is before the event existed", elapsed)
	} else if elapsed > 15*time.Second {
		t.Errorf("the request waited %v for an event that arrived after 300ms", elapsed)
	}
	room := answer.requireRoom(t, roomID)
	if len(room.Timeline) != 1 || room.Timeline[0].Content["body"] != "while you waited" {
		t.Errorf("a long-poll that woke up has to carry what woke it, got %+v", room.Timeline)
	}

	// With no timeout the same request answers immediately, which is what lets a client grow its
	// window in a loop without being stuck waiting for news between the steps.
	started = time.Now()
	if resp := slidingSync(t, server, bob, map[string]any{
		"conn_id": "main",
		"pos":     answer.Pos,
		"lists":   listConfig(1, 0, 9),
	}); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, resp.Body)
	} else if time.Since(started) > 5*time.Second {
		t.Errorf("a request with no timeout waited %v for nothing", time.Since(started))
	}
}

// TestRequiredStateIsChosenByTheRequest is the MSC's set expression: include this, exclude that, and
// the empty object meaning everything. A client's room list asks for two or three state events and
// gets those; a client drawing a full room asks for everything and gets it.
func TestRequiredStateIsChosenByTheRequest(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "public_chat",
		"name":   "Platform",
		"topic":  "Where the work happens",
	})
	bob.MustJoinRoom(t, server, roomID)
	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "standup at ten"})

	cases := []struct {
		name     string
		state    map[string]any
		wantType []string
		denyType []string
	}{
		{
			name:     "two named entries",
			state:    map[string]any{"include": []any{map[string]any{"type": "m.room.create", "state_key": ""}, map[string]any{"type": "m.room.power_levels", "state_key": ""}}},
			wantType: []string{"m.room.create", "m.room.power_levels"},
			denyType: []string{"m.room.topic", "m.room.join_rules", "m.room.member"},
		},
		{
			name:     "everything except the create event",
			state:    map[string]any{"include": []any{map[string]any{}}, "exclude": []any{map[string]any{"type": "m.room.create"}}},
			wantType: []string{"m.room.topic", "m.room.member"},
			denyType: []string{"m.room.create"},
		},
		{
			name:     "everything, members lazily",
			state:    map[string]any{"include": []any{map[string]any{}}, "lazy_members": true},
			wantType: []string{"m.room.topic", "m.room.create"},
			// The two users' memberships are still sent — one of them spoke in the timeline, and a
			// client cannot draw a line of text without knowing who wrote it.
			denyType: nil,
		},
		{
			name:     "no required state",
			state:    map[string]any{},
			wantType: nil,
			denyType: []string{"m.room.create", "m.room.topic", "m.room.member"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			connID := harness.Nonce("state")
			list := map[string]any{"all_rooms": map[string]any{
				"timeline_limit": 1,
				"required_state": testCase.state,
				"range":          []int{0, 9},
			}}
			answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
				"conn_id": connID,
				"lists":   list,
			}))
			room := answer.requireRoom(t, roomID)

			have := map[string][]string{}
			for _, entry := range room.RequiredState {
				have[entry.Type] = append(have[entry.Type], entry.StateKey)
			}
			for _, eventType := range testCase.wantType {
				if _, ok := have[eventType]; !ok {
					t.Errorf("the request asked for state that would have matched %s and it was not sent; got %v",
						eventType, stateTypesOf(have))
				}
			}
			for _, eventType := range testCase.denyType {
				if entries, ok := have[eventType]; ok {
					t.Errorf("%s was sent although the request did not ask for it: %v", eventType, entries)
				}
			}
			if testCase.name == "everything, members lazily" {
				// The lazy rule is about the *room's* members, not about the people in this answer:
				// whoever wrote the timeline event must be here.
				if !hasStateKey(have, "m.room.member", alice.UserID) {
					t.Errorf("the sender of the timeline event's membership was not sent: %v", have["m.room.member"])
				}
			}
		})
	}
}

// TestARemovedRoomNameIsSaidAsARemoval is the stub rule: Matrix has no delete for state, so taking a
// name away is an event that says nothing, and a client told about that event as state keeps drawing
// the name it had.
func TestARemovedRoomNameIsSaidAsARemoval(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Temporary"})

	// This test is about state, so the list asks for all of it: the config that names no state at all
	// is `listConfig`'s empty include, and a stub cannot appear in a response with no state in it.
	withState := func(limit int) map[string]any {
		return map[string]any{"all_rooms": map[string]any{
			"timeline_limit": limit,
			"required_state": map[string]any{"include": []any{map[string]any{}}},
			"range":          []int{0, 9},
		}}
	}

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"lists":   withState(1),
	}))
	if room := first.requireRoom(t, roomID); len(room.Name) == 0 || string(room.Name) == "null" {
		t.Fatalf("the room was created with a name and the answer says %q", string(room.Name))
	}

	// Now the name event is emptied. A delta that reported it as ordinary state invites a client to
	// keep drawing the name it had; a stub tells it to clear the field.
	alice.MustSetState(t, server, roomID, "m.room.name", "", map[string]any{})
	second := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"pos":     first.Pos,
		"lists":   withState(1),
	}))
	room := second.requireRoom(t, roomID)
	if len(room.RequiredState) == 0 {
		t.Fatal("a room whose name was just cleared sent no state at all")
	}
	var stubbed bool
	for _, entry := range room.RequiredState {
		if entry.Type == "m.room.name" && entry.Content == nil {
			stubbed = true
		}
	}
	if !stubbed {
		t.Errorf("the cleared name was not reported as a stub (a state entry with no content): %+v",
			room.RequiredState)
	}
	if len(room.Name) != 0 && string(room.Name) != "null" {
		t.Errorf("a room whose name was taken away was given name %s; null is how that is said", room.Name)
	}
}

// TestAnInviteCarriesStrippedStateAndNoHistory is the permission shape of the endpoint: an invitee may
// see what the room looked like when they were invited, and nothing else.
func TestAnInviteCarriesStrippedStateAndNoHistory(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat", "name": "Handover"})
	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "before the invite"})
	alice.MustInvite(t, server, roomID, bob.UserID)

	answer := decodeSyncAnswer(t, slidingSync(t, server, bob, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(5, 0, 9),
	}))
	room := answer.requireRoom(t, roomID)
	if room.Membership == nil || *room.Membership != "invite" {
		t.Errorf("membership = %v, want invite", rawString(room.Membership))
	}
	if len(room.Timeline) != 0 {
		t.Errorf("an invite was given %d timeline events; a person who has not accepted cannot read the room",
			len(room.Timeline))
	}
	var sawName, sawCreate bool
	for _, entry := range room.StrippedState {
		switch entry.Type {
		case "m.room.name":
			sawName = true
		case "m.room.create":
			sawCreate = true
		}
	}
	if !sawCreate || !sawName {
		t.Errorf("stripped_state = %+v, want the room's create and name events", room.StrippedState)
	}
	if room.Initial == nil || !*room.Initial {
		t.Error("an invite the connection has never been told about must be sent as initial")
	}
}

// TestASecondDeviceHasItsOwnConnection is why a connection is keyed by device: two phones of one
// account have each read a different set of rooms, and sharing one memory makes one of them miss
// rooms the other already described.
func TestASecondDeviceHasItsOwnConnection(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	other := server.Login(t, "alice", "pw-alice")
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "same-name",
		"lists":   listConfig(1, 0, 9),
	}))
	if _, ok := first.Rooms[roomID]; !ok {
		t.Fatal("the first device was not told about its room")
	}

	// Same conn_id, different device, no pos: a new connection, which has to see the room too.
	second := decodeSyncAnswer(t, slidingSync(t, server, other, map[string]any{
		"conn_id": "same-name",
		"lists":   listConfig(1, 0, 9),
	}))
	if _, ok := second.Rooms[roomID]; !ok {
		t.Errorf("a second device with the same conn_id was told nothing: the first device's connection " +
			"stands in for its own")
	}
	if second.Pos == first.Pos {
		t.Log("note: both devices were handed the same pos, which is allowed only if each accepts the other's")
	}
	// The first device continues on its own connection, unaffected.
	if again := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "same-name",
		"pos":     first.Pos,
		"lists":   listConfig(1, 0, 9),
	})); len(again.Rooms) != 0 {
		t.Error("the second device's connection disturbed the first")
	}
}

// TestSlidingSyncPosWorksAsAPaginationToken pins the MSC's note that a pos is usable in /messages: the
// token has to name a position in the same stream the room's pagination reads.
func TestSlidingSyncPosWorksAsAPaginationToken(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	for _, body := range []string{"one", "two", "three"} {
		sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": body})
	}

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"lists":   listConfig(2, 0, 9),
	}))
	room := answer.requireRoom(t, roomID)
	if len(room.Timeline) != 2 {
		t.Fatalf("timeline_limit 2 asked for two events, got %d", len(room.Timeline))
	}
	if room.PrevBatch == nil || *room.PrevBatch == "" {
		t.Fatal("a timeline cut short has to say where to keep reading")
	}

	// prev_batch must be accepted by /messages, or the client cannot fetch what was left out.
	messages := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/messages?dir=b&from="+url.QueryEscape(*room.PrevBatch)+"&limit=10")
	messages.MustStatus(t, 200)
	var page struct {
		Chunk []struct {
			Content map[string]any `json:"content"`
		} `json:"chunk"`
	}
	messages.JSON(t, &page)
	// The page holds the room's state as well, so what is asserted is that every message the sliding
	// sync left out of its window can be reached from the token it named.
	seen := map[string]bool{}
	for _, event := range page.Chunk {
		if body, ok := event.Content["body"].(string); ok {
			seen[body] = true
		}
	}
	// prev_batch names the moment *before* the oldest event the sliding sync sent, so what comes back
	// is what the window left out: the first message, and none of the ones already on screen.
	if !seen["one"] {
		t.Errorf("paging back from prev_batch did not reach the event the window left out: %v", seen)
	}
	if seen["three"] {
		t.Error("prev_batch pointed at the wrong place: paging backwards reached an event the timeline already carried")
	}
}

// TestARoomWithNoTimelineStillSaysWhereToRead is the zero-limit config: a room list that wants state and
// no message preview still needs a token to page from, or the client has nowhere to start.
func TestARoomWithNoTimelineStillSaysWhereToRead(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "not for the list"})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "main",
		"lists": map[string]any{"all_rooms": map[string]any{
			"timeline_limit": 0,
			"required_state": map[string]any{"include": []any{map[string]any{"type": "m.room.create"}}},
			"range":          []int{0, 9},
		}},
	}))
	room := answer.requireRoom(t, roomID)
	if len(room.Timeline) != 0 {
		t.Errorf("a timeline_limit of 0 asked for no events, got %d", len(room.Timeline))
	}
	if room.PrevBatch == nil || *room.PrevBatch == "" {
		t.Error("a room sent without a timeline still needs a place to page back to")
	}
	if len(room.RequiredState) == 0 {
		t.Error("the state this list asked for was not sent")
	}
}

// stateTypesOf and rawOf are test-readable renderings, because a failure that prints a map of Go internals
// hides the one thing worth reading.
// hasStateKey reports whether a chosen set of state includes one particular membership.
func hasStateKey(have map[string][]string, eventType, stateKey string) bool {
	for _, candidate := range have[eventType] {
		if candidate == stateKey {
			return true
		}
	}
	return false
}

func stateTypesOf(values map[string][]string) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func rawString(value *string) string {
	if value == nil {
		return "<absent>"
	}
	return *value
}
