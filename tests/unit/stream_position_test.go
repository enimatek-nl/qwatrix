package unit_test

// One clock for everything a client is told about.
//
// `/sync` decides what a client has already been told by comparing one token against the position
// of every change: a room event, a read receipt, a presence change, a to-device message, a client
// writing its own settings. That is only a question with a sane answer if all of those positions
// come from one sequence. Borrowing the tail of the event stream for a receipt or an account-data
// write puts the change at a position the client already holds, and then the change is not merely
// late — it is invisible forever, and a client waiting for the echo of its own write waits forever
// with it.
//
// This is the invariant underneath "Element's encryption reset spins for ever": the client writes
// four account-data keys in a row and does not send the next request until the previous write has
// come back down /sync.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/store"
)

func openStreamStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestEveryAccountDataWriteTakesAFreshStreamPosition is the property a client's single `since`
// token depends on: a write is placed after everything that came before it, including the other
// kinds of write.
func TestEveryAccountDataWriteTakesAFreshStreamPosition(t *testing.T) {
	st := openStreamStore(t)
	ctx := context.Background()

	var previous int64
	for _, dataType := range []string{"com.example.first", "com.example.second", "com.example.third"} {
		content, err := json.Marshal(map[string]any{"written": dataType})
		if err != nil {
			t.Fatalf("encoding the content: %v", err)
		}
		err = st.Do(ctx, func(ctx context.Context, tx store.Tx) error {
			return st.AccountData.Put(ctx, &store.AccountData{
				Localpart: "author",
				Type:      dataType,
				Content:   string(content),
			})
		})
		if err != nil {
			t.Fatalf("writing %q: %v", dataType, err)
		}
		position, err := st.Events.MaxPosition(ctx)
		if err != nil {
			t.Fatalf("reading the end of the stream: %v", err)
		}
		if position <= previous {
			t.Errorf("writing %q left the end of the stream at %d, which is not past the %d a "+
				"client was already told about", dataType, position, previous)
		}
		previous = position
	}
}

// TestEventsAndAccountDataNeverShareAPosition pins the collision that loses a change: if an event
// and a write take the same position, then a client sitting at that token has already seen one of
// the two and never sees the other.
func TestEventsAndAccountDataNeverShareAPosition(t *testing.T) {
	st := openStreamStore(t)
	ctx := context.Background()

	content, err := json.Marshal(map[string]any{"written": "between events"})
	if err != nil {
		t.Fatalf("encoding the content: %v", err)
	}
	err = st.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := st.Events.Insert(ctx, tx, &store.EventRow{
			EventID: "$first", RoomID: "!room:test.local", Type: "m.room.create",
			JSON: `{"type":"m.room.create"}`,
		}); err != nil {
			return err
		}
		return st.AccountData.Put(ctx, &store.AccountData{
			Localpart: "author", Type: "com.example.between", Content: string(content),
		})
	})
	if err != nil {
		t.Fatalf("writing an event and account data: %v", err)
	}

	// An event written afterwards must be ahead of both, not beside one of them.
	var eventPosition int64
	err = st.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := st.Events.Insert(ctx, tx, &store.EventRow{
			EventID: "$second", RoomID: "!room:test.local", Type: "m.room.message",
			JSON: `{"type":"m.room.message"}`,
		})
		if err != nil {
			return err
		}
		eventPosition = row.ID
		return nil
	})
	if err != nil {
		t.Fatalf("writing the second event: %v", err)
	}
	end, err := st.Events.MaxPosition(ctx)
	if err != nil {
		t.Fatalf("reading the end of the stream: %v", err)
	}
	if eventPosition != end {
		t.Errorf("the event written last sits at %d while the end of the stream is %d; whatever sits "+
			"between them was passed by, and a client at %d is never told about it",
			eventPosition, end, eventPosition)
	}
}
