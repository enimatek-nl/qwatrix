package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// uploadDeviceKeys puts a minimal device-key document up for an account, the way a client does when
// it turns encryption on for a new device.
func uploadDeviceKeys(t *testing.T, account *harness.Account, server *harness.Server) {
	t.Helper()
	resp := account.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/upload",
		Body: map[string]any{
			"device_keys": map[string]any{
				"user_id":    account.UserID,
				"device_id":  account.DeviceID,
				"algorithms": []string{"m.olm.v1.curve25519-aes-sha2", "m.megolm.v1.aes-sha2"},
				"keys":       map[string]string{"ed25519:" + account.DeviceID: "dummy-key"},
				"signatures": map[string]map[string]string{
					account.UserID: {"ed25519:" + account.DeviceID: "dummy-signature"},
				},
			},
		},
	})
	resp.MustStatus(t, http.StatusOK)
}

// syncDeviceLists waits for a user ID to appear in a section of device_lists.
func syncDeviceLists(t *testing.T, account *harness.Account, server *harness.Server, since *string, section, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sync := account.MustSync(t, server, url.Values{"timeout": []string{"0"}})
		*since = sync.NextBatch
		var lists struct {
			Changed []string `json:"changed"`
			Left    []string `json:"left"`
		}
		raw, err := json.Marshal(sync.DeviceLists)
		if err != nil {
			t.Fatalf("device_lists is unreadable: %v", err)
		}
		if err := json.Unmarshal(raw, &lists); err != nil {
			t.Fatalf("device_lists is malformed: %v", err)
		}
		candidates := lists.Changed
		if section == "left" {
			candidates = lists.Left
		}
		for _, candidate := range candidates {
			if candidate == want {
				return
			}
		}
	}
	t.Fatalf("%s never appeared in device_lists.%s", want, section)
}

// TestDeviceListChangesReachRoomMates checks that a new device is announced to the people who encrypt
// for its owner.
//
// This is the one part of end-to-end encryption a client cannot quietly get wrong on its own. When a
// person adds a phone, every other client that sends them a message has to learn about the new device;
// if it does not, messages keep going to the old set and the new device shows an empty room. The
// sender sees nothing wrong, the recipient sees nothing arrive, and no error is generated anywhere.
func TestDeviceListChangesReachRoomMates(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "watcher", "pw-watcher", false)
	bob := server.Register(t, "joiner", "pw-joiner", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	// Bob's join itself is a device-list change: alice now has a reason to ask who he is.
	since := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch
	uploadDeviceKeys(t, bob, server)

	// A stranger who shares no room is not told: a device list is only interesting to somebody who
	// might encrypt for that person, and telling everyone is how a server turns one login into a
	// sync for every account it hosts.
	stranger := server.Register(t, "stranger", "pw-stranger", false)
	strangerSince := stranger.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch

	sinceCopy := since
	syncDeviceLists(t, alice, server, &sinceCopy, "changed", bob.UserID)

	// /keys/changes reports the same news between the two tokens, which is what a client that
	// missed the syncs uses to catch up.
	resp := alice.MustGet(t, server, "/_matrix/client/v3/keys/changes?from="+
		url.QueryEscape(since)+"&to="+url.QueryEscape(sinceCopy))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /keys/changes: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}
	var changes struct {
		Changed []string `json:"changed"`
		Left    []string `json:"left"`
	}
	resp.JSON(t, &changes)
	found := false
	for _, userID := range changes.Changed {
		if userID == bob.UserID {
			found = true
		}
	}
	if !found {
		t.Errorf("GET /keys/changes reports %v, want it to include %s", changes.Changed, bob.UserID)
	}

	// The stranger hears nothing about it.
	strangerSync := stranger.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{strangerSince}})
	if len(strangerSync.DeviceLists.Changed) != 0 {
		t.Errorf("an unrelated account was told about %v", strangerSync.DeviceLists.Changed)
	}

	// Leaving is reported as `left`, which tells a client to throw keys away rather than re-query
	// them.
	bob.MustLeaveRoom(t, server, roomID)
	leftSince := sinceCopy
	syncDeviceLists(t, alice, server, &leftSince, "left", bob.UserID)
}

// TestJoiningAnnouncesTheNewMember checks the announcement a room needs most.
//
// When a stranger walks into a room where everybody encrypts, the room's members have to start
// trusting that stranger's keys before they can send them anything. So the join itself is the device
// list change — not merely a later key upload — and the people who care are found by asking which
// rooms this user is in, which is a question that has no answer until the join has been recorded.
func TestJoiningAnnouncesTheNewMember(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "resident", "pw-resident", false)
	bob := server.Register(t, "newcomer", "pw-newcomer", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	// Alice is settled before Bob appears, so her token names a moment without him in it.
	token := alice.MustSync(t, server, url.Values{}).NextBatch
	bob.MustJoinRoom(t, server, roomID)

	since := token
	syncDeviceLists(t, alice, server, &since, "changed", bob.UserID)

	// The same news is answerable between two tokens, for a client that was not watching.
	resp := alice.MustGet(t, server, "/_matrix/client/v3/keys/changes?from="+
		url.QueryEscape(token)+"&to="+url.QueryEscape(since))
	var changes struct {
		Changed []string `json:"changed"`
	}
	resp.JSON(t, &changes)
	if len(changes.Changed) != 1 || changes.Changed[0] != bob.UserID {
		t.Errorf("GET /keys/changes reports %v, want exactly %s", changes.Changed, bob.UserID)
	}
}

// TestARoommateLeavingIsReportedAsLeaving covers the local half of what complement asks of a device
// list: two people on one server, one of them leaves the room they shared, and the other has to be told
// that the keys she holds for that person stopped mattering.
//
// It is the same report as a new device, in the other direction, and it is just as silent when it goes
// missing: a client that never hears `left` keeps encrypting for devices it was told to forget.
func TestARoommateLeavingIsReportedAsLeaving(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "stayeralice", "pw-stayeralice", false)
	bob := server.Register(t, "goerbob", "pw-goerbob", false)
	// Joinable without an invite: this test is about what a roommate's departure reports, not about
	// who may enter.
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	first := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	if first.NextBatch == "" {
		t.Fatal("the first sync returned no position to continue from")
	}

	server.Do(t, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/leave",
		Token:  bob.AccessToken,
	}).MustStatus(t, http.StatusOK)

	deadline := time.Now().Add(10 * time.Second)
	for {
		next := alice.MustSync(t, server, url.Values{"since": []string{first.NextBatch}, "timeout": []string{"0"}})
		if containsString(next.DeviceLists.Left, bob.UserID) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bob leaving the room he shared with alice never appeared in device_lists.left: %+v",
				next.DeviceLists)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, straw := range haystack {
		if straw == needle {
			return true
		}
	}
	return false
}

// TestAFormerRoommatesDeviceUpdatesAreNotReported is the other half of the rule that a device list is
// a promise about somebody you can still reach.
//
// Bob leaves the room he shared with Alice and then turns encryption on for a new phone. Alice's
// server is not owed that update and must not deliver it: it no longer has a reason to hold Bob's
// devices, and reporting them would have it encrypt for a stranger's new device long after the reason
// for trusting it went away. A server that keeps announcing a departed user's devices is also a server
// whose clients see endless "so-and-so added a device" notices for people they no longer talk to.
func TestAFormerRoommatesDeviceUpdatesAreNotReported(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "remainingalice", "pw-remainingalice", false)
	bob := server.Register(t, "departingbob", "pw-departingbob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	uploadDeviceKeys(t, bob, server)
	bob.MustJoinRoom(t, server, roomID)

	since := ""
	if !advanceUntil(t, server, alice, &since, func(next harness.SyncResponse) bool {
		return strings.Contains(string(mustMarshal(t, next.Rooms.Join)), bob.UserID)
	}) {
		t.Fatal("alice never saw bob join the room she created")
	}

	server.Do(t, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/leave",
		Token:  bob.AccessToken,
	}).MustStatus(t, http.StatusOK)

	if !advanceUntil(t, server, alice, &since, func(next harness.SyncResponse) bool {
		return containsString(next.DeviceLists.Left, bob.UserID)
	}) {
		t.Fatalf("alice was never told bob left, in device_lists.left: %+v", since)
	}

	// Bob changes his device keys after leaving. Alice must hear nothing about it.
	uploadDeviceKeys(t, bob, server)
	for attempt := 0; attempt < 3; attempt++ {
		next := alice.MustSync(t, server, joinParams(since))
		if containsString(next.DeviceLists.Changed, bob.UserID) {
			t.Fatalf("alice was told about a device update from %s, who no longer shares a room with her: %+v",
				bob.UserID, next.DeviceLists)
		}
		if next.NextBatch != "" {
			since = next.NextBatch
		}
	}
}

// advanceUntil keeps taking incremental syncs until one satisfies `wanted`, leaving `since` at the
// position just past it.
//
// The checking and the advancing have to be the same call: a sync is consumption, and a helper that
// syncs to look and then syncs again to move on reads every update twice and finds the interesting one
// in neither.
func advanceUntil(t *testing.T, server *harness.Server, account *harness.Account, since *string, wanted func(harness.SyncResponse) bool) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		next := account.MustSync(t, server, joinParams(*since))
		if wanted(next) {
			if next.NextBatch != "" {
				*since = next.NextBatch
			}
			return true
		}
		if next.NextBatch != "" {
			*since = next.NextBatch
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// joinParams is an incremental sync; no position asks for the first one, which is the only difference
// between "what is happening now" and "what happened while I was away".
func joinParams(since string) url.Values {
	params := url.Values{"timeout": []string{"0"}}
	if since != "" {
		params.Set("since", since)
	}
	return params
}
