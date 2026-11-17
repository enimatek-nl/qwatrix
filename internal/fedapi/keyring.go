package fedapi

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/identity"
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
			//
			// The reason is logged because it is otherwise destroyed here, and the caller's
			// message — "could not download key" — names the server and nothing else. Whether the
			// peer was unreachable, answered without the key, or published a document that would not
			// verify are three different faults belonging to three different machines, and a federation
			// link that does not work is diagnosed by which of the three it was.
			slog.Warn("fedapi: could not fetch a peer's signing keys",
				"server", server, "error", err)
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

// perspectiveFetch asks a key server about a peer that can no longer speak for itself.
//
// A server's signing key is published by that server, which is why [notaryFetch] is the only source
// this server wants to believe about a live peer. But a server that has stopped existing publishes
// nothing, and the events it signed stay in other people's rooms for as long as those rooms do — and
// rooms outlive servers routinely: a domain drops its address, a deployment is abandoned, and the
// membership event of a user who was there in 2021 is still part of the state of a room somebody tries
// to join today. With the origin as the only source, that one unresolvable key makes the room
// permanently unjoinable for this server and every event that server signed permanently unverifiable
// for it, which is a slow way of discovering that the federation graph is older than this server's
// willingness to remember it.
//
// The specification's answer is a perspective key server: it answers `POST /_matrix/key/v2/query` with
// the key documents it has cached, and a cached document is still the document the peer published —
// signed by the very key it names. That is what keeps a key server from being asked to do the impossible:
// it cannot hand over a key that verifies something its owner never signed, and it cannot speak over a
// live peer, because a peer answering for itself is always asked first and always wins.
//
// What a key server can do is lie about a server that is no longer around to contradict it, and that is
// the trust this fallback places — the same one every deployment that ships a perspective server places.
// It is why the setting exists rather than being hardcoded on: an operator who would rather watch those
// rooms stay unjoinable sets `-key-perspectives` empty and keeps this server able to believe only what a
// peer says about itself while it is alive.
//
// One rule from [notaryFetch] cannot come along. Refusing a document that has expired against the
// clock is right when the server is speaking about itself and wrong when a key server is speaking about
// a dead one: the answer about a server that stopped publishing in 2021 is *required* to be old. What
// has to hold instead is that the document covered the moment the signature was made, which is what the
// caller's timestamp asks for — and gomatrixserverlib re-checks it per event, so a retired key still
// cannot be used to authenticate something signed after it was retired, and a stale key cannot be used
// to authenticate a live request.
type perspectiveFetch struct {
	peer *Peer
	// notaries are asked in order, and the first one that answers wins.
	notaries []spec.ServerName
}

func (f *perspectiveFetch) FetcherName() string { return "qwatrix-key-perspective" }

// FetchKeys implements gomatrixserverlib.KeyFetcher.
func (f *perspectiveFetch) FetchKeys(ctx context.Context, requests map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp) (map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult, error) {
	out := map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult{}
	if f.peer == nil || len(f.notaries) == 0 || len(requests) == 0 {
		return out, nil
	}
	for _, notary := range f.notaries {
		for request, result := range f.ask(ctx, notary, requests, requests) {
			out[request] = result
		}
		if stillWanted := stillMissing(requests, out); len(stillWanted) > 0 {
			// Ask the same question without naming the keys. A key server's cache is addressed by key
			// ID, so "I have nothing for that ID" is a statement about an ID and not about the server
			// being asked about — and a key server asked an open question answers with everything it
			// remembers, which is the difference between a key that was never cached and a server that
			// was never seen. One extra request, and only where the first one came back empty.
			for request, result := range f.ask(ctx, notary, openQuestion(stillWanted), stillWanted) {
				out[request] = result
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return out, nil
}

// ask poses one query to one key server and returns the answers that may be believed.
//
// `posed` is what goes over the wire and `wanted` is what the caller needs, which are the same question
// until the second pass above opens it up: an answer is only ever used for a lookup that asked for it,
// and never for one it merely resembles.
func (f *perspectiveFetch) ask(ctx context.Context, notary spec.ServerName,
	posed map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp,
	wanted map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp,
) map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult {
	out := map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult{}
	documents, err := f.peer.LookupServerKeys(ctx, notary, posed)
	if err != nil {
		// A key server that is down is a key server that did not help, and there may be another one
		// behind it — so this is logged, not returned. The reason is kept here for the same reason
		// [notaryFetch] keeps it: the caller's message names the server whose key was wanted and says
		// nothing about who was asked, or about what they said back.
		slog.Warn("fedapi: a key server did not answer",
			"notary", notary, "error", err)
		return out
	}
	for _, document := range documents {
		if !askedAbout(document.ServerName, wanted) {
			// An answer about a server nobody asked about is not an answer, and believing it would let
			// one key server introduce a signing key under any name it chose. It is worth a line
			// because it is not a thing a correct key server does.
			slog.Warn("fedapi: a key server answered about a server this lookup did not ask about",
				"notary", notary, "server", document.ServerName)
			continue
		}
		if err := checkKeyDocument(document.ServerName, document); err != nil {
			slog.Warn("fedapi: a key server gave an unusable key document",
				"notary", notary, "server", document.ServerName, "error", err)
			continue
		}
		for request, until := range wanted {
			if request.ServerName != document.ServerName {
				continue
			}
			if result, ok := perspectiveResult(document, request.KeyID, until); ok {
				out[request] = result
			}
		}
	}
	return out
}

// stillMissing is the part of a lookup no answer has satisfied yet.
func stillMissing(wanted map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp,
	found map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult,
) map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp {
	out := map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp{}
	for request, until := range wanted {
		if _, ok := found[request]; !ok {
			out[request] = until
		}
	}
	return out
}

// openQuestion is the same lookup with the key IDs taken out of it: one question per server, asking what
// ever is known about it, needing to cover at least as far ahead as the furthest requirement left.
func openQuestion(wanted map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp) map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp {
	out := map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp{}
	for request, until := range wanted {
		open := gomatrixserverlib.PublicKeyLookupRequest{ServerName: request.ServerName}
		if held, seen := out[open]; !seen || until > held {
			out[open] = until
		}
	}
	return out
}

// askedAbout reports whether a document is about one of the servers this lookup was for.
func askedAbout(server spec.ServerName, requests map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp) bool {
	for request := range requests {
		if request.ServerName == server {
			return true
		}
	}
	return false
}

// checkKeyDocument decides whether a key document may be believed when it did not arrive over a
// connection to the server it names.
//
// [CheckKeyResponse] asks the stricter question, and its strictness is about the channel: believing a
// server that speaks about itself is only safe because TLS says who answered, which is why "you
// published no current keys" is a refusal there. A key server speaks about somebody else, and the
// interesting answer about a dead one is exactly the answer that is no longer current — so what is left
// to check is everything the connection was not carrying: the document must name the server this lookup
// is about, it must hold at least one key, and every key in it, current or retired, must have signed
// the document it appears in.
func checkKeyDocument(server spec.ServerName, keys gomatrixserverlib.ServerKeys) error {
	if keys.ServerName != server {
		return fmt.Errorf("the key document names %s, not the server %s this lookup was for",
			keys.ServerName, server)
	}
	if len(keys.VerifyKeys)+len(keys.OldVerifyKeys) == 0 {
		return fmt.Errorf("%s published no signing keys", server)
	}
	for keyID, entry := range keys.VerifyKeys {
		if err := gomatrixserverlib.VerifyJSON(string(server), keyID,
			xed25519.PublicKey(entry.Key), keys.Raw); err != nil {
			return fmt.Errorf("%s published a key that does not sign its own key document: %w", server, err)
		}
	}
	for keyID, entry := range keys.OldVerifyKeys {
		if err := gomatrixserverlib.VerifyJSON(string(server), keyID,
			xed25519.PublicKey(entry.Key), keys.Raw); err != nil {
			return fmt.Errorf("%s published a retired key that does not sign its own key document: %w", server, err)
		}
	}
	return nil
}

// perspectiveResult picks the one key a lookup asked for out of a key server's document, and reports
// whether it may be believed for the moment the caller needs to check.
func perspectiveResult(document gomatrixserverlib.ServerKeys, keyID gomatrixserverlib.KeyID,
	until spec.Timestamp) (gomatrixserverlib.PublicKeyLookupResult, bool) {
	if entry, ok := document.VerifyKeys[keyID]; ok {
		// A current key has to run at least as far as the caller needs: an event must be verifiable at
		// the moment it was made, and a signed request at the moment it was signed. How far ahead is the
		// caller's question, and gomatrixserverlib re-asks it per event.
		if document.ValidUntilTS == gomatrixserverlib.PublicKeyNotValid || document.ValidUntilTS < until {
			return gomatrixserverlib.PublicKeyLookupResult{}, false
		}
		return gomatrixserverlib.PublicKeyLookupResult{
			VerifyKey:    entry,
			ExpiredTS:    gomatrixserverlib.PublicKeyNotExpired,
			ValidUntilTS: document.ValidUntilTS,
		}, true
	}
	if entry, ok := document.OldVerifyKeys[keyID]; ok {
		// A retired key is published for exactly this purpose — the historic event that cannot be checked
		// any other way — and it is handed on as what it is, so that it is believed only for a signature
		// made before it was retired.
		return gomatrixserverlib.PublicKeyLookupResult{
			VerifyKey:    entry.VerifyKey,
			ExpiredTS:    entry.ExpiredTS,
			ValidUntilTS: gomatrixserverlib.PublicKeyNotValid,
		}, entry.ExpiredTS != gomatrixserverlib.PublicKeyNotExpired
	}
	return gomatrixserverlib.PublicKeyLookupResult{}, false
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

// NewKeyRing builds the verifier that checks peer signatures: keys are read from the cache, then
// fetched from the peer that signed the document, and then — if key servers were named — asked of those.
//
// The order is the whole policy. A peer speaking about itself, over a connection whose certificate the
// transport vouched for, outranks a third party talking about it; the third party is worth asking only
// when the peer cannot answer, which is the case the specification's perspective servers exist for.
// Naming no key server leaves this server able to believe only what its peers publish while they are
// alive, and unable to join a room that holds an event from a server that has gone away.
//
// The identity may be nil for a deployment that has no signing key — a homeserver not federating — in
// which case only this server's own key becomes unresolvable, which is the least it can get away with.
func NewKeyRing(peer *Peer, id *identity.Identity, perspectives ...string) *gomatrixserverlib.KeyRing {
	fetchers := make([]gomatrixserverlib.KeyFetcher, 0, 3)
	if id != nil {
		fetchers = append(fetchers, &localKeys{id: id})
	}
	if peer != nil {
		fetchers = append(fetchers, &notaryFetch{peer: peer, now: time.Now})
		if notaries := keyServerNames(perspectives); len(notaries) > 0 {
			fetchers = append(fetchers, &perspectiveFetch{peer: peer, notaries: notaries})
		}
	}
	return &gomatrixserverlib.KeyRing{
		KeyFetchers: fetchers,
		KeyDatabase: newKeyCache(),
	}
}

// keyServerNames reads the configured key servers into the form the fetchers want, dropping the blanks
// that an empty setting and a trailing comma both leave behind, and any repeat of a name.
func keyServerNames(names []string) []spec.ServerName {
	out := make([]spec.ServerName, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, spec.ServerName(name))
	}
	return out
}

// localKeys answers key lookups for this server's own name out of its identity.
//
// A key ring that only knows how to ask the network cannot answer "who is this server" without a round
// trip — and the question is not hypothetical. A peer hands back an event this server signed (a join
// this server made, reflected in the room's state), and every reader re-checks every signature in
// front of it. This server should never have to ask anybody, including itself over the network, who it
// is: the answer is the key it is holding, and a deployment whose own key endpoint is briefly
// unreachable would otherwise refuse its own history.
type localKeys struct {
	id *identity.Identity
}

func (l *localKeys) FetchKeys(_ context.Context,
	requests map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp,
) (map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult, error) {
	out := map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult{}
	for request := range requests {
		if request.ServerName != l.id.ServerName || request.KeyID != l.id.KeyID {
			continue
		}
		out[request] = gomatrixserverlib.PublicKeyLookupResult{
			VerifyKey: gomatrixserverlib.VerifyKey{
				Key: spec.Base64Bytes(l.id.PublicKey()),
			},
			ExpiredTS:    gomatrixserverlib.PublicKeyNotExpired,
			ValidUntilTS: l.id.ValidUntil(),
		}
	}
	return out, nil
}

func (l *localKeys) FetcherName() string { return "qwatrix-local-identity" }
