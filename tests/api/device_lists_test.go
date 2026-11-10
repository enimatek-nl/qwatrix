package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
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
