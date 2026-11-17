package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/tidwall/gjson"
)

// peerDirectory is a stand-in homeserver with a room directory and aliases, which remembers what it
// was asked. The recording is mutex-guarded because the request arrives on the server's own
// goroutine while the test is already asserting on it.
type peerDirectory struct {
	*fakePeer

	mu       sync.Mutex
	askedFor []string
	signed   bool
	// refuses lists path fragments this peer answers with "I do not serve this endpoint", which is
	// how a test reproduces a server that speaks only the older spelling, or only the public one.
	refuses []string
}

const (
	peerRoomOnFederation = "!resolved-by-federation"
	peerRoomOnClientAPI  = "!resolved-by-client-api"
	peerPublishedRoom    = "#found-on-peer"
)

func newPeerDirectory(t *testing.T) *peerDirectory {
	t.Helper()

	peer := &peerDirectory{fakePeer: newFakePeer(t)}
	peer.serves = func(w http.ResponseWriter, r *http.Request) bool {
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/_matrix/federation/v1/publicRooms"):
			peer.record(path, r)
			if peer.refusing(path) {
				peer.refused(t, w)
				return true
			}
			answerPeerJSON(t, w, map[string]any{
				"chunk": []map[string]any{{
					"room_id":            "!found:" + peer.host(),
					"canonical_alias":    peerPublishedRoom + ":" + peer.host(),
					"name":               "found on peer",
					"num_joined_members": 7,
					"world_readable":     false,
					"guest_can_join":     false,
					"join_rule":          "public",
				}},
				"total_room_count_estimate": 1,
			})
		case strings.HasPrefix(path, "/_matrix/federation/v1/resolve/"):
			peer.record(path, r)
			if peer.refusing(path) {
				peer.refused(t, w)
				return true
			}
			answerPeerJSON(t, w, map[string]any{
				"origin": peer.host(),
				"rooms":  []string{peerRoomOnFederation + ":" + peer.host()},
			})
		case strings.HasPrefix(path, "/_matrix/federation/v1/query/directory"):
			peer.record(path, r)
			if peer.refusing(path) {
				peer.refused(t, w)
				return true
			}
			answerPeerJSON(t, w, map[string]any{
				"room_id": peerRoomOnFederation + ":" + peer.host(),
				"servers": []string{peer.host()},
			})
		case strings.HasPrefix(path, "/_matrix/client/v3/directory/room/"):
			peer.record(path, r)
			answerPeerJSON(t, w, map[string]any{
				"room_id": peerRoomOnClientAPI + ":" + peer.host(),
				"servers": []string{peer.host()},
			})
		case strings.Contains(path, "/make_join/"):
			// The join is not what this file is testing; refusing here stops it just after it has
			// proved that the alias became the room it was supposed to.
			peer.record(path, r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			answerPeerJSON(t, w, map[string]any{
				"errcode": "M_NOT_FOUND", "error": "the test peer holds nothing",
			})
		default:
			return false
		}
		return true
	}
	return peer
}

func (p *peerDirectory) host() string { return string(p.serverNo) }

func (p *peerDirectory) refusing(path string) bool {
	for _, refusal := range p.refuses {
		if strings.Contains(path, refusal) {
			return true
		}
	}
	return false
}

func (p *peerDirectory) record(path string, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.askedFor = append(p.askedFor, path)
	if strings.HasPrefix(r.Header.Get("Authorization"), "X-Matrix") {
		p.signed = true
	}
}

func (p *peerDirectory) paths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.askedFor...)
}

func (p *peerDirectory) wasAsked(substring string) bool {
	for _, path := range p.paths() {
		if strings.Contains(path, substring) {
			return true
		}
	}
	return false
}

func (p *peerDirectory) askedSigned() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.signed
}

// refused answers the way the specification says a server answers an endpoint it does not implement:
// a 404 whose error code says the request was not recognised. Answering it with a 200 and an error
// body would be a different bug, and one this peer must not invent.
func (p *peerDirectory) refused(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	answerPeerJSON(t, w, map[string]any{
		"errcode": "M_UNRECOGNIZED", "error": "this peer does not serve that endpoint",
	})
}

func answerPeerJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the peer's answer: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// TestAnotherServersRoomDirectoryIsBrowsedThroughThisOne is the explore-pane bug.
//
// Element puts a server name in a box and asks this server what *that* server publishes. Ignoring the
// parameter and answering with this server's own list is not a smaller answer, it is an answer about
// a different thing: the room the user was told about is nowhere in it, and the pane says the server
// has no rooms.
func TestAnotherServersRoomDirectoryIsBrowsedThroughThisOne(t *testing.T) {
	peer := newPeerDirectory(t)
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "browser", "pw-browser", false)

	for _, form := range []struct{ name, path string }{
		{"GET with the specification's parameter", "?server_of_interest="},
		{"GET with the parameter clients send", "?server="},
	} {
		response := alice.MustGet(t, server,
			"/_matrix/client/v3/publicRooms"+form.path+peer.host()+"&limit=20").MustStatus(t, http.StatusOK)
		if chunk := gjson.GetBytes(response.Body, "chunk"); !strings.Contains(chunk.Raw, peerPublishedRoom) {
			t.Errorf("%s: the peer's room is missing\nchunk: %s", form.name, chunk.Raw)
		}
	}

	posted := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/publicRooms",
		Body:   map[string]any{"server": peer.host(), "limit": 20},
	}).MustStatus(t, http.StatusOK)
	if body := string(posted.Body); !strings.Contains(body, peerPublishedRoom) {
		t.Errorf("the POST form did not carry the peer's room either\n%s", body)
	}

	if !peer.wasAsked("/_matrix/federation/v1/publicRooms") {
		t.Errorf("the peer was never asked; this server answered its own list for %q\nasked: %v",
			peer.host(), peer.paths())
	}
	if !peer.askedSigned() {
		t.Error("the directory question was unsigned, which no real peer answers")
	}
}

// TestAnotherServersDirectoryIsNotSilentlyThisServersOwn covers the tempting shortcut in the fix: a
// remote directory that cannot be fetched must not fall back to the local list, because the local
// list is a confident statement about the wrong server.
func TestAnotherServersDirectoryIsNotSilentlyThisServersOwn(t *testing.T) {
	peer := newPeerDirectory(t)
	peer.refuses = []string{"/_matrix/federation/v1/publicRooms"}
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "browser2", "pw-browser2", false)

	// A published room of this server's own, so that a silent fallback would show it.
	alice.MustPostJSON(t, server, "/_matrix/client/v3/createRoom", map[string]any{
		"name": "mine", "preset": "public_chat", "visibility": "public",
	})

	response := alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/publicRooms?server_of_interest=" + peer.host(),
	})
	body := string(response.Body)
	if response.StatusCode == http.StatusOK {
		t.Errorf("a directory this server could not fetch was answered as if it were empty\n%s", body)
	}
	if strings.Contains(body, "mine") {
		t.Errorf("this server's own rooms were passed off as the peer's answer\n%s", body)
	}
}

// TestTheThirdPartyProtocolListIsAnswered pins the endpoint Element Web asks *before* it asks for
// rooms, and which therefore decides whether its directory pane renders at all.
//
// The browser console read `M_UNRECOGNIZED: GET /_matrix/client/v3/thirdparty/protocols`, thrown from
// the same hook that renders the list. This server bridges no third-party networks, so an empty list
// is the honest answer — and an empty list completes, where a 404 raises an exception.
func TestTheThirdPartyProtocolListIsAnswered(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "protocols", "pw-protocols", false)

	for _, version := range []string{"v3", "v1"} {
		path := "/_matrix/client/" + version + "/thirdparty/protocols"
		response := alice.MustGet(t, server, path).MustStatus(t, http.StatusOK)
		if got := gjson.GetBytes(response.Body, "instances"); !got.IsArray() || len(got.Array()) != 0 {
			t.Errorf("%s: instances should be an empty array, got %s", path, got.Raw)
		}
	}
}

// TestAnAliasOnAnotherServerIsResolvedBeforeJoining is the `#room:elsewhere` join.
//
// A join that names an alias has to learn which room that is, and only the server the alias carries
// knows. Until this, an alias missing from the local table ended the enquiry, so joining anybody
// else's alias failed with this server's own "no such alias" — which a user cannot tell apart from a
// room that has been deleted.
func TestAnAliasOnAnotherServerIsResolvedBeforeJoining(t *testing.T) {
	peer := newPeerDirectory(t)
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "joiner", "pw-joiner", false)

	alias := "#wanted:" + peer.host()
	response := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/join/" + url.PathEscape(alias),
		Body:   map[string]any{},
	})
	if !peer.wasAsked("/_matrix/federation/v1/resolve/") {
		t.Errorf("the alias was never put to the server that owns it\njoin said: %d %s\nasked: %v",
			response.StatusCode, response.Body, peer.paths())
	}
	if !peer.wasAsked("make_join") || !peer.wasAsked(peerRoomOnFederation) {
		t.Errorf("the join did not go on to ask for the room the alias names\nasked: %v\njoin said: %d %s",
			peer.paths(), response.StatusCode, response.Body)
	}
}

// TestAnAliasIsStillResolvedWhenFederationDoesNotServeIt is for a server that answers its own aliases
// over its client API and never implemented federation's directory endpoints.
//
// The specification makes an unsupported endpoint a 404 M_UNRECOGNIZED, which is a statement about
// the peer's code and not about the room. Reading it as "no such room" tells the user the room does
// not exist, which is more than this server can know.
func TestAnAliasIsStillResolvedWhenFederationDoesNotServeIt(t *testing.T) {
	peer := newPeerDirectory(t)
	peer.refuses = []string{"/_matrix/federation/v1/resolve/", "/_matrix/federation/v1/query/directory"}
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "joiner2", "pw-joiner2", false)

	alias := "#wanted:" + peer.host()
	response := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/join/" + url.PathEscape(alias),
		Body:   map[string]any{},
	})
	if !peer.wasAsked("/_matrix/client/v3/directory/room/") {
		t.Errorf("the peer's own published alias lookup was never tried\nasked: %v\njoin said: %d %s",
			peer.paths(), response.StatusCode, response.Body)
	}
	if !peer.wasAsked(peerRoomOnClientAPI) {
		t.Errorf("the room resolved from the peer's client API was not used for the join\nasked: %v",
			peer.paths())
	}
}

// TestThisServerTellsAPeerWhatItPublishesAndWhatItsAliasesMean is the same door from the other side.
//
// A room this server calls public is discoverable exactly as far as another server can learn of it,
// and the peer's two questions are this one's room directory and its alias table.
func TestThisServerTellsAPeerWhatItPublishesAndWhatItsAliasesMean(t *testing.T) {
	peer := newFakePeer(t)
	server := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	alice := server.Register(t, "publisher", "pw-publisher", false)

	created := alice.MustPostJSON(t, server, "/_matrix/client/v3/createRoom", map[string]any{
		"name":            "worth finding",
		"room_alias_name": "worth-finding",
		"preset":          "public_chat",
		"visibility":      "public",
	})
	roomID := gjson.GetBytes(created.Body, "room_id").String()
	alias := "#worth-finding:" + server.ServerName

	chunk, _ := json.Marshal(peerAsks(t, server, peer, "/_matrix/federation/v1/publicRooms?limit=50")["chunk"])
	if !strings.Contains(string(chunk), roomID) {
		t.Errorf("a peer asking what this server publishes was not told about the room\nchunk: %s", chunk)
	}

	resolved, _ := json.Marshal(peerAsks(t, server, peer,
		"/_matrix/federation/v1/resolve/"+url.PathEscape(alias))["rooms"])
	if !strings.Contains(string(resolved), roomID) {
		t.Errorf("an alias of this server did not resolve for a peer\nrooms: %s", resolved)
	}

	legacy := peerAsks(t, server, peer,
		"/_matrix/federation/v1/query/directory?room_alias="+url.PathEscape(alias))
	if legacy["room_id"] != roomID {
		t.Errorf("the older alias lookup answered %v, want %s", legacy["room_id"], roomID)
	}
}

// TestThisServerDoesNotResolveSomebodyElsesAlias keeps this server out of the middle.
//
// Answering for `#room:somewhere.else` would mean repeating what another server said, on demand from
// any peer that can sign a request, and routing a stranger's enquiry through this server's name.
func TestThisServerDoesNotResolveSomebodyElsesAlias(t *testing.T) {
	peer := newFakePeer(t)
	server := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())

	response := peerRequest(t, server, peer, http.MethodGet,
		"/_matrix/federation/v1/resolve/"+url.PathEscape("#not-ours:anywhere.example"), nil)
	defer func() { _ = response.Body.Close() }()
	answer := jsonOf(t, response.Body)
	if response.StatusCode == http.StatusOK {
		t.Fatal("this server answered for an alias that belongs to another server")
	}
	if answer["errcode"] != "M_NOT_FOUND" {
		t.Errorf("refusing a foreign alias got %v, want M_NOT_FOUND: %v", answer["errcode"], answer)
	}
}

// TestThisServersOwnDirectoryIsUnchangedWhenNobodyElseIsNamed guards the ordinary case against the
// new branch: no server named, this server's own list, exactly as before — and naming *this* server
// is the same question rather than one to send over the wire.
func TestThisServersOwnDirectoryIsUnchangedWhenNobodyElseIsNamed(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "own", "pw-own", false)
	alice.MustPostJSON(t, server, "/_matrix/client/v3/createRoom", map[string]any{
		"name": "plain and local", "preset": "public_chat", "visibility": "public",
	})

	for _, path := range []string{
		"/_matrix/client/v3/publicRooms",
		"/_matrix/client/v3/publicRooms?server=" + server.ServerName,
	} {
		response := alice.MustGet(t, server, path).MustStatus(t, http.StatusOK)
		if body := string(response.Body); !strings.Contains(body, "plain and local") {
			t.Errorf("%s lost this server's own rooms\n%s", path, body)
		}
	}
}
