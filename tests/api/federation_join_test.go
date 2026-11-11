package api_test

// The outbound half of a federated join: a local user asking to join a room that only another server
// holds.
//
// Everything in `federation_sendjoin_test.go` is this server deciding whether to let somebody in. This
// file is the opposite direction — this server is the stranger at the gate — and the trust problem is
// sharper, because the whole answer comes from a peer with an interest in being believed. The peer
// chooses the state, the room version and the room's history, and this server is about to sign an event
// and show the result to its own users. So the checks are not a formality:
//
// The join template is the peer's, and this server signs it. A template that names a different room, a
// different user, or a membership other than "join" must be refused before the signature goes on it,
// because after that point this server has authored the forgery itself.
//
// The state that comes back is the peer's claim about a room it controls, and every event in it has to
// carry a signature that holds up against its sender's published key. Unsigned or mis-signed state is a
// peer making things up.
//
// And the room's rules, as the peer itself described them, must actually allow the join. A peer that
// says "welcome" while handing back a state in which this user is banned is either broken or lying, and
// neither is a room this server should show its users.
//
// The tests below each break exactly one of those, and each also asserts that nothing was persisted: a
// join that was refused must leave no half-room behind, because a room whose state arrived incompletely
// is what every later decision gets read from.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// federatedRoom is a room the fake peer holds: enough signed state for a join to be authorised against
// it, plus the knobs that let a test hand back a broken answer.
type federatedRoom struct {
	roomID  string
	version string
	remote  string
	peer    *fakePeer

	state []gomatrixserverlib.PDU
	// broken mutates the send_join answer. It is the test's way of being a dishonest peer, and each
	// test uses it for exactly one lie.
	broken func(*federatedJoinAnswer)
	// priorMember is a membership event the room already held for the local user. It belongs to the
	// state as the peer describes it *before* the join it is answering for, which is why a correct
	// answer ends up holding two member events for one user: this one, and the join.
	priorMember gomatrixserverlib.PDU
	// joins counts how often this peer was asked to let the user in.
	joins int
}

// federatedJoinAnswer is the body of a send_join response, as a test writes it.
type federatedJoinAnswer struct {
	Origin        string            `json:"origin"`
	State         []json.RawMessage `json:"state"`
	AuthChain     []json.RawMessage `json:"auth_chain"`
	ServersInRoom []string          `json:"servers_in_room,omitempty"`
}

// serveFederatedRoom mounts the peer's half of the join handshake on a fake peer.
func serveFederatedRoom(t *testing.T, peer *fakePeer, room *federatedRoom) {
	t.Helper()

	peer.serves = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/make_join/"):
			serveMakeJoinTemplate(t, peer, room, w, r)
			return true
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v2/send_join/"):
			serveSendJoin(t, peer, room, w, r)
			return true
		default:
			return false
		}
	}
}

func serveMakeJoinTemplate(t *testing.T, peer *fakePeer, room *federatedRoom, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	userID := makeJoinUser(r.URL.Path)
	if userID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The template is what the peer says a join by this user would look like: the position in the
	// graph, the auth events, the depth. The sender is filled in by the joining server, which is why
	// the library marshals it as an empty string rather than omitting it.
	template := gomatrixserverlib.ProtoEvent{
		Type:       "m.room.member",
		RoomID:     room.roomID,
		StateKey:   &userID,
		Depth:      room.state[len(room.state)-1].Depth() + 1,
		PrevEvents: []string{room.state[len(room.state)-1].EventID()},
		// The create event, the join rules and the power levels: what the room's own rules say a
		// join must be authorised against. A peer that names fewer is not offering a shorter chain,
		// it is offering a rule that never gets read — see requireCompleteAuthChain.
		AuthEvents: room.authEventsForJoin(),
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"membership": "join"})),
	}
	raw, err := json.Marshal(map[string]any{
		"room_version": room.version,
		"event":        template,
	})
	if err != nil {
		t.Fatalf("encoding the join template: %v", err)
	}
	writePeerJSON(w, raw)
}

func serveSendJoin(t *testing.T, peer *fakePeer, room *federatedRoom, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	room.joins++
	var joinEvent map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&joinEvent); err != nil {
		t.Fatalf("the join event this server sent cannot be read: %v", err)
	}
	joinRaw, err := json.Marshal(joinEvent)
	if err != nil {
		t.Fatalf("re-encoding the join event: %v", err)
	}

	answer := federatedJoinAnswer{
		Origin:        string(peer.serverNo),
		State:         stateJSON(room.state),
		AuthChain:     []json.RawMessage{json.RawMessage(room.state[0].JSON()), json.RawMessage(room.state[1].JSON())},
		ServersInRoom: []string{string(peer.serverNo)},
	}
	// A peer answers with the room as it now stands, which includes the join it just accepted.
	// Returning the state from before the join is a state without the member it is answering for.
	if room.priorMember != nil {
		// The state as it stood before the join, which for a user who had been here includes their
		// old membership event alongside the join that replaces it.
		answer.State = append(answer.State, json.RawMessage(room.priorMember.JSON()))
	}
	answer.State = append(answer.State, joinRaw)
	if room.broken != nil {
		room.broken(&answer)
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encoding the send_join answer: %v", err)
	}
	writePeerJSON(w, raw)
}

// makeJoinUser pulls the user ID out of a make_join path. The path is
// /_matrix/federation/v1/make_join/{roomID}/{userID}, both escaped.
func makeJoinUser(path string) string {
	const prefix = "/_matrix/federation/v1/make_join/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 2 {
		return ""
	}
	decoded, err := url.PathUnescape(parts[1])
	if err != nil {
		return ""
	}
	return decoded
}

func stateJSON(events []gomatrixserverlib.PDU) []json.RawMessage {
	raw := make([]json.RawMessage, 0, len(events))
	for _, event := range events {
		raw = append(raw, json.RawMessage(event.JSON()))
	}
	return raw
}

func writePeerJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// newFederatedRoom builds a room on the fake peer: a public room with one remote member, all of it
// signed by the peer's own key, because that is what its answer has to be signed with for this server
// to believe any of it.
// newFederatedRoom builds a public room as some other server holds it.
func newFederatedRoom(t *testing.T, peer *fakePeer) *federatedRoom {
	t.Helper()
	return newFederatedRoomWithJoinRule(t, peer, "public")
}

func newFederatedRoomWithJoinRule(t *testing.T, peer *fakePeer, joinRule string) *federatedRoom {
	t.Helper()

	const version = "11"
	remote := "@charlie:" + string(peer.serverNo)
	roomID := "!federated:" + string(peer.serverNo)

	// The create event starts the graph: no predecessors, no auth events.
	create := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		Type:     "m.room.create",
		RoomID:   roomID,
		StateKey: strPointer(""),
		SenderID: remote,
		Depth:    1,
		Content:  spec.RawJSON(mustMarshal(t, map[string]any{"room_version": version, "m.federate": true})),
	})
	powerLevels := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		Type:       "m.room.power_levels",
		RoomID:     roomID,
		StateKey:   strPointer(""),
		SenderID:   remote,
		Depth:      2,
		PrevEvents: []string{create.EventID()},
		AuthEvents: []string{create.EventID()},
		Content: spec.RawJSON(mustMarshal(t, map[string]any{
			"users":          map[string]any{remote: 100},
			"users_default":  0,
			"events_default": 0,
			"state_default":  50,
			"ban":            50,
			"kick":           50,
			"invite":         0,
			"redact":         0,
		})),
	})
	member := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		Type:       "m.room.member",
		RoomID:     roomID,
		StateKey:   &remote,
		SenderID:   remote,
		Depth:      3,
		PrevEvents: []string{powerLevels.EventID()},
		AuthEvents: []string{create.EventID(), powerLevels.EventID()},
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"membership": "join"})),
	})
	joinRules := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		Type:       "m.room.join_rules",
		RoomID:     roomID,
		StateKey:   strPointer(""),
		SenderID:   remote,
		Depth:      4,
		PrevEvents: []string{member.EventID()},
		AuthEvents: []string{create.EventID(), powerLevels.EventID()},
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"join_rule": joinRule})),
	})

	return &federatedRoom{
		roomID:  roomID,
		version: version,
		remote:  remote,
		peer:    peer,
		// In depth order, create first: the join template this peer offers is built from the last
		// event, and its auth events from the first two.
		state: []gomatrixserverlib.PDU{create, powerLevels, member, joinRules},
	}
}

// authEventsForJoin lists what this peer's room says a join must be authorised against: the create
// event, the join rules and the power levels — plus the joining user's own previous membership event,
// when the room holds one. A real server reads that out of its own state; a test fixture that left it
// out would be offering a join whose auth chain ignores what the user already is.
func (room *federatedRoom) authEventsForJoin() []string {
	ids := []string{room.state[0].EventID(), room.state[3].EventID(), room.state[1].EventID()}
	if room.priorMember != nil {
		ids = append(ids, room.priorMember.EventID())
	}
	return ids
}

func strPointer(value string) *string { return &value }

func TestAJoinOfARoomOnAnotherServer(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	room := newFederatedRoom(t, peer)
	serveFederatedRoom(t, peer, room)

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
		alice.AccessToken, map[string]any{})
	body := response.Body
	if response.StatusCode != http.StatusOK {
		t.Fatalf("joining a room on a peer failed (%d): %s", response.StatusCode, body)
	}
	var joined struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &joined); err != nil {
		t.Fatalf("the join answer is not the documented shape: %v (%s)", err, body)
	}
	if joined.RoomID != room.roomID {
		t.Errorf("joined room %q, want %q", joined.RoomID, room.roomID)
	}
	if room.joins != 1 {
		t.Errorf("the peer was asked to admit the join %d times, want 1", room.joins)
	}

	// The point of a join is that the user's own client sees the room.
	sync := alice.MustSync(t, server, nil)
	joinedRoom, ok := sync.Rooms.Join[room.roomID]
	if !ok {
		t.Fatalf("the federated room is not in the local user's sync (rooms.join has %d rooms)",
			len(sync.Rooms.Join))
	}
	// And the members the peer described have to be here, or the room is empty from this side: no
	// names, no avatars, and no way to know who else is listening.
	members := roomMembers(t, alice, server, room.roomID)
	if !members[room.remote] {
		t.Errorf("the remote member %s is not in the room's member list (%v)", room.remote, members)
	}
	if !members["@host:"+string(harness.ServerName)] {
		t.Errorf("the local user's own join is not in the member list (%v)", members)
	}
	_ = joinedRoom
}

func TestAFederatedJoinRefusesATemplateForSomebodyElse(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	room := newFederatedRoom(t, peer)

	// The template normally names the user in its path; this peer names somebody else in the event
	// while answering for alice. Signing it would have this server author a join for a user it does
	// not host, and the peer would then present it as coming from this server.
	peer.serves = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/make_join/"):
			impostor := "@victims:" + string(peer.serverNo)
			template := gomatrixserverlib.ProtoEvent{
				Type:       "m.room.member",
				RoomID:     room.roomID,
				StateKey:   &impostor,
				Depth:      5,
				PrevEvents: []string{room.state[3].EventID()},
				AuthEvents: []string{room.state[0].EventID(), room.state[1].EventID()},
				Content:    spec.RawJSON(mustMarshal(t, map[string]any{"membership": "join"})),
			}
			raw, err := json.Marshal(map[string]any{"room_version": room.version, "event": template})
			if err != nil {
				t.Fatalf("encoding the impostor template: %v", err)
			}
			writePeerJSON(w, raw)
			return true
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v2/send_join/"):
			t.Error("a join this server refused to sign was still sent to the peer")
			return true
		default:
			return false
		}
	}

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
		alice.AccessToken, map[string]any{})
	if response.StatusCode == http.StatusOK {
		t.Fatal("a join for a user this server does not host was accepted")
	}
	// The refusal has to be the template check. Without this assertion the test would also pass if
	// the join were refused for an unrelated reason further along — and this server does have a
	// second line of defence here, since it fills the sender and state key in itself rather than
	// signing what it was handed, so a weak check would still hide behind it.
	if body := string(response.Body); !strings.Contains(body, "@victims:") {
		t.Errorf("the refusal should name the user the peer tried to join as, said: %s", body)
	}
}

func TestAFederatedJoinRefusesContradictoryState(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	room := newFederatedRoom(t, peer)

	// A second set of power levels for the same room, signed by the peer and consistent enough to
	// verify. Nothing else about the answer is wrong: the join is present, the chain is complete, the
	// signatures hold. Two m.room.power_levels events is the case no other check can see, because
	// both entries are valid events and only one of them can be the room's rules.
	another := buildRemoteEvent(t, peer, room.version, gomatrixserverlib.ProtoEvent{
		Type:     "m.room.power_levels",
		RoomID:   room.roomID,
		StateKey: strPointer(""),
		SenderID: room.remote,
		Depth:    6,
		Content: spec.RawJSON(mustMarshal(t, map[string]any{
			"users": map[string]any{room.remote: 100},
		})),
	})

	room.broken = func(answer *federatedJoinAnswer) {
		kept := answer.State
		answer.State = append(stateJSON(append(
			append([]gomatrixserverlib.PDU{}, room.state...), another)), kept[len(kept)-1])
	}
	serveFederatedRoom(t, peer, room)

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
		alice.AccessToken, map[string]any{})
	if response.StatusCode == http.StatusOK {
		t.Fatal("a room with two sets of power levels was accepted as a room")
	}
	// Naming the collision is the point: a room whose state answers one question twice has to be
	// refused *as* contradictory, not accepted with whichever entry was read last.
	if body := string(response.Body); !strings.Contains(body, "m.room.power_levels") {
		t.Errorf("the refusal should name the state key the peer contradicted itself on, said: %s", body)
	}
	assertRoomAbsent(t, alice, server, room.roomID)
}

func TestAFederatedJoinRefusesUnsignedState(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	room := newFederatedRoom(t, peer)

	room.broken = func(answer *federatedJoinAnswer) {
		// The create event, with its signature removed: an event nobody can attribute, describing
		// the rules every other event in the room is authorised by.
		answer.State = append([]json.RawMessage{stripSignatures(t, answer.State[0])}, answer.State[1:]...)
	}
	serveFederatedRoom(t, peer, room)

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
		alice.AccessToken, map[string]any{})
	if response.StatusCode == http.StatusOK {
		t.Fatal("state whose signature does not verify was accepted as a room")
	}
	assertRoomAbsent(t, alice, server, room.roomID)
}

func TestAFederatedJoinRefusesStateFromAnotherRoom(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	room := newFederatedRoom(t, peer)

	// A state event from a different room — properly built and signed for that other room, so the
	// only thing wrong with it is which room it belongs to. A copy of one of this room's events
	// would prove nothing.
	foreignRoom := "!somebody-elses:" + string(peer.serverNo)
	foreign := buildRemoteEvent(t, peer, room.version, gomatrixserverlib.ProtoEvent{
		Type:     "m.room.create",
		RoomID:   foreignRoom,
		StateKey: strPointer(""),
		SenderID: room.remote,
		Depth:    1,
		Content:  spec.RawJSON(mustMarshal(t, map[string]any{"room_version": room.version})),
	})
	room.broken = func(answer *federatedJoinAnswer) {
		// A state event from a different room, wearing this room's place in the answer. Accepting
		// it would let a peer define this room's rules with somebody else's events.
		answer.State = append(answer.State, json.RawMessage(foreign.JSON()))
	}
	serveFederatedRoom(t, peer, room)

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
		alice.AccessToken, map[string]any{})
	if response.StatusCode == http.StatusOK {
		t.Fatal("state describing a different room was accepted as this one")
	}
	assertRoomAbsent(t, alice, server, room.roomID)
}

// TestAFederatedJoinRefusesAPrivateRoom applies the room's own rules to a state this server did not
// write. The room is invite-only, this user was never invited, and the peer answers the join as though
// it were allowed — consistently, with the invite-only join rules in both the state and the join's own
// auth chain, so nothing about the answer is self-contradictory. The only thing wrong with it is that
// the room does not admit this user.
//
// The check belongs here rather than only there, because "there" is a server with its own interests.
// And it belongs to the room's own authorisation rules rather than to a special case for invite-only
// rooms, because those are the same rules this server will enforce against its own members in this room
// from now on — a room joined by bypassing its rules would be enforced inconsistently forever.
func TestAFederatedJoinRefusesAPrivateRoom(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	room := newFederatedRoomWithJoinRule(t, peer, "invite")
	serveFederatedRoom(t, peer, room)

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
		alice.AccessToken, map[string]any{})
	if response.StatusCode == http.StatusOK {
		t.Fatalf("a user was joined into an invite-only room by a peer that never invited them: %s",
			response.Body)
	}
	assertRoomAbsent(t, alice, server, room.roomID)
}

func TestAJoinOfAnUnreachableRoomIsAnErrorAndNotAHalfRoom(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	serveFederatedRoom(t, peer, newFederatedRoom(t, peer))
	hitsBefore := peer.hits.Load()

	// Nobody is named and the room's own home server does not exist. The honest answer is a failure
	// that says which room could not be reached, after this server tried the only address it was
	// given — and no room row, no membership, and no half-state to be found in /sync afterwards.
	const roomID = "!nowhere:nowhere.invalid"
	response := server.PostJSON(t, "/_matrix/client/v3/join/"+url.PathEscape(roomID),
		alice.AccessToken, map[string]any{})
	body := response.Body
	if response.StatusCode == http.StatusOK {
		t.Fatalf("joining an unreachable room reported success: %s", body)
	}
	if !strings.Contains(string(body), roomID) {
		t.Errorf("the failure should name the room it could not reach, said: %s", body)
	}
	assertRoomAbsent(t, alice, server, roomID)
	if hits := peer.hits.Load(); hits != hitsBefore {
		t.Errorf("a join with no server named contacted an unrelated peer (%d requests)", hits-hitsBefore)
	}
}

// stripSignatures removes an event's signatures, which is how a peer hands over an event that cannot
// be attributed to anybody.
func stripSignatures(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("the state event cannot be re-read: %v", err)
	}
	delete(fields, "signatures")
	stripped, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encoding the unsigned event: %v", err)
	}
	return stripped
}

// roomMembers reads the room's member list as the local user sees it.
func roomMembers(t *testing.T, alice *harness.Account, server *harness.Server, roomID string) map[string]bool {
	t.Helper()

	var page struct {
		Chunk []struct {
			Type     string `json:"type"`
			StateKey string `json:"state_key"`
			Content  struct {
				Membership string `json:"membership"`
			} `json:"content"`
		} `json:"chunk"`
	}
	response := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/members")
	response.MustStatus(t, http.StatusOK)
	response.JSON(t, &page)

	members := map[string]bool{}
	for _, event := range page.Chunk {
		if event.Type == "m.room.member" && event.Content.Membership == "join" {
			members[event.StateKey] = true
		}
	}
	return members
}

// assertRoomAbsent checks that a refused join left nothing behind, which is the part a caller is
// likely to get wrong: the join failed, and yet the room is now in the user's list with no state, or
// has a membership row with no events to go with it.
func assertRoomAbsent(t *testing.T, alice *harness.Account, server *harness.Server, roomID string) {
	t.Helper()

	if _, ok := alice.MustSync(t, server, nil).Rooms.Join[roomID]; ok {
		t.Errorf("a refused federated join still left the room in the user's sync: %s", roomID)
	}
	// Deliberately not MustGet: for a refused join the point is that this answer is *not* a
	// success, and a helper that fails on 404 would turn the right behaviour into a test failure.
	response := server.Get(t, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/state",
		alice.AccessToken)
	if response.StatusCode == http.StatusOK {
		t.Errorf("a refused federated join still left state stored for the room: %s", roomID)
	}
}

// TestAFederatedJoinOfARejoiningUser is the answer a correct peer sends, and the one an over-strict
// check breaks.
//
// A send_join response carries the room's state as it was *before* the join, plus the join event
// itself. For a user who had been in the room and left, that means the state legitimately holds two
// m.room.member events for the same user: the leave the room was in, and the join that ended it. A
// server that insists on one member event per user — reasonable-sounding, and what this server used to
// do — refuses the rejoin of anybody who ever left, and would refuse an invited user's join for the
// same reason.
//
// The room is public here rather than invite-only because an invite carries a second requirement of its
// own (the invited user's server has to co-sign it, which this server does not yet do — see T714), and
// this test is about the shape of the answer, not about the room's rules.
func TestAFederatedJoinOfARejoiningUser(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	room := newFederatedRoom(t, peer)

	localUser := "@host:" + string(harness.ServerName)
	room.priorMember = buildRemoteEvent(t, peer, room.version, gomatrixserverlib.ProtoEvent{
		Type:     "m.room.member",
		RoomID:   room.roomID,
		StateKey: &localUser,
		// The peer's own moderator removed this user, which is why the peer can sign it: an event is
		// signed by its sender's server, not by whoever relays it.
		SenderID: room.remote,
		Depth:    6,
		Content:  spec.RawJSON(mustMarshal(t, map[string]any{"membership": "leave"})),
	})
	serveFederatedRoom(t, peer, room)

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
		alice.AccessToken, map[string]any{})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a user who had left was refused a federated rejoin (%d): %s",
			response.StatusCode, response.Body)
	}
	if got := federationMemberCount(t, alice, server, room.roomID); got != 2 {
		t.Fatalf("the federated room holds %d member events, want 2 (the leave and the join)", got)
	}
}

// federationMemberCount reads a room's member list back through the client API, which is the only side
// of this a user can see.
func federationMemberCount(t *testing.T, account *harness.Account, server *harness.Server,
	roomID string) int {
	t.Helper()
	response := account.MustGet(t, server, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/members")
	var page struct {
		Chunk []json.RawMessage `json:"chunk"`
	}
	if err := json.Unmarshal(response.Body, &page); err != nil {
		t.Fatalf("the members list is not the documented shape: %v (%s)", err, response.Body)
	}
	return len(page.Chunk)
}

// TestAFederatedJoinRefusesATemplateWithAnIncompleteChain is a peer that offers a join whose auth
// events do not include everything the room's rules need.
//
// Authorisation reads the auth events an event *names*, which is correct and also a way for a rule to
// go unread: a join that names the create event and the power levels, but not the room's join rules, is
// checked against no join rule at all — and a missing join rule does not refuse anybody, it reads as
// "public". So this server works out what the state *would* require for this join and refuses when the
// join left one out, rather than letting the absence stand for permission.
func TestAFederatedJoinRefusesATemplateWithAnIncompleteChain(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	room := newFederatedRoom(t, peer)

	// A peer that forgets the join rules: the template is otherwise well-formed.
	peer.serves = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/make_join/"):
			template := gomatrixserverlib.ProtoEvent{
				Type:       "m.room.member",
				RoomID:     room.roomID,
				StateKey:   strPointer(makeJoinUser(r.URL.Path)),
				Depth:      5,
				PrevEvents: []string{room.state[3].EventID()},
				AuthEvents: []string{room.state[0].EventID(), room.state[1].EventID()},
				Content:    spec.RawJSON(mustMarshal(t, map[string]any{"membership": "join"})),
			}
			raw := mustMarshal(t, map[string]any{"room_version": room.version, "event": template})
			writePeerJSON(w, raw)
			return true
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v2/send_join/"):
			serveSendJoin(t, peer, room, w, r)
			return true
		default:
			return false
		}
	}

	response := server.PostJSON(t, "/_matrix/client/v3/join/"+
		url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
		alice.AccessToken, map[string]any{})
	t.Logf("INCOMPLETE-CHAIN %d %s", response.StatusCode, string(response.Body))
	if response.StatusCode == http.StatusOK {
		t.Fatal("a join that never mentioned the room's join rules was accepted")
	}
	if body := string(response.Body); !strings.Contains(body, "m.room.join_rules") {
		t.Errorf("the refusal should name the rule the join left out, said: %s", body)
	}
	assertRoomAbsent(t, alice, server, room.roomID)
}

// TestAFederatedJoinIsAcceptedWhenThePeerOmitsTheJoinFromItsState covers a peer that answers
// `send_join` with the state *as it stood before* the join.
//
// The specification describes the answer as the room's current state, and Synapse includes the join
// event in it. complement's own reference federation server — the thing test suites worldwide use to
// stand in for a remote homeserver — deliberately builds its state list before inserting the event, so
// the join it has just accepted is missing from the list it returns. Refusing that answer means
// refusing a server that said yes: the peer authenticated the request, signed its state, and replied
// 200. Whether the join event rides inside the state array tells this server nothing it does not
// already know, because the join is the event this server built itself from the peer's own template
// and verified against the peer's signing key.
func TestAFederatedJoinIsAcceptedWhenThePeerOmitsTheJoinFromItsState(t *testing.T) {
	// Two shapes of the same answer: a room that never saw this user before, and a room holding an
	// older membership event for them. The second is the interesting one — the state does name the
	// user, with a membership that is not the join being made.
	for _, shape := range []struct {
		name       string
		priorLeave bool
	}{
		{name: "a user the room has never had"},
		{name: "a user who had left", priorLeave: true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
			alice := server.Register(t, "host", "pw-host", false)
			peer := newFakePeer(t)
			room := newFederatedRoom(t, peer)

			if shape.priorLeave {
				localUser := "@host:" + string(harness.ServerName)
				room.priorMember = buildRemoteEvent(t, peer, room.version, gomatrixserverlib.ProtoEvent{
					Type:     "m.room.member",
					RoomID:   room.roomID,
					StateKey: &localUser,
					SenderID: room.remote,
					Depth:    6,
					Content:  spec.RawJSON(mustMarshal(t, map[string]any{"membership": "leave"})),
				})
			}
			room.broken = func(answer *federatedJoinAnswer) {
				// The join event is the entry the harness appended last.
				answer.State = answer.State[:len(answer.State)-1]
			}
			serveFederatedRoom(t, peer, room)

			response := server.PostJSON(t, "/_matrix/client/v3/join/"+
				url.PathEscape(room.roomID)+"?server_name="+url.QueryEscape(string(peer.serverNo)),
				alice.AccessToken, map[string]any{})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("a peer that accepted the join was treated as refusing it (%d): %s",
					response.StatusCode, response.Body)
			}

			// Accepting the answer must still produce a room this user is in, with the remote
			// member in it and their own join as the membership of record.
			sync := alice.MustSync(t, server, nil)
			if _, ok := sync.Rooms.Join[room.roomID]; !ok {
				t.Fatalf("the room is not in the local user's sync (%d joined rooms)",
					len(sync.Rooms.Join))
			}
			members := roomMembers(t, alice, server, room.roomID)
			if !members[room.remote] {
				t.Errorf("the remote member %s is missing from the member list (%v)", room.remote, members)
			}
			if count := federationMemberCount(t, alice, server, room.roomID); count != 2 {
				t.Errorf("the room holds %d member events, want 2 (the state's own membership and this join)",
					count)
			}
		})
	}
}
