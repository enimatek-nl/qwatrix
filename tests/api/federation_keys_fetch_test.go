package api_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/fedapi"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// fakePeer is a stand-in for another homeserver: it has a signing key, publishes it the way the
// specification says, and can sign a document the way an event would be signed.
type fakePeer struct {
	server   *httptest.Server
	serverNo spec.ServerName
	keyID    gomatrixserverlib.KeyID
	priv     ed25519.PrivateKey
	hits     atomic.Int64
	// broken mutates the published key response to test rejection paths.
	broken func(map[string]any)
	// serves offers this fake any request it does not natively handle, and returns whether it
	// answered. Tests use it to make the peer a source of room history (GET /event/{eventID}).
	serves func(http.ResponseWriter, *http.Request) bool
	// otherHits counts the requests this fake answered on behalf of `serves`.
	otherHits atomic.Int64
}

func newFakePeer(t *testing.T) *fakePeer {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the peer key: %v", err)
	}
	peer := &fakePeer{
		keyID: gomatrixserverlib.KeyID("ed25519:peer-key"),
		priv:  priv,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.hits.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/_matrix/key/v2/server") {
			if peer.serves != nil && peer.serves(w, r) {
				peer.otherHits.Add(1)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The server name comes from the request's Host header: the document names the
		// server it claims to be, and a verifier compares the two. (`r.URL.Host` is empty in
		// a server-side handler — a detail that once made this fake publish nobody.)
		name := spec.ServerName(r.Host)
		body := map[string]any{
			"server_name":    string(name),
			"valid_until_ts": spec.Timestamp(time.Now().Add(time.Hour).UnixMilli()),
			"verify_keys": map[string]any{
				string(peer.keyID): map[string]any{"key": encodeBase64(pub)},
			},
			"old_verify_keys": map[string]any{},
		}
		if peer.broken != nil {
			peer.broken(body)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the peer key response: %v", err)
		}
		if body["server_name"] == string(name) {
			raw, err = gomatrixserverlib.SignJSON(string(name), peer.keyID, peer.priv, raw)
			if err != nil {
				t.Fatalf("signing the peer key response: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	peer.server = httptest.NewTLSServer(handler)
	// The httptest host is an IP literal with a port, which is a legal federation server name
	// for a test and needs no DNS or .well-known lookup.
	peer.serverNo = spec.ServerName(strings.TrimPrefix(peer.server.URL, "https://"))
	t.Cleanup(peer.server.Close)
	return peer
}

// document returns a signed document whose signature the ring should be able to check, plus the
// public key needed to forge nothing.
func (p *fakePeer) document(t *testing.T) []byte {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"hello": "world", "number": 1})
	if err != nil {
		t.Fatalf("encoding the document: %v", err)
	}
	signed, err := gomatrixserverlib.SignJSON(string(p.serverNo), p.keyID, p.priv, raw)
	if err != nil {
		t.Fatalf("signing the document: %v", err)
	}
	return signed
}

func (p *fakePeer) peerClient(t *testing.T) *fedapi.Peer {
	t.Helper()

	// The fake peer's certificate is the test server's own, so verification is skipped for the
	// same reason a test network skips it. The rejection tests below are what show the *key*
	// checks still bite.
	client, err := fedapi.NewPeer(fedapi.PeerOptions{
		ServerName:            spec.ServerName("qwatrix.test"),
		KeyID:                 "ed25519:ours",
		PrivateKey:            testSigningKey(t),
		InsecureSkipTLSVerify: true,
	})
	if err != nil {
		t.Fatalf("building the federation client: %v", err)
	}
	return client
}

// TestFederationBelievesAPeerThatSignsForItself checks the path a real peer takes.
//
// A signature on an event is only worth the key that made it, and the key is only worth the
// response that published it. This test walks that chain end to end over TLS: fetch the key, check
// that the response signs itself, then use the key to check something else the peer signed.
func TestFederationBelievesAPeerThatSignsForItself(t *testing.T) {
	peer := newFakePeer(t)
	ring := fedapi.NewKeyRing(peer.peerClient(t))

	results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{fedapi.VerifyRequest(peer.serverNo, time.Now(), peer.document(t))})
	if err != nil {
		t.Fatalf("VerifyJSONs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Error != nil {
		t.Fatalf("a peer signing for itself was rejected: %v", results[0].Error)
	}
	if got := peer.hits.Load(); got == 0 {
		t.Errorf("no key was fetched, so the verification was not real")
	}
}

// TestFederationRefusesAKeyResponseThatDoesNotProvePossession keeps the check honest about what it
// proves.
//
// A key response has to be signed by the key it publishes. That proves the author holds the private
// half, which is what makes the answer worth caching — and it is *all* it proves. Identity comes
// from the TLS certificate on the connection the answer arrived over: a self-signed response naming
// this server, from an attacker who can answer the request, would pass the signature check. So the
// check is a bar on carelessness, not on a person in the middle, and `Peer` refuses to skip
// certificate verification unless an operator asks for it.
func TestFederationRefusesAKeyResponseThatDoesNotProvePossession(t *testing.T) {
	peer := newFakePeer(t)
	attackerPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the adversary key: %v", err)
	}
	// Somebody else's public key published under this peer's key ID, signed with this peer's real
	// key: the author demonstrably holds the honest key, but is not speaking for the key it names.
	mismatched := peer.signedKeyResponse(t, peer.serverNo, attackerPub, peer.priv)
	var keys gomatrixserverlib.ServerKeys
	if err := json.Unmarshal(mismatched, &keys); err != nil {
		t.Fatalf("decoding the key response: %v", err)
	}
	if err := fedapi.CheckKeyResponse(peer.serverNo, keys); err == nil {
		t.Fatal("a key response published a key that never signed it")
	} else if !strings.Contains(err.Error(), "does not sign") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}

	// The honest response, same helper, is accepted — otherwise the test above would pass for free.
	honest := peer.signedKeyResponse(t, peer.serverNo, peer.priv.Public().(ed25519.PublicKey), peer.priv)
	if err := json.Unmarshal(honest, &keys); err != nil {
		t.Fatalf("decoding the honest key response: %v", err)
	}
	if err := fedapi.CheckKeyResponse(peer.serverNo, keys); err != nil {
		t.Errorf("the honest key response was rejected: %v", err)
	}
}

// TestFederationRefusesAPeerNamingAnotherServer checks the response that is internally consistent but
// describes a server nobody asked about.
func TestFederationRefusesAPeerNamingAnotherServer(t *testing.T) {
	peer := newFakePeer(t)
	pub := peer.priv.Public().(ed25519.PublicKey)
	document := peer.signedKeyResponse(t, "somewhere.else", pub, peer.priv)

	var keys gomatrixserverlib.ServerKeys
	if err := json.Unmarshal(document, &keys); err != nil {
		t.Fatalf("decoding the key response: %v", err)
	}
	if err := fedapi.CheckKeyResponse(peer.serverNo, keys); err == nil {
		t.Fatal("a key response naming a different server was believed")
	} else if !strings.Contains(err.Error(), "not the peer") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

// TestFederationRefusesAKeyWithNoKeysAtAll checks the quiet empty answer. A peer that publishes no
// keys at all has answered nothing, and treating that as a successful lookup would let a stalled
// server quietly invalidate every event it ever sent.
func TestFederationRefusesAKeyWithNoKeysAtAll(t *testing.T) {
	peer := newFakePeer(t)
	document := peer.signedKeyResponse(t, peer.serverNo, peer.priv.Public().(ed25519.PublicKey), peer.priv)
	var keys gomatrixserverlib.ServerKeys
	if err := json.Unmarshal(document, &keys); err != nil {
		t.Fatalf("decoding the key response: %v", err)
	}
	keys.VerifyKeys = nil
	if err := fedapi.CheckKeyResponse(peer.serverNo, keys); err == nil {
		t.Error("a key response with no keys was accepted")
	}
}

// TestFederationCachesPeerKeys checks that one peer is asked once.
//
// Every federated event verification would otherwise be a round trip to the sender, and federation
// verification happens while a peer is waiting on an answer — so an unbounded key fetch turns a
// busy room into a stack of blocking HTTP requests.
func TestFederationCachesPeerKeys(t *testing.T) {
	peer := newFakePeer(t)
	ring := fedapi.NewKeyRing(peer.peerClient(t))
	ctx := context.Background()
	request := []gomatrixserverlib.VerifyJSONRequest{fedapi.VerifyRequest(peer.serverNo, time.Now(), peer.document(t))}
	for i := 0; i < 3; i++ {
		if results, err := ring.VerifyJSONs(ctx, request); err != nil || results[0].Error != nil {
			t.Fatalf("verification %d failed: %v %v", i, err, results[0].Error)
		}
	}
	if got := peer.hits.Load(); got != 1 {
		t.Errorf("the peer was asked for its key %d times, want 1", got)
	}
}

// signedKeyResponse publishes publishFor's identity signed by signWith, which is how a forged or a
// mismatched key response is built.
func (p *fakePeer) signedKeyResponse(t *testing.T, publishFor spec.ServerName, publish ed25519.PublicKey, signWith ed25519.PrivateKey) []byte {
	t.Helper()

	body := map[string]any{
		"server_name":    string(publishFor),
		"valid_until_ts": spec.Timestamp(time.Now().Add(time.Hour).UnixMilli()),
		"verify_keys": map[string]any{
			string(p.keyID): map[string]any{"key": encodeBase64(publish)},
		},
		"old_verify_keys": map[string]any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the key response: %v", err)
	}
	signed, err := gomatrixserverlib.SignJSON(string(publishFor), p.keyID, signWith, raw)
	if err != nil {
		t.Fatalf("signing the key response: %v", err)
	}
	return signed
}

// encodeBase64 is the specification's encoding for a key: standard base64 without padding.
func encodeBase64(key []byte) string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString(key), "=")
}

func testSigningKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating our signing key: %v", err)
	}
	return priv
}
