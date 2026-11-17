package api_test

// A server's signing key is published by that server, which is a rule that works exactly as long as the
// server does. Rooms outlive servers: a domain drops its address, a deployment is abandoned, and the
// membership event of somebody who joined in 2021 stays part of the state of a room somebody tries to
// join today. With the origin as the only source of keys, that one unresolvable key makes the room
// permanently unjoinable and every event that server signed permanently unverifiable.
//
// So these tests are about the second source the specification provides — a perspective key server,
// which hands back the key documents it cached while the peer was still publishing them — and about the
// boundary of what it may be believed to do. It may answer for a peer that cannot answer. It may not
// answer for a server nobody asked about, publish a document the keys inside it do not sign, or make a
// key that stopped being valid speak for the present. And when no key server is named, this server asks
// nobody: the peer's own answer is the only one there is.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/fedapi"
	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// goneServerNo is a server name that answers nothing. It carries a port, which is what stops the
// federation resolver looking for a well-known redirect or an SRV record and dialling exactly this
// address instead: connection refused, in the time it takes to fail.
const goneServerNo = spec.ServerName("127.0.0.1:1")

// goneServer is a homeserver that has stopped existing. What is left of it is its signing key and the
// documents it signed while it was up — and a key whose validity ended in the past, because a server that
// has been gone for a while does not publish a fresh one.
type goneServer struct {
	serverNo spec.ServerName
	keyID    gomatrixserverlib.KeyID
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	// signedAt is a moment this server was still around; validUntil is when its key stopped covering
	// anything new. Both are in the past by design: a signature made while the server lived is the case
	// the fallback exists for, and a signature claimed for the present is not.
	signedAt   time.Time
	validUntil time.Time
}

func newGoneServer(t *testing.T) *goneServer {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the gone server's key: %v", err)
	}
	return &goneServer{
		serverNo:   goneServerNo,
		keyID:      gomatrixserverlib.KeyID("ed25519:was-here"),
		priv:       priv,
		pub:        pub,
		signedAt:   time.Now().Add(-time.Hour),
		validUntil: time.Now().Add(-time.Hour / 2),
	}
}

// document is something this server signed while it was still signing things.
func (g *goneServer) document(t *testing.T) []byte {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"hello": "world", "signed_by": "the-gone-server"})
	if err != nil {
		t.Fatalf("encoding the document: %v", err)
	}
	signed, err := gomatrixserverlib.SignJSON(string(g.serverNo), g.keyID, g.priv, raw)
	if err != nil {
		t.Fatalf("signing the document: %v", err)
	}
	return signed
}

// keyDocument is the document this server published for its key — the one a key server is being asked to
// remember. Retiring it moves the key out of `verify_keys` and into `old_verify_keys`, which is how a
// server that rotated its key kept the old one reachable for the events it had already signed.
func (g *goneServer) keyDocument(t *testing.T, retired bool) json.RawMessage {
	t.Helper()

	key := map[string]any{"key": encodeBase64(g.pub)}
	body := map[string]any{
		"server_name": string(g.serverNo),
	}
	if retired {
		key["expired_ts"] = spec.Timestamp(g.validUntil.UnixMilli())
		body["verify_keys"] = map[string]any{}
		body["old_verify_keys"] = map[string]any{string(g.keyID): key}
		// The document itself stays current: the server published it before it went, and a document
		// that has expired says nothing about the keys inside it having expired with it.
		body["valid_until_ts"] = spec.Timestamp(time.Now().Add(time.Hour).UnixMilli())
	} else {
		body["verify_keys"] = map[string]any{string(g.keyID): key}
		body["old_verify_keys"] = map[string]any{}
		body["valid_until_ts"] = spec.Timestamp(g.validUntil.UnixMilli())
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the key document: %v", err)
	}
	signed, err := gomatrixserverlib.SignJSON(string(g.serverNo), g.keyID, g.priv, raw)
	if err != nil {
		t.Fatalf("signing the key document: %v", err)
	}
	return signed
}

// forgedKeyDocument publishes somebody else's key under the gone server's name, signed with the gone
// server's real key: the author demonstrably holds the honest key, but is not speaking for the key it
// names — which is the check a key server's answer cannot skip, because the connection it arrived over
// says who the key server is and nothing about the server it is talking about.
func forgedKeyDocument(t *testing.T, g *goneServer) json.RawMessage {
	t.Helper()

	attackerPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the adversary key: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"server_name":     string(g.serverNo),
		"verify_keys":     map[string]any{string(g.keyID): map[string]any{"key": encodeBase64(attackerPub)}},
		"old_verify_keys": map[string]any{},
		"valid_until_ts":  spec.Timestamp(g.validUntil.UnixMilli()),
	})
	if err != nil {
		t.Fatalf("encoding the forged key document: %v", err)
	}
	signed, err := gomatrixserverlib.SignJSON(string(g.serverNo), g.keyID, g.priv, raw)
	if err != nil {
		t.Fatalf("signing the forged key document: %v", err)
	}
	return signed
}

// fakeKeyServer is a perspective key server: it answers the key query with whatever documents a test
// hands it, which is how a test decides exactly what this server gets told about a peer it can no longer
// ask for itself.
type fakeKeyServer struct {
	server   *httptest.Server
	serverNo spec.ServerName
	hits     atomic.Int64
	// documents builds this key server's answer. A test uses it to be helpful, to be wrong, or to
	// admit it never heard of the server either.
	documents func(query map[string]any) []json.RawMessage
}

func newFakeKeyServer(t *testing.T) *fakeKeyServer {
	t.Helper()

	notary := &fakeKeyServer{}
	notary.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/_matrix/key/v2/query") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		notary.hits.Add(1)
		var query map[string]any
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		documents := []json.RawMessage{}
		if notary.documents != nil {
			if offered := notary.documents(query); offered != nil {
				documents = offered
			}
		}
		body, err := json.Marshal(map[string]any{"server_keys": documents})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writePeerJSON(w, body)
	}))
	notary.serverNo = spec.ServerName(strings.TrimPrefix(notary.server.URL, "https://"))
	t.Cleanup(notary.server.Close)
	return notary
}

// buildRemoteEventAs builds a room event the way a peer builds one, with the identity and the moment a
// test names. `buildRemoteEvent` signs as the peer that holds the room; this is the same thing for a
// server that is not in the room any more — which is how an event whose signer has gone away ends up in
// state that has to be checked.
func buildRemoteEventAs(t *testing.T, serverNo spec.ServerName, keyID gomatrixserverlib.KeyID,
	priv ed25519.PrivateKey, at time.Time, version string,
	event gomatrixserverlib.ProtoEvent) gomatrixserverlib.PDU {
	t.Helper()

	impl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(version))
	if err != nil {
		t.Fatalf("room version %q: %v", version, err)
	}
	built, err := impl.NewEventBuilderFromProtoEvent(&event).Build(at, serverNo, keyID, priv)
	if err != nil {
		t.Fatalf("building the event: %v", err)
	}
	return built
}

// TestAKeyServerRemembersTheKeyOfAServerThatHasGone checks the case the fallback exists for.
//
// The peer cannot answer for itself, and the signature still has to be believed — not because this
// server enjoys the paperwork, but because the alternative is a room that can never be joined again. The
// signature was made while the key was valid, and a key server that has kept the document is the only
// remaining way to know that the key was ever published at all.
func TestAKeyServerRemembersTheKeyOfAServerThatHasGone(t *testing.T) {
	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(map[string]any) []json.RawMessage {
		return []json.RawMessage{gone.keyDocument(t, false)}
	}

	ring := fedapi.NewKeyRing(newFakePeer(t).peerClient(t), nil, string(notary.serverNo))
	results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{
		fedapi.VerifyRequest(gone.serverNo, gone.signedAt, gone.document(t)),
	})
	if err != nil {
		t.Fatalf("VerifyJSONs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Error != nil {
		t.Fatalf("a signature only a key server could explain was refused: %v", results[0].Error)
	}
	if got := notary.hits.Load(); got != 1 {
		t.Errorf("the key server was asked %d times, want the one question that was answered", got)
	}
}

// TestAKeyServerThatOnlyAnswersAnOpenQuestionIsStillUnderstood models the key server whose cache is
// addressed by key ID and whose memory is not: asked for one key it has never filed under that name, it
// says nothing; asked what it knows about the server, it hands over the key it kept.
//
// The second question is worth asking because the first one is not evidence. "No entry for that key ID"
// is a fact about a filing cabinet, and the failure this fallback exists for is a server that will never
// file another key again.
func TestAKeyServerThatOnlyAnswersAnOpenQuestionIsStillUnderstood(t *testing.T) {
	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(query map[string]any) []json.RawMessage {
		if namesKeys(query) {
			return []json.RawMessage{}
		}
		return []json.RawMessage{gone.keyDocument(t, false)}
	}

	ring := fedapi.NewKeyRing(newFakePeer(t).peerClient(t), nil, string(notary.serverNo))
	results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{
		fedapi.VerifyRequest(gone.serverNo, gone.signedAt, gone.document(t)),
	})
	if err != nil {
		t.Fatalf("VerifyJSONs: %v", err)
	}
	if results[0].Error != nil {
		t.Fatalf("the key server's answer to the open question was not used: %v", results[0].Error)
	}
	if got := notary.hits.Load(); got < 2 {
		t.Errorf("the key server was asked %d times, want the named question and then the open one", got)
	}
}

// namesKeys reports whether a key query named the keys it wanted — the thing that separates the
// specification's question from the open one.
func namesKeys(query map[string]any) bool {
	asked, _ := query["server_keys"].(map[string]any)
	for _, keys := range asked {
		if named, _ := keys.(map[string]any); len(named) > 0 {
			return true
		}
	}
	return false
}

// TestAKeyServerCannotMakeASupersededKeySpeakForToday keeps the fallback narrow.
//
// The same document, the same key, the same signature — and refused, because the question moved from
// "did this key sign this, while it was valid" to "does this key speak for this server now". A key whose
// validity has run out may explain the past and may not authorise the present, which is what keeps a
// server that has been switched off from being a way to sign things today.
func TestAKeyServerCannotMakeASupersededKeySpeakForToday(t *testing.T) {
	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(map[string]any) []json.RawMessage {
		return []json.RawMessage{gone.keyDocument(t, false)}
	}

	ring := fedapi.NewKeyRing(newFakePeer(t).peerClient(t), nil, string(notary.serverNo))
	results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{
		fedapi.VerifyRequest(gone.serverNo, time.Now(), gone.document(t)),
	})
	if err != nil {
		t.Fatalf("VerifyJSONs: %v", err)
	}
	if results[0].Error == nil {
		t.Fatal("a key that stopped being valid was believed for the present")
	}
}

// TestARetiredKeyIsBelievedOnlyForTheYearsItWasValidFor covers the other half of a key document: the
// retired keys, published for exactly this purpose — the historic event that cannot be checked any other
// way.
func TestARetiredKeyIsBelievedOnlyForTheYearsItWasValidFor(t *testing.T) {
	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(map[string]any) []json.RawMessage {
		return []json.RawMessage{gone.keyDocument(t, true)}
	}

	ring := fedapi.NewKeyRing(newFakePeer(t).peerClient(t), nil, string(notary.serverNo))
	for _, check := range []struct {
		name  string
		at    time.Time
		wants bool
	}{
		{"while it was still valid", gone.signedAt, true},
		{"after it was retired", time.Now(), false},
	} {
		results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{
			fedapi.VerifyRequest(gone.serverNo, check.at, gone.document(t)),
		})
		if err != nil {
			t.Fatalf("%s: VerifyJSONs: %v", check.name, err)
		}
		if failed := results[0].Error != nil; failed == check.wants {
			if check.wants {
				t.Errorf("a retired key was refused for a signature made %s: %v", check.name, results[0].Error)
			} else {
				t.Errorf("a retired key was believed for a signature made %s", check.name)
			}
		}
	}
}

// TestAKeyServerAnsweringAboutNobodyIsNotBelieved: a document that is internally perfect but describes a
// server this lookup never asked about is a key server introducing a signing key under a name of its own
// choosing.
func TestAKeyServerAnsweringAboutNobodyIsNotBelieved(t *testing.T) {
	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(map[string]any) []json.RawMessage {
		document := gone.keyDocument(t, false)
		// The signature and the key still agree; only the name has moved.
		shifted := strings.Replace(string(document), string(gone.serverNo), "somebody-else.test", 1)
		return []json.RawMessage{json.RawMessage(shifted)}
	}

	ring := fedapi.NewKeyRing(newFakePeer(t).peerClient(t), nil, string(notary.serverNo))
	results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{
		fedapi.VerifyRequest(gone.serverNo, gone.signedAt, gone.document(t)),
	})
	if err != nil {
		t.Fatalf("VerifyJSONs: %v", err)
	}
	if results[0].Error == nil {
		t.Fatal("a key document describing another server was believed")
	}
}

// TestAKeyServerPublishingSomebodyElsesModuleIsNotBelieved keeps the one check that the connection cannot
// make: a document is only worth the keys inside it signing it.
func TestAKeyServerPublishingSomebodyElsesModuleIsNotBelieved(t *testing.T) {
	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(map[string]any) []json.RawMessage {
		return []json.RawMessage{forgedKeyDocument(t, gone)}
	}

	ring := fedapi.NewKeyRing(newFakePeer(t).peerClient(t), nil, string(notary.serverNo))
	results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{
		fedapi.VerifyRequest(gone.serverNo, gone.signedAt, gone.document(t)),
	})
	if err != nil {
		t.Fatalf("VerifyJSONs: %v", err)
	}
	if results[0].Error == nil {
		t.Fatal("a key document that its own keys do not sign was believed")
	}
}

// TestNoKeyServerIsAskedWhenNoneAreNamed pins the default, because the default is the security position: a
// deployment that has not named a key server believes only what a peer says about itself, and accepts that
// a server which has gone away takes its events with it.
func TestNoKeyServerIsAskedWhenNoneAreNamed(t *testing.T) {
	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(map[string]any) []json.RawMessage {
		return []json.RawMessage{gone.keyDocument(t, false)}
	}

	ring := fedapi.NewKeyRing(newFakePeer(t).peerClient(t), nil)
	results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{
		fedapi.VerifyRequest(gone.serverNo, gone.signedAt, gone.document(t)),
	})
	if err != nil {
		t.Fatalf("VerifyJSONs: %v", err)
	}
	if results[0].Error == nil {
		t.Fatal("a key was obtained without a key server being named")
	}
	if notary.hits.Load() != 0 {
		t.Errorf("the key server was asked %d times without being configured", notary.hits.Load())
	}
}

// TestAKeyServerThatKnowsNothingIsNotAnAnswer covers the honest shrug: a key server that has never heard of
// the peer neither helps nor hurts, and the event is refused for the reason it always was.
func TestAKeyServerThatKnowsNothingIsNotAnAnswer(t *testing.T) {
	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(map[string]any) []json.RawMessage { return []json.RawMessage{} }

	ring := fedapi.NewKeyRing(newFakePeer(t).peerClient(t), nil, string(notary.serverNo))
	results, err := ring.VerifyJSONs(context.Background(), []gomatrixserverlib.VerifyJSONRequest{
		fedapi.VerifyRequest(gone.serverNo, gone.signedAt, gone.document(t)),
	})
	if err != nil {
		t.Fatalf("VerifyJSONs: %v", err)
	}
	if results[0].Error == nil {
		t.Fatal("an empty answer was treated as a key")
	}
	if notary.hits.Load() == 0 {
		t.Error("the key server was never asked")
	}
}

// joinOfARoomWhoseMemberHasGoneAway stands a room on a live peer whose state holds one member signed by a
// server that has since disappeared — the situation a room falls into by existing longer than one of its
// members' homeservers. `named` is whether this server was told about a key server to ask; the notary is
// stood up either way, so that the refusal test can prove it was left alone.
func joinOfARoomWhoseMemberHasGoneAway(t *testing.T, named bool) (*harness.Server, *harness.Account, *federatedRoom, *fakeKeyServer, *goneServer) {
	t.Helper()

	gone := newGoneServer(t)
	notary := newFakeKeyServer(t)
	notary.documents = func(map[string]any) []json.RawMessage {
		return []json.RawMessage{gone.keyDocument(t, false)}
	}

	options := []harness.Option{harness.WithFederationInsecureTLS()}
	if named {
		options = append(options, harness.WithKeyPerspectives(string(notary.serverNo)))
	}
	server := harness.NewWithRegistration(t, options...)
	alice := server.Register(t, "host", "pw-host", false)

	peer := newFakePeer(t)
	room := newFederatedRoom(t, peer)
	// A member who joined while their own server still existed. Nothing about this event is unusual:
	// it is signed by the server that ran it, at a time when that server published the key, and the room
	// has kept it in its state ever since.
	member := "@old:" + string(gone.serverNo)
	goneMember := buildRemoteEventAs(t, gone.serverNo, gone.keyID, gone.priv, gone.signedAt, room.version,
		gomatrixserverlib.ProtoEvent{
			Type:       "m.room.member",
			RoomID:     room.roomID,
			StateKey:   &member,
			SenderID:   member,
			Depth:      5,
			PrevEvents: []string{room.state[len(room.state)-1].EventID()},
			AuthEvents: []string{room.state[0].EventID(), room.state[1].EventID()},
			Content:    spec.RawJSON(mustMarshal(t, map[string]any{"membership": "join"})),
		})
	room.broken = func(answer *federatedJoinAnswer) {
		answer.State = append(answer.State, json.RawMessage(goneMember.JSON()))
	}
	serveFederatedRoom(t, peer, room)
	return server, alice, room, notary, gone
}

// TestAJoinOfARoomWhoseMemberHasGoneAwayForever is the report this all came from: the peer admits the join
// and hands back a room this server cannot believe, because one signature in its state belongs to a server
// nobody can ask any more. With a key server named, the room is a room.
func TestAJoinOfARoomWhoseMemberHasGoneAwayForever(t *testing.T) {
	server, alice, room, notary, gone := joinOfARoomWhoseMemberHasGoneAway(t, true)

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(room.peer.serverNo)),
		alice.AccessToken, map[string]any{})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("joining a room whose state outlived one server failed (%d): %s",
			response.StatusCode, response.Body)
	}
	members := roomMembers(t, alice, server, room.roomID)
	if !members["@old:"+string(gone.serverNo)] {
		t.Errorf("the member whose server has gone is missing from the room (%v)", members)
	}
	if notary.hits.Load() == 0 {
		t.Error("the join succeeded without asking the key server, so nothing was proved about that signature")
	}
}

// TestAJoinRefusesARoomWhoseMemberHasGoneAwayWhenNoKeyServerIsNamed is the same room and the same state,
// refused: without a second source the signature cannot be checked, and an unchecked signature is a member
// list this server would be inventing. Naming the server whose key could not be obtained is what makes the
// refusal diagnosable from the client instead of from the log.
func TestAJoinRefusesARoomWhoseMemberHasGoneAwayWhenNoKeyServerIsNamed(t *testing.T) {
	server, alice, room, notary, gone := joinOfARoomWhoseMemberHasGoneAway(t, false)

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(room.peer.serverNo)),
		alice.AccessToken, map[string]any{})
	if response.StatusCode == http.StatusOK {
		t.Fatal("a state event nobody can attribute was accepted as part of a room")
	}
	if body := string(response.Body); !strings.Contains(body, string(gone.serverNo)) {
		t.Errorf("the refusal should name the server whose key could not be obtained, said: %s", body)
	}
	assertRoomAbsent(t, alice, server, room.roomID)
	if notary.hits.Load() != 0 {
		t.Errorf("a key server was consulted %d times without being configured", notary.hits.Load())
	}
}
