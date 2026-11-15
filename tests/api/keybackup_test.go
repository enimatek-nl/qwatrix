package api_test

// Server-side room key backup, tested as the thing people actually use it for: losing a device.
//
// The scenario this feature exists for is unrepeatable in a test — a phone dies, a laptop is stolen —
// so these tests aim at the properties that make the recovery possible at all:
//
//   - a backup that does not exist yet answers "no backup", because that answer is what makes a
//     client offer to create one, and a client that cannot tell "empty" from "missing" shows a person
//     a recovery key they never had;
//   - what goes into a backup comes out unchanged, including fields this server has never heard of,
//     because a session this server re-writes on the way through is a session nobody can decrypt;
//   - two devices uploading the same session leave the better copy rather than the later one, since
//     "later" is decided by which phone had a network first;
//   - keys can only be written to the current backup, so a client asleep since last month cannot
//     encrypt a session to an old recovery key and file it where the current one will never look;
//   - a password change does not destroy a backup. That is the deliberate opposite of what happens to
//     a pusher, and they are opposite because a pusher is a device's permission to be told things and
//     a backup is what a person needs after they have lost the device.
//
// The payloads are encrypted to a key this server never sees, so these tests also pin what the server
// does *not* know: it cannot tell a genuine Megolm session from the word "hello", and it must not
// dress that up in the shapes it returns.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// createBackup creates a backup for an account and returns its version, as a client would.
func createBackup(t *testing.T, server *harness.Server, account *harness.Account) string {
	t.Helper()
	resp := account.MustPostJSON(t, server, "/_matrix/client/v3/room_keys/version", map[string]any{
		"algorithm": "m.megolm_backup.v1.curve25519-aes-sha2",
		"auth_data": map[string]any{
			"public_key": "aPublicKeyThisServerOnlyStores",
			"signatures": map[string]any{"ed25519:DEVICE": "aSignature"},
		},
	})
	resp.MustStatus(t, 200)
	var body struct {
		Version string `json:"version"`
	}
	resp.JSON(t, &body)
	if body.Version == "" {
		t.Fatalf("POST /room_keys/version answered no version: %s", resp.Body)
	}
	return body.Version
}

// backupVersion reads GET /room_keys/version, and reports whether one exists.
func backupVersion(t *testing.T, server *harness.Server, account *harness.Account) (map[string]any, bool) {
	t.Helper()
	resp := account.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/room_keys/version",
		Token:  account.AccessToken,
	})
	if resp.StatusCode == http.StatusNotFound {
		// A 404 is an answer, not a failure: it is how a client learns to offer making one.
		return nil, false
	}
	resp.MustStatus(t, 200)
	var info map[string]any
	resp.JSON(t, &info)
	return info, true
}

// putKey stores one session, and answers the status and body of that request.
func putKey(t *testing.T, server *harness.Server, account *harness.Account,
	version, roomID, sessionID string, session map[string]any, headers map[string]string) *harness.Response {
	t.Helper()
	return account.Do(t, server, harness.Request{
		Method:  http.MethodPut,
		Path:    "/_matrix/client/v3/room_keys/keys/" + harness.PathEscape(roomID) + "/" + sessionID + "?version=" + version,
		Token:   account.AccessToken,
		Body:    session,
		Headers: headers,
	})
}

// getKey reads one session back.
func getKey(t *testing.T, server *harness.Server, account *harness.Account,
	version, roomID, sessionID string) *harness.Response {
	t.Helper()
	return account.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/room_keys/keys/" + harness.PathEscape(roomID) + "/" + sessionID + "?version=" + version,
		Token:  account.AccessToken,
	})
}

// sessionOf is a Megolm session as a client uploads it. Only session_data is meaningful to anyone
// but this server, and it is opaque to this server entirely.
func sessionOf(data string, index, forwarded int, verified bool) map[string]any {
	return map[string]any{
		"first_message_index": index,
		"forwarded_count":     forwarded,
		"is_verified":         verified,
		"session_data": map[string]any{
			"ephemeral":  "an encrypted blob this server must not touch",
			"ciphertext": data,
		},
		"algorithm": "m.megolm.v1.aes-sha2",
	}
}

// TestABackupIsCreatedAndReadBack is the lifecycle a client walks on first run.
//
// The interesting part is the first assertion: before a backup exists, the endpoint says so with a
// 404 rather than inventing an empty one. That is the difference between Element offering "encrypt
// your keys" and showing a recovery key for a backup nobody made.
func TestABackupIsCreatedAndReadBack(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)

	if _, found := backupVersion(t, server, alice); found {
		t.Fatal("a brand new account already has a key backup")
	}

	version := createBackup(t, server, alice)
	info, found := backupVersion(t, server, alice)
	if !found {
		t.Fatal("the backup that was just created is not there")
	}
	if info["version"] != version {
		t.Errorf("GET /room_keys/version reports %v, want the version that was handed out (%s)",
			info["version"], version)
	}
	if info["algorithm"] != "m.megolm_backup.v1.curve25519-aes-sha2" {
		t.Errorf("algorithm: got %v, want the one that was submitted", info["algorithm"])
	}
	authData, _ := info["auth_data"].(map[string]any)
	if authData == nil || authData["public_key"] != "aPublicKeyThisServerOnlyStores" {
		t.Fatalf("auth_data did not come back as submitted: %v", info["auth_data"])
	}
	if info["count"] != float64(0) {
		t.Errorf("count: got %v, want 0 — a client uses this to tell an empty backup from none",
			info["count"])
	}
	if etag, _ := info["etag"].(string); etag == "" {
		t.Errorf("etag: got %v, want a string a client can guard a write with", info["etag"])
	}

	// A second backup is a second backup: the old one stays readable, because the sessions in it are
	// the only copy of those conversations and they were encrypted to a key the new one does not have.
	second := createBackup(t, server, alice)
	if second == version {
		t.Fatalf("two backups were both handed version %s, so a client cannot tell them apart", second)
	}
	if info, _ := backupVersion(t, server, alice); info["version"] != second {
		t.Errorf("the current version is %v, want the newest one (%s)", info["version"], second)
	}
	resp := alice.MustGet(t, server, "/_matrix/client/v3/room_keys/version/"+version)
	resp.MustStatus(t, 200)
}

// TestASessionComesBackUnchanged is the privacy-and-correctness property of the whole feature.
//
// This server cannot read a session, so its side of the bargain is to store the object and hand back
// the same bytes. The field named `a_field_this_server_has_never_seen` is the actual test: a server
// that decodes a session into its own struct and re-serialises it passes every other assertion in this
// file and still loses a person's keys when the format moves on without it.
func TestASessionComesBackUnchanged(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	version := createBackup(t, server, alice)

	roomID := "!roomone:test.local"
	session := sessionOf("first", 3, 1, false)
	session["a_field_this_server_has_never_seen"] = map[string]any{"kept": true}

	resp := putKey(t, server, alice, version, roomID, "session-a", session, nil)
	resp.MustStatus(t, 200)
	var written struct {
		Count int64 `json:"count"`
		ETag  any   `json:"etag"`
	}
	resp.JSON(t, &written)
	if written.Count != 1 {
		t.Errorf("count: got %d, want 1 session stored", written.Count)
	}

	resp = getKey(t, server, alice, version, roomID, "session-a")
	resp.MustStatus(t, 200)
	var stored map[string]any
	resp.JSON(t, &stored)
	if stored["first_message_index"] != float64(3) || stored["forwarded_count"] != float64(1) {
		t.Errorf("the session's claims came back altered: %v", stored)
	}
	if stored["is_verified"] != false {
		t.Errorf("is_verified: got %v, want the false that was sent", stored["is_verified"])
	}
	if _, ok := stored["session_data"].(map[string]any); !ok {
		t.Fatalf("session_data came back as something other than the object sent: %v", stored["session_data"])
	}
	kept, _ := stored["a_field_this_server_has_never_seen"].(map[string]any)
	if kept == nil || kept["kept"] != true {
		t.Errorf("a field this server does not model was dropped: %v", stored)
	}

	// The two wider views address the same session, so that a client restoring a whole backup can
	// walk it without asking after every room it has ever been in.
	resp = alice.MustGet(t, server, "/_matrix/client/v3/room_keys/keys/"+harness.PathEscape(roomID)+"?version="+version)
	resp.MustStatus(t, 200)
	var roomView struct {
		Sessions map[string]json.RawMessage `json:"sessions"`
	}
	resp.JSON(t, &roomView)
	if _, ok := roomView.Sessions["session-a"]; !ok {
		t.Errorf("the room view holds %v, want the session", roomView.Sessions)
	}

	resp = alice.MustGet(t, server, "/_matrix/client/v3/room_keys/keys?version="+version)
	resp.MustStatus(t, 200)
	var allView struct {
		Rooms map[string]struct {
			Sessions map[string]json.RawMessage `json:"sessions"`
		} `json:"rooms"`
	}
	resp.JSON(t, &allView)
	if _, ok := allView.Rooms[roomID]; !ok {
		t.Errorf("the whole-backup view holds %d rooms, want the one that was stored", len(allView.Rooms))
	}

	// A session that is not there is a 404, while a room with nothing in it is an empty answer: the
	// client asking for one session needs to know this one is missing, and the client walking rooms
	// has no way to know which rooms were backed up.
	resp = getKey(t, server, alice, version, roomID, "session-that-was-never-stored")
	resp.MustStatus(t, 404)
	resp = alice.MustGet(t, server, "/_matrix/client/v3/room_keys/keys/"+
		harness.PathEscape("!empty-room:test.local")+"?version="+version)
	resp.MustStatus(t, 200)
}

// TestTheBetterCopyOfASessionWins is the rule that makes two devices one backup.
//
// Every client of an account uploads the sessions it holds, so the same session arrives several times
// from several devices, each with whatever it happened to know when it exported the key. Nothing about
// the arrival order is meaningful, so the server compares what the clients claim instead: a verified
// copy beats an unverified one, a copy reaching further back into the room beats one starting later,
// and a copy that has been forwarded fewer times is worth more than one that has been shared around.
func TestTheBetterCopyOfASessionWins(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	version := createBackup(t, server, alice)
	roomID := "!compare:test.local"

	for index, testCase := range []struct {
		name        string
		stored      map[string]any
		challenger  map[string]any
		replaceWith map[string]any
	}{
		{
			name:        "a verified copy replaces an unverified one",
			stored:      sessionOf("unverified", 10, 5, false),
			challenger:  sessionOf("verified", 20, 9, true),
			replaceWith: sessionOf("verified", 20, 9, true),
		},
		{
			name:        "a copy reaching further back replaces one starting later",
			stored:      sessionOf("later", 10, 0, false),
			challenger:  sessionOf("earlier", 9, 100, false),
			replaceWith: sessionOf("earlier", 9, 100, false),
		},
		{
			name:        "a less-forwarded copy replaces a shared-around one",
			stored:      sessionOf("shared", 10, 9, false),
			challenger:  sessionOf("private", 10, 1, false),
			replaceWith: sessionOf("private", 10, 1, false),
		},
		{
			name:        "a worse copy is dropped",
			stored:      sessionOf("good", 10, 1, true),
			challenger:  sessionOf("worse", 11, 2, false),
			replaceWith: sessionOf("good", 10, 1, true),
		},
		{
			// The one case where the specification's prose and the deployed servers disagree,
			// pinned rather than smoothed over. The prose reads as a cascade — is_verified
			// first, then index, then forwarding — which would keep the verified copy. The
			// servers in the field treat the three claims as independent wins, so a copy that
			// decrypts more of the room's history replaces one that decrypts less, verified or
			// not. qwatrix follows the field: a key that reaches further back is strictly more
			// useful, the divergence is invisible to the ecosystem's own conformance tests, and
			// a private second opinion about which of a user's own claims to believe is not
			// something this server can justify — it cannot verify any of the three claims,
			// since the payload is encrypted to the owner.
			name:        "a copy decrypting more history replaces a verified one",
			stored:      sessionOf("verified", 10, 5, true),
			challenger:  sessionOf("unverified", 1, 9, false),
			replaceWith: sessionOf("unverified", 1, 9, false),
		},
		{
			name:        "a copy no better in any claim is dropped",
			stored:      sessionOf("verified", 10, 5, true),
			challenger:  sessionOf("unverified", 11, 6, false),
			replaceWith: sessionOf("verified", 10, 5, true),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sessionID := fmt.Sprintf("session-%d", index)
			putKey(t, server, alice, version, roomID, sessionID, testCase.stored, nil).
				MustStatus(t, 200)
			putKey(t, server, alice, version, roomID, sessionID, testCase.challenger, nil).
				MustStatus(t, 200)
			resp := getKey(t, server, alice, version, roomID, sessionID)
			resp.MustStatus(t, 200)
			var stored map[string]any
			resp.JSON(t, &stored)
			data, _ := stored["session_data"].(map[string]any)
			// The ciphertext is the only way to see which copy survived, since the metadata is
			// what the two copies disagree about.
			want, _ := testCase.replaceWith["session_data"].(map[string]any)["ciphertext"]
			if data["ciphertext"] != want {
				t.Errorf("after the second upload the backup holds %v, want %v", data["ciphertext"], want)
			}
		})
	}
}

// TestKeysGoOnlyToTheCurrentBackup is the rule that keeps a stale client from filing keys somewhere
// nothing will ever look.
//
// A backup exists because its sessions are encrypted under a recovery key. A client still running
// last month's backup would encrypt to last month's key, and a server that accepted the write would
// have stored a session that the current backup cannot decrypt and the old backup no longer reads —
// quietly, and forever. So the write is refused, and the refusal names the version to use instead,
// because a client can act on a number and cannot act on prose.
func TestKeysGoOnlyToTheCurrentBackup(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	first := createBackup(t, server, alice)
	roomID := "!stale:test.local"
	putKey(t, server, alice, first, roomID, "in-first", sessionOf("first backup", 1, 0, false), nil).
		MustStatus(t, 200)

	second := createBackup(t, server, alice)

	resp := putKey(t, server, alice, first, roomID, "late", sessionOf("too late", 2, 0, false), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("writing to an old backup: got %d, want 403 — %s", resp.StatusCode, resp.Body)
	}
	var refusal struct {
		ErrCode        string `json:"errcode"`
		CurrentVersion string `json:"current_version"`
	}
	resp.JSON(t, &refusal)
	if refusal.ErrCode != "M_WRONG_VERSION" {
		t.Errorf("errcode: got %q, want M_WRONG_VERSION", refusal.ErrCode)
	}
	if refusal.CurrentVersion != second {
		t.Errorf("current_version: got %q, want %s — the client needs the number, not the story",
			refusal.CurrentVersion, second)
	}

	// The keys already in the old backup are not punished for their container being out of date:
	// they are still the only copy of those messages.
	getKey(t, server, alice, first, roomID, "in-first").MustStatus(t, 200)
	// And the current backup is writable, which is the whole point of saying so.
	putKey(t, server, alice, second, roomID, "in-second", sessionOf("second backup", 1, 0, false), nil).
		MustStatus(t, 200)

	// A version this server never issued is not a backup, and does not need a special error to say
	// so.
	if resp := putKey(t, server, alice, "9999", roomID, "nowhere", sessionOf("nobody", 1, 0, false), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("writing to a version that never existed: got %d, want 404", resp.StatusCode)
	}
	// A version that is not even a number is the same non-backup, and must not be a 500 either.
	if resp := getKey(t, server, alice, "not-a-version", roomID, "in-first"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("reading a version that is not a number: got %d, want 404", resp.StatusCode)
	}
	// Asking for keys without saying which backup is not a request this server can answer.
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/room_keys/keys",
		Token:  alice.AccessToken,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /room_keys/keys with no version: got %d, want 400", resp.StatusCode)
	}
}

// TestAKeyBackupSurvivesAPasswordChange is the deliberate opposite of a pusher.
//
// A password change revokes other sessions, and a pusher — one session's standing permission to be
// sent notifications — goes with the session that granted it. A backup is the other case entirely: it
// is what a person reaches for when the devices are gone, and the moment they are most likely to change
// their password is the moment after somebody else had their account. Destroying the backup there
// would delete the one thing that makes their history recoverable, on the request that was about
// locking the door.
func TestAKeyBackupSurvivesAPasswordChange(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	version := createBackup(t, server, alice)
	roomID := "!survive:test.local"
	putKey(t, server, alice, version, roomID, "keep-me", sessionOf("still here", 1, 0, true), nil).
		MustStatus(t, 200)

	resp := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/account/password",
		Token:  alice.AccessToken,
		Body: map[string]any{
			"new_password":   "pw-alice-two",
			"logout_devices": true,
			"auth": map[string]any{
				"type":       "m.login.password",
				"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
				"password":   "pw-alice",
			},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("changing the password: %d %s", resp.StatusCode, resp.Body)
	}

	// A new session on the same account is exactly the device that needs the backup, and finds it.
	fresh := server.Login(t, "alice", "pw-alice-two")
	info, found := backupVersion(t, server, fresh)
	if !found {
		t.Fatal("the key backup is gone after a password change; that is the moment it is needed most")
	}
	if info["version"] != version {
		t.Errorf("the surviving backup is version %v, want the one that was made (%s)", info["version"], version)
	}
	if info["count"] != float64(1) {
		t.Errorf("the backup holds %v sessions after the password change, want 1", info["count"])
	}
	getKey(t, server, fresh, version, roomID, "keep-me").MustStatus(t, 200)
}

// TestAWriteAgainstAStaleETagIsRefused covers the case two of a user's own devices race to write.
//
// Both read the backup, both add a session, and whichever writes last would otherwise overwrite what
// the first stored — with no way for either to know. The etag is what turns that into one failure and
// one retry.
func TestAWriteAgainstAStaleETagIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	version := createBackup(t, server, alice)
	roomID := "!race:test.local"

	info, _ := backupVersion(t, server, alice)
	staleETag, _ := info["etag"].(string)

	resp := putKey(t, server, alice, version, roomID, "first", sessionOf("first", 1, 0, false),
		map[string]string{"If-Match": `"` + staleETag + `"`})
	resp.MustStatus(t, 200)

	after, _ := backupVersion(t, server, alice)
	freshETag, _ := after["etag"].(string)
	if freshETag == staleETag {
		t.Fatalf("the etag did not move after a write (%s), so If-Match guards nothing", freshETag)
	}

	resp = putKey(t, server, alice, version, roomID, "second", sessionOf("second", 1, 0, false),
		map[string]string{"If-Match": `"` + staleETag + `"`})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a write guarded by a stale etag: got %d, want 403 — %s", resp.StatusCode, resp.Body)
	}
	getKey(t, server, alice, version, roomID, "second").MustStatus(t, 404)

	// With the etag it now has, the same write goes through.
	if resp = putKey(t, server, alice, version, roomID, "second", sessionOf("second", 1, 0, false),
		map[string]string{"If-Match": `"` + freshETag + `"`}); resp.StatusCode != http.StatusOK {
		t.Errorf("a write guarded by the current etag: got %d, want 200 — %s", resp.StatusCode, resp.Body)
	}
}

// TestAnUnusableSessionIsRefusedAtTheDoor is about failing loudly.
//
// A session with nothing in it would be accepted by a server that only counted fields, arrive in the
// backup as a stored session, and be reported to the user as backed up. That is the one outcome worse
// than refusing: the person believes their history is safe, and discovers otherwise on the one device
// that needs it.
func TestAnUnusableSessionIsRefusedAtTheDoor(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	version := createBackup(t, server, alice)
	roomID := "!empty:test.local"

	resp := putKey(t, server, alice, version, roomID, "no-data", map[string]any{
		"first_message_index": 1,
		"is_verified":         true,
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a session with no session_data: got %d, want 400 — %s", resp.StatusCode, resp.Body)
	}
	getKey(t, server, alice, version, roomID, "no-data").MustStatus(t, 404)

	// Bulk uploads are checked the same way: a client should not be able to smuggle a bad session
	// past a validation that lives on the single-session path.
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   "/_matrix/client/v3/room_keys/keys?version=" + version,
		Token:  alice.AccessToken,
		Body: map[string]any{"rooms": map[string]any{roomID: map[string]any{
			"sessions": map[string]any{
				"good": sessionOf("good", 1, 0, false),
				"bad":  map[string]any{"first_message_index": 1},
			},
		}}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bulk upload holding one session with no data: got %d, want 400 — %s",
			resp.StatusCode, resp.Body)
	}
	// The good session went nowhere either: half an upload is not a backup.
	getKey(t, server, alice, version, roomID, "good").MustStatus(t, 404)
}

// TestADeletedBackupIsGone is the end of a backup's life.
//
// Deleting the current backup takes its keys with it, in the same step. A container without its keys
// is a thing that can be addressed but never used, and keys without a container cannot be addressed
// at all — so a version that failed halfway would be a pile of encrypted blobs with nobody able to
// point at them, take up space, and keep being reported as a backup.
func TestADeletedBackupIsGone(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	version := createBackup(t, server, alice)
	roomID := "!gone:test.local"
	putKey(t, server, alice, version, roomID, "doomed", sessionOf("doomed", 1, 0, false), nil).
		MustStatus(t, 200)

	resp := alice.MustDelete(t, server, "/_matrix/client/v3/room_keys/version/"+version)
	resp.MustStatus(t, 200)
	if _, found := backupVersion(t, server, alice); found {
		t.Error("the backup is still reported after being deleted")
	}
	// And its keys with it: reading them by the version that used to hold them is reading nothing.
	if resp = getKey(t, server, alice, version, roomID, "doomed"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the deleted backup still serves keys: %d", resp.StatusCode)
	}

	// A second deletion is a 404, not an error about something else having gone wrong.
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodDelete,
		Path:   "/_matrix/client/v3/room_keys/version/" + version,
		Token:  alice.AccessToken,
	})
	resp.MustStatus(t, 404)
}

// TestDeletingKeysLeavesTheBackupIs the difference between emptying a box and destroying it.
func TestDeletingKeysLeavesTheBackup(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	version := createBackup(t, server, alice)
	roomID := "!emptied:test.local"
	putKey(t, server, alice, version, roomID, "one", sessionOf("one", 1, 0, false), nil).MustStatus(t, 200)
	putKey(t, server, alice, version, roomID, "two", sessionOf("two", 2, 0, false), nil).MustStatus(t, 200)

	resp := alice.MustDelete(t, server, "/_matrix/client/v3/room_keys/keys/"+
		harness.PathEscape(roomID)+"/one?version="+version)
	resp.MustStatus(t, 200)
	getKey(t, server, alice, version, roomID, "one").MustStatus(t, 404)
	getKey(t, server, alice, version, roomID, "two").MustStatus(t, 200)

	// The box is now empty and still says so, which is what distinguishes "I deleted my keys" from
	// "I never backed up" in the one place a user can see it.
	info, found := backupVersion(t, server, alice)
	if !found {
		t.Fatal("deleting the last key also deleted the backup")
	}
	if info["count"] != float64(1) {
		t.Errorf("count: got %v, want 1 — the session that was not deleted", info["count"])
	}
}
