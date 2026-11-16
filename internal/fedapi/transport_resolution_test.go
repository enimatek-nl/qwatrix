package fedapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// A Matrix server name is an identity, not an address, and the specification is explicit that the
// two are found separately: a name with no port may redirect the caller through
// `/.well-known/matrix/server`, and a name that is a plain host name may point at a port through an
// SRV record. A homeserver that ignores that cannot ask where a peer actually lives, which means it
// cannot ask a peer anything at all — and the failure looks like the peer being down.
//
// Resolution itself is gomatrixserverlib's job; what this server has to get right is *using* it:
// dialling the address the peer pointed at while keeping the identity in the Host header and in the
// TLS handshake, because the identity is what the certificate, the request signature and the signing
// key are all about.

// TestAPeerNamedWithoutAPortIsReachedWhereItSaidToBe covers the redirect itself: the caller asked
// about `matrix.example`, and the answer says the address is somewhere else.
func TestAPeerNamedWithoutAPortIsReachedWhereItSaidToBe(t *testing.T) {
	var asked seenRequests
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.record(r.Host, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"server": map[string]any{"name": "synapse/1.99"}})
	}))
	defer peerServer.Close()

	p := newTestPeer(t, PeerOptions{
		// The name has no port, and no listener lives at it: only the resolver below can
		// turn it into the test server's address.
		ServerName:            spec.ServerName("qwatrix.test"),
		InsecureSkipTLSVerify: true,
		ResolvePeer:           toAddress(t, "matrix.example", peerServer.Listener.Addr().String()),
	})

	var answer struct {
		Server struct{ Name string } `json:"server"`
	}
	if err := p.GetJSON(context.Background(), "matrix.example", "/_matrix/federation/v1/version", &answer); err != nil {
		t.Fatalf("asking a peer whose name redirects: %v", err)
	}
	if answer.Server.Name != "synapse/1.99" {
		t.Errorf("got %#v", answer)
	}
	// Dialling a different address must not change who the request is about. A peer that answers
	// from a shared hosting farm picks its virtual host from this header; so does every signature
	// check on the way back.
	hosts := asked.hostsSeen()
	if len(hosts) != 1 || hosts[0] != "matrix.example" {
		t.Errorf("the request went out with Host %q, want the peer's own name matrix.example", hosts)
	}
}

// TestAPeerNamedWithAPortIsDialledExactly is the other half of the rule: a name that spells out
// where it lives is not a name that can be redirected, and an operator who wrote a port down wants
// that honoured — that is how a deployment behind a non-standard port is reached at all.
func TestAPeerNamedWithAPortIsDialledExactly(t *testing.T) {
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"server": map[string]any{"name": "qwatrix/9.9"}})
	}))
	defer peerServer.Close()

	resolved := 0
	p := newTestPeer(t, PeerOptions{
		ServerName:            spec.ServerName("qwatrix.test"),
		InsecureSkipTLSVerify: true,
		ResolvePeer: func(context.Context, spec.ServerName) ([]fclient.ResolutionResult, error) {
			resolved++
			return nil, nil
		},
	})

	name := spec.ServerName(strings.TrimPrefix(peerServer.URL, "https://"))
	var answer struct {
		Server struct{ Name string } `json:"server"`
	}
	if err := p.GetJSON(context.Background(), name, "/_matrix/federation/v1/version", &answer); err != nil {
		t.Fatalf("asking a peer named with a port: %v", err)
	}
	if answer.Server.Name != "qwatrix/9.9" {
		t.Errorf("got %#v", answer)
	}
	if resolved != 0 {
		t.Errorf("a server name that carries a port was resolved anyway (%d lookups)", resolved)
	}
}

// TestAnUnsignedKeyRequestFollowsTheSameAddress is the reason this matters more than a version
// banner: a peer's signing key is fetched over the same unsigned path, and a server that cannot get
// it can verify nothing anyone tells it.
func TestAnUnsignedKeyRequestFollowsTheSameAddress(t *testing.T) {
	var asked seenRequests
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.record(r.Host, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"server_name": "matrix.example"})
	}))
	defer peerServer.Close()

	p := newTestPeer(t, PeerOptions{
		ServerName:            spec.ServerName("qwatrix.test"),
		InsecureSkipTLSVerify: true,
		ResolvePeer:           toAddress(t, "matrix.example", peerServer.Listener.Addr().String()),
	})
	var document map[string]any
	if err := p.GetJSON(context.Background(), "matrix.example", "/_matrix/key/v2/server", &document); err != nil {
		t.Fatalf("fetching a signing key from a redirected peer: %v", err)
	}
	if !asked.hasPath("/_matrix/key/v2/server") {
		t.Errorf("the key request never arrived, only %s", asked.describe())
	}
}

// TestAPeerThatResolvesNowhereIsSaidSo keeps a failed lookup from becoming a silent fallback to
// some other address: no result is an error naming the peer, not a connection attempt elsewhere.
func TestAPeerThatResolvesNowhereIsSaidSo(t *testing.T) {
	p := newTestPeer(t, PeerOptions{
		ServerName:            spec.ServerName("qwatrix.test"),
		InsecureSkipTLSVerify: true,
		ResolvePeer: func(context.Context, spec.ServerName) ([]fclient.ResolutionResult, error) {
			return nil, nil
		},
	})
	var answer struct{}
	err := p.GetJSON(context.Background(), "nowhere.example", "/_matrix/federation/v1/version", &answer)
	if err == nil {
		t.Fatal("a peer that resolves to nothing was reported as reachable")
	}
	if !strings.Contains(err.Error(), "nowhere.example") {
		t.Errorf("error %q does not name the peer this server tried to reach", err)
	}
}

// TestAPeerIsLookedUpOnce is about the cost of asking. Discovery means a `.well-known` HTTPS request
// and SRV lookups, and federation asks where a peer lives on every single request — a signing-key
// fetch, a transaction, a join. A server that redid the lookup each time would spend more time
// resolving than talking, and a peer with a slow resolver would look slow even when it is answering
// promptly.
func TestAPeerIsLookedUpOnce(t *testing.T) {
	var asked seenRequests
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.record(r.Host, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"server": map[string]any{"name": "synapse/1.99"}})
	}))
	defer peerServer.Close()

	var lookups atomic.Int64
	p := newTestPeer(t, PeerOptions{
		ServerName:            spec.ServerName("qwatrix.test"),
		InsecureSkipTLSVerify: true,
		ResolvePeer:           counting(toAddress(t, "matrix.example", peerServer.Listener.Addr().String()), &lookups),
	})
	for i := 0; i < 3; i++ {
		var answer struct {
			Server struct{ Name string } `json:"server"`
		}
		if err := p.GetJSON(context.Background(), "matrix.example", "/_matrix/federation/v1/version", &answer); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if got := lookups.Load(); got != 1 {
		t.Errorf("the peer's address was looked up %d times for three requests, want 1", got)
	}
	if len(asked.hostsSeen()) != 3 {
		t.Errorf("the peer saw %d requests, want 3", len(asked.hostsSeen()))
	}
}

// TestAStuckLookupDoesNotHoldTheRequestForever bounds the worst case: `.well-known` is fetched with
// a client of somebody else's choosing, and a peer whose DNS is wedged must not be able to park a
// federation request for half a minute.
func TestAStuckLookupDoesNotHoldTheRequestForever(t *testing.T) {
	p := newTestPeer(t, PeerOptions{
		ServerName:            spec.ServerName("qwatrix.test"),
		InsecureSkipTLSVerify: true,
		ResolvePeer: func(ctx context.Context, _ spec.ServerName) ([]fclient.ResolutionResult, error) {
			// A resolver that answers, but only after ten seconds: long enough to be a
			// problem, short enough for the test to finish when the fix is missing.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Second):
				return nil, fmt.Errorf("resolver gave up")
			}
		},
	})
	start := time.Now()
	var answer struct{}
	err := p.GetJSON(context.Background(), "slow.example", "/_matrix/federation/v1/version", &answer)
	if err == nil {
		t.Fatal("a lookup that never finished reported success")
	}
	// The resolver would have answered after ten seconds on its own; the request must not wait for
	// that. The exact budget is a tuning decision, so this only pins that the request gave up well
	// before the thing it was waiting for.
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("a wedged lookup held the request for %v", took)
	}
}

// TestThisServerIsNeverSentToItsOwnResolver is the boring case that matters: a server that asks its
// own name where it lives can be told something that is true of the public deployment and useless
// from inside the container — and the question costs a network round trip either way.
func TestThisServerIsNeverSentToItsOwnResolver(t *testing.T) {
	var lookups atomic.Int64
	p := newTestPeer(t, PeerOptions{
		ServerName:            spec.ServerName("home.example"),
		InsecureSkipTLSVerify: true,
		ResolvePeer: func(_ context.Context, name spec.ServerName) ([]fclient.ResolutionResult, error) {
			lookups.Add(1)
			return nil, nil
		},
	})
	var answer struct{}
	if err := p.GetJSON(context.Background(), "home.example", "/_matrix/federation/v1/version", &answer); err == nil {
		t.Fatal("expected the request to fail: nothing listens at home.example:8448")
	}
	if got := lookups.Load(); got != 0 {
		t.Errorf("this server's own name was resolved %d time(s)", got)
	}
}

// seenRequests records what a stand-in peer was asked, in the order it was asked. The recorder is
// shared between the handler goroutine and the test, so it is guarded rather than a channel: a
// channel would need a reader that knows how many requests are coming.
// counting wraps a resolver, counting the lookups that went through it.
func counting(resolve ResolveFunc, counter *atomic.Int64) ResolveFunc {
	return func(ctx context.Context, name spec.ServerName) ([]fclient.ResolutionResult, error) {
		counter.Add(1)
		return resolve(ctx, name)
	}
}

type seenRequests struct {
	mu    sync.Mutex
	hosts []string
	paths []string
}

func (r *seenRequests) record(host, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts = append(r.hosts, host)
	r.paths = append(r.paths, path)
}

func (r *seenRequests) hostsSeen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

func (r *seenRequests) hasPath(want string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		if p == want {
			return true
		}
	}
	return false
}

func (r *seenRequests) describe() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.paths, ", ")
}

// toAddress resolves one name to one address, which is all the specification's discovery rules come
// down to once there is no DNS to point at.
func toAddress(t *testing.T, name, address string) func(context.Context, spec.ServerName) ([]fclient.ResolutionResult, error) {
	t.Helper()
	return func(_ context.Context, got spec.ServerName) ([]fclient.ResolutionResult, error) {
		if string(got) != name {
			return nil, nil
		}
		return []fclient.ResolutionResult{{
			Destination: address,
			Host:        got,
			// The certificate is checked against the identity, not the address, which is what
			// makes a redirect safe instead of an invitation to impersonate anybody.
			TLSServerName: name,
		}}, nil
	}
}

func newTestPeer(t *testing.T, opts PeerOptions) *Peer {
	t.Helper()
	if opts.ServerName == "" {
		opts.ServerName = "qwatrix.test"
	}
	if opts.KeyID == "" {
		opts.KeyID = "ed25519:test"
	}
	if len(opts.PrivateKey) == 0 {
		_, key, _ := ed25519.GenerateKey(nil)
		opts.PrivateKey = key
	}
	p, err := NewPeer(opts)
	if err != nil {
		t.Fatalf("building a peer client: %v", err)
	}
	return p
}
