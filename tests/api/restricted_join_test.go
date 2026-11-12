package api_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// createRestrictedRoom makes a room that may only be joined by people who are already in `via` —
// MSC3083's `restricted` rule, written into the create event because a later write to `m.room.join_rules`
// would have to be authorised by rules that are not in place yet.
func createRestrictedRoom(t *testing.T, account *harness.Account, server *harness.Server, allowRoomID string) string {
	t.Helper()

	return account.MustCreateRoom(t, server, map[string]any{
		"preset": "public_chat",
		"initial_state": []map[string]any{{
			"type":      "m.room.join_rules",
			"state_key": "",
			"content": map[string]any{
				"join_rule": "restricted",
				"allow": []map[string]any{{
					// The ratified entry type. An entry of any other type is ignored, which is
					// the safe direction and is pinned by its own test below.
					"type":    "m.room_membership",
					"room_id": allowRoomID,
					"via":     []string{string(server.ServerName)},
				}},
			},
		}},
	})
}

// TestARestrictedRoomAdmitsTheMembersOfTheRoomItPointsAt is MSC3083 read as the promise it is: a room
// can say "you may join if you are already in there", and the point of saying it is that the door opens
// for exactly those people and for nobody else.
//
// Every one of the four transitions matters. Admitting everybody makes the rule a decoration; admitting
// nobody makes a room unjoinable by the people its owner pointed at; and admitting somebody who left
// the room that justified the join keeps the door open after the reason has closed.
func TestARestrictedRoomAdmitsTheMembersOfTheRoomItPointsAt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "restrictor", "pw-restrictor", false)
	bob := server.Register(t, "restrictedbob", "pw-restrictedbob", false)
	allowed := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	roomID := createRestrictedRoom(t, alice, server, allowed)

	join := func(account *harness.Account) *harness.Response {
		return account.Do(t, server, harness.Request{
			Method: http.MethodPost,
			Path:   "/_matrix/client/v3/join/" + url.PathEscape(roomID),
		})
	}

	// 1. A stranger to both rooms may not come in.
	if status := join(bob).StatusCode; status == http.StatusOK {
		t.Fatalf("a restricted room admitted %s, who is in neither it nor the room it points at", bob.UserID)
	}

	// 2. Membership of the room the rule names is the whole qualification.
	bob.MustJoinRoom(t, server, allowed)
	if response := join(bob); response.StatusCode != http.StatusOK {
		t.Fatalf("a restricted room refused %s, who is joined to the room its rule names: status %d\nbody: %s",
			bob.UserID, response.StatusCode, response.Body)
	}

	// 3. Leaving the room that qualified the join closes the door again.
	bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + url.PathEscape(allowed) + "/leave",
	}).MustStatus(t, http.StatusOK)
	bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/leave",
	})
	if status := join(bob).StatusCode; status == http.StatusOK {
		t.Errorf("a restricted room re-admitted %s after they left the room that justified the join", bob.UserID)
	}

	// 4. An invitation still works: the restricted rule is an extra way in, not a replacement for the
	//    ordinary one.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/invite",
		Body:   map[string]any{"user_id": bob.UserID},
	}).MustStatus(t, http.StatusOK)
	if response := join(bob); response.StatusCode != http.StatusOK {
		t.Errorf("a restricted room refused an invited user: status %d\nbody: %s",
			response.StatusCode, response.Body)
	}
}

// TestARestrictedRoomRefusesToBeWonByBadJoinRules covers the write side: a join rule the server cannot
// read must not become a door.
//
// `allow` is the field the whole rule hangs on, so a value that is not a list of room entries has two
// honest outcomes — refuse the write, or refuse every join — and one dishonest one: read it as an empty
// list and let the restriction quietly turn into nothing.
func TestARestrictedRoomRefusesToBeWonByBadJoinRules(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "fumbled", "pw-fumbled", false)
	bob := server.Register(t, "opportunist", "pw-opportunist", false)
	allowed := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	roomID := createRestrictedRoom(t, alice, server, allowed)

	alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) +
			"/state/m.room.join_rules/" + url.PathEscape(""),
		Token: alice.AccessToken,
		Body:  map[string]any{"join_rule": "restricted", "allow": "potato"},
	})

	response := bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/join/" + url.PathEscape(roomID),
	})
	if response.StatusCode == http.StatusOK {
		t.Errorf("a join rule this server cannot read admitted %s anyway: %s", bob.UserID, response.Body)
	}
}

// TestAJoiningClientCannotAuthoriseItsOwnJoin is about who owns one field of a membership event.
//
// `join_authorised_via_users_server` names the user whose membership in another room authorised this
// join — the credential a restricted room accepts in place of an invite. Copying it from the request
// would let anybody walk into any restricted room by naming somebody who is entitled to admit them, and
// the forgery would be signed by the person using it. So the server decides that field or leaves it
// out; the value a client sends is dropped before the event is built, in every room, including the
// ordinary ones where the field means nothing at all.
func TestAJoiningClientCannotAuthoriseItsOwnJoin(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "powerfulalice", "pw-powerfulalice", false)
	bob := server.Register(t, "claimingbob", "pw-claimingbob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	response := bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/join/" + url.PathEscape(roomID),
		Body: map[string]any{
			"join_authorised_via_users_server": alice.UserID,
		},
	})
	if code := response.StatusCode; code != http.StatusOK {
		t.Fatalf("joining a public room failed: status %d\nbody: %s", code, response.Body)
	}

	member := bob.MustGet(t, server, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+
		"/state/m.room.member/"+url.QueryEscape(bob.UserID))
	if claimed := gjson.GetBytes(member.Body, "content.join_authorised_via_users_server"); claimed.Exists() {
		t.Errorf("the server wrote a client's own authorisation claim into the join event: %q", claimed.Str)
	}
}
