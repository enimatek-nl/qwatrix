package fedapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
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
	// notify wakes the delivery loop the moment an event is written, so a queued event is on its way
	// now rather than at the next tick. See [Outbox.WithWakeup].
	notify *notifier.Notifier
}

// WithWakeup ties the queue to the server's change notifier, so an event queued for a peer is on its
// way now rather than at the next tick.
//
// The tick stays as the fallback: it is what retries a peer that was away, and what runs a pass when
// a queued event's own fuse expires. But a tick is a latency floor, and a floor measured in seconds
// is visible — a client that sends a message and watches a federated room sees the gap, and a peer
// waiting for one event to finish a handshake can give up inside it.
func (o *Outbox) WithWakeup(changes *notifier.Notifier) *Outbox {
	o.notify = changes
	return o
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
	// Invitations travel by their own endpoint, and are handed back co-signed.
	rows, signed := o.coSignInvites(ctx, rows)
	if len(signed) > 0 {
		if err := o.store.FederationOutbox.Delete(ctx, signed); err != nil {
			slog.Error("fedapi: could not clear invitations delivered to a peer", "error", err)
		}
	}
	coSignedCount := len(signed)
	if len(rows) == 0 {
		return coSignedCount, nil
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
	return delivered + coSignedCount, nil
}

// coSignInvites delivers, by the endpoint built for it, every invitation in this pass whose invitee
// lives at the destination it was queued for, and returns the rows that still need the ordinary
// transaction path.
//
// An invitation cannot be relayed like a message. The specification asks the invited user's own server
// to sign it — see [events.Service.CoSignEvent] for why — so the copy this server queued at the moment
// the invite was written is, by construction, a copy nobody will accept. Sending it anyway produces a
// peer that answers 200 and refuses the event inside, which is the worst possible combination: the
// obligation is cleared and the invitation is gone.
//
// Once the peer has signed, that signature belongs to the event everywhere. The stored event is
// rewritten, and so are the copies still waiting for other servers, because they are queued as JSON
// rather than re-read at delivery time, and a stale copy would be refused by the very next peer.
func (o *Outbox) coSignInvites(ctx context.Context, rows []store.FederationOutbox) (
	remaining []store.FederationOutbox, delivered []int64,
) {

	remaining = make([]store.FederationOutbox, 0, len(rows))
	coSignedByEvent := map[string]string{}
	for _, row := range rows {
		if !events.IsInvite([]byte(row.JSON)) || !invitesUserAt(row, o.serverName) {
			remaining = append(remaining, row)
			continue
		}
		coSigned, err := o.requestInviteSignature(ctx, row)
		if err != nil {
			// The peer may be unreachable, may not implement this endpoint at all, or may simply
			// refuse to sign. None of those is a reason to stop inviting: the invitation is kept
			// queued and goes out by the ordinary path as well, unsigned, which is what a peer
			// without this endpoint expects and what every homeserver did before the second
			// signature was asked for. A peer that *does* verify will refuse the unsigned copy, and
			// that refusal is now reported per event rather than swallowed, so the invitation stays
			// in the queue with the reason beside it instead of vanishing.
			remaining = append(remaining, row)
			slog.Warn("fedapi: the invitee's server did not co-sign an invitation; sending it unsigned",
				"room_id", row.RoomID, "event_id", row.EventID, "destination", row.Destination,
				"error", err)
			continue
		}
		coSignedByEvent[row.EventID] = coSigned
		delivered = append(delivered, row.ID)
	}
	for eventID, coSigned := range coSignedByEvent {
		if err := o.store.Events.ReplaceJSON(ctx, eventID, coSigned); err != nil {
			slog.Warn("fedapi: could not store the co-signed copy of an invitation",
				"event_id", eventID, "error", err)
		}
		if _, err := o.store.FederationOutbox.RewriteJSON(ctx, eventID, coSigned); err != nil {
			slog.Warn("fedapi: could not rewrite the queued copies of a co-signed invitation",
				"event_id", eventID, "error", err)
		}
		// The rows in this pass were read before the rewrite, so they still carry the unsigned bytes.
		for i, row := range remaining {
			if row.EventID == eventID {
				remaining[i].JSON = coSigned
			}
		}
	}
	return remaining, delivered
}

// invitesUserAt reports whether a queued event invites one of the given destination's users — the case
// where that server is the one whose signature is required. An invitation relayed on behalf of another
// server is already co-signed by the time it reaches this point, and goes by the ordinary path.
func invitesUserAt(row store.FederationOutbox, origin spec.ServerName) bool {
	if gjson.Get(row.JSON, "type").String() != "m.room.member" {
		return false
	}
	stateKey := gjson.Get(row.JSON, "state_key").String()
	user, err := spec.NewUserID(stateKey, true)
	if err != nil {
		return false
	}
	return string(user.Domain()) == row.Destination && row.Destination != string(origin)
}

// requestInviteSignature asks the invitee's server for its signature and returns the event with both.
func (o *Outbox) requestInviteSignature(ctx context.Context, row store.FederationOutbox) (string, error) {
	stored, err := o.store.Events.ByEventID(ctx, row.EventID)
	if err != nil {
		return "", fmt.Errorf("finding the invitation to co-sign: %w", err)
	}
	version, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(stored.RoomVersion))
	if err != nil {
		return "", fmt.Errorf("the invitation's room version %q: %w", stored.RoomVersion, err)
	}
	event, err := version.NewEventFromTrustedJSON([]byte(row.JSON), false)
	if err != nil {
		return "", fmt.Errorf("reading the invitation to co-sign: %w", err)
	}
	request, err := fclient.NewInviteV2Request(event, nil)
	if err != nil {
		return "", err
	}
	answer, err := o.peer.Signed().SendInviteV2(ctx, o.serverName, spec.ServerName(row.Destination), request)
	if err != nil {
		return "", fmt.Errorf("asking %s to co-sign an invitation: %w", row.Destination, err)
	}
	signed := string(answer.Event)
	if !gjson.Get(signed, "signatures").Get(gjson.Escape(row.Destination)).Exists() {
		// The signature is the whole point of the call. An answer without it would be relayed and
		// refused downstream, which is the failure this path exists to prevent.
		return "", fmt.Errorf("%s returned an invitation it had not signed", row.Destination)
	}
	return signed, nil
}

// Run drains the ledger until the context ends.
//
// It runs when something was just written and otherwise once a tick, which is the difference between
// a federation that looks instantaneous and one that looks like a two-second conversation.
func (o *Outbox) Run(ctx context.Context) error {
	ticker := time.NewTicker(outboxTick)
	defer ticker.Stop()
	var generation uint64
	if o.notify != nil {
		generation = o.notify.Generation()
	}
	for {
		if !o.wait(ctx, ticker, &generation) {
			return nil
		}
		// One pass at a time, however much is queued: catching up a peer that was away for a day is
		// not something to do at full speed on somebody else's bandwidth.
		if _, err := o.DrainOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			slog.Warn("fedapi: the federation delivery pass failed", "error", err)
		}
	}
}

// wait parks the loop until there is a reason to run a pass, and reports whether the loop should
// continue. A timeout is a reason too: it is how a peer that was away gets retried.
func (o *Outbox) wait(ctx context.Context, ticker *time.Ticker, generation *uint64) bool {
	if o.notify == nil {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			return true
		}
	}
	if o.notify.WaitForChange(ctx, *generation, outboxTick) {
		*generation = o.notify.Generation()
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	// The wait timed out. A tick already waiting is the same reason, so do not run twice for it.
	select {
	case <-ticker.C:
	default:
	}
	return true
}

// outboxBatch is the set of queued events for one peer.
type outboxBatch struct {
	destination string
	rows        []store.FederationOutbox
}

// SplitDelivery reads a peer's answer to a transaction and says which queued copies it may clear.
//
// The status line carries no information here: a peer that refused every event inside still answers
// 200, because the transaction itself was well-formed and each event's fate is reported per event. A
// sender that clears its queue on the status has therefore deleted events the peer turned away — no
// retry, no log, and a room on the far side quietly missing whatever it was. An event the peer does not
// mention at all is treated the same way as one it refused: silence is not delivery.
func SplitDelivery(rows []store.FederationOutbox, answer fclient.RespSend) (
	delivered, refused []int64, reason string,
) {
	byEvent := make(map[string]fclient.PDUResult, len(answer.PDUs))
	for eventID, result := range answer.PDUs {
		byEvent[eventID] = result
	}
	for _, row := range rows {
		result, mentioned := byEvent[row.EventID]
		if mentioned && result.Error == "" {
			delivered = append(delivered, row.ID)
			continue
		}
		refused = append(refused, row.ID)
		if reason == "" {
			if !mentioned {
				reason = fmt.Sprintf("%s answered for %s", row.Destination, row.EventID)
				continue
			}
			if why := withoutPeerPrefix(result.Error); why != "" {
				reason = fmt.Sprintf("%s refused %s: %s", row.Destination, row.EventID, why)
			}
		}
	}
	return delivered, refused, reason
}

// withoutPeerPrefix trims the string gomatrixserverlib puts in front of a peer's own refusal, which is
// otherwise a wall of JSON in the middle of a log line.
func withoutPeerPrefix(message string) string {
	if i := strings.Index(message, "}"); i >= 0 && i+2 < len(message) {
		return strings.TrimSpace(message[i+2:])
	}
	return strings.TrimSpace(message)
}

// SendDeviceListUpdate delivers one device-list update to one peer as a transaction that carries no
// events at all.
//
// Device lists travel this way because only a user's own server may speak for that user's devices, and
// the peer cannot ask: it has to be told, at the moment the change happens, or its clients keep
// encrypting to keys that have moved.
//
// Two things this does not do, both recorded rather than quietly skipped. It is not queued — an update
// that cannot be sent now is superseded by the next one, and a device list is a statement about *now*.
// And it is not signed by the device it describes, which is what the specification asks for and what a
// verifying peer checks; doing that means the device's own key, not this server's signing key.
func (o *Outbox) SendDeviceListUpdate(ctx context.Context, destination string, content json.RawMessage) error {
	if o == nil || o.peer == nil || destination == "" || destination == string(o.serverName) {
		return nil
	}
	transaction := gomatrixserverlib.Transaction{
		TransactionID:  gomatrixserverlib.TransactionID(o.nextTransactionID()),
		Origin:         o.serverName,
		Destination:    spec.ServerName(destination),
		OriginServerTS: spec.Timestamp(time.Now().UnixMilli()),
		EDUs: []gomatrixserverlib.EDU{{
			Type:        deviceListUpdateEDU,
			Origin:      string(o.serverName),
			Destination: destination,
			Content:     spec.RawJSON(content),
		}},
	}
	if _, err := o.peer.Signed().SendTransaction(ctx, transaction); err != nil {
		return fmt.Errorf("sending a device-list update to %s: %w", destination, err)
	}
	return nil
}

// deviceListUpdateEDU is the ephemeral event type that carries a device-list change.
const deviceListUpdateEDU = "m.device_list_update"

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
	sent := make([]store.FederationOutbox, 0, outboxEventsPerTransaction)
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
		sent = append(sent, row)
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
	answer, err := o.peer.Signed().SendTransaction(ctx, transaction)
	if err != nil {
		delay := outboxBackoff(batch.rows[0].Attempts)
		next := time.Now().Add(delay).UnixMilli()
		if deferErr := o.store.FederationOutbox.Deferred(ctx, ids, next, err.Error()); deferErr != nil {
			slog.Error("fedapi: could not defer a failed federation delivery",
				"destination", batch.destination, "error", deferErr)
		}
		return 0, fmt.Errorf("sending a transaction to %s: %w", batch.destination, err)
	}
	// The peer answered, which is not the same as the peer taking the events: refusals live in the
	// body of a 200, so the queue is cleared per event and no further.
	accepted, rejected, why := SplitDelivery(sent, answer)
	if len(rejected) > 0 {
		delay := outboxBackoff(0)
		if deferErr := o.store.FederationOutbox.Deferred(ctx, rejected,
			time.Now().Add(delay).UnixMilli(), why); deferErr != nil {
			slog.Error("fedapi: could not defer events a peer refused", "error", deferErr)
		}
		// This is the operator's only notice that a room is missing an event. Before it, the
		// transaction was logged as delivered and the refusal was discarded with the queue row.
		slog.Warn("fedapi: a peer refused events inside a transaction it accepted",
			"destination", batch.destination, "events", len(rejected), "of", len(sent),
			"reason", why)
	}
	if len(accepted) == 0 {
		return 0, fmt.Errorf("%s refused every event in the transaction: %s", batch.destination, why)
	}
	ids = accepted
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
