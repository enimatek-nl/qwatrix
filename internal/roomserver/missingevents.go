package roomserver

// Answering a peer that says "your room has events I have never seen, between these two".
//
// A federating server discovers a gap constantly: an event arrives whose `prev_events` name history it
// does not hold, and rather than refuse an honest event it asks the server that reported it — this one —
// for the stretch in between. Without that question being answerable, a room whose members live on two
// homeservers ends up with two histories that never join up: each side keeps refusing the events on the
// far side of its gap, and the two servers go on with different versions of the same conversation.
//
// The answer is a window of the room's own events, oldest first, strictly between the two ends the peer
// named. Two details of that shape matter more than they look:
//
//   - The ends themselves are not returned. The peer asked because it has those two and is missing what
//     hangs between them; sending the ends back would be resend traffic, and a peer that stores what it is
//     given would be duplicating events it already has.
//   - The order is oldest first, not newest first. A peer is about to attach this window to the earliest
//     event it named, and a window handed over backwards attaches a room's history in the wrong direction
//     — which is the kind of damage that shows up much later, as a timeline that pages backwards.
//
// What the peer is shown is bounded by the same rule as every other read of room contents here: a server
// may read what its own users are in. The history of a room is not public information because it is
// federated, and a request that names a room the asking server has left is refused rather than answered
// with the history it is no longer entitled to.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

const (
	// missingEventsDefaultLimit is the window size when the request does not name one. The specification
	// caps this endpoint at 100 events, which is also the point past which the answer stops being a
	// window and starts being the room.
	missingEventsDefaultLimit = 100
	missingEventsMaxLimit     = 100
	// missingEventsWalkBound caps how many events one request may look at while walking the graph,
	// however small the answer turns out to be. A peer can name an `earliest_events` this server has
	// never seen, and then the walk backwards from `latest_events` has no natural stopping point at all:
	// without a bound, that is one request from a peer that costs this server a walk over every event in
	// the room, and a room a year old has enough of those to be an outage.
	missingEventsWalkBound = 500
)

// MissingEventsForPeer answers `POST /_matrix/federation/v1/get_missing_events/{roomID}`.
//
// `origin` is the server that signed the request, which is the only identity this answer trusts: the
// window is narrowed to what that server's own users may see, including the room's history visibility —
// an event from before a server joined a "joined"-visibility room is handed over with its content removed
// rather than withheld, because the peer needs the shape of the graph to close its gap and does not need
// to read what was said.
func (s *Service) MissingEventsForPeer(ctx context.Context, roomID, origin string,
	earliest, latest []string, limit int, minDepth int64) ([]json.RawMessage, error) {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
		}
		return nil, err
	}
	if limit <= 0 {
		limit = missingEventsDefaultLimit
	}
	if limit > missingEventsMaxLimit {
		limit = missingEventsMaxLimit
	}
	// The same rule as every other read of room contents: a server may read what its own users are in.
	// A gap request is the most tempting of them to leave open, because it looks like a mechanical
	// question about event IDs rather than a read of a room — and it is the one that hands over the events
	// a room's members were told about privately, in order, with their content intact.
	allowed, err := s.peerMayReadRoom(ctx, roomID, origin)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, matrix.NewForbidden("this server may not read the history of this room")
	}
	if len(latest) == 0 {
		// Nothing to walk backwards from. This is a malformed request rather than an empty answer, and
		// saying so is what stops a peer concluding this room has no history at all.
		return nil, matrix.NewMissingParam("latest_events")
	}

	// Everything reachable from the earliest events, ends included, is by definition not missing.
	known, err := s.walkHistoryBackwards(ctx, roomID, earliest, true)
	if err != nil {
		return nil, err
	}
	// Everything reachable from the newest events is what this server could offer.
	reachable, err := s.walkHistoryBackwards(ctx, roomID, latest, false)
	if err != nil {
		return nil, err
	}

	// The asking server's own place in the room's history, which is what bounds its read of a
	// "joined" or "invited" room. The earliest membership this server holds for any of their users is
	// used rather than the room's, because two of their users may have joined months apart and the
	// earlier one is the one whose history they are entitled to.
	bound, err := s.peerHistoryBound(ctx, roomID, origin)
	if err != nil {
		return nil, err
	}
	version, err := s.RoomVersionFor(ctx, roomID)
	if err != nil {
		return nil, err
	}

	candidates := make([]store.EventRow, 0, len(reachable))
	for eventID, row := range reachable {
		if _, known := known[eventID]; known {
			continue
		}
		if row.RejectedReason != "" || row.Detached {
			// A rejected event is one this server decided is not part of the room, and detached history
			// is a room's past this server was told about without being told how it connects. Offering
			// either would be this server recommending an event it does not believe in.
			continue
		}
		if minDepth > 0 && row.Depth < minDepth {
			continue
		}
		candidates = append(candidates, row)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		// Depth first, because that is the order the graph says these events happened in; timestamp to
		// settle events at the same depth; event ID last so the answer does not depend on map order.
		if candidates[i].Depth != candidates[j].Depth {
			return candidates[i].Depth < candidates[j].Depth
		}
		if candidates[i].OriginServerTS != candidates[j].OriginServerTS {
			return candidates[i].OriginServerTS < candidates[j].OriginServerTS
		}
		return candidates[i].EventID < candidates[j].EventID
	})
	if len(candidates) > limit {
		// The oldest `limit` of them: the peer is closing the gap at the end it named first, and events
		// this far back are the ones it cannot get any other way.
		candidates = candidates[:limit]
	}

	events := make([]json.RawMessage, 0, len(candidates))
	for _, row := range candidates {
		raw := json.RawMessage(row.JSON)
		if s.withholdsHistoryFrom(room.HistoryVisibility) && bound > 0 && row.ID < bound {
			redacted, err := s.events.Redact(string(version.Version()), row.JSON)
			if err != nil {
				// A window with one event missing is a gap the peer will ask about again; a whole request
				// that fails because one event could not be re-encoded is a room that stops federating.
				continue
			}
			raw = json.RawMessage(redacted)
		}
		events = append(events, raw)
	}
	return events, nil
}

// withholdsHistoryFrom reports whether a room's visibility rule means "not yet" for an event a reader
// predates: `shared` says hand it over, `world_readable` says everybody may read it, and `invited` and
// `joined` say the content belongs to the people who were there.
func (s *Service) withholdsHistoryFrom(visibility string) bool {
	switch visibility {
	case matrix.HistoryInvited, matrix.HistoryJoined, "":
		return true
	default:
		return false
	}
}

// peerHistoryBound is the stream position at which the asking server became part of this room, or 0 when
// this server cannot tell — in which case nothing is withheld, because a rule that cannot be applied is
// not a licence to hide a room's whole history from a legitimate peer.
func (s *Service) peerHistoryBound(ctx context.Context, roomID, origin string) (int64, error) {
	if origin == "" || origin == string(s.cfg.ServerName) {
		return 0, nil
	}
	// Every membership the room has, not only the joins: what is wanted is the *earliest* moment one of
	// their users appeared, and a user who joined and left was in the room earlier than either event says.
	members, err := s.store.Memberships.List(ctx, roomID, nil, nil)
	if err != nil {
		return 0, fmt.Errorf("reading this room's members: %w", err)
	}
	var bound int64
	for _, member := range members {
		if _, domain, err := matrix.ParseUserID(member.UserID, s.cfg.ServerName); err != nil {
			continue
		} else if string(domain) != origin {
			continue
		}
		if member.StreamPos <= 0 {
			continue
		}
		if bound == 0 || member.StreamPos < bound {
			bound = member.StreamPos
		}
	}
	return bound, nil
}

// walkHistoryBackwards collects the events reachable from a set of events by following `prev_events`,
// which is the only direction a Matrix graph can be walked without an index over every event in a room.
//
// `includeSeeds` decides whether the events named are themselves part of the answer. It is what makes the
// two ends of a request mean different things: the earliest events are excluded because the peer has them,
// and the newest ones are excluded because the peer has those too.
//
// The walk stops at `missingEventsWalkBound` events. Stopping early is a truncated answer rather than a
// wrong one — the peer will find the rest of the gap still missing and ask again with a nearer end, which
// is the loop this endpoint is designed for.
func (s *Service) walkHistoryBackwards(ctx context.Context, roomID string, seeds []string,
	includeSeeds bool) (map[string]store.EventRow, error) {
	found := map[string]store.EventRow{}
	frontier := make([]string, 0, len(seeds))
	seen := map[string]bool{}
	isSeed := map[string]bool{}
	for _, seed := range seeds {
		if seed != "" && !seen[seed] {
			seen[seed] = true
			isSeed[seed] = true
			frontier = append(frontier, seed)
		}
	}
	for len(frontier) > 0 && len(found) < missingEventsWalkBound {
		rows, err := s.store.Events.ByEventIDs(ctx, frontier)
		if err != nil {
			return nil, fmt.Errorf("walking this room's history: %w", err)
		}
		frontier = frontier[:0]
		for i := range rows {
			row := rows[i]
			if row.RoomID != roomID {
				// An event ID that resolves to another room's event is not this room's history, however
				// the peer came to name it here.
				continue
			}
			if _, want := found[row.EventID]; !want && (includeSeeds || !isSeed[row.EventID]) {
				found[row.EventID] = row
			}
			for _, prev := range parseEventIDList(row.PrevEvents) {
				if !seen[prev] && len(found) < missingEventsWalkBound {
					seen[prev] = true
					frontier = append(frontier, prev)
				}
			}
		}
	}
	return found, nil
}

// parseEventIDList reads one of the stored JSON arrays of event IDs. A value that cannot be parsed yields
// nothing: a broken edge in one event's history is a reason to stop walking there, not a reason to refuse
// a peer's whole request.
func parseEventIDList(raw string) []string {
	if raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
}
