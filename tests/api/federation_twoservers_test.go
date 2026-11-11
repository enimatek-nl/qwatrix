package api_test

import (
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// reserveFreePort reserves a port by binding and releasing it. The gap between releasing and binding is a risk
// a test is allowed to take; the alternative — a fixed port — is worse the moment two runs happen at
// once, and a failure that depends on what else is using the machine is not evidence about anything.
func reserveFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return port
}

// dialableServerName names a homeserver by an address it can actually be reached on, because a federation
// signature covers the server name that made it: a server whose name and listener disagree is not the
// same server twice.
func dialableServerName(t *testing.T) string {
	t.Helper()
	return "127.0.0.1:" + strconv.Itoa(reserveFreePort(t))
}

func roomIDFromJoinAnswer(t *testing.T, body []byte) string {
	t.Helper()
	var answer struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer is not the documented shape: %v (%s)", err, body)
	}
	return answer.RoomID
}

func federatedMemberKeys(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var page struct {
		Chunk []struct {
			StateKey string `json:"state_key"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("the members list is not the documented shape: %v (%s)", err, body)
	}
	seen := make(map[string]bool, len(page.Chunk))
	for _, member := range page.Chunk {
		seen[member.StateKey] = true
	}
	return seen
}

func namesInSet(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	return out
}

// TestTwoHomeserversFederateAJoin is two qwatrix servers, neither of them a test double, doing the join
// handshake over TLS: hs2 holds the room, hs1 has the user, and hs1 asks hs2 to let them in.
//
// Every other federation test in this package drives one real server with a fake peer that answers
// whatever it is asked. Those are the right place to ask *what does this server accept*, because a fake
// can be made to answer in shapes no real server would. This one asks the question a fake cannot: does a
// request this server signs survive verification by another server applying the same rules — including
// this server's own. A client that mis-signs and a server that mis-verifies are invisible to each other
// when the counterparty is a stub, and this is precisely the link where they would cancel out: the
// signature covers the method, the URI and the body, so only a second implementation of the check finds
// out that the two ends disagree.
func TestTwoHomeserversFederateAJoin(t *testing.T) {
	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t) // binds the address its own server name names
	remote := hs2.Register(t, "remotehost", "pw-remote", false)

	created := remote.MustPostJSON(t, hs2, "/_matrix/client/v3/createRoom", map[string]any{
		"name":         "held elsewhere",
		"preset":       "public_chat",
		"room_version": "11",
		"visibility":   "private",
	})
	roomID := roomIDFromJoinAnswer(t, created.Body)
	remote.MustPutJSON(t, hs2, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+
		"/send/m.room.message/"+harness.Nonce("held"),
		map[string]any{"msgtype": "m.text", "body": "written elsewhere"})

	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)

	joined := alice.MustPostJSON(t, hs1, "/_matrix/client/v3/join/"+url.PathEscape(roomID)+
		"?server_name="+url.QueryEscape(hs2.ServerName), map[string]any{})
	if got := roomIDFromJoinAnswer(t, joined.Body); got != roomID {
		t.Fatalf("hs1 joined room %q, wanted %q", got, roomID)
	}

	// A join this server merely recorded and a room the user is in are different things: the members
	// the other server described have to be readable from this side, or there is no room here to
	// paginate, no one to encrypt to, and nothing for a client to show.
	seen := federatedMemberKeys(t, alice.MustGet(t, hs1,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/members").Body)
	if !seen["@alice:"+hs1.ServerName] {
		t.Errorf("after a federated join the room's members are %v, which does not include the user who joined", namesInSet(seen))
	}
	if !seen["@remotehost:"+hs2.ServerName] {
		t.Errorf("after a federated join the room's members are %v, which does not include the user who lives on the other server", namesInSet(seen))
	}
}

// TestAHomeserverNamedWithoutAPortFederatesAJoin is the same handshake with the other server called by
// a name that carries no port — "localhost", reached on the federation port the specification defines.
//
// It is a separate test rather than a variation on the one above because that is where the containers and
// this test suite part company: Complement names its homeservers hs1 and hs2, with no port, and every
// harness server names itself with one. A federation signature covers the URI and the server names, so
// "the name had better resolve to the port I actually listen on" is a property a test either has or does
// not, and a join that only works when the name spells the port is not a join that works on a network.
func TestAHomeserverNamedWithoutAPortFederatesAJoin(t *testing.T) {
	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName("localhost"), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	if hs2.FederationServer(t).URL == "" {
		t.Skip("the federation address for a port-less name is not listening")
	}
	remote := hs2.Register(t, "remotehost", "pw-remote", false)
	created := remote.MustPostJSON(t, hs2, "/_matrix/client/v3/createRoom", map[string]any{
		"name": "held elsewhere, addressed by name", "preset": "public_chat", "room_version": "11",
	})
	roomID := roomIDFromJoinAnswer(t, created.Body)

	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)

	joined := alice.MustPostJSON(t, hs1, "/_matrix/client/v3/join/"+url.PathEscape(roomID)+
		"?server_name=localhost", map[string]any{})
	if got := roomIDFromJoinAnswer(t, joined.Body); got != roomID {
		t.Fatalf("hs1 joined room %q, wanted %q", got, roomID)
	}
}

// TestAMessageTravelsToAServerThatSharesTheRoom is the outbound direction of the join tests above:
// alice, on one homeserver, says something in a room that also has members on another, and the other
// homeserver ends up with what she said.
//
// Until this test the server could only ever *receive*. A homeserver that can be written to but cannot
// speak is a homeserver whose users can read a room and never answer it, which looks like a network
// problem to everybody involved and is not one.
func TestAMessageTravelsToAServerThatSharesTheRoom(t *testing.T) {
	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	bob := hs2.Register(t, "bob", "pw-bob", false)
	roomID := bob.MustCreateRoom(t, hs2, map[string]any{"preset": "public_chat", "name": "shared"})

	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	alice.MustPostJSON(t, hs1, "/_matrix/client/v3/join/"+url.PathEscape(roomID)+
		"?server_name="+url.QueryEscape(hs2.ServerName), map[string]any{}).MustStatus(t, 200)

	for _, body := range []string{"hello over federation", "and again"} {
		alice.MustSendText(t, hs1, roomID, body)
	}

	// Delivery is a queue with retries, so draining once is allowed to be partial; three passes is the
	// same thing the production worker would do inside a few seconds.
	delivered := 0
	for attempt := 0; attempt < 3 && len(messageBodies(messagesOf(t, bob, hs2, roomID))) < 2; attempt++ {
		delivered += hs1.DrainFederation(t)
	}
	bodies := messageBodies(messagesOf(t, bob, hs2, roomID))
	for _, want := range []string{"hello over federation", "and again"} {
		found := false
		for _, body := range bodies {
			if body == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%q never reached the other homeserver, which holds %v (%d events delivered)\n"+
				"HS1 LOG:\n%s\nHS2 LOG:\n%s",
				want, bodies, delivered, hs1.Logs(), hs2.Logs())
		}
	}
}
