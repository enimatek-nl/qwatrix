package fedapi

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
}

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
// The address is used as the server name says it: host, and port when the name carries one. A
// deployment whose peers rely on `_matrix._https` SRV records or a `.well-known` redirect needs that
// resolution added here; asking the name as written and failing loudly when it does not answer is
// the honest intermediate, and is what complement's container network expects.
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
	return &matrixSchemeRoundTripper{next: clone}, nil
}

// DefaultFederationPort is the port a peer is contacted on when its server name does not say.
const DefaultFederationPort = "8448"

// matrixSchemeRoundTripper turns a federation request into an HTTPS connection.
//
// gomatrixserverlib describes an outbound peer with its own `matrix://` scheme and normally installs
// its own transport to resolve that into a real address — SRV records, a `.well-known` redirect, the
// federation port. Passing a transport of our own (which TLS policy requires: an operator's CA, or
// the test-network escape hatch) replaces that resolver wholesale, and every request then fails with
// `unsupported protocol scheme "matrix"`.
//
// So this wrapper performs the part of that job this server's documented policy actually allows: the
// name is used as written, over HTTPS, with the specification's federation port when the name carries
// none. SRV and `.well-known` resolution remain unimplemented and are said so in the docs; what this
// removes is a failure mode where the code looked configurable but could not dial anything.
type matrixSchemeRoundTripper struct {
	next http.RoundTripper
}

func (t *matrixSchemeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL != nil {
		var target *url.URL
		rewrite := false
		if request.URL.Scheme == "matrix" {
			clone := *request.URL
			clone.Scheme = "https"
			target, rewrite = &clone, true
		} else {
			target = request.URL
		}
		// A server name with no port means the federation port, and this has to be applied to a URL
		// that already says "https" as well as to one that says "matrix": the outbound library converts
		// the scheme itself and does not fill the port in, so a peer called "matrix.org" or "hs1" —
		// which is every name that does not spell one out, so nearly all of them — would otherwise be
		// dialed on 443, where no homeserver is listening. A name that *does* carry a port is taken
		// exactly as written, which is the whole documented policy: no SRV, no .well-known, no
		// guessing.
		//
		// This transport only ever carries federation traffic, so "no port means the federation port"
		// cannot catch anything else; a deployment running its federation listener somewhere else has
		// to say so in the server name, and does.
		if target.Scheme == "https" && target.Port() == "" && target.Hostname() != "" {
			target.Host = target.Hostname() + ":" + DefaultFederationPort
			rewrite = true
		}
		if rewrite {
			// Cloned because a RoundTripper must not modify the request it was handed.
			rewritten := *request
			rewritten.URL = target
			request = &rewritten
		}
	}
	return t.next.RoundTrip(request)
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
