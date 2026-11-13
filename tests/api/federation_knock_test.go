package api_test

// Knocking on a room whose members live somewhere else.
//
// A knock is the one room operation whose whole point is to reach people who have not agreed to talk yet,
// which makes it the least forgiving handshake here. The knocking server holds nothing about the room: not
// its version, not where its graph ends, not which events authorise a change in it. It has to ask the kind
// of stranger it is trying to join — and then sign an event built from that stranger's answer.
//
// So the test runs the whole way between two real homeservers, because each half alone can look correct
// while the pair is broken. A draft that arrives with somebody else's identity in it would come back
// signed by this server; a knock that is accepted but never told to the room leaves the user waiting in a
// doorway nobody is watching; and a room that answers with nothing leaves the knocking user with a request
// they cannot even confirm they sent.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

func TestAUserKnocksOnARoomHeldByAnotherServer(t *testing.T) {
	// `hosts` runs the room; `knocksFrom` runs the user who wants in.
	hosts := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hosts.FederationServer(t)
	alice := hosts.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, hosts, map[string]any{
		"room_version": "7", "preset": "private_chat",
	})
	alice.MustSetState(t, hosts, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})

	knocksFrom := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	knocksFrom.FederationServer(t)
	bob := knocksFrom.Register(t, "knocker", "pw-knock", false)

	const reason = "I work with alice on the plumbing"
	response := bob.MustPostJSON(t, knocksFrom,
		"/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/knock?via="+
			url.QueryEscape(hosts.ServerName),
		map[string]any{"reason": reason})
	if response.StatusCode != 200 {
		t.Fatalf("a knock on another server's room got %d: %s", response.StatusCode, response.Body)
	}

	// The knocking user's own server has to show the knock as outstanding. Without this the client has
	// no way to tell "the room is considering" from "the request never left the building", and the user
	// knocks again, forever.
	var member map[string]any
	for attempt := 0; attempt < 20 && member == nil; attempt++ {
		sync := bob.MustSync(t, knocksFrom, url.Values{})
		events := sync.Rooms.Knock[roomID].KnockState.Events
		if len(events) == 0 {
			continue
		}
		member = memberEventFor(t, events, bob.UserID)
	}
	if member == nil {
		t.Fatalf("bob's server shows no knock in the knock section of his sync; the request left and vanished")
	}
	content, _ := member["content"].(map[string]any)
	if content["membership"] != "knock" {
		t.Fatalf("bob's own server says his membership here is %v, want knock", content["membership"])
	}
	if content["reason"] != reason {
		t.Fatalf("the reason did not survive the round trip: %v", content)
	}

	// And the room was told. This is the difference between a knock and a private note: somebody in
	// there has to be able to answer, which means the membership has to arrive as a fact about the room
	// and not as a rumour.
	answered := false
	for attempt := 0; attempt < 20 && !answered; attempt++ {
		sync := alice.MustSync(t, hosts, url.Values{})
		answered = membershipFor(t, sync.Rooms.Join[roomID].Timeline.Events, bob.UserID) == "knock"
	}
	if !answered {
		joined := alice.MustGet(t, hosts,
			"/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/state/m.room.member/"+
				harness.PathEscape(bob.UserID))
		t.Fatalf("the room never learned that %s knocked; its own members see %d %s",
			bob.UserID, joined.StatusCode, joined.Body)
	}
}

func TestAUserCannotKnockOnAnotherServersRoomThatDoesNotAdmitKnocks(t *testing.T) {
	hosts := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hosts.FederationServer(t)
	alice := hosts.Register(t, "host", "pw-host", false)
	// The default for `private_chat` is an invite-only room, which is a door that says no.
	roomID := alice.MustCreateRoom(t, hosts, map[string]any{"room_version": "7", "preset": "private_chat"})

	knocksFrom := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	knocksFrom.FederationServer(t)
	bob := knocksFrom.Register(t, "knocker", "pw-knock", false)

	// Not the `Must` helper: this knock is expected to be refused, and a helper that insists on 2xx
	// would fail the test before its own assertion could say why the refusal matters.
	response := bob.Do(t, knocksFrom, harness.Request{
		Method: http.MethodPost,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/knock?via=" +
			url.QueryEscape(hosts.ServerName),
		Body: map[string]any{"reason": "Let me in?"},
	})
	if response.StatusCode == 200 {
		t.Fatalf("a stranger knocked their way into an invite-only room on another server: %s",
			response.Body)
	}
	// The refusal has to arrive *as a refusal*. A room that said "this room is invite-only" and a server
	// that was unreachable both reaching the client as "no such room" would teach the user to stop
	// asking questions that have answers.
	if response.StatusCode != 403 {
		var body struct {
			ErrCode string `json:"errcode"`
			Message string `json:"error"`
		}
		response.JSON(t, &body)
		t.Fatalf("a knock refused by the room reached the client as %d %s (%s), want the room's own 403",
			response.StatusCode, body.ErrCode, body.Message)
	}

	// A refused knock is not stored as though it had happened: bob's server must not show a knock on a
	// room that turned it away, or he will wait for an answer that was never going to come.
	sync := bob.MustSync(t, knocksFrom, url.Values{})
	if _, ok := sync.Rooms.Knock[roomID]; ok {
		t.Fatalf("bob's server recorded a knock that was refused: %s",
			marshalKnockState(t, sync.Rooms.Knock[roomID].KnockState.Events))
	}
}

func marshalKnockState(t *testing.T, events []json.RawMessage) string {
	t.Helper()
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("the knock state cannot be shown: %v", err)
	}
	return string(raw)
}

func TestASecondKnockOnAnotherServersRoomIsTakenToTheRoom(t *testing.T) {
	hosts := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hosts.FederationServer(t)
	alice := hosts.Register(t, "host", "pw-host", false)
	roomID := alice.MustCreateRoom(t, hosts, map[string]any{
		"room_version": "7", "preset": "private_chat",
	})
	alice.MustSetState(t, hosts, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})

	knocksFrom := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	knocksFrom.FederationServer(t)
	bob := knocksFrom.Register(t, "knocker", "pw-knock", false)

	knock := func(reason string) *harness.Response {
		return bob.MustPostJSON(t, knocksFrom,
			"/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/knock?via="+
				url.QueryEscape(hosts.ServerName), map[string]any{"reason": reason})
	}
	if response := knock("First question"); response.StatusCode != 200 {
		t.Fatalf("the first knock got %d: %s", response.StatusCode, response.Body)
	}
	// The second knock is the interesting one. After the first, this server holds a *description* of
	// the room — a report from the server that holds it — and answering a knock from that would both
	// trust the report and leave the room's members untold. A repeat knock goes to the room again.
	second := knock("I wanted to add: I bring tools")
	if second.StatusCode != 200 {
		t.Fatalf("a repeat knock got %d: %s", second.StatusCode, second.Body)
	}

	// And the room was told twice: its own view of the knocker carries the newest reason.
	seen := ""
	for attempt := 0; attempt < 20 && seen == ""; attempt++ {
		state := alice.MustGet(t, hosts,
			"/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/state/m.room.member/"+
				harness.PathEscape(bob.UserID))
		var content map[string]any
		if state.StatusCode == 200 {
			state.JSON(t, &content)
			if content["membership"] == "knock" && content["reason"] != "First question" {
				seen = content["reason"].(string)
			}
		}
	}
	if seen == "" {
		t.Fatalf("the room never saw the repeat knock; its view of the knocker stayed on the first one")
	}
}

func TestARefusedRemoteJoinReachesTheClientAsARefusal(t *testing.T) {
	hosts := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hosts.FederationServer(t)
	alice := hosts.Register(t, "host", "pw-host", false)
	// An invite-only room that asks to be knocked on: joining it cold is refused twice over — by the
	// lack of an invite, and by a join rule that does not admit a bare join.
	roomID := alice.MustCreateRoom(t, hosts, map[string]any{
		"room_version": "7", "preset": "private_chat",
	})
	alice.MustSetState(t, hosts, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})

	knocksFrom := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	knocksFrom.FederationServer(t)
	bob := knocksFrom.Register(t, "knocker", "pw-knock", false)

	response := bob.Do(t, knocksFrom, harness.Request{
		Method: http.MethodPost,
		Path: "/_matrix/client/v3/join/" + harness.PathEscape(roomID) + "?server_name=" +
			url.QueryEscape(hosts.ServerName),
		Body: map[string]any{},
	})
	if response.StatusCode == 200 {
		t.Fatalf("a stranger joined a knock-only room on another server: %s", response.Body)
	}
	if response.StatusCode != 403 {
		t.Fatalf("the room's refusal reached the client as %d (%s), want the refusal itself",
			response.StatusCode, response.Body)
	}
}
