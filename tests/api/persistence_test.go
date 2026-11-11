package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// AC-7 says a homeserver that loses its users' messages when the process restarts has failed, and
// until these tests existed nothing in this suite checked it: every other test asked a *running*
// server a question, and a running server can always answer from memory.
//
// What these tests draw apart is state that exists only in the process and state that exists on
// disk. The two look identical right up until the moment they are not.

// TestARestartKeepsTheRoomAndItsHistory asserts that rooms, state, and events come back, and that
// a new event still fits onto the graph the previous process built.
func TestARestartKeepsTheRoomAndItsHistory(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "private_chat", "name": "keep me",
	})
	alice.MustInvite(t, server, roomID, bob.UserID)
	bob.MustJoinRoom(t, server, roomID)
	for _, body := range []string{"first", "second", "third"} {
		alice.MustSendText(t, server, roomID, body)
	}

	restarted := server.Restart(t)

	// The membership list is room state; it has to be there, not empty.
	members := roomMembers(t, alice, restarted, roomID)
	if !members[bob.UserID] {
		t.Fatalf("after a restart the room forgot its members: %v", members)
	}

	// The events sent before the restart are still the room's history, not a gap.
	before := messagesOf(t, alice, restarted, roomID)
	if got := countBodies(messageBodies(before), "first") +
		countBodies(messageBodies(before), "second") +
		countBodies(messageBodies(before), "third"); got != 3 {
		t.Fatalf("after a restart the room held %v, wanted the three messages sent before it: %v",
			messageBodies(before), before)
	}

	// A room that came back is a room the new process can still write to.
	if eventID := alice.MustSendText(t, restarted, roomID, "after the restart"); eventID == "" {
		t.Fatalf("sending an event after the restart returned no event id")
	}
	alice.MustTimelineHas(t, restarted, roomID, "m.room.message", "after the restart")
}

// TestARestartKeepsSessionsAndDevices asserts that restarting does not log anybody out. An access
// token is a name for a session the homeserver issued; if it stops working when the process
// restarts, every client on every device has to be re-authenticated by a human.
func TestARestartKeepsSessionsAndDevices(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	before := alice.MustGet(t, server, "/_matrix/client/v3/account/whoami").MustStatus(t, 200)

	restarted := server.Restart(t)
	after := restarted.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken)
	if after.StatusCode != http.StatusOK {
		t.Fatalf("a token issued before the restart was refused after it (%d): %s",
			after.StatusCode, after.Body)
	}

	var who, again struct {
		UserID   string `json:"user_id"`
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(before.Body, &who); err != nil {
		t.Fatalf("whoami before the restart: %v", err)
	}
	if err := json.Unmarshal(after.Body, &again); err != nil {
		t.Fatalf("whoami after the restart: %v", err)
	}
	if again != who {
		t.Fatalf("the restart changed the session: was %+v, now %+v", who, again)
	}

	// The device that token belongs to is still a device the user has.
	devices := alice.MustGet(t, restarted, "/_matrix/client/v3/devices").MustStatus(t, 200)
	if !strings.Contains(string(devices.Body), who.DeviceID) {
		t.Fatalf("the device %q is not listed after the restart: %s", who.DeviceID, devices.Body)
	}

	// Logging in again still works, and the account is the same account.
	reloged := restarted.Login(t, "alice", "pw-alice")
	if reloged.UserID != who.UserID {
		t.Fatalf("the password now belongs to %q, was %q", reloged.UserID, who.UserID)
	}
	// A wrong password is still a wrong password: a restart must not loosen authentication.
	restarted.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
		"type": "m.login.password", "identifier": map[string]any{"type": "m.id.user", "user": "alice"},
		"password": "not the password",
	}).MustStatus(t, 403)
}

// TestARestartKeepsSyncProgress is the sync-token half of AC-7. A client that was mid-conversation
// must not have to ask for the world again, and the delta it receives afterwards has to continue
// from where the old process left off.
func TestARestartKeepsSyncProgress(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	alice.MustSendText(t, server, roomID, "before the restart")

	// Catch up so the token genuinely names a position mid-conversation, rather than the moment
	// before the room existed.
	alice.SyncUntil(t, server, nil, func(res harness.SyncResponse) bool {
		return timelineHasBody(t, res, roomID, "before the restart")
	})
	midway := alice.MustSync(t, server, url.Values{})

	restarted := server.Restart(t)
	if eventID := alice.MustSendText(t, restarted, roomID, "after the restart"); eventID == "" {
		t.Fatalf("sending after the restart failed")
	}

	// Syncing from the position the old process handed out must show what happened next rather than
	// nothing at all, which is what a token the new process silently ignored would look like.
	reached := alice.SyncUntil(t, restarted, &midway.NextBatch, func(res harness.SyncResponse) bool {
		return timelineHasBody(t, res, roomID, "after the restart")
	})
	if !timelineHasBody(t, reached, roomID, "after the restart") {
		t.Fatalf("syncing from the pre-restart token %q never showed the post-restart message\n%s",
			midway.NextBatch, restarted.Logs())
	}

	// And a client syncing from the beginning still sees the room: the restart did not turn it into
	// history nobody can reach.
	fromScratch := alice.SyncUntil(t, restarted, nil, func(res harness.SyncResponse) bool {
		return timelineHasBody(t, res, roomID, "after the restart")
	})
	if !timelineHasBody(t, fromScratch, roomID, "after the restart") {
		t.Fatalf("a fresh sync after the restart does not show the room")
	}
}

// TestARestartKeepsClientState asserts that what a client configured survives: account data, a
// saved filter, read markers, and a push-rule change. Each is a promise that a settings screen does
// not have to be filled in again after a server upgrade.
func TestARestartKeepsClientState(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	eventID := alice.MustSendText(t, server, roomID, "read me")
	// Account data is addressed by full user ID; a bare localpart is not an identifier.
	accountData := "/_matrix/client/v3/user/" + url.PathEscape(alice.UserID) + "/account_data/io.qwatrix.test"
	alice.MustPutJSON(t, server, accountData, map[string]any{"kept": true})
	markRead(t, server, alice, roomID, eventID, "")
	markerPath := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/account_data/m.fully_read"
	alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/read_markers",
		map[string]any{"m.fully_read": eventID, "m.read": eventID}).MustStatus(t, 200)
	filterID := createFilter(t, server, alice, map[string]any{
		"room": map[string]any{"timeline": map[string]any{"limit": 3}},
	})
	// A sender rule, because its ID is self-contained: content rules need a pattern, and this test
	// is about surviving a restart, not about the rule language.
	alice.MustPutJSON(t, server,
		"/_matrix/client/v3/pushrules/global/sender/"+url.QueryEscape(alice.UserID),
		map[string]any{"actions": []any{"dont_notify"}})

	// The facts have to exist before the restart, or the checks after it would prove nothing at all:
	// a fact that was never stored also "survives" nothing.
	if body := jsonBodyOf(t, alice.MustGet(t, server, markerPath)); body["event_id"] != eventID {
		t.Fatalf("the fixture stored no read marker, so the restart below could not have lost one: %v", body)
	}

	restarted := server.Restart(t)
	data := alice.MustGet(t, restarted, accountData).MustStatus(t, 200)
	if !strings.Contains(string(data.Body), "kept") {
		t.Fatalf("account data did not survive the restart: %s", data.Body)
	}

	// The saved filter still exists, and the server still honours it: asking for a three-event
	// timeline must not be answered with a filter the new process has never heard of.
	saved := alice.MustGet(t, restarted,
		"/_matrix/client/v3/user/"+url.PathEscape(alice.UserID)+"/filter/"+filterID).
		MustStatus(t, 200)
	if !strings.Contains(string(saved.Body), "timeline") {
		t.Fatalf("the filter saved before the restart is unrecognisable after it: %s", saved.Body)
	}

	// Read state is a per-user fact clients spend a great deal of code not having to recompute. The
	// fully-read marker is kept as the room's account data, so that is where a restart has to have
	// kept it.
	markers := jsonBodyOf(t, alice.MustGet(t, restarted, markerPath).MustStatus(t, 200))
	if markers["event_id"] != eventID {
		t.Fatalf("the read marker moved across the restart to %v, want %s", markers["event_id"], eventID)
	}

	// And the read receipt survived: a room the user finished reading must not come back unread,
	// because that is a badge a client cannot be told to put away.
	after := alice.MustSync(t, restarted, url.Values{"timeout": []string{"0"}})
	if unread := after.Rooms.Join[roomID].UnreadNotifications; unread != nil && unread.NotificationCount != 0 {
		t.Fatalf("the room came back with %d unread messages after the restart, want 0: %+v",
			unread.NotificationCount, unread)
	}

	// Push rules are regenerated from defaults at startup, so what has to survive is the change.
	rules := alice.MustGet(t, restarted, "/_matrix/client/v3/pushrules/").MustStatus(t, 200)
	if !strings.Contains(string(rules.Body), alice.UserID) {
		t.Fatalf("the push rule added before the restart is gone after it: %s", rules.Body)
	}
}

// TestARestartKeepsUploadedMedia checks the two halves of media durability separately, because they
// fail separately: the bytes live in the data directory and the metadata lives in the database, and
// a restart can orphan either one alone.
func TestARestartKeepsUploadedMedia(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	body := []byte("qwatrix persistence probe")
	uri := uploadFile(t, alice, server, "probe.txt", "text/plain", body)
	origin, id := splitMXC(t, uri)

	restarted := server.Restart(t)

	downloaded := restarted.Get(t, "/_matrix/media/v3/download/"+url.PathEscape(origin)+"/"+
		url.PathEscape(id), alice.AccessToken)
	if downloaded.StatusCode != http.StatusOK {
		t.Fatalf("the media bytes did not survive the restart (%d): %s",
			downloaded.StatusCode, downloaded.Body)
	}
	if string(downloaded.Body) != string(body) {
		t.Fatalf("the media bytes changed across the restart")
	}

	// A media repository that lost its index would still serve the file above, so the other half of
	// durability is that the server knows the media belongs to this user's room history: an event
	// that refers to it must still be readable.
	roomID := alice.MustCreateRoom(t, restarted, map[string]any{"preset": "private_chat"})
	sent := restarted.PutJSON(t, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/send/m.room.message/"+harness.Nonce("media"), alice.AccessToken, map[string]any{
		"msgtype": "m.file", "url": uri, "body": "probe.txt",
	})
	if sent.StatusCode != 200 {
		t.Fatalf("sending an event that names the media (%d): %s\nSERVER LOG:\n%s",
			sent.StatusCode, sent.Body, restarted.Logs())
	}
	alice.MustTimelineHas(t, restarted, roomID, "m.room.message", "probe.txt")
}

// timelineHasBody reports whether the given room's timeline in a sync response carries an event
// whose "body" is exactly want.
func timelineHasBody(t *testing.T, res harness.SyncResponse, roomID, want string) bool {
	t.Helper()
	if len(res.Rooms.Join) == 0 {
		return false
	}
	for _, event := range harness.DecodeEvents(t, res.Rooms.Join[roomID].Timeline.Events) {
		var content struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(event.Content, &content); err != nil {
			continue
		}
		if content.Body == want {
			return true
		}
	}
	return false
}

// countBodies counts how often want appears among message bodies.
func countBodies(bodies []string, want string) int {
	matched := 0
	for _, body := range bodies {
		if body == want {
			matched++
		}
	}
	return matched
}

// TestRestartStopsTheServerItReplaced pins the property every test in this file depends on. If a
// "restart" left the original process answering, each durability assertion above would be measuring
// the server that never restarted, and the file would be a very confident way to prove nothing.
func TestRestartStopsTheServerItReplaced(t *testing.T) {
	server := harness.NewWithRegistration(t)
	server.Register(t, "alice", "pw-alice", false)

	restarted := server.Restart(t)

	if _, err := server.Client().Get(server.URL + "/_matrix/client/versions"); err == nil {
		t.Fatalf("the replaced homeserver still answers at %s", server.URL)
	}
	// The replacement answers, and answers as the same homeserver.
	restarted.Get(t, "/_matrix/client/versions", "").MustStatus(t, 200)
	if restarted.ServerName != server.ServerName {
		t.Fatalf("the restarted server answers for %q, was %q", restarted.ServerName, server.ServerName)
	}
	if restarted.Dir != server.Dir {
		t.Fatalf("the restarted server opened %q, not the data directory %q it replaced",
			restarted.Dir, server.Dir)
	}
}
