package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// createFilter stores a filter and returns its ID.
func createFilter(t *testing.T, server *harness.Server, account *harness.Account, definition map[string]any) string {
	t.Helper()
	resp := account.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/user/" + harness.PathEscape(account.UserID) + "/filter",
		Body:   definition,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create filter: %d %s\nSERVER LOG:\n%s", resp.StatusCode, string(resp.Body),
			lastServerError(server))
	}
	body := map[string]any{}
	resp.JSON(t, &body)
	id, _ := body["filter_id"].(string)
	if id == "" {
		t.Fatalf("no filter_id in %v", body)
	}
	return id
}

// TestFilterRoundTrip covers saving a filter and reading it back.
//
// The interesting property is that the answer is what was sent, including keys this server does
// not act on: a filter that changes by being saved is a client configuration that silently drifts.
func TestFilterRoundTrip(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "filterowner", "pw-filterowner", false)

	definition := map[string]any{
		"room": map[string]any{
			"timeline": map[string]any{"limit": float64(10), "types": []string{"m.room.message"}},
			"state":    map[string]any{"types": []string{}},
		},
		// A key from a spec revision this server does not implement. It must survive.
		"future_extension": map[string]any{"keep": "me"},
	}
	id := createFilter(t, server, alice, definition)

	stored := jsonBodyOf(t, alice.MustGet(t, server, "/_matrix/client/v3/user/"+
		harness.PathEscape(alice.UserID)+"/filter/"+id))
	room, _ := stored["room"].(map[string]any)
	timeline, _ := room["timeline"].(map[string]any)
	if timeline["limit"] != float64(10) {
		t.Errorf("room.timeline.limit came back as %v, want 10", timeline["limit"])
	}
	future, _ := stored["future_extension"].(map[string]any)
	if future == nil || future["keep"] != "me" {
		t.Errorf("an unimplemented key was dropped from the stored filter: %v", stored)
	}

	// Two filters are two filters: saving twice must not overwrite.
	second := createFilter(t, server, alice, map[string]any{"room": map[string]any{}})
	if second == id {
		t.Fatalf("every filter of an account gets the same ID %q", id)
	}

	// An unknown ID is a bad request, and another account's ID is the same thing — never a
	// listing of it.
	alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/filter/9999",
	}).MustStatus(t, http.StatusBadRequest)

	bob := server.Register(t, "filterpeer", "pw-filterpeer", false)
	bob.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/user/" + harness.PathEscape(bob.UserID) + "/filter/" + id,
	}).MustStatus(t, http.StatusBadRequest)
	alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/user/" + harness.PathEscape(bob.UserID) + "/filter/" + id,
	}).MustStatus(t, http.StatusForbidden)

	// A body that is not an object cannot be a set of rules.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/filter",
		Body:   []any{},
	}).MustStatus(t, http.StatusBadRequest)
}

// TestFilterRejectsBadShapes checks that a filter whose shape is wrong is refused.
//
// The rule is narrow on purpose: unknown keys are accepted, because a client may know a newer
// spec than this server does, but a *known* key of the wrong kind must not be stored. A "types"
// that turned out to be a string would match nothing, and the client would be shown an empty
// timeline with a 200 saying its filter worked.
func TestFilterRejectsBadShapes(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "badfilter", "pw-badfilter", false)

	notAnObject := "not_an_object"
	notAList := "not_a_list"
	for name, definition := range map[string]map[string]any{
		"presence not an object":     {"presence": notAnObject},
		"account data not an object": {"account_data": notAnObject},
		"timeline not an object":     {"room": map[string]any{"timeline": notAnObject}},
		"state not an object":        {"room": map[string]any{"state": notAnObject}},
		"ephemeral not an object":    {"room": map[string]any{"ephemeral": notAnObject}},
		"room data not an object":    {"room": map[string]any{"account_data": notAnObject}},
		"types not a list":           {"room": map[string]any{"timeline": map[string]any{"types": notAList}}},
		"not_types not a list":       {"room": map[string]any{"timeline": map[string]any{"not_types": notAList}}},
		"rooms not a list":           {"room": map[string]any{"timeline": map[string]any{"rooms": notAList}}},
		"senders not a list":         {"room": map[string]any{"timeline": map[string]any{"senders": notAList}}},
		"types not strings":          {"room": map[string]any{"timeline": map[string]any{"types": []int{1}}}},
		"bad room ID":                {"room": map[string]any{"timeline": map[string]any{"rooms": []string{"not_a_room_id"}}}},
		"bad sender ID":              {"room": map[string]any{"timeline": map[string]any{"senders": []string{"not_a_sender_id"}}}},
	} {
		resp := alice.Do(t, server, harness.Request{
			Method: http.MethodPost,
			Path:   "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/filter",
			Body:   definition,
		})
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("%s: got %d, want a 4xx", name, resp.StatusCode)
		}
	}

	// A key this server has never heard of is not the client's mistake.
	createFilter(t, server, alice, map[string]any{
		"some_future_key": map[string]any{"would_reject": "this if the rule were inverted"},
	})
}

// TestSyncHonoursFilter covers what a filter changes about a sync.
func TestSyncHonoursFilter(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "filteredsync", "pw-filteredsync", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	// Interleave two event types so a type filter has something to exclude.
	alice.MustSendText(t, server, roomID, "one")
	alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
			"/send/complement.dummy/txn-other",
		Body: map[string]any{"body": "not a message"},
	}).MustStatus(t, http.StatusOK)
	alice.MustSendText(t, server, roomID, "two")

	// A window smaller than the room's timeline.
	small := createFilter(t, server, alice, map[string]any{
		"room": map[string]any{"timeline": map[string]any{"limit": float64(2)}},
	})
	sync := alice.MustSync(t, server, url.Values{"filter": []string{small}, "timeout": []string{"0"}})
	list := sync.Rooms.Join[roomID].Timeline
	if len(list.Events) != 2 {
		t.Fatalf("the filter's limit was not applied: got %d events, want 2", len(list.Events))
	}
	// The window was cut, so the client must be told to page back for the rest.
	if !list.Limited {
		t.Errorf("a trimmed timeline reported limited=false")
	}

	// Types filter *before* the window: two messages exist, so the answer is two messages and
	// not two events of which some survived filtering.
	messages := createFilter(t, server, alice, map[string]any{
		"room": map[string]any{"timeline": map[string]any{
			"limit": float64(10),
			"types": []string{"m.room.message"},
		}},
	})
	sync = alice.MustSync(t, server, url.Values{"filter": []string{messages}, "timeout": []string{"0"}})
	events := harness.DecodeEvents(t, sync.Rooms.Join[roomID].Timeline.Events)
	if len(events) != 2 {
		t.Fatalf("the type filter left %d events, want the 2 messages only", len(events))
	}
	for _, event := range events {
		if event.Type != "m.room.message" {
			t.Errorf("a filtered timeline contained a %s", event.Type)
		}
	}
	if sync.Rooms.Join[roomID].Timeline.Limited {
		t.Errorf("a timeline holding every eligible event reported limited=true, so the client would page for nothing")
	}

	// not_types is the same rule from the other side.
	notMessages := createFilter(t, server, alice, map[string]any{
		"room": map[string]any{"timeline": map[string]any{
			"limit":     float64(10),
			"not_types": []string{"m.room.message"},
		}},
	})
	sync = alice.MustSync(t, server, url.Values{"filter": []string{notMessages}, "timeout": []string{"0"}})
	for _, event := range harness.DecodeEvents(t, sync.Rooms.Join[roomID].Timeline.Events) {
		if event.Type == "m.room.message" {
			t.Errorf("not_types excluded nothing")
		}
	}

	// A wildcard type pattern is the spec's spelling of a family.
	family := createFilter(t, server, alice, map[string]any{
		"room": map[string]any{"timeline": map[string]any{
			"limit": float64(10),
			"types": []string{"m.room.*"},
		}},
	})
	sync = alice.MustSync(t, server, url.Values{"filter": []string{family}, "timeout": []string{"0"}})
	if got := len(harness.DecodeEvents(t, sync.Rooms.Join[roomID].Timeline.Events)); got == 0 {
		t.Errorf("a m.room.* filter matched nothing")
	}

	// An unknown filter ID is refused, not ignored: an ignored filter means the client is
	// served a stream it did not ask for and has no way to notice.
	alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/sync?filter=4242&timeout=0",
	}).MustStatus(t, http.StatusBadRequest)
}

// TestSyncAcceptsInlineFilter covers the other spelling of the filter parameter.
func TestSyncAcceptsInlineFilter(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "inlinefilter", "pw-inlinefilter", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSendText(t, server, roomID, "a")
	alice.MustSendText(t, server, roomID, "b")
	alice.MustSendText(t, server, roomID, "c")

	inline := `{"room":{"timeline":{"limit":1}}}`
	path := "/_matrix/client/v3/sync?filter=" + url.QueryEscape(inline) + "&timeout=0"
	syncResp := alice.MustGet(t, server, path)
	syncResp.MustStatus(t, http.StatusOK)
	var body struct {
		Rooms struct {
			Join map[string]struct {
				Timeline struct {
					Events []json.RawMessage `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(syncResp.Body, &body); err != nil {
		t.Fatalf("sync response is not JSON: %v", err)
	}
	if got := len(body.Rooms.Join[roomID].Timeline.Events); got != 1 {
		t.Errorf("an inline filter gave %d timeline events, want 1", got)
	}

	// Malformed JSON in the parameter is a bad request rather than an unfiltered sync.
	alice.Do(t, server, harness.Request{
		Method: http.MethodGet, Path: "/_matrix/client/v3/sync?filter={&timeout=0",
	}).MustStatus(t, http.StatusBadRequest)
}

// lastServerError returns the server's own error lines, which is how a 500 becomes a cause.
func lastServerError(server *harness.Server) string {
	logs := server.Logs()
	keep := []string{}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=ERROR") || strings.Contains(line, "error=") {
			keep = append(keep, line)
		}
	}
	if len(keep) > 4 {
		keep = keep[len(keep)-4:]
	}
	return strings.Join(keep, "\n")
}
