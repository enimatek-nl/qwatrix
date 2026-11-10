package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// ephemeralOfType returns the raw content of the first ephemeral event of a type in a room's
// sync entry, and whether one was present.
func ephemeralOfType(t *testing.T, sync harness.SyncResponse, roomID, eventType string) (map[string]any, bool) {
	t.Helper()
	for _, raw := range sync.Rooms.Join[roomID].Ephemeral.Events {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("ephemeral event is not an object: %v", err)
		}
		if fields["type"] == eventType {
			content, _ := fields["content"].(map[string]any)
			return content, true
		}
	}
	return nil, false
}

// accountDataOfType returns the content of the first room account-data event of a type.
func accountDataOfType(t *testing.T, sync harness.SyncResponse, roomID, eventType string) (map[string]any, bool) {
	t.Helper()
	for _, raw := range sync.Rooms.Join[roomID].AccountData.Events {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("account data event is not an object: %v", err)
		}
		if fields["type"] == eventType {
			content, _ := fields["content"].(map[string]any)
			return content, true
		}
	}
	return nil, false
}

// TestTypingReachesRoommatesAndNobodyElse covers who is told that somebody is composing.
//
// A typing indicator is only ever useful to people watching the same room, so it must reach the
// room's other members and must not be a way for an outsider to watch a room they are not in.
func TestTypingReachesRoommatesAndNobodyElse(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "typer", "pw-typer", false)
	bob := server.Register(t, "typewatcher", "pw-typewatcher", false)
	charlie := server.Register(t, "typeoutsider", "pw-typeoutsider", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/typing/" +
		harness.PathEscape(alice.UserID)

	// Bob's position before Alice types, so what he sees next is only what happened after.
	bobSync := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	_ = bobSync
	bobToken := bobSync.NextBatch

	alice.MustPutJSON(t, server, path, map[string]any{"typing": true, "timeout": 30000}).
		MustStatus(t, http.StatusOK)

	sync := bob.MustSync(t, server, url.Values{"since": []string{bobToken}, "timeout": []string{"0"}})
	content, ok := ephemeralOfType(t, sync, roomID, "m.typing")
	if !ok {
		t.Fatalf("the typing indicator never reached the other member of the room")
	}
	list, _ := content["user_ids"].([]any)
	if len(list) != 1 || list[0] != alice.UserID {
		t.Fatalf("the typing list was %v, want just %s", content["user_ids"], alice.UserID)
	}

	// The room is already named by the section the event appears in. Repeating it would give
	// clients two spellings of one fact, and the spec reserves the field for federation.
	if _, exists := content["room_id"]; exists {
		t.Errorf("the sync-delivered typing event carried a room_id: %v", content)
	}

	// Stopping has to be an event of its own: silence would be indistinguishable from a sync
	// that had not caught up, leaving clients to time their spinners out by guessing.
	alice.MustPutJSON(t, server, path, map[string]any{"typing": false}).MustStatus(t, http.StatusOK)
	stopped := bob.MustSync(t, server, url.Values{"since": []string{bobToken}, "timeout": []string{"0"}})
	content, ok = ephemeralOfType(t, stopped, roomID, "m.typing")
	if !ok {
		t.Fatalf("stopping typing produced no ephemeral event, so a client cannot tell typing ended")
	}
	list, _ = content["user_ids"].([]any)
	if len(list) != 0 {
		t.Errorf("the typing list was still %v after stopping", content["user_ids"])
	}

	// Charlie shares no room with Alice, so he must learn nothing about this room at all.
	charlieSync := charlie.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	if _, joined := charlieSync.Rooms.Join[roomID]; joined {
		t.Fatalf("a non-member received the room's typing activity")
	}
}

// TestTypingIsOnlyFirstPerson checks that a device cannot claim somebody else is typing.
func TestTypingIsOnlyFirstPerson(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "honesttyper", "pw-honesttyper", false)
	bob := server.Register(t, "frameduser", "pw-frameduser", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/typing/" +
		harness.PathEscape(bob.UserID)
	resp := alice.Do(t, server, harness.Request{
		Method: http.MethodPut, Path: path, Body: map[string]any{"typing": true, "timeout": 10000},
	})
	resp.MustStatus(t, http.StatusForbidden)
}

// TestReceiptReachesSync covers read receipts: the endpoint, and the ephemeral event that tells a
// client (and the reader's own other devices) where the reads have reached.
func TestReceiptReachesSync(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "reader", "pw-reader", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	eventID := alice.MustSendText(t, server, roomID, "read me")

	alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/receipt/m.read/"+harness.PathEscape(eventID), map[string]any{}).
		MustStatus(t, http.StatusOK)

	sync := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	content, ok := ephemeralOfType(t, sync, roomID, "m.receipt")
	if !ok {
		t.Fatalf("the read receipt never reached /sync")
	}
	// Content is keyed by event ID then by receipt type, so a client finds its own reads in a
	// busy room without scanning every receipt in it.
	perEvent, _ := content[eventID].(map[string]any)
	if perEvent == nil {
		t.Fatalf("receipt content was not keyed by event ID: %v", content)
	}
	read, _ := perEvent["m.read"].(map[string]any)
	if read == nil {
		t.Fatalf("receipt was not of type m.read: %v", perEvent)
	}
	byUser, _ := read[alice.UserID].(map[string]any)
	if byUser == nil {
		t.Fatalf("receipt did not name the reader: %v", read)
	}
	if _, exists := byUser["ts"]; !exists {
		t.Errorf("receipt has no ts, so a client cannot order two of them: %v", byUser)
	}

	// An unknown receipt type is refused rather than stored where nothing will ever read it.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
			"/receipt/m.nonsense/" + harness.PathEscape(eventID),
		Body: map[string]any{},
	}).MustStatus(t, http.StatusBadRequest)

	// A receipt for an event in another room is the same 404 as a nonexistent event: confirming
	// it exists somewhere else would leak the existence of a room this caller may not see.
	other := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	otherEvent := alice.MustSendText(t, server, other, "elsewhere")
	alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
			"/receipt/m.read/" + harness.PathEscape(otherEvent),
		Body: map[string]any{},
	}).MustStatus(t, http.StatusNotFound)
}

// TestReadMarkersCoverBothPositions checks POST /read_markers moves the reader's bookmark and,
// with it, the receipt clients send alongside it.
func TestReadMarkersCoverBothPositions(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "marker", "pw-marker", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	eventID := alice.MustSendText(t, server, roomID, "finished")

	alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/read_markers",
		map[string]any{"m.fully_read": eventID, "m.read": eventID}).
		MustStatus(t, http.StatusOK)

	sync := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})

	if _, ok := ephemeralOfType(t, sync, roomID, "m.receipt"); !ok {
		t.Errorf("the marker request did not also move the read receipt")
	}
	// The fully-read marker arrives as room account data, not as a receipt: it is the reader's
	// own bookmark and is never shown to anybody else.
	content, ok := accountDataOfType(t, sync, roomID, "m.fully_read")
	if !ok {
		t.Fatalf("the fully-read marker never reached /sync as room account data")
	}
	if content["event_id"] != eventID {
		t.Errorf("the marker pointed at %v, want %s", content["event_id"], eventID)
	}

	// And it survives as ordinary room account data the client can re-read.
	body := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/account_data/m.fully_read"))
	if body["event_id"] != eventID {
		t.Errorf("stored marker was %v", body)
	}

	// A marker naming no position at all is a request to do nothing, which is a client bug.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/read_markers",
		Body:   map[string]any{},
	}).MustStatus(t, http.StatusBadRequest)
}

// TestAccountDataRoundTrip covers the two account-data endpoints.
//
// Account data is a client's private key/value store — `m.direct`, `m.ignored_user_list`, the
// layout a user prefers. It is per-account and per-room, never part of the room's state, and
// never visible to another member, which is what separates it from a state event.
func TestAccountDataRoundTrip(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "dataowner", "pw-dataowner", false)
	bob := server.Register(t, "datapeer", "pw-datapeer", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	globalPath := "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/account_data/com.example.pref"
	roomPath := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/account_data/com.example.pref"

	alice.MustPutJSON(t, server, globalPath, map[string]any{"theme": "dark"}).MustStatus(t, http.StatusOK)
	alice.MustPutJSON(t, server, roomPath, map[string]any{"pinned": []string{"$a"}}).
		MustStatus(t, http.StatusOK)

	global := jsonBodyOf(t, alice.MustGet(t, server, globalPath))
	if global["theme"] != "dark" {
		t.Errorf("global account data came back as %v", global)
	}
	room := jsonBodyOf(t, alice.MustGet(t, server, roomPath))
	if _, ok := room["pinned"].([]any); !ok {
		t.Errorf("room account data came back as %v", room)
	}

	// Overwrite replaces the whole value rather than merging: a client that merged would have no
	// way to remove a key it had previously written.
	overwrite := alice.Do(t, server, harness.Request{Method: http.MethodPut, Path: globalPath,
		Body: map[string]any{"theme": "light"}})
	if overwrite.StatusCode != http.StatusOK {
		t.Fatalf("overwrite failed: %d %s\nSERVER LOG:\n%s", overwrite.StatusCode, string(overwrite.Body), server.Logs())
	}
	replaced := jsonBodyOf(t, alice.MustGet(t, server, globalPath))
	if len(replaced) != 1 || replaced["theme"] != "light" {
		t.Errorf("account data was merged rather than replaced: %v", replaced)
	}

	// A peer in the same room must not be able to read it, even though the room is shared. The
	// answer is a 404 rather than a 403 because the key is namespaced by owner: bob is being
	// shown his own, absent, entry — which is also why the response cannot confirm that alice
	// has one.
	peerRead := bob.Do(t, server, harness.Request{Method: http.MethodGet, Path: roomPath})
	peerRead.MustStatus(t, http.StatusNotFound)
	if strings.Contains(string(peerRead.Body), "pinned") {
		t.Errorf("a peer read another user's room account data: %s", peerRead.Body)
	}

	// Both scopes stay separate: same key, different meaning.
	if _, leaked := jsonBodyOf(t, alice.MustGet(t, server, globalPath))["pinned"]; leaked {
		t.Errorf("room-scoped data leaked into the global key")
	}

	// A key that was never written is a 404, and deleting it is not an error.
	alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/account_data/com.example.missing",
	}).MustStatus(t, http.StatusNotFound)
	alice.MustDelete(t, server, globalPath).MustStatus(t, http.StatusOK)
	alice.Do(t, server, harness.Request{Method: http.MethodGet, Path: globalPath}).
		MustStatus(t, http.StatusNotFound)

	// Global account data reaches the owner through /sync. Written fresh here because the key
	// above was just deleted, and a sync that reported deleted data would be worse than one
	// that reported nothing.
	otherPath := "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/account_data/com.example.synced"
	alice.MustPutJSON(t, server, otherPath, map[string]any{"written": "recently"}).
		MustStatus(t, http.StatusOK)
	sync := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	found := false
	for _, raw := range sync.AccountData.Events {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("account data event is not an object: %v", err)
		}
		if fields["type"] == "com.example.synced" {
			found = true
		}
	}
	if !found {
		t.Errorf("global account data never appeared in the sync response")
	}
}

// TestProfileChangeReachesRoomAsAMemberEvent checks that renaming yourself re-announces you in
// the rooms you are in.
//
// A display name lives in the profile and, redundantly, in every `m.room.member` event, because
// that is how a room names its members without a lookup per event. Matrix has no
// "profile changed" event, so the only way peers learn is a new member event — and they are the
// people looking at the old name.
func TestProfileChangeReachesRoomAsAMemberEvent(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "renamer", "pw-renamer", false)
	bob := server.Register(t, "renamewatcher", "pw-renamewatcher", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	bobToken := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch

	alice.MustPutJSON(t, server, "/_matrix/client/v3/profile/"+
		harness.PathEscape(alice.UserID)+"/displayname",
		map[string]any{"displayname": "New Name"}).MustStatus(t, http.StatusOK)

	// Bob sees a member event carrying the new name — not just a profile endpoint that happens
	// to answer correctly if he thinks to ask.
	sync := bob.MustSync(t, server, url.Values{"since": []string{bobToken}, "timeout": []string{"0"}})
	found := ""
	for _, raw := range sync.Rooms.Join[roomID].Timeline.Events {
		var event struct {
			Type    string `json:"type"`
			Sender  string `json:"sender"`
			Content struct {
				Membership  string `json:"membership"`
				DisplayName string `json:"displayname"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("timeline event is not JSON: %v", err)
		}
		if event.Type == "m.room.member" && event.Sender == alice.UserID &&
			event.Content.Membership == "join" {
			found = event.Content.DisplayName
		}
	}
	if found != "New Name" {
		t.Fatalf("the room never received a member event naming alice %q, saw %q", "New Name", found)
	}

	// A name the user chose for this room alone survives a global rename: otherwise renaming
	// yourself everywhere would silently overwrite the one place you asked not to be.
	alice.MustSetState(t, server, roomID, "m.room.member", alice.UserID, map[string]any{
		"membership": "join", "displayname": "Room Only Name",
	})
	alice.MustPutJSON(t, server, "/_matrix/client/v3/profile/"+
		harness.PathEscape(alice.UserID)+"/displayname",
		map[string]any{"displayname": "Even Newer Name"}).MustStatus(t, http.StatusOK)
	member := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/state/m.room.member/"+harness.PathEscape(alice.UserID)))
	if member["displayname"] != "Room Only Name" {
		t.Errorf("a room-specific display name was overwritten by the global rename: %v", member["displayname"])
	}
}

// TestReceiptsNameTheirThreadOnlyWhenTheyHaveOne checks the difference between a receipt about a
// thread and a receipt about the room.
//
// A threaded read marker says "I have caught up in this conversation"; an unthreaded one says "I have
// read this room". They are different facts, and a client can only tell them apart if the server is
// silent about the thread when there is not one — an empty thread ID is not "no thread", it reads as
// a thread the client has simply never seen.
func TestReceiptsNameTheirThreadOnlyWhenTheyHaveOne(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "reader1", "pw-reader1", false)
	bob := server.Register(t, "reader2", "pw-reader2", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	first := alice.MustSendText(t, server, roomID, "root")
	alice.MustSendText(t, server, roomID, "in thread")

	receipts := func(sync harness.SyncResponse) []map[string]any {
		var out []map[string]any
		for _, raw := range sync.Rooms.Join[roomID].Ephemeral.Events {
			var event struct {
				Type    string                                          `json:"type"`
				Content map[string]map[string]map[string]map[string]any `json:"content"`
			}
			if err := json.Unmarshal(raw, &event); err != nil || event.Type != "m.receipt" {
				continue
			}
			// content is event ID -> receipt type -> user ID -> position.
			for _, byType := range event.Content {
				for _, byUser := range byType {
					for _, position := range byUser {
						out = append(out, position)
					}
				}
			}
		}
		return out
	}

	// An unthreaded receipt.
	bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
			"/receipt/m.read/" + harness.PathEscape(first),
		Body: map[string]any{},
	}).MustStatus(t, http.StatusOK)
	since := ""
	sync := bob.SyncUntil(t, server, &since, func(resp harness.SyncResponse) bool {
		return len(receipts(resp)) > 0
	})
	seen := receipts(sync)
	if len(seen) == 0 {
		t.Fatal("no receipt arrived in /sync")
	}
	for _, entry := range seen {
		if _, ok := entry["thread_id"]; ok {
			t.Errorf("an unthreaded receipt carries a thread_id: %v", entry)
		}
	}

	// And a threaded one, which must name the thread it belongs to.
	threadID := alice.MustSendText(t, server, roomID, "thread root")
	target := alice.MustSendText(t, server, roomID, "thread reply")
	bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
			"/receipt/m.read/" + harness.PathEscape(target),
		Body: map[string]any{"thread_id": threadID},
	}).MustStatus(t, http.StatusOK)
	sync = bob.SyncUntil(t, server, &since, func(resp harness.SyncResponse) bool {
		for _, entry := range receipts(resp) {
			if entry["thread_id"] == threadID {
				return true
			}
		}
		return false
	})
	found := false
	for _, entry := range receipts(sync) {
		if entry["thread_id"] == threadID {
			found = true
		}
	}
	if !found {
		t.Errorf("a threaded receipt did not name its thread in %v", receipts(sync))
	}
}
