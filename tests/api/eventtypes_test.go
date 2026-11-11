package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
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

// TestAStateOnlyEventTypeIsNotAcceptedThroughTheSendEndpoint keeps a room's rules honest.
//
// `m.room.power_levels` written through PUT /state is the room's rules. The same type written through
// POST /send is a *timeline* event with no state key: it changes nothing, and yet the room now holds an
// event that looks exactly like the room's power rules to anything reading the timeline rather than the
// state. A room member can put that in front of every other member, which is a lie about the room rather
// than a change to it — and the honest answer is to refuse the write.
func TestAStateOnlyEventTypeIsNotAcceptedThroughTheSendEndpoint(t *testing.T) {
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
		response := bob.Do(t, server, harness.Request{
			Method: http.MethodPut,
			Path: "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) +
				"/send/" + url.PathEscape(eventType) + "/txn-state-as-message",
			Body: map[string]any{"users": map[string]any{"@eve:test.local": 100}},
		})
		if response.StatusCode == http.StatusOK {
			t.Errorf("%s was accepted as a timeline event; it is a state type, and a fake one in the "+
				"timeline is read as the real thing by clients that walk events: %s", eventType, response.Body)
		}
		if body := string(response.Body); strings.Contains(body, "M_UNKNOWN") {
			t.Errorf("%s was refused by something that is not a refusal: %s", eventType, body)
		}
	}

	// The room's real rules are untouched, which is the point of the refusal.
	after := alice.MustGet(t, server,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/state/m.room.power_levels").Body
	if string(before) != string(after) {
		t.Errorf("the room's power levels changed from the attempted writes:\n before: %s\n after:  %s",
			before, after)
	}
}
