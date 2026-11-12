package fedapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"

	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/matrix-org/gomatrixserverlib"
	fclient "github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// Gap filling bounds. A peer can always claim to be missing history, and can therefore make this
// server fetch history forever on its own bandwidth for events nobody asked about. The ceiling is what
// turns "the peer is telling the truth" into an assumption that costs something bounded when it is not.
const (
	// gapFillRequests is how many requests to one peer one refused event may cause.
	//
	// The bound is on the work, not on the number of tries, and the difference matters. A peer asked for
	// a gap may answer with a window of it — complement's own federation mock answers with the last ten
	// events regardless of how deep the gap is, which is a defensible thing for a homeserver to do too.
	// Each further event then costs one more request, so a ceiling of a few tries refuses a gap of twelve
	// events that could be bridged honestly, and two servers that both hold all of a room end up
	// permanently split over it. Forty capped requests are a bounded amount of bandwidth and latency
	// spent on one event a peer chose to send. A hundred is the point at which the walk stops being a
	// way to keep up with a busy room and starts being a way to spend an afternoon on one peer's word:
	// it is also how many events the largest legitimate gap-filling answer can hold, so the ceiling says
	// "one room-sized surprise", not "one request".
	//
	// A gap deeper than this is not answered by a bigger number. It is answered by the room's state
	// arriving whole — a backfill, or the outlier handling that stores an event with the state its peer
	// vouches for — and a server that walked a thousand events one by one would have spent a thousand
	// round trips saying no.
	gapFillRequests = 100
	// gapFillTransactionRequests is the ceiling for a whole transaction rather than one event in it.
	//
	// A per-event bound is not enough on its own, because a transaction carries many events: a peer can
	// put fifty events that each need a walk into one request, and a peer that grows impatient and sends
	// the same transaction again has handed itself an amplifier and taken one out of this server with it.
	// Two hundred is the one-event ceiling plus a second walk's worth, which is what a transaction with
	// more than one gap in it realistically means.
	gapFillTransactionRequests = 200
	// gapFillEvents is how many events one request may bring back. The specification caps the
	// endpoint at 100; asking for the cap means a peer holding a long run of history can bridge a
	// long gap in one round trip instead of one event per request.
	gapFillEvents = 100
)

// admitWithGapFill admits an event, fetching a bounded amount of the history it depends on first.
//
// Refusing an event because its predecessors are unknown is correct and, left alone, terminal: the peer
// has already sent what it has, and the two servers then sit at different places in the same room with
// the local user seeing less of it every time. The peer that reported the event is also the server most
// likely to hold the missing piece, so the gap is offered back to it — a bounded number of times.
//
// Two endpoints can close a gap and they suit different peers, so both are tried:
//
// `POST /get_missing_events/{roomID}` returns the events between a point this server knows and the
// event the peer just sent. It is tried first because a gap is usually a run of events — a peer that
// reconnects after a while away, a room that moved while this server was not watching — and one request
// can take the whole run. It also needs something this server holds to start from, which is why
// roomserver.MissingPredecessorError carries the predecessors it does have.
//
// `GET /event/{eventID}` asks for one event. Slower per event, and the fallback for a peer that serves
// only that — which is also the only question that makes progress when the bulk endpoint answers with a
// window of the gap instead of all of it.
//
// The loop is a loop and not recursion because a peer can describe a cycle: A follows B, B follows A.
// Each step fetches what the room server just said it needs and retries the event the peer actually
// sent, so progress is visible in the log and the request budget is a stop rather than a suggestion.
func (h *Handler) admitWithGapFill(ctx context.Context, budget *int, origin, roomID string,
	event gomatrixserverlib.PDU) error {
	walker := &gapWalker{handler: h, ctx: ctx, origin: origin, roomID: roomID, latest: event.EventID(),
		shared:  budget,
		fetched: map[string]bool{event.EventID(): true}, attempted: map[string]bool{},
		held: map[string]gomatrixserverlib.PDU{}}

	for {
		err := h.opts.Join.AcceptFederationEvent(ctx, roomID, event)
		if err == nil {
			return nil
		}
		var gap *roomserver.MissingPredecessorError
		if !errors.As(err, &gap) {
			return err
		}
		if !walker.placeGap(gap) {
			// The walk ran out of road — the peer could not supply an event, or the events it supplies
			// do not reach back to anything this server holds. The refusal the room server gave is the
			// true answer, and it names what is still missing.
			if walker.outOfBudget() {
				slog.Warn("fedapi: gave up filling a gap in a federated room",
					"room_id", roomID, "missing_event_id", gap.EventID, "requests", walker.requests)
			}
			return err
		}
	}
}

// gapWalker is the state of one gap-filling walk: how much of the peer's bandwidth has been spent, what
// has been asked for, what has been tried, and what arrived but cannot be placed yet.
//
// The last of those is the reason this is a type and not a loop. An event fetched from a peer is not
// storable just because it arrived: it follows an event this server also does not have. Storing it would
// be the detached fragment this package exists to prevent, and refusing it loses a piece of a gap the
// server is halfway across. So a fetched event is *held*, the walk moves to the event before it, and the
// held ones get their turn once the ground under them appears.
//
// `fetched` and `attempted` are what make the walk terminate. A peer can describe a cycle — A follows B,
// B follows A — and a walker with no memory of where it has been follows it forever.
type gapWalker struct {
	handler *Handler
	ctx     context.Context
	origin  string
	roomID  string
	// latest is the event the peer actually sent: the far end of the gap, and the point the bulk
	// endpoint is asked to fill up to.
	latest   string
	requests int
	// shared is the transaction's counter, or nil when this walk answers only for itself. Every request
	// the walk makes is charged to it as well as to the walk's own count.
	shared *int
	// bulkAsked records that the bulk endpoint has been given its one chance. Its answer is a function
	// of the two points named in the request, and those do not move while a gap is being filled: the
	// anchors are the predecessors this server holds and the room's tip, and neither changes by storing
	// events in the middle. Asking twice buys a round trip to be handed a window already declined.
	bulkAsked bool
	fetched   map[string]bool
	attempted map[string]bool
	held      map[string]gomatrixserverlib.PDU
}

// canSpend reports whether the walk may make one more request to the peer. Both ceilings are the walk's
// concern: the event's bound limits what one peer's word can cost this server, and the transaction's
// limits what a transaction full of such words can cost.
func (w *gapWalker) canSpend() bool {
	return w.requests < gapFillRequests &&
		(w.shared == nil || *w.shared < gapFillTransactionRequests)
}

// spend charges the walk for one request made.
func (w *gapWalker) spend() {
	w.requests++
	if w.shared != nil {
		*w.shared++
	}
}

func (w *gapWalker) outOfBudget() bool { return !w.canSpend() }

// placeGap puts one named missing event where it belongs, walking further back into the gap as needed.
// It reports whether anything was stored, which is the only signal the caller has that the walk is
// approaching the room rather than treading water.
func (w *gapWalker) placeGap(gap *roomserver.MissingPredecessorError) bool {
	for {
		if !w.canSpend() {
			return false
		}
		if !w.bulkAsked {
			w.bulkAsked = true
			if len(gap.Anchors) > 0 {
				w.spend()
				w.handler.fetchMissingEvents(w.ctx, w.origin, w.roomID, w.latest, gap, w.held)
				return true
			}
		}
		if !w.fetched[gap.EventID] {
			w.fetched[gap.EventID] = true
			w.spend()
			for _, candidate := range w.handler.requestEvent(w.ctx, w.origin, w.roomID, gap.EventID) {
				w.held[candidate.EventID()] = candidate
			}
			if _, ok := w.held[gap.EventID]; !ok {
				// The peer was asked and did not produce the event. Every other question it can answer
				// has been answered; this one is why the gap is a gap.
				return false
			}
		}
		candidate, ok := w.held[gap.EventID]
		if !ok || w.attempted[gap.EventID] {
			// Either nothing ever arrived under that name, or placing it was already tried and the event
			// before it has still not appeared. Both are the end of this walk.
			return false
		}
		w.attempted[gap.EventID] = true
		err := w.handler.opts.Join.AcceptFederationEvent(w.ctx, w.roomID, candidate)
		if err == nil {
			delete(w.held, candidate.EventID())
			// Storing this event changes what every other held event was waiting for, so the record of
			// which ones could not be placed is dropped rather than kept: it describes a room that no
			// longer looks like this one. Cleared only on a store, which is what keeps a cycle of held
			// events — A follows B, B follows A — from restarting the walk forever.
			w.attempted = map[string]bool{}
			return true
		}
		var deeper *roomserver.MissingPredecessorError
		if !errors.As(err, &deeper) {
			slog.Debug("fedapi: refused a fetched gap-fill event",
				"room_id", w.roomID, "event_id", gap.EventID, "error", err)
			return false
		}
		// The event arrived and still cannot be placed: the ground under it is missing too. Walk on.
		gap = deeper
	}
}

// fetchMissingEvents asks the peer for the events between a point this server holds and the event it
// just sent, and admits everything it gives back.
//
// It reports whether anything was admitted. A refusal to admit one event of a batch is not a refusal of
// the batch: a peer returning fifty events this server cannot authenticate should still have its honest
// events stored, and the room server decides event by event exactly as it does for a transaction.
func (h *Handler) fetchMissingEvents(ctx context.Context, origin, roomID, latest string,
	gap *roomserver.MissingPredecessorError, keep map[string]gomatrixserverlib.PDU) bool {
	if h.opts.Peer == nil || len(gap.Anchors) == 0 {
		// No anchor to ask from. "Everything this room has, please" is not a request to make of a peer
		// and not one this server would survive answering.
		return false
	}
	version, err := h.opts.Join.RoomVersionFor(ctx, roomID)
	if err != nil {
		return false
	}
	response, err := h.opts.Peer.Signed().LookupMissingEvents(ctx, h.opts.ServerName, spec.ServerName(origin),
		roomID, fclient.MissingEvents{
			Limit:          gapFillEvents,
			EarliestEvents: gap.Anchors,
			LatestEvents:   []string{latest},
		}, version.Version())
	if err != nil {
		slog.Debug("fedapi: a peer could not fill a gap in a federated room",
			"origin", origin, "room_id", roomID, "error", err)
		return false
	}

	// Verify everything before storing anything, in dependency order. A batch of missing events is
	// normally a chain, and an event is only admissible once the one before it is here — so shallowest
	// first, then a second pass over whatever that left for a fork to settle. Without the ordering a
	// fifty-event gap costs fifty round trips to the peer.
	pending := make([]gomatrixserverlib.PDU, 0, len(response.Events))
	for _, raw := range response.Events {
		candidate, reason := h.parseRelayedEvent(ctx, json.RawMessage(raw))
		if reason != "" {
			slog.Debug("fedapi: refused a gap-fill event", "room_id", roomID, "reason", reason)
			continue
		}
		// A gap in one room is not an invitation to write into another.
		if fetched := candidate.RoomID().String(); fetched != roomID {
			slog.Warn("fedapi: a peer supplied an event from another room while filling a gap",
				"origin", origin, "room_id", roomID, "supplied_room_id", fetched)
			continue
		}
		pending = append(pending, candidate)
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].Depth() < pending[j].Depth() })

	admitted := false
	for pass := 0; pass < 2 && len(pending) > 0; pass++ {
		remaining := make([]gomatrixserverlib.PDU, 0, len(pending))
		for _, candidate := range pending {
			if err := h.opts.Join.AcceptFederationEvent(ctx, roomID, candidate); err == nil {
				admitted = true
				continue
			} else {
				slog.Debug("fedapi: could not store a gap-fill event",
					"event_id", candidate.EventID(), "error", err)
			}
			// The window arrived and cannot be placed yet because the event before the oldest of them is
			// still missing. Keeping it costs a little memory and saves the walk a request per event it
			// would otherwise have to ask for again one at a time.
			if keep != nil {
				keep[candidate.EventID()] = candidate
			}
			remaining = append(remaining, candidate)
		}
		pending = remaining
	}
	return admitted
}

// fetchEvent asks the peer for one event it has not sent, and admits it.
//
// The event is verified exactly as an event delivered in a transaction is — same room version, same
// signature check against the sender's key — because a `/event` response is a body a peer chose to send,
// not evidence. It must also belong to the room whose gap is being filled, and it is refused otherwise:
// without that restriction gap filling is a way to write into rooms this server would not otherwise
// have joined.
//
// It reports whether anything was admitted, so the caller retries on true and refuses on false. A peer
// that answers with garbage then costs a refusal rather than a hang.
// requestEvent asks the peer for one event it has not sent and returns what it gave back, verified and
// stored nowhere.
//
// Not storing is the point. A fetched event usually follows another fetched event, and the walk has to
// be allowed to keep going back before any of them can be placed; a caller that stores as it goes either
// refuses honest events for the wrong reason or stores ones that do not attach to anything.
//
// Each event is verified exactly as one delivered in a transaction is — same room version, same signature
// check against the sender's key — because a `/event` response is a body a peer chose to send, not
// evidence. It must also belong to the room whose gap is being filled: without that restriction, filling
// a gap in one room is a way to write into another.
func (h *Handler) requestEvent(ctx context.Context, origin, roomID, eventID string) []gomatrixserverlib.PDU {
	if h.opts.Peer == nil {
		return nil
	}
	// The library names the *requesting* server "origin" and the peer second, which is the opposite of
	// how this file reads from the peer's side everywhere else. Swapping them produces
	// "no signing identity for server name <peer>": the client was asked to sign as somebody it is not.
	transaction, err := h.opts.Peer.Signed().GetEvent(ctx, h.opts.ServerName, spec.ServerName(origin), eventID)
	if err != nil {
		slog.Debug("fedapi: a peer could not supply a missing event",
			"origin", origin, "event_id", eventID, "error", err)
		return nil
	}
	var fetched []gomatrixserverlib.PDU
	for _, raw := range transaction.PDUs {
		event, reason := h.parseRelayedEvent(ctx, raw)
		if reason != "" {
			slog.Debug("fedapi: refused a fetched event", "event_id", eventID, "reason", reason)
			continue
		}
		if from := event.RoomID().String(); from != roomID {
			slog.Warn("fedapi: a peer supplied an event from another room while filling a gap",
				"origin", origin, "room_id", roomID, "supplied_room_id", from)
			continue
		}
		fetched = append(fetched, event)
	}
	return fetched
}
