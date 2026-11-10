package fedapi

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	xed25519 "golang.org/x/crypto/ed25519"
)

// keyCacheTTL is how long a fetched peer key may be reused when the peer itself gave no reason to
// keep it longer. A key response is valid until its own `valid_until_ts`, which is typically days;
// this only bounds the case where a peer says something shorter than nothing useful.
const keyCacheTTL = 5 * time.Minute

// keyCache is the in-memory cache of peer signing keys.
//
// It is in-process rather than in the database on purpose. Peer keys are cheap to refetch, they
// expire on their own schedule, and a restart that forgets them costs one request per peer — which
// is a smaller price than a table of other people's credentials sitting in this server's database
// with no expiry story of its own.
type keyCache struct {
	mu   sync.Mutex
	seen map[gomatrixserverlib.PublicKeyLookupRequest]cachedKey
}

type cachedKey struct {
	result  gomatrixserverlib.PublicKeyLookupResult
	expires time.Time
}

func newKeyCache() *keyCache {
	return &keyCache{seen: map[gomatrixserverlib.PublicKeyLookupRequest]cachedKey{}}
}

// FetchKeys implements gomatrixserverlib.KeyDatabase.
func (c *keyCache) FetchKeys(_ context.Context, requests map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp) (map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult{}
	now := time.Now()
	for request, wantedUntil := range requests {
		cached, ok := c.seen[request]
		if !ok || now.After(cached.expires) {
			continue
		}
		// A cached key shorter-lived than the caller needs is no answer at all: an event must be
		// verifiable at the moment it was received, and handing back a key that stopped covering
		// that instant would turn a cache hit into a false rejection.
		if cached.result.ValidUntilTS != gomatrixserverlib.PublicKeyNotValid && cached.result.ValidUntilTS < wantedUntil {
			continue
		}
		out[request] = cached.result
	}
	return out, nil
}

// StoreKeys implements gomatrixserverlib.KeyDatabase.
func (c *keyCache) StoreKeys(_ context.Context, results map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for request, result := range results {
		expires := now.Add(keyCacheTTL)
		if result.ValidUntilTS > 0 {
			if until := time.UnixMilli(int64(result.ValidUntilTS)); until.After(expires) {
				expires = until
			}
		}
		c.seen[request] = cachedKey{result: result, expires: expires}
	}
	return nil
}

// FetcherName implements gomatrixserverlib.KeyFetcher.
func (c *keyCache) FetcherName() string { return "qwatrix-key-cache" }

// notaryFetch is the fetcher that asks a peer for its own signing keys.
//
// The keys are taken from the origin server rather than from a notary this server has been
// configured to trust, because that is what the peer-to-peer trust model says: a server vouches for
// itself with a signature, and the certificate of the connection is what stops the answer being
// forged. The consequence is that a fetched key response must be self-verified before it is
// believed, which is what fetch does.
type notaryFetch struct {
	peer *Peer
	// now is injected so a test can stand still while a key expires.
	now func() time.Time
}

func (f *notaryFetch) FetcherName() string { return "qwatrix-peer-notary" }

// FetchKeys implements gomatrixserverlib.KeyFetcher.
func (f *notaryFetch) FetchKeys(ctx context.Context, requests map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp) (map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult, error) {
	// One request per peer, whatever the number of keys asked for: a peer publishes all of its
	// keys in one document, and asking twice for the same document is both slower and a way to
	// look like a denial-of-service attempt.
	perServer := map[spec.ServerName][]gomatrixserverlib.PublicKeyLookupRequest{}
	for request := range requests {
		perServer[request.ServerName] = append(perServer[request.ServerName], request)
	}
	servers := make([]spec.ServerName, 0, len(perServer))
	for server := range perServer {
		servers = append(servers, server)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i] < servers[j] })

	out := map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult{}
	for _, server := range servers {
		fetched, err := f.fetch(ctx, server, requests, perServer[server])
		if err != nil {
			// One unreachable peer must not withhold the keys of the others: a caller
			// verifying events from three servers should still be able to believe two.
			continue
		}
		for request, result := range fetched {
			out[request] = result
		}
	}
	return out, nil
}

// fetch asks one peer for its keys and returns only the ones the caller asked for.
func (f *notaryFetch) fetch(
	ctx context.Context,
	server spec.ServerName,
	wanted map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp,
	askedFor []gomatrixserverlib.PublicKeyLookupRequest,
) (map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult, error) {
	// Ask for validity at least as far ahead as the furthest requirement: a key that stops being
	// valid before the caller needs it is a wasted round trip.
	var minimum spec.Timestamp
	for _, request := range askedFor {
		if wanted[request] > minimum {
			minimum = wanted[request]
		}
	}

	var keys gomatrixserverlib.ServerKeys
	if err := f.peer.GetJSON(ctx, server, "/_matrix/key/v2/server", &keys); err != nil {
		return nil, fmt.Errorf("fetching signing keys from %s: %w", server, err)
	}
	if err := CheckKeyResponse(server, keys); err != nil {
		return nil, err
	}
	if keys.ValidUntilTS > 0 && keys.ValidUntilTS < minimum {
		return nil, fmt.Errorf("%s published keys valid only until %s, which is not far enough ahead",
			server, time.UnixMilli(int64(keys.ValidUntilTS)))
	}
	at := f.now()
	if keys.ValidUntilTS > 0 && spec.Timestamp(at.UnixMilli()) > keys.ValidUntilTS {
		return nil, fmt.Errorf("%s published keys that expired at %s", server, time.UnixMilli(int64(keys.ValidUntilTS)))
	}

	out := map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult{}
	for keyID, verifyKey := range keys.VerifyKeys {
		request := gomatrixserverlib.PublicKeyLookupRequest{ServerName: server, KeyID: keyID}
		if _, wanted := wanted[request]; !wanted {
			continue
		}
		// The document must be signed by the key it publishes. Without this check a peer could be
		// replaced in transit and every event it ever "signed" would verify against the attacker's
		// key.
		if err := gomatrixserverlib.VerifyJSON(string(server), keyID,
			xed25519.PublicKey(verifyKey.Key), keys.Raw); err != nil {
			return nil, fmt.Errorf("%s published a key that does not sign its own key response: %w", server, err)
		}
		out[request] = gomatrixserverlib.PublicKeyLookupResult{
			VerifyKey:    verifyKey,
			ExpiredTS:    gomatrixserverlib.PublicKeyNotExpired,
			ValidUntilTS: keys.ValidUntilTS,
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s published none of the keys we asked for", server)
	}
	return out, nil
}

// VerifyRequest builds a peer signature check with the specification's validity rule attached.
//
// The validity function is not optional: gomatrixserverlib calls it, and a request built without it
// panics rather than failing. Applying it by construction means no caller can check a signature
// while silently skipping the rule that a key stops counting seven days after it was published —
// the cap that limits how far a stolen key can reach back into the past.
func VerifyRequest(server spec.ServerName, at time.Time, message []byte) gomatrixserverlib.VerifyJSONRequest {
	return gomatrixserverlib.VerifyJSONRequest{
		ServerName:           server,
		AtTS:                 spec.Timestamp(at.UnixMilli()),
		Message:              message,
		ValidityCheckingFunc: gomatrixserverlib.StrictValiditySignatureCheck,
	}
}

// CheckKeyResponse decides whether a peer's published key document may be believed, without
// touching the network.
//
// Two things have to hold. The document must name the server we asked, because anything able to
// answer on an address can otherwise claim to be a different server. And every key it publishes
// must have signed the document itself: that self-signature is what makes the answer attributable
// to whoever holds the private half, and without it an attacker who can answer the request could
// publish a key of their own and have every event they "signed" believed.
func CheckKeyResponse(server spec.ServerName, keys gomatrixserverlib.ServerKeys) error {
	if keys.ServerName != server {
		return fmt.Errorf("the key response names %s, not the peer %s we asked", keys.ServerName, server)
	}
	if len(keys.VerifyKeys) == 0 {
		return fmt.Errorf("%s published no signing keys", server)
	}
	for keyID, verifyKey := range keys.VerifyKeys {
		if err := gomatrixserverlib.VerifyJSON(string(server), keyID,
			xed25519.PublicKey(verifyKey.Key), keys.Raw); err != nil {
			return fmt.Errorf("%s published a key that does not sign its own key response: %w", server, err)
		}
	}
	return nil
}

// NewKeyRing builds the verifier that checks peer signatures: keys are read from the cache, and on
// a miss fetched from the peer that signed the document.
func NewKeyRing(peer *Peer) *gomatrixserverlib.KeyRing {
	cache := newKeyCache()
	return &gomatrixserverlib.KeyRing{
		KeyFetchers: []gomatrixserverlib.KeyFetcher{&notaryFetch{peer: peer, now: time.Now}},
		KeyDatabase: cache,
	}
}
