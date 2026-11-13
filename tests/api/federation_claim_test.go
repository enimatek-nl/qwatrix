package api_test

// Starting an Olm session with a device that lives on another homeserver, from the client's side.
//
// This is the half that makes the served endpoints real rather than merely available. A server that
// *answers* a peer's key claim lets a conversation in through the front door; a server whose own users
// cannot *ask* is a server whose clients can receive encrypted messages and never send one. Between two
// qwatrix homeservers both halves now have to work, and a test that checked only one would have let the
// other stay missing — which is exactly how this one came to be missing: the endpoints were built, wired
// and documented from the receiving side, and nobody had asked the question from the sending side.
//
// The assertion that matters is not that the client received a key. It is that the key came from the
// server holding it: a claim that quietly returned an invented key — or nothing at all, which a client
// reads as "this device is unreachable" and acts on by giving up — would look like success here and fail
// in the only way that matters, which is a session that never decrypts anything.

import (
	"encoding/json"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

func TestAClientStartsASessionWithADeviceOnAnotherServer(t *testing.T) {
	// hosted has the device being talked to; reader has the client that wants to start the session.
	hosted := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hosted.FederationServer(t)
	bob := hosted.Register(t, "bob", "pw-bob", false)
	withUploadedKeys(t, hosted, bob, 2)

	reader := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	reader.FederationServer(t)
	alice := reader.Register(t, "alice", "pw-alice", false)

	if held := oneTimeKeysHeld(t, hosted, bob); held != 2 {
		t.Fatalf("bob's device holds %d one-time keys before the claim, want 2", held)
	}

	claim := func(device map[string]string) map[string]any {
		t.Helper()
		response := alice.MustPostJSON(t, reader, "/_matrix/client/v3/keys/claim", map[string]any{
			"one_time_keys": map[string]any{bob.UserID: device},
		}).MustStatus(t, 200)
		var answer map[string]any
		response.JSON(t, &answer)
		return answer
	}

	answer := claim(map[string]string{bob.DeviceID: "signed_curve25519"})
	keys, found := entry(t, answer, "one_time_keys", bob.UserID, bob.DeviceID)
	if !found || len(keys) != 1 {
		t.Fatalf("alice's claim for bob's remote device took %v, want exactly one key", answer)
	}
	// The key has to be one of bob's, in the shape his server published it: an algorithm-prefixed ID and
	// a signed document beneath it. A client wraps a session to the *content*, so a key that arrives in
	// some intermediate shape is a session that fails on the far side.
	for keyID, raw := range keys {
		var key struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("the claimed key cannot be re-read: %v", err)
		}
		if err := json.Unmarshal(encoded, &key); err != nil {
			t.Fatalf("the claimed key %q is not an object: %v\n%s", keyID, err, encoded)
		}
		if key.Type != "signed_curve25519" || key.Key == "" {
			t.Fatalf("claimed %q as %v, want a usable signed_curve25519 key", keyID, key)
		}
	}

	// And the key left the server that held it. Without this, a server could answer its own client's
	// claim for a remote device out of its own stock — or out of nothing — and every check above would
	// still pass.
	if held := oneTimeKeysHeld(t, hosted, bob); held != 1 {
		t.Fatalf("bob's device holds %d one-time keys after alice's claim, want one spent", held)
	}
}

func TestAClientsClaimForAnUnreachableServerIsSaidOutLoud(t *testing.T) {
	// A peer that cannot be reached is a fact the client has to be told, because both alternatives are
	// worse in opposite directions: answering "no keys" is indistinguishable from a device that has run
	// out, and would make a client stop trying to reach a perfectly reachable person; failing the whole
	// request would let one dead homeserver stop a client from starting sessions with anybody else in the
	// room.
	reader := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	reader.FederationServer(t)
	alice := reader.Register(t, "alice", "pw-alice", false)

	// A server name that is nobody's: nothing is listening on that port, so no connection can be made.
	const ghost = "@bob:127.0.0.1:1"
	response := alice.MustPostJSON(t, reader, "/_matrix/client/v3/keys/claim", map[string]any{
		"one_time_keys": map[string]any{ghost: map[string]string{"DEVICE": "signed_curve25519"}},
	}).MustStatus(t, 200)
	var answer map[string]any
	response.JSON(t, &answer)

	if keys, found := entry(t, answer, "one_time_keys", ghost); found && len(keys) > 0 {
		t.Fatalf("this server invented a one-time key for a device it cannot have reached: %v", keys)
	}
	if _, told := entry(t, answer, "failures", "127.0.0.1:1"); !told {
		t.Fatalf("an unreachable server was passed over in silence; a client deserves to know why a "+
			"session did not start:\n%v", answer)
	}
}
