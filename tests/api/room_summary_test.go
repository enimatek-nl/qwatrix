package api_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// TestRoomSummaryCountsFollowTheMembership checks that the counts a client draws a room list from
// track the room rather than only growing.
//
// A sync summary exists so a client can render a room list without asking for state, which makes
// it a claim about the room. An invited count that never comes back down tells the user somebody
// is still being asked, after that somebody joined — and a summary is exactly the kind of data a
// client keeps rather than re-checks.
func TestRoomSummaryCountsFollowTheMembership(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "hostess", "pw-hostess", false)
	bob := server.Register(t, "guestbob", "pw-guestbob", false)
	token := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch

	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "public_chat", "invite": []string{bob.UserID},
	})

	// One joined (alice), one invited (bob).
	summary := summaryOf(t, alice, server, &token, roomID)
	if got := numberAt(summary, "m.joined_member_count"); got != 1 {
		t.Errorf("joined = %v, want 1", got)
	}
	if got := numberAt(summary, "m.invited_member_count"); got != 1 {
		t.Errorf("invited = %v, want 1", got)
	}

	// Accepting the invite moves the same person between the two counts.
	bob.MustJoinRoom(t, server, roomID)
	summary = summaryOf(t, alice, server, &token, roomID)
	if got := numberAt(summary, "m.joined_member_count"); got != 2 {
		t.Errorf("after the join, joined = %v, want 2", got)
	}
	if got := numberAt(summary, "m.invited_member_count"); got != 0 {
		t.Errorf("after the join, invited = %v, want 0", got)
	}

	// Leaving takes them out of the joined count and does not put them back into the invited
	// one: a former member is not waiting to be let in.
	bob.MustLeaveRoom(t, server, roomID)
	summary = summaryOf(t, alice, server, &token, roomID)
	if got := numberAt(summary, "m.joined_member_count"); got != 1 {
		t.Errorf("after the leave, joined = %v, want 1", got)
	}
	if got := numberAt(summary, "m.invited_member_count"); got != 0 {
		t.Errorf("after the leave, invited = %v, want 0", got)
	}
}

// summaryOf waits for the room to appear in the next incremental sync and returns its summary.
func summaryOf(t *testing.T, account *harness.Account, server *harness.Server, token *string, roomID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sync := account.MustSync(t, server, url.Values{
			"since": []string{*token}, "timeout": []string{"0"},
		})
		*token = sync.NextBatch
		if room, ok := sync.Rooms.Join[roomID]; ok && room.Summary != nil {
			return room.Summary
		}
		if time.Now().After(deadline) {
			t.Fatal("the room never appeared with a summary in an incremental sync")
		}
	}
}

// numberAt reads a summary count, treating an absent key as zero.
func numberAt(summary map[string]any, key string) int64 {
	value, ok := summary[key]
	if !ok || value == nil {
		return 0
	}
	number, ok := value.(float64)
	if !ok {
		return -1
	}
	return int64(number)
}

// TestMessagesLazyLoadingSendsOnlyNeededMembers checks MSC1337 on /messages.
//
// A client that asks for members lazily is saying it already holds the member events it needs and
// only wants the ones it cannot render the page without. In a room with a thousand members that is
// the difference between a page of messages and a page of state.
func TestMessagesLazyLoadingSendsOnlyNeededMembers(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "watcher", "pw-watcher", false)
	bob := server.Register(t, "talker", "pw-talker", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	token := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch
	bob.MustSendText(t, server, roomID, "bob spoke here")
	page := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+
		"/messages?dir=f&from="+harness.PathEscape(token)+
		"&filter=%7B%22lazy_load_members%22%3Atrue%7D"))

	chunk, _ := page["chunk"].([]any)
	if len(chunk) == 0 {
		t.Fatalf("the forward page came back empty: %v", page)
	}
	state, ok := page["state"].([]any)
	if !ok {
		t.Fatalf("no state section: %v", page)
	}
	for _, entry := range state {
		event, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("state entry is not an object: %v", entry)
		}
		if event["type"] != "m.room.member" {
			t.Errorf("lazy loading sent %v in the state section, want only member events",
				event["type"])
		}
		if event["state_key"] != bob.UserID {
			t.Errorf("lazy loading sent the member event of %v; only the chunk's senders are needed",
				event["state_key"])
		}
	}
	// The chunk has one sender, so one member event is the whole answer — alice's own join,
	// the room name and the power levels are all state the client already has.
	if len(state) != 1 {
		t.Errorf("the lazy state section held %d events, want exactly 1: %v", len(state), state)
	}

	// Without the filter, the state section is the room's state — the client asked for it.
	full := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/messages?dir=f&from="+harness.PathEscape(token)))
	if fullState, ok := full["state"].([]any); !ok || len(fullState) <= len(state) {
		t.Errorf("an unfiltered page sent %v state events, want more than the %d lazy ones",
			full["state"], len(state))
	}
}

// TestPaginationTokensContinueWithoutGapsOrRepeats checks the two rules a paging client depends
// on: a token names an event the client already has, and walking to the end of the room shows
// every event exactly once.
//
// A backward read includes the event its `from` names, because the token a client holds is a sync
// token, and a sync token names the newest event it has seen. Excluding that event loses one event
// per page, and the client cannot notice — its history simply has a hole where its own last message
// was. And `end` is omitted once there is nowhere left to go, because the specification makes the
// omission the signal to stop: a server that always answers with a token has a client that always
// asks again.
func TestPaginationTokensContinueWithoutGapsOrRepeats(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "pagger", "pw-pagger", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	const messages = 4
	for i := 0; i < messages; i++ {
		alice.MustSendText(t, server, roomID, fmt.Sprintf("page-%d", i))
	}

	// The token a client pages backward from is the one its last /sync gave it.
	token := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch
	page := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/messages?dir=b&limit=1&from="+harness.PathEscape(token)))
	chunk, _ := page["chunk"].([]any)
	if len(chunk) != 1 {
		t.Fatalf("a one-event backward page held %d events", len(chunk))
	}
	newest := chunk[0].(map[string]any)
	if body, _ := newest["content"].(map[string]any)["body"].(string); body != "page-3" {
		t.Errorf("paging back from a sync token returned %v first, want the event that token names",
			body)
	}

	// Walk to the start of the room. Every event must appear exactly once, and the walk has to
	// terminate: `end` disappearing is the only stop signal a client is given.
	seen := map[string]int{}
	from := token
	for steps := 0; steps < 200; steps++ {
		body := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
			harness.PathEscape(roomID)+"/messages?dir=b&limit=3&from="+harness.PathEscape(from)))
		events, _ := body["chunk"].([]any)
		for _, entry := range events {
			event := entry.(map[string]any)
			id, _ := event["event_id"].(string)
			seen[id]++
		}
		next, ok := body["end"].(string)
		if !ok || next == "" {
			// Reached the beginning. The room's create event is the floor.
			if len(seen) < messages+6 {
				t.Fatalf("paging to the start saw %d events, want the whole room", len(seen))
			}
			for id, count := range seen {
				if count > 1 {
					t.Errorf("event %s was served %d times while paging backwards", id, count)
				}
			}
			return
		}
		from = next
	}
	t.Fatal("backward pagination never reached the start of the room, so a client would loop forever")
}

// TestMessagesWithoutATokenStartsAtTheLatestMessage checks the request a client makes when it has
// no token at all: "give me the end of this room".
//
// An absent `from` is not position zero. Read that way, "the newest 5 events" becomes "the 5 events
// before anything happened", and the answer is an empty timeline from a server that sounded fine.
func TestMessagesWithoutATokenStartsAtTheLatestMessage(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "tokenless", "pw-tokenless", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	for _, body := range []string{"first", "second", "last"} {
		alice.MustSendText(t, server, roomID, body)
	}

	read := func(query string) map[string]any {
		t.Helper()
		resp := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
			harness.PathEscape(roomID)+"/messages?"+query)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /messages?%s: got %d, want 200\nbody: %s", query, resp.StatusCode, resp.Body)
		}
		var body map[string]any
		resp.JSON(t, &body)
		return body
	}

	for _, query := range []string{"dir=b&limit=5", "dir=b&limit=1", "limit=5"} {
		page := read(query)
		chunk, _ := page["chunk"].([]any)
		if len(chunk) == 0 {
			t.Errorf("GET /messages?%s returned an empty chunk, want the end of the room: %v", query, page)
			continue
		}
		newest := chunk[len(chunk)-1].(map[string]any)
		body, _ := newest["content"].(map[string]any)["body"].(string)
		if body != "last" {
			t.Errorf("GET /messages?%s ended at %q, want the newest message", query, body)
		}
	}
}
