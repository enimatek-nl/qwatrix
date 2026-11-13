package fedapi

// Asking a peer for history this server does not hold.
//
// Two questions, asked together in practice. "Where was this room at that moment?" is answered by one
// event; "and what was around it?" is answered by a window. The room server decides when to ask and what
// may be kept; this file is the transport, which knows how to sign a request and — more importantly —
// how not to believe an answer.
//
// Everything fetched here is a body a peer chose to send. Each event is therefore checked against its
// sender's signing key, exactly as an event arriving in a transaction is, and refused if that check
// fails: a server that stores what a peer tells it without verifying is a server whose history anybody
// can write.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// DetachedStore is the half of the room server this reader needs: keep these verified events without
// placing them in the room's graph.
type DetachedStore interface {
	StoreDetachedEvents(ctx context.Context, events []gomatrixserverlib.PDU) (int, error)
}

// RoomVersions is how a fetched event is parsed: a room's version decides the event's ID format, its hash
// and its redaction rules, so an event cannot be read without it.
type RoomVersions interface {
	RoomVersionFor(ctx context.Context, roomID string) (gomatrixserverlib.IRoomVersion, error)
}

// RemoteHistoryOptions wires the reader to its peer client and to the room server's storage.
type RemoteHistoryOptions struct {
	ServerName spec.ServerName
	Peer       *Peer
	Events     *events.Service
	KeyRing    *gomatrixserverlib.KeyRing
	Store      DetachedStore
	Versions   RoomVersions
}

// RemoteHistory asks peers about a room's past.
type RemoteHistory struct {
	opts RemoteHistoryOptions
}

// NewRemoteHistory builds the reader.
func NewRemoteHistory(opts RemoteHistoryOptions) *RemoteHistory {
	return &RemoteHistory{opts: opts}
}

// timestampAnswer is the MSC3059 federation response: one event, and the moment it sat at.
type timestampAnswer struct {
	Origin         spec.ServerName `json:"origin"`
	OriginServerTS int64           `json:"origin_server_ts"`
	PDU            json.RawMessage `json:"pdu"`
}

// EventAtTimestamp asks a resident server where a room was at a moment.
//
// The event comes back verified or as an error: this server is about to store what it is given and then
// tell a user that this is where the room's history was, and an unverified event makes that a lie
// somebody else authored.
func (r *RemoteHistory) EventAtTimestamp(ctx context.Context, server, roomID string,
	timestampMS int64, forward bool) (json.RawMessage, error) {
	if r.opts.Peer == nil {
		return nil, matrix.NewUnknown("this homeserver cannot ask peers about history")
	}
	direction := "b"
	if forward {
		direction = "f"
	}
	path := "/_matrix/federation/v1/timestamp_to_event/" + url.PathEscape(roomID) +
		"?ts=" + strconv.FormatInt(timestampMS, 10) + "&dir=" + direction

	var answer timestampAnswer
	if err := r.opts.Peer.GetSignedJSON(ctx, spec.ServerName(server), path, &answer); err != nil {
		return nil, fmt.Errorf("federation: asking %s where %s was: %w", server, roomID, err)
	}
	if len(answer.PDU) == 0 {
		// A peer that answers with an empty `pdu` has not found the moment either. Saying so lets the
		// caller answer honestly rather than storing nothing and reporting something.
		return nil, matrix.NewNotFound("%s has no event in %q at that moment", server, roomID)
	}
	events, err := r.verify(ctx, server, roomID, [][]byte{answer.PDU}, "timestamp answer")
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, matrix.NewUnknown(fmt.Sprintf(
			"%s supplied a timestamp answer that could not be trusted", server))
	}
	return json.RawMessage(events[0].JSON()), nil
}

// BackfillHistory asks a peer for a window of a room's history and keeps what is new here.
//
// A window is best-effort by nature: an event that cannot be verified is dropped rather than failing the
// whole window, because losing a peer's badly-signed event is a smaller loss than telling a user their
// question failed. The drop is logged at warning level, since a peer that keeps supplying unverifiable
// events is a peer worth naming.
func (r *RemoteHistory) BackfillHistory(ctx context.Context, server, roomID string, limit int) (int, error) {
	if r.opts.Peer == nil || limit <= 0 {
		return 0, nil
	}
	transaction, err := r.opts.Peer.Signed().Backfill(ctx, r.opts.ServerName, spec.ServerName(server),
		roomID, limit, nil)
	if err != nil {
		return 0, fmt.Errorf("federation: asking %s to backfill %s: %w", server, roomID, err)
	}
	raws := make([][]byte, 0, len(transaction.PDUs))
	for _, raw := range transaction.PDUs {
		raws = append(raws, raw)
	}
	events, err := r.verify(ctx, server, roomID, raws, "backfill window")
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}
	return r.opts.Store.StoreDetachedEvents(ctx, events)
}

// verify parses raw events as this room's version and checks each one's signature.
//
// The room check is not ceremony. A peer can put any event in a window, and without it, being told to
// backfill one room is a way to write into another.
func (r *RemoteHistory) verify(ctx context.Context, server, roomID string, raws [][]byte,
	which string) ([]gomatrixserverlib.PDU, error) {
	version, err := r.opts.Versions.RoomVersionFor(ctx, roomID)
	if err != nil {
		return nil, err
	}
	userIDForSender := r.opts.Events.UserIDForSender()
	verified := make([]gomatrixserverlib.PDU, 0, len(raws))
	for _, raw := range raws {
		event, err := version.NewEventFromUntrustedJSON(raw)
		if err != nil {
			slog.Warn("fedapi: a peer's history could not be read",
				"origin", server, "room_id", roomID, "which", which, "error", err)
			continue
		}
		if declared := event.RoomID().String(); declared != roomID {
			slog.Warn("fedapi: a peer's history named another room",
				"origin", server, "room_id", roomID, "declared_room_id", declared, "which", which)
			continue
		}
		if err := gomatrixserverlib.VerifyEventSignatures(ctx, event, r.opts.KeyRing, userIDForSender); err != nil {
			slog.Warn("fedapi: a peer's history did not verify",
				"origin", server, "event_id", event.EventID(), "error", err)
			continue
		}
		verified = append(verified, event)
	}
	return verified, nil
}
