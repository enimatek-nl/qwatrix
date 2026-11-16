package api_test

// A peer's question about a user used to be answerable with a list of devices. That was always half
// an answer: a device key says which device it is, and only a cross-signing key says whether the
// person whose name it carries recognises it. A client on the far side of a federation border that
// gets only the first half shows its user an unverified device, and there is nothing the user can
// do about it from where they are standing.
//
// So both key endpoints carry the identity too. Neither carries the *user-signing* key: that one
// records whom a person vouches for, and it is nobody's business but the user's — including a peer
// that is asking on behalf of one of its own clients.

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// peerAsks signs a GET of this server's federation listener and decodes the answer.
func peerAsks(t *testing.T, server *harness.Server, peer *fakePeer, path string) map[string]any {
	t.Helper()

	request := fclient.NewFederationRequest(http.MethodGet, peer.serverNo,
		spec.ServerName(server.ServerName), path)
	if err := request.Sign(peer.serverNo, peer.keyID, peer.priv); err != nil {
		t.Fatalf("signing the request: %v", err)
	}
	httpRequest, err := request.HTTPRequest()
	if err != nil {
		t.Fatalf("building the HTTP request: %v", err)
	}
	httpRequest.URL.Scheme = "https"
	httpRequest.URL.Host = strings.TrimPrefix(server.FederationServer(t).URL, "https://")
	response, err := server.FederationClient(t).Do(httpRequest)
	if err != nil {
		t.Fatalf("asking the question: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("a signed question got %d: %s", response.StatusCode, body)
	}
	return jsonOf(t, response.Body)
}

// TestAPeerAskingAboutADeviceIsToldWhoseItIs covers the single-user device lookup, which is the
// endpoint a homeserver uses when it wants to encrypt for somebody it has never met.
func TestAPeerAskingAboutADeviceIsToldWhoseItIs(t *testing.T) {
	server := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	server.FederationServer(t)
	alice := server.Register(t, "remoteidentity", "pw-remoteidentity", false)
	withUploadedKeys(t, server, alice, 1)
	mustUploadCrossSigningKeys(t, alice, server, newIdentity(t))

	peer := newFakePeer(t)
	answer := peerAsks(t, server, peer,
		"/_matrix/federation/v1/user/devices/"+url.PathEscape(alice.UserID))

	for _, field := range []string{"master_key", "self_signing_key"} {
		if raw, found := answer[field]; !found || raw == nil {
			t.Fatalf("the answer carried no %q, so a peer cannot tell whether alice's devices are hers: %v", field, answer)
		}
	}
	// The user-signing key is the key that says whom alice trusts. A peer asking about a device has
	// no use for it and no claim on it.
	if raw, found := answer["user_signing_key"]; found && raw != nil {
		t.Errorf("the answer carried alice's user-signing key, which is hers to publish: %v", answer)
	}
}

// TestAPeerKeyQueryCarriesTheIdentityIs the batched form of the same rule, used when a whole room of
// remote users has to be resolved at once.
func TestAPeerKeyQueryCarriesTheIdentity(t *testing.T) {
	server := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	server.FederationServer(t)
	alice := server.Register(t, "batchedidentity", "pw-batchedidentity", false)
	withUploadedKeys(t, server, alice, 1)
	who := newIdentity(t)
	mustUploadCrossSigningKeys(t, alice, server, who)

	peer := newFakePeer(t)
	answer := peerPosts(t, server, peer, "/_matrix/federation/v1/user/keys/query",
		map[string]any{"device_keys": map[string]any{alice.UserID: []string{}}})

	for _, field := range []string{"master_keys", "self_signing_keys"} {
		keys, found := entry(t, answer, field, alice.UserID)
		if !found || len(keys) == 0 {
			t.Fatalf("the answer carried no %q for alice, so her devices arrive unattributed: %v", field, answer)
		}
	}
	if keys, found := entry(t, answer, "user_signing_keys", alice.UserID); found && len(keys) > 0 {
		t.Errorf("the batched answer carried alice's user-signing key: %v", answer)
	}
}
