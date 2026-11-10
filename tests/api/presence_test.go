package api_test

// Presence: setting it, reading it, and being told about it only if you share a room.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

func TestPresenceStatus(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "presencealice", "pw-presencealice", false)
	bob := server.Register(t, "presencebob", "pw-presencebob", false)

	// A user who has never said anything is unavailable rather than unknown: the endpoint
	// asks what somebody's status is, and that is answerable.
	resp := alice.MustGet(t, server, "/_matrix/client/v3/presence/"+url.PathEscape(alice.UserID)+"/status")
	var initial struct {
		Presence string `json:"presence"`
	}
	resp.JSON(t, &initial)
	if initial.Presence != "unavailable" {
		t.Errorf("initial presence = %q, want unavailable", initial.Presence)
	}

	statusPath := "/_matrix/client/v3/presence/" + url.PathEscape(alice.UserID) + "/status"

	body := alice.MustPutJSON(t, server, statusPath, map[string]any{
		"presence":   "online",
		"status_msg": "Writing the homeserver",
	})
	body.MustStatus(t, http.StatusOK)

	resp = alice.MustGet(t, server, statusPath)
	var online struct {
		Presence      string `json:"presence"`
		StatusMsg     string `json:"status_msg"`
		LastActiveAgo *int64 `json:"last_active_ago"`
	}
	resp.JSON(t, &online)
	if online.Presence != "online" || online.StatusMsg != "Writing the homeserver" {
		t.Errorf("presence = %+v, want online with the status message", online)
	}
	if online.LastActiveAgo == nil {
		t.Error("last_active_ago is missing; clients render 'last seen' from it")
	}

	// An unknown presence value is a parameter error, not a silent typo.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   statusPath,
		Body:   map[string]any{"presence": "busy"},
	}).MustError(t, http.StatusBadRequest, "M_INVALID_PARAM")

	// Somebody else may not put words in your mouth.
	bob.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   "/_matrix/client/v3/presence/" + url.PathEscape(alice.UserID) + "/status",
		Body:   map[string]any{"presence": "offline"},
	}).MustStatus(t, http.StatusForbidden)
}

// TestPresenceReachesRoommatesOnly is the privacy rule of presence: the users who are told
// are the ones who share a room, and nobody else.
func TestPresenceReachesRoommatesOnly(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "presentroomalice", "pw-1", false)
	bob := server.Register(t, "presentroombob", "pw-2", false)
	stranger := server.Register(t, "presentstranger", "pw-3", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	// Both start from a token of their own so each can watch for the other's change.
	bobSync := bob.MustSync(t, server, nil)
	strangerSync := stranger.MustSync(t, server, nil)

	statusPath := "/_matrix/client/v3/presence/" + url.PathEscape(alice.UserID) + "/status"
	alice.MustPutJSON(t, server, statusPath, map[string]any{
		"presence":   "online",
		"status_msg": "In the room",
	})

	assertPresenceVisible := func(t *testing.T, account *harness.Account, since string, want bool) {
		t.Helper()
		// The change has to be read from a sync that started before it, which is what
		// `since` is for.
		sinceToken := since
		var found *presenceEvent
		for attempt := 0; attempt < 5 && found == nil; attempt++ {
			resp := account.MustSync(t, server, url.Values{"since": []string{sinceToken}})
			for _, raw := range resp.Presence.Events {
				var event presenceEvent
				if err := json.Unmarshal(raw, &event); err != nil {
					continue
				}
				if event.Sender == alice.UserID {
					found = &event
					break
				}
			}
			sinceToken = resp.NextBatch
		}
		if want && found == nil {
			t.Fatalf("%s was not told about alice's presence", account.Localpart)
		}
		if !want && found != nil {
			t.Fatalf("%s was told about alice's presence without sharing a room: %+v", account.Localpart, found)
		}
		if found != nil {
			if found.Content.Presence != "online" || found.Content.StatusMsg != "In the room" {
				t.Errorf("presence event content = %+v", found.Content)
			}
			if found.Content.CurrentlyActive != true {
				t.Errorf("currently_active = %v, want true for an online user", found.Content.CurrentlyActive)
			}
		}
	}

	assertPresenceVisible(t, bob, bobSync.NextBatch, true)
	assertPresenceVisible(t, stranger, strangerSync.NextBatch, false)
}

// presenceEvent is one m.presence event from a sync response.
type presenceEvent struct {
	Type    string `json:"type"`
	Sender  string `json:"sender"`
	Content struct {
		Presence        string `json:"presence"`
		StatusMsg       string `json:"status_msg"`
		LastActiveAgo   *int64 `json:"last_active_ago"`
		CurrentlyActive bool   `json:"currently_active"`
	} `json:"content"`
}

// TestSyncSetPresence checks that a sync can carry the heartbeat, which is how a client that
// only ever syncs still shows up as online.
func TestSyncSetPresence(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "setsyncpresence", "pw-setsync", false)

	alice.MustSync(t, server, url.Values{"set_presence": []string{"online"}})

	resp := alice.MustGet(t, server, "/_matrix/client/v3/presence/"+url.PathEscape(alice.UserID)+"/status")
	var body struct {
		Presence string `json:"presence"`
	}
	resp.JSON(t, &body)
	if body.Presence != "online" {
		t.Errorf("presence after sync?set_presence=online = %q, want online", body.Presence)
	}

	// "offline" is the specification's spelling of "do not mark me active", so it must not
	// overwrite the presence the user already declared.
	alice.MustSync(t, server, url.Values{"set_presence": []string{"offline"}})
	resp = alice.MustGet(t, server, "/_matrix/client/v3/presence/"+url.PathEscape(alice.UserID)+"/status")
	var after struct {
		Presence string `json:"presence"`
	}
	resp.JSON(t, &after)
	if after.Presence != "online" {
		t.Errorf("presence after sync?set_presence=offline = %q, want it left at online", after.Presence)
	}
}

// TestPresenceReachesTheRoomsYouShare checks that a person's presence travels to the people who
// would otherwise be talking to a stranger.
//
// Encryption aside, a room is a group of people who need to know who is around. A client that cannot
// see its room-mates' status renders a room full of grey dots and has no way to tell a member who
// walked away from one who is reading right now. Presence is also the one part of a sync that must
// not churn: marking somebody online once is news, marking them online every time they sync is noise
// that costs a request per person per room.
func TestPresenceReachesTheRoomsYouShare(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "watcher", "pw-watcher", false)
	bob := server.Register(t, "arriving", "pw-arriving", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	// Alice is settled; Bob has an empty view of the server and then walks into her room.
	aliceSync := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	bobSync := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	bob.MustJoinRoom(t, server, roomID)

	// Alice learns Bob is around.
	since := aliceSync.NextBatch
	if sync := alice.SyncUntil(t, server, &since, func(resp harness.SyncResponse) bool {
		return presenceNames(resp, bob.UserID)
	}); !presenceNames(sync, bob.UserID) {
		t.Fatal("alice never heard of bob's presence")
	}

	// Bob learns the same about Alice, who was here first — from a token that predates his
	// joining, which is the moment he became entitled to know her.
	bobSince := bobSync.NextBatch
	if sync := bob.SyncUntil(t, server, &bobSince, func(resp harness.SyncResponse) bool {
		return presenceNames(resp, alice.UserID)
	}); !presenceNames(sync, alice.UserID) {
		t.Fatal("bob never heard of alice's presence")
	}

	// Nothing has changed since, so nothing further should be reported about either of them.
	quiet := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{since}})
	if presenceNames(quiet, bob.UserID) {
		t.Error("an unchanged presence was reported again, so /sync churns once per sync")
	}

	// A status the user set themselves is theirs to keep.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   "/_matrix/client/v3/presence/" + harness.PathEscape(alice.UserID) + "/status",
		Body:   map[string]any{"presence": "unavailable", "status_msg": "at lunch"},
	}).MustStatus(t, http.StatusOK)
	alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	resp := alice.MustGet(t, server, "/_matrix/client/v3/presence/"+harness.PathEscape(alice.UserID)+"/status")
	var status struct {
		Presence string `json:"presence"`
	}
	resp.JSON(t, &status)
	if status.Presence != "unavailable" {
		t.Errorf("syncing overwrote a presence the user chose: got %q, want unavailable", status.Presence)
	}
}

// presenceNames reports whether the sync's presence list announces the given user.
func presenceNames(sync harness.SyncResponse, userID string) bool {
	for _, raw := range sync.Presence.Events {
		var event struct {
			Type   string `json:"type"`
			Sender string `json:"sender"`
		}
		if err := json.Unmarshal(raw, &event); err == nil &&
			event.Type == "m.presence" && event.Sender == userID {
			return true
		}
	}
	return false
}

// TestJoiningMakesYouKnownEvenWithoutSyncing checks the one case a presence server cannot delegate
// to the user's own client.
//
// Presence that only ever changes when a person touches their client describes a person who is
// ignoring it. The room needs an answer at the moment they walk in — before their client has asked
// for anything — or the people already inside are shown a stranger whose status will stay unknown
// until that stranger happens to act.
func TestJoiningMakesYouKnownEvenWithoutSyncing(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "regular", "pw-regular", false)
	bob := server.Register(t, "silent", "pw-silent", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	aliceSync := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	// Bob joins and then does nothing at all: no sync, no status, no request of any kind.
	bob.MustJoinRoom(t, server, roomID)

	since := aliceSync.NextBatch
	sync := alice.SyncUntil(t, server, &since, func(resp harness.SyncResponse) bool {
		return presenceNames(resp, bob.UserID)
	})
	if !presenceNames(sync, bob.UserID) {
		t.Fatal("a member who never synced stayed unknown to the room")
	}
}

// TestAPeerIsMeasuredByEveryRoomYouShare checks the case a quiet stranger creates.
//
// A person who has been on the server for a while already has a presence record, stamped long ago.
// When they walk into a room the client is in, nothing about *them* has changed recently — what
// changed is that the client is now entitled to know them. So the question is not "is this person's
// presence new?" but "did any room we share change?", and a reader who shares several rooms with the
// same person has to be asked that question about all of them: answering it from whichever room
// happened to be read first silently loses the news.
func TestAPeerIsMeasuredByEveryRoomYouShare(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "sharedrooms", "pw-sharedrooms", false)
	bob := server.Register(t, "quietstranger", "pw-quietstranger", false)

	// An old room of theirs, where Bob's presence was recorded long ago.
	first := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, first)
	bob.MustSync(t, server, url.Values{"timeout": []string{"0"}})

	// A second room, created later, that Bob joins while Alice waits.
	second := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSendText(t, server, second, "in here")
	token := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch
	bob.MustJoinRoom(t, server, second)

	since := token
	sync := alice.SyncUntil(t, server, &since, func(resp harness.SyncResponse) bool {
		return presenceNames(resp, bob.UserID)
	})
	if !presenceNames(sync, bob.UserID) {
		t.Fatal("alice was never told about the stranger who joined her room")
	}
}
