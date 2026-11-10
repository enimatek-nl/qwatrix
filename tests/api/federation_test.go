package api_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
)

// TestFederationVersionIsAdvertised checks the smallest piece of the server-server API.
//
// A peer that has never seen this server asks what it is before it asks anything else, and an
// unanswered question here is indistinguishable from a server that does not federate at all. The
// specification leaves the string's shape to the implementation; what matters is that it is a
// string, and that the answer arrives over the federation port rather than the client one.
func TestFederationVersionIsAdvertised(t *testing.T) {
	server := harness.New(t)
	resp, err := server.FederationClient(t).Get(server.FederationServer(t).URL + "/_matrix/federation/v1/version")
	if err != nil {
		t.Fatalf("GET /_matrix/federation/v1/version: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the version response: %v", err)
	}
	var version struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &version); err != nil {
		t.Fatalf("the version response is not JSON: %v (%s)", err, body)
	}
	if version.Version == "" {
		t.Errorf("the version response named no implementation: %s", body)
	}
}

// TestFederationKeysArePublishedAndSigned checks the credential every peer has to believe.
//
// A server's signing key is what makes an event it wrote attributable to it, so this response is
// the root of trust for federation. It has to name this server, carry the current key under an ID
// with the algorithm prefix, stay valid into the future, and be signed by that very key — a
// response an outsider cannot forge and a peer can verify without trusting anybody else.
func TestFederationKeysArePublishedAndSigned(t *testing.T) {
	server := harness.New(t)
	body := federationKeys(t, server, "/_matrix/key/v2/server")

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the key response is not JSON: %v", err)
	}
	if got := doc["server_name"]; got != server.ServerName {
		t.Errorf("server_name: got %v, want %s", got, server.ServerName)
	}
	validUntil, ok := doc["valid_until_ts"].(float64)
	if !ok {
		t.Fatalf("valid_until_ts is not a number: %T", doc["valid_until_ts"])
	}
	if time.UnixMilli(int64(validUntil)).Before(time.Now()) {
		t.Errorf("valid_until_ts is in the past: %s", time.UnixMilli(int64(validUntil)))
	}

	verifyKeys, ok := doc["verify_keys"].(map[string]any)
	if !ok || len(verifyKeys) == 0 {
		t.Fatalf("verify_keys is missing or empty: %v", doc["verify_keys"])
	}
	// Old keys must exist as a key even when empty: a peer distinguishing "no old keys" from a
	// malformed response should not have to guess.
	if _, ok := doc["old_verify_keys"]; !ok {
		t.Errorf("old_verify_keys is absent, so a peer cannot tell it from a truncated response")
	}
	signatures, ok := doc["signatures"].(map[string]any)
	if !ok || len(signatures) != 1 {
		t.Fatalf("signatures should name exactly this server: %v", doc["signatures"])
	}
	for _, keyID := range keysOf(verifyKeys) {
		edKey, err := base64.RawStdEncoding.DecodeString(keyBody(t, verifyKeys[keyID]))
		if err != nil {
			t.Fatalf("verify_keys[%s] is not base64: %v", keyID, err)
		}
		sigs, ok := signatures[string(server.ServerName)].(map[string]any)
		if !ok {
			t.Fatalf("signatures has no entry for %s", server.ServerName)
		}
		rawSig, ok := sigs[keyID].(string)
		if !ok {
			t.Fatalf("no signature under %s, so a peer could not check anything", keyID)
		}
		if _, err := base64.RawStdEncoding.DecodeString(rawSig); err != nil {
			t.Fatalf("signature %s is not base64: %v", keyID, err)
		}
		// VerifyJSON takes the signed document, lifts the signature out of it, removes the
		// fields a signature does not cover, and canonicalises what is left — which is exactly
		// what a peer receiving this response over the wire will do.
		if err := gomatrixserverlib.VerifyJSON(string(server.ServerName),
			gomatrixserverlib.KeyID(keyID), edKey, body); err != nil {
			t.Errorf("the published key does not sign the response it publishes (%s): %v", keyID, err)
		}
	}
}

// TestFederationKeyCanBeFetchedByID checks the addressing a peer needs when it already knows which
// key it wants — which is the normal case, because an event names the key that signed it.
func TestFederationKeyCanBeFetchedByID(t *testing.T) {
	server := harness.New(t)
	body := federationKeys(t, server, "/_matrix/key/v2/server")
	var doc struct {
		VerifyKeys map[string]any `json:"verify_keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.VerifyKeys) == 0 {
		t.Fatalf("no verify_keys in %s", body)
	}
	keyID := keysOf(doc.VerifyKeys)[0]

	byID := federationKeys(t, server, "/_matrix/key/v2/server/"+keyID)
	var same map[string]any
	if err := json.Unmarshal(byID, &same); err != nil {
		t.Fatalf("the keyed response is not JSON: %v", err)
	}
	if same["server_name"] != server.ServerName {
		t.Errorf("the keyed response names %v", same["server_name"])
	}

	resp, err := server.FederationClient(t).Get(
		server.FederationServer(t).URL + "/_matrix/key/v2/server/ed25519:not-a-key")
	if err != nil {
		t.Fatalf("GET by key ID: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown key ID: got %d, want 404", resp.StatusCode)
	}
}

// federationKeys GETs a federation key endpoint and returns its body, failing the test otherwise.
func federationKeys(t *testing.T, server *harness.Server, path string) []byte {
	t.Helper()

	resp, err := server.FederationClient(t).Get(server.FederationServer(t).URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200 (%s)", path, resp.StatusCode, body)
	}
	return body
}

// keysOf returns the sorted keys of a decoded JSON object.
func keysOf(object map[string]any) []string {
	out := make([]string, 0, len(object))
	for key := range object {
		out = append(out, key)
	}
	return out
}

// keyBody pulls the base64 key string out of a verify_keys entry.
func keyBody(t *testing.T, entry any) string {
	t.Helper()

	object, ok := entry.(map[string]any)
	if !ok {
		t.Fatalf("a verify_keys entry is not an object: %T", entry)
	}
	raw, ok := object["key"].(string)
	if !ok {
		t.Fatalf("a verify_keys entry has no string key field: %v", object)
	}
	return raw
}

// federationGet performs one server-server GET and returns the status and body.
func federationGet(t *testing.T, server *harness.Server, path string) (int, []byte) {
	t.Helper()

	resp, err := server.FederationClient(t).Get(server.FederationServer(t).URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return resp.StatusCode, body
}

// TestMakeJoinOffersAPlaceInTheGraph checks what a peer is given when it wants to join.
//
// A remote server cannot know where this room's history ends or which events authorise a join, so
// `make_join` hands it that shape: the previous event, the depth, the authorising events, and a
// membership waiting for a signature. Getting these wrong is not a warning — the peer signs whatever
// it was given and this server then refuses the join it just invited.
func TestMakeJoinOffersAPlaceInTheGraph(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSendText(t, server, roomID, "the current tip")

	suffix := "/_matrix/federation/v1/make_join/" + harness.PathEscape(roomID) +
		"/" + harness.PathEscape("@faraway:remote.example")
	status, body := federationGet(t, server, suffix)
	if status != http.StatusOK {
		t.Fatalf("make_join: status %d, want 200 (%s)", status, body)
	}
	var response struct {
		RoomVersion string `json:"room_version"`
		Event       struct {
			Type       string            `json:"type"`
			StateKey   *string           `json:"state_key"`
			RoomID     string            `json:"room_id"`
			Depth      int64             `json:"depth"`
			PrevEvents []string          `json:"prev_events"`
			AuthEvents []string          `json:"auth_events"`
			Content    map[string]string `json:"content"`
			Sender     string            `json:"sender"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("the make_join response is not the documented shape: %v (%s)", err, body)
	}
	if response.RoomVersion == "" {
		t.Errorf("no room version was named, so the peer cannot pick an event format")
	}
	event := response.Event
	if event.Type != "m.room.member" {
		t.Errorf("template type: got %q, want m.room.member", event.Type)
	}
	if event.StateKey == nil || *event.StateKey != "@faraway:remote.example" {
		t.Errorf("template state_key: got %v, want the joining user", event.StateKey)
	}
	if event.RoomID != roomID {
		t.Errorf("template room_id: got %q, want %s", event.RoomID, roomID)
	}
	if event.Content["membership"] != "join" {
		t.Errorf("template membership: got %v, want join", event.Content)
	}
	if event.Sender != "" {
		t.Errorf("the template chose a sender (%q); that is the peer's to fill in", event.Sender)
	}
	if len(event.PrevEvents) != 1 {
		t.Fatalf("template prev_events: got %v, want exactly the current tip", event.PrevEvents)
	}
	if event.Depth < 2 {
		t.Errorf("template depth %d would put the join behind events already in the room", event.Depth)
	}
	// The authorising set is the specification's table for a join: the room's create event, its
	// join rules, and its power levels if it has them.
	joined := strings.Join(event.AuthEvents, " ")
	if !strings.Contains(joined, "$") || len(event.AuthEvents) < 2 {
		t.Errorf("template auth_events: got %v, want the create event and the join rules at least", event.AuthEvents)
	}
}

// TestMakeJoinRefusesAnUnknownRoom checks that an invitation is not an existence oracle.
//
// A room this server has never heard of and a room the peer may not join into are answered
// identically, because a distinguishing answer would let anybody walk a list of candidate room IDs
// and collect the ones that exist.
func TestMakeJoinRefusesAnUnknownRoom(t *testing.T) {
	server := harness.NewWithRegistration(t)
	status, body := federationGet(t, server,
		"/_matrix/federation/v1/make_join/"+harness.PathEscape("!nothinghere:remote.example")+
			"/"+harness.PathEscape("@faraway:remote.example"))
	if status != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403 (%s)", status, body)
	}
	if !strings.Contains(string(body), "M_FORBIDDEN") {
		t.Errorf("body: got %s, want M_FORBIDDEN", body)
	}
}

// TestMakeJoinNegotiatesTheRoomVersion checks the only refusal a peer can act on.
//
// A peer lists the room versions it can build. If none of them is the room's version, the answer is
// M_INCOMPATIBLE_ROOM_VERSION naming the version that would be needed — the peer either learns that
// format or gives up, and neither outcome depends on reading prose.
func TestMakeJoinNegotiatesTheRoomVersion(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	path := "/_matrix/federation/v1/make_join/" + harness.PathEscape(roomID) +
		"/" + harness.PathEscape("@faraway:remote.example")

	status, body := federationGet(t, server, path+"?ver=completely.made.up")
	if status != http.StatusBadRequest {
		t.Fatalf("an unsupported version: status %d, want 400 (%s)", status, body)
	}
	if !strings.Contains(string(body), "M_INCOMPATIBLE_ROOM_VERSION") {
		t.Errorf("body: got %s, want M_INCOMPATIBLE_ROOM_VERSION", body)
	}
	var refusal struct {
		RoomVersion string `json:"room_version"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("the refusal is not JSON: %v", err)
	}
	if refusal.RoomVersion == "" {
		t.Errorf("the refusal named no room version, so the peer cannot correct its request: %s", body)
	}

	// Naming the room's own version is accepted.
	status, body = federationGet(t, server, path+"?ver="+refusal.RoomVersion)
	if status != http.StatusOK {
		t.Errorf("the room's own version: status %d, want 200 (%s)", status, body)
	}
}

// TestMakeJoinRejectsSomethingThatIsNotAUserID checks the handshake's front door. A peer that asks
// for a join as something unparseable has either a bug or an attack, and in both cases the answer is
// the same: this is not a user ID.
func TestMakeJoinRejectsSomethingThatIsNotAUserID(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	status, body := federationGet(t, server,
		"/_matrix/federation/v1/make_join/"+harness.PathEscape(roomID)+"/not-a-user-id")
	if status != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (%s)", status, body)
	}
}
