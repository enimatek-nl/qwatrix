package roomserver

import (
	"context"
	"log/slog"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// queueFederationDelivery records that every other homeserver with a member in this room is owed a copy
// of a locally originated event.
//
// The obligation is written in the same transaction as the event, and that is the entire design: a
// separate write afterwards can be lost between the two statements, and the result is an event that
// some member of the room will never be told about. Nothing then looks wrong — the sender's client was
// told the message was sent, this server has it, and one server keeps a room whose copy of history
// quietly differs from everybody else's.
//
// Only this server's own events are queued. Forwarding a peer's events on to the rest of the room is
// federation's job too, and is not done yet; the consequence is recorded in the documentation rather
// than hidden, and it is that a room spanning three homeservers only hears from the ones its members are
// directly connected to.
func (s *Service) queueFederationDelivery(ctx context.Context, tx store.Tx, roomID string, row *store.EventRow) error {
	if !s.federatesOutbound {
		// A deployment that is not federating must not grow a queue it will never drain.
		return nil
	}
	// Whose servers are owed a copy: everybody currently joined, and everybody waiting on an
	// invite. Invited counts because an invitation is a claim about a room made to a server that
	// has not accepted it yet — a kick or a ban of that invitee has to arrive, or their server
	// keeps an offer that has been withdrawn. Knocking does not count: a knocker is not in the
	// room and is not owed its history.
	members, err := s.store.Memberships.ListInTX(ctx, tx, roomID, []string{"join", "invite"}, nil)
	if err != nil {
		return err
	}
	queued := 0
	seen := map[string]bool{}
	now := time.Now().UnixMilli()
	rows := make([]*store.FederationOutbox, 0, len(members))
	for _, member := range members {
		_, domain, err := matrix.ParseUserID(member.UserID, s.cfg.ServerName)
		if err != nil || domain == s.cfg.ServerName || seen[domain] {
			// This server's own members are not owed a delivery: they already have the event, because
			// this is the server that just made it.
			continue
		}
		seen[domain] = true
		queued++
		rows = append(rows, &store.FederationOutbox{
			Destination: domain,
			RoomID:      roomID,
			EventID:     row.EventID,
			JSON:        row.JSON,
			NextMS:      now,
			CreatedMS:   now,
		})
	}
	// The subject of a membership event is owed the event even when the event is what removed
	// them. The list above is computed after the event was applied, so a ban has already turned
	// its target into a non-member and a leave has turned them into an ex-member: without this,
	// the one homeserver that must be told is dropped from the queue, and nothing downstream can
	// notice, because the peer never learns it was skipped. The victim's reads of the room are
	// refused from now on, so a server that waits to be asked will never be asked — the victim
	// just keeps a room they were thrown out of.
	if row.Type == matrix.EventTypeMember && row.StateKey != nil {
		if _, domain, err := matrix.ParseUserID(*row.StateKey, s.cfg.ServerName); err == nil &&
			domain != s.cfg.ServerName && !seen[domain] {
			seen[domain] = true
			queued++
			rows = append(rows, &store.FederationOutbox{
				Destination: domain,
				RoomID:      roomID,
				EventID:     row.EventID,
				JSON:        row.JSON,
				NextMS:      now,
				CreatedMS:   now,
			})
		}
	}

	if len(rows) == 0 {
		return nil
	}
	if err := s.store.FederationOutbox.Append(ctx, tx, rows); err != nil {
		return err
	}
	slog.Debug("roomserver: queued a room event for delivery to peers",
		"room_id", roomID, "event_id", row.EventID, "servers", queued)
	return nil
}
