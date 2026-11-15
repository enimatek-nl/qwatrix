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

// A list's filters are what turns "my rooms" into "the rooms a client is drawing", and they are the
// difference between an app that opens onto a room list and one that opens onto a room list plus every
// space, bridge and abandoned experiment the account ever joined.
//
// Two properties run through these cases. Filters are applied *before* the range, so a window of twenty
// is twenty matching rooms rather than whatever twenty happened to be recent; and `count` reports the
// rooms that match, not the rooms sent, because that is the number a client uses to decide whether to
// ask for more.

// putGlobalAccountData writes one account-data type, which is how a client records `m.direct`.
func putGlobalAccountData(t *testing.T, server *harness.Server, who *harness.Account, dataType string, value any) {
	t.Helper()
	who.MustPutJSON(t, server, "/_matrix/client/v3/user/"+harness.PathEscape(who.UserID)+
		"/account_data/"+dataType, value).MustStatus(t, 200)
}

// putRoomAccountData writes one room's account data, which is where a room's `m.tag` lives.
func putRoomAccountData(t *testing.T, server *harness.Server, who *harness.Account, roomID, dataType string, value any) {
	t.Helper()
	who.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/account_data/"+dataType, value).MustStatus(t, 200)
}

// listWithFilter is a one-list request whose filters are given by the caller.
func listWithFilter(filters map[string]any, from, to int) map[string]any {
	config := map[string]any{
		"timeline_limit": 1,
		"required_state": map[string]any{"include": []any{}},
		"range":          []int{from, to},
	}
	if filters != nil {
		config["filters"] = filters
	}
	return map[string]any{"all_rooms": config}
}

// TestAListOfOnlyDirectRooms is the filter Element X uses for its people tab: `m.direct` is account
// data the client owns, so the server reads it rather than keeping its own notion of who is a
// contact.
func TestAListOfOnlyDirectRooms(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	direct := alice.MustCreateRoom(t, server, map[string]any{"preset": "trusted_private_chat"})
	ordinary := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	sendContent(t, server, alice, ordinary, map[string]any{"msgtype": "m.text", "body": "team"})
	sendContent(t, server, alice, direct, map[string]any{"msgtype": "m.text", "body": "hi"})
	putGlobalAccountData(t, server, alice, "m.direct", map[string]any{bob.UserID: []string{direct}})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "dm-list",
		"lists":   listWithFilter(map[string]any{"is_dm": true}, 0, 9),
	}))
	if _, ok := answer.Rooms[direct]; !ok {
		t.Errorf("the direct room was left out of the direct list: %v", roomIDsOf(answer))
	}
	if _, ok := answer.Rooms[ordinary]; ok {
		t.Errorf("a room nobody tagged as direct was in the direct list: %v", roomIDsOf(answer))
	}
	if answer.listCount(t, "all_rooms") != 1 {
		t.Errorf("count = %d, want 1: the count is the rooms matching the filter, not the rooms sent",
			answer.listCount(t, "all_rooms"))
	}

	inverse := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "room-list",
		"lists":   listWithFilter(map[string]any{"is_dm": false}, 0, 9),
	}))
	if _, ok := inverse.Rooms[ordinary]; !ok {
		t.Errorf("is_dm false should have kept the ordinary room: %v", roomIDsOf(inverse))
	}
	if _, ok := inverse.Rooms[direct]; ok {
		t.Error("is_dm false kept the direct room as well, which makes the filter do nothing")
	}
}

// TestAListOfOnlyEncryptedRooms reads the room's encryption state, which a client needs before it can
// offer to start a secure conversation from the list at all.
func TestAListOfOnlyEncryptedRooms(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	secure := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	outInTheOpen := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSetState(t, server, secure, "m.room.encryption", "", map[string]any{
		"algorithm": "m.megolm.v1.aes-sha2",
	})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "secure",
		"lists":   listWithFilter(map[string]any{"is_encrypted": true}, 0, 9),
	}))
	if _, ok := answer.Rooms[secure]; !ok {
		t.Errorf("the encrypted room was not in the encrypted list: %v", roomIDsOf(answer))
	}
	if _, ok := answer.Rooms[outInTheOpen]; ok {
		t.Errorf("an unencrypted room was in the encrypted list: %v", roomIDsOf(answer))
	}
}

// TestAListOfRoomsWaitingForAnAnswer is `is_invited`: the invites a client badges are drawn from the
// same endpoint as the rooms, so that a device shows one list rather than two sources of truth.
func TestAListOfRoomsWaitingForAnAnswer(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	waiting := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	answered := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, answered)
	alice.MustInvite(t, server, waiting, bob.UserID)

	answer := decodeSyncAnswer(t, slidingSync(t, server, bob, map[string]any{
		"conn_id": "invites",
		"lists":   listWithFilter(map[string]any{"is_invited": true}, 0, 9),
	}))
	if _, ok := answer.Rooms[waiting]; !ok {
		t.Errorf("the outstanding invite was not in the invite list: %v", roomIDsOf(answer))
	}
	if _, ok := answer.Rooms[answered]; ok {
		t.Errorf("a room bob already joined was in the invite list: %v", roomIDsOf(answer))
	}
}

// TestAListOfSpacesAndAListOfEverythingElse is the room-type pair, including the rule that
// `not_room_types` wins over `room_types` when both name the same thing: a client's general list is
// "everything except spaces", and it must not have to know how the two interact.
func TestAListOfSpacesAndAListOfEverythingElse(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	space := alice.MustCreateRoom(t, server, map[string]any{"room_version": "11", "creation_content": map[string]any{
		"type": "m.space",
	}, "preset": "private_chat"})
	room := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	spaces := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "spaces",
		"lists":   listWithFilter(map[string]any{"room_types": []any{"m.space"}}, 0, 9),
	}))
	if _, ok := spaces.Rooms[space]; !ok {
		t.Errorf("the space was not in the space list: %v", roomIDsOf(spaces))
	}
	if _, ok := spaces.Rooms[room]; ok {
		t.Errorf("an ordinary room was in the space list: %v", roomIDsOf(spaces))
	}

	everythingElse := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "rooms",
		"lists":   listWithFilter(map[string]any{"not_room_types": []any{"m.space"}}, 0, 9),
	}))
	if _, ok := everythingElse.Rooms[room]; !ok {
		t.Errorf("an ordinary room was left out of the not-a-space list: %v", roomIDsOf(everythingElse))
	}
	if _, ok := everythingElse.Rooms[space]; ok {
		t.Errorf("the space was in the not-a-space list: %v", roomIDsOf(everythingElse))
	}

	both := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "both",
		"lists": listWithFilter(map[string]any{
			"room_types":     []any{"m.space"},
			"not_room_types": []any{"m.space"},
		}, 0, 9),
	}))
	if len(both.Rooms) != 0 {
		t.Errorf("naming a type in both lists should keep nothing, kept %v", roomIDsOf(both))
	}
}

// TestAListFilteredByTag is room tagging, which lives in the room's own `m.tag` account data: a client
// draws a favourites list from it, and the list must not repeat the favourites in the general one.
func TestAListFilteredByTag(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	favourite := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	plain := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	putRoomAccountData(t, server, alice, favourite, "m.tag", map[string]any{
		"tags": map[string]any{"m.favourite": map[string]any{"order": 1.0}},
	})

	favourites := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "fav",
		"lists":   listWithFilter(map[string]any{"tags": []any{"m.favourite"}}, 0, 9),
	}))
	if _, ok := favourites.Rooms[favourite]; !ok {
		t.Errorf("the tagged room was not in the tagged list: %v", roomIDsOf(favourites))
	}
	if _, ok := favourites.Rooms[plain]; ok {
		t.Errorf("an untagged room was in the favourites list: %v", roomIDsOf(favourites))
	}

	general := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "general",
		"lists":   listWithFilter(map[string]any{"not_tags": []any{"m.favourite"}}, 0, 9),
	}))
	if _, ok := general.Rooms[plain]; !ok {
		t.Errorf("the untagged room was left out of the list that excludes favourites: %v", roomIDsOf(general))
	}
	if _, ok := general.Rooms[favourite]; ok {
		t.Error("not_tags did not exclude the tagged room, so a client would show it twice")
	}
}

// TestAListOfOneSpacesRooms reads the space's own `m.space.child` state: the server looks only in the
// spaces the client named and the user is joined to, and never walks into subspaces — that is the MSC's
// rule, and it is what keeps a list of one space's rooms from becoming a recursive federation crawl.
func TestAListOfOneSpacesRooms(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	carol := server.Register(t, "carol", "pw-carol", false)
	space := alice.MustCreateRoom(t, server, map[string]any{
		"room_version": "11", "preset": "private_chat",
		"creation_content": map[string]any{"type": "m.space"},
	})
	child := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	elsewhere := carol.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSetState(t, server, space, "m.space.child", child, map[string]any{"via": []string{}})
	// A space alice is not in names a child she may not be told about; the MSC says to ignore the
	// space, which also stops a filter being a way to enumerate rooms over somebody's shoulder.
	carol.MustSetState(t, server, elsewhere, "m.space.child", child, map[string]any{})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "space-children",
		"lists":   listWithFilter(map[string]any{"spaces": []any{space}}, 0, 9),
	}))
	if _, ok := answer.Rooms[child]; !ok {
		t.Errorf("the room named as a child of the space was not in the space list: %v", roomIDsOf(answer))
	}
	if _, ok := answer.Rooms[elsewhere]; ok {
		t.Errorf("the space filter returned a room the space does not name: %v", roomIDsOf(answer))
	}

	unknown := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "unknown-space",
		"lists":   listWithFilter(map[string]any{"spaces": []any{"!no-such-space:test"}}, 0, 9),
	}))
	if len(unknown.Rooms) != 0 {
		t.Errorf("a space this server has never heard of should match nothing, matched %v", roomIDsOf(unknown))
	}
}

// TestFiltersAreAppliedBeforeTheRange is the ordering that makes a sliding window work at all: a client
// asking for twenty direct chats wants twenty direct chats, not twenty rooms of which it is then told
// three are direct.
func TestFiltersAreAppliedBeforeTheRange(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	direct := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "trusted_private_chat"})
		sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": fmt.Sprintf("dm %d", i)})
		direct = append(direct, roomID)
	}
	for i := 0; i < 4; i++ {
		roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
		sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": fmt.Sprintf("room %d", i)})
	}
	putGlobalAccountData(t, server, alice, "m.direct", map[string]any{bob.UserID: direct})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "two-dms",
		"lists":   listWithFilter(map[string]any{"is_dm": true}, 0, 1),
	}))
	if len(answer.Rooms) != 2 {
		t.Fatalf("range [0,1] of the direct list sent %d rooms: %v", len(answer.Rooms), roomIDsOf(answer))
	}
	if answer.listCount(t, "all_rooms") != 3 {
		t.Errorf("count = %d, want 3: the filter matches three rooms even though two were sent",
			answer.listCount(t, "all_rooms"))
	}
	for roomID := range answer.Rooms {
		if !containsString(direct, roomID) {
			t.Errorf("a non-direct room %s was in the window of a filtered list", roomID)
		}
	}
}

// roomIDsOf renders an answer's rooms for a failure message.
func roomIDsOf(answer syncAnswer) []string {
	out := make([]string, 0, len(answer.Rooms))
	for roomID := range answer.Rooms {
		out = append(out, roomID)
	}
	sort.Strings(out)
	return out
}

// The extensions: the streams that are not rooms.
//
// MSC4186 leaves the shape of an extension to a future MSC, so these tests pin the shapes the clients
// actually speak — the ones matrix-rust-sdk parses against Synapse today. A server that answers
// `typing` with a nicer shape of its own is a server Element X cannot use.

// extensionOf decodes one extension out of an answer, or reports that it was not sent at all.
func extensionOf(t *testing.T, answer syncAnswer, name string) map[string]any {
	t.Helper()
	raw, ok := answer.Raw["extensions"]
	if !ok {
		t.Fatalf("the answer carried no extensions at all: %s", answer.Raw["pos"])
	}
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extensions); err != nil {
		t.Fatalf("extensions is not an object: %v", err)
	}
	body, ok := extensions[name]
	if !ok {
		t.Fatalf("extension %q was not answered (answered: %s)", name, raw)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("extension %q is not an object: %v", name, err)
	}
	return value
}

// enableAll turns on every extension this server serves, which is what a client does.
func enableAll() map[string]any {
	on := map[string]any{"enabled": true}
	return map[string]any{
		"to_device":    on,
		"e2ee":         on,
		"typing":       on,
		"receipts":     on,
		"account_data": on,
	}
}

// TestAConnectionWithoutExtensionsIsNotGivenAny is the cheap case that has to keep working: an app that
// asks only for rooms should not pay for, or be told about, five streams it did not ask for.
func TestAConnectionWithoutExtensionsIsNotGivenAny(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "hello"})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id": "no-extensions",
		"lists":   listWithFilter(nil, 0, 4),
	}))
	if _, ok := answer.Raw["extensions"]; ok {
		t.Errorf("an answer to a request with no extensions carried %s", answer.Raw["extensions"])
	}
}

// TestTypingReachesASlidingSyncConnection is the typing indicator: the room-shaped extension reports
// who is typing now, in the rooms this answer is about.
func TestTypingReachesASlidingSyncConnection(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "typing",
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	// A connection that has just been opened has not missed anything, so the first answer says nobody
	// is typing rather than reporting whoever happens to be mid-sentence.
	typing := extensionOf(t, first, "typing")
	if rooms, ok := typing["rooms"].(map[string]any); !ok || len(rooms) != 0 {
		t.Errorf("a new connection was told people were typing: %v", typing)
	}

	bob.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/typing/"+harness.PathEscape(bob.UserID), map[string]any{"typing": true, "timeout": 30000})
	second := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "typing",
		"pos":        first.Pos,
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	typing = extensionOf(t, second, "typing")
	rooms, ok := typing["rooms"].(map[string]any)
	if !ok {
		t.Fatalf("typing extension has no rooms object: %v", typing)
	}
	content, ok := rooms[roomID].(map[string]any)
	if !ok {
		t.Fatalf("this room's typing was not reported: %v", rooms)
	}
	users, _ := content["user_ids"].([]any)
	if len(users) != 1 || users[0] != bob.UserID {
		t.Errorf("typing reported %v, want just %s", users, bob.UserID)
	}
}

// TestAReceiptReachesASlidingSyncConnection is the read marker. A receipt is a fact that outlives the
// moment it was sent, so unlike typing it must survive a reconnect: it is how a second device learns
// that the notifications it was showing have been read on the first.
func TestAReceiptReachesASlidingSyncConnection(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "read me"})

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "receipts",
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	if len(first.Rooms[roomID].Timeline) == 0 {
		t.Fatal("the test's premise is wrong: the answer carried no timeline to acknowledge")
	}
	eventID := first.Rooms[roomID].Timeline[len(first.Rooms[roomID].Timeline)-1].EventID

	bob.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/receipt/m.read/"+harness.PathEscape(eventID), map[string]any{})

	second := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "receipts",
		"pos":        first.Pos,
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	receipts := extensionOf(t, second, "receipts")
	rooms, ok := receipts["rooms"].(map[string]any)
	if !ok {
		t.Fatalf("receipts extension has no rooms object: %v", receipts)
	}
	content, ok := rooms[roomID].(map[string]any)
	if !ok {
		t.Fatalf("this room's receipts were not reported: %v", rooms)
	}
	// A receipt content is keyed by the event it acknowledges, then by the receipt type, then by the
	// user who sent it — the shape m.receipt has always had, and the shape a client walks to move a
	// read marker to the right place.
	acknowledged, ok := content[eventID].(map[string]any)
	if !ok {
		t.Fatalf("the receipt did not acknowledge the event it was sent for (%s): %v", eventID, content)
	}
	read, ok := acknowledged["m.read"].(map[string]any)
	if !ok {
		t.Fatalf("the receipt is not an m.read: %v", acknowledged)
	}
	if _, ok := read[bob.UserID]; !ok {
		t.Errorf("the receipt did not name %s: %v", bob.UserID, read)
	}
}

// TestAToDeviceMessageReachesASlidingSyncConnection is the one extension that decides whether
// encryption works: a room key shared with one device rather than a room travels through the device's
// queue, and a connection that never drained it would leave a device able to encrypt and unable to
// read.
func TestAToDeviceMessageReachesASlidingSyncConnection(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "to-device",
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	if _, ok := answer.Raw["extensions"]; !ok {
		t.Fatal("an answer to a request that enabled to_device carried no extensions")
	}
	// Nothing is queued yet, but the shape has to be there: a client tracks a cursor here and cannot
	// be made to guess one.
	first := extensionOf(t, answer, "to_device")
	if _, ok := first["next_batch"]; !ok {
		t.Errorf("the to_device extension answered %v without a cursor", first)
	}

	response := sendToDevice(t, bob, server, "extension-share-1",
		map[string]map[string]any{alice.UserID: {alice.DeviceID: map[string]any{"session_id": "shared-one"}}})
	if response.StatusCode != 200 {
		t.Fatalf("sending a to-device message: got %d\n%s", response.StatusCode, response.Body)
	}

	second := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "to-device",
		"pos":        answer.Pos,
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	payload := extensionOf(t, second, "to_device")
	events, _ := payload["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("the queued message did not reach the connection: %v", payload)
	}
	message, _ := events[0].(map[string]any)
	if message["type"] != "m.room_key" {
		t.Errorf("the delivered to-device message is %v", message)
	}
	content, _ := message["content"].(map[string]any)
	if content["session_id"] != "shared-one" {
		t.Errorf("the delivered message carries %v", content)
	}

	// A queue is consumed, not positioned: the same message must not be delivered twice, because a
	// room key shared twice is a key two sessions could use.
	third := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "to-device",
		"pos":        second.Pos,
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	if events, _ := extensionOf(t, third, "to_device")["events"].([]any); len(events) != 0 {
		t.Errorf("a delivered to-device message was delivered again: %v", events)
	}
}

// TestEncryptionCountsRideAlong is the e2ee extension: a device that has run out of one-time keys
// cannot be told so by an event, so the count has to be in every answer that asks for it.
func TestEncryptionCountsRideAlong(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	upload(t, alice, server, map[string]any{"one_time_keys": oneTimeKeysSet(3)})
	// The same number two endpoints report has to be the same number, or a client that reads one and
	// writes the other is going to be told to upload keys it just uploaded.
	status := alice.Do(t, server, harness.Request{Method: http.MethodGet, Path: "/_matrix/client/v3/keys/status"})
	status.MustStatus(t, 200)

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "e2ee",
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	e2ee := extensionOf(t, answer, "e2ee")
	if text := string(status.Body); text == "" {
		t.Fatal("the key status endpoint answered nothing")
	}
	counts, ok := e2ee["device_one_time_keys_count"].(map[string]any)
	if !ok {
		t.Fatalf("the e2ee extension has no key counts: %v", e2ee)
	}
	if counts["signed_curve25519"] != float64(3) {
		t.Errorf("one-time key counts = %v, want three signed_curve25519; GET /keys/status said %s",
			counts, status.Body)
	}
	if _, ok := e2ee["device_unused_fallback_key_types"].([]any); !ok {
		t.Errorf("the e2ee extension omitted the fallback key types, which MSC3391 says are always included: %v", e2ee)
	}
	lists, ok := e2ee["device_lists"].(map[string]any)
	if !ok {
		t.Fatalf("the e2ee extension has no device_lists: %v", e2ee)
	}
	if changed, _ := lists["changed"].([]any); len(changed) != 0 {
		t.Errorf("a new connection was told devices changed: %v", changed)
	}
}

// TestDevicesThatAppearAreReportedToASlidingSyncConnection is the other half of the same extension: a
// colleague who buys a phone is invisible to a sender that keeps encrypting for the old set of
// devices, and the symptom — one device never receiving anything — is not something the sender can see.
func TestDevicesThatAppearAreReportedToASlidingSyncConnection(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "device-lists",
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))

	// Bob joins the room, which is the moment alice's client needs his device set: a message she
	// encrypts now has to reach him, and he was not in the room she last looked at.
	bob.MustJoinRoom(t, server, roomID)

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "device-lists",
		"pos":        first.Pos,
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	lists := extensionOf(t, answer, "e2ee")["device_lists"].(map[string]any)
	changed, _ := lists["changed"].([]any)
	if !containsString(stringsOf(changed), bob.UserID) {
		t.Errorf("device_lists.changed = %v, want it to include %s", changed, bob.UserID)
	}
}

// TestAccountDataReachesASlidingSyncConnection is how a device learns that a room was marked read,
// that a push rule changed, or that its own user added a contact: all three are account data, and all
// three are invisible to the room lists.
func TestAccountDataReachesASlidingSyncConnection(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	first := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "account-data",
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	global, _ := extensionOf(t, first, "account_data")["global"].([]any)
	if len(global) != 0 {
		t.Errorf("a new connection was told account data had changed: %v", global)
	}

	putRoomAccountData(t, server, alice, roomID, "m.tag", map[string]any{
		"tags": map[string]any{"m.favourite": map[string]any{"order": 1.0}},
	})
	putGlobalAccountData(t, server, alice, "m.direct", map[string]any{})

	second := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "account-data",
		"pos":        first.Pos,
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": enableAll(),
	}))
	accountData := extensionOf(t, second, "account_data")
	rooms, ok := accountData["rooms"].(map[string]any)
	if !ok {
		t.Fatalf("the account_data extension has no rooms object: %v", accountData)
	}
	events, ok := rooms[roomID].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("this room's account data was not reported: %v", rooms)
	}
	entry, _ := events[0].(map[string]any)
	if entry["type"] != "m.tag" {
		t.Errorf("the reported room account data is %v", entry)
	}
	global, _ = accountData["global"].([]any)
	found := false
	for _, raw := range global {
		if entry, _ := raw.(map[string]any); entry["type"] == "m.direct" {
			found = true
		}
	}
	if !found {
		t.Errorf("the global batch did not include the m.direct write: %v", global)
	}
}

// TestAnUnknownExtensionIsIgnoredRatherThanRefused is the compatibility rule that lets clients grow: a
// server that met an extension name for the first time has to keep serving the rooms, because refusing
// the whole request would leave an app that enabled one new extension unable to sync at all.
func TestAnUnknownExtensionIsIgnoredRatherThanRefused(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.text", "body": "hello"})

	answer := decodeSyncAnswer(t, slidingSync(t, server, alice, map[string]any{
		"conn_id":    "unknown-extension",
		"lists":      listWithFilter(nil, 0, 4),
		"extensions": map[string]any{"org.example.some_future_thing": map[string]any{"enabled": true}},
	}))
	if _, ok := answer.Rooms[roomID]; !ok {
		t.Errorf("a request naming an unknown extension was not given its rooms: %v", roomIDsOf(answer))
	}
	if !extensionAbsent(answer, "org.example.some_future_thing") {
		t.Errorf("the server answered an extension it does not implement: %s", answer.Raw["extensions"])
	}
}

// extensionAbsent reports whether an extension was left out of an answer.
func extensionAbsent(answer syncAnswer, name string) bool {
	raw, ok := answer.Raw["extensions"]
	if !ok {
		return true
	}
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extensions); err != nil {
		return false
	}
	_, ok = extensions[name]
	return !ok
}

// stringsOf turns a decoded JSON array into strings for a membership test.
func stringsOf(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// TestTheVersionsEndpointAdvertisesSlidingSync pins the flag that makes the commitment.
//
// matrix-rust-sdk — the library Element X is built from — reads this flag to decide between a
// sliding-sync proxy and this server's own endpoint. Advertising it is therefore not documentation: it
// is a client giving up /sync's answers and holding this server to its own, which is why the flag is
// set once the endpoint can carry a client and not before, and why it is tested rather than mentioned.
func TestTheVersionsEndpointAdvertisesSlidingSync(t *testing.T) {
	server := harness.NewWithRegistration(t)
	body := server.Do(t, harness.Request{Method: http.MethodGet, Path: "/_matrix/client/versions"})
	body.MustStatus(t, 200)
	var versions struct {
		Versions         []string        `json:"versions"`
		UnstableFeatures map[string]bool `json:"unstable_features"`
	}
	body.JSON(t, &versions)
	if !versions.UnstableFeatures["org.matrix.simplified_msc3575"] {
		t.Errorf("GET /versions does not advertise simplified sliding sync: %v", versions.UnstableFeatures)
	}
	// The flag and the route have to agree, in both directions: a flag with no route is a client that
	// cannot sync, and a route with no flag is a client that never finds it.
	answer := decodeSyncAnswer(t, slidingSync(t, server, server.Register(t, "alice", "pw-alice", false), map[string]any{
		"conn_id": "flagged",
		"lists":   listWithFilter(nil, 0, 1),
	}))
	if answer.Pos == "" {
		t.Error("the endpoint behind the advertised flag did not answer with a position")
	}
}
