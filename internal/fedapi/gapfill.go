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
	// gapFillRounds is how many requests to the peer one refused event may cause.
	gapFillRounds = 5
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
// `GET /event/{eventID}` asks for one event. Slower, and the fallback for a peer that serves only that.
//
// The loop is a loop and not recursion because a peer can describe a cycle: A follows B, B follows A.
// Each round fetches what the room server just said it needs and retries the event the peer actually
// sent, so progress is visible in the log and the bound is a real stop.
func (h *Handler) admitWithGapFill(ctx context.Context, origin, roomID string, event gomatrixserverlib.PDU) error {
	err := h.opts.Join.AcceptFederationEvent(ctx, roomID, event)
	var gap *roomserver.MissingPredecessorError
	for round := 0; ; round++ {
		if err == nil {
			return nil
		}
		if !errors.As(err, &gap) {
			return err
		}
		if round >= gapFillRounds {
			slog.Warn("fedapi: gave up filling a gap in a federated room",
				"room_id", roomID, "missing_event_id", gap.EventID, "rounds", round)
			return err
		}
		if !h.fetchMissingEvents(ctx, origin, roomID, event.EventID(), gap) {
			// The bulk endpoint answered nothing this server could use — a peer that does not serve
			// it, or one that claims to have nothing between the two points. Ask for the one event
			// the room server named, which is the question the oldest peers can still answer.
			if !h.fetchEvent(ctx, origin, roomID, gap.EventID) {
				// Nothing could be fetched, so the original refusal is the answer to give back: it is
				// the true one, and it names what is missing.
				return err
			}
		}
		err = h.opts.Join.AcceptFederationEvent(ctx, roomID, event)
	}
}

// fetchMissingEvents asks the peer for the events between a point this server holds and the event it
// just sent, and admits everything it gives back.
//
// It reports whether anything was admitted. A refusal to admit one event of a batch is not a refusal of
// the batch: a peer returning fifty events this server cannot authenticate should still have its honest
// events stored, and the room server decides event by event exactly as it does for a transaction.
func (h *Handler) fetchMissingEvents(ctx context.Context, origin, roomID, latest string, gap *roomserver.MissingPredecessorError) bool {
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
			if err := h.opts.Join.AcceptFederationEvent(ctx, roomID, candidate); err != nil {
				slog.Debug("fedapi: could not store a gap-fill event",
					"event_id", candidate.EventID(), "error", err)
				remaining = append(remaining, candidate)
				continue
			}
			admitted = true
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
func (h *Handler) fetchEvent(ctx context.Context, origin, roomID, eventID string) bool {
	if h.opts.Peer == nil {
		return false
	}
	// The library names the *requesting* server "origin" and the peer second, which is the opposite of
	// how this file reads from the peer's side everywhere else. Swapping them produces
	// "no signing identity for server name <peer>": the client was asked to sign as somebody it is not.
	transaction, err := h.opts.Peer.Signed().GetEvent(ctx, h.opts.ServerName, spec.ServerName(origin), eventID)
	if err != nil {
		slog.Debug("fedapi: a peer could not supply a missing event",
			"origin", origin, "event_id", eventID, "error", err)
		return false
	}

	admitted := false
	for _, raw := range transaction.PDUs {
		event, reason := h.parseRelayedEvent(ctx, raw)
		if reason != "" {
			slog.Debug("fedapi: refused a fetched event", "event_id", eventID, "reason", reason)
			continue
		}
		if fetched := event.RoomID().String(); fetched != roomID {
			slog.Warn("fedapi: a peer supplied an event from another room while filling a gap",
				"origin", origin, "room_id", roomID, "supplied_room_id", fetched)
			continue
		}
		if err := h.opts.Join.AcceptFederationEvent(ctx, roomID, event); err != nil {
			slog.Debug("fedapi: could not store a fetched event", "event_id", eventID, "error", err)
			continue
		}
		admitted = true
	}
	return admitted
}
