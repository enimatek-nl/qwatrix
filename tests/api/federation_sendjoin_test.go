package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
)

// The join handshake end to end: this server offers a place in the graph, a peer signs a join for one
// of its users, and this server decides whether to let it in.
//
// The tests below are deliberately written against a peer that is not this server — a second signing
// identity, its own key, its own HTTPS server — because that is the situation the code exists for. A
// test where the joiner is trusted would pass for reasons that have nothing to do with whether the
// checks work.

// signedFederationPut sends a PUT to this server's federation listener the way a real peer would:
// signed with the peer's own key over the method, URI, origin, destination and body.
func signedFederationPut(t *testing.T, server *harness.Server, peer *fakePeer, path string, body any) *http.Response {
	t.Helper()

	request := fclient.NewFederationRequest(http.MethodPut, peer.serverNo, harness.ServerName, path)
	// Content first, then the signature: the signature covers the body, and the library refuses
	// to modify a request once it has been signed rather than let a caller leave a signature
	// pointing at something else.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the body: %v", err)
	}
	// json.RawMessage, not []byte: the library marshals the content itself, and a plain byte
	// slice marshals to a base64 string, which arrives as an event that cannot be read.
	if err := request.SetContent(json.RawMessage(raw)); err != nil {
		t.Fatalf("setting the request content: %v", err)
	}
	if err := request.Sign(peer.serverNo, peer.keyID, peer.priv); err != nil {
		t.Fatalf("signing the request: %v", err)
	}
	httpRequest, err := request.HTTPRequest()
	if err != nil {
		t.Fatalf("building the HTTP request: %v", err)
	}
	// The library describes a federation request with its own `matrix://` scheme and rewrites it
	// in its own transport. A test driving a plain HTTP client has to do that rewrite itself.
	// Only the scheme and host change: the path is what the signature covers, and rewriting the
	// URI would sign a different request than the one sent.
	httpRequest.URL.Scheme = "https"
	httpRequest.URL.Host = strings.TrimPrefix(server.FederationServer(t).URL, "https://")
	response, err := server.FederationClient(t).Do(httpRequest)
	if err != nil {
		t.Fatalf("sending the federation request: %v", err)
	}
	return response
}

// buildJoin asks this server for a join template and returns it as a signed event, the way a peer
// would: fill in who is joining, build, sign with the peer's own key.
func buildJoin(t *testing.T, server *harness.Server, peer *fakePeer, roomID, userID string, tamper func(*gomatrixserverlib.ProtoEvent)) (gomatrixserverlib.PDU, string) {
	t.Helper()

	status, body := federationGet(t, server, "/_matrix/federation/v1/make_join/"+
		harness.PathEscape(roomID)+"/"+harness.PathEscape(userID))
	if status != http.StatusOK {
		t.Fatalf("make_join: status %d (%s)", status, body)
	}
	var template struct {
		RoomVersion string                       `json:"room_version"`
		Event       gomatrixserverlib.ProtoEvent `json:"event"`
	}
	if err := json.Unmarshal(body, &template); err != nil {
		t.Fatalf("the join template is not the documented shape: %v (%s)", err, body)
	}
	event := template.Event
	event.SenderID = userID
	key := userID
	event.StateKey = &key
	if tamper != nil {
		tamper(&event)
	}

	version, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(template.RoomVersion))
	if err != nil {
		t.Fatalf("room version %q: %v", template.RoomVersion, err)
	}
	builder := version.NewEventBuilderFromProtoEvent(&event)
	signed, err := builder.Build(time.Now(), peer.serverNo, peer.keyID, peer.priv)
	if err != nil {
		t.Fatalf("building the join event: %v", err)
	}
	return signed, template.RoomVersion
}

func federationBody(t *testing.T, response *http.Response) []byte {
	t.Helper()

	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return raw
}

// TestSendJoinAdmitsARemoteUser is the happy path of the handshake.
//
// The interesting part is not the 200. It is that the answer carries the room's state and
// authentication chain — the joining server has no way to check this room otherwise — and that a
// local member then sees the new remote member in the room's /members.
func TestSendJoinAdmitsARemoteUser(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	peer := newFakePeer(t)
	remote := "@faraway:" + string(peer.serverNo)
	event, _ := buildJoin(t, server, peer, roomID, remote, nil)

	path := "/_matrix/federation/v2/send_join/" + harness.PathEscape(roomID) + "/" +
		harness.PathEscape(event.EventID())
	response := signedFederationPut(t, server, peer, path, json.RawMessage(event.JSON()))
	body := federationBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("send_join: status %d (%s)", response.StatusCode, body)
	}

	var acceptance struct {
		State     []json.RawMessage `json:"state"`
		AuthChain []json.RawMessage `json:"auth_chain"`
		Origin    string            `json:"origin"`
	}
	if err := json.Unmarshal(body, &acceptance); err != nil {
		t.Fatalf("the send_join answer is not the documented shape: %v (%s)", err, body)
	}
	if len(acceptance.State) == 0 {
		t.Fatal("the answer carries no state, so the peer cannot check anything about this room")
	}
	if len(acceptance.AuthChain) == 0 {
		t.Error("the answer carries no auth chain, so the peer cannot tell that the state is authorised")
	}
	if acceptance.Origin != string(peer.serverNo) {
		t.Errorf("origin: got %q, want %q", acceptance.Origin, peer.serverNo)
	}
	// The state must include the join it just accepted: the peer is entitled to the room as it
	// now stands, and a state that predates the event being joined is a state without the join.
	//
	// It has to be found by what it says rather than by its ID. From room version 3 onwards an
	// event's ID is a hash of the event, so it is not a field inside it — the peer recomputes the
	// ID from the JSON it was sent, which is also why the events here must be the signed
	// originals rather than a re-rendering.
	if !stateHasJoin(t, acceptance.State, remote) {
		t.Errorf("the accepted join is not in the state returned to the peer (%d events)",
			len(acceptance.State))
	}

	// And the room agrees locally: alice sees the remote user as joined.
	members := string(alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/members").Body)
	if !strings.Contains(members, remote) {
		t.Errorf("the remote member is not in the room's members: %s", members)
	}
}

// TestSendJoinRefusesAnUnsignedRequest is the difference between an authenticated server and a
// request that mentioned a server name.
//
// Federation has no sessions and no tokens: the only proof that a request came from the server it
// claims is an ed25519 signature the claimed server's published key verifies. Without this refusal
// every private room on this server would be joinable by anybody who could write a URL.
func TestSendJoinRefusesAnUnsignedRequest(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	peer := newFakePeer(t)
	remote := "@faraway:" + string(peer.serverNo)
	event, _ := buildJoin(t, server, peer, roomID, remote, nil)

	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encoding the event: %v", err)
	}
	target := server.FederationServer(t).URL + "/_matrix/federation/v2/send_join/" +
		harness.PathEscape(roomID) + "/" + harness.PathEscape(event.EventID())
	request, err := http.NewRequest(http.MethodPut, target, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.FederationClient(t).Do(request)
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	body := federationBody(t, response)
	if response.StatusCode == http.StatusOK {
		t.Fatalf("an unsigned join was accepted: %s", body)
	}
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d, want an authentication refusal (%s)", response.StatusCode, body)
	}

	// The room must be untouched by the refusal, not half-joined.
	members := string(alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/members").Body)
	if strings.Contains(members, remote) {
		t.Errorf("the refused join still reached the room: %s", members)
	}
}

// TestSendJoinRefusesAJoinForSomebodyElse checks the one authority a peer actually has.
//
// A server may join its own users and nobody else's. Signed correctly, from the right key, for the
// right room — and still refused, because the user named in the event belongs to a different domain
// than the server that signed it. Without that rule, one compromised homeserver could fill every
// room on every other server with identities it does not hold.
func TestSendJoinRefusesAJoinForSomebodyElse(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	peer := newFakePeer(t)
	// A join for a user who lives somewhere else, built with this peer's key. `make_join` will
	// hand out a template for any syntactically valid user ID — it is a description of what a
	// join would look like, not a permission slip — so the refusal has to happen at send_join,
	// where the signature is known.
	other := "@faraway:somewhere.else"
	impersonated, _ := buildJoin(t, server, peer, roomID, other, nil)

	path := "/_matrix/federation/v2/send_join/" + harness.PathEscape(roomID) + "/" +
		harness.PathEscape(impersonated.EventID())
	response := signedFederationPut(t, server, peer, path, json.RawMessage(impersonated.JSON()))
	body := federationBody(t, response)
	// gomatrixserverlib computes a different event ID for a different sender, so the path names
	// the event it built; either an authorisation refusal or a signature refusal is correct, and
	// both must leave the room untouched.
	if response.StatusCode == http.StatusOK {
		t.Fatalf("a server was allowed to join a user it does not host: %s", body)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (%s)", response.StatusCode, body)
	}

	members := string(alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/members").Body)
	if strings.Contains(members, other) {
		t.Errorf("the impersonated join reached the room: %s", members)
	}
}

// TestSendJoinRefusesAPrivateRoom checks that the room's own rules are applied to federated joins.
//
// A room whose join rule is `invite` is private whether the request arrived on the client port or on
// the federation port. The check is the same authorisation engine a local join passes through, which
// is the only reason it can be relied on to say the same thing.
func TestSendJoinRefusesAPrivateRoom(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"visibility": "private"})

	peer := newFakePeer(t)
	remote := "@faraway:" + string(peer.serverNo)
	event, _ := buildJoin(t, server, peer, roomID, remote, nil)

	path := "/_matrix/federation/v2/send_join/" + harness.PathEscape(roomID) + "/" +
		harness.PathEscape(event.EventID())
	response := signedFederationPut(t, server, peer, path, json.RawMessage(event.JSON()))
	body := federationBody(t, response)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403 for a join into an invite-only room (%s)",
			response.StatusCode, body)
	}
	if !strings.Contains(string(body), "M_FORBIDDEN") {
		t.Errorf("body: got %s, want M_FORBIDDEN", body)
	}
}

// TestSendJoinAnswersTheLegacyEnvelope covers the response shape a pre-v2 peer parses.
//
// Version 1 wraps the answer in a two-element array of status and body. A v1 peer handed a bare
// object would read the whole response as a status code, so this is not a compatibility nicety: a
// client that falls back to v1 after a 404 on v2 gets exactly the shape its parser expects.
func TestSendJoinAnswersTheLegacyEnvelope(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	peer := newFakePeer(t)
	remote := "@faraway:" + string(peer.serverNo)
	event, _ := buildJoin(t, server, peer, roomID, remote, nil)

	path := "/_matrix/federation/v1/send_join/" + harness.PathEscape(roomID) + "/" +
		harness.PathEscape(event.EventID())
	response := signedFederationPut(t, server, peer, path, json.RawMessage(event.JSON()))
	body := federationBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("v1 send_join: status %d (%s)", response.StatusCode, body)
	}
	var envelope []json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("a v1 answer is not the legacy [status, body] envelope: %v (%s)", err, body)
	}
	if len(envelope) != 2 {
		t.Fatalf("envelope: got %d elements, want 2", len(envelope))
	}
	var status int
	if err := json.Unmarshal(envelope[0], &status); err != nil || status != http.StatusOK {
		t.Errorf("the envelope's status element: got %s, want 200", envelope[0])
	}
	var payload struct {
		State []json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(envelope[1], &payload); err != nil {
		t.Fatalf("the envelope's body is not the send_join object: %v", err)
	}
	if len(payload.State) == 0 {
		t.Error("the v1 answer carries no state")
	}
}

// stateHasJoin looks for the remote user's join among the state events a peer was given.
func stateHasJoin(t *testing.T, state []json.RawMessage, userID string) bool {
	t.Helper()

	for _, raw := range state {
		var event struct {
			Type     string `json:"type"`
			StateKey string `json:"state_key"`
			Content  struct {
				Membership string `json:"membership"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("a state event in the answer is not JSON this server understands: %v", err)
		}
		if event.Type == "m.room.member" && event.StateKey == userID &&
			event.Content.Membership == "join" {
			return true
		}
	}
	return false
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return raw
}
