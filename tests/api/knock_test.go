package api_test

// Knocking: a room whose door says "knock first" answering a stranger honestly.
//
// Knocking is the third way into a room, next to an invite and an open join rule, and it is the one that
// keeps a promise the other two cannot make: a user who is not in a room can ask to be let in, and *somebody
// in the room is told*. It is therefore the one path where a homeserver is tempted to pretend — to file the
// request privately, to answer 200 and write nothing, or to hand the knocker the whole room in advance.
// None of those are what the protocol asks for: the knock is a membership event that the room's members can
// see and act on, and what the knocker gets back is just enough state to know they knocked on the right
// door.
//
// The interesting refusals are the point. A room whose join rule is `invite` must refuse a knock rather than
// queue it, because a knock nobody can see is a worse lie than a door that says no. And a room whose join
// rule is `knock` must refuse a *join* from a stranger, because the whole value of knocking is that
// somebody in there has to say yes.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// knock asks this server, as `who`, to knock on a room, and answers with the raw response.
func knock(t *testing.T, server *harness.Server, who *harness.Account, roomID, reason string,
	query url.Values) *harness.Response {
	t.Helper()

	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/knock"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var body any
	if reason != "" {
		body = map[string]string{"reason": reason}
	} else {
		body = map[string]any{}
	}
	return who.Do(t, server, harness.Request{Method: http.MethodPost, Path: path, Body: body})
}

// memberEventFor finds the membership event in a decoded list of events that speaks about userID. When a
// room holds several — a knock and the leave that answered it — the last one wins, because that is the
// order a sync delivers them in and the room's current answer is the newest statement about the user.
func memberEventFor(t *testing.T, events []json.RawMessage, userID string) map[string]any {
	t.Helper()

	var found map[string]any
	for _, raw := range events {
		var event map[string]any
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}
		if event["type"] != "m.room.member" || event["state_key"] != userID {
			continue
		}
		found = event
	}
	return found
}

// membershipIn reports the membership a member event carries, or "" when there is no such event.
func membershipFor(t *testing.T, events []json.RawMessage, userID string) string {
	t.Helper()

	event := memberEventFor(t, events, userID)
	if event == nil {
		return ""
	}
	content, _ := event["content"].(map[string]any)
	membership, _ := content["membership"].(string)
	return membership
}

func TestAKnockIsRefusedWhereTheRoomDoesNotAskForKnocks(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})

	response := knock(t, server, bob, roomID, "Hello, I know alice", nil)
	if response.StatusCode == http.StatusOK {
		t.Fatalf("a stranger was allowed to knock on a room whose join rule is invite: %s", response.Body)
	}
	// A refusal is not a queue. If the room kept the request, the members would never learn that
	// anybody asked, and the knocker would believe they had.
	sync := alice.MustSync(t, server, url.Values{})
	if room, ok := sync.Rooms.Join[roomID]; ok {
		if membershipFor(t, room.Timeline.Events, bob.UserID) != "" {
			t.Fatalf("the refused knock was written into the room anyway:\n%s", response.Body)
		}
	}
	if _, ok := sync.Rooms.Knock[roomID]; ok {
		t.Fatalf("the knocking user was put in the knock section of sync for a room that refused them")
	}
}

func TestAKnockOnARoomThatAllowsItIsWrittenWhereTheRoomCanSeeIt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"room_version": "7", "preset": "private_chat"})
	alice.MustSetState(t, server, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})

	// Knocking must also be possible where the room's own version is the reason: version 7 is the first
	// that knows the membership, and complement knocks on v7 and v10.
	const reason = "I would like to join, please"
	response := knock(t, server, bob, roomID, reason, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a knock on a room that allows it got %d: %s", response.StatusCode, response.Body)
	}

	// The knocker's own sync: the room appears in the `knock` section with just enough state to know
	// what was knocked on, including their own membership event and the reason they gave.
	sync := bob.MustSync(t, server, url.Values{})
	knocked, ok := sync.Rooms.Knock[roomID]
	if !ok {
		t.Fatalf("the knocked room is missing from the knock section of the knocker's sync:\n%s",
			marshalSync(t, sync))
	}
	member := memberEventFor(t, knocked.KnockState.Events, bob.UserID)
	if member == nil {
		t.Fatalf("the knock state does not carry the knocker's own membership event:\n%s",
			marshalEvents(t, knocked.KnockState.Events))
	}
	content, _ := member["content"].(map[string]any)
	if content["membership"] != "knock" {
		t.Fatalf("the knocker's event says membership %v, want knock", content["membership"])
	}
	if content["reason"] != reason {
		t.Fatalf("the knock state lost the reason: %v", content)
	}

	// The room's own members see the knock as a membership event in the room, because that is the only
	// way a knock can be answered: somebody in there has to know somebody knocked.
	inRoom := alice.MustSync(t, server, url.Values{})
	joined, ok := inRoom.Rooms.Join[roomID]
	if !ok {
		t.Fatalf("the host lost the room from their sync while a knock was happening")
	}
	if got := membershipFor(t, joined.Timeline.Events, bob.UserID); got != "knock" {
		t.Fatalf("the room's members see %q for the knocker, want knock", got)
	}

	// Knocking again is legal: a reason can improve, and a knock that has been sitting unanswered for a
	// week is worth repeating. The room should end up agreeing that the user is still knocking.
	again := knock(t, server, bob, roomID, "Still there?", nil)
	if again.StatusCode != http.StatusOK {
		t.Fatalf("knocking a second time got %d: %s", again.StatusCode, again.Body)
	}
	after := alice.MustSync(t, server, url.Values{})
	if got := membershipFor(t, after.Rooms.Join[roomID].Timeline.Events, bob.UserID); got != "knock" {
		t.Fatalf("after a second knock the room sees %q, want knock", got)
	}
}

func TestARoomThatAsksForKnocksStillRefusesAJoin(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"room_version": "7", "preset": "private_chat"})
	alice.MustSetState(t, server, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})

	// The value of knocking is that somebody in the room has to agree. If a stranger could simply join,
	// the knock rule would be a slower way of being public.
	joined := bob.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/join",
		Body:   map[string]any{},
	})
	if joined.StatusCode == http.StatusOK {
		t.Fatalf("a stranger joined a room whose join rule is knock: %s", joined.Body)
	}
}

func TestAKnockCanBeWithdrawnAndRepeated(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"room_version": "7", "preset": "private_chat"})
	alice.MustSetState(t, server, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})

	if response := knock(t, server, bob, roomID, "Let me in?", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("the first knock got %d: %s", response.StatusCode, response.Body)
	}

	// Changing one's mind is part of the protocol: leaving a room one knocked on is the withdrawal, and
	// it has to be visible as such in the knocker's own sync.
	since := bob.MustSync(t, server, url.Values{}).NextBatch
	bob.MustLeaveRoom(t, server, roomID)
	var left bool
	for i := 0; i < 20 && !left; i++ {
		sync := bob.MustSync(t, server, url.Values{"since": {since}})
		since = sync.NextBatch
		if room, ok := sync.Rooms.Leave[roomID]; ok {
			if membershipFor(t, room.Timeline.Events, bob.UserID) == "leave" {
				left = true
			}
		}
	}
	if !left {
		t.Fatalf("withdrawing a knock left no leave for the knocker to see")
	}

	// And the door is still open: a withdrawn knock is not a ban.
	if response := knock(t, server, bob, roomID, "Let me in... again?", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("re-knocking after withdrawing got %d: %s", response.StatusCode, response.Body)
	}
}

func TestAModeratorCanShutTheDoorOnAKnock(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"room_version": "7", "preset": "private_chat"})
	alice.MustSetState(t, server, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})

	if response := knock(t, server, bob, roomID, "Please", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("the knock got %d: %s", response.StatusCode, response.Body)
	}

	// Rejecting a knock is a kick: a member with the power writes the knocker's membership to leave.
	kicked := alice.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/kick",
		map[string]any{"user_id": bob.UserID, "reason": "I don't think so"})
	if kicked.StatusCode != http.StatusOK {
		t.Fatalf("rejecting a knock got %d: %s", kicked.StatusCode, kicked.Body)
	}
	seen := alice.MustSync(t, server, url.Values{})
	if got := membershipFor(t, seen.Rooms.Join[roomID].Timeline.Events, bob.UserID); got != "leave" {
		t.Fatalf("after a rejection the room sees %q for the knocker, want leave", got)
	}

	// A rejection is not a verdict either: the knocker may ask again, which is what the specification
	// says and what complement checks. Only a ban says no for good.
	if response := knock(t, server, bob, roomID, "Pleeease let me in?", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("knocking after a rejection got %d: %s", response.StatusCode, response.Body)
	}
}

func TestAKnockNeedsARoomVersionThatKnowsTheMembership(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	// Version 1 has no knock membership. Its authorisation rules cannot produce one, so this server must
	// say that the room is the wrong shape rather than write an event no peer will accept.
	roomID := alice.MustCreateRoom(t, server, map[string]any{"room_version": "1", "preset": "private_chat"})
	alice.MustSetState(t, server, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})

	response := knock(t, server, bob, roomID, "Hello?", nil)
	if response.StatusCode == http.StatusOK {
		t.Fatalf("a knock was accepted in room version 1, which has no such membership: %s", response.Body)
	}
}

func TestAKnockIsRefusedForTheRightReasonOnAnOldRoom(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"room_version": "1", "preset": "private_chat"})
	alice.MustSetState(t, server, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})
	response := knock(t, server, bob, roomID, "Hello?", nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("an old room's knock was answered %d, want 400: %s", response.StatusCode, response.Body)
	}
	// And refused for the right reason. "You may not" sends a client looking for a permission that
	// cannot be granted; the room is the wrong shape, and that is a thing the user can act on by
	// asking for a newer room.
	var body struct {
		ErrCode string `json:"errcode"`
	}
	response.JSON(t, &body)
	if body.ErrCode != "M_INCOMPATIBLE_ROOM_VERSION" {
		t.Fatalf("a knock on room version 1 was refused with %s, want M_INCOMPATIBLE_ROOM_VERSION: %s",
			body.ErrCode, response.Body)
	}
}

func TestAKnockRestrictedRoomTakesKnocksToo(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	// `knock_restricted` (room versions 10 and up) is the join rule that means "you may be let in on the
	// strength of another room, *or* you may knock". The second half is this: a room that mixes the two
	// must not lose the knock, because a user in no room at all has no other way in.
	roomID := alice.MustCreateRoom(t, server, map[string]any{"room_version": "10", "preset": "private_chat"})
	alice.MustSetState(t, server, roomID, "m.room.join_rules", "",
		map[string]any{"join_rule": "knock_restricted", "allow": []any{}})

	response := knock(t, server, bob, roomID, "I was told to knock here", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a knock on a knock_restricted room got %d: %s", response.StatusCode, response.Body)
	}
	sync := bob.MustSync(t, server, url.Values{})
	events := sync.Rooms.Knock[roomID].KnockState.Events
	if membershipFor(t, events, bob.UserID) != "knock" {
		t.Fatalf("the knock on a knock_restricted room is not in the knocker's sync:\n%s",
			marshalEvents(t, events))
	}
	inRoom := alice.MustSync(t, server, url.Values{})
	if got := membershipFor(t, inRoom.Rooms.Join[roomID].Timeline.Events, bob.UserID); got != "knock" {
		t.Fatalf("the room sees %q for a knock_restricted knock, want knock", got)
	}
}

func TestAKnockIsShownTheDoorAndNotTheHouse(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "host", "pw-host", false)
	carol := server.Register(t, "insider", "pw-insider", false)
	bob := server.Register(t, "knocker", "pw-knock", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"room_version": "7", "preset": "private_chat"})
	alice.MustInvite(t, server, roomID, carol.UserID)
	carol.MustJoinRoom(t, server, roomID)
	alice.MustSendText(t, server, roomID, "the plumbing under the sink is going")
	alice.MustSetState(t, server, roomID, "m.room.join_rules", "", map[string]any{"join_rule": "knock"})
	alice.MustSetState(t, server, roomID, "m.room.name", "", map[string]any{"name": "Households"})

	if response := knock(t, server, bob, roomID, "Hello?", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("the knock got %d: %s", response.StatusCode, response.Body)
	}

	// What a knocking user is shown is the front of the house: enough to know they knocked on the right
	// door, and nothing that belongs to the people inside it.
	events := bob.MustSync(t, server, url.Values{}).Rooms.Knock[roomID].KnockState.Events
	if len(events) == 0 {
		t.Fatalf("a knocking user was shown no state at all, so the knock looks like a request that vanished")
	}
	var sawName, sawJoinRules, sawCreate bool
	for _, raw := range events {
		var event struct {
			Type     string `json:"type"`
			StateKey string `json:"state_key"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}
		switch event.Type {
		case "m.room.name":
			sawName = true
		case "m.room.join_rules":
			sawJoinRules = true
		case "m.room.create":
			sawCreate = true
		case "m.room.history_visibility", "m.room.avatar", "m.room.canonical_alias":
			// Part of the front door: what a visitor is promised about the history they cannot
			// read, and how the room is drawn. Neither says anything about who is inside.
		case "m.room.member":
			if event.StateKey != bob.UserID {
				t.Fatalf("a knocking user was handed the membership event of %s: they are not in this room, "+
					"and the member list is precisely what they are knocking to find out", event.StateKey)
			}
		default:
			t.Fatalf("a knocking user was shown %q, which is not part of a room's front door:\n%s",
				event.Type, raw)
		}
	}
	if !sawName || !sawJoinRules || !sawCreate {
		t.Fatalf("the room's front door is incomplete (name %v, join rules %v, create %v)",
			sawName, sawJoinRules, sawCreate)
	}
}

func marshalSync(t *testing.T, sync harness.SyncResponse) string {
	t.Helper()
	raw, err := json.Marshal(sync)
	if err != nil {
		t.Fatalf("the sync response cannot be shown: %v", err)
	}
	return string(raw)
}

func marshalEvents(t *testing.T, events []json.RawMessage) string {
	t.Helper()
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("the events cannot be shown: %v", err)
	}
	return string(raw)
}
