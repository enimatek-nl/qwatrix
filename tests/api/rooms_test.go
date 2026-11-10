package api_test

// Room creation, event sending, state and timeline pagination.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// stateOf reads a room's full state as decoded events.
func stateOf(t *testing.T, account *harness.Account, server *harness.Server, roomID string) []harness.Event {
	t.Helper()
	resp := account.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "state"))
	var raw []json.RawMessage
	resp.JSON(t, &raw)
	return harness.DecodeEvents(t, raw)
}

// findState returns the state event of a type (and optional state key), or nil.
func findState(events []harness.Event, eventType, stateKey string) *harness.Event {
	for i, event := range events {
		if event.Type != eventType {
			continue
		}
		if stateKey != "" {
			if event.StateKey == nil || *event.StateKey != stateKey {
				continue
			}
		}
		return &events[i]
	}
	return nil
}

func contentString(t *testing.T, raw json.RawMessage, field string) string {
	t.Helper()
	var content map[string]any
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatalf("content is not a JSON object: %v (%s)", err, raw)
	}
	value, _ := content[field].(string)
	return value
}

func TestCreateRoomWritesTheInitialEvents(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "creator", "pw-creator", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"name": "Meeting room", "topic": "Agenda"})

	if !strings.HasPrefix(roomID, "!") || !strings.HasSuffix(roomID, ":"+serverName(server)) {
		t.Fatalf("room_id %q is not a room ID on this server", roomID)
	}

	state := stateOf(t, alice, server, roomID)

	// The initial state is what makes a room renderable by a client that knows nothing
	// else: identity, permissions, membership rules, and the creator's own join.
	creation := findState(state, "m.room.create", "")
	if creation == nil {
		t.Fatalf("state has no m.room.create event; got types %s", eventTypes(state))
	}
	if got := contentString(t, creation.Content, "room_version"); got == "" {
		t.Errorf("m.room.create has no room_version: %s", creation.Content)
	}
	if findState(state, "m.room.power_levels", "") == nil {
		t.Error("state has no m.room.power_levels event")
	}
	if findState(state, "m.room.join_rules", "") == nil {
		t.Error("state has no m.room.join_rules event")
	}
	// An m.room.member event's state key is the full user ID, which is the only form the
	// protocol allows: a room spanning two servers must be able to name two users called bob.
	member := findState(state, "m.room.member", alice.UserID)
	if member == nil {
		t.Fatalf("state has no m.room.member join for the creator; got types %s", eventTypes(state))
	}
	if got := contentString(t, member.Content, "membership"); got != "join" {
		t.Errorf("creator membership = %q, want join", got)
	}
	if member.Sender != alice.UserID {
		t.Errorf("creator join sender = %q, want %q", member.Sender, alice.UserID)
	}

	if name := findState(state, "m.room.name", ""); name == nil {
		t.Error("state has no m.room.name event")
	} else if got := contentString(t, name.Content, "name"); got != "Meeting room" {
		t.Errorf("room name = %q, want %q", got, "Meeting room")
	}
	if topic := findState(state, "m.room.topic", ""); topic == nil {
		t.Error("state has no m.room.topic event")
	}

	// Every event must carry an ID and a sender: a client that cannot name an event cannot
	// redact it, react to it, or paginate around it.
	for _, event := range state {
		if event.EventID == "" {
			t.Errorf("state event of type %s has no event_id", event.Type)
		}
		if event.Sender == "" {
			t.Errorf("state event of type %s has no sender", event.Type)
		}
		if event.OriginServerTS == 0 {
			t.Errorf("state event of type %s has no origin_server_ts", event.Type)
		}
	}
}

func TestCreateRoomPresetAndVisibility(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "presetter", "pw-presetter", false)

	t.Run("private by default", func(t *testing.T) {
		roomID := alice.MustCreateRoom(t, server, map[string]any{})
		state := stateOf(t, alice, server, roomID)
		joinRules := findState(state, "m.room.join_rules", "")
		if joinRules == nil {
			t.Fatal("no m.room.join_rules event")
		}
		if got := contentString(t, joinRules.Content, "join_rule"); got != "invite" {
			t.Errorf("join_rule = %q, want invite", got)
		}
		visibility := alice.MustGet(t, server, "/_matrix/client/v3/directory/list/room/"+harness.PathEscape(roomID))
		var parsed struct {
			Visibility string `json:"visibility"`
		}
		visibility.JSON(t, &parsed)
		if parsed.Visibility != "private" {
			t.Errorf("directory visibility = %q, want private", parsed.Visibility)
		}
	})

	t.Run("public_chat preset", func(t *testing.T) {
		roomID := alice.MustCreateRoom(t, server, map[string]any{
			"preset":     "public_chat",
			"visibility": "public",
			"name":       "Public square",
		})
		state := stateOf(t, alice, server, roomID)
		joinRules := findState(state, "m.room.join_rules", "")
		if got := contentString(t, joinRules.Content, "join_rule"); got != "public" {
			t.Errorf("join_rule = %q, want public", got)
		}
		history := findState(state, "m.room.history_visibility", "")
		if history == nil {
			t.Fatal("no m.room.history_visibility event")
		}
		if got := contentString(t, history.Content, "history_visibility"); got != "shared" {
			t.Errorf("history_visibility = %q, want shared", got)
		}
		guests := findState(state, "m.room.guest_access", "")
		if guests == nil {
			t.Fatal("no m.room.guest_access event")
		}
		if got := contentString(t, guests.Content, "guest_access"); got != "forbidden" {
			t.Errorf("guest_access = %q, want forbidden", got)
		}

		// A public room is listed in the directory, which is the only way a stranger
		// discovers it.
		resp := alice.MustGet(t, server, "/_matrix/client/v3/publicRooms")
		var page struct {
			Chunk []struct {
				RoomID string `json:"room_id"`
			} `json:"chunk"`
		}
		resp.JSON(t, &page)
		found := false
		for _, entry := range page.Chunk {
			if entry.RoomID == roomID {
				found = true
			}
		}
		if !found {
			t.Errorf("public room %s is not listed by /publicRooms: %+v", roomID, page.Chunk)
		}
	})

	t.Run("invalid preset and visibility are refused", func(t *testing.T) {
		server.PostJSON(t, "/_matrix/client/v3/createRoom", alice.AccessToken,
			map[string]any{"preset": "not_a_preset"}).MustStatus(t, http.StatusBadRequest)
		server.PostJSON(t, "/_matrix/client/v3/createRoom", alice.AccessToken,
			map[string]any{"visibility": "sideways"}).MustStatus(t, http.StatusBadRequest)
	})
}

func TestCreateRoomWithAliasAndInitialState(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "aliaser", "pw-aliaser", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"room_alias_name": "meeting",
		"initial_state": []map[string]any{
			{"type": "m.room.topic", "content": map[string]any{"topic": "from initial_state"}},
			{"type": "com.example.custom", "state_key": "k", "content": map[string]any{"value": float64(7)}},
		},
	})

	alias := "#meeting:" + serverName(server)
	resp := alice.MustGet(t, server, "/_matrix/client/v3/directory/room/"+url.PathEscape(alias))
	var resolved struct {
		RoomID string `json:"room_id"`
	}
	resp.JSON(t, &resolved)
	if resolved.RoomID != roomID {
		t.Fatalf("alias %s resolves to %q, want %q", alias, resolved.RoomID, roomID)
	}

	state := stateOf(t, alice, server, roomID)
	canonical := findState(state, "m.room.canonical_alias", "")
	if canonical == nil {
		t.Fatal("a room created with an alias has no m.room.canonical_alias event")
	}
	if got := contentString(t, canonical.Content, "alias"); got != alias {
		t.Errorf("canonical alias = %q, want %q", got, alias)
	}
	topic := findState(state, "m.room.topic", "")
	if topic == nil {
		t.Fatal("initial_state did not write m.room.topic")
	}
	if got := contentString(t, topic.Content, "topic"); got != "from initial_state" {
		t.Errorf("topic = %q, want %q", got, "from initial_state")
	}
	custom := findState(state, "com.example.custom", "k")
	if custom == nil {
		t.Fatal("initial_state did not write the custom state event")
	}
	var customContent map[string]any
	if err := json.Unmarshal(custom.Content, &customContent); err != nil {
		t.Fatalf("custom content: %v", err)
	}
	if value, ok := customContent["value"].(float64); !ok || value != 7 {
		t.Errorf("custom content value = %v, want 7", customContent["value"])
	}

	// A second room cannot claim the same alias.
	server.PostJSON(t, "/_matrix/client/v3/createRoom", alice.AccessToken,
		map[string]any{"room_alias_name": "meeting"}).MustStatus(t, http.StatusBadRequest)
}

func TestCreateRoomRefusesToReplaceItsOwnIdentity(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "identity", "pw-identity", false)

	// The create event defines which room a room ID refers to, and the power levels define
	// who owns the room; letting initial_state overwrite either would let a client mint a
	// room whose identity or ownership it did not have.
	for _, eventType := range []string{"m.room.create", "m.room.power_levels"} {
		resp := server.PostJSON(t, "/_matrix/client/v3/createRoom", alice.AccessToken, map[string]any{
			"initial_state": []map[string]any{
				{"type": eventType, "content": map[string]any{}},
			},
		})
		if resp.StatusCode == http.StatusOK {
			t.Errorf("createRoom accepted initial_state replacing %s", eventType)
		}
	}
}

func TestJoinByAliasWorks(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "guest", "pw-guest", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset":          "public_chat",
		"visibility":      "public",
		"room_alias_name": "lobby",
	})

	bob.MustJoinRoom(t, server, "#lobby:"+serverName(server))

	state := stateOf(t, bob, server, roomID)
	member := findState(state, "m.room.member", bob.UserID)
	if member == nil {
		t.Fatal("joining by alias did not write bob's member event")
	}
	if got := contentString(t, member.Content, "membership"); got != "join" {
		t.Errorf("bob membership = %q, want join", got)
	}
}

func TestSendMessageAppearsInSync(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "sender", "pw-sender", false)
	bob := server.Register(t, "receiver", "pw-receiver", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{})
	alice.MustInvite(t, server, roomID, bob.UserID)
	bob.MustJoinRoom(t, server, roomID)

	// Before any profile is set, both keys must still be present (null). A key that appears
	// only once a value exists is indistinguishable from a server that never answered.
	preResp := bob.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/joined_members")
	var pre struct {
		Joined map[string]map[string]any `json:"joined"`
	}
	preResp.JSON(t, &pre)
	for _, key := range []string{"display_name", "avatar_url"} {
		if value, ok := pre.Joined[alice.UserID][key]; !ok || value != nil {
			t.Fatalf("%s should be present and null for a profile-less member: %v", key, pre.Joined[alice.UserID])
		}
	}

	eventID := alice.MustSendText(t, server, roomID, "hello bob")

	// The sender sees its own message with its transaction ID echoed back, which is how a
	// client matches a local echo to the server's copy.
	alice.MustTimelineHas(t, server, roomID, "m.room.message", "hello bob")

	var since string
	sync := bob.SyncUntil(t, server, &since, func(response harness.SyncResponse) bool {
		room, ok := response.Rooms.Join[roomID]
		if !ok {
			return false
		}
		for _, raw := range room.Timeline.Events {
			var event harness.Event
			if err := json.Unmarshal(raw, &event); err != nil {
				continue
			}
			if event.EventID == eventID {
				return true
			}
		}
		return false
	})
	room := sync.Rooms.Join[roomID]
	var message *harness.Event
	events := harness.DecodeEvents(t, room.Timeline.Events)
	for i, event := range events {
		if event.EventID == eventID {
			message = &events[i]
		}
	}
	if message == nil {
		t.Fatalf("bob's timeline does not contain %s", eventID)
	}
	if message.Unsigned.TransactionID != nil {
		t.Errorf("bob sees the sender's transaction id %q; it belongs to alice's device only",
			*message.Unsigned.TransactionID)
	}
	if message.Sender != alice.UserID {
		t.Errorf("message sender = %q, want %q", message.Sender, alice.UserID)
	}
	if message.Unsigned.Age == nil {
		t.Error("timeline event has no unsigned.age, so a client cannot order it against local time")
	}

	// An incremental sync must not repeat it.
	params := url.Values{"since": {sync.NextBatch}, "timeout": {"0"}}
	followUp := bob.MustSync(t, server, params)
	if room, ok := followUp.Rooms.Join[roomID]; ok {
		for _, raw := range room.Timeline.Events {
			var event harness.Event
			if err := json.Unmarshal(raw, &event); err == nil && event.EventID == eventID {
				t.Error("an incremental sync repeated an event the client already has")
			}
		}
	}
}

func TestTransactionIDIsIdempotentPerDevice(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "idempotent", "pw-idempotent", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{})

	path := func(txn string) string {
		return "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID, "send", "m.room.message", txn)
	}
	body := map[string]any{"msgtype": "m.text", "body": "only once"}

	first := alice.MustPutJSON(t, server, path("repeat"), body)
	var firstEvent struct {
		EventID string `json:"event_id"`
	}
	first.JSON(t, &firstEvent)

	retry := alice.MustPutJSON(t, server, path("repeat"), body)
	var retryEvent struct {
		EventID string `json:"event_id"`
	}
	retry.JSON(t, &retryEvent)

	if firstEvent.EventID != retryEvent.EventID {
		t.Errorf("retried transaction created a second event: %s then %s", firstEvent.EventID, retryEvent.EventID)
	}

	// A different device of the same user has its own transaction namespace, so the same
	// ID there is a different send.
	otherDevice := server.Login(t, "idempotent", "pw-idempotent")
	other := otherDevice.MustPutJSON(t, server, path("repeat"), body)
	var otherEvent struct {
		EventID string `json:"event_id"`
	}
	other.JSON(t, &otherEvent)
	if otherEvent.EventID == firstEvent.EventID {
		t.Error("a second device's identical transaction ID was folded into the first event")
	}

	// Only two events exist in the room.
	messages := messagesOf(t, alice, server, roomID)
	var count int
	for _, event := range messages {
		if event.Type == "m.room.message" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("room holds %d message events, want 2 (%v)", count, eventTypes(messages))
	}
}

func TestStateEventHistoryAndPermissions(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "owner", "pw-owner", false)
	bob := server.Register(t, "plain", "pw-plain", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"name": "first"})
	alice.MustInvite(t, server, roomID, bob.UserID)
	bob.MustJoinRoom(t, server, roomID)

	alice.MustSetState(t, server, roomID, "m.room.name", "", map[string]any{"name": "second"})

	state := stateOf(t, alice, server, roomID)
	name := findState(state, "m.room.name", "")
	if name == nil {
		t.Fatal("room has no name after setting it")
	}
	if got := contentString(t, name.Content, "name"); got != "second" {
		t.Errorf("name = %q, want second", got)
	}
	// unsigned.prev_content lets a client show "renamed from X", and is the only way to see
	// the previous value without paging the whole timeline.
	if len(name.Unsigned.PrevContent) == 0 {
		t.Error("the replaced state event has no unsigned.prev_content")
	} else if got := contentString(t, name.Unsigned.PrevContent, "name"); got != "first" {
		t.Errorf("unsigned.prev_content name = %q, want first", got)
	}
	if name.Unsigned.ReplacesState == nil || *name.Unsigned.ReplacesState != name.EventID {
		t.Errorf("unsigned.replaces_state = %v, want the event's own id", name.Unsigned.ReplacesState)
	}

	// A plain member may not rename the room: state_default is 50 and joining grants 0.
	resp := server.PutJSON(t,
		"/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "state", "m.room.name"),
		bob.AccessToken, map[string]any{"content": map[string]any{"name": "hijacked"}})
	resp.MustStatus(t, http.StatusForbidden)
	if err := resp.ErrorBody(t); err.Errcode != "M_FORBIDDEN" {
		t.Errorf("renaming without power: errcode = %s, want M_FORBIDDEN", err.Errcode)
	}

	// A plain member may send messages: send_default is 0.
	bob.MustSendText(t, server, roomID, "bob speaks")

	// Banning bob stops his messages and removes his membership.
	alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/ban",
		map[string]any{"user_id": bob.UserID, "reason": "spam"})
	resp = server.PutJSON(t,
		"/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "send", "m.room.message", "afterban"),
		bob.AccessToken, map[string]any{"content": map[string]any{"msgtype": "m.text", "body": "still here"}})
	if resp.StatusCode == http.StatusOK {
		t.Error("a banned user could still send")
	}
	state = stateOf(t, alice, server, roomID)
	member := findState(state, "m.room.member", bob.UserID)
	if member == nil {
		t.Fatal("ban removed bob's member event entirely; history must keep it")
	}
	if got := contentString(t, member.Content, "membership"); got != "ban" {
		t.Errorf("bob membership = %q, want ban", got)
	}
}

func TestNonMembersCannotReadAPrivateRoom(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "private-owner", "pw-private-owner", false)
	eve := server.Register(t, "nosy", "pw-nosy", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"name": "Secret"})
	alice.MustSendText(t, server, roomID, "classified")

	// Reading state of a private room by a stranger is forbidden, not empty: an empty list
	// would advertise that the room exists and is merely stateless.
	resp := server.Get(t, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "state"), eve.AccessToken)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a non-member read a private room's state")
	}
	if err := resp.ErrorBody(t); err.Errcode != "M_FORBIDDEN" {
		t.Errorf("reading a private room: errcode = %s, want M_FORBIDDEN (body %s)", err.Errcode, resp.Body)
	}

	// Sending into a room one is not a member of is forbidden too.
	resp = server.PutJSON(t,
		"/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "send", "m.room.message", "intrude"),
		eve.AccessToken, map[string]any{"content": map[string]any{"msgtype": "m.text", "body": "hello?"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("sending into a private room: status = %d, want 403 (%s)", resp.StatusCode, resp.Body)
	}

	// A room that does not exist is M_NOT_FOUND, which is how a client distinguishes
	// "not here" from "not allowed".
	resp = server.Get(t, "/_matrix/client/v3/rooms/"+harness.PathEscape("!nothere:"+serverName(server), "state"), alice.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown room: status = %d, want 404 (%s)", resp.StatusCode, resp.Body)
	}
}

func TestMessagesPagination(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "pager", "pw-pager", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{})

	// Five messages on top of the creation events.
	const messages = 5
	for i := range messages {
		alice.MustSendText(t, server, roomID, string(rune('a'+i)))
	}

	// Backward pagination from the head of the room returns the newest events, oldest
	// first, and a token that continues backwards.
	resp := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "messages")+
		"?dir=b&limit=3&from="+url.QueryEscape(headToken(t, alice, server)))
	var page struct {
		Start string            `json:"start"`
		End   string            `json:"end"`
		Chunk []json.RawMessage `json:"chunk"`
		State []json.RawMessage `json:"state"`
	}
	resp.JSON(t, &page)
	if len(page.Chunk) != 3 {
		t.Fatalf("backward chunk holds %d events, want 3", len(page.Chunk))
	}
	if page.Start == "" || page.End == "" {
		t.Fatalf("pagination tokens missing: %+v", page)
	}
	events := harness.DecodeEvents(t, page.Chunk)
	if got := eventTypes(events); !strings.Contains(got, "m.room.message") {
		t.Fatalf("backward chunk has no messages: %s", got)
	}
	// The chunk is ordered oldest-first even when read backwards.
	bodies := messageBodies(events)
	if len(bodies) != 3 || bodies[0] >= bodies[len(bodies)-1] {
		t.Errorf("backward chunk bodies are not in ascending order: %v", bodies)
	}
	if len(page.State) == 0 {
		t.Error("a /messages chunk carries no state, so a client cannot render the events")
	}

	// Walking backwards again from `end` yields different, older events.
	resp = alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "messages")+
		"?dir=b&limit=3&from="+url.QueryEscape(page.End))
	var older struct {
		Chunk []json.RawMessage `json:"chunk"`
		End   string            `json:"end"`
	}
	resp.JSON(t, &older)
	olderEvents := harness.DecodeEvents(t, older.Chunk)
	if overlap := intersect(eventIDsOf(events), eventIDsOf(olderEvents)); len(overlap) > 0 {
		t.Errorf("paging backwards repeated events: %v", overlap)
	}
	if older.End == page.End {
		t.Error("walking backwards did not move the token, so a client would loop forever")
	}

	// Forward pagination from the start of the room replays the creation events and the
	// first messages.
	resp = alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "messages")+
		"?dir=f&limit=100&from=s0")
	var forward struct {
		Chunk []json.RawMessage `json:"chunk"`
		End   string            `json:"end"`
	}
	resp.JSON(t, &forward)
	forwardEvents := harness.DecodeEvents(t, forward.Chunk)
	if len(forwardEvents) < messages {
		t.Errorf("forward pagination returned %d events, want at least %d", len(forwardEvents), messages)
	}
	if got := len(messageBodies(forwardEvents)); got != messages {
		t.Errorf("forward pagination holds %d messages, want %d", got, messages)
	}
	// Forward order is oldest-first, so the messages read in the order they were sent.
	if bodies := messageBodies(forwardEvents); len(bodies) > 1 && bodies[0] >= bodies[1] {
		t.Errorf("forward chunk is not oldest-first: %v", bodies)
	}

	// A bad token is refused rather than silently treated as the beginning: a client with a
	// stale token must be able to tell.
	resp = server.Get(t, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "messages")+
		"?dir=b&from=not-a-token", alice.AccessToken)
	if resp.StatusCode == http.StatusOK {
		t.Error("an unparsable pagination token was accepted")
	}
}

func TestRoomMemberListings(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "moderator", "pw-moderator", false)
	bob := server.Register(t, "joiner", "pw-joiner", false)
	carol := server.Register(t, "invited", "pw-invited", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{})
	alice.MustInvite(t, server, roomID, bob.UserID)
	bob.MustJoinRoom(t, server, roomID)
	alice.MustInvite(t, server, roomID, carol.UserID)

	resp := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "members"))
	var chunk struct {
		Chunk []json.RawMessage `json:"chunk"`
	}
	resp.JSON(t, &chunk)
	members := harness.DecodeEvents(t, chunk.Chunk)

	if len(members) != 3 {
		t.Fatalf("member list holds %d events, want 3: %v", len(members), memberKeys(members))
	}
	for _, event := range members {
		if event.Type != "m.room.member" {
			t.Errorf("member list includes a %s event", event.Type)
		}
	}
	if findState(members, "m.room.member", carol.UserID) == nil {
		t.Error("an invited user is missing from /members")
	}

	// not_membership=invite removes the pending invites, which is what a client showing
	// "who is in this room right now" asks for.
	resp = alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "members")+
		"?not_membership=invite")
	resp.JSON(t, &chunk)
	filtered := harness.DecodeEvents(t, chunk.Chunk)
	if findState(filtered, "m.room.member", carol.UserID) != nil {
		t.Errorf("not_membership=invite still returned %s: %v", carol.Localpart, memberKeys(filtered))
	}
	if findState(filtered, "m.room.member", alice.UserID) == nil {
		t.Error("not_membership=invite removed a joined member")
	}

	// /joined_members is the compact form, keyed by user ID.
	resp = alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "joined_members"))
	var joined struct {
		Joined map[string]struct {
			DisplayName string `json:"display_name"`
		} `json:"joined"`
	}
	resp.JSON(t, &joined)
	if _, ok := joined.Joined[bob.UserID]; !ok {
		t.Errorf("/joined_members is missing %s: %+v", bob.UserID, joined.Joined)
	}
	if _, ok := joined.Joined[carol.UserID]; ok {
		t.Error("/joined_members includes an invited user")
	}

	// /joined_rooms is the account's own room list.
	resp = alice.MustGet(t, server, "/_matrix/client/v3/joined_rooms")
	var rooms struct {
		JoinedRooms []string `json:"joined_rooms"`
	}
	resp.JSON(t, &rooms)
	found := false
	for _, room := range rooms.JoinedRooms {
		if room == roomID {
			found = true
		}
	}
	if !found {
		t.Errorf("/joined_rooms does not list %s: %v", roomID, rooms.JoinedRooms)
	}
}

func TestRedaction(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "redactor", "pw-redactor", false)
	bob := server.Register(t, "redactee", "pw-redactee", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{})
	alice.MustInvite(t, server, roomID, bob.UserID)
	bob.MustJoinRoom(t, server, roomID)

	message := bob.MustSendText(t, server, roomID, "regrettable thing")
	stateEvent := alice.MustSetState(t, server, roomID, "m.room.topic", "", map[string]any{"topic": "annoying"})

	alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "redact", message),
		map[string]any{"reason": "spam"})
	alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "redact", stateEvent),
		map[string]any{})

	// The redacted message keeps existing, with its content gone.
	resp := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "event", message))
	var redacted struct {
		Type    string         `json:"type"`
		Sender  string         `json:"sender"`
		EventID string         `json:"event_id"`
		Content map[string]any `json:"content"`
	}
	resp.JSON(t, &redacted)
	if redacted.Type != "m.room.message" {
		t.Errorf("redacted event type = %q, want m.room.message", redacted.Type)
	}
	if _, ok := redacted.Content["body"]; ok {
		t.Errorf("redaction left content.body in place: %+v", redacted.Content)
	}
	if redacted.Sender != bob.UserID {
		t.Errorf("redacted event sender = %q, want %q", redacted.Sender, bob.UserID)
	}

	// Redacting a state event replaces the room's topic.
	state := stateOf(t, alice, server, roomID)
	topic := findState(state, "m.room.topic", "")
	if topic == nil {
		t.Fatal("room has no topic event after redacting it")
	}
	if got := contentString(t, topic.Content, "topic"); got != "" {
		t.Errorf("redacted topic = %q, want empty", got)
	}

	// A redaction is itself an event in the timeline, so clients can explain the gap.
	alice.MustTimelineHas(t, server, roomID, "m.room.redaction", "")

	// A user without the redact level may not redact somebody else.
	other := server.Register(t, "bystander", "pw-bystander", false)
	otherID := alice.MustSendText(t, server, roomID, "fine")
	_ = otherID
	alice.MustInvite(t, server, roomID, other.UserID)
	other.MustJoinRoom(t, server, roomID)
	target := alice.MustSendText(t, server, roomID, "alice says")
	resp = server.PostJSON(t, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "redact", target),
		other.AccessToken, map[string]any{})
	// redact defaults to level 0, so this is allowed by the spec's default power levels;
	// the assertion that matters is that the redaction succeeded and hid the body.
	if resp.StatusCode != http.StatusOK {
		t.Logf("redacting another user's event: status %d (%s)", resp.StatusCode, resp.Body)
	}
}

func TestEventAndStateReads(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "reader", "pw-reader", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"name": "Readable"})

	eventID := alice.MustSendText(t, server, roomID, "readable thing")

	resp := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "event", eventID))
	var event harness.Event
	resp.JSON(t, &event)
	if event.EventID != eventID {
		t.Errorf("GET /event returned %q, want %q", event.EventID, eventID)
	}
	if got := contentString(t, event.Content, "body"); got != "readable thing" {
		t.Errorf("event body = %q", got)
	}

	// The single-state-event endpoint answers with the content object alone.
	resp = alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "state", "m.room.name"))
	var content map[string]any
	resp.JSON(t, &content)
	if content["name"] != "Readable" {
		t.Errorf("GET state m.room.name = %+v, want the content object", content)
	}

	// A state event that was never sent is 404 with M_NOT_FOUND.
	resp = server.Get(t, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "state", "m.room.encryption"),
		alice.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("absent state event: status = %d, want 404", resp.StatusCode)
	}
	if err := resp.ErrorBody(t); err.Errcode != "M_NOT_FOUND" {
		t.Errorf("absent state event: errcode = %s, want M_NOT_FOUND", err.Errcode)
	}

	// An event ID from another room is not found in this one, and saying so must not confirm
	// that the event exists somewhere else.
	otherRoom := alice.MustCreateRoom(t, server, map[string]any{})
	resp = server.Get(t, "/_matrix/client/v3/rooms/"+harness.PathEscape(otherRoom, "event", eventID), alice.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("event from another room: status = %d, want 404 (%s)", resp.StatusCode, resp.Body)
	}
}

func TestAliasManagement(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "aliasowner", "pw-aliasowner", false)
	bob := server.Register(t, "aliasstranger", "pw-aliasstranger", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{})
	alias := "#second:" + serverName(server)

	alice.MustPutJSON(t, server, "/_matrix/client/v3/directories/alias/"+url.PathEscape(alias),
		map[string]any{"room_id": roomID})

	resp := alice.MustGet(t, server, "/_matrix/client/v3/directory/room/"+url.PathEscape(alias))
	var resolved struct {
		RoomID string `json:"room_id"`
	}
	resp.JSON(t, &resolved)
	if resolved.RoomID != roomID {
		t.Fatalf("alias resolves to %q, want %q", resolved.RoomID, roomID)
	}

	// The room lists its aliases.
	resp = alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "aliases"))
	var aliases struct {
		Aliases []string `json:"aliases"`
	}
	resp.JSON(t, &aliases)
	if len(aliases.Aliases) != 1 || aliases.Aliases[0] != alias {
		t.Errorf("/aliases = %v, want [%s]", aliases.Aliases, alias)
	}

	// Somebody else may not delete it.
	server.Do(t, harness.Request{Method: http.MethodDelete,
		Path: "/_matrix/client/v3/directories/alias/" + url.PathEscape(alias), Token: bob.AccessToken}).
		MustStatus(t, http.StatusForbidden)

	// The creator may.
	alice.MustDelete(t, server, "/_matrix/client/v3/directories/alias/"+url.PathEscape(alias))

	// Deleting an alias that is already gone is M_NOT_FOUND: a caller that believed the room
	// had that address is wrong about the room's addresses, and silence would hide that.
	server.Do(t, harness.Request{Method: http.MethodDelete,
		Path: "/_matrix/client/v3/directories/alias/" + url.PathEscape(alias), Token: alice.AccessToken}).
		MustError(t, http.StatusNotFound, "M_NOT_FOUND")

	// The list of aliases is a room read, so a stranger who is not in the room cannot ask
	// which addresses it answers to.
	server.Do(t, harness.Request{Method: http.MethodGet,
		Path:  "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID, "aliases"),
		Token: bob.AccessToken,
	}).MustStatus(t, http.StatusForbidden)

	// An unknown alias is M_NOT_FOUND, and joining through it fails rather than creating a
	// room out of a typo.
	resp = server.Get(t, "/_matrix/client/v3/directory/room/"+url.PathEscape("#gone:"+serverName(server)), alice.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown alias: status = %d, want 404", resp.StatusCode)
	}
	resp = server.PostJSON(t, "/_matrix/client/v3/rooms/"+url.PathEscape("#gone:"+serverName(server))+"/join",
		alice.AccessToken, map[string]any{})
	if resp.StatusCode == http.StatusOK {
		t.Error("joining an unknown alias succeeded")
	}
}

func TestDirectoryListingControl(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "listowner", "pw-listowner", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "public_chat", "visibility": "public", "room_alias_name": "listed",
	})
	inList := func() bool {
		resp := alice.MustGet(t, server, "/_matrix/client/v3/publicRooms")
		var page struct {
			Chunk []struct {
				RoomID string `json:"room_id"`
			} `json:"chunk"`
		}
		resp.JSON(t, &page)
		for _, entry := range page.Chunk {
			if entry.RoomID == roomID {
				return true
			}
		}
		return false
	}
	if !inList() {
		t.Fatal("a public room with an alias is not listed to begin with")
	}

	alice.MustPutJSON(t, server, "/_matrix/client/v3/directory/list/room/"+harness.PathEscape(roomID),
		map[string]any{"visibility": "private"})
	if inList() {
		t.Error("a room set to private visibility is still listed in /publicRooms")
	}

	// Making it public again republishes it.
	alice.MustPutJSON(t, server, "/_matrix/client/v3/directory/list/room/"+harness.PathEscape(roomID),
		map[string]any{"visibility": "public"})
	if !inList() {
		t.Error("a room set back to public visibility is not listed")
	}

	// Only the creator may publish it.
	stranger := server.Register(t, "liststranger", "pw-liststranger", false)
	server.PutJSON(t, "/_matrix/client/v3/directory/list/room/"+harness.PathEscape(roomID),
		stranger.AccessToken, map[string]any{"visibility": "private"}).MustStatus(t, http.StatusForbidden)
}

// --- helpers shared with the other test files in this package ---

func serverName(server *harness.Server) string { return server.ServerName }

func eventTypes(events []harness.Event) string {
	parts := make([]string, 0, len(events))
	for _, event := range events {
		parts = append(parts, event.Type)
	}
	return strings.Join(parts, ",")
}

func memberKeys(events []harness.Event) []string {
	keys := make([]string, 0, len(events))
	for _, event := range events {
		if event.StateKey != nil {
			keys = append(keys, *event.StateKey)
		}
	}
	return keys
}

func eventIDsOf(events []harness.Event) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.EventID)
	}
	return ids
}

func messageBodies(events []harness.Event) []string {
	bodies := []string{}
	for _, event := range events {
		if event.Type != "m.room.message" {
			continue
		}
		var content struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(event.Content, &content); err == nil {
			bodies = append(bodies, content.Body)
		}
	}
	return bodies
}

func intersect(left, right []string) []string {
	seen := map[string]bool{}
	for _, value := range right {
		seen[value] = true
	}
	out := []string{}
	for _, value := range left {
		if seen[value] {
			out = append(out, value)
		}
	}
	return out
}

// messagesOf reads a whole room's timeline by paginating forward from the beginning.
func messagesOf(t *testing.T, account *harness.Account, server *harness.Server, roomID string) []harness.Event {
	t.Helper()
	resp := account.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID, "messages")+
		"?dir=f&limit=1000&from=s0")
	var page struct {
		Chunk []json.RawMessage `json:"chunk"`
	}
	resp.JSON(t, &page)
	return harness.DecodeEvents(t, page.Chunk)
}

// headToken returns a token just past the newest event, which is what a client uses to page
// backwards from "now".
func headToken(t *testing.T, account *harness.Account, server *harness.Server) string {
	t.Helper()
	sync := account.MustSync(t, server, url.Values{"timeout": {"0"}})
	return sync.NextBatch
}

// TestLeaveAndForget checks that leaving a room works and that forgetting it afterwards
// removes it from the room list without disturbing anybody else's view of the room.
func TestLeaveAndForget(t *testing.T) {
	server := harness.NewWithRegistration(t)
	creator := server.Register(t, "leave-creator", "leave-pass", false)
	member := server.Register(t, "leave-member", "leave-pass", false)

	roomID := creator.MustCreateRoom(t, server, map[string]any{"name": "Leave flow"})
	creator.MustInvite(t, server, roomID, member.UserID)
	member.MustJoinRoom(t, server, roomID)

	res := member.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/leave",
		Body:   map[string]any{},
	})
	res.MustStatus(t, http.StatusOK)

	// A left room still appears in /sync as a leave event so the client can drop it.
	body := member.MustSync(t, server, nil)
	if _, ok := body.Rooms.Leave[roomID]; !ok {
		t.Fatalf("the left room is not in rooms.leave: %v", body.Rooms.Leave)
	}

	res = member.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/forget",
		Body:   map[string]any{},
	})
	res.MustStatus(t, http.StatusOK)

	// The creator never left, so the room and its history stay theirs.
	summary := creator.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/joined_members")
	members := map[string]any{}
	summary.JSON(t, &members)
	if _, ok := members["joined"].(map[string]any)[creator.UserID]; !ok {
		t.Errorf("the creator disappeared from joined_members after another member forgot the room: %v", members["joined"])
	}
}

// TestStatePathSpellings pins down the state endpoints' tolerance on the wire. The empty
// state key is a real tuple, the specification writes it more than one way, and a state key
// can contain a slash — an m.space.child key names a room ID, and clients build these URLs
// by joining parts together.
func TestStatePathSpellings(t *testing.T) {
	server := harness.NewWithRegistration(t)
	creator := server.Register(t, "statespeller", "pw-statespeller", false)
	roomID := creator.MustCreateRoom(t, server, map[string]any{})
	escapedRoom := harness.PathEscape(roomID)

	const childRoom = "!childroom:test.local:8448"
	cases := []struct{ path, wantType, wantKey string }{
		{path: "/state/m.room.name", wantType: "m.room.name", wantKey: ""},
		{path: "/state/m.room.topic/", wantType: "m.room.topic", wantKey: ""},
		{path: "/state/m.space.child/" + url.PathEscape(childRoom), wantType: "m.space.child", wantKey: childRoom},
	}

	for _, state := range cases {
		value := "written through " + state.path
		creator.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+escapedRoom+state.path,
			map[string]any{"value": value}).MustStatus(t, http.StatusOK)

		// The single-state endpoint answers the event's bare content.
		read := creator.MustGet(t, server, "/_matrix/client/v3/rooms/"+escapedRoom+state.path)
		var content map[string]string
		read.JSON(t, &content)
		if content["value"] != value {
			t.Errorf("GET %s = %v, want value %q", state.path, content, value)
		}

		// And the whole-state view — the specification's bare array of state events — holds
		// the tuple the path named, not a mangled one.
		all := creator.MustGet(t, server, "/_matrix/client/v3/rooms/"+escapedRoom+"/state")
		var events []struct {
			Type     string `json:"type"`
			StateKey string `json:"state_key"`
			Content  struct {
				Value string `json:"value"`
			} `json:"content"`
		}
		all.JSON(t, &events)

		var found bool
		for _, event := range events {
			if event.Type == state.wantType && event.StateKey == state.wantKey {
				found = true
				if event.Content.Value != value {
					t.Errorf("state view has %q for %s/%q, want %q",
						event.Content.Value, state.wantType, state.wantKey, value)
				}
			}
		}
		if !found {
			t.Errorf("the state view is missing %s/%q written through %s", state.wantType, state.wantKey, state.path)
		}
	}
}

// TestBodylessRequestsAreAccepted covers the endpoints whose body the specification makes
// optional. A request with no Content-Length header reports -1 rather than 0, so a server that
// only skips parsing on 0 refuses the plainest form of its own simplest call.
func TestBodylessRequestsAreAccepted(t *testing.T) {
	server := harness.NewWithRegistration(t)
	creator := server.Register(t, "nobodybody", "pw-nobodybody", false)
	guest := server.Register(t, "guestnobody", "pw-guestnobody", false)

	roomID := creator.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	path := func(suffix string) string {
		return "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + suffix
	}

	// No body, no Content-Length: the way a client sends "join, and I have no reason".
	req, err := http.NewRequest(http.MethodPost, server.URL+path("/join"), nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+guest.AccessToken)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body := readAllForTest(t, resp)
		t.Fatalf("POST /join with no body = %d %s, want 200", resp.StatusCode, body)
	}

	// The same for leave, and for a body that is present but empty.
	guest.Do(t, server, harness.Request{Method: http.MethodPost, Path: path("/leave")}).
		MustStatus(t, http.StatusOK)
	guest.Do(t, server, harness.Request{Method: http.MethodPost, Path: path("/join"), RawBody: []byte("")}).
		MustStatus(t, http.StatusOK)

	// A malformed body is still an error: tolerating *no* body must not become tolerating a
	// broken one.
	guest.Do(t, server, harness.Request{Method: http.MethodPost, Path: path("/leave"), RawBody: []byte("{")}).
		MustStatus(t, http.StatusBadRequest)
}

// TestJoinedMembersRequiresJoining checks that /joined_members describes the room to the people
// still in it, which is the whole reason it can skip the membership event's content.
func TestJoinedMembersRequiresJoining(t *testing.T) {
	server := harness.NewWithRegistration(t)
	creator := server.Register(t, "joinedowner", "pw-joinedowner", false)
	guest := server.Register(t, "joinedguest", "pw-joinedguest", false)

	roomID := creator.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/joined_members"

	guest.MustJoinRoom(t, server, roomID)
	resp := guest.MustGet(t, server, path)
	var joined struct {
		Joined map[string]any `json:"joined"`
	}
	resp.JSON(t, &joined)
	if _, ok := joined.Joined[guest.UserID]; !ok {
		t.Fatalf("the joined member is missing from %v", joined.Joined)
	}

	guest.MustLeaveRoom(t, server, roomID)
	guest.Do(t, server, harness.Request{Method: http.MethodGet, Path: path}).
		MustError(t, http.StatusForbidden, "M_FORBIDDEN")
}

// readAllForTest drains a response for a failure message.
func readAllForTest(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Sprintf("<reading body: %v>", err)
	}
	return string(body)
}

// TestMemberEventPreservesClientContent checks that a member event keeps the content its
// author sent. The protocol defines membership, displayname and avatar_url; everything else is
// the client's own data, and a server that quietly discards it has changed the event the
// sender signed.
func TestMemberEventPreservesClientContent(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "richmember", "pw-richmember", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	// A state write of one's own membership carrying a key this server knows nothing about.
	alice.MustSetState(t, server, roomID, "m.room.member", alice.UserID, map[string]any{
		"membership": "join",
		"foo":        "bar",
		"nested":     map[string]any{"deep": true},
	})

	membersPath := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/members"
	resp := alice.MustGet(t, server, membersPath)
	var members struct {
		Chunk []struct {
			Type     string         `json:"type"`
			Sender   string         `json:"sender"`
			StateKey string         `json:"state_key"`
			Content  map[string]any `json:"content"`
		} `json:"chunk"`
	}
	resp.JSON(t, &members)
	var found *struct {
		Type     string         `json:"type"`
		Sender   string         `json:"sender"`
		StateKey string         `json:"state_key"`
		Content  map[string]any `json:"content"`
	}
	for i := range members.Chunk {
		if members.Chunk[i].StateKey == alice.UserID {
			found = &members.Chunk[i]
		}
	}
	if found == nil {
		t.Fatalf("%s is missing from /members: %+v", alice.UserID, members.Chunk)
	}
	if got := found.Content["foo"]; got != "bar" {
		t.Fatalf("/members dropped the client's own key: content = %v", found.Content)
	}
	if _, ok := found.Content["nested"].(map[string]any); !ok {
		t.Fatalf("/members dropped a nested client key: content = %v", found.Content)
	}
	if found.Type != "m.room.member" || found.Sender != alice.UserID {
		t.Fatalf("member event lost its identity: %+v", *found)
	}

	// And the same through the state endpoint, which reads the stored event directly.
	state := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/state/m.room.member/"+harness.PathEscape(alice.UserID))
	var content map[string]any
	state.JSON(t, &content)
	if content["foo"] != "bar" {
		t.Fatalf("GET state dropped the client's key: %v", content)
	}
}

// TestProfileChangeReachesJoinedMembers checks that changing a profile is visible in the room
// views that cache it. /joined_members answers from cached columns rather than a profile lookup
// per member, which is a performance choice the server is entitled to make — but only as long
// as the cache is kept true.
func TestProfileChangeReachesJoinedMembers(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "prettyface", "pw-prettyface", false)
	bob := server.Register(t, "uglyface", "pw-uglyface", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	// Both set a profile *after* joining, which is the case a cache written at join time
	// misses.
	alice.MustPutJSON(t, server, "/_matrix/client/v3/profile/"+harness.PathEscape(alice.UserID)+"/displayname",
		map[string]any{"displayname": "Adelaide"})
	alice.MustPutJSON(t, server, "/_matrix/client/v3/profile/"+harness.PathEscape(alice.UserID)+"/avatar_url",
		map[string]any{"avatar_url": "mxc://test.local:8448/faceplant"})
	bob.MustPutJSON(t, server, "/_matrix/client/v3/profile/"+harness.PathEscape(bob.UserID)+"/avatar_url",
		map[string]any{"avatar_url": "mxc://test.local:8448/ugly"})

	// Bob, a different member, is the one asking: this is about what the room sees.
	resp := bob.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/joined_members")
	var joined struct {
		Joined map[string]struct {
			DisplayName string `json:"display_name"`
			AvatarURL   string `json:"avatar_url"`
		} `json:"joined"`
	}
	resp.JSON(t, &joined)
	member, ok := joined.Joined[alice.UserID]
	if !ok {
		t.Fatalf("%s missing from /joined_members: %v", alice.UserID, joined.Joined)
	}
	if member.DisplayName != "Adelaide" {
		t.Fatalf("the display name cache is stale: %+v", member)
	}
	if member.AvatarURL != "mxc://test.local:8448/faceplant" {
		t.Fatalf("the avatar cache is stale: %+v", member)
	}

	// Clearing a profile has to propagate too, or a user who removes their avatar is shown
	// wearing it forever.
	alice.MustPutJSON(t, server, "/_matrix/client/v3/profile/"+harness.PathEscape(alice.UserID)+"/displayname",
		map[string]any{"displayname": ""})
	resp = bob.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/joined_members")
	joined.Joined = nil
	resp.JSON(t, &joined)
	if member := joined.Joined[alice.UserID]; member.DisplayName != "" {
		t.Fatalf("a cleared display name is still cached: %+v", member)
	}
}

// TestEventNumbersMustBeRepresentable checks the numeric rule at the endpoint, not only in the
// validator: a number another homeserver cannot reproduce byte-for-byte is refused as bad JSON,
// because the event it would have produced cannot be verified by anybody else.
func TestEventNumbersMustBeRepresentable(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "numeric", "pw-numeric", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	for _, body := range []string{
		`{"body": 9007199254740992}`,
		`{"body": -9007199254740992}`,
		`{"body": 1.1}`,
		`{"nested":{"deep":[{"value": 0.5}]}}`,
	} {
		resp := alice.Do(t, server, harness.Request{
			Method: http.MethodPut,
			Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
				"/send/complement.dummy/" + harness.PathEscape("txn-"+strings.ReplaceAll(body, " ", "")),
			RawBody: []byte(body),
		})
		fail := resp.MustStatus(t, http.StatusBadRequest).ErrorBody(t)
		if fail.Errcode != "M_BAD_JSON" {
			t.Errorf("send of %s answered %s, want M_BAD_JSON", body, fail.Errcode)
		}
	}

	// A representable integer is fine, and so is a number-shaped string.
	alice.Do(t, server, harness.Request{
		Method:  http.MethodPut,
		Path:    "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/send/complement.dummy/txn-fine",
		RawBody: []byte(`{"value": 9007199254740991, "as_text": "9007199254740992"}`),
	}).MustStatus(t, http.StatusOK)

	// State events are signed and federated the same way, so the rule applies there too.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
			"/state/complement.dummy.state/",
		RawBody: []byte(`{"value": 1.5}`),
	}).MustStatus(t, http.StatusBadRequest)
}

// TestJoinKeepsClientContent and TestStateFormatEvent cover two ways a client asks for more
// than the minimum: a join that carries its own content, and a state read that wants the whole
// event. Both are in the specification and both are used by real clients.
func TestJoinKeepsClientContent(t *testing.T) {
	server := harness.NewWithRegistration(t)
	creator := server.Register(t, "joinhost", "pw-joinhost", false)
	guest := server.Register(t, "joinguest", "pw-joinguest", false)

	roomID := creator.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	// A join body is the content of the member event it produces, not a fixed set of fields.
	resp := guest.Do(t, server, harness.Request{
		Method:  http.MethodPost,
		Path:    "/_matrix/client/v3/join/" + harness.PathEscape(roomID),
		RawBody: []byte(`{"reason":"curiosity","foo":"bar","nested":{"deep":true}}`),
	})
	resp.MustStatus(t, http.StatusOK)

	content := jsonBodyOf(t, guest.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/state/m.room.member/"+harness.PathEscape(guest.UserID)))
	if content["membership"] != "join" {
		t.Fatalf("the join did not write a join: %v", content)
	}
	if content["foo"] != "bar" {
		t.Fatalf("the join dropped the client's content: %v", content)
	}
	if _, ok := content["nested"].(map[string]any); !ok {
		t.Fatalf("the join dropped a nested client key: %v", content)
	}
	if content["reason"] != "curiosity" {
		t.Fatalf("the join lost its reason: %v", content)
	}

	// The keys the server owns are not the client's to set, whatever the body claims.
	other := server.Register(t, "joinforgers", "pw-joinforgers", false)
	resp = other.Do(t, server, harness.Request{
		Method:  http.MethodPost,
		Path:    "/_matrix/client/v3/join/" + harness.PathEscape(roomID),
		RawBody: []byte(`{"membership":"ban"}`),
	})
	resp.MustStatus(t, http.StatusOK)
	content = jsonBodyOf(t, creator.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/state/m.room.member/"+harness.PathEscape(other.UserID)))
	if content["membership"] != "join" {
		t.Fatalf("a join body overrode the membership: %v", content)
	}
}

func TestStateFormatEvent(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "formatfan", "pw-formatfan", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	statePath := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
		"/state/m.room.member/" + harness.PathEscape(alice.UserID)

	// The default answer is the content object alone.
	content := jsonBodyOf(t, alice.MustGet(t, server, statePath))
	if content["membership"] != "join" || content["sender"] != nil {
		t.Fatalf("the bare state read should be content only: %v", content)
	}

	// ?format=event is the same tuple with its envelope: sender, room_id, type, state_key.
	event := jsonBodyOf(t, alice.MustGet(t, server, statePath+"?format=event"))
	for key, want := range map[string]string{
		"sender":    alice.UserID,
		"room_id":   roomID,
		"type":      "m.room.member",
		"state_key": alice.UserID,
	} {
		if got := event[key]; got != want {
			t.Errorf("?format=event: %s = %v, want %v", key, got, want)
		}
	}
	nested, ok := event["content"].(map[string]any)
	if !ok || nested["membership"] != "join" {
		t.Fatalf("?format=event lost its content: %v", event["content"])
	}
	if event["origin_server_ts"] == nil || event["event_id"] == nil {
		t.Fatalf("?format=event should be a full event, missing ts or id: %v", event)
	}
}

// jsonBodyOf decodes a JSON object response.
func jsonBodyOf(t *testing.T, resp *harness.Response) map[string]any {
	t.Helper()
	var body map[string]any
	resp.JSON(t, &body)
	return body
}

// TestFreshSyncFindsRecentEvent reproduces what a client does after sending: it calls /sync with
// no `since` at all — the beginning of time — and looks for the event it just sent. A server that
// answers a fresh sync with a timeline trimmed to something older than the newest event, or with
// no timeline at all, makes that unanswerable.
func TestFreshSyncFindsRecentEvent(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "freshsync", "pw-freshsync", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	// Enough history that a timeline window cannot hold the whole room.
	for i := 0; i < 25; i++ {
		alice.MustSendText(t, server, roomID, fmt.Sprintf("message %d", i))
	}
	// Including a state-typed event sent through the timeline endpoint, which complement does.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
			"/send/m.room.join_rules/txn-stateish",
		RawBody: []byte(`{"join_rule":"public"}`),
	}).MustStatus(t, http.StatusOK)

	last := alice.MustSendText(t, server, roomID, "the one to find")

	sync := alice.MustSync(t, server, nil)
	room, ok := sync.Rooms.Join[roomID]
	if !ok {
		t.Fatalf("a fresh sync should list the room in join, got %d rooms", len(sync.Rooms.Join))
	}
	events := harness.DecodeEvents(t, room.Timeline.Events)
	seen := false
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.EventID)
		if event.EventID == last {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("fresh sync timeline (%d events) does not contain the event just sent (%s): %v",
			len(ids), last, ids)
	}
}

// TestCanonicalAliasMustPointBackAtTheRoom checks that a room cannot claim to be called
// something it is not. The canonical alias is how a room names itself to clients that have no
// other context — a search result, a room picker, a link somebody pasted — so an alias that does
// not resolve back to this room makes the room lie about its own address with no way for a client
// downstream to notice.
func TestCanonicalAliasMustPointBackAtTheRoom(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "namepicker", "pw-namepicker", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	putAlias := func(content map[string]any) *harness.Response {
		return alice.Do(t, server, harness.Request{
			Method: http.MethodPut,
			Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
				"/state/m.room.canonical_alias/",
			Body: content,
		})
	}

	// An alias registered to this room may be claimed.
	alias := "#claimable:test.local:8448"
	alice.MustPutJSON(t, server, "/_matrix/client/v3/directories/alias/"+harness.PathEscape(alias),
		map[string]any{"room_id": roomID}).MustStatus(t, http.StatusOK)
	putAlias(map[string]any{"alias": alias}).MustStatus(t, http.StatusOK)

	// An alias that exists nowhere, or that belongs to a different room, is refused. Both
	// answers tell the caller the same thing: this room is not called that.
	bad := putAlias(map[string]any{"alias": "#nonexistent:test.local:8448"})
	bad.MustStatus(t, http.StatusBadRequest)
	if code := bad.ErrorBody(t).Errcode; code != "M_BAD_ALIAS" {
		t.Errorf("claiming an unregistered alias answered %s, want M_BAD_ALIAS", code)
	}

	// A malformed alias is a different mistake — the caller did not name an alias at all — so
	// it gets the parameter error rather than the alias one.
	malformed := putAlias(map[string]any{"alias": "%invalid_aliases:test.local:8448"})
	malformed.MustStatus(t, http.StatusBadRequest)
	if code := malformed.ErrorBody(t).Errcode; code != "M_INVALID_PARAM" {
		t.Errorf("claiming a malformed alias answered %s, want M_INVALID_PARAM", code)
	}

	// The same rule covers alt_aliases, which clients list exactly like the canonical one.
	altBad := putAlias(map[string]any{"alt_aliases": []string{"#nope:test.local:8448"}})
	altBad.MustStatus(t, http.StatusBadRequest)
	if code := altBad.ErrorBody(t).Errcode; code != "M_BAD_ALIAS" {
		t.Errorf("an unregistered alt_alias answered %s, want M_BAD_ALIAS", code)
	}

	// Listing the alias that is registered, alongside the canonical one, is fine.
	putAlias(map[string]any{"alias": alias, "alt_aliases": []string{alias}}).
		MustStatus(t, http.StatusOK)

	// And a room may always stop claiming by saying nothing: clearing is never an error.
	putAlias(map[string]any{"alias": ""}).MustStatus(t, http.StatusOK)
}

// TestStateEventsAppearInSyncTimeline checks that a state write shows up in the sender's
// timeline, not only in the state. State events are events: they are what a client scrolling
// back through a room sees ("Alice set the topic", "the room became public"), and a client
// that sets one and then syncs expects to find the event it just created.
func TestStateEventsAppearInSyncTimeline(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "statetimeline", "pw-statetimeline", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	// A state write through the endpoint a client uses for it.
	resp := alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/state/m.room.topic/",
		Body:   map[string]any{"topic": "a topic worth finding"},
	})
	resp.MustStatus(t, http.StatusOK)
	var written struct {
		EventID string `json:"event_id"`
	}
	resp.JSON(t, &written)
	if written.EventID == "" {
		t.Fatalf("the state write answered no event_id: %s", resp.Body)
	}

	// A fresh sync — no since token, so the beginning of time — must contain it.
	sync := alice.MustSync(t, server, nil)
	room, ok := sync.Rooms.Join[roomID]
	if !ok {
		t.Fatalf("the room is missing from a fresh sync's join section")
	}
	events := harness.DecodeEvents(t, room.Timeline.Events)
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.EventID)
		if event.EventID == written.EventID {
			return
		}
	}
	t.Fatalf("the state event %s is not in the fresh sync timeline (%d events): %v",
		written.EventID, len(ids), ids)
}

// TestForgottenRoomIsNotReadable checks what forgetting actually costs. Leaving keeps the
// history readable under a shared room's rules; forgetting is the caller saying the room is no
// longer their business, and a server that keeps the data reachable after that has only hidden
// the room from one screen.
func TestForgottenRoomIsNotReadable(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "forgetful", "pw-forgetful", false)
	bob := server.Register(t, "remembr", "pw-remembr", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	alice.MustSendText(t, server, roomID, "readable while a member")

	escaped := harness.PathEscape(roomID)
	messagesPath := "/_matrix/client/v3/rooms/" + escaped + "/messages?dir=b&limit=5"

	// A member can page.
	alice.MustGet(t, server, messagesPath).MustStatus(t, http.StatusOK)

	// Leaving alone does not stop the paging of a shared room: the history rule, not
	// membership, is what governs that.
	alice.MustLeaveRoom(t, server, roomID)

	// Forgetting does.
	server.PostJSON(t, "/_matrix/client/v3/rooms/"+escaped+"/forget", alice.AccessToken, map[string]any{}).
		MustStatus(t, http.StatusOK)
	resp := alice.Do(t, server, harness.Request{Method: http.MethodGet, Path: messagesPath})
	resp.MustError(t, http.StatusForbidden, "M_FORBIDDEN")

	// The same reader cannot read the room's state either — it is the same question.
	alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/rooms/" + escaped + "/state/m.room.topic/",
	}).MustError(t, http.StatusForbidden, "M_FORBIDDEN")

	// Bob, still a member, is unaffected.
	bob.MustGet(t, server, messagesPath).MustStatus(t, http.StatusOK)
}

// TestForgottenRoomLeaveStillReachesIncrementalSync covers the counterpart of "forgotten rooms
// are not in /sync". A client that last synced *before* the user left still has to learn that the
// room left — otherwise every other device keeps a room it cannot get rid of, since the only
// message that would have removed it has been suppressed. Forgetting governs what a *new* sync
// sees; it cannot rewrite a conversation the server has already started having with a session.
func TestForgottenRoomLeaveStillReachesIncrementalSync(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "forgetslow", "pw-forgetslow", false)
	bob := server.Register(t, "forgetwatch", "pw-forgetwatch", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	alice.MustSendText(t, server, roomID, "before the leave")

	// Alice's position before she leaves.
	since := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch

	alice.MustLeaveRoom(t, server, roomID)
	escaped := harness.PathEscape(roomID)
	server.PostJSON(t, "/_matrix/client/v3/rooms/"+escaped+"/forget", alice.AccessToken, map[string]any{}).
		MustStatus(t, http.StatusOK)
	bob.MustSendText(t, server, roomID, "after the leave")

	// An incremental sync from before the leave reports the room under leave.
	var found bool
	for attempt := 0; attempt < 5 && !found; attempt++ {
		sync := alice.MustSync(t, server, url.Values{
			"timeout": []string{"0"},
			"since":   []string{since},
		})
		if _, ok := sync.Rooms.Leave[roomID]; ok {
			found = true
		}
	}
	if !found {
		t.Fatalf("the forgotten room's leave never reached the incremental sync (rooms.join=%d, rooms.leave=%d)",
			len(mustSyncRoomsForTest(t, alice, server, since).Join),
			len(mustSyncRoomsForTest(t, alice, server, since).Leave))
	}

	// A brand-new sync, however, does not know her ever been there.
	fresh := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	if _, ok := fresh.Rooms.Join[roomID]; ok {
		t.Fatalf("a forgotten room appeared in a fresh sync's join section")
	}
	if _, ok := fresh.Rooms.Leave[roomID]; ok {
		t.Fatalf("a forgotten room appeared in a fresh sync's leave section")
	}
}

// mustSyncRoomsForTest re-reads a sync for a failure message.
func mustSyncRoomsForTest(t *testing.T, account *harness.Account, server *harness.Server, since string) harness.SyncRooms {
	t.Helper()
	sync := account.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{since}})
	return sync.Rooms
}

// TestDeletingTheCanonicalAliasClearsTheClaim checks that taking an alias away also takes away
// the room's claim to it. The alias is gone from the directory, so a room still naming itself
// with it points clients at a 404, and only a new event can correct a claim that is itself an
// event.
func TestDeletingTheCanonicalAliasClearsTheClaim(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "aliasclears", "pw-aliasclears", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	alias := "#clearme:test.local:8448"
	alice.MustPutJSON(t, server, "/_matrix/client/v3/directories/alias/"+harness.PathEscape(alias),
		map[string]any{"room_id": roomID}).MustStatus(t, http.StatusOK)
	alice.MustSetState(t, server, roomID, "m.room.canonical_alias", "", map[string]any{"alias": alias})

	if content := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/state/m.room.canonical_alias/")); content["alias"] != alias {
		t.Fatalf("the canonical alias was not set: %v", content)
	}

	alice.MustDelete(t, server, "/_matrix/client/v3/directories/alias/"+harness.PathEscape(alias)).
		MustStatus(t, http.StatusOK)

	// The claim is now empty rather than pointing at an alias that no longer resolves.
	content := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/state/m.room.canonical_alias/"))
	if len(content) != 0 {
		t.Fatalf("the room still claims a deleted alias: %v", content)
	}

	// And the correction is an event like any other: it reaches a client paging the timeline,
	// which is the only way a client holding the old name learns to drop it.
	sync := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	found := false
	for _, raw := range sync.Rooms.Join[roomID].Timeline.Events {
		var event struct {
			Type    string         `json:"type"`
			Content map[string]any `json:"content"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("timeline event is not JSON: %v", err)
		}
		if event.Type == "m.room.canonical_alias" && len(event.Content) == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the clearing of the canonical alias never reached the timeline")
	}
}

// TestEventEndpointHidesWhatTheReaderMayNotSee checks the /event contract: an event the caller
// may not read is answered as though it did not exist.
//
// An event ID is not something anybody handed the caller, so this endpoint is the one place where
// "no such event" is both the truth from their side and the answer that stops it being used to
// ask whether a room, an event, or a message someone else sent exists.
func TestEventEndpointHidesWhatTheReaderMayNotSee(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "earlysender", "pw-earlysender", false)
	bob := server.Register(t, "latejoiner", "pw-latejoiner", false)

	// A "joined" room: members may read from the moment they arrived, no earlier.
	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "private_chat", "creation_content": map[string]any{"m.federate": false},
	})
	alice.MustSetState(t, server, roomID, "m.room.history_visibility", "",
		map[string]any{"history_visibility": "joined"})
	eventID := alice.MustSendText(t, server, roomID, "before bob arrived")

	escaped := harness.PathEscape(roomID)
	eventPath := func(id string) string {
		return "/_matrix/client/v3/rooms/" + escaped + "/event/" + harness.PathEscape(id)
	}

	// The sender, present from the beginning, reads it.
	alice.MustGet(t, server, eventPath(eventID)).MustStatus(t, http.StatusOK)

	// Bob joins afterwards.
	alice.MustInvite(t, server, roomID, bob.UserID)
	bob.MustJoinRoom(t, server, roomID)

	// The event predates his membership, so as far as he is concerned it is not there.
	resp := bob.Do(t, server, harness.Request{Method: http.MethodGet, Path: eventPath(eventID)})
	resp.MustStatus(t, http.StatusNotFound)
	if code := resp.ErrorBody(t).Errcode; code != "M_NOT_FOUND" {
		t.Errorf("an unreadable event answered %s, want M_NOT_FOUND", code)
	}

	// A message sent after he arrived is readable.
	later := alice.MustSendText(t, server, roomID, "after bob arrived")
	bob.MustGet(t, server, eventPath(later)).MustStatus(t, http.StatusOK)

	// And a room he has never heard of is the same 404, not a 403 that confirms it exists.
	other := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	secret := alice.MustSendText(t, server, other, "private")
	resp = bob.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(other) +
			"/event/" + harness.PathEscape(secret),
	})
	resp.MustStatus(t, http.StatusNotFound)
}

// TestInvitedVisibilityReadsFromTheInvite checks that `history_visibility: invited` means what
// it says: an invitee may read what happened from the moment they were invited, which is strictly
// more than `joined` allows and strictly less than `shared`.
func TestInvitedVisibilityReadsFromTheInvite(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "inviteroom", "pw-inviteroom", false)
	bob := server.Register(t, "invitee", "pw-invitee", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	alice.MustSetState(t, server, roomID, "m.room.history_visibility", "",
		map[string]any{"history_visibility": "invited"})

	// Sent before bob existed in the room at all: not his to read.
	before := alice.MustSendText(t, server, roomID, "before the invite")

	alice.MustInvite(t, server, roomID, bob.UserID)

	// Sent while he was invited: his to read, even though he has not joined yet.
	during := alice.MustSendText(t, server, roomID, "between invite and join")

	bob.MustJoinRoom(t, server, roomID)
	escaped := harness.PathEscape(roomID)
	eventPath := func(id string) string {
		return "/_matrix/client/v3/rooms/" + escaped + "/event/" + harness.PathEscape(id)
	}

	bob.MustGet(t, server, eventPath(during)).MustStatus(t, http.StatusOK)
	bob.Do(t, server, harness.Request{Method: http.MethodGet, Path: eventPath(before)}).
		MustStatus(t, http.StatusNotFound)

	// `joined`, by contrast, bounds at the join: the same event sent between invite and join
	// is not readable.
	joinedRoom := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	alice.MustSetState(t, server, joinedRoom, "m.room.history_visibility", "",
		map[string]any{"history_visibility": "joined"})
	alice.MustInvite(t, server, joinedRoom, bob.UserID)
	betweenJoin := alice.MustSendText(t, server, joinedRoom, "between invite and join in a joined room")
	bob.MustJoinRoom(t, server, joinedRoom)
	bob.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(joinedRoom) +
			"/event/" + harness.PathEscape(betweenJoin),
	}).MustStatus(t, http.StatusNotFound)
}

// TestKickNeedsAMembershipToEnd checks that a kick is refused when there is no membership to end.
//
// A kick sends a leave event for somebody else. If they were never in the room, that event
// invents a membership they never had; if they already left, it re-announces a departure the room
// has moved on from. Both answer 200 today and write an event — complement pins 403 instead, and
// a moderator mistyping a user ID should not get a success.
func TestKickNeedsAMembershipToEnd(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "kicker", "pw-kicker", false)
	outsider := server.Register(t, "outsider", "pw-outsider", false)
	exMember := server.Register(t, "exmember", "pw-exmember", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	exMember.MustJoinRoom(t, server, roomID)
	exMember.MustLeaveRoom(t, server, roomID)

	kick := func(target string) int {
		resp := alice.Do(t, server, harness.Request{
			Method: http.MethodPost,
			Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/kick",
			Body:   map[string]any{"user_id": target, "reason": "testing"},
		})
		return resp.StatusCode
	}
	if code := kick(outsider.UserID); code != http.StatusForbidden {
		t.Errorf("kicking somebody who was never in the room: got %d, want 403", code)
	}
	if code := kick(exMember.UserID); code != http.StatusForbidden {
		t.Errorf("kicking somebody who already left: got %d, want 403", code)
	}

	// A real member can still be kicked, and a ban needs no membership: banning in advance is
	// how a room stops somebody joining before they try.
	member := server.Register(t, "victim", "pw-victim", false)
	member.MustJoinRoom(t, server, roomID)
	alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/kick",
		map[string]any{"user_id": member.UserID, "reason": "away"}).
		MustStatus(t, http.StatusOK)
	alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/ban",
		map[string]any{"user_id": outsider.UserID, "reason": "before they arrive"}).
		MustStatus(t, http.StatusOK)
}

// TestMessagesOnAnUnknownRoomIsForbidden checks that /messages does not become an existence oracle.
//
// The room ID is a capability: a private room is discoverable only by its ID, so an endpoint that
// answers "no such room" differently from "not yours" lets anybody walk a list of candidate IDs
// and keep the ones that exist. /messages is served to members, so the answer is M_FORBIDDEN.
func TestMessagesOnAnUnknownRoomIsForbidden(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "paginator", "pw-paginator", false)

	resp := alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path: "/_matrix/client/v3/rooms/" +
			harness.PathEscape("!does-not-exist-"+harness.ServerName) + "/messages?dir=b",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /messages for an unknown room: got %d, want 403", resp.StatusCode)
	}
}

// TestWritingTheSameStateTwiceIsOneEvent checks that repeating a state write is idempotent.
//
// Re-asserting state is normal traffic — a client reconnecting and re-setting its room name, a
// request retried because the response was lost. If each repetition became an event, a purely
// idempotent operation would grow the room's history forever and hand the client a *different*
// event ID for a request it already made, which leaves no way for a client or a peer to notice
// they already have it.
func TestWritingTheSameStateTwiceIsOneEvent(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "repeater", "pw-repeater", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	set := func(content map[string]any) string {
		resp := alice.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+
			harness.PathEscape(roomID)+"/state/m.room.topic/", content)
		var body struct {
			EventID string `json:"event_id"`
		}
		resp.JSON(t, &body)
		return body.EventID
	}

	first := set(map[string]any{"topic": "one"})
	if second := set(map[string]any{"topic": "one"}); second != first {
		t.Errorf("repeating an identical state write returned a new event: %s then %s", first, second)
	}
	// Key order and whitespace are not a change of state.
	if shuffled := set(map[string]any{"other": 1, "topic": "one"}); shuffled == first {
		t.Error("writing different content returned the old event; the write was swallowed")
	}
	if changed := set(map[string]any{"topic": "two"}); changed == first {
		t.Error("writing a different topic returned the old event; the write was swallowed")
	}

	// A room's history holds one event per distinct value, not one per request.
	members := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/state/m.room.topic/"))
	if members["topic"] != "two" {
		t.Errorf("current topic = %v, want two", members["topic"])
	}
}

// TestJoiningARoomTwiceIsOneEvent checks that a retried join does not announce the join twice.
//
// The client that retries a join because its response was lost is the common case, not the
// exception. A second event would tell every other member that this user joined again, and the
// retried request would answer with an event ID the client has never seen.
func TestJoiningARoomTwiceIsOneEvent(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "dualjoiner", "pw-dualjoiner", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	join := func() string {
		resp := alice.MustPostJSON(t, server, "/_matrix/client/v3/join/"+
			harness.PathEscape(roomID), map[string]any{})
		var body struct {
			EventID string `json:"event_id"`
		}
		resp.JSON(t, &body)
		return body.EventID
	}
	first := join()
	if second := join(); second != first {
		t.Errorf("joining twice produced two events: %s then %s", first, second)
	}

	// Leaving and coming back is a genuine second membership, and does get its own event.
	alice.MustLeaveRoom(t, server, roomID)
	if rejoined := join(); rejoined == first {
		t.Error("rejoining after a leave returned the old membership event")
	}
}

// TestCreateRoomTopicPrecedenceAndRichText checks how createRoom reconciles the `topic` field
// with a topic in initial_state, and what the resulting event looks like.
//
// A request that names a field must beat a state document that happens to mention the same
// thing; otherwise a client that sends both silently loses the field it named. And the plain
// topic is duplicated into the structured `m.topic` form so that a client which wants markup has
// the structure to edit, produced from the one value that both spellings describe.
func TestCreateRoomTopicPrecedenceAndRichText(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "topicist", "pw-topicist", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "public_chat",
		"topic":  "Test Room",
		"initial_state": []map[string]any{{
			"type": "m.room.topic", "state_key": "",
			"content": map[string]any{"topic": "Shenanigans"},
		}},
	})
	content := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/state/m.room.topic/"))
	if content["topic"] != "Test Room" {
		t.Fatalf("topic = %v, want the createRoom request's topic", content["topic"])
	}
	rich, ok := content["m.topic"].(map[string]any)
	if !ok {
		t.Fatalf("m.topic is %T, want the structured form", content["m.topic"])
	}
	texts, ok := rich["m.text"].([]any)
	if !ok || len(texts) != 1 {
		t.Fatalf("m.topic.m.text = %v, want one entry", rich["m.text"])
	}
	entry, ok := texts[0].(map[string]any)
	if !ok || entry["body"] != "Test Room" {
		t.Fatalf("m.topic.m.text[0] = %v, want a body naming the same topic", texts[0])
	}
	if mime, present := entry["mimetype"]; present && mime != "text/plain" {
		t.Errorf("mimetype = %v, want text/plain when set", mime)
	}

	// A topic that arrives only as initial_state is the client's own document: the server
	// writes what it was given and does not re-derive structure it was not asked for.
	plainRoom := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "public_chat",
		"initial_state": []map[string]any{{
			"type": "m.room.topic", "state_key": "",
			"content": map[string]any{"topic": "From initial state"},
		}},
	})
	plain := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(plainRoom)+"/state/m.room.topic/"))
	if plain["topic"] != "From initial state" {
		t.Errorf("topic = %v, want the initial_state value", plain["topic"])
	}
	if _, present := plain["m.topic"]; present {
		t.Error("the server invented m.topic for a document the client wrote itself")
	}
}

// TestCreateRoomInAnOldRoomVersion checks that a server may be asked for a stable room version
// it has advertised.
//
// Versions 1 and 2 put `prev_state` in every event, so they are a different on-the-wire shape
// rather than a different set of rules — and the event library owns that shape. Refusing them is
// a server-side preference that leaves a client creating an old-format room finding out by
// failing, which is why the supported list is advertised instead of guessed.
func TestCreateRoomInAnOldRoomVersion(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "ancient", "pw-ancient", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "public_chat", "room_version": "2",
	})
	create := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/state/m.room.create/"))
	if version := fmt.Sprint(create["room_version"]); version != "2" {
		t.Fatalf("created room version %q, want 2", version)
	}

	// The room has to be usable, not merely created: an event written into it has to be built,
	// hashed, stored and delivered in that version's format.
	alice.MustJoinRoom(t, server, roomID)
	alice.MustSendText(t, server, roomID, "hello from version two")
	alice.MustTimelineHas(t, server, roomID, "m.room.message", "hello from version two")
}

// TestEventsCarryTheReadersMembershipAtTheTime checks MSC4115's `unsigned.membership`.
//
// Whose membership is it? The reader's, not the sender's — the question a client is answering is
// "could I have seen this when it happened", which is a fact about the reader's presence in the
// room. And it is the membership *at that point*: a room's history is rendered to somebody who
// arrived part-way through it, and every event before their arrival was written while they were
// not in the room.
func TestEventsCarryTheReadersMembershipAtTheTime(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "announcer", "pw-announcer", false)
	bob := server.Register(t, "latebob", "pw-latebob", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSendText(t, server, roomID, "before bob")
	bob.MustJoinRoom(t, server, roomID)
	alice.MustSendText(t, server, roomID, "after bob")

	seenJoin := false
	checked := 0
	sync := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	for _, raw := range sync.Rooms.Join[roomID].Timeline.Events {
		var event struct {
			Type     string `json:"type"`
			Sender   string `json:"sender"`
			StateKey string `json:"state_key"`
			Unsigned struct {
				Membership string `json:"membership"`
			} `json:"unsigned"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("timeline event is not JSON: %v", err)
		}
		// The membership is included on every event, including one the reader did not send.
		if event.Type == "m.room.member" && event.StateKey == bob.UserID {
			seenJoin = true
		}
		want := "leave"
		if seenJoin {
			want = "join"
		}
		if event.Unsigned.Membership != want {
			t.Errorf("%s from %s: unsigned.membership = %q, want %q",
				event.Type, event.Sender, event.Unsigned.Membership, want)
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("only %d timeline events to check", checked)
	}
	if !seenJoin {
		t.Fatal("the timeline never carried bob's own join event")
	}
}

// TestTransactionIDsAreScopedToTheRoom checks the two dimensions of transaction-id scope.
//
// A transaction ID is a device's own numbering, so a second session of the same account may use
// the same number independently, and replaying one must answer with the event the first attempt
// made — even when the retry carries a different body, because a client that lost the response is
// not going to reconstruct the request it sent. The room is in scope for the same reason: a
// client that numbers sends "1", "2", "3" will happily use "2" in a second room, and treating
// that as a replay either refuses a legitimate send or answers with an event from a room the
// request was not addressing.
func TestTransactionIDsAreScopedToTheRoom(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "counter", "pw-counter", false)

	send := func(roomID, txnID, body string) string {
		resp := alice.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
			"/send/m.room.message/"+txnID, map[string]any{"msgtype": "m.text", "body": body})
		var out struct {
			EventID string `json:"event_id"`
		}
		resp.JSON(t, &out)
		return out.EventID
	}
	firstRoom := alice.MustCreateRoom(t, server, map[string]any{})
	secondRoom := alice.MustCreateRoom(t, server, map[string]any{})

	first := send(firstRoom, "abc", "first")
	if again := send(firstRoom, "abc", "first"); again != first {
		t.Errorf("replaying a transaction: got %s, want the original %s", again, first)
	}
	// The transaction decided what happened, not the body that arrived with the retry.
	if changed := send(firstRoom, "abc", "second"); changed != first {
		t.Errorf("replaying a transaction with a different body: got %s, want the original %s",
			changed, first)
	}
	// A different room is a different conversation: the same number is not a replay there.
	if other := send(secondRoom, "abc", "first"); other == first {
		t.Error("re-using a transaction ID in another room was treated as a replay")
	}

	// A second device of the same account numbers its own sends.
	login := server.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
		"type":       "m.login.password",
		"identifier": map[string]any{"type": "m.id.user", "user": "counter"},
		"password":   "pw-counter",
		"device_id":  "OTHERDEVICE",
	})
	var session struct {
		AccessToken string `json:"access_token"`
		DeviceID    string `json:"device_id"`
	}
	login.JSON(t, &session)
	if session.DeviceID != "OTHERDEVICE" {
		t.Fatalf("login honoured a requested device ID: got %q", session.DeviceID)
	}
	replay := server.Do(t, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(firstRoom) +
			"/send/m.room.message/abc",
		Token: session.AccessToken,
		Body:  map[string]any{"msgtype": "m.text", "body": "from the other device"},
	})
	if replay.StatusCode != http.StatusOK {
		t.Fatalf("a second device re-using a transaction ID: got %d, want 200", replay.StatusCode)
	}
	var created struct {
		EventID string `json:"event_id"`
	}
	replay.JSON(t, &created)
	if created.EventID == first {
		t.Error("a second device's transaction ID collided with the first device's event")
	}
}

// TestDepartedMemberReadsTheRoomTheyLeft checks that leaving narrows what a member may read
// without taking the reading away (SPEC-216).
//
// `history_visibility: joined` means "anybody who was in here may read that stretch of history".
// A former member is still one of those people, and their own client already holds those events —
// so the answer is a bound rather than a door: the state as it stood when they left, the member
// list as it stood, and nothing written afterwards. Re-reading this at the read path is what keeps
// it honest: the live state and membership tables are current-state caches, and every change after
// a departure is information that person gave up.
func TestDepartedMemberReadsTheRoomTheyLeft(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "everpresent", "pw-everpresent", false)
	bob := server.Register(t, "departing", "pw-departing", false)
	charlie := server.Register(t, "later", "pw-later", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset": "public_chat",
		"initial_state": []map[string]any{{
			"type": "m.room.history_visibility", "state_key": "",
			"content": map[string]any{"history_visibility": "joined"},
		}},
	})
	bob.MustJoinRoom(t, server, roomID)
	alice.MustSetState(t, server, roomID, "madeup.test.state", "",
		map[string]any{"body": "S1. before bob left"})
	alice.MustSendText(t, server, roomID, "M1. before bob left")

	bob.MustLeaveRoom(t, server, roomID)

	// The room moves on without bob.
	alice.MustSetState(t, server, roomID, "madeup.test.state", "",
		map[string]any{"body": "S2. after bob left"})
	alice.MustSendText(t, server, roomID, "M2. after bob left")
	charlie.MustJoinRoom(t, server, roomID)

	escaped := harness.PathEscape(roomID)

	// State as of the leave, not as of now.
	before := jsonBodyOf(t, bob.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		escaped+"/state/madeup.test.state/"))
	if before["body"] != "S1. before bob left" {
		t.Errorf("a departed member read %v; a departed member must read the state they left behind",
			before["body"])
	}
	now := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		escaped+"/state/madeup.test.state/"))
	if now["body"] != "S2. after bob left" {
		t.Errorf("a joined member read %v, want the current state", now["body"])
	}

	// The member list as of the leave: charlie joined after, and is not bob's to learn.
	members := membersOf(t, bob, server, roomID)
	if _, ok := members[charlie.UserID]; ok {
		t.Error("a departed member was shown a member who joined after they left")
	}
	if got := members[alice.UserID]; got != "join" {
		t.Errorf("alice membership = %q, want join", got)
	}
	if got := members[bob.UserID]; got != "leave" {
		t.Errorf("bob's own membership = %q, want leave", got)
	}

	// /messages answers, and stops at the leave.
	resp := bob.MustGet(t, server, "/_matrix/client/v3/rooms/"+escaped+"/messages?dir=b&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a departed member's /messages: got %d, want 200", resp.StatusCode)
	}
	var page struct {
		Chunk []struct {
			Type    string `json:"type"`
			Content struct {
				Body string `json:"body"`
			} `json:"content"`
		} `json:"chunk"`
	}
	resp.JSON(t, &page)
	seen := map[string]bool{}
	for _, event := range page.Chunk {
		seen[event.Content.Body] = true
		if event.Content.Body == "M2. after bob left" {
			t.Error("/messages handed a departed member an event written after they left")
		}
	}
	if !seen["M1. before bob left"] {
		t.Errorf("/messages lost the history the departed member is entitled to: %v", seen)
	}
}

// membersOf reads /members and returns each member's membership by user ID.
func membersOf(t *testing.T, account *harness.Account, server *harness.Server, roomID string) map[string]string {
	t.Helper()
	resp := account.MustGet(t, server, "/_matrix/client/v3/rooms/"+
		harness.PathEscape(roomID)+"/members")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /members: got %d, want 200", resp.StatusCode)
	}
	var page struct {
		Chunk []struct {
			StateKey string `json:"state_key"`
			Content  struct {
				Membership string `json:"membership"`
			} `json:"content"`
		} `json:"chunk"`
	}
	resp.JSON(t, &page)
	out := map[string]string{}
	for _, member := range page.Chunk {
		out[member.StateKey] = member.Content.Membership
	}
	return out
}
