package api_test

// A peer's two questions about this server's users' encryption keys, and the one rule that governs both.
//
// An encrypted conversation between two homeservers cannot begin without them. The client on the far
// side needs the device's identity keys — which it can already get by asking this server's
// `/user/devices/{userID}` — and then a one-time key to wrap its first message to. The second is the
// interesting one: handing a key out *spends* it, so a server that answers for users it does not host is
// spending secrets that are not its to spend, and a client starting a session has no way to tell a real
// one-time key from one a passing server invented.
//
// So both endpoints are signed, and both answer for the domain in the user ID and for nobody else.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// peerPosts sends a signed POST to this server's federation listener. The content is set before the
// signature, because the signature covers the content; the library refuses to modify a signed request
// rather than let a caller leave a signature pointing at something else.
func peerPosts(t *testing.T, server *harness.Server, peer *fakePeer, path string, body any) map[string]any {
	t.Helper()

	response := peerRequest(t, server, peer, http.MethodPost, path, body)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a signed key question got %d, want 200", response.StatusCode)
	}
	return jsonOf(t, response.Body)
}

// peerRequest signs one federation request with a body as the peer and returns the response with its body
// unread, for the tests that need to look at a refusal.
func peerRequest(t *testing.T, server *harness.Server, peer *fakePeer, method, path string,
	body any) *http.Response {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the body: %v", err)
	}
	request := fclient.NewFederationRequest(method, peer.serverNo,
		spec.ServerName(server.ServerName), path)
	if err := request.SetContent(json.RawMessage(raw)); err != nil {
		t.Fatalf("setting the request content: %v", err)
	}
	if err := request.Sign(peer.serverNo, peer.keyID, peer.priv); err != nil {
		t.Fatalf("signing the request: %v", err)
	}
	httpRequest, err := request.HTTPRequest()
	if err != nil {
		t.Fatalf("building the HTTP request: %v", err)
	}
	// Only the scheme and host change: the path is what the signature covers.
	httpRequest.URL.Scheme = "https"
	httpRequest.URL.Host = strings.TrimPrefix(server.FederationServer(t).URL, "https://")
	response, err := server.FederationClient(t).Do(httpRequest)
	if err != nil {
		t.Fatalf("sending the federation request: %v", err)
	}
	return response
}

// unsignedPosts sends the same request with no signature at all, which is how a request that did not come
// from the server it claims to come from looks.
func unsignedPosts(t *testing.T, server *harness.Server, path string, body any) int {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the body: %v", err)
	}
	url := server.FederationServer(t).URL + path
	response, err := server.FederationClient(t).Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("sending the unsigned request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

// jsonOf reads a response body as a JSON object.
func jsonOf(t *testing.T, body io.Reader) map[string]any {
	t.Helper()

	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the answer is not a JSON object: %v\n%s", err, raw)
	}
	return out
}

// entry walks a nested answer object without tripping over the characters a Matrix identifier contains,
// which is why these answers are walked as maps rather than through a path expression.
func entry(t *testing.T, answer map[string]any, path ...string) (map[string]any, bool) {
	t.Helper()

	var current any = answer
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[key]
		if !ok {
			return nil, false
		}
	}
	object, ok := current.(map[string]any)
	return object, ok
}

// withUploadedKeys gives a user a device with identity keys and n prepaid message keys, which is the state
// a peer's question is asked about.
func withUploadedKeys(t *testing.T, server *harness.Server, account *harness.Account, oneTimeKeys int) {
	t.Helper()

	body := map[string]any{"device_keys": deviceKeysFor(account.UserID, account.DeviceID)}
	if oneTimeKeys > 0 {
		body["one_time_keys"] = oneTimeKeysSet(oneTimeKeys)
	}
	stored := upload(t, account, server, body)
	if oneTimeKeys > 0 {
		// The upload answers with what the device now holds, which is the only confirmation that the
		// keys were stored rather than merely accepted.
		counts, _ := stored["one_time_key_counts"].(map[string]any)
		if held, _ := counts["signed_curve25519"].(float64); int(held) != oneTimeKeys {
			t.Fatalf("the device reports %v, wanted %d one-time keys", stored["one_time_key_counts"], oneTimeKeys)
		}
	}
}

// keyOf names the single key in a claim answer, whose only key is the point of the assertion.
func keyOf(t *testing.T, keys map[string]any) string {
	t.Helper()

	for keyID := range keys {
		return keyID
	}
	t.Fatalf("a claim answer named no key: %v", keys)
	return ""
}

// oneTimeKeysHeld reads back how many prepaid message keys a device still has, which is the only way to
// tell "nothing was spent" from "something was spent and not shown".
func oneTimeKeysHeld(t *testing.T, server *harness.Server, account *harness.Account) int64 {
	t.Helper()

	status := account.MustGet(t, server, "/_matrix/client/v3/keys/status")
	var counts map[string]int64
	status.JSON(t, &counts)
	return counts["signed_curve25519"]
}

func TestAPeerAsksWhatKeysThisServersUsersHave(t *testing.T) {
	server := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	server.FederationServer(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	withUploadedKeys(t, server, alice, 2)
	peer := newFakePeer(t)

	answer := peerPosts(t, server, peer, "/_matrix/federation/v1/user/keys/query", map[string]any{
		"device_keys": map[string]any{alice.UserID: []string{}},
	})
	documents, found := entry(t, answer, "device_keys", alice.UserID)
	if !found || len(documents) == 0 {
		t.Fatalf("the query returned no device keys for alice:\n%v", answer)
	}
	document, ok := documents[alice.DeviceID].(map[string]any)
	if !ok {
		t.Fatalf("alice's device %s is not in the answer: %v", alice.DeviceID, documents)
	}
	// The document has to be the one she published, signatures and all: the asking server passes it to
	// its own users, who verify it against her device and nothing else.
	if got, _ := document["user_id"].(string); got != alice.UserID {
		t.Fatalf("the key document names %v, want %s", document["user_id"], alice.UserID)
	}
	if _, signed := document["signatures"]; !signed {
		t.Fatalf("the key document carries no signatures, so it proves nothing:\n%v", document)
	}

	// Somebody else's user is not this server's to answer for. Inventing an empty answer for them would
	// be a lie with consequences: the asking client would read "no devices" and give up on an encrypted
	// conversation that is perfectly possible. Silence about them is the honest answer.
	outsider := "@outsider:" + string(peer.serverNo)
	mixed := peerPosts(t, server, peer, "/_matrix/federation/v1/user/keys/query", map[string]any{
		"device_keys": map[string]any{alice.UserID: []string{}, outsider: []string{}},
	})
	if spoke, found := entry(t, mixed, "device_keys", outsider); found {
		t.Fatalf("the answer spoke about a user this server does not host: %v", spoke)
	}
	if _, found := entry(t, mixed, "device_keys", alice.UserID); !found {
		t.Fatalf("one name this server does not host cost alice her answer:\n%v", mixed)
	}

	if status := unsignedPosts(t, server, "/_matrix/federation/v1/user/keys/query",
		map[string]any{"device_keys": map[string]any{alice.UserID: []string{}}}); status == http.StatusOK {
		t.Fatalf("an unsigned key query was answered; anybody could then collect this server's keys")
	}
}

func TestAPeerStartsAnOlmSessionByClaimingAOneTimeKey(t *testing.T) {
	server := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	server.FederationServer(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	withUploadedKeys(t, server, alice, 2)
	peer := newFakePeer(t)

	if held := oneTimeKeysHeld(t, server, alice); held != 2 {
		t.Fatalf("alice's device holds %d one-time keys before the claim, want 2", held)
	}

	path := "/_matrix/federation/v1/user/keys/claim"
	claim := func(users map[string]map[string]string) map[string]any {
		t.Helper()
		return peerPosts(t, server, peer, path, map[string]any{"one_time_keys": users})
	}

	claimOnce := func() map[string]any {
		t.Helper()
		answer := claim(map[string]map[string]string{alice.UserID: {alice.DeviceID: "signed_curve25519"}})
		keys, found := entry(t, answer, "one_time_keys", alice.UserID, alice.DeviceID)
		if !found || len(keys) != 1 {
			t.Fatalf("a claim took %v, want exactly one key", answer)
		}
		return keys
	}

	first := claimOnce()
	second := claimOnce()
	// The key is spent. Two claims have to produce two different keys, and never the same one twice:
	// the forward secrecy of the session that starts with a one-time key depends on that key having been
	// used exactly once, and a device that restarts and re-serves its stock would quietly break every
	// session it has already started.
	if same, _ := first[keyOf(t, first)].(string); same != "" && same == second[keyOf(t, second)] {
		t.Fatalf("a one-time key was handed out twice: %v", first)
	}
	if keyOf(t, first) == keyOf(t, second) {
		t.Fatalf("a one-time key was handed out twice under the same ID: %v then %v", first, second)
	}

	// And when the stock runs out, the honest answer is nothing at all — not an error, because a device
	// that has run out of prepaid keys is a normal participant of the protocol, and not a substitute,
	// because the asking device would build a session this device cannot decrypt.
	empty := claim(map[string]map[string]string{alice.UserID: {alice.DeviceID: "signed_curve25519"}})
	if left, found := entry(t, empty, "one_time_keys", alice.UserID, alice.DeviceID); found && len(left) > 0 {
		t.Fatalf("a third claim took a key from a device that had two: %v", left)
	}
	if held := oneTimeKeysHeld(t, server, alice); held != 0 {
		t.Fatalf("alice's device reports %d one-time keys after two claims, want none", held)
	}

	// A claim for a user this server does not host spends nothing — and must not disturb the devices it
	// does host, which is what the count is checked against.
	outsider := "@outsider:" + string(peer.serverNo)
	foreign := claim(map[string]map[string]string{outsider: {"ABCDEFGH": "signed_curve25519"}})
	if granted, found := entry(t, foreign, "one_time_keys", outsider); found {
		t.Fatalf("this server spent a key for a user it does not host: %v", granted)
	}
	if held := oneTimeKeysHeld(t, server, alice); held != 0 {
		t.Fatalf("alice's device holds %d one-time keys, want the two she uploaded to be exactly spent", held)
	}

	if status := unsignedPosts(t, server, path, map[string]any{
		"one_time_keys": map[string]map[string]string{alice.UserID: {alice.DeviceID: "signed_curve25519"}},
	}); status == http.StatusOK {
		t.Fatalf("an unsigned key claim was answered; any request that can reach this port could then drain " +
			"this server's devices and start sessions nobody asked for")
	}
}
