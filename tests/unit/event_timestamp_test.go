package unit_test

// The store half of `GET /rooms/{roomID}/timestamp_to_event`, tested where the interesting case can be
// built at all.
//
// Through the client API, two events sharing a millisecond is a race with the clock — worth testing, but
// not worth depending on. Here the stamps are written by hand, and the tie is the whole point: complement
// drives a homeserver with an application service that places several events at one exact timestamp, and
// then asks which one `dir=f` and `dir=b` find. A query that sorts by timestamp alone returns whichever
// the index hands back, which is right by luck half the time and wrong in a way nobody can reproduce.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/store"
)

func writeStampedReader(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "timestamps.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// stampEvents writes three messages in one room, the first two sharing a millisecond and the third
// stamped later, and returns their event IDs in insertion order.
func stampEvents(t *testing.T, st *store.Store, roomID string) []string {
	t.Helper()
	specs := []struct {
		eventID string
		ts      int64
	}{
		{"$tie-first", 1_700_000_000_000},
		{"$tie-second", 1_700_000_000_000},
		{"$later", 1_700_000_001_000},
	}
	var ids []string
	err := st.Do(context.Background(), func(ctx context.Context, tx store.Tx) error {
		for _, spec := range specs {
			if _, err := st.Events.Insert(ctx, tx, &store.EventRow{
				EventID:        spec.eventID,
				RoomID:         roomID,
				Type:           "m.room.message",
				Sender:         "@someone:test.local",
				JSON:           "{}",
				RoomVersion:    "11",
				OriginServerTS: spec.ts,
			}); err != nil {
				return err
			}
			ids = append(ids, spec.eventID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("writing the events: %v", err)
	}
	return ids
}

func TestATimestampTieIsBrokenByInsertionOrder(t *testing.T) {
	st := writeStampedReader(t)
	const roomID = "!tie:test.local"
	ids := stampEvents(t, st, roomID)
	earliest := ids[0]

	ctx := context.Background()
	// Forward asks for the earliest event at or after the moment and, among events sharing it, the one
	// that happened first — the beginning of that millisecond.
	forward, err := st.Events.NearestByTimestamp(ctx, roomID, 1_700_000_000_000, true, 0)
	if err != nil {
		t.Fatalf("looking forward from the shared millisecond: %v", err)
	}
	if forward.EventID != earliest {
		t.Errorf("forward from a tie found %q, want %q", forward.EventID, earliest)
	}

	// Backward asks for the latest event at or before the moment and, on the same tie, the one that
	// happened last. Same instant, different answer — which is exactly why the direction is required.
	backward, err := st.Events.NearestByTimestamp(ctx, roomID, 1_700_000_000_000, false, 0)
	if err != nil {
		t.Fatalf("looking backward from the shared millisecond: %v", err)
	}
	if backward.EventID != ids[1] {
		t.Errorf("backward from a tie found %q, want %q", backward.EventID, ids[1])
	}
}

func TestATimestampSearchStopsWhereTheReaderStops(t *testing.T) {
	st := writeStampedReader(t)
	const roomID = "!cutoff:test.local"
	ids := stampEvents(t, st, roomID)

	// A former member's read of a room ends at the position their membership ended at. Asking for the far
	// future with that ceiling must return the last event they may still see, not the room's newest one:
	// an answer beyond the cutoff is a pointer to an event the same reader would then be refused, which
	// is worse than no answer because the client cannot tell the difference.
	ctx := context.Background()
	var lastAllowed int64
	if err := st.Do(ctx, func(ctx context.Context, _ store.Tx) error {
		position, err := st.Events.MaxPositionInRoom(ctx, roomID)
		if err != nil {
			return err
		}
		// Everything but the newest event is readable.
		lastAllowed = position - 1
		return nil
	}); err != nil {
		t.Fatalf("reading the room's positions: %v", err)
	}

	// Asked from the far future, so the only thing that can stop the search short is the ceiling.
	found, err := st.Events.NearestByTimestamp(ctx, roomID, 4_102_444_800_000, false, lastAllowed)
	if err != nil {
		t.Fatalf("looking backward with a ceiling: %v", err)
	}
	if found.EventID == ids[2] {
		t.Errorf("the search returned %q, which sits past the reader's ceiling", found.EventID)
	}
	if found.EventID != ids[1] {
		t.Errorf("the search found %q, want the newest readable event %q", found.EventID, ids[1])
	}
}

func TestATimestampSearchOutsideTheRoomFindsNothing(t *testing.T) {
	st := writeStampedReader(t)
	const roomID = "!elsewhere:test.local"
	stampEvents(t, st, "!somewhere-else:test.local")

	// One room's history is not another's: without the room constraint, a timestamp lookup could name an
	// event the caller has no business reading.
	ctx := context.Background()
	if _, err := st.Events.NearestByTimestamp(ctx, roomID, 1, true, 0); err == nil {
		t.Fatal("an empty room answered a timestamp search")
	} else if !store.IsNotFound(err) {
		t.Fatalf("looking in an empty room: %v", err)
	}
}
