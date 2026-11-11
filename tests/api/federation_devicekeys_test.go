package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	fclient "github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// Device lists over federation are what makes a cross-server encrypted message readable. Everything
// else in this suite can fail loudly; this fails by delivering an event that nobody can decrypt, after
// which the client shows a lock icon and the user assumes somebody else's server is at fault.
//
// These tests use two real homeservers wherever a signature is involved. The fake peer is useful and is
// used below for the shapes it can produce, but a fake peer cannot decline to verify a signature — and
// every real bug found in this server's federation so far was in the half where two servers each
// checked the other's arithmetic.

// TestARemoteUsersDevicesComeFromTheirOwnServer is the feature: alice, on one homeserver, asks who she
// may encrypt to for bob, who lives on another — and gets bob's devices, fetched from bob's server.
func TestARemoteUsersDevicesComeFromTheirOwnServer(t *testing.T) {
	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	bob := hs2.Register(t, "bob", "pw-bob", false)
	uploadDeviceKeys(t, bob, hs2)

	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)

	if alice.UserID == bob.UserID {
		t.Fatalf("the two homeservers issued the same user ID: %q", alice.UserID)
	}

	response := alice.MustPostJSON(t, hs1, "/_matrix/client/v3/keys/query", map[string]any{
		"device_keys": map[string][]string{bob.UserID: {}},
	}).MustStatus(t, 200)

	var answer struct {
		DeviceKeys map[string]map[string]struct {
			UserID   string            `json:"user_id"`
			DeviceID string            `json:"device_id"`
			Keys     map[string]string `json:"keys"`
		} `json:"device_keys"`
	}
	if err := json.Unmarshal(response.Body, &answer); err != nil {
		t.Fatalf("the query answer is not the documented shape: %v\n%s", err, response.Body)
	}
	devices, ok := answer.DeviceKeys[bob.UserID]
	if !ok {
		t.Fatalf("the query did not answer for %q at all: %s", bob.UserID, response.Body)
	}
	device, ok := devices[bob.DeviceID]
	if !ok {
		t.Fatalf("%q published device %q and the query did not carry it: %s\n"+
			"HS1 LOG:\n%s\nHS2 LOG:\n%s",
			bob.UserID, bob.DeviceID, response.Body, hs1.Logs(), hs2.Logs())
	}
	if device.UserID != bob.UserID {
		t.Fatalf("the device key names %q, was fetched as %q", device.UserID, bob.UserID)
	}
	if device.Keys["ed25519:"+bob.DeviceID] != "dummy-key" {
		t.Fatalf("the device key material did not survive the round trip: %+v", device.Keys)
	}
}

// TestAQueryForARemoteUserSurvivesTheirServerBeingDown records what a client sees when the other
// homeserver is unreachable. It has to be an empty answer about that user and not a failure of the
// whole query: one unreachable server must not lock every encrypted room a user is in, because a
// query usually covers a whole room at once.
func TestAQueryForARemoteUserSurvivesTheirServerBeingDown(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	uploadDeviceKeys(t, alice, hs1)

	// A user on a server name that answers nothing. The port was reserved and released, so the
	// connection is refused rather than left to hang.
	gone := harness.Account{UserID: "@gone:127.0.0.1:" + strconv.Itoa(reserveFreePort(t))}

	response := alice.MustPostJSON(t, hs1, "/_matrix/client/v3/keys/query", map[string]any{
		"device_keys": map[string][]string{alice.UserID: {}, gone.UserID: {}},
	}).MustStatus(t, 200)

	var answer struct {
		DeviceKeys map[string]map[string]json.RawMessage `json:"device_keys"`
	}
	if err := json.Unmarshal(response.Body, &answer); err != nil {
		t.Fatalf("the answer is not the documented shape: %v\n%s", err, response.Body)
	}
	if len(answer.DeviceKeys[alice.UserID]) == 0 {
		t.Fatalf("the local user's own devices were lost because a stranger was unreachable: %s",
			response.Body)
	}
	if entry, ok := answer.DeviceKeys[gone.UserID]; !ok {
		t.Fatalf("the unreachable user was omitted, which a client reads as an unanswered question: %s",
			response.Body)
	} else if len(entry) != 0 {
		t.Fatalf("the unreachable user was given devices from nowhere: %v", entry)
	}
}

// TestAPeerCannotPublishAnotherServersDeviceList is the security half. A homeserver that would describe
// `@alice:hs1` lets any peer mint a device for alice on hs1 by answering a query with a key of its
// choosing — which is not a bug in encryption, it is the end of it.
func TestAPeerCannotPublishAnotherServersDeviceList(t *testing.T) {
	server := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	server.FederationServer(t)
	foreign := "@victim:127.0.0.1:" + strconv.Itoa(reserveFreePort(t))
	peer := newFakePeer(t)
	status, body := federationGetSigned(t, server, peer, "/_matrix/federation/v1/user/devices/"+
		url.PathEscape(foreign))
	if status == http.StatusOK {
		t.Fatalf("the server described %q, a user it does not host: %s", foreign, body)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d for %q, want 404 (a refusal that is not \"no such user\" would let a peer "+
			"/probe which accounts exist here)\nbody: %s", status, foreign, body)
	}
	if errcode := errcodeOf(t, body); errcode != "M_NOT_FOUND" {
		t.Fatalf("errcode = %q, want M_NOT_FOUND: %s", errcode, body)
	}
}

// TestAPublishedDeviceListCarriesThisServersOwnUsers is the serving half, seen from the outside: a peer
// asking about a local user gets the device that user uploaded, and nothing belonging to anybody else.
func TestAPublishedDeviceListCarriesThisServersOwnUsers(t *testing.T) {
	server := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	server.FederationServer(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	uploadDeviceKeys(t, alice, server)
	bob := server.Register(t, "bob", "pw-bob", false)

	// Unsigned first: the answer is what a peer uses to decide whom to encrypt to, so the question
	// has to be signed, and a server that answers it anonymously is a server whose device lists can be
	// answered by whoever is in the middle.
	if status, body := federationGet(t, server, "/_matrix/federation/v1/user/devices/"+
		url.PathEscape(alice.UserID)); status < 400 {
		t.Fatalf("an unsigned peer was handed a device list (%d): %s", status, body)
	}

	peer := newFakePeer(t)
	status, body := federationGetSigned(t, server, peer, "/_matrix/federation/v1/user/devices/"+
		url.PathEscape(alice.UserID))
	if status != http.StatusOK {
		t.Fatalf("a signed peer asking about this server's own user failed (%d): %s\nSERVER LOG:\n%s",
			status, body, server.Logs())
	}
	var answer struct {
		UserID      string           `json:"user_id"`
		DeviceLists []map[string]any `json:"device_lists"`
		Devices     []map[string]any `json:"devices"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the device list is not JSON: %v\n%s", err, body)
	}
	if answer.UserID != alice.UserID {
		t.Fatalf("the answer names %q, was asked about %q", answer.UserID, alice.UserID)
	}
	// The array is published under the specification's name. The other spelling is accepted when
	// reading a peer, but this server does not get to be the reason a peer's parser finds nothing.
	published := answer.DeviceLists
	if len(published) == 0 {
		t.Fatalf("the published device list is empty, and the alternative spelling would be a "+
			"compatibility accident rather than a decision: %s", body)
	}
	if got, ok := published[0]["device_id"].(string); !ok || got != alice.DeviceID {
		t.Fatalf("the published device list does not carry %q: %s", alice.DeviceID, body)
	}
	_ = bob
}

// errcodeOf reads the errcode of an error body without insisting on the rest of it.
func errcodeOf(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		ErrCode string `json:"errcode"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// A refusal that is not a matrix error at all is reported by the caller's own assertion on the
		// body, which is printed alongside; this returns empty so that happens.
		return ""
	}
	return parsed.ErrCode
}

// federationGetSigned asks a server's federation listener something as a peer whose signing key that
// server can go and fetch, and returns the status and body.
//
// The signing matters: every federation read except keys and version is signed, so an unsigned request
// tests the refusal rather than the endpoint — which is worth a test of its own, but not this one.
func federationGetSigned(t *testing.T, server *harness.Server, peer *fakePeer, path string) (int, []byte) {
	t.Helper()

	request := fclient.NewFederationRequest(http.MethodGet, peer.serverNo,
		spec.ServerName(server.ServerName), path)
	if err := request.Sign(peer.serverNo, peer.keyID, peer.priv); err != nil {
		t.Fatalf("signing the request: %v", err)
	}
	httpRequest, err := request.HTTPRequest()
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	// Only the scheme and host change. The path is what the signature covers, and rewriting it would
	// sign a different request than the one sent.
	httpRequest.URL.Scheme = "https"
	httpRequest.URL.Host = strings.TrimPrefix(server.FederationServer(t).URL, "https://")
	response, err := server.FederationClient(t).Do(httpRequest)
	if err != nil {
		t.Fatalf("sending the request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	return response.StatusCode, body
}
