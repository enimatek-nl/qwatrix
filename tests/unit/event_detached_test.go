package unit_test

// Detached events: history this server was told about without being told how it connects.
//
// A server that joins a room receives its current state and none of its past. When one of its users asks
// about that past, the server fetches a window from a resident peer and has to keep it somewhere. Keeping
// it as though it had been received normally would be a lie about three things at once — that this server
// checked the events against the room's rules, that they lead up to wherever the room is now, and that a
// client watching for new activity should be woken by news from last year.
//
// So a fetched event is stored *detached*, and the queries divide by that fact: the ones that page around
// a room's history see it (that is why it was fetched), and the ones that decide what is new, what the
// room's state is, or what a client has not seen yet do not.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/store"
)

func writeDetachedStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "detached.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// writeDetachedPair stores one ordinary event and one fetched event in a room, the fetched one stamped
// earlier so that a query ordering by time alone would put it first.
func writeDetachedPair(t *testing.T, st *store.Store, roomID string) {
	t.Helper()
	err := st.Do(context.Background(), func(ctx context.Context, tx store.Tx) error {
		for _, spec := range []struct {
			eventID  string
			ts       int64
			detached bool
		}{
			{"$local-newest", 1_700_000_002_000, false},
			{"$fetched-older", 1_700_000_001_000, true},
		} {
			if _, err := st.Events.Insert(ctx, tx, &store.EventRow{
				EventID:        spec.eventID,
				RoomID:         roomID,
				Type:           "m.room.message",
				Sender:         "@someone:test.local",
				JSON:           "{}",
				RoomVersion:    "11",
				OriginServerTS: spec.ts,
				Detached:       spec.detached,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("writing the events: %v", err)
	}
}

func eventIDsOf(t *testing.T, rows []store.EventRow) []string {
	t.Helper()
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.EventID)
	}
	return ids
}

func hasEventID(rows []store.EventRow, eventID string) bool {
	for _, row := range rows {
		if row.EventID == eventID {
			return true
		}
	}
	return false
}

func TestAFetchedEventIsInvisibleUntilAQueryAsksForIt(t *testing.T) {
	st := writeDetachedStore(t)
	const roomID = "!detached:test.local"
	writeDetachedPair(t, st, roomID)
	ctx := context.Background()

	// The default reads — the ones /sync, unread badges and every other "what is happening" question use
	// — do not see it. That is the safe direction: a query that forgets to think about detached events
	// tells a client nothing new rather than waking it with history.
	ordinary, err := st.Events.Range(ctx, store.EventQuery{RoomIDs: []string{roomID}})
	if err != nil {
		t.Fatalf("reading the room: %v", err)
	}
	if hasEventID(ordinary, "$fetched-older") {
		t.Errorf("a detached event appeared in an ordinary read: %v", eventIDsOf(t, ordinary))
	}
	if !hasEventID(ordinary, "$local-newest") {
		t.Errorf("the ordinary event went missing: %v", eventIDsOf(t, ordinary))
	}

	// Paging around a room's history does see it, which is the entire reason it was fetched.
	paged, err := st.Events.Range(ctx, store.EventQuery{RoomIDs: []string{roomID}, IncludeDetached: true})
	if err != nil {
		t.Fatalf("reading the room including detached events: %v", err)
	}
	if !hasEventID(paged, "$fetched-older") {
		t.Errorf("paging the room skipped the history that was fetched for it: %v", eventIDsOf(t, paged))
	}
}

func TestATimestampSearchReachesHistoryThatWasFetchedForIt(t *testing.T) {
	st := writeDetachedStore(t)
	const roomID = "!detached:test.local"
	writeDetachedPair(t, st, roomID)
	ctx := context.Background()

	// The moment between the two events. Asking forward must find the fetched one, or the endpoint would
	// answer a question about the room's past while holding the answer and refusing to say so.
	row, err := st.Events.NearestByTimestamp(ctx, roomID, 1_700_000_000_500, true, 0)
	if err != nil {
		t.Fatalf("searching forward: %v", err)
	}
	if row.EventID != "$fetched-older" {
		t.Errorf("the search found %s, want the event that was fetched for this question", row.EventID)
	}
}

func TestADetachedEventDoesNotMoveWhereTheRoomEnds(t *testing.T) {
	st := writeDetachedStore(t)
	const roomID = "!detached:test.local"
	writeDetachedPair(t, st, roomID)
	ctx := context.Background()

	// The room's tip is where its *own* events end. A fetched event is filed after them by arrival, and if
	// it counted as the end, every /messages answer would promise a further page that is never there and
	// every /sync token would start ahead of the room.
	tip, err := st.Events.Tip(ctx, roomID)
	if err != nil {
		t.Fatalf("reading the room's tip: %v", err)
	}
	if tip.EventID != "$local-newest" {
		t.Errorf("the room's tip is %s, want the newest event this server actually has in the DAG", tip.EventID)
	}
}
