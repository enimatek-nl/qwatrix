package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// sendRelated sends a message that relates to another event and returns its event ID.
func sendRelated(t *testing.T, account *harness.Account, server *harness.Server, roomID, relType, target, body string) string {
	t.Helper()
	resp := account.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/send/m.room.message/txn-" +
			harness.PathEscape(body),
		Body: map[string]any{
			"msgtype":      "m.text",
			"body":         body,
			"m.relates_to": map[string]any{"rel_type": relType, "event_id": target},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sending %q: got %d, want 200\nbody: %s", body, resp.StatusCode, resp.Body)
	}
	var out struct {
		EventID string `json:"event_id"`
	}
	resp.JSON(t, &out)
	if out.EventID == "" {
		t.Fatalf("sending %q returned no event ID", body)
	}
	return out.EventID
}

// relationIDs reads a /relations page and returns the event IDs in the chunk, oldest first.
func relationIDs(t *testing.T, account *harness.Account, server *harness.Server, path, query string) ([]string, string) {
	t.Helper()
	resp := account.MustGet(t, server, path+"?"+query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?%s: got %d, want 200\nbody: %s", path, query, resp.StatusCode, resp.Body)
	}
	var page struct {
		Chunk     []harness.Event `json:"chunk"`
		NextBatch string          `json:"next_batch"`
	}
	resp.JSON(t, &page)
	ids := make([]string, 0, len(page.Chunk))
	for _, event := range page.Chunk {
		ids = append(ids, event.EventID)
	}
	return ids, page.NextBatch
}

// TestRelationsAreGroupedByTheEventTheyPointAt checks that a thread can be read back.
//
// A thread is not a container: it is whatever a pile of events happened to point at. That means the
// only way to recover one is to ask for the relations of the event they name, and the only thing that
// makes the answer right is including every event that pointed, whatever type it was.
func TestRelationsAreGroupedByTheEventTheyPointAt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "threader", "pw-threader", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	root := alice.MustSendText(t, server, roomID, "root")

	reply := sendRelated(t, alice, server, roomID, "m.thread", root, "reply")
	dummy := alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/send/m.dummy/txn-dummy",
		Body: map[string]any{
			"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root},
		},
	})
	dummy.MustStatus(t, http.StatusOK)
	var dummyBody struct {
		EventID string `json:"event_id"`
	}
	dummyBodyJSON := dummy.Body
	_ = json.Unmarshal([]byte(dummyBodyJSON), &dummyBody)
	edit := sendRelated(t, alice, server, roomID, "m.replace", root, "* edited root")

	base := "/_matrix/client/v1/rooms/" + harness.PathEscape(roomID) + "/relations/" +
		harness.PathEscape(root)

	// Every relation, whatever its type.
	got, _ := relationIDs(t, alice, server, base, "limit=100")
	if missing := except([]string{reply, dummyBody.EventID, edit}, got); len(missing) > 0 {
		t.Errorf("GET /relations is missing %v, chunk was %v", missing, got)
	}

	// Filtered by relation type.
	got, _ = relationIDs(t, alice, server, base+"/m.thread", "limit=100")
	if missing := except([]string{reply, dummyBody.EventID}, got); len(missing) > 0 {
		t.Errorf("filtering by m.thread is missing %v, chunk was %v", missing, got)
	}
	if hasEvent(got, edit) {
		t.Errorf("filtering by m.thread included the edit %s", edit)
	}

	// Filtered by relation type and event type: an m.dummy is a member of the thread but is not an
	// m.room.message, and a client that asked for messages does not want it.
	got, _ = relationIDs(t, alice, server, base+"/m.thread/m.room.message", "limit=100")
	if len(got) != 1 || got[0] != reply {
		t.Errorf("filtering by m.thread and m.room.message returned %v, want just %s", got, reply)
	}

	// A wildcard event type means the same thing as leaving it off.
	got, _ = relationIDs(t, alice, server, base+"/m.thread/*", "limit=100")
	if missing := except([]string{reply, dummyBody.EventID}, got); len(missing) > 0 {
		t.Errorf("the wildcard event type is missing %v, chunk was %v", missing, got)
	}

	// An event with no relations is an empty list, not an error: asking what replies to a message
	// that has none has a truthful answer of "nothing".
	lonely := alice.MustSendText(t, server, roomID, "unanswered")
	got, next := relationIDs(t, alice, server, "/_matrix/client/v1/rooms/"+
		harness.PathEscape(roomID)+"/relations/"+harness.PathEscape(lonely), "")
	if len(got) != 0 {
		t.Errorf("an unanswered message has relations: %v", got)
	}
	if next != "" {
		t.Errorf("an empty page offers a continuation token %q, so a client would page forever", next)
	}
}

// TestRelationsPaginationWalksBothDirections checks that a long thread can be walked without losing
// or repeating an event, in either direction.
//
// Five replies are easy to hand back in one page; a thousand are not. The token is the only thing a
// client has to say "and the rest", so a page boundary that drops or repeats an event turns a long
// thread into a thread with holes in it.
func TestRelationsPaginationWalksBothDirections(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "pager", "pw-pager", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	root := alice.MustSendText(t, server, roomID, "root")

	var ids []string
	for i := 0; i < 10; i++ {
		ids = append(ids, sendRelated(t, alice, server, roomID, "m.thread", root,
			fmt.Sprintf("reply-%d", i)))
	}
	base := "/_matrix/client/v1/rooms/" + harness.PathEscape(roomID) + "/relations/" +
		harness.PathEscape(root)

	// Backward: newest first.
	page, next := relationIDs(t, alice, server, base, "limit=3")
	if missing := except([]string{ids[9], ids[8], ids[7]}, page); len(missing) > 0 {
		t.Fatalf("the first backward page is missing %v, got %v", missing, page)
	}
	if next == "" {
		t.Fatal("a backward page that is not the last has no continuation token")
	}
	page, _ = relationIDs(t, alice, server, base, "limit=3&from="+url.QueryEscape(next))
	if missing := except([]string{ids[6], ids[5], ids[4]}, page); len(missing) > 0 {
		t.Errorf("the second backward page is missing %v, got %v", missing, page)
	}
	if anyOf(page, []string{ids[7], ids[8], ids[9]}) {
		t.Errorf("the second backward page repeated an event from the first: %v", page)
	}

	// Forward: oldest first, from the beginning of the room.
	page, next = relationIDs(t, alice, server, base, "limit=3&dir=f")
	if missing := except([]string{ids[0], ids[1], ids[2]}, page); len(missing) > 0 {
		t.Errorf("the first forward page is missing %v, got %v", missing, page)
	}
	page, _ = relationIDs(t, alice, server, base, "limit=3&dir=f&from="+url.QueryEscape(next))
	if missing := except([]string{ids[3], ids[4], ids[5]}, page); len(missing) > 0 {
		t.Errorf("the second forward page is missing %v, got %v", missing, page)
	}

	// A /sync token is a valid `from`: this is how a client picks up replies that arrived while it
	// was asleep, and the token it holds is the one its last sync handed it.
	token := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch
	late := sendRelated(t, alice, server, roomID, "m.thread", root, "reply after the token")
	page, _ = relationIDs(t, alice, server, base, "limit=10&dir=f&from="+url.QueryEscape(token))
	if len(page) != 1 || page[0] != late {
		t.Errorf("paging forward from a sync token returned %v, want just the reply sent after it", page)
	}
}

// except returns the members of wanted that are absent from got.
func except(wanted, got []string) []string {
	present := map[string]bool{}
	for _, id := range got {
		present[id] = true
	}
	var missing []string
	for _, id := range wanted {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

func hasEvent(haystack []string, needle string) bool {
	return len(except([]string{needle}, haystack)) == 0
}

func anyOf(haystack, needles []string) bool {
	return len(except(needles, haystack)) < len(needles)
}
