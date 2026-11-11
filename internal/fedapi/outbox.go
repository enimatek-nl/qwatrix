package fedapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// How hard to try to deliver, and for how long.
const (
	// outboxPassesPerTick bounds how many peers one pass speaks to. A server sharing rooms with a
	// hundred peers should not build a burst of a hundred simultaneous connections every tick, and the
	// ones it misses will be reached on the next.
	outboxPeersPerPass = 8
	// outboxEventsPerTransaction bounds one transaction's body. Peers may reject an oversized
	// request, and a peer that has been asleep for an hour should be caught up in pages rather than in
	// one request that gets refused for being large.
	outboxEventsPerTransaction = 50
	// outboxFirstBackoff is the delay before a failed delivery is retried; it doubles per attempt.
	outboxFirstBackoff = 2 * time.Second
	// outboxMaxBackoff caps the doubling, so a peer that is offline for a day is asked about twice a
	// minute rather than about a year from now.
	outboxMaxBackoff = 10 * time.Minute
	// outboxGiveUpAfterBounds the retries. Past this the peer has said, many times, that it does not
	// want this; keeping the queue forever would be a slow denial of service on ourselves.
	outboxGiveUpAfter = 200
	// outboxDiscardAfter is how long this server will keep trying at all.
	outboxDiscardAfter = 7 * 24 * time.Hour
	// outboxTick is how often the ledger is consulted when there is nothing to do.
	outboxTick = 2 * time.Second
)

// Outbox delivers the peer copies this server owes: the events the room server wrote into the delivery
// ledger, in transactions, until each peer has confirmed them.
//
// It is a queue and not an inline send for two reasons that both show up in normal use. A peer that is
// briefly unreachable must not make a local user's `PUT /send/event` fail — their message was accepted,
// and it is true — and a room whose members live on forty servers must not make the sender wait for
// forty round trips, one of which will be slow.
type Outbox struct {
	peer       *Peer
	store      *store.Store
	serverName spec.ServerName
	// sequence numbers the transactions, which a peer uses to answer "did you already send me that?".
	// It only has to be unique per destination, and monotone is enough.
	sequence atomic.Int64
}

// NewOutbox builds the delivery worker for one server's own ledger.
func NewOutbox(peer *Peer, st *store.Store, serverName spec.ServerName) *Outbox {
	return &Outbox{peer: peer, store: st, serverName: serverName}
}

// DrainOnce performs one delivery pass and returns how many events reached a peer.
//
// Exported because a test that needs the delivery to have happened cannot be served by a goroutine it
// has no influence over: the production loop calls this on a tick, and a test calls it when it wants the
// answer now.
func (o *Outbox) DrainOnce(ctx context.Context) (int, error) {
	if o == nil || o.peer == nil || o.store == nil {
		return 0, nil
	}
	if err := o.discardAbandoned(ctx); err != nil {
		slog.Warn("fedapi: could not discard abandoned federation deliveries", "error", err)
	}

	rows, err := o.store.FederationOutbox.Due(ctx, time.Now().UnixMilli(), outboxPeersPerPass)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	delivered := 0
	for _, batch := range groupByDestination(rows) {
		sent, err := o.deliver(ctx, batch)
		if err != nil {
			// The batch stays queued with a longer fuse. Note that a peer that answered at all is
			// distinct from one that could not be reached: both are a failure to deliver here, and the
			// difference is in the log because "the peer is refusing us" and "there is no peer" call for
			// different fixes.
			slog.Warn("fedapi: could not deliver events to a peer",
				"destination", batch.destination, "events", len(batch.rows), "error", err)
			continue
		}
		delivered += sent
	}
	return delivered, nil
}

// Run drains the ledger until the context ends.
func (o *Outbox) Run(ctx context.Context) error {
	ticker := time.NewTicker(outboxTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// One pass per tick, however much is queued: catching up a peer that was away for a day is
			// not something to do at full speed on somebody else's bandwidth.
			if _, err := o.DrainOnce(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				slog.Warn("fedapi: the federation delivery pass failed", "error", err)
			}
		}
	}
}

// outboxBatch is the set of queued events for one peer.
type outboxBatch struct {
	destination string
	rows        []store.FederationOutbox
}

// groupByDestination splits the queue into one batch per peer, in the order the events were queued.
func groupByDestination(rows []store.FederationOutbox) []outboxBatch {
	index := map[string]int{}
	out := []outboxBatch{}
	for _, row := range rows {
		at, ok := index[row.Destination]
		if !ok {
			at = len(out)
			index[row.Destination] = at
			out = append(out, outboxBatch{destination: row.Destination})
		}
		out[at].rows = append(out[at].rows, row)
	}
	return out
}

// deliver sends one peer everything this pass holds for it.
func (o *Outbox) deliver(ctx context.Context, batch outboxBatch) (int, error) {
	ids := make([]int64, 0, len(batch.rows))
	pdus := make([]json.RawMessage, 0, outboxEventsPerTransaction)
	for i, row := range batch.rows {
		if i >= outboxEventsPerTransaction {
			// Deliberately not sent: the rest waits for the next pass, which keeps this transaction's
			// size bounded rather than growing with how long the peer has been asleep.
			break
		}
		if !json.Valid([]byte(row.JSON)) {
			// A queued event that cannot be parsed would be refused by the peer forever, and every
			// retry of the batch would fail because of it. Dropping that one row is what keeps the
			// room moving.
			slog.Error("fedapi: a queued federation delivery is not valid JSON and is being dropped",
				"destination", batch.destination, "event_id", row.EventID, "room_id", row.RoomID)
			_ = o.store.FederationOutbox.Delete(ctx, []int64{row.ID})
			continue
		}
		ids = append(ids, row.ID)
		pdus = append(pdus, json.RawMessage(row.JSON))
	}
	if len(pdus) == 0 {
		return 0, nil
	}

	transaction := gomatrixserverlib.Transaction{
		TransactionID:  gomatrixserverlib.TransactionID(o.nextTransactionID()),
		Origin:         o.serverName,
		Destination:    spec.ServerName(batch.destination),
		OriginServerTS: spec.Timestamp(time.Now().UnixMilli()),
		PDUs:           pdus,
	}
	if _, err := o.peer.Signed().SendTransaction(ctx, transaction); err != nil {
		delay := outboxBackoff(batch.rows[0].Attempts)
		next := time.Now().Add(delay).UnixMilli()
		if deferErr := o.store.FederationOutbox.Deferred(ctx, ids, next, err.Error()); deferErr != nil {
			slog.Error("fedapi: could not defer a failed federation delivery",
				"destination", batch.destination, "error", deferErr)
		}
		return 0, fmt.Errorf("sending a transaction to %s: %w", batch.destination, err)
	}
	if err := o.store.FederationOutbox.Delete(ctx, ids); err != nil {
		// The peer has the events. Leaving the rows queued means it is asked again, which is legal —
		// transactions are idempotent by their ID and events by their event ID — and is the safer way
		// to be wrong: a duplicate delivered is a no-op, an event never delivered is a missing message.
		return 0, fmt.Errorf("clearing delivered events for %s: %w", batch.destination, err)
	}
	slog.Info("fedapi: delivered events to a peer",
		"destination", batch.destination, "events", len(pdus))
	return len(pdus), nil
}

// discardAbandoned forgets deliveries this server will not attempt again, and says so loudly: an
// operator has to be able to find out that a message was given up on.
func (o *Outbox) discardAbandoned(ctx context.Context) error {
	dropped, err := o.store.FederationOutbox.DropOld(ctx,
		time.Now().Add(-outboxDiscardAfter).UnixMilli(), outboxGiveUpAfter)
	if err != nil {
		return err
	}
	if dropped > 0 {
		slog.Error("fedapi: gave up on federation deliveries that a peer never accepted",
			"events", dropped, "give_up_after_attempts", outboxGiveUpAfter,
			"age", outboxDiscardAfter.String())
	}
	return nil
}

// outboxBackoff doubles the delay with each failed attempt and caps it.
func outboxBackoff(attempts int) time.Duration {
	delay := outboxFirstBackoff
	for attempt := 0; attempt < attempts && delay < outboxMaxBackoff; attempt++ {
		delay *= 2
	}
	if delay > outboxMaxBackoff {
		return outboxMaxBackoff
	}
	return delay
}

// nextTransactionID builds an identifier unique to this process run. A peer keeps the most recent ones
// to answer "already done", so reuse would turn a delivery into a no-op that this server then forgets
// about while the peer never stored it.
func (o *Outbox) nextTransactionID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixMilli(), o.sequence.Add(1))
}
