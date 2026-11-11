package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// TestLeftRoomShowsTheRoomAsTheReaderLeftIt checks what a departed member is told about a room.
//
// Leaving narrows what a person may see; it does not erase what they saw. So the leave section carries
// the room up to the moment they left — not the events written after, which belong to the people still
// in it — and it does so once. A room that reappears in every sync for as long as other people keep
// talking in it is a client showing a room it left scrolling past forever.
func TestLeftRoomShowsTheRoomAsTheReaderLeftIt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "stayer", "pw-stayer", false)
	bob := server.Register(t, "departed", "pw-departed", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	since := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch
	alice.MustSendText(t, server, roomID, "before the leave")
	bob.MustLeaveRoom(t, server, roomID)
	alice.MustSendText(t, server, roomID, "after the leave")

	// Both the initial sync and the incremental one from a token taken before the leave have to
	// stop at the leave.
	for name, syncReq := range map[string]url.Values{
		"initial":     {"timeout": []string{"0"}},
		"incremental": {"timeout": []string{"0"}, "since": []string{since}},
	} {
		sync := bob.MustSync(t, server, syncReq)
		leave, ok := sync.Rooms.Leave[roomID]
		if !ok {
			t.Fatalf("%s sync: the left room is missing from the leave section", name)
		}
		bodies := messageBodies(harness.DecodeEvents(t, leave.Timeline.Events))
		if !strings.Contains(strings.Join(bodies, ","), "before the leave") {
			t.Errorf("%s sync: the leave timeline is %v, want the message sent before the leave",
				name, bodies)
		}
		if strings.Contains(strings.Join(bodies, ","), "after the leave") {
			t.Errorf("%s sync: the leave timeline shows history written after the reader left: %v",
				name, bodies)
		}
		// A left room is described by its timeline *or* its state, never both. When the timeline
		// carries the events the reader saw before leaving, that is the description; a state array
		// beside it is a second, staler answer to the same question, and complement's archive test
		// fails one that sends both. When the timeline is empty — a `limit: 0` filter, a room left
		// before anything was sent — the state is what describes the room, which is the other test.
		if len(leave.State.Events) != 0 {
			t.Errorf("%s sync: the leave section carries state next to a non-empty timeline, so a "+
				"client has two descriptions of one room: %d state events", name, len(leave.State.Events))
		}
	}

	// Once the client's token is past the leave, the room is not mentioned again — however much
	// happens inside it.
	alice.MustSendText(t, server, roomID, "more chatter")
	latest := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{since}}).NextBatch
	sync := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{latest}})
	if leave, ok := sync.Rooms.Leave[roomID]; ok {
		t.Errorf("a room left long ago is still in the leave section: %+v", leave)
	}
}

// TestLeftRoomAlwaysCarriesATimeline checks that a left room's entry has the shape a client expects
// whatever it contains.
//
// A client that asked for no timeline events still asked for a timeline. Answering with the key
// missing forces the client to treat "absent" as a state to reason about, when the answer it needs is
// simply "none" — and a room the reader left before anything happened is a normal thing to see.
func TestLeftRoomAlwaysCarriesATimeline(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "earlybird", "pw-earlybird", false)
	bob := server.Register(t, "flake", "pw-flake", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	bob.MustLeaveRoom(t, server, roomID)

	// A timeline limit of zero asks for the room with no events at all.
	raw := bob.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/sync?timeout=0&filter=" + url.QueryEscape(`{"room":{"timeline":{"limit":0},"include_leave":true}}`),
	})
	raw.MustStatus(t, http.StatusOK)
	var sync struct {
		Rooms struct {
			Leave map[string]struct {
				Timeline *struct {
					Events []json.RawMessage `json:"events"`
				} `json:"timeline"`
				State *struct {
					Events []json.RawMessage `json:"events"`
				} `json:"state"`
			} `json:"leave"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(raw.Body, &sync); err != nil {
		t.Fatalf("sync is unreadable: %v", err)
	}
	room, ok := sync.Rooms.Leave[roomID]
	if !ok {
		t.Fatalf("the left room is missing from the leave section")
	}
	if room.Timeline == nil {
		t.Fatal("the leave entry has no timeline object; a client asked for a timeline and got a guess")
	}
	if len(room.Timeline.Events) != 0 {
		t.Errorf("a timeline limit of zero returned %d events", len(room.Timeline.Events))
	}
	// With no timeline to explain the room, the state has to carry it, including the leave itself.
	if room.State == nil || len(room.State.Events) == 0 {
		t.Fatal("a leave entry with an empty timeline carries no state, so the client cannot see what it left")
	}
	seenLeave := false
	for _, raw := range room.State.Events {
		var event struct {
			Type     string `json:"type"`
			StateKey string `json:"state_key"`
			Content  struct {
				Membership string `json:"membership"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}
		if event.Type == "m.room.member" && event.StateKey == bob.UserID && event.Content.Membership == "leave" {
			seenLeave = true
		}
	}
	if !seenLeave {
		t.Error("the leave event is missing from the state the client is left with")
	}
}

// TestIncrementalSyncOmitsUnchangedRooms checks that an incremental sync says only what changed.
//
// A sync response is a diff, and a diff that lists files it did not touch is not a diff. Clients pay
// for every room they are handed — repainting it, re-fetching its state, waking their own
// notifications — so a room reported as "nothing happened here either" is work that pretends to be
// news. A client that genuinely wants the whole picture asks for it with full_state, and gets it.
func TestIncrementalSyncOmitsUnchangedRooms(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "quiet", "pw-quiet", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	first := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	if _, ok := first.Rooms.Join[roomID]; !ok {
		t.Fatal("an initial sync must include the room, or a new client has nothing to render")
	}

	// Nothing has happened since that token, so the next sync should be quiet about it.
	second := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{first.NextBatch}})
	if room, ok := second.Rooms.Join[roomID]; ok {
		t.Errorf("an unchanged room was reported anyway: timeline=%d state=%d",
			len(room.Timeline.Events), len(room.State.Events))
	}

	// full_state asks for the opposite and has to be honoured.
	full := alice.MustSync(t, server, url.Values{
		"timeout": []string{"0"}, "since": []string{first.NextBatch}, "full_state": []string{"true"},
	})
	if _, ok := full.Rooms.Join[roomID]; !ok {
		t.Error("full_state was ignored, so a client cannot ask for the whole picture")
	}

	// And a room with news is still reported.
	alice.MustSendText(t, server, roomID, "something happened")
	news := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{second.NextBatch}})
	room, ok := news.Rooms.Join[roomID]
	if !ok {
		t.Fatal("a room with a new event was left out of the sync")
	}
	if len(room.Timeline.Events) == 0 {
		t.Error("the reported room carries no timeline")
	}
}

// TestJoiningAfterTheTokenMakesTheTimelineLimited pins what `limited` means for a room the client
// has just been allowed into.
//
// The client's token names a moment before it could see this room, and the room already has a
// history from that moment. The server cannot deliver that history as though the client had been
// told it late — the client is being introduced to the room, not catching up on it — so the timeline
// it receives starts somewhere in the middle. `limited` is how the server admits that: it is the flag
// that makes a client page back with /messages instead of concluding it has seen everything there is.
func TestJoiningAfterTheTokenMakesTheTimelineLimited(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "oldrOOM", "pw-olroom", false)
	bob := server.Register(t, "latereader", "pw-latereader", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	for _, body := range []string{"before you arrived 1", "before you arrived 2"} {
		alice.MustSendText(t, server, roomID, body)
	}

	// Bob's token is taken while he is still outside the room.
	bobSync := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	bob.MustJoinRoom(t, server, roomID)
	// The join itself, and one more message after it.
	alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	alice.MustSendText(t, server, roomID, "after you arrived")

	sync := bob.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{bobSync.NextBatch}})
	room, ok := sync.Rooms.Join[roomID]
	if !ok {
		t.Fatal("a room the client has just joined was left out of the sync")
	}
	if len(room.Timeline.Events) == 0 {
		t.Fatal("the newly joined room carries an empty timeline")
	}
	if !room.Timeline.Limited {
		t.Errorf("a timeline that starts at the join reported limited=false, so the client would never page back for the history it missed")
	}

	// A client that has been in the room the whole time, and is simply catching up, must not be
	// told the same thing: nothing was dropped from its timeline.
	aliceSync := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	alice.MustSendText(t, server, roomID, "one more")
	caught := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{aliceSync.NextBatch}})
	if room, ok := caught.Rooms.Join[roomID]; ok && room.Timeline.Limited {
		t.Errorf("a reader who missed nothing was told their timeline was limited")
	}
}
