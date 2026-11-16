package fedapi

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// DefaultTimeout is how long a peer is given to answer one request. Federation is synchronous by
// specification — a peer holds the connection until it has the answer — so the alternative to a
// timeout is waiting forever on a server that has stopped caring.
const DefaultTimeout = 30 * time.Second

// PeerOptions configures how this server talks to other servers.
type PeerOptions struct {
	// ServerName, KeyID and PrivateKey are this server's signing identity: every outbound
	// federation request is signed with them, and a peer checks that signature against the
	// key this server publishes at /_matrix/key/v2/server.
	ServerName spec.ServerName
	KeyID      string
	PrivateKey ed25519.PrivateKey

	// Timeout bounds one peer request. Zero means DefaultTimeout.
	Timeout time.Duration
	// TrustedCACertFile is an additional certificate authority to trust for peer
	// certificates — the shape of a deployment behind a private CA.
	TrustedCACertFile string
	// InsecureSkipTLSVerify turns off peer certificate verification entirely.
	//
	// This defeats one of the two anchors federation rests on. Certificate verification is what
	// stops a man in the middle from answering "here is my signing key" with a key of his own;
	// the signature checks that follow are worthless if the key response was intercepted. It
	// exists because test networks issue self-signed peer certificates, and it logs a warning
	// at start-up for exactly that reason.
	InsecureSkipTLSVerify bool
	// UserAgent identifies this implementation to peers.
	UserAgent string
	// ResolvePeer turns a peer's server name into the addresses it lives at.
	//
	// Leaving it unset uses the specification's discovery rules: a `.well-known/matrix/server`
	// redirect, then a `_matrix-fed._tcp` SRV record, then the deprecated `_matrix._tcp` one, then
	// the federation port. It exists as a seam because resolution is DNS and HTTPS, and a test
	// network has neither.
	ResolvePeer ResolveFunc
	// ResolveTimeout bounds one discovery lookup, independently of Timeout: finding a peer is one
	// step of talking to it, and a wedged resolver must not spend the whole request budget.
	ResolveTimeout time.Duration
}

// ResolveFunc is the discovery half of federation: server name in, addresses out.
type ResolveFunc func(context.Context, spec.ServerName) ([]fclient.ResolutionResult, error)

// Peer is this server's outbound federation client.
type Peer struct {
	// client answers the unsigned questions about a peer: its signing keys and its version.
	client *fclient.Client
	// fed signs every other request with this server's identity.
	fed fclient.FederationClient
	// transport is kept so the TLS decisions can be reported by Status.
	transport http.RoundTripper
	opts      PeerOptions
	// insecure records the verification decision separately from the options, because it is the
	// one setting an operator should be able to confirm after the fact.
	insecure bool
}

// NewPeer builds the outbound federation client.
func NewPeer(opts PeerOptions) (*Peer, error) {
	if opts.ServerName == "" {
		return nil, fmt.Errorf("federation: an outbound peer client needs a server name to sign as")
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "qwatrix"
	}
	if opts.ResolvePeer == nil {
		opts.ResolvePeer = fclient.ResolveServer
	}
	if opts.ResolveTimeout == 0 {
		opts.ResolveTimeout = resolutionBudget
		if opts.Timeout > 0 && opts.Timeout < resolutionBudget {
			opts.ResolveTimeout = opts.Timeout
		}
	}
	transport, err := peerTransport(opts)
	if err != nil {
		return nil, err
	}
	signing, err := signingIdentity(opts)
	if err != nil {
		return nil, err
	}
	return &Peer{
		client:    fclient.NewClient(fclient.WithTransport(transport), fclient.WithTimeout(opts.Timeout)),
		fed:       fclient.NewFederationClient([]*fclient.SigningIdentity{signing}, fclient.WithTransport(transport), fclient.WithTimeout(opts.Timeout), fclient.WithUserAgent(opts.UserAgent)),
		transport: transport,
		opts:      opts,
		insecure:  opts.InsecureSkipTLSVerify,
	}, nil
}

// GetJSON fetches a JSON document from a peer over HTTPS.
//
// The name is looked up where the specification says to look it up — see
// [matrixSchemeRoundTripper] for the rules — and the request is then sent to the address that turned
// up, under the name. A name that spells out a port, or is an address, is dialled exactly as
// written, which is what complement's container network and a two-server test network rely on.
func (p *Peer) GetJSON(ctx context.Context, server spec.ServerName, path string, into any) error {
	url := "https://" + string(server) + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("federation: asking %s for %s: %w", server, path, err)
	}
	request.Header.Set("User-Agent", p.opts.UserAgent)
	response, err := p.transport.RoundTrip(request)
	if err != nil {
		return fmt.Errorf("federation: asking %s for %s: %w", server, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("federation: %s answered %s with %s", server, path, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPeerKeyResponseBytes))
	if err != nil {
		return fmt.Errorf("federation: reading %s's answer from %s: %w", path, server, err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("federation: %s sent something that is not JSON on %s: %w", server, path, err)
	}
	return nil
}

// maxPeerKeyResponseBytes bounds a key response. It is a small, fixed shape, and a peer that sends
// more than this is either broken or trying to make this server hold a large body in memory.
const maxPeerKeyResponseBytes = 64 << 10

// maxPeerDocumentBytes bounds a signed JSON answer read from a peer. A device list grows with the
// number of devices a user has, so it cannot use the key response's ceiling; it is still bounded,
// because a peer can always answer something enormous.
const maxPeerDocumentBytes = 4 << 20

// GetSignedJSON asks a peer for a JSON document with this server's signature on the request.
//
// [Peer.GetJSON] is the unsigned path and exists for the two questions a server must be able to ask
// before it trusts anybody: what are your signing keys, and what version are you. Everything else in
// federation is signed — not because the answers are secret, but because "which devices does
// `@alice:hs1` have?" is a question worth answering fraudulently, and an unsigned channel lets whoever
// is in the middle answer it.
//
// The caller decodes the body into its own type rather than a library struct, because some federation
// answers are signed documents whose bytes have to survive the journey: a device key whose
// `signatures` field was dropped by an intermediate struct is a key no client will accept, and the
// failure surfaces months later as a conversation that will not decrypt.
func (p *Peer) GetSignedJSON(ctx context.Context, server spec.ServerName, path string, into any) error {
	identity, err := signingIdentity(p.opts)
	if err != nil {
		return fmt.Errorf("federation: %s", err)
	}
	request := fclient.NewFederationRequest(http.MethodGet, p.opts.ServerName, server, path)
	// Signed before the HTTP request is built, because the signature covers the method and the URI and
	// the library refuses to modify a request once it has been signed — which is the right rule: the
	// alternative is a signature that quietly points at different bytes.
	if err := request.Sign(identity.ServerName, identity.KeyID, identity.PrivateKey); err != nil {
		return fmt.Errorf("federation: signing the request for %s: %w", server, err)
	}
	httpRequest, err := request.HTTPRequest()
	if err != nil {
		return fmt.Errorf("federation: building the request for %s: %w", server, err)
	}
	httpRequest = httpRequest.WithContext(ctx)

	response, err := p.transport.RoundTrip(httpRequest)
	if err != nil {
		return fmt.Errorf("federation: asking %s for %s: %w", server, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("federation: %s answered %s with %s", server, path, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPeerDocumentBytes))
	if err != nil {
		return fmt.Errorf("federation: reading %s's answer from %s: %w", path, server, err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("federation: %s's answer from %s is not JSON: %w", path, server, err)
	}
	return nil
}

// Signed is the client that signs requests with this server's identity. Every endpoint that asks a
// peer for something about a room — state, events, a transaction to deliver — goes through this.
func (p *Peer) Signed() fclient.FederationClient { return p.fed }

// Insecure reports whether peer certificate verification is off.
func (p *Peer) Insecure() bool { return p.insecure }

// ServerName is the identity this client signs as.
func (p *Peer) ServerName() spec.ServerName { return p.opts.ServerName }

// peerTransport builds the HTTP transport, applying the certificate decisions.
func peerTransport(opts PeerOptions) (http.RoundTripper, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TrustedCACertFile != "" {
		pem, err := os.ReadFile(opts.TrustedCACertFile)
		if err != nil {
			return nil, fmt.Errorf("federation: reading the trusted CA %s: %w", opts.TrustedCACertFile, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			// A system pool is not guaranteed to exist (a scratch container, a
			// static build). Starting from an empty one and adding the operator's CA
			// is the honest reading of "additionally trust this authority".
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("federation: %s contains no certificate to trust", opts.TrustedCACertFile)
		}
		tlsConfig.RootCAs = pool
	}
	if opts.InsecureSkipTLSVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("federation: the default HTTP transport is not configurable")
	}
	clone := base.Clone()
	clone.TLSClientConfig = tlsConfig
	clone.ForceAttemptHTTP2 = true
	return &matrixSchemeRoundTripper{
		next:    clone,
		resolve: opts.ResolvePeer,
		tls:     tlsConfig,
		self:    opts.ServerName,
		timeout: opts.ResolveTimeout,
	}, nil
}

// DefaultFederationPort is the port a peer is contacted on when its server name does not say.
const DefaultFederationPort = "8448"

// matrixSchemeRoundTripper turns a federation request into an HTTPS connection to the right address.
//
// gomatrixserverlib describes an outbound peer with its own `matrix://` scheme and normally installs
// its own transport to resolve that into a real address. Passing a transport of our own (which TLS
// policy requires: an operator's CA, or the test-network escape hatch) replaces that resolver
// wholesale, and every request would then fail with `unsupported protocol scheme "matrix"` — so this
// wrapper does the resolving itself:
//
//   - a name that carries a port, or is an IP literal, is dialled exactly as written, because the
//     specification says a server name that spells out where it lives is not a name that can be
//     redirected;
//   - any other name is looked up — `.well-known/matrix/server`, then `_matrix-fed._tcp`, then the
//     deprecated `_matrix._tcp`, then the federation port as the last resort — and the request is
//     sent to the address that turned up with the *name* still in the Host header and in the TLS
//     handshake.
//
// Keeping the name in those two places is not cosmetic. The Host header is how a peer behind a shared
// load balancer knows which homeserver was being asked, the certificate is issued for the name and
// not for the address it happens to answer on, and the signing key this server later fetches is
// published under the name. A client that dialled the address and forgot the name would connect and
// then be unable to believe anything it was told.
type matrixSchemeRoundTripper struct {
	next    http.RoundTripper
	resolve ResolveFunc
	tls     *tls.Config
	// self is this server's own name. Asking a resolver where this server lives is not a question
	// with a useful answer: the address a peer reaches it on and the address it should dial are
	// different things, and inside a container the second one is the interesting one.
	self    spec.ServerName
	timeout time.Duration
	// perName holds one transport per TLS server name. Cloning a transport is not free, and doing it
	// for every request to a peer that lives behind one address pool would be pure overhead.
	perName sync.Map
	// mu guards cache, which remembers where a name was last seen.
	mu    sync.Mutex
	cache map[string]resolutionEntry
}

// resolutionEntry is a remembered answer, with the time it stops being worth believing.
type resolutionEntry struct {
	results []fclient.ResolutionResult
	until   time.Time
}

// resolutionTTL and resolutionFailureTTL are how long a lookup is remembered. A peer that moves its
// federation listener is rare enough that five minutes of staleness is cheaper than a lookup per
// request; a peer that could not be found at all is worth retrying sooner, because the common reason
// is that it was still starting.
const (
	resolutionTTL        = 5 * time.Minute
	resolutionFailureTTL = time.Minute
	// resolutionBudget is how long discovery may take. Finding a peer is normally a few milliseconds
	// of DNS; the budget exists for the peer whose name resolution hangs, which must not be able to
	// hold a federation request open for the whole of its timeout.
	resolutionBudget = 5 * time.Second
)

func (t *matrixSchemeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL == nil {
		return t.next.RoundTrip(request)
	}
	target := request.URL
	if target.Scheme == "matrix" {
		clone := *target
		clone.Scheme = "https"
		target = &clone
	}
	// A name with no port would otherwise be dialled on 443, where no homeserver listens; the
	// resolver below takes care of the port when it does run, so this is only the fall-back for the
	// names it deliberately does not touch.
	if target.Port() == "" {
		withPort := *target
		withPort.Host = withPort.Hostname() + ":" + DefaultFederationPort
		target = &withPort
	}
	if request.URL.Port() != "" || isIPAddress(target.Hostname()) || spec.ServerName(target.Hostname()) == t.self {
		// Dialled as written. Rewrite the scheme and port, and go.
		return t.next.RoundTrip(clonedTo(request, target))
	}

	name := spec.ServerName(target.Hostname())
	results, err := t.resolveCached(request.Context(), name)
	if err != nil {
		return nil, fmt.Errorf("federation: finding where %s lives: %w", name, err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("federation: %s does not say where to reach it", name)
	}
	var lastErr error
	for _, result := range results {
		destination, err := url.Parse(target.String())
		if err != nil {
			return nil, fmt.Errorf("federation: %s", err)
		}
		destination.Host = result.Destination
		outgoing := clonedTo(request, destination)
		if result.Host != "" {
			outgoing.Host = string(result.Host)
		}
		roundTripper := t.forServerName(string(result.TLSServerName))
		resp, err := roundTripper.RoundTrip(outgoing)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("federation: asking %s at the address it gave (%s): %w", name, results[0].Destination, lastErr)
}

// resolveCached looks a name up unless it was looked up recently.
//
// The cache is this server's, not the resolver's: discovery costs a `.well-known` HTTPS request and
// SRV lookups, and federation asks the question on every request to a peer. Bounded by the peer
// request timeout, because a resolver that is wedged (a DNS server that swallows queries, a
// `.well-known` endpoint behind an overloaded load balancer) must not be able to park a federation
// connection for longer than the request itself was ever going to wait.
func (t *matrixSchemeRoundTripper) resolveCached(ctx context.Context, name spec.ServerName) ([]fclient.ResolutionResult, error) {
	t.mu.Lock()
	entry, fresh := t.cache[string(name)], false
	if entry.until.After(time.Now()) {
		fresh = true
	}
	t.mu.Unlock()
	if fresh {
		return entry.results, nil
	}

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	results, err := t.resolve(ctx, name)

	t.mu.Lock()
	if t.cache == nil {
		t.cache = map[string]resolutionEntry{}
	}
	ttl := resolutionFailureTTL
	if err == nil && len(results) > 0 {
		ttl = resolutionTTL
	}
	t.cache[string(name)] = resolutionEntry{results: results, until: time.Now().Add(ttl)}
	t.mu.Unlock()
	return results, err
}

// forServerName returns a transport that asks for `tlsServerName`'s certificate, which is the name
// rather than the address the name turned out to live on.
func (t *matrixSchemeRoundTripper) forServerName(tlsServerName string) http.RoundTripper {
	if tlsServerName == "" {
		return t.next
	}
	if cached, ok := t.perName.Load(tlsServerName); ok {
		if transport, ok := cached.(http.RoundTripper); ok {
			return transport
		}
	}
	base, ok := t.next.(*http.Transport)
	if !ok {
		return t.next
	}
	config := t.tls.Clone()
	config.ServerName = tlsServerName
	transport := base.Clone()
	transport.TLSClientConfig = config
	actual, _ := t.perName.LoadOrStore(tlsServerName, transport)
	if resolved, ok := actual.(http.RoundTripper); ok {
		return resolved
	}
	return t.next
}

// clonedTo returns a copy of the request pointed at a different URL, because a RoundTripper must not
// modify the request it was handed.
func clonedTo(request *http.Request, target *url.URL) *http.Request {
	clone := *request
	clone.URL = target
	return &clone
}

// isIPAddress reports whether a host is a bare address, which the specification treats like a name
// with a port: nothing to look up.
func isIPAddress(host string) bool {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return net.ParseIP(host) != nil
}

// signingIdentity assembles the identity gomatrixserverlib signs with.
//
// A missing key is an error rather than an unauthenticated client: an unsigned request to a peer is
// not a lesser form of the same request, it is a request that will be rejected with an error that
// points at the peer's configuration.
func signingIdentity(opts PeerOptions) (*fclient.SigningIdentity, error) {
	if len(opts.PrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("federation: no usable signing key was configured")
	}
	return &fclient.SigningIdentity{
		ServerName: opts.ServerName,
		KeyID:      gomatrixserverlib.KeyID(opts.KeyID),
		PrivateKey: opts.PrivateKey,
	}, nil
}
