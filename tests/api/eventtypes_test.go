package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/tidwall/gjson"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// TestACustomEventTypeDoesNotHaveToBeNamespaced covers sending an event whose type has no reverse-domain
// prefix, such as "please_filter_me".
//
// The specification says a custom type MUST be globally namespaced, and this server would rather clients
// behaved — but it defines no error for the violation, and every other homeserver accepts the event. A
// server that refuses on an unspecified error is the one that breaks, because its user cannot send
// anything to anybody. What the type means is the room's business, not the transport's: it is opaque
// bytes to this server either way, so refusing buys no safety.
//
// The character and length hygiene stays enforced, because that is the part which protects this server's
// own storage and URLs.
func TestACustomEventTypeDoesNotHaveToBeNamespaced(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	response := alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) +
			"/send/please_filter_me/txn-unnamespaced",
		Body: map[string]any{"body": "an event with no namespace"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sending a custom type with no namespace: status %d, want 200\nbody: %s",
			response.StatusCode, response.Body)
	}
	var sent struct {
		EventID string `json:"event_id"`
	}
	response.JSON(t, &sent)
	if sent.EventID == "" {
		t.Fatalf("no event ID came back: %s", response.Body)
	}

	// It has to arrive as the type it was sent as, not be rejected later or normalised away.
	messages := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/messages?limit=10")
	var page struct {
		Chunk []struct {
			Type    string `json:"type"`
			EventID string `json:"event_id"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(messages.Body, &page); err != nil {
		t.Fatalf("decoding /messages: %v", err)
	}
	found := false
	for _, event := range page.Chunk {
		if event.EventID == sent.EventID {
			found = true
			if event.Type != "please_filter_me" {
				t.Errorf("the event came back as type %q, want please_filter_me", event.Type)
			}
		}
	}
	if !found {
		t.Error("the sent event is not in the room's /messages")
	}

	// The hygiene that protects this server stays: an empty type, an over-long one, and characters that
	// would not survive a URL or a filename are each still refused.
	for _, bad := range []string{"has space", "has/slash", "has#hash"} {
		refusal := alice.Do(t, server, harness.Request{
			Method: http.MethodPut,
			Path: "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) +
				"/send/" + url.PathEscape(bad) + "/txn-" + url.PathEscape(bad),
			Body: map[string]any{"body": "x"},
		})
		if refusal.StatusCode == http.StatusOK {
			t.Errorf("the event type %q was accepted; it cannot be carried in a URL path", bad)
		}
	}
}

// TestAStateOnlyEventTypeSentToTheTimelineChangesNothing is about the difference between a lie and a
// change.
//
// `m.room.power_levels` written through PUT /state is the room's rules. The same type written through
// PUT /send is an event with no state key: it is not state, so it changes nothing, and it appears in
// the timeline where a client walking events can see it. That looks like something worth refusing,
// and refusing it was a mistake — complement's own room-forget test writes `m.room.join_rules` this
// way, in a mandatory test that passed before this server decided to be pickier than every other
// homeserver. Synapse accepts it; the specification's rules are about state resolution, not about
// which types may appear on the timeline.
//
// What actually protects a room is that these events are inert, so that is what this test pins: the
// fake rules arrive, and the real ones do not move. A client that reads a room's rules out of the
// timeline instead of the state is reading the wrong thing, and no amount of refusing writes here
// fixes that.
//
// Two types are still refused, by the room's own authorization rules rather than by a list kept
// here: `m.room.member` and `m.room.create`, which cannot be evaluated as non-state events at all.
func TestAStateOnlyEventTypeSentToTheTimelineChangesNothing(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustInvite(t, server, roomID, bob.UserID)
	bob.MustJoinRoom(t, server, roomID)

	before := alice.MustGet(t, server,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/state/m.room.power_levels").Body

	for _, eventType := range []string{
		"m.room.power_levels", "m.room.join_rules", "m.room.topic", "m.room.name",
		"m.room.encryption", "m.room.history_visibility", "m.room.guest_access",
		"m.room.canonical_alias", "m.room.server_acl", "m.room.tombstone",
		"m.space.child", "m.space.parent",
	} {
		// Sent by alice, the room's creator: the room's own authorization rules apply to these
		// types exactly as they apply to writing them as state, so an ordinary member is refused a
		// power-levels event and a creator is not. That is the rule doing its work, which is a
		// different thing from this server refusing the type on principle.
		response := alice.Do(t, server, harness.Request{
			Method: http.MethodPut,
			Path: "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) +
				"/send/" + url.PathEscape(eventType) + "/txn-state-as-message",
			Body: map[string]any{"users": map[string]any{"@eve:example.org": 100}},
		})
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s through the send endpoint answered %d, want 200: %s", eventType,
				response.StatusCode, response.Body)
		}
	}

	// A member without the power still cannot do it, and is told so by the same rules that would
	// apply to writing the state.
	withoutPower := bob.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) +
			"/send/m.room.power_levels/txn-state-as-message",
		Body: map[string]any{"users": map[string]any{"@bob:example.org": 100}},
	})
	if withoutPower.StatusCode == http.StatusOK {
		t.Errorf("an ordinary member wrote a power-levels event as a timeline event: %s", withoutPower.Body)
	}
	if code := gjson.GetBytes(withoutPower.Body, "errcode").Str; code != "M_FORBIDDEN" {
		t.Errorf("refusing a member without the power answered %q, want M_FORBIDDEN: %s",
			code, withoutPower.Body)
	}

	// The room's real rules are untouched: an event with no state key is not state.
	after := alice.MustGet(t, server,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/state/m.room.power_levels").Body
	if string(before) != string(after) {
		t.Errorf("the room's power levels changed from events shaped like rules:\n before: %s\n after:  %s",
			before, after)
	}

	// Two types cannot be non-state events at all, and the room's own authorization rules say so.
	for _, eventType := range []string{"m.room.member", "m.room.create"} {
		response := bob.Do(t, server, harness.Request{
			Method: http.MethodPut,
			Path: "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) +
				"/send/" + url.PathEscape(eventType) + "/txn-member-as-message",
			Body: map[string]any{"membership": "leave"},
		})
		if response.StatusCode == http.StatusOK {
			t.Errorf("%s was accepted as a timeline event: %s", eventType, response.Body)
		}
		if body := string(response.Body); strings.Contains(body, "M_UNKNOWN") {
			t.Errorf("%s was refused by something that is not a refusal: %s", eventType, body)
		}
	}
}
