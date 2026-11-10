package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// deviceKeys builds a device-key document with the shape the protocol requires. The keys are
// not real: this server stores and relays a device key without verifying it, because the
// signature is over the exact bytes and the verifier is the client that has a reason to care.
func deviceKeysFor(userID, deviceID string) map[string]any {
	return map[string]any{
		"user_id":    userID,
		"device_id":  deviceID,
		"algorithms": []string{"m.olm.v1.curve25519-aes-sha2", "m.megolm.v1.aes-sha2"},
		"keys": map[string]string{
			"curve25519:" + deviceID: "EkHGWHinmF1EhRsmETGErH+aGK1E4Fg4Z4X1rEUGlEA",
			"ed25519:" + deviceID:    "Ksh1HbVLdrmBcGwNlBAVfaXZLYeyLhFzDqjL1IKmEt8",
		},
		"signatures": map[string]any{
			userID: map[string]string{"ed25519:" + deviceID: "sig-not-checked-here"},
		},
	}
}

// oneTimeKeySet builds n one-time keys under the algorithm the whole ecosystem uses.
func oneTimeKeysSet(n int) map[string]map[string]any {
	out := map[string]map[string]any{}
	for i := 0; i < n; i++ {
		keyID := fmt.Sprintf("signed_curve25519:%d", i)
		out[keyID] = map[string]any{
			"type":       "signed_curve25519",
			"key":        fmt.Sprintf("AAAA-key-%d", i),
			"signatures": map[string]any{},
		}
	}
	return out
}

func upload(t *testing.T, account *harness.Account, server *harness.Server, body map[string]any) map[string]any {
	t.Helper()
	resp := account.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/upload",
		Body:   body,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /keys/upload: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}
	var out map[string]any
	resp.JSON(t, &out)
	return out
}

// TestUploadAndQueryKeys checks the round trip a client depends on: upload identity and prepaid
// message keys, then have another device find exactly what was published.
func TestUploadAndQueryKeys(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "keyholder", "pw-keyholder", false)
	bob := server.Register(t, "keyseeker", "pw-keyseeker", false)

	published := deviceKeysFor(alice.UserID, alice.DeviceID)
	keys := oneTimeKeysSet(4)
	response := upload(t, alice, server, map[string]any{
		"device_keys":   published,
		"one_time_keys": keys,
	})
	counts, ok := response["one_time_key_counts"].(map[string]any)
	if !ok {
		t.Fatalf("response has no one_time_key_counts: %v", response)
	}
	if got := counts["signed_curve25519"]; got != float64(4) {
		t.Errorf("one_time_key_counts = %v, want signed_curve25519: 4", counts)
	}

	// Re-uploading the same keys is what a client does when it never got the response. The
	// count must not grow: a duplicate one-time key could be spent twice.
	again := upload(t, alice, server, map[string]any{"one_time_keys": keys})
	if counts := again["one_time_key_counts"].(map[string]any); counts["signed_curve25519"] != float64(4) {
		t.Errorf("re-uploading the same keys counted %v, want 4", counts["signed_curve25519"])
	}

	// A query for all of alice's devices returns her document unchanged — the signature is
	// over these exact bytes.
	query := bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/query",
		Body:   map[string]any{"device_keys": map[string]any{alice.UserID: []string{}}},
	})
	if query.StatusCode != http.StatusOK {
		t.Fatalf("query status %d\n%s\n=== server errors ===\n%s",
			query.StatusCode, query.Body, lastKeyError(t, server))
	}
	var found struct {
		DeviceKeys map[string]map[string]json.RawMessage `json:"device_keys"`
	}
	query.JSON(t, &found)
	aliceDevices, ok := found.DeviceKeys[alice.UserID]
	if !ok {
		t.Fatalf("the query lost alice: %v", found.DeviceKeys)
	}
	raw, ok := aliceDevices[alice.DeviceID]
	if !ok {
		t.Fatalf("the query lost alice's device %q: %v", alice.DeviceID, aliceDevices)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("the stored device key is not an object: %v", err)
	}
	for _, key := range []string{"algorithms", "device_id", "keys", "signatures", "user_id"} {
		if _, ok := stored[key]; !ok {
			t.Errorf("the round-tripped device key lost %q: %s", key, raw)
		}
	}

	// A user with no keys still gets an entry: an empty answer to a question is different
	// from not answering it.
	quiet := bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/query",
		Body:   map[string]any{"device_keys": map[string]any{bob.UserID: []string{}}},
	})
	quiet.MustStatus(t, http.StatusOK)
	var quietBody struct {
		DeviceKeys map[string]map[string]json.RawMessage `json:"device_keys"`
	}
	quiet.JSON(t, &quietBody)
	entries, ok := quietBody.DeviceKeys[bob.UserID]
	if !ok {
		t.Fatalf("a keyless user was omitted from the response: %v", quietBody.DeviceKeys)
	}
	if len(entries) != 0 {
		t.Errorf("a keyless user was given keys: %v", entries)
	}
}

// TestKeyEndpointsRejectBadRequests checks the rejections a client cannot be allowed to make
// silently.
func TestKeyEndpointsRejectBadRequests(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "keymistaker", "pw-keymistaker", false)
	bob := server.Register(t, "keyvictim", "pw-keyvictim", false)

	// A device key without its signature verifies for nobody; a client that uploaded one
	// would wonder forever why it was never queried.
	resp := bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/upload",
		Body: map[string]any{
			"device_keys": map[string]any{"user_id": bob.UserID, "device_id": bob.DeviceID},
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("incomplete device keys: got %d, want 400", resp.StatusCode)
	}
	var failure struct {
		Code string `json:"errcode"`
	}
	resp.JSON(t, &failure)
	if failure.Code != "M_BAD_JSON" {
		t.Errorf("errcode = %q, want M_BAD_JSON", failure.Code)
	}

	// Keys claiming to belong to another user would let any account mint identity for
	// somebody else's device.
	resp = bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/upload",
		Body: map[string]any{
			"device_keys": deviceKeysFor(alice.UserID, alice.DeviceID),
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("keys for another user: got %d, want 400", resp.StatusCode)
	}

	// A query whose device list is an object rather than a list decodes into nothing usable;
	// answering "no keys" would tell the client the user's devices had vanished.
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/query",
		Body: map[string]any{
			"device_keys": map[string]any{bob.UserID: map[string]bool{"device_id1": true}},
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("device list as an object: got %d, want 400", resp.StatusCode)
	}
	resp.JSON(t, &failure)
	if failure.Code != "M_BAD_JSON" {
		t.Errorf("errcode = %q, want M_BAD_JSON", failure.Code)
	}
}

// TestClaimOneTimeKeysChecksTheyAreSpentOnceAndInOrder checks the two properties that make a
// one-time key one-time: it leaves the pile when claimed, and a pile claimed twice hands out the
// keys in the order they were uploaded.
func TestClaimOneTimeKeysChecksTheyAreSpentOnceAndInOrder(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "sender", "pw-sender", false)
	bob := server.Register(t, "recipient", "pw-recipient", false)

	upload(t, bob, server, map[string]any{
		"device_keys":   deviceKeysFor(bob.UserID, bob.DeviceID),
		"one_time_keys": map[string]any{"signed_curve25519:first": map[string]any{"key": "one"}},
	})
	// A separate upload, so "first" is genuinely older than "second" in the store.
	upload(t, bob, server, map[string]any{
		"one_time_keys": map[string]any{"signed_curve25519:second": map[string]any{"key": "two"}},
	})

	claim := func() map[string]map[string]map[string]json.RawMessage {
		resp := alice.Do(t, server, harness.Request{
			Method: http.MethodPost,
			Path:   "/_matrix/client/v3/keys/claim",
			Body: map[string]any{
				"one_time_keys": map[string]any{
					bob.UserID: map[string]string{bob.DeviceID: "signed_curve25519"},
				},
			},
		})
		resp.MustStatus(t, http.StatusOK)
		var out struct {
			OneTimeKeys map[string]map[string]map[string]json.RawMessage `json:"one_time_keys"`
		}
		resp.JSON(t, &out)
		return out.OneTimeKeys
	}

	first := claim()
	if got := claimKeyOf(t, first, bob); got != "signed_curve25519:first" {
		t.Errorf("first claim returned %q, want the oldest uploaded key", got)
	}
	second := claim()
	if got := claimKeyOf(t, second, bob); got != "signed_curve25519:second" {
		t.Errorf("second claim returned %q, want the next key", got)
	}
	// Out of keys: the claimant's answer is that the device is absent, not an error, because
	// "this device is offline and has no prepaid keys" is a normal state of the world.
	if empty := claim(); len(empty[bob.UserID]) != 0 {
		t.Errorf("a device with no keys left was claimed anyway: %v", empty)
	}
}

// claimKeyOf returns the single key ID a claim returned.
func claimKeyOf(t *testing.T, claimed map[string]map[string]map[string]json.RawMessage, account *harness.Account) string {
	t.Helper()
	for _, devices := range claimed {
		for _, keys := range devices {
			for keyID := range keys {
				return keyID
			}
		}
	}
	return ""
}

// lastKeyError returns the server's own complaint, which is the difference between a 500 and a
// diagnosis.
func lastKeyError(t *testing.T, server *harness.Server) string {
	t.Helper()
	out := ""
	for _, line := range strings.Split(server.Logs(), "\n") {
		if strings.Contains(line, "level=ERROR") || strings.Contains(line, "error=") {
			out += line + "\n"
		}
	}
	return out
}
