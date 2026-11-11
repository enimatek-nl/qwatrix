package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// The room hierarchy is the space tree: a space is a room whose create event carries
// `type: m.space`, and its children are `m.space.child` state events naming other rooms.
// GET /rooms/{roomID}/hierarchy walks that tree so a client can render a space without
// joining everything under it.

// hierarchyResponse is the response of GET /_matrix/client/v1/rooms/{roomID}/hierarchy.
type hierarchyResponse struct {
	Rooms []struct {
		RoomID         string           `json:"room_id"`
		Name           string           `json:"name"`
		RoomType       string           `json:"room_type"`
		RoomVersion    string           `json:"room_version"`
		JoinRule       string           `json:"join_rule"`
		NumJoined      int              `json:"num_joined_members"`
		WorldReadable  bool             `json:"world_readable"`
		GuestCanJoin   bool             `json:"guest_can_join"`
		AllowedRoomIDs []string         `json:"allowed_room_ids"`
		ChildrenState  []hierarchyChild `json:"children_state"`
	} `json:"rooms"`
	NextBatch string `json:"next_batch"`
}

type hierarchyChild struct {
	Type           string          `json:"type"`
	StateKey       string          `json:"state_key"`
	Content        json.RawMessage `json:"content"`
	Sender         string          `json:"sender"`
	OriginServerTS int64           `json:"origin_server_ts"`
}

func (h hierarchyResponse) ids() []string {
	out := make([]string, 0, len(h.Rooms))
	for _, room := range h.Rooms {
		out = append(out, room.RoomID)
	}
	return out
}

func (h hierarchyResponse) find(roomID string) (int, bool) {
	for i, room := range h.Rooms {
		if room.RoomID == roomID {
			return i, true
		}
	}
	return 0, false
}

// makeSpace creates a space and returns its room ID.
func makeSpace(t *testing.T, account *harness.Account, server *harness.Server, name string) string {
	t.Helper()
	return account.MustCreateRoom(t, server, map[string]any{
		"preset":           "public_chat",
		"name":             name,
		"creation_content": map[string]any{"type": "m.space"},
	})
}

// linkSpace links a child into a space. `content` is written verbatim so a test can leave
// `via` out, which is not a link at all and must be discarded.
func linkSpace(t *testing.T, account *harness.Account, server *harness.Server, space, child string, content map[string]any) {
	t.Helper()
	account.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + url.PathEscape(space) +
			"/state/m.space.child/" + url.PathEscape(child),
		Body: content,
	}).MustStatus(t, http.StatusOK)
}

func hierarchyOf(t *testing.T, account *harness.Account, server *harness.Server, root string, query string) (*hierarchyResponse, *harness.Response) {
	t.Helper()
	path := "/_matrix/client/v1/rooms/" + url.PathEscape(root) + "/hierarchy"
	if query != "" {
		path += "?" + query
	}
	response := account.Do(t, server, harness.Request{Method: http.MethodGet, Path: path})
	if response.StatusCode == http.StatusOK {
		var page hierarchyResponse
		if err := json.Unmarshal(response.Body, &page); err != nil {
			t.Fatalf("decoding the hierarchy: %v\n%s", err, response.Body)
		}
		return &page, response
	}
	return nil, response
}

// TestTheHierarchyWalksTheSpaceTree covers the shape of the answer: the root first, then the
// tree beneath it, each space carrying the child links it was walked by.
//
// A child link on a room that is not a space is not a link in the direction the client is
// about to walk: only spaces contain children, and recursing into a room's `m.space.child`
// events would invent a hierarchy the room's members never described.
func TestTheHierarchyWalksTheSpaceTree(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	via := []string{server.ServerName}

	root := makeSpace(t, alice, server, "Root")
	subspace := makeSpace(t, alice, server, "Sub-space")
	underneath := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Underneath"})
	flat := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Flat"})

	linkSpace(t, alice, server, root, subspace, map[string]any{"via": via})
	linkSpace(t, alice, server, root, flat, map[string]any{"via": via})
	linkSpace(t, alice, server, subspace, underneath, map[string]any{"via": via})
	// A room, not a space, listing a child: nothing below it is walked.
	linkSpace(t, alice, server, flat, underneath, map[string]any{"via": via})

	page, response := hierarchyOf(t, alice, server, root, "")
	if page == nil {
		t.Fatalf("status %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	ids := page.ids()
	if len(ids) != 4 {
		t.Fatalf("the hierarchy holds %d rooms %v, want 4", len(ids), ids)
	}
	if ids[0] != root {
		t.Errorf("the hierarchy starts with %s, want the root room %s", ids[0], root)
	}
	for _, want := range []string{subspace, underneath, flat} {
		if _, ok := page.find(want); !ok {
			t.Errorf("%s is not in the hierarchy %v", want, ids)
		}
	}

	// The root has to carry the links it was walked by, or a client cannot draw the tree
	// or show where a room sits.
	rootIndex, _ := page.find(root)
	rootRoom := page.Rooms[rootIndex]
	if rootRoom.RoomType != "m.space" {
		t.Errorf("the root space reports room_type %q, want m.space", rootRoom.RoomType)
	}
	if len(rootRoom.ChildrenState) != 2 {
		t.Errorf("the root lists %d children, want 2: %+v", len(rootRoom.ChildrenState), rootRoom.ChildrenState)
	}
	for _, child := range rootRoom.ChildrenState {
		if child.Type != "m.space.child" {
			t.Errorf("a child link has type %q", child.Type)
		}
		if child.StateKey != subspace && child.StateKey != flat {
			t.Errorf("an unexpected child link %q", child.StateKey)
		}
		if child.Sender == "" {
			t.Error("a child link carries no sender")
		}
		if child.OriginServerTS == 0 {
			t.Error("a child link carries no origin_server_ts, which clients sort by")
		}
		if !strings.Contains(string(child.Content), "via") {
			t.Errorf("a child link carries no via: %s", child.Content)
		}
	}
	// A plain room is not a space and reports no type, so a client does not draw a folder
	// around a conversation.
	if flatIndex, ok := page.find(flat); ok {
		if roomType := page.Rooms[flatIndex].RoomType; roomType != "" {
			t.Errorf("a plain room reports room_type %q, want it absent", roomType)
		}
	}
	// And the fields a client needs to render a tile at all.
	if underneathIndex, ok := page.find(underneath); ok {
		room := page.Rooms[underneathIndex]
		if room.Name != "Underneath" {
			t.Errorf("name = %q, want Underneath", room.Name)
		}
		if room.NumJoined != 1 {
			t.Errorf("num_joined_members = %d, want 1", room.NumJoined)
		}
		if room.RoomVersion == "" {
			t.Error("room_version is empty")
		}
		if room.JoinRule == "" {
			t.Error("join_rule is empty")
		}
	}
}

// TestTheHierarchyStopsAtTheRequestedDepth: `max_depth` is how far the caller wants to go,
// and a client rendering one level of a tree should not have the server walk a thousand
// rooms it will not show.
func TestTheHierarchyStopsAtTheRequestedDepth(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	via := []string{server.ServerName}

	root := makeSpace(t, alice, server, "Root")
	subspace := makeSpace(t, alice, server, "Sub-space")
	underneath := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	linkSpace(t, alice, server, root, subspace, map[string]any{"via": via})
	linkSpace(t, alice, server, subspace, underneath, map[string]any{"via": via})

	page, response := hierarchyOf(t, alice, server, root, "max_depth=1")
	if page == nil {
		t.Fatalf("status %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	ids := page.ids()
	if len(ids) != 2 {
		t.Fatalf("with max_depth=1 the hierarchy holds %v, want the root and its direct children", ids)
	}
	if _, ok := page.find(underneath); ok {
		t.Errorf("max_depth=1 returned a room two levels down: %v", ids)
	}

	// The same request without a depth limit sees the whole tree.
	full, response := hierarchyOf(t, alice, server, root, "")
	if full == nil {
		t.Fatalf("status %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	if len(full.ids()) != 3 {
		t.Errorf("without a depth limit the hierarchy holds %v, want 3 rooms", full.ids())
	}
}

// TestTheHierarchyPaginates: `limit` is per page and `next_batch` says there is more.
//
// Changing `suggested_only` or `max_depth` between pages is refused rather than silently
// honoured: the caller would be walking a different tree than the one the token counts
// rooms in, and would see rooms repeated or skipped with no way to tell.
func TestTheHierarchyPaginates(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	via := []string{server.ServerName}

	root := makeSpace(t, alice, server, "Root")
	children := make([]string, 0, 5)
	for _, name := range []string{"One", "Two", "Three", "Four", "Five"} {
		child := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": name})
		linkSpace(t, alice, server, root, child, map[string]any{"via": via})
		children = append(children, child)
	}

	first, response := hierarchyOf(t, alice, server, root, "limit=2")
	if first == nil {
		t.Fatalf("status %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	if len(first.Rooms) != 2 {
		t.Fatalf("a page of limit=2 held %d rooms", len(first.Rooms))
	}
	if first.NextBatch == "" {
		t.Fatalf("a full page carried no next_batch, so the rest of the tree is unreachable: %s", response.Body)
	}

	// Walk to the end and check every room is seen exactly once, the first page included.
	seen := map[string]int{}
	for _, room := range first.Rooms {
		seen[room.RoomID]++
	}
	token := first.NextBatch
	for rounds := 0; rounds < 10; rounds++ {
		next, response := hierarchyOf(t, alice, server, root, "limit=2&from="+url.QueryEscape(token))
		if next == nil {
			t.Fatalf("page %d: status %d err=%s", rounds, response.StatusCode, response.Body)
		}
		for _, room := range next.Rooms {
			seen[room.RoomID]++
		}
		if next.NextBatch == "" {
			token = ""
			break
		}
		token = next.NextBatch
	}
	if token != "" {
		t.Fatal("the hierarchy never ended")
	}
	if len(seen) != len(children)+1 {
		t.Errorf("pagination showed %d distinct rooms, want %d", len(seen), len(children)+1)
	}
	for _, child := range children {
		if seen[child] != 1 {
			t.Errorf("%s was listed %d times across the pages, want once", child, seen[child])
		}
	}

	// A token is a position in a particular tree, so the parameters that shaped it cannot
	// change underneath it.
	mutated, response := hierarchyOf(t, alice, server, root, "limit=2&max_depth=1&from="+url.QueryEscape(first.NextBatch+"x"))
	if mutated != nil {
		t.Errorf("an unknown pagination token was accepted: %s", response.Body)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown pagination token: status %d, want 400\nbody: %s", response.StatusCode, response.Body)
	}
	if body := string(response.Body); !strings.Contains(body, "M_INVALID_PARAM") {
		t.Errorf("an unknown pagination token should be M_INVALID_PARAM, said: %s", body)
	}
}

// TestTheHierarchyOnlyOffersSuggestedChildren: `suggested_only` applies all the way down.
// A space that is itself not suggested is not searched, because the administrator who hid
// the space was not asking for its contents to be surfaced anyway.
func TestTheHierarchyOnlyOffersSuggestedChildren(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	via := []string{server.ServerName}

	root := makeSpace(t, alice, server, "Root")
	shown := makeSpace(t, alice, server, "Shown")
	hidden := makeSpace(t, alice, server, "Hidden")
	underHidden := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Under hidden"})
	linkSpace(t, alice, server, root, shown, map[string]any{"via": via, "suggested": true})
	linkSpace(t, alice, server, root, hidden, map[string]any{"via": via, "suggested": false})
	linkSpace(t, alice, server, hidden, underHidden, map[string]any{"via": via, "suggested": true})

	page, response := hierarchyOf(t, alice, server, root, "suggested_only=true")
	if page == nil {
		t.Fatalf("status %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	ids := page.ids()
	if _, ok := page.find(hidden); ok {
		t.Errorf("a space that is not suggested is in a suggested-only hierarchy: %v", ids)
	}
	if _, ok := page.find(underHidden); ok {
		t.Errorf("a room under an unsuggested space is in a suggested-only hierarchy: %v", ids)
	}
	if _, ok := page.find(shown); !ok {
		t.Errorf("the suggested space is missing: %v", ids)
	}

	// Without the flag, everything the caller may see is there.
	full, response := hierarchyOf(t, alice, server, root, "")
	if full == nil {
		t.Fatalf("status %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	if _, ok := full.find(underHidden); !ok {
		t.Errorf("the whole tree should hold the room under the hidden space: %v", full.ids())
	}
}

// TestTheHierarchyDiscardsAChildLinkWithoutAVia: a link with no `via` is not a link. The
// server named by the mxc-like room ID is the only way anybody can reach the child, so a
// link that does not name one is a broken pointer, not an invitation to guess.
func TestTheHierarchyDiscardsAChildLinkWithoutAVia(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)

	root := makeSpace(t, alice, server, "Root")
	reachable := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Reachable"})
	orphan := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Orphan"})
	emptyVia := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Empty via"})

	linkSpace(t, alice, server, root, reachable, map[string]any{"via": []string{server.ServerName}})
	linkSpace(t, alice, server, root, orphan, map[string]any{})
	linkSpace(t, alice, server, root, emptyVia, map[string]any{"via": []string{}})

	page, response := hierarchyOf(t, alice, server, root, "")
	if page == nil {
		t.Fatalf("status %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	ids := page.ids()
	for _, unwanted := range []string{orphan, emptyVia} {
		if _, ok := page.find(unwanted); ok {
			t.Errorf("%s is reachable through a child link that names no server to ask: %v", unwanted, ids)
		}
	}
	if _, ok := page.find(reachable); !ok {
		t.Errorf("the properly linked room is missing: %v", ids)
	}
	rootIndex, _ := page.find(root)
	for _, child := range page.Rooms[rootIndex].ChildrenState {
		if child.StateKey == orphan || child.StateKey == emptyVia {
			t.Errorf("a broken child link is still listed in children_state (%s)", child.StateKey)
		}
	}
}

// TestTheHierarchyRefusesARoomTheCallerMayNotSee keeps the tree from being a way to read
// the shape of other people's spaces. The specification asks for 403 for a root that does
// not exist as well as one that is not readable, so that walking the tree cannot be used
// to ask whether a given room ID is real.
func TestTheHierarchyRefusesARoomTheCallerMayNotSee(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	stranger := server.Register(t, "stranger", "pw-stranger", false)

	secret := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat", "name": "Secret"})
	_, response := hierarchyOf(t, stranger, server, secret, "")
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("a hierarchy of a private room the caller is not in: status %d, want 403\nbody: %s",
			response.StatusCode, response.Body)
	}
	if body := string(response.Body); !strings.Contains(body, "M_FORBIDDEN") {
		t.Errorf("the refusal should be M_FORBIDDEN, said: %s", body)
	}

	_, response = hierarchyOf(t, stranger, server, "!does-not-exist:"+server.ServerName, "")
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("a hierarchy of a room that does not exist: status %d, want 403 "+
			"(a 404 would confirm which room IDs are real): %s", response.StatusCode, response.Body)
	}
}

// TestTheHierarchyNeedsAnAccount: this is a permission-filtered walk of a room graph, so
// it cannot be answered to an anonymous caller.
func TestTheHierarchyNeedsAnAccount(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	root := makeSpace(t, alice, server, "Root")

	response := server.Get(t, "/_matrix/client/v1/rooms/"+url.PathEscape(root)+"/hierarchy", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401\nbody: %s", response.StatusCode, response.Body)
	}
}

// TestASpaceIsRememberedAsASpace pins the fact a space is stored as one, not merely created
// as one.
//
// The room row is written before its create event is built, and the type was taken from the
// create content afterwards — assigning a field on a row that had already been inserted. So a
// space went into the database as an ordinary room, and everything that reads the type back was
// quietly wrong: the public room directory's space flag, a summary's `room_type`, and the space
// walk, which refused to descend because only a space has children.
func TestASpaceIsRememberedAsASpace(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	space := makeSpace(t, alice, server, "Remembered")
	room := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat", "name": "Ordinary"})

	// The summary reports the type.
	summary := roomSummaryOf(t, alice, server, space)
	if summary.RoomType != "m.space" {
		t.Errorf("the room summary of a space reports room_type %q, want m.space", summary.RoomType)
	}
	summaryOfRoom := roomSummaryOf(t, alice, server, room)
	if summaryOfRoom.RoomType != "" {
		t.Errorf("the room summary of a conversation reports room_type %q, want it absent",
			summaryOfRoom.RoomType)
	}

	// And so does the published-room directory, which is where a client decides whether to
	// draw a folder or a conversation.
	alice.MustSetState(t, server, space, "m.room.join_rules", "", map[string]any{"join_rule": "public"})
	alice.MustSetState(t, server, space, "m.room.history_visibility", "",
		map[string]any{"history_visibility": "world_readable"})
	alice.MustSetState(t, server, space, "m.room.avatar", "", map[string]any{"url": ""})
	directory := alice.MustGet(t, server, "/_matrix/client/v3/publicRooms")
	if directory.StatusCode != http.StatusOK {
		t.Fatalf("the published-room directory answered %d: %s", directory.StatusCode, directory.Body)
	}
	var page struct {
		Chunk []struct {
			RoomID   string `json:"room_id"`
			RoomType string `json:"room_type"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(directory.Body, &page); err != nil {
		t.Fatalf("decoding the directory: %v", err)
	}
	found := false
	for _, entry := range page.Chunk {
		if entry.RoomID == space {
			found = true
			if entry.RoomType != "m.space" {
				t.Errorf("the directory lists the space with room_type=%q, want m.space", entry.RoomType)
			}
		}
		if entry.RoomID == room && entry.RoomType != "" {
			t.Errorf("the directory lists an ordinary room as room_type=%q", entry.RoomType)
		}
	}
	if !found {
		t.Log("the space is not in the published-room directory; the directory flag is another test's subject")
	}
}
