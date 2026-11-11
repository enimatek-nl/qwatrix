package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// createSpace makes a space and returns its room ID. A space is an ordinary room whose create event
// carries a type, which is why it needs no special endpoint to make.
func createSpace(t *testing.T, account *harness.Account, server *harness.Server, name string) string {
	t.Helper()
	return account.MustCreateRoom(t, server, map[string]any{
		"preset":           "public_chat",
		"name":             name,
		"creation_content": map[string]any{"type": "m.space"},
	})
}

// TestRoomSummaryNamesTheRoomsARestrictedJoinAccepts covers GET /_matrix/client/v1/room_summary/{room}.
//
// The field that matters here is `allowed_room_ids`: a restricted room can be joined by members of
// other rooms, and a client that cannot see which ones can only offer the user a join attempt and
// explain the failure afterwards. Telling them beforehand is the difference between a locked door with a
// sign on it and a locked door.
func TestRoomSummaryNamesTheRoomsARestrictedJoinAccepts(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)

	space := createSpace(t, alice, server, "Office")
	restricted := alice.MustCreateRoom(t, server, map[string]any{
		"preset":       "public_chat",
		"room_version": "8",
		"name":         "Restricted",
		"initial_state": []map[string]any{{
			"type":      "m.room.join_rules",
			"state_key": "",
			"content": map[string]any{
				"join_rule": "restricted",
				"allow": []map[string]any{
					{"type": "m.room_membership", "room_id": space},
				},
			},
		}},
	})
	inviteOnly := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat", "name": "Private"})

	summary := roomSummaryOf(t, alice, server, restricted)
	if summary.RoomID != restricted {
		t.Errorf("room_id = %q, want %q", summary.RoomID, restricted)
	}
	if summary.JoinRule != "restricted" {
		t.Errorf("join_rule = %q, want restricted", summary.JoinRule)
	}
	if len(summary.AllowedRoomIDs) != 1 || summary.AllowedRoomIDs[0] != space {
		t.Errorf("allowed_room_ids = %v, want [%s]", summary.AllowedRoomIDs, space)
	}
	if summary.Name != "Restricted" {
		t.Errorf("name = %q, want Restricted", summary.Name)
	}
	if summary.RoomVersion == "" {
		t.Error("room_version is empty: a client negotiating a join needs to know what it is being joined into")
	}
	if summary.NumJoined != 1 {
		t.Errorf("num_joined_members = %d, want 1", summary.NumJoined)
	}

	// A room that is not restricted must not carry the field at all. An empty list would tell a client
	// "restricted by nobody", which is a different claim from "not restricted".
	plain := roomSummaryOf(t, alice, server, inviteOnly)
	if plain.AllowedRoomIDs != nil {
		t.Errorf("an invite-only room reported allowed_room_ids %v, want the field absent", plain.AllowedRoomIDs)
	}
	if plain.JoinRule != "invite" {
		t.Errorf("join_rule = %q, want invite", plain.JoinRule)
	}
}

// TestRoomSummaryFollowsAnAlias is the other half of the same endpoint: the path parameter is a room ID
// *or alias*, and a client that resolved one should not have to care which it sent.
func TestRoomSummaryFollowsAnAlias(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Named"})

	alias := "#office:" + server.ServerName
	alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   "/_matrix/client/v3/directory/room/" + url.PathEscape(alias),
		Body:   map[string]any{"room_id": roomID},
	}).MustStatus(t, 200)
	alice.MustSetState(t, server, roomID, "m.room.canonical_alias", "", map[string]any{"alias": alias})

	summary := roomSummaryOf(t, alice, server, alias)
	if summary.RoomID != roomID {
		t.Errorf("room_id = %q for an alias lookup, want %q", summary.RoomID, roomID)
	}
}

// TestRoomSummaryRefusesARoomThatIsNotThere pins the refusal: this is a named room, and there is none.
func TestRoomSummaryRefusesARoomThatIsNotThere(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	// MustGet would fail the test on the non-2xx this case is about, so the request is made directly.
	response := alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v1/room_summary/" + url.PathEscape("!nope:"+server.ServerName),
	})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", response.StatusCode, response.Body)
	}
}

// roomSummary is the response of GET /_matrix/client/v1/room_summary/{room}.
type roomSummary struct {
	RoomID         string   `json:"room_id"`
	Name           string   `json:"name"`
	Topic          string   `json:"topic"`
	CanonicalAlias string   `json:"canonical_alias"`
	RoomType       string   `json:"room_type"`
	RoomVersion    string   `json:"room_version"`
	NumJoined      int      `json:"num_joined_members"`
	WorldReadable  bool     `json:"world_readable"`
	GuestCanJoin   bool     `json:"guest_can_join"`
	JoinRule       string   `json:"join_rule"`
	AllowedRoomIDs []string `json:"allowed_room_ids"`
}

func roomSummaryOf(t *testing.T, account *harness.Account, server *harness.Server, roomIDOrAlias string) roomSummary {
	t.Helper()
	response := account.MustGet(t, server, "/_matrix/client/v1/room_summary/"+url.PathEscape(roomIDOrAlias))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /room_summary/%s: status %d, want 200\nbody: %s", roomIDOrAlias, response.StatusCode, response.Body)
	}
	var summary roomSummary
	if err := json.Unmarshal(response.Body, &summary); err != nil {
		t.Fatalf("decoding the room summary: %v\n%s", err, response.Body)
	}
	return summary
}
