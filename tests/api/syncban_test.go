package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/tidwall/gjson"
)

// TestABannedUserIsToldTheRoomLeft is the half of a ban that belongs to the person banned.
//
// POST /rooms/{room}/ban changes a membership to `ban`, and the specification puts the room under
// `rooms.leave` for its target: a banned user is out of the room, and the timeline they are given is
// how a client explains why a room it used to show has disappeared. Answering their /sync with
// nothing at all — no join, no invite, no leave — leaves a client holding a room it cannot open and
// cannot account for, which is the failure mode every client has a special case for.
//
// The ban arrives the same way over federation: a peer pushes the membership event and this server
// stores it. So the fix is in what /sync says about a membership of `ban`, not in how the ban is
// applied.
func TestABannedUserIsToldTheRoomLeft(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	// A position before the ban, so the sync below has to report the change rather than repeat a
	// state bob was already given.
	since := bob.MustSync(t, server, nil).NextBatch

	ban := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/ban",
		Body:   map[string]any{"user_id": bob.UserID, "reason": "because the test says so"},
	})
	if ban.StatusCode != http.StatusOK {
		t.Fatalf("banning a member answered %d: %s", ban.StatusCode, ban.Body)
	}

	// timeout=0 asks for whatever is there without waiting. The waiting is a separate question and
	// it is asked below, because a ban that is never noticed is a sync that simply never returns.
	synced := bob.MustGet(t, server, "/_matrix/client/v3/sync?since="+since+"&timeout=0")
	if synced.StatusCode != http.StatusOK {
		t.Fatalf("sync answered %d: %s", synced.StatusCode, synced.Body)
	}
	var rooms struct {
		Leave map[string]json.RawMessage `json:"leave"`
		Join  map[string]json.RawMessage `json:"join"`
	}
	if err := json.Unmarshal([]byte(gjson.GetBytes(synced.Body, "rooms").Raw), &rooms); err != nil {
		t.Fatalf("decoding the sync's rooms: %v\n%s", err, synced.Body)
	}
	left, ok := rooms.Leave[roomID]
	if !ok {
		t.Fatalf("a banned user's sync does not put the room under leave (rooms=%s)\nfull body: %s",
			gjson.GetBytes(synced.Body, "rooms").Raw, synced.Body)
	}
	if _, stillJoined := rooms.Join[roomID]; stillJoined {
		t.Errorf("the room is still under join for a user who was just banned")
	}
	// And the ban has to wake a sync that is already waiting: a client sitting on a long-poll is
	// not polling, so a change that does not notify is a change nobody is told about.
	// Undoing a ban is two writes: an unban (a membership of `leave`, which the room's rules let the
	// banning user send) and then the invite. Inviting straight from `ban` is refused, and rightly.
	unban := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/unban",
		Body:   map[string]any{"user_id": bob.UserID},
	})
	if unban.StatusCode != http.StatusOK {
		t.Fatalf("unbanning answered %d: %s", unban.StatusCode, unban.Body)
	}

	waking := make(chan gjson.Result, 1)
	go func() {
		response := bob.Do(t, server, harness.Request{
			Method: http.MethodGet,
			Path:   "/_matrix/client/v3/sync?since=" + since + "&timeout=20000",
		})
		waking <- gjson.ParseBytes(response.Body)
	}()
	invite := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/invite",
		Body:   map[string]any{"user_id": bob.UserID},
	})
	if invite.StatusCode != http.StatusOK {
		t.Fatalf("re-inviting the banned user answered %d: %s — unban then invite is how a ban is undone",
			invite.StatusCode, invite.Body)
	}
	select {
	case body := <-waking:
		// The room ID contains dots and a colon, which a gjson path would read as structure.
		var invited struct {
			Rooms struct {
				Invite map[string]json.RawMessage `json:"invite"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal([]byte(body.Raw), &invited); err != nil {
			t.Fatalf("decoding the sync that woke up: %v\n%s", err, body.Raw)
		}
		if _, ok := invited.Rooms.Invite[roomID]; !ok {
			t.Errorf("the waiting sync did not carry the new invite: %s", body.Raw)
		}
	case <-time.After(30 * time.Second):
		t.Errorf("a waiting sync was not woken by an invite after the ban; the long-poll sat there")
	}

	// The membership in the room they were shown says `ban`: a client distinguishes "you left"
	// from "you were thrown out", and the difference is what it tells the user.
	// The event that ended the membership is in the timeline the user is given. "You are out" and
	// "you were thrown out, for this reason" are different messages to a user, and only the event
	// says which happened.
	var leftRoom struct {
		State struct {
			Events []struct {
				Type    string `json:"type"`
				Content struct {
					Membership string `json:"membership"`
				} `json:"content"`
			} `json:"events"`
		} `json:"state"`
		Timeline struct {
			Events []struct {
				Type    string `json:"type"`
				Content struct {
					Membership string `json:"membership"`
					Reason     string `json:"reason"`
				} `json:"content"`
			} `json:"events"`
		} `json:"timeline"`
	}
	if err := json.Unmarshal(left, &leftRoom); err != nil {
		t.Fatalf("decoding the left room: %v\n%s", err, left)
	}
	// The state is there too. A client that has to render a room it is no longer in — the ban
	// notice, the "you no longer have access" screen — draws it from the state it is handed, because
	// it cannot ask the server for more. Synapse sends it, and the specification carries the field for
	// exactly this.
	if !hasBanEvent(leftRoom.State.Events) {
		t.Errorf("the left room carries no state of its own (%d events) — a client rendering the room "+
			"it cannot enter has nothing to draw: %s", len(leftRoom.State.Events), left)
	}

	for _, event := range leftRoom.Timeline.Events {
		if event.Type == "m.room.member" && event.Content.Membership == "ban" {
			if event.Content.Reason == "" {
				t.Logf("the ban carries no reason, which the test sent: %s", event.Content.Reason)
			}
			return
		}
	}
	t.Errorf("the left room's timeline does not carry the ban event; it holds %d events: %s",
		len(leftRoom.Timeline.Events), left)
}

type leftEvent = struct {
	Type    string `json:"type"`
	Content struct {
		Membership string `json:"membership"`
	} `json:"content"`
}

func hasBanEvent(events []leftEvent) bool {
	for _, event := range events {
		if event.Type == "m.room.member" && event.Content.Membership == "ban" {
			return true
		}
	}
	return false
}
