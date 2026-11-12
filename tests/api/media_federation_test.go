package api_test

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// TestRemoteMediaIsFetchedFromTheServerThatHoldsIt is the difference between an image in a chat and a
// grey square in a chat.
//
// A Matrix mxc URI names the server the file belongs to, and the client's homeserver is asked to serve
// it. If that server has never heard of the file and does not go and ask, the user sees a broken image
// and there is nothing in the room state, the sync, or the error message to explain why — the file is
// simply not there, forever, even though it is sitting on the server two hops away.
func TestRemoteMediaIsFetchedFromTheServerThatHoldsIt(t *testing.T) {
	// hs1 runs at debug level so the test can count how often it is actually asked. A cache that is
	// never found still answers the second request — by fetching again — and that is exactly the kind
	// of defect that only shows up as load on the other server.
	hs1 := harness.NewWithRegistration(t, harness.WithLogLevel(slog.LevelDebug),
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	payload := []byte("\x89PNG\r\n\x1a\nfake-payload-for-a-federated-download-test")
	mxc := uploadFile(t, alice, hs1, "report.png", "image/png", payload)
	origin, mediaID := splitMXC(t, mxc)

	hs2 := harness.NewWithRegistration(t, harness.WithLogLevel(slog.LevelDebug),
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	bob := hs2.Register(t, "bob", "pw-bob", false)

	response := bob.MustGet(t, hs2, "/_matrix/client/v1/media/download/"+
		url.PathEscape(origin)+"/"+url.PathEscape(mediaID))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("downloading another server's media: status %d, want 200\nbody: %s\nHS2 LOG:\n%s",
			response.StatusCode, response.Body, hs2.Logs())
	}
	if string(response.Body) != string(payload) {
		t.Errorf("the bytes changed on the way through the second server: got %d bytes starting %q, want %d bytes starting %q",
			len(response.Body), response.Body[:min(24, len(response.Body))], len(payload), payload[:24])
	}
	if got := response.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png: a client renders an image or offers a download based on this",
			got)
	}
	// The disposition is a deliberate `attachment`: a file fetched from this server must not be
	// rendered by a browser that was merely pointed at it. What has to survive the second hop is the
	// uploader's suggested name, which is what a client offers to save the file as.
	if got := response.Header.Get("Content-Disposition"); got != `attachment; filename="report.png"` {
		t.Errorf("Content-Disposition = %q, want the uploader's filename preserved across the hop", got)
	}

	// The fetching server keeps what it fetched. Without the cache, every re-render of the message
	// asks the origin again, and the day the origin drops the file the room loses an image it has been
	// displaying for months.
	if again := bob.MustGet(t, hs2, "/_matrix/client/v1/media/download/"+
		url.PathEscape(origin)+"/"+url.PathEscape(mediaID)); again.StatusCode != http.StatusOK {
		t.Fatalf("the second download of the same remote media: status %d, want 200", again.StatusCode)
	}
	// The count is taken from whichever log holds it, because one process has one default logger and
	// two servers with different buffers: the maximum is the honest reading, and the sum would
	// double-count the moment a harness change shared the buffer.
	if asked := max(strings.Count(hs1.Logs(), "served a file to a peer"),
		strings.Count(hs2.Logs(), "served a file to a peer")); asked != 1 {
		t.Errorf("hs1 was asked for the file %d times, want 1: the second request should have been "+
			"served from the copy hs2 already holds", asked)
	}
}

// TestAFederatedMediaRequestCannotAskForAnotherServersFile is the boundary that makes the endpoint
// above safe to expose to peers: a signed request naming a media ID this server does not hold must not
// be answered from whatever the server happens to have cached about somebody else.
//
// Media IDs are opaque but they are not secret, and a server that answered for foreign IDs would let
// one homeserver serve bytes under another server's name.
func TestAFederatedMediaRequestCannotAskForAnotherServersFile(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	mxc := uploadFile(t, alice, hs1, "private.png", "image/png", []byte("only on hs1"))
	_, mediaID := splitMXC(t, mxc)

	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	hs2.Register(t, "bob", "pw-bob", false)

	peer := newFakePeer(t)
	status, body := federationGetSigned(t, hs2, peer,
		"/_matrix/federation/v1/media/download/"+url.PathEscape(mediaID))
	if status != http.StatusNotFound {
		t.Fatalf("asking hs2 for hs1's media: status %d, want 404\nbody: %s\nHS2 LOG:\n%s",
			status, body, hs2.Logs())
	}
}

// TestAFederatedMediaRequestMustBeSigned keeps the download endpoint off the list of things an
// anonymous passer-by can ask a peer.
func TestAFederatedMediaRequestMustBeSigned(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	mxc := uploadFile(t, alice, hs1, "public.png", "image/png", []byte("bytes"))
	_, mediaID := splitMXC(t, mxc)

	status, _ := federationGet(t, hs1, "/_matrix/federation/v1/media/download/"+url.PathEscape(mediaID))
	if status != http.StatusUnauthorized {
		t.Fatalf("an unsigned federated media request: status %d, want 401\nHS1 LOG:\n%s",
			status, hs1.Logs())
	}
}

// TestAFederatedMediaRequestServesThisServersOwnFile is the same endpoint from the side that is
// supposed to answer: a peer asking this server for a file this server originates gets the bytes, with
// the content type the uploader chose.
func TestAFederatedMediaRequestServesThisServersOwnFile(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	payload := []byte("bytes that belong to hs1")
	mxc := uploadFile(t, alice, hs1, "own.txt", "text/plain", payload)
	_, mediaID := splitMXC(t, mxc)

	peer := newFakePeer(t)
	status, body := federationGetSigned(t, hs1, peer,
		"/_matrix/federation/v1/media/download/"+url.PathEscape(mediaID))
	if status != http.StatusOK {
		t.Fatalf("a peer asking for this server's own media: status %d, want 200\nbody: %s\nHS1 LOG:\n%s",
			status, body, hs1.Logs())
	}
	if string(body) != string(payload) {
		t.Errorf("the served bytes are %q, want %q", body, payload)
	}
}

// TestAPeerAsksForMediaByTheSpecifiedPath is the ratified spelling of the same question —
// GET /_matrix/federation/v1/download/{serverName}/{mediaID} — which names the origin in the path.
//
// Serving only the draft shape is the mistake this pins: this server grew the draft, so its own fetches
// worked against itself, and every other implementation in the federation was asking a question this
// server answered with a 404.
func TestAPeerAsksForMediaByTheSpecifiedPath(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	payload := []byte("bytes asked for the specified way")
	mxc := uploadFile(t, alice, hs1, "specified.txt", "text/plain", payload)
	_, mediaID := splitMXC(t, mxc)

	peer := newFakePeer(t)
	path := "/_matrix/federation/v1/download/" + url.PathEscape(string(hs1.ServerName)) + "/" +
		url.PathEscape(mediaID)
	status, body := federationGetSigned(t, hs1, peer, path)
	if status != http.StatusOK {
		t.Fatalf("a peer asking by the specified path: status %d, want 200\nbody: %s\nHS1 LOG:\n%s",
			status, body, hs1.Logs())
	}
	if string(body) != string(payload) {
		t.Errorf("the served bytes are %q, want %q", body, payload)
	}
}

// Naming somebody else's server in that path is a question this server must not answer from its cache.
// It holds copies of other servers' files — that is what caching media means — and serving them here
// would let anybody who once saw a media ID read it from a server that was never supposed to have it.
func TestThisServerDoesNotServeAnotherServersMedia(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)

	peer := newFakePeer(t)
	path := "/_matrix/federation/v1/download/" + url.PathEscape("somewhere.else.example") + "/" +
		url.PathEscape("some-media-id")
	status, _ := federationGetSigned(t, hs1, peer, path)
	if status == http.StatusOK {
		t.Fatal("this server served a file for another server's name")
	}
	if status != http.StatusNotFound {
		t.Errorf("status %d for another server's media, want 404 (a question this server cannot answer)", status)
	}
}
