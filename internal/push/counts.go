package push

// An unread count, and the honesty rule that goes with it.
//
// A gateway is offered the recipient's total of unacknowledged notifications so it can put a number
// on an icon. That is a claim about a person's unread messages, and there are two ways to get it
// wrong: state a number that is not right, which tells the user to stop looking at something that is
// waiting for them; or refuse to state one, which leaves the badge as it was.
//
// So this counts what the server recorded against what the person has read, and when it cannot
// establish the total it reports nothing at all. Silence is not a comfortable answer — a gateway may
// show no badge for somebody with ten things to read — but it is the answer that is true, and the
// alternative is a server that reassures people for no reason.
//
// What is deliberately *not* here is a running total kept current as events arrive. That is what a
// summary table would be for, and it would have to be recomputed on every read receipt, every rule
// edit, and every event this server learns about late. A wrong summary is worse than a query, and a
// count that costs a query is a count that is right.

import (
	"context"

	"github.com/enimatek-nl/qwatrix/internal/store"
)

// unreadCounter answers "how many notifications does this person have left", once per pass.
//
// It remembers per account for the length of the pass: a message in a room with twelve of this
// server's users is twelve deliveries to build, and each would otherwise re-read the receipts the
// first one already read.
type unreadCounter struct {
	store   *store.Store
	reader  Reader
	answers map[string]int64
	// missing records the accounts whose total could not be established, so that a second ask in
	// the same pass does not repeat the query that failed or the scan that hit its bound.
	missing map[string]bool
}

func newUnreadCounter(s *Service) *unreadCounter {
	return &unreadCounter{
		store:   s.store,
		reader:  s.reader,
		answers: map[string]int64{},
		missing: map[string]bool{},
	}
}

// unread reports how many of an account's notifications are unread, and whether that is knowable.
//
// A notification is unread until a receipt covers it: a room's own receipt reads everything up to
// where it is put, in every thread, and a thread's receipt reads only that thread. That is the rule
// the unread badge in /sync uses, and it has to be the same one — a phone whose badge says a room
// has something in it while the app's room list says it is read is a bug nobody can reproduce,
// because each half looks correct on its own.
func (c *unreadCounter) unread(ctx context.Context, localpart string) (int64, bool) {
	if count, ok := c.answers[localpart]; ok {
		return count, true
	}
	if c.missing[localpart] {
		return 0, false
	}
	count, ok := c.measure(ctx, localpart)
	if !ok {
		c.missing[localpart] = true
		return 0, false
	}
	c.answers[localpart] = count
	return count, true
}

// measure does the counting, reporting false when the total could not be established.
func (c *unreadCounter) measure(ctx context.Context, localpart string) (int64, bool) {
	rows, err := c.store.Push.ListRecentNotifications(ctx, localpart, unreadScanLimit)
	if err != nil {
		return 0, false
	}
	if len(rows) >= unreadScanLimit {
		// At least this many notifications exist, so the total is unknown. Withholding the count
		// is the only honest remaining move, and somebody with this much unread is not being
		// helped by a number on an icon either way.
		return 0, false
	}
	if c.reader == nil {
		// Without read positions there is no way to know what has been read. Saying every
		// notification is unread is the mistake in the other direction, and it compounds: the
		// count could never come down.
		return 0, false
	}
	var unread int64
	// Receipts are per room, so they are read once per room rather than once per notification.
	positions := map[string]readPositions{}
	for _, row := range rows {
		where, err := positionsOf(ctx, c.reader, positions, row.RoomID, localpart)
		if err != nil {
			return 0, false
		}
		if row.Position <= where.roomWide {
			continue
		}
		if row.ThreadID != "" {
			if covered, known := where.threads[row.ThreadID]; known && row.Position <= covered {
				continue
			}
		}
		unread++
	}
	return unread, true
}

// readPositions is how far a reader has gone in one room.
type readPositions struct {
	roomWide int64
	threads  map[string]int64
}

// positionsOf asks the reader once per room and keeps the answer for the rest of the pass.
func positionsOf(ctx context.Context, reader Reader, cache map[string]readPositions,
	roomID, localpart string) (readPositions, error) {
	if cached, ok := cache[roomID]; ok {
		return cached, nil
	}
	roomWide, threads, err := reader.ReadPositions(ctx, roomID, localpart)
	if err != nil {
		return readPositions{}, err
	}
	positions := readPositions{roomWide: roomWide, threads: threads}
	cache[roomID] = positions
	return positions, nil
}

// idsOf collects the pending-delivery IDs of a group.
func idsOf(rows []store.PendingPush) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
