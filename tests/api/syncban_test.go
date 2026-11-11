package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
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
	// And an invite after the ban has to wake a sync that is already waiting: a client sitting on a
	// long-poll is not polling, so a change that does not notify is a change nobody is told about.
	// The waiter starts after the unban, because the ban itself is allowed to wake it first — that
	// would be the same property, demonstrated twice.
	unban := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/unban",
		Body:   map[string]any{"user_id": bob.UserID},
	})
	if unban.StatusCode != http.StatusOK {
		t.Fatalf("unbanning answered %d: %s", unban.StatusCode, unban.Body)
	}
	nextSync := bob.MustGet(t, server, "/_matrix/client/v3/sync?since="+since+"&timeout=0")
	token := gjson.GetBytes(nextSync.Body, "next_batch").String()

	waking := make(chan gjson.Result, 1)
	go func() {
		response := bob.Do(t, server, harness.Request{
			Method: http.MethodGet,
			Path:   "/_matrix/client/v3/sync?since=" + token + "&timeout=20000",
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

	// The event that ended the membership is in the timeline the user is given. "You are out" and
	// "you were thrown out, for this reason" are different messages to a user, and only the event
	// says which happened.
	var leftRoom struct {
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

// TestABanFromAnotherServerReachesTheVictim is the other half of a ban: the room is on somebody
// else's homeserver, and the person doing the banning is not this server's user.
//
// The event has to be *pushed*. A banned user's reads of that room are now refused, so a server that
// waits for the victim's homeserver to ask will find that it never does, and the victim goes on holding
// a room they were thrown out of — sending messages that will be refused, and seeing a member list that
// no longer includes them.
//
// Which is why the queue that decides whom to tell cannot be computed from "who is joined". By the time
// the ban exists, its target is by definition no longer joined: the event is what removed them. Computing
// the destination list after applying the event drops the one server that must be told, and nothing
// downstream can notice, because the peer never knows it was skipped.
func TestABanFromAnotherServerReachesTheVictim(t *testing.T) {
	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	bob := hs2.Register(t, "bob", "pw-bob", false)
	roomID := bob.MustCreateRoom(t, hs2, map[string]any{"preset": "public_chat"})

	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)
	alice.MustPostJSON(t, hs1, "/_matrix/client/v3/join/"+url.PathEscape(roomID)+
		"?server_name="+url.QueryEscape(hs2.ServerName), map[string]any{}).MustStatus(t, 200)
	hs1.DrainFederation(t)
	since := alice.MustSync(t, hs1, url.Values{"timeout": []string{"0"}}).NextBatch

	ban := bob.Do(t, hs2, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/ban",
		Body:   map[string]any{"user_id": alice.UserID, "reason": "federation must carry this"},
	})
	if ban.StatusCode != http.StatusOK {
		t.Fatalf("banning a remote member answered %d: %s", ban.StatusCode, ban.Body)
	}
	hs2.DrainFederation(t)

	// Alice's own homeserver now knows she was banned, and says so in her sync.
	var left map[string]json.RawMessage
	for attempt := 0; attempt < 6; attempt++ {
		synced := alice.MustGet(t, hs1, "/_matrix/client/v3/sync?since="+since+"&timeout=0")
		left = decodeLeaveSection(t, synced.Body, roomID)
		if len(left) > 0 {
			return
		}
		hs1.DrainFederation(t)
		hs2.DrainFederation(t)
	}
	t.Errorf("a ban issued on the room's own server never reached the victim's server: alice's sync has "+
		"no leave entry for %s", roomID)

	// The same care is needed to let them back in. An invite is a membership event aimed at a user
	// this server does not own, and if its target is collapsed to a localpart it invites the local
	// account of that name — which may not exist — while the remote user is never invited at all.
	// A ban and its reversal then disagree about who was in the room.
	unban := bob.Do(t, hs2, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/unban",
		Body:   map[string]any{"user_id": alice.UserID},
	})
	if unban.StatusCode != http.StatusOK {
		t.Fatalf("unbanning a remote member answered %d: %s", unban.StatusCode, unban.Body)
	}
	reinvite := bob.Do(t, hs2, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/invite",
		Body:   map[string]any{"user_id": alice.UserID},
	})
	if reinvite.StatusCode != http.StatusOK {
		t.Fatalf("re-inviting a remote member answered %d: %s", reinvite.StatusCode, reinvite.Body)
	}
	hs2.DrainFederation(t)
	for attempt := 0; attempt < 4; attempt++ {
		synced := alice.MustGet(t, hs1, "/_matrix/client/v3/sync?since="+since+"&timeout=0")
		if gjson.GetBytes(synced.Body, "rooms.invite."+gjson.Escape(roomID)).Exists() {
			return
		}
		hs1.DrainFederation(t)
		hs2.DrainFederation(t)
	}
	t.Errorf("the unban and re-invite never reached the remote user either: alice's sync shows no invite "+
		"for %s, so the ban and its reversal named different people", roomID)
}

// decodeLeaveSection returns the room's entry under rooms.leave, if it has one.
func decodeLeaveSection(t *testing.T, body []byte, roomID string) map[string]json.RawMessage {
	t.Helper()
	var rooms struct {
		Leave map[string]json.RawMessage `json:"leave"`
	}
	if err := json.Unmarshal([]byte(gjson.GetBytes(body, "rooms").Raw), &rooms); err != nil {
		t.Fatalf("decoding the sync's rooms: %v\n%s", err, body)
	}
	entry, ok := rooms.Leave[roomID]
	if !ok {
		return nil
	}
	return map[string]json.RawMessage{roomID: entry}
}
