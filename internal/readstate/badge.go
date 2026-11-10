package readstate

// The unread badge: what a room is worth to the person who is not in it right now.
//
// A badge is a claim about the *person*, not about a device or a request. Three consequences run
// through this file:
//
//   - It is measured from read receipts, because a receipt is the reader saying how far they got. A
//     sync token measures what one device has been told, and would make the badge shorter every time
//     the person looked — which is backwards.
//   - It counts notifications, not events. A rename, a reaction to a message already read, and the
//     reader's own words are all events and none of them is an errand. What counts is decided by the
//     reader's own push rules, so "mute this room" and "highlight me on mentions" have consequences
//     the person can see and check.
//   - A threaded receipt reads its own thread and nothing else; an unthreaded receipt reads
//     everything up to where it is put, in every thread, because that is the one gesture that means
//     "catch me up".

import (
	"context"
	"log/slog"

	"github.com/enimatek-nl/qwatrix/internal/pushrules"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// badgeScanLimit bounds how far back a badge looks.
//
// The number of unread events a badge can report is capped, not the number it displays: a room with
// a hundred thousand unread events is not a room anybody can catch up on one at a time, and a badge
// that took a second to compute on every sync would make the whole server slow for whoever has the
// biggest room. Clients already render "99+" for a count they cannot show.
const badgeScanLimit = 1000

// BadgeCounts is one badge: events worth a notification, and the subset worth a highlight.
type BadgeCounts struct {
	NotificationCount int64 `json:"notification_count"`
	HighlightCount    int64 `json:"highlight_count"`
}

// BadgeClassifier decides what one event is worth to one person. It is satisfied by
// [pushrules.Service], and injected so that this package does not depend on how a notification is
// decided — only on the answer.
type BadgeClassifier interface {
	Notifies(ctx context.Context, localpart string, ev pushrules.Event) (notifies, highlights bool, err error)
}

// WithBadgeClassifier installs the push-rule service as the arbiter of what counts as unread.
//
// Without a classifier every badge is zero, which is a correct-but-useless answer: the alternative
// would be counting every event, and a badge that says a rename is waiting for you teaches people to
// ignore badges.
func (s *Service) WithBadgeClassifier(classifier BadgeClassifier) *Service {
	s.classifier = classifier
	return s
}

// Badge reports what one room is worth to one reader.
//
// When splitThreads is asked for, the returned counts describe the main timeline only and each
// thread with something unread is reported apart, keyed by its root event. When it is not, the counts
// cover the whole room including its threads, because a client that has never heard of threads still
// has to show a true total.
func (s *Service) Badge(ctx context.Context, localpart, roomID string, splitThreads bool) (*BadgeCounts, map[string]BadgeCounts, error) {
	// Where the reader is. A person cannot have unread anything in a room they never joined, and
	// the events that built a room before they arrived are its history rather than its news.
	membership, err := s.store.Memberships.Get(ctx, roomID, localpart)
	if err != nil {
		if store.IsNotFound(err) {
			return &BadgeCounts{}, nil, nil
		}
		return nil, nil, err
	}
	if membership.Membership != "join" && membership.Membership != "invite" {
		return &BadgeCounts{}, nil, nil
	}

	// How far they have read. An unthreaded receipt covers every scope up to its position; a
	// threaded one covers only its own thread, and `"main"` is the scope a receipt about the
	// room's own timeline carries.
	unthreaded, threaded, err := s.readPositions(ctx, roomID, localpart)
	if err != nil {
		return nil, nil, err
	}

	// The oldest position that could still hold something unread in any scope. Reading from the
	// newest receipt would be wrong — a thread read long ago must not hide an unthreaded message
	// that predates it — so this takes the earliest bound and lets the per-scope comparison below
	// do the excluding.
	from := membership.StreamPos
	if unthreaded > 0 && unthreaded < from {
		from = unthreaded
	}
	for _, position := range threaded {
		if position > 0 && position < from {
			from = position
		}
	}

	rows, err := s.store.Events.Range(ctx, store.EventQuery{
		RoomIDs: []string{roomID},
		After:   from,
		Limit:   badgeScanLimit,
	})
	if err != nil {
		return nil, nil, err
	}

	var total, main BadgeCounts
	threads := map[string]BadgeCounts{}
	for _, row := range rows {
		event, err := pushrules.EventFromRow(&row)
		if err != nil {
			// An event this server cannot read is not worth interrupting anybody for; the
			// alternative is a /sync that fails because of one bad row.
			slog.Warn("readstate: skipping an unread event that could not be decoded",
				"room_id", roomID, "error", err)
			continue
		}
		if !countsTowardBadge(row, event) {
			continue
		}
		if s.classifier == nil {
			continue
		}
		notifies, highlights, err := s.classifier.Notifies(ctx, localpart, event)
		if err != nil {
			return nil, nil, err
		}
		if !notifies {
			continue
		}

		scope := event.ThreadRoot()
		if scope == "" {
			scope = "main"
		}
		if row.ID <= readPositionFor(scope, unthreaded, threaded) {
			// Already read, in the very scope this event belongs to.
			continue
		}

		total.NotificationCount++
		if highlights {
			total.HighlightCount++
		}
		if scope == "main" {
			main.NotificationCount++
			if highlights {
				main.HighlightCount++
			}
			continue
		}
		thread := threads[scope]
		thread.NotificationCount++
		if highlights {
			thread.HighlightCount++
		}
		threads[scope] = thread
	}

	if !splitThreads {
		// The whole-room answer, and no thread section for a client to misread as the total.
		return &total, nil, nil
	}
	if len(threads) == 0 {
		// An empty map would tell a client "there are no threads here" when the truth might be
		// "every thread here is read" — and it cannot tell those apart from a server that does
		// not do threads at all. Only threads with something unread are reported.
		return &main, nil, nil
	}
	return &main, threads, nil
}

// countsTowardBadge filters out the events that are not news, before the more expensive question of
// whether this particular reader wants to hear about them.
//
// Three kinds are excluded, and each exclusion is a judgement about what a person means by
// "unread":
//
//   - state events describe the room rather than saying something to anybody, so a rename, a new
//     avatar, or a power-level change is not waiting to be read;
//   - a redaction takes something away, which is not something to come back to;
//   - an edit is the same message corrected, and counting it would make a person's own typo raise a
//     second badge beside the message it fixes;
//   - an annotation (a reaction) acknowledges a message somebody already saw, whichever of the two
//     event types it arrives as — `m.reaction`, and the `m.room.reaction` form that carries a thread
//     — and a badge full of acknowledgements is how people start ignoring badges.
//
// Everything else is put to the reader's push rules, which is where a notice or a silenced sender
// are handled — by rules the reader can see and change.
func countsTowardBadge(row store.EventRow, event pushrules.Event) bool {
	if row.StateKey != nil {
		return false
	}
	switch row.Type {
	case "m.room.redaction", "m.room.create":
		return false
	}
	switch event.RelType() {
	case "m.replace", "m.annotation":
		return false
	}
	return true
}

// readPositions reads this reader's receipts in one room, split into the unthreaded position and the
// per-thread ones.
func (s *Service) readPositions(ctx context.Context, roomID, localpart string) (int64, map[string]int64, error) {
	receipts, err := s.store.Receipts.ListByRoom(ctx, roomID, 0)
	if err != nil {
		return 0, nil, err
	}
	threaded := map[string]int64{}
	var unthreaded int64
	for _, receipt := range receipts {
		if receipt.Localpart != localpart || receipt.ReceiptType != "m.read" {
			continue
		}
		// The position the reader has read *up to*, which is the position of the event the
		// receipt names — not the receipt's own stream position, which records when the
		// receipt was made so that an incremental /sync can tell which receipts are news.
		// Reading that instead would mean a receipt placed on an old message marking the whole
		// room read, because "just now" is later than everything.
		position, err := s.readPositionOf(ctx, receipt)
		if err != nil {
			return 0, nil, err
		}
		if receipt.ThreadID == "" {
			if position > unthreaded {
				unthreaded = position
			}
			continue
		}
		if position > threaded[receipt.ThreadID] {
			threaded[receipt.ThreadID] = position
		}
	}
	return unthreaded, threaded, nil
}

// readPositionOf resolves a receipt to the stream position it covers.
//
// An event this server no longer has covers nothing: treating a receipt for an unknown event as
// "read up to the end" would let a redaction or a prune silently empty a person's badge.
func (s *Service) readPositionOf(ctx context.Context, receipt store.Receipt) (int64, error) {
	if receipt.EventID == "" {
		return 0, nil
	}
	row, err := s.store.Events.ByEventID(ctx, receipt.EventID)
	if store.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

// readPositionFor answers "how far has this scope been read".
//
// The unthreaded receipt counts, because putting a receipt with no thread is the one gesture that
// means "up to here, everywhere". A threaded receipt counts only its own thread.
func readPositionFor(scope string, unthreaded int64, threaded map[string]int64) int64 {
	position := unthreaded
	if threadedAt, ok := threaded[scope]; ok && threadedAt > position {
		position = threadedAt
	}
	return position
}
