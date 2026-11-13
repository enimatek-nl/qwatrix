package roomserver

// Read paths: state, timeline pagination, members, room lists, aliases, directory.
//
// Reads are deliberately read-only and lock-free: they open no transaction and take no
// write lock, because a homeserver's read volume is dominated by /sync and /messages, and
// making those wait behind a room mutation would turn one slow write into a stall for every
// polling client.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// RenderOptions are the caller-specific parts of rendering a stored event for one client.
type RenderOptions struct {
	// DeviceID selects whose transaction ID is echoed: only the device that sent an event
	// sees its own unsigned.transaction_id.
	DeviceID string
	// Localpart and Membership are the reader's relationship to the room, which decides
	// whether history is visible at all.
	Localpart  string
	Membership string
	// MembershipStreamPos is the stream position of the reader's own member event, used by
	// the history-visibility rules to decide what "before you joined" means.
	MembershipStreamPos int64
	// Forgotten is the reader's forgotten flag: they have left and then asked the server to
	// forget the room.
	Forgotten bool
	// SkipTransactionID suppresses the transaction echo entirely (used over federation,
	// where a client's transaction ID is nobody else's business).
	SkipTransactionID bool
	// Cache shares per-room reads between the events of one response. It is optional:
	// without it every event pays for its own lookup, which is correct and slower.
	Cache *RenderCache
}

// RenderCache holds the per-room reads that are the same for every event in one response.
//
// Its whole reason for existing is [Service.membershipTrail]: a `/sync` response can render a
// couple of hundred events, and asking the database who the reader was before each of them
// would turn one response into two hundred queries. The cached value is a membership *trail* —
// a list of positions that never changes once written, because events are immutable and are
// never deleted — so a per-request lifetime is more conservative than the data requires.
type RenderCache struct {
	service *Service
	mu      sync.Mutex
	trails  map[string][]store.MembershipChange
}

// NewRenderCache starts a cache for one response.
func (s *Service) NewRenderCache() *RenderCache {
	return &RenderCache{service: s, trails: map[string][]store.MembershipChange{}}
}

// membershipTrail returns the reader's membership changes in a room, oldest first.
func (c *RenderCache) membershipTrail(ctx context.Context, roomID, stateKey string) ([]store.MembershipChange, error) {
	if c == nil {
		return c.service.store.Events.MembershipTrail(ctx, roomID, stateKey)
	}
	key := roomID + "\x00" + stateKey
	c.mu.Lock()
	trail, ok := c.trails[key]
	c.mu.Unlock()
	if ok {
		return trail, nil
	}
	trail, err := c.service.store.Events.MembershipTrail(ctx, roomID, stateKey)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.trails == nil {
		c.trails = map[string][]store.MembershipChange{}
	}
	c.trails[key] = trail
	c.mu.Unlock()
	return trail, nil
}

// MembershipAt returns the reader's room membership at the point of a given stream position,
// which is MSC4115's `unsigned.membership`.
//
// The default is "leave", and the specification is explicit about why: a user who had no
// `m.room.member` state at that moment was not in the room, and "leave" is what the membership
// of a non-member is called. Note whose membership this is — the *reader's*, not the sender's.
// The question a client is asking is "could I have seen this at the time", and that is a fact
// about the reader's presence in the room, not about who was talking.
func (s *Service) MembershipAt(ctx context.Context, roomID, localpart string, at int64, cache *RenderCache) (string, error) {
	stateKey := matrix.UserID(localpart, s.cfg.ServerName)
	trail, err := cache.membershipTrail(ctx, roomID, stateKey)
	if err != nil {
		return "", err
	}
	membership := matrix.MembershipLeave
	for _, change := range trail {
		if change.Position > at {
			break
		}
		membership = change.Membership
	}
	return membership, nil
}

// Render turns a stored event into the JSON a client sees.
//
// The bytes of the stored event are the signed ones and are not rewritten; what happens
// here is additive and lives under `unsigned`, which is the specification's designated
// place for facts about an event that the sender cannot vouch for.
func (s *Service) Render(ctx context.Context, row *store.EventRow, opts RenderOptions) (json.RawMessage, error) {
	decorate := events.UnsignedOptions{EventID: row.EventID}
	// A transaction ID belongs to the device that sent it, and to that device only.
	if row.TransactionID != "" && !opts.SkipTransactionID &&
		opts.DeviceID != "" && row.DeviceID == opts.DeviceID {
		decorate.TransactionID = row.TransactionID
	}
	if row.ReceivedMS > 0 {
		decorate.Now = time.Now()
	}
	// `unsigned.membership` is the reader's own standing in the room when this event was
	// written. It is best-effort by the spec's own wording ("wherever practical"), so a
	// lookup failure is not a reason to fail the read: the event itself is already answerable.
	if opts.Localpart != "" {
		if membership, err := s.MembershipAt(ctx, row.RoomID, opts.Localpart, row.ID, opts.Cache); err == nil {
			decorate.Membership = membership
		}
	}
	// The state this event replaced, for the `prev_content` a client needs to show that a
	// room name or topic was edited rather than set.
	if row.StateKey != nil {
		entry, err := s.store.State.Get(ctx, row.RoomID, row.Type, *row.StateKey)
		if err == nil && entry.ReplacesEventID != "" && entry.EventID == row.EventID {
			if replaced, err := s.store.Events.ByEventID(ctx, entry.ReplacesEventID); err == nil {
				decorate.ReplacesState = row.EventID
				decorate.PrevContent = contentOf(replaced.JSON)
			}
		}
	}
	return s.events.Decorate(json.RawMessage(row.JSON), decorate)
}

// contentOf extracts an event's content block, for prev_content.
func contentOf(eventJSON string) json.RawMessage {
	var document struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(eventJSON), &document); err != nil {
		return nil
	}
	return document.Content
}

// EventRow renders a single event for one reader.
func (s *Service) EventRow(ctx context.Context, eventID string, opts RenderOptions) (json.RawMessage, error) {
	row, err := s.store.Events.ByEventID(ctx, eventID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Event %q does not exist", eventID)
		}
		return nil, err
	}
	return s.Render(ctx, row, opts)
}

// EventInRoom returns one event, checked against the reader's right to see the room.
func (s *Service) EventInRoom(ctx context.Context, roomID, eventID string, opts RenderOptions) (json.RawMessage, error) {
	// /event answers 404 for everything the caller may not see, including a room it may not
	// read. This is the one read endpoint where 403 would be the wrong kindness: an event ID is
	// a capability nobody handed the caller, so "no such event" is both the truth from their
	// point of view and the answer that stops the endpoint from being used to ask whether a
	// room or an event exists somewhere they have no standing.
	notFound := func(reason string, args ...any) error {
		return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound, reason, args...)
	}
	if err := s.checkReadable(ctx, roomID, opts); err != nil {
		return nil, notFound("Event %q does not exist", eventID)
	}
	row, err := s.store.Events.ByEventID(ctx, eventID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Event %q does not exist", eventID)
		}
		return nil, err
	}
	if row.RoomID != roomID {
		// An event that exists but not in the room named: the spec's answer is "not found",
		// because confirming which room an event lives in would leak the existence of a
		// private room.
		return nil, notFound("Event %q does not exist in this room", eventID)
	}
	if !s.visibleTo(ctx, roomID, row, opts) {
		return nil, notFound("Event %q does not exist", eventID)
	}
	return s.Render(ctx, row, opts)
}

// visibleTo applies the part of the history rules that is about one event rather than the room:
// under a visibility of "joined" or "invited", a member may read what happened while they were a
// member, and not what happened before they arrived. Without this bound, joining a long-lived
// room would hand a newcomer every message sent before their invite — which is precisely what
// the room's creator chose to withhold, and the reason the bound is enforced at read time
// instead of at write time, since the same event must become readable to somebody who joins and
// stays.
//
// "shared" and "world_readable" deliberately have no such bound: those two exist to say that a
// member may read the room's past.
func (s *Service) visibleTo(ctx context.Context, roomID string, row *store.EventRow, opts RenderOptions) bool {
	if cutoff := s.readCutoff(opts); cutoff > 0 && row.ID > cutoff {
		// Written after this reader left. Their own leave event sits at the cutoff and is
		// included — a member who could not see the event that ended their membership would
		// have no way to explain the room's history stopping where it does.
		return false
	}
	if opts.MembershipStreamPos == 0 || row.ID >= opts.MembershipStreamPos {
		// Nothing before the reader's own membership to bound, or this event is not before it.
		// A former member has already been bounded above by the cutoff.
		return true
	}
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		// A room this server cannot read is not a room whose events it will hand out.
		return false
	}
	switch room.HistoryVisibility {
	case matrix.HistoryInvited:
		// The bound is the invite, not the join: an invitee who could not read what was sent
		// between being invited and joining would lose messages their own client cannot
		// recover, and "you may read from the moment you were told about this room" is the
		// whole difference between invited and joined visibility.
		//
		// The invite's position is not on the membership row, which only holds the *current*
		// membership, so it is read from the event stream: events are never deleted, so the
		// user's earliest member event is the invitation itself.
		invitePos, err := s.store.Events.FirstPositionOfState(ctx, roomID,
			matrix.EventTypeMember, matrix.UserID(opts.Localpart, s.cfg.ServerName))
		if err != nil || invitePos == 0 {
			return false
		}
		return row.ID >= invitePos
	case matrix.HistoryJoined, "":
		return false
	default:
		return true
	}
}

// readCutoff returns the stream position past which this reader may no longer read, or 0 when
// nothing bounds them.
//
// A former member is entitled to the history they were part of, and to nothing after it: they
// left, and the room's next events are not theirs. The membership row's position is the leave
// event itself, so it serves as both the lower bound (what came before is theirs under a
// "shared" rule) and the upper one.
func (s *Service) readCutoff(opts RenderOptions) int64 {
	if opts.Membership == matrix.MembershipLeave {
		return opts.MembershipStreamPos
	}
	return 0
}

// checkReadable applies the room's history visibility to a read request.
//
// This is the single place history visibility is enforced for reads, so that /messages,
// /event, /context and /relations cannot disagree about what a client may see.
func (s *Service) checkReadable(ctx context.Context, roomID string, opts RenderOptions) error {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", roomID)
		}
		return err
	}
	// Forgetting is the caller's own declaration that the room is no longer their business, so
	// it ends read access even where the room's history rule would still allow it. A "forget"
	// that only hid the room from one device's list, leaving the history one HTTP call away,
	// would be a UI change sold as a data one — and the caller may have forgotten precisely
	// because a colleague's machine should not still be able to read it.
	if opts.Forgotten {
		return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
			"You have forgotten this room")
	}

	switch room.HistoryVisibility {
	case matrix.HistoryWorldReadable:
		return nil
	case matrix.HistoryJoined, "":
		switch opts.Membership {
		case matrix.MembershipJoin, matrix.MembershipLeave:
			// Leaving does not unsee what a member saw. "joined" is the room saying "anybody
			// who was in here may read that stretch of history", and a former member is
			// still one of those people — which is why a client can still render a room the
			// user left, and why deleting their access at the door would lose messages their
			// own device already holds. The bound is temporal, not a refusal: see
			// [Service.readCutoff].
			return nil
		}
		return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
			"This room's history is only readable by its members")
	case matrix.HistoryInvited:
		switch opts.Membership {
		case matrix.MembershipJoin, matrix.MembershipInvite, matrix.MembershipLeave:
			return nil
		}
		return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
			"This room's history is only readable by members")
	case matrix.HistoryShared:
		switch opts.Membership {
		case matrix.MembershipJoin, matrix.MembershipLeave:
			// "shared" means members keep reading what they were part of; a ban ends that,
			// and an invite has not begun it.
			return nil
		}
		return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
			"This room's history is only readable by members")
	default:
		// An unknown visibility value can only come from a room this server did not create.
		// Refusing is the safe answer; a homeserver that guesses readable is the one that
		// ends up in a security report.
		if opts.Membership == matrix.MembershipJoin {
			return nil
		}
		return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
			"This room's history visibility is not understood by this homeserver")
	}
}

// fetchHistoryAtTimestamp asks a resident server about a moment this server cannot place, and keeps the
// answer.
//
// Two requests, and the second is the one that makes the endpoint useful. The MSC tells a server that
// fetched an event this way to backfill around it, and the reason is on the client's side of the
// conversation: an event with nothing beside it is an answer the user can look at and nowhere to go, and
// what they do next is page. Asking for a window now saves every client that follows a round trip through
// a peer.
//
// It reports whether the peer named anything, and returns the transport's error alongside a false answer,
// because "the peer had nothing" and "the peer could not be reached" need different log lines and the
// same fallback: whatever this server already knows.
func (s *Service) fetchHistoryAtTimestamp(ctx context.Context, req TimestampRequest) (bool, error) {
	if s.remoteHistory == nil {
		return false, nil
	}
	server, remote := roomResidentServer(req.RoomID, s.cfg.ServerName)
	if !remote {
		// This server hosts the room, so there is nobody to ask who knows more than it does.
		return false, nil
	}
	raw, err := s.remoteHistory.EventAtTimestamp(ctx, server, req.RoomID, req.Timestamp, req.Forward)
	if err != nil {
		return false, err
	}

	// The window is fetched and kept before the single event it usually contains, and the order is not
	// fussiness. A stream position is assigned when an event arrives, and this server has no way to
	// re-place one later: storing the answer first would give it a position older than the neighbour it
	// was fetched alongside, and a client paging backward from the answer would walk away from the
	// history it sits in.
	if _, err := s.remoteHistory.BackfillHistory(ctx, server, req.RoomID, 100); err != nil {
		// A window is a bonus on top of an answer that has already arrived; refusing the user's
		// question because the bonus failed would be a worse service than an incomplete history.
		slog.Debug("backfilling around a fetched event failed", "room_id", req.RoomID, "error", err)
	}
	events, err := s.ParsedEvents(ctx, req.RoomID, []json.RawMessage{raw})
	if err != nil {
		return false, err
	}
	if _, err := s.StoreDetachedEvents(ctx, events); err != nil {
		return false, err
	}
	return true, nil
}

// roomResidentServer is the server named by a room ID — the one that created it, and the first one worth
// asking about a room this server did not create. It reports false for a room of this server's own, which
// is not a question to ask anybody.
func roomResidentServer(roomID, serverName string) (string, bool) {
	_, origin, err := matrix.ParseRoomID(roomID)
	if err != nil || origin == "" || origin == serverName {
		return "", false
	}
	return origin, true
}

// ContextRequest asks for the neighbourhood of one event: what it was, what happened just before and
// just after it, and the tokens to keep walking in either direction.
type ContextRequest struct {
	RoomID  string
	EventID string
	// Limit is the ceiling on the two arrays together, not on each: a client that asked for ten events
	// does not want twenty-one, and a client that asked for none still wants the event it named.
	Limit  int
	Reader RenderOptions
}

// ContextResponse is the event, its neighbours, and the state a client needs to render them.
//
// `events_before` runs newest-first and `events_after` oldest-first — each array ordered away from the
// centre, because that is the direction the client is drawing. The tokens are the other half of the
// response: an event with no way to walk on from it is a dead end, which is what the client had before it
// asked.
type ContextResponse struct {
	Event        json.RawMessage   `json:"event"`
	EventsBefore []json.RawMessage `json:"events_before"`
	EventsAfter  []json.RawMessage `json:"events_after"`
	Start        string            `json:"start"`
	End          string            `json:"end"`
	State        []json.RawMessage `json:"state"`
}

// EventContext answers "what was around this event?", which is what a client asks after it has landed on
// an event from anywhere at all: a search result, a notification, a reply, a timestamp lookup.
//
// The centre is read through the same single-event read the client would have used, so the two endpoints
// cannot disagree about whether the event exists for this reader: if it may not see the event, it
// certainly may not see its neighbours, and an answer that carried the surroundings but not the event
// would be a leak dressed as a partial failure.
func (s *Service) EventContext(ctx context.Context, req ContextRequest) (*ContextResponse, error) {
	if req.Limit < 0 {
		return nil, matrix.NewInvalidParam("limit must not be negative")
	}
	// The room's history rule first, answered the way every other history read answers it: a stranger is
	// told they may not read this room's past, which is a fact about the reader, and only then is an event
	// looked up at all. `/messages` and `/timestamp_to_event` say the same thing the same way, and three
	// doors into the same history must not offer three dialects.
	if err := s.checkReadable(ctx, req.RoomID, req.Reader); err != nil {
		if apiErr, ok := matrix.AsAPIError(err); ok && apiErr.Status == http.StatusNotFound {
			return nil, matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
				"You are not permitted to read the history of this room")
		}
		return nil, err
	}
	event, err := s.EventInRoom(ctx, req.RoomID, req.EventID, req.Reader)
	if err != nil {
		return nil, err
	}
	row, err := s.store.Events.ByEventID(ctx, req.EventID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewNotFound("Event %q does not exist", req.EventID)
		}
		return nil, err
	}

	// The limit covers both arrays, so each side gets half. Rounding the earlier side up means a reader
	// walking back from an event sees at least as much as one walking on.
	// A caller with no opinion passes a negative and gets the specification's default of ten. Zero is a
	// different opinion: it means "the event and its tokens, and nothing around it", which is exactly what
	// a client asks for when it wants a place to page from. Defaulting zero to ten would answer a request
	// for nothing with a page of history and no way to ask for less.
	limit := req.Limit
	if limit < 0 {
		limit = 10
	}
	if limit > 100 {
		// The same ceiling `/messages` puts on one page, and for the same reason: `/context` renders
		// every event, its neighbourhood and the state at the centre, so one generous number from one
		// client is a request to render a room.
		limit = 100
	}
	beforeLimit := (limit + 1) / 2
	afterLimit := limit / 2

	// Both reads are bounded by the room, so an event that has drifted out of it (or never belonged to
	// it) cannot drag another room's history into the answer.
	// A limit of zero still returns the event it was asked for, and no neighbours: the two reads are
	// skipped rather than run with a limit of zero, because "no limit" and "no events" are different
	// instructions and a query cannot tell them apart. Clients send `limit=0` on purpose to ask for the
	// event and its tokens alone.
	var before, after []store.EventRow
	if beforeLimit > 0 {
		before, err = s.store.Events.Range(ctx, store.EventQuery{
			RoomIDs: []string{req.RoomID}, Limit: beforeLimit, Before: row.ID,
			OrderDesc: true, IncludeDetached: true,
		})
		if err != nil {
			return nil, err
		}
	}
	if afterLimit > 0 {
		after, err = s.store.Events.Range(ctx, store.EventQuery{
			RoomIDs: []string{req.RoomID}, Limit: afterLimit, After: row.ID,
			IncludeDetached: true,
		})
		if err != nil {
			return nil, err
		}
	}

	response := &ContextResponse{
		Event:        event,
		EventsBefore: []json.RawMessage{},
		EventsAfter:  []json.RawMessage{},
	}
	// `events_before` comes back newest-first from a descending read, which is the order the response
	// wants; `events_after` comes back oldest-first from an ascending one. Neither is reversed, which is
	// how "away from the centre" survives into the answer.
	for i := range before {
		rendered, err := s.Render(ctx, &before[i], req.Reader)
		if err != nil {
			return nil, err
		}
		response.EventsBefore = append(response.EventsBefore, rendered)
	}
	for i := range after {
		rendered, err := s.Render(ctx, &after[i], req.Reader)
		if err != nil {
			return nil, err
		}
		response.EventsAfter = append(response.EventsAfter, rendered)
	}

	// The tokens name the boundary this response reached, not the events it carried: a client that paged
	// on from here continues with the next event rather than the last one it already has. When a side was
	// empty the boundary is the event itself, which is where a reader with nothing to work from has to
	// start.
	// Going backward is the one direction that must exclude the centre even when there is nothing to
	// show, because `/messages?dir=b` reads *through* the event its token names: a token pointing at the
	// centre hands the client the event it already has and calls it progress. Forward needs no such
	// adjustment — a forward read begins after its token.
	response.Start = formatStreamToken(row.ID - 1)
	if len(before) > 0 {
		response.Start = formatStreamToken(before[len(before)-1].ID - 1)
	}
	response.End = formatStreamToken(row.ID)
	if len(after) > 0 {
		response.End = formatStreamToken(after[len(after)-1].ID)
	}

	// The state as it stood at the centre of the response. A client that jumped here has no other way to
	// know who was in the room or what it was called when these messages were written, and rendering
	// them with today's state would show it a room that never existed.
	stateRows, err := s.store.Events.StateSnapshot(ctx, req.RoomID, row.ID)
	if err != nil || len(stateRows) == 0 {
		// A room keeps snapshots for the points where a reader stopped reading; the current state is the
		// answer for a point that never needed one.
		stateRows, err = s.stateRowsFor(ctx, req.RoomID, req.Reader)
		if err != nil {
			return nil, err
		}
	}
	response.State = []json.RawMessage{}
	for i := range stateRows {
		rendered, err := s.Render(ctx, &stateRows[i], req.Reader)
		if err != nil {
			return nil, err
		}
		response.State = append(response.State, rendered)
	}
	return response, nil
}

// RemoteHistory is how the room server asks a peer about history this server does not hold.
//
// It is a seam rather than a dependency because the direction of trust matters: the transport knows how
// to sign a request and verify what comes back, and the room server knows what is worth asking and what
// may be kept. Neither belongs in the other.
type RemoteHistory interface {
	// EventAtTimestamp asks `server` where `roomID` was at the given moment and returns the event it
	// named. What comes back has been verified against the sender's keys: a peer's answer is a body it
	// chose to send, not evidence.
	EventAtTimestamp(ctx context.Context, server, roomID string, timestampMS int64, forward bool) (json.RawMessage, error)
	// BackfillHistory asks `server` for a window of `roomID`'s history, keeps what is new here, and
	// reports how many events that was.
	BackfillHistory(ctx context.Context, server, roomID string, limit int) (int, error)
}

// TimestampRequest asks where a room's history was at a given moment.
type TimestampRequest struct {
	RoomID string
	// Timestamp is milliseconds since the Unix epoch — the same clock an event's own
	// `origin_server_ts` is written in, which is what makes the comparison meaningful.
	Timestamp int64
	// Forward asks for the earliest event at or after the moment. Backward asks for the latest one at
	// or before it. These are different questions and, in a room whose events share a millisecond,
	// they have different answers.
	Forward bool
	Reader  RenderOptions
}

// TimestampResponse is the event found, with the stamp it actually carries: a client jumps on from
// here, and an answer whose timestamp disagrees with the event it names sends the client somewhere it
// did not ask to go.
type TimestampResponse struct {
	OriginServerTS int64  `json:"origin_server_ts"`
	EventID        string `json:"event_id"`
}

// EventAtTimestamp answers "what was happening at this moment?".
//
// Two decisions in here are the whole feature. Nothing is invented at the ends: a search that runs past
// the newest event, or back before the room existed, has no answer and says so, because clamping would
// turn "the room knows nothing about 1970" into a confident claim about its first message. And the reader
// is held to the room's history rule rather than to a rule invented for this endpoint — it is a read of
// history, so `/messages` and this cannot disagree about who may see the room's past.
func (s *Service) EventAtTimestamp(ctx context.Context, req TimestampRequest) (*TimestampResponse, error) {
	if err := s.checkReadable(ctx, req.RoomID, req.Reader); err != nil {
		// The same answer `/messages` gives a stranger, for the same reason: a 404 here is an
		// existence oracle over a room ID nobody may read.
		if apiErr, ok := matrix.AsAPIError(err); ok && apiErr.Status == http.StatusNotFound {
			return nil, matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
				"You are not permitted to read the history of this room")
		}
		return nil, err
	}
	// For a room this server does not host, ask a resident server first.
	//
	// Not because the local store is empty — it usually is not. A server that joined last week holds its
	// own join event, and a search that finds it would answer "the room was here" with a confident
	// untruth: this server's copy of a remote room's timeline is known to be incomplete, so a local hit is
	// not evidence that the local event is the *nearest* one. The server that has the room is the one that
	// can say where the room was.
	if found, err := s.fetchHistoryAtTimestamp(ctx, req); err != nil {
		// Said out loud and then set aside: a peer that cannot be reached is reason to answer from what
		// this server knows, not reason to refuse a question about the past.
		slog.Debug("asking a peer where its room was failed",
			"room_id", req.RoomID, "error", err)
	} else if !found {
		slog.Debug("a peer had nothing to say about a moment in a room it hosts",
			"room_id", req.RoomID)
	}

	// A former member's question is answered inside the stretch of history they may still read; the
	// ceiling is what keeps the answer from naming an event their own client would then be refused.
	row, err := s.store.Events.NearestByTimestamp(ctx, req.RoomID, req.Timestamp, req.Forward,
		s.readCutoff(req.Reader))
	if err != nil {
		if store.IsNotFound(err) {
			direction := "before"
			if req.Forward {
				direction = "after"
			}
			return nil, matrix.NewNotFound("this room has no event %s timestamp %d", direction, req.Timestamp)
		}
		return nil, err
	}
	return &TimestampResponse{OriginServerTS: row.OriginServerTS, EventID: row.EventID}, nil
}

// StateEventRows returns a room's current state as rows.
func (s *Service) StateEventRows(ctx context.Context, roomID string, opts RenderOptions) ([]json.RawMessage, error) {
	if err := s.checkReadable(ctx, roomID, opts); err != nil {
		return nil, err
	}
	// A former member is answered from the state as it stood when they left, because "the
	// room's current state" is a fact they no longer have a right to. Rebuilding it is cheap
	// enough to do here rather than to store: the events are the history.
	rows, err := s.stateRowsFor(ctx, roomID, opts)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []json.RawMessage{}, nil
	}
	out := make([]json.RawMessage, 0, len(rows))
	for i := range rows {
		event, renderErr := s.Render(ctx, &rows[i], opts)
		if renderErr != nil {
			return nil, renderErr
		}
		out = append(out, event)
	}
	return out, nil
}

// stateRowsFor returns the room's state for one reader: the live state for a member, and the
// snapshot at their leave for a former member. Both come back ordered by (type, state_key), so
// two identical rooms produce byte-identical responses and a client comparing two syncs does not
// see a state "change" that is only reordering.
func (s *Service) stateRowsFor(ctx context.Context, roomID string, opts RenderOptions) ([]store.EventRow, error) {
	if cutoff := s.readCutoff(opts); cutoff > 0 {
		return s.store.Events.StateSnapshot(ctx, roomID, cutoff)
	}
	state, err := s.store.State.All(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if len(state) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(state))
	for _, entry := range state {
		ids = append(ids, entry.EventID)
	}
	rows, err := s.store.Events.ByEventIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.EventRow, len(rows))
	for _, row := range rows {
		byID[row.EventID] = row
	}
	ordered := make([]store.EventRow, 0, len(state))
	for _, entry := range state {
		if row, ok := byID[entry.EventID]; ok {
			ordered = append(ordered, row)
		}
	}
	return ordered, nil
}

// StateEventRow returns one state tuple, or [store.ErrNotFound]-shaped Matrix error.
func (s *Service) StateEventRow(ctx context.Context, roomID, eventType, stateKey string, opts RenderOptions) (json.RawMessage, error) {
	if err := s.checkReadable(ctx, roomID, opts); err != nil {
		return nil, err
	}
	// A former member is shown what the room said at the time, not what it says now: the
	// current value of a state key can be written entirely after they left, and handing it to
	// them would sell back the one thing leaving is supposed to stop.
	if cutoff := s.readCutoff(opts); cutoff > 0 {
		row, err := s.store.Events.StateAt(ctx, roomID, eventType, stateKey, cutoff)
		if err != nil {
			if store.IsNotFound(err) {
				return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
					"This room has no %q event", eventType)
			}
			return nil, err
		}
		return s.Render(ctx, row, opts)
	}
	entry, err := s.store.State.Get(ctx, roomID, eventType, stateKey)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"This room has no %q event", eventType)
		}
		return nil, err
	}
	row, err := s.store.Events.ByEventID(ctx, entry.EventID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"This room has no %q event", eventType)
		}
		return nil, err
	}
	return s.Render(ctx, row, opts)
}

// RoomPDUs returns a room's current state as parsed events, for services that reason about
// state rather than serve it.
func (s *Service) RoomPDUs(ctx context.Context, roomID string) ([]gomatrixserverlib.PDU, error) {
	return s.stateAt(ctx, roomID)
}

// Pagination directions.
const (
	DirectionForward  = "f"
	DirectionBackward = "b"
)

// errBadStreamToken is the sentinel for an unparsable pagination token.
var errBadStreamToken = errors.New("roomserver: unparsable stream token")

// Filter values of /messages.
const (
	FilterRecent = "recent"
	FilterFuzzy  = "fuzzy"
	FilterAll    = ""
)

// MessagesRequest is a GET /messages request.
type MessagesRequest struct {
	RoomID    string
	From      string
	To        string
	Direction string
	Limit     int
	Filter    string
	// LazyLoadMembers asks for the state section to be trimmed to the member events needed to
	// render the chunk, which is MSC1337 and what clients send.
	LazyLoadMembers bool
	// ContainsURL keeps only the events whose content carries a URL (or, when false, only those
	// that do not). Nil is a third state meaning "the caller did not ask", which is every event.
	ContainsURL *bool
	Reader      RenderOptions
}

// MessagesResponse is the response body.
type MessagesResponse struct {
	Start string            `json:"start"`
	Chunk []json.RawMessage `json:"chunk"`
	// End is the token to continue paging with, omitted when the end of the readable
	// timeline has been reached — which is how a client knows to stop.
	End string `json:"end,omitempty"`
	// State is the room state at the start of the chunk, which is what lets a client
	// render the events it is about to show without a second round trip.
	State []json.RawMessage `json:"state,omitempty"`
}

// Messages paginates a room's timeline.
//
// Tokens are the literal stream positions, which is an implementation detail the
// specification leaves open ("opaque strings") and which pays for itself: a token can be
// compared, stored and validated without a lookup table that can drift from the events it
// refers to.
func (s *Service) Messages(ctx context.Context, req MessagesRequest) (*MessagesResponse, error) {
	if err := s.checkReadable(ctx, req.RoomID, req.Reader); err != nil {
		// A room this server has never heard of is answered as "you may not read this", not
		// as "no such room". /messages is served to a caller who is presumed to be a member,
		// and the spec's own wording for this endpoint is M_FORBIDDEN; more importantly, a
		// 404 is an existence oracle — it lets anybody walk a list of candidate room IDs and
		// keep the ones that answer differently, which is exactly the enumeration a private
		// room's ID is supposed to prevent.
		if apiErr, ok := matrix.AsAPIError(err); ok && apiErr.Status == http.StatusNotFound {
			return nil, matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
				"You are not permitted to read the history of this room")
		}
		return nil, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	current, err := s.store.Events.MaxPositionInRoom(ctx, req.RoomID)
	if err != nil {
		return nil, err
	}

	from, err := parseStreamToken(req.From)
	if err != nil {
		return nil, err
	}
	// `from` is optional, and the specification says a request without one starts from the
	// current state of the room. Reading an absent token as position zero would be a silently
	// different question — "what happened before anything" — and a client that simply asks for
	// the latest page gets an empty answer and a reason to think the room is empty.
	if req.From == "" || from < 0 {
		from = current
	}
	to, err := parseStreamToken(req.To)
	if err != nil {
		return nil, err
	}

	// A former member reads the room as it was when they left, so the range they can ask for
	// ends there too. Without this, a departed member could page forward from before their
	// leave into events written after it — the per-event check in [Service.visibleTo] hides
	// them from the chunk, but the response's `end` token and the paging loop would still walk
	// through private history, and a room's `from`/`end` pair is a capability the client is
	// expected to be able to hand to a friend.
	if cutoff := s.readCutoff(req.Reader); cutoff > 0 {
		if from > cutoff || from <= 0 {
			from = cutoff
		}
		if to > cutoff || to == 0 {
			to = cutoff
		}
	}

	// `dir` defaults to backward, which is what "give me the end of the room" means: a client
	// with no token wants the newest events, and the oldest-first ordering of the chunk is how a
	// timeline is displayed.
	direction := req.Direction
	if direction == "" {
		direction = DirectionBackward
	}
	// Detached history — what this server fetched from a peer rather than received in the course of
	// federation — is paged like any other history, because that is the only reason it was fetched. It is
	// /sync and the unread badges that exclude it, where the question is what the room has done, not what
	// the room remembers.
	query := store.EventQuery{RoomIDs: []string{req.RoomID}, Limit: limit, IncludeDetached: true}
	switch direction {
	case DirectionBackward:
		// A backward read includes the event named by `from`. The token a client has is a
		// sync token, and a sync token names the newest event the client already holds — so a
		// client asking to page backward from where it stands is asking for a page that starts
		// with that event. Excluding it loses one event per page, and the client has no way to
		// notice: the history simply has a hole where its own last message was.
		query.Before = from + 1
		query.OrderDesc = true
	case DirectionForward:
		query.After = from
		if to > 0 {
			query.Before = to + 1
		}
	default:
		return nil, matrix.NewInvalidParam("dir must be %q or %q", DirectionForward, DirectionBackward)
	}

	if req.Filter == FilterRecent && req.Reader.MembershipStreamPos > 0 {
		// "recent" is defined relative to when the reader joined, which the membership row
		// records as a stream position.
		if query.After < req.Reader.MembershipStreamPos {
			query.After = req.Reader.MembershipStreamPos
		}
		query.Before = 0
		query.OrderDesc = false
	}

	rows, err := s.store.Events.Range(ctx, query)
	if err != nil {
		return nil, err
	}

	// The `contains_url` filter runs on the way out, before the page is cut, for the same reason
	// the sync filters do: a client that asked for the five newest files wants five files, not
	// whichever five events happened to be newest. Reading a wider window is the cost, bounded so
	// that a filter cannot turn one page request into a scan of a year of history.
	if req.ContainsURL != nil {
		query.Limit = 0
		rows, err = s.store.Events.Range(ctx, boundedEventQuery(query))
		if err != nil {
			return nil, err
		}
		matching := rows[:0]
		for _, row := range rows {
			if eventHasURL(row) == *req.ContainsURL {
				matching = append(matching, row)
			}
		}
		rows = matching
		if len(rows) > limit {
			// Still in query order, so this keeps the near end: newest for a backward read,
			// oldest for a forward one.
			rows = rows[:limit]
		}
	}

	// A backward read comes back newest-first and is answered oldest-first, matching the
	// direction the client is walking.
	if query.OrderDesc {
		for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
			rows[left], rows[right] = rows[right], rows[left]
		}
	}

	// The pair of tokens a client uses to continue paging. A read that found nothing still
	// has to answer with a usable pair, because a client paging through an empty range
	// relies on `end` being where it left off — otherwise it loops forever.
	// `end` is the token to pass as `from` to continue, and it is omitted once there is
	// nowhere left to go — the specification makes the omission the signal that pagination is
	// finished, and a client that is always given a token keeps asking for a page it has
	// already seen. A backward page therefore ends one *below* its oldest event, so the next
	// page starts below it and no event is served twice.
	response := &MessagesResponse{
		Start: formatStreamToken(from),
		Chunk: []json.RawMessage{},
	}
	switch direction {
	case DirectionBackward:
		earliest, err := s.store.Events.MinPositionInRoom(ctx, req.RoomID)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			if rows[0].ID > earliest {
				response.End = formatStreamToken(rows[0].ID - 1)
			}
		}
	case DirectionForward:
		if len(rows) > 0 {
			if rows[len(rows)-1].ID < current {
				response.End = formatStreamToken(rows[len(rows)-1].ID)
			}
		}
	}
	for i := range rows {
		event, err := s.Render(ctx, &rows[i], req.Reader)
		if err != nil {
			return nil, err
		}
		response.Chunk = append(response.Chunk, event)
	}
	// The state of the room as it stood at the far end of the chunk. For a backward read
	// that is the oldest event returned, which is the state the earliest returned event was
	// created against.
	stateRows, err := s.stateRowsFor(ctx, req.RoomID, req.Reader)
	if err != nil {
		return nil, err
	}
	if req.LazyLoadMembers {
		// A client that asked to load members lazily is saying it already has the member
		// events it needs and only wants the ones it cannot render the chunk without. For a
		// room with a thousand members that is the difference between a page of messages and
		// a page of state: every event in the chunk was sent by somebody, and those few
		// member events are all the response has to carry.
		stateRows = membersOfSenders(stateRows, rows)
	}
	for i := range stateRows {
		event, err := s.Render(ctx, &stateRows[i], req.Reader)
		if err != nil {
			return nil, err
		}
		response.State = append(response.State, event)
	}
	return response, nil
}

// membersOfSenders trims a state list to the `m.room.member` events of the devices and users that
// wrote the given events, which is everything a client needs to attribute the chunk.
func membersOfSenders(stateRows []store.EventRow, events []store.EventRow) []store.EventRow {
	senders := make(map[string]bool, len(events))
	for _, row := range events {
		senders[row.Sender] = true
	}
	out := make([]store.EventRow, 0, len(senders))
	for _, row := range stateRows {
		if row.Type != matrix.EventTypeMember || row.StateKey == nil {
			continue
		}
		if senders[*row.StateKey] {
			out = append(out, row)
		}
	}
	return out
}

// parseStreamToken reads a pagination or sync token.
//
// Both "s123" and "123" are accepted, and an unparsable token is [matrix.ErrUnknownToken]
// for /sync (the spec's answer for a bad `since`) — callers map it.
func parseStreamToken(token string) (int64, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(token, "s"))
	if trimmed == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		// The error value is a sentinel: /messages wants M_INVALID_PARAM for a bad token
		// while /sync must answer the spec's 401 M_UNKNOWN_TOKEN, so the caller maps it.
		return 0, errBadStreamToken
	}
	return value, nil
}

// formatStreamToken writes a pagination token.
func formatStreamToken(position int64) string {
	return "s" + strconv.FormatInt(position, 10)
}

// JoinedRooms lists the rooms a user is in, for /joined_rooms and /sync's room list.
func (s *Service) JoinedRooms(ctx context.Context, localpart string) ([]string, error) {
	rooms, err := s.store.Memberships.RoomsForUser(ctx, localpart,
		matrix.MembershipJoin, matrix.MembershipInvite, matrix.MembershipLeave,
		matrix.MembershipBan, matrix.MembershipKnock)
	if err != nil {
		return nil, err
	}
	return rooms, nil
}

// MembersRequest is a GET /rooms/{roomID}/members or /joined_members request.
type MembersRequest struct {
	RoomID string
	// NotMembership excludes memberships (comma-separated in the query string).
	NotMembership []string
	// Membership restricts to a single membership value.
	Membership string
	// FuzzyMemberDataFields keeps fields the client did not ask to filter on; it changes
	// only which fields are returned, not which members.
	FuzzyMemberDataFields bool
	EventFormat           string
	// Terse asks for trimmed member events (content reduced to the membership). It is off
	// unless a client names the extension, because a client that did not ask for trimmed
	// events is a client that wants the display name and anything else the member put in
	// its own event content.
	Terse  bool
	Reader RenderOptions
}

// MemberEntry is one member of a room, in the shape /members returns.
type MemberEntry struct {
	Type           string          `json:"type"`
	StateKey       string          `json:"state_key"`
	Content        json.RawMessage `json:"content"`
	Sender         string          `json:"sender"`
	EventID        string          `json:"event_id,omitempty"`
	OriginServerTS int64           `json:"origin_server_ts,omitempty"`
	Unsigned       json.RawMessage `json:"unsigned,omitempty"`
}

// Members lists a room's members as m.room.member events, honouring the membership filters.
func (s *Service) Members(ctx context.Context, req MembersRequest) ([]json.RawMessage, error) {
	if err := s.checkReadable(ctx, req.RoomID, req.Reader); err != nil {
		return nil, err
	}
	// A former member is shown the room's membership as of the moment they left. The live
	// membership table is a current-state cache, and every change since their departure is
	// information they gave up the right to — including who joined afterwards, which is the
	// case a sytest here pins by name.
	if cutoff := s.readCutoff(req.Reader); cutoff > 0 {
		return s.membersAt(ctx, req, cutoff)
	}
	memberships := []string{}
	if req.Membership != "" {
		memberships = append(memberships, req.Membership)
	}
	rows, err := s.store.Memberships.List(ctx, req.RoomID, memberships, req.NotMembership)
	if err != nil {
		return nil, err
	}

	out := make([]json.RawMessage, 0, len(rows))
	for _, membership := range rows {
		entry, err := s.store.State.Get(ctx, req.RoomID, matrix.EventTypeMember,
			memberStateKey(membership.Localpart, s.cfg.ServerName))
		if err != nil {
			// A membership row without a member event is a cache entry whose event has been
			// forgotten; skipping it is correct because the event is the truth.
			continue
		}
		eventRow, err := s.store.Events.ByEventID(ctx, entry.EventID)
		if err != nil {
			continue
		}
		event, err := s.Render(ctx, eventRow, req.Reader)
		if err != nil {
			return nil, err
		}
		if req.Terse {
			event = terseEvent(event)
		}
		out = append(out, event)
	}
	return out, nil
}

// terseEvent is the trimmed member event an opt-in /members request asks for.
//
// Trimming is a bandwidth optimisation the specification permits and no client asks for, so
// [MembersRequest.Terse] defaults to false and a client has to name the extension. The
// trimmed form keeps type and sender as well as the membership: those three are what make the
// row identifiable as a member event, and a "trimmed" event a client cannot attribute is
// simply a broken event. Every other content key — a display name, and anything a client put
// there itself — is dropped, which is the whole point and the whole cost of trimming.
func terseEvent(event json.RawMessage) json.RawMessage {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(event, &document); err != nil {
		return event
	}
	terse := map[string]json.RawMessage{}
	for _, key := range []string{"type", "sender", "room_id"} {
		if value, ok := document[key]; ok {
			terse[key] = value
		}
	}
	if content, ok := document["content"]; ok {
		var contentFields map[string]json.RawMessage
		if err := json.Unmarshal(content, &contentFields); err == nil {
			trimmed := map[string]json.RawMessage{}
			if membership, ok := contentFields["membership"]; ok {
				trimmed["membership"] = membership
			}
			if encoded, err := json.Marshal(trimmed); err == nil {
				terse["content"] = encoded
			}
		}
	}
	if stateKey, ok := document["state_key"]; ok {
		terse["state_key"] = stateKey
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	// Escaping off: this document embeds the event's own content bytes, and re-escaping
	// them would change the strings the client is comparing against.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(terse); err != nil {
		return event
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

// AliasesOfRoom returns the room's aliases.
func (s *Service) AliasesOfRoom(ctx context.Context, roomID string) ([]string, error) {
	aliases, err := s.store.Aliases.ListByRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		out = append(out, alias.Alias)
	}
	return out, nil
}

// ResolveAlias resolves a local alias to a room ID.
func (s *Service) ResolveAlias(ctx context.Context, alias string) (string, error) {
	entry, err := s.store.Aliases.Get(ctx, alias)
	if err != nil {
		if store.IsNotFound(err) {
			return "", matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Alias %q does not exist on this homeserver", alias)
		}
		return "", err
	}
	return entry.RoomID, nil
}

// CreateAlias points a new alias at a room.
func (s *Service) CreateAlias(ctx context.Context, alias, roomID, creatorLocalpart string) error {
	if _, err := s.store.Rooms.Get(ctx, roomID); err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", roomID)
		}
		return err
	}
	err := s.store.Aliases.Create(ctx, &store.RoomAlias{
		Alias:            alias,
		RoomID:           roomID,
		CreatorLocalpart: creatorLocalpart,
	})
	if store.IsAlreadyExists(err) {
		return matrix.NewAPIError(http.StatusConflict, matrix.ErrRoomInUse,
			"Alias %q is already in use", alias)
	}
	if err != nil {
		return err
	}
	s.notifier.Publish()
	return nil
}

// DeleteAlias removes an alias. Only the room's creator or an admin of the room may do so,
// which is what the room's power levels say rather than what this server guesses.
func (s *Service) DeleteAlias(ctx context.Context, alias, localpart string) error {
	entry, err := s.store.Aliases.Get(ctx, alias)
	if err != nil {
		if store.IsNotFound(err) {
			// The specification answers 404 M_NOT_FOUND: a caller deleting an alias it
			// believed existed is wrong about the room's addresses, and silence would hide
			// that from it.
			return matrix.NewNotFound("Alias %q was not found", alias)
		}
		return err
	}
	if entry.CreatorLocalpart != localpart {
		allowed, err := s.hasPower(ctx, entry.RoomID, localpart, 50)
		if err != nil {
			return err
		}
		if !allowed {
			return matrix.NewForbidden("Only the creator or a room admin can delete this alias")
		}
	}
	if err := s.store.Aliases.Delete(ctx, alias); err != nil {
		return err
	}
	if err := s.clearCanonicalAlias(ctx, entry.RoomID, localpart, alias); err != nil {
		// The alias is gone either way; failing the whole call here would leave the caller
		// believing nothing happened and re-adding an alias that is already deleted.
		slog.Warn("roomserver: could not clear the canonical alias after deleting it",
			"room_id", entry.RoomID, "alias", alias, "error", err)
	}
	s.notifier.Publish()
	return nil
}

// clearCanonicalAlias removes a deleted alias from the room's own claim about what it is
// called.
//
// The alias is already gone from the directory, so a room whose m.room.canonical_alias still
// names it points at nothing: clients keep showing a link that resolves to a 404, and the only
// thing that can fix the claim is an event, because that is what the claim is. When the deleted
// alias was the whole claim the content becomes an empty object — the room saying it has no
// preferred name — which is also what clients watch for to drop a name they are showing.
func (s *Service) clearCanonicalAlias(ctx context.Context, roomID, localpart, alias string) error {
	entry, err := s.store.State.Get(ctx, roomID, matrix.EventTypeCanonicalAlias, "")
	if err != nil {
		if store.IsNotFound(err) {
			return nil // the room never claimed a name
		}
		return err
	}
	row, err := s.store.Events.ByEventID(ctx, entry.EventID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	var document struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(row.JSON), &document); err != nil {
		return nil
	}
	var claim struct {
		Alias      string   `json:"alias"`
		AltAliases []string `json:"alt_aliases"`
	}
	if err := json.Unmarshal(document.Content, &claim); err != nil {
		return nil
	}
	if claim.Alias != alias && !containsString(claim.AltAliases, alias) {
		return nil // this alias was not part of the claim
	}

	remaining := map[string]any{}
	if claim.Alias != "" && claim.Alias != alias {
		remaining["alias"] = claim.Alias
	}
	var kept []string
	for _, candidate := range claim.AltAliases {
		if candidate != alias {
			kept = append(kept, candidate)
		}
	}
	if len(kept) > 0 {
		remaining["alt_aliases"] = kept
	}

	content, err := json.Marshal(remaining)
	if err != nil {
		return err
	}
	_, err = s.SendStateEvent(ctx, Sender{
		Localpart: localpart,
		UserID:    matrix.UserID(localpart, s.cfg.ServerName),
	}, roomID, matrix.EventTypeCanonicalAlias, "", content)
	return err
}

// containsString reports whether a slice holds a value.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// hasPower reports whether a local part meets a power level threshold in a room.
func (s *Service) hasPower(ctx context.Context, roomID, localpart string, required int64) (bool, error) {
	state, err := s.stateAt(ctx, roomID)
	if err != nil {
		return false, err
	}
	levels, err := s.events.PowerLevels(state)
	if err != nil {
		return false, err
	}
	return levels.UserLevel(spec.SenderID(matrix.UserID(localpart, s.cfg.ServerName))) >= required, nil
}

// PublicRoomsRequest is a GET /publicRooms or GET /directory/list/room request.
type PublicRoomsRequest struct {
	Limit       int
	Since       string
	Filter      string
	GuestAccess string
}

// PublicRoomsPage is the response.
type PublicRoomsPage struct {
	Chunk      []PublicRoom `json:"chunk"`
	NextBatch  string       `json:"next_batch,omitempty"`
	PrevBatch  string       `json:"prev_batch,omitempty"`
	TotalRooms int64        `json:"total_rooms,omitempty"`
}

// PublicRoom is one entry of the room directory.
type PublicRoom struct {
	RoomID         string   `json:"room_id"`
	Aliases        []string `json:"aliases,omitempty"`
	Name           string   `json:"name,omitempty"`
	Topic          string   `json:"topic,omitempty"`
	AvatarURL      string   `json:"avatar_url,omitempty"`
	CanonicalAlias string   `json:"canonical_alias,omitempty"`
	NumJoined      int64    `json:"num_joined_members"`
	// WorldReadable has no omitempty: the key is part of the room-directory contract, and a
	// client that has to handle its absence cannot tell "not world readable" apart from a
	// server that did not answer.
	WorldReadable bool   `json:"world_readable"`
	GuestCanJoin  bool   `json:"guest_can_join"`
	JoinRule      string `json:"join_rule,omitempty"`
	RoomVersion   string `json:"room_version,omitempty"`
	// RoomType is the `type` from the room's create event, "m.space" for a space and absent
	// for a conversation. The room directory once spelled this `space: true`; the current
	// specification carries the type itself, which is what a client needs in order to filter
	// on a type of its own rather than on "is this a space, yes or no".
	RoomType string `json:"room_type,omitempty"`
}

// PublicRooms lists rooms this server publishes in the room directory.
func (s *Service) PublicRooms(ctx context.Context, req PublicRoomsRequest) (*PublicRoomsPage, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rooms, err := s.store.Rooms.ListWithJoinRule(ctx,
		[]string{matrix.JoinRulePublic, matrix.JoinRuleKnock, matrix.JoinRuleKnockRestricted}, limit+1, req.Since)
	if err != nil {
		return nil, err
	}

	page := &PublicRoomsPage{Chunk: []PublicRoom{}, PrevBatch: req.Since}
	for i, room := range rooms {
		if i == limit {
			page.NextBatch = formatRoomCursor(rooms[i-1].ID)
			break
		}
		if req.Filter != "" && !strings.Contains(strings.ToLower(room.Name+" "+room.Topic+" "+room.CanonicalAlias),
			strings.ToLower(req.Filter)) {
			continue
		}
		if req.GuestAccess == "can_join" && room.GuestAccess != matrix.GuestCanJoin {
			continue
		}
		if !room.Publishable {
			// A room this server has unpublished is not listed even though its join rule
			// still lets anybody in — the two are separate decisions in the specification.
			continue
		}
		aliases, err := s.AliasesOfRoom(ctx, room.ID)
		if err != nil {
			return nil, err
		}
		entry := PublicRoom{
			RoomID:         room.ID,
			Aliases:        aliases,
			Name:           room.Name,
			Topic:          room.Topic,
			AvatarURL:      room.AvatarURL,
			CanonicalAlias: room.CanonicalAlias,
			NumJoined:      room.JoinedCount,
			GuestCanJoin:   room.GuestAccess == matrix.GuestCanJoin,
			WorldReadable:  room.HistoryVisibility == matrix.HistoryWorldReadable,
			JoinRule:       room.JoinRule,
			RoomVersion:    room.Version,
			RoomType:       room.RoomType,
		}
		page.Chunk = append(page.Chunk, entry)
	}
	return page, nil
}

// SetRoomDirectoryVisibility publishes or unpublishes a room.
func (s *Service) SetRoomDirectoryVisibility(ctx context.Context, roomID, visibility, localpart string) error {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", roomID)
		}
		return err
	}
	switch visibility {
	case matrix.DirectoryVisibilityPrivate, matrix.DirectoryVisibilityPublic, matrix.DirectoryVisibilityUnlisted:
	default:
		return matrix.NewInvalidParam("visibility must be one of %q, %q or %q",
			matrix.DirectoryVisibilityPrivate, matrix.DirectoryVisibilityPublic, matrix.DirectoryVisibilityUnlisted)
	}
	if err := s.checkRoomAdmin(ctx, room, localpart); err != nil {
		return err
	}
	listed := visibility == matrix.DirectoryVisibilityPublic
	if err := s.store.Rooms.Update(ctx, roomID, map[string]any{"publishable": listed}); err != nil {
		return err
	}
	// The alias keeps the same flag, so that resolving a room through its directory entry
	// and through the room itself agree.
	if err := s.store.Aliases.SetListedInDirectory(ctx, roomID, listed); err != nil {
		return err
	}
	s.notifier.Publish()
	return nil
}

// RoomDirectoryVisibility reads a room's directory visibility.
func (s *Service) RoomDirectoryVisibility(ctx context.Context, roomID string) (string, error) {
	aliases, err := s.store.Aliases.ListByRoom(ctx, roomID)
	if err != nil {
		return "", err
	}
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return "", matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", roomID)
		}
		return "", err
	}
	if room.Publishable {
		return matrix.DirectoryVisibilityPublic, nil
	}
	switch room.JoinRule {
	case matrix.JoinRulePublic, matrix.JoinRuleKnock, matrix.JoinRuleKnockRestricted:
		// Joinable but not advertised: the specification's third value, and the one a
		// client cannot infer from the join rule alone.
		return matrix.DirectoryVisibilityUnlisted, nil
	}
	_ = aliases
	return matrix.DirectoryVisibilityPrivate, nil
}

// checkRoomAdmin requires the caller to be the room creator or hold the admin level.
func (s *Service) checkRoomAdmin(ctx context.Context, room *store.Room, localpart string) error {
	if room.CreatorLocalpart == localpart {
		return nil
	}
	allowed, err := s.hasPower(ctx, room.ID, localpart, 50)
	if err != nil {
		return err
	}
	if !allowed {
		return matrix.NewForbidden("This action requires room admin rights")
	}
	return nil
}

// RoomSummary is the /rooms/{roomID}/summary payload.
type RoomSummary struct {
	RoomID             string   `json:"room_id"`
	Name               string   `json:"name,omitempty"`
	Topic              string   `json:"topic,omitempty"`
	AvatarURL          string   `json:"avatar_url,omitempty"`
	CanonicalAlias     string   `json:"canonical_alias,omitempty"`
	Aliases            []string `json:"aliases,omitempty"`
	NumJoined          int64    `json:"num_joined_members"`
	WorldReadable      bool     `json:"world_readable,omitempty"`
	GuestCanJoin       bool     `json:"guest_can_join,omitempty"`
	JoinRule           string   `json:"join_rule,omitempty"`
	RoomVersion        string   `json:"room_version,omitempty"`
	MembershipOf       string   `json:"membership,omitempty"`
	AllowedRoomVersion string   `json:"allowed_room_version,omitempty"`
}

// RoomSummaryV1 is the response of GET /_matrix/client/v1/room_summary/{roomIDOrAlias}.
//
// It is a different shape from [RoomSummary] — that one answers /rooms/{id}/hero+summary, which is a
// member-counting endpoint with a legacy shape — and the two are not interchangeable. This one exists
// so a client can decide *before* attempting a join what joining would require.
type RoomSummaryV1 struct {
	RoomID         string `json:"room_id"`
	Name           string `json:"name,omitempty"`
	Topic          string `json:"topic,omitempty"`
	AvatarURL      string `json:"avatar_url,omitempty"`
	CanonicalAlias string `json:"canonical_alias,omitempty"`
	RoomType       string `json:"room_type,omitempty"`
	RoomVersion    string `json:"room_version,omitempty"`
	NumJoined      int64  `json:"num_joined_members"`
	WorldReadable  bool   `json:"world_readable"`
	GuestCanJoin   bool   `json:"guest_can_join"`
	JoinRule       string `json:"join_rule,omitempty"`
	// AllowedRoomIDs is present only for a restricted room. Omitting it for an unrestricted room is
	// not the same statement as an empty list: one says "not restricted", the other "restricted by
	// nobody", and a client that offered a join on the second reading would be wrong.
	AllowedRoomIDs []string `json:"allowed_room_ids,omitempty"`
	Predecessor    string   `json:"predecessor,omitempty"`
	Successor      string   `json:"successor,omitempty"`
}

// RoomSummaryV1 describes a room for the room-summary endpoint, resolving an alias if the caller sent
// one: the path parameter is a room ID *or alias*, and a client should not have to know which it
// happens to hold.
func (s *Service) RoomSummaryV1(ctx context.Context, roomIDOrAlias string, opts RenderOptions) (*RoomSummaryV1, error) {
	roomID := roomIDOrAlias
	if strings.HasPrefix(roomID, "#") {
		resolved, err := s.ResolveAlias(ctx, roomID)
		if err != nil {
			return nil, err
		}
		roomID = resolved
	}
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", roomID)
		}
		return nil, err
	}
	// Permission is the same question every read of this room asks: a summary of a room the caller
	// cannot see is a way of reading its name, its member count and its join rules.
	if err := s.checkReadable(ctx, roomID, opts); err != nil {
		return nil, err
	}

	summary := &RoomSummaryV1{
		RoomID:         room.ID,
		Name:           room.Name,
		Topic:          room.Topic,
		AvatarURL:      room.AvatarURL,
		CanonicalAlias: room.CanonicalAlias,
		RoomType:       room.RoomType,
		RoomVersion:    room.Version,
		NumJoined:      room.JoinedCount,
		WorldReadable:  room.HistoryVisibility == matrix.HistoryWorldReadable,
		GuestCanJoin:   room.GuestAccess == matrix.GuestCanJoin,
		JoinRule:       room.JoinRule,
		Predecessor:    room.PredecessorRoomID,
		Successor:      room.UpgradedToRoomID,
	}
	if room.JoinRule == matrix.JoinRuleRestricted || room.JoinRule == matrix.JoinRuleKnockRestricted {
		// The rooms a restricted join admits are in the join-rules event, not in the cached row: the
		// cache holds the rule's name, which is enough to know a room is restricted and not enough to
		// say which of the caller's own memberships would satisfy it.
		allowed, err := s.allowedRoomIDs(ctx, roomID, opts)
		if err != nil {
			return nil, err
		}
		summary.AllowedRoomIDs = allowed
	}
	return summary, nil
}

// allowedRoomIDs reads the rooms a restricted join admits from the room's join-rules event.
func (s *Service) allowedRoomIDs(ctx context.Context, roomID string, opts RenderOptions) ([]string, error) {
	content, err := s.StateEventRow(ctx, roomID, matrix.EventTypeJoinRules, "", opts)
	if err != nil {
		// A room whose join rules cannot be read is not going to report any admitted rooms, and that is
		// an answer rather than a failure: the caller asked which rooms a restricted join would admit,
		// and "none that this server can name" is what it can be told. The reason goes to the log,
		// because a room that *should* be restricted and is not is an administration bug worth finding.
		slog.Warn("roomserver: could not read the join rules of a restricted room",
			"room_id", roomID, "error", err)
		return nil, nil
	}
	return allowedRoomsFromJoinRuleContent(roomID, content)
}

// allowedRoomIDsForAuth answers the same question as [Service.allowedRoomIDs] for the server's own
// use, reading the room's join-rules event with this server's authority instead of the caller's.
//
// The distinction is not a shortcut around permission. Deciding whether a restricted room is one a
// caller could join is an authorization question, and asking it as a client guarantees a 403 in
// exactly the case that matters: a restricted room whose history the caller cannot read, which is
// what a restricted room looks like to somebody who is not in it yet. Asked that way the answer is
// always "not joinable", so the room disappears from the space walk for the very people its allow
// entry was written to admit.
//
// Nothing is disclosed by the answer. A room the caller may not see is still not shown to them; all
// they learn is that a room they could reach is one they could join.
func (s *Service) allowedRoomIDsForAuth(ctx context.Context, roomID string) []string {
	entry, err := s.store.State.Get(ctx, roomID, matrix.EventTypeJoinRules, "")
	if err != nil {
		return nil
	}
	row, err := s.store.Events.ByEventID(ctx, entry.EventID)
	if err != nil {
		return nil
	}
	allowed, err := allowedRoomsFromJoinRuleContent(roomID, json.RawMessage(row.JSON))
	if err != nil {
		slog.Warn("roomserver: could not read the join rules of a restricted room for authorization",
			"room_id", roomID, "error", err)
		return nil
	}
	return allowed
}

// allowedRoomsFromJoinRuleContent picks the admitted rooms out of a join-rules event.
//
// It accepts a whole event or a bare content object because both reach it: the client-facing read
// path returns events, and a caller holding a content object should get the same answer rather than
// an empty one.
func allowedRoomsFromJoinRuleContent(roomID string, content json.RawMessage) ([]string, error) {
	var wrapper struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(content, &wrapper); err != nil {
		return nil, fmt.Errorf("roomserver: the join rules of %s are unreadable: %w", roomID, err)
	}
	body := content
	if len(wrapper.Content) > 0 {
		body = wrapper.Content
	}
	var rules struct {
		Allow []struct {
			Type   string `json:"type"`
			RoomID string `json:"room_id"`
		} `json:"allow"`
	}
	if err := json.Unmarshal(body, &rules); err != nil {
		return nil, fmt.Errorf("roomserver: the join rules of %s are unreadable: %w", roomID, err)
	}
	// Room IDs are kept in the order the event lists them, with duplicates removed: the order is the
	// room administrator's, and a client that displays them has no better ordering to use.
	seen := map[string]bool{}
	out := []string{}
	for _, entry := range rules.Allow {
		// An entry naming a *space* rather than a room admits that space's members; MSC3083 spells
		// both as `room_id`, and the value is a room ID either way.
		if entry.RoomID == "" || seen[entry.RoomID] {
			continue
		}
		seen[entry.RoomID] = true
		out = append(out, entry.RoomID)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Summary describes a room for /summary and for the room hierarchy.
func (s *Service) Summary(ctx context.Context, roomID string, opts RenderOptions) (*RoomSummary, error) {
	if err := s.checkReadable(ctx, roomID, opts); err != nil {
		return nil, err
	}
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", roomID)
		}
		return nil, err
	}
	aliases, err := s.AliasesOfRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	return &RoomSummary{
		RoomID:             room.ID,
		Name:               room.Name,
		Topic:              room.Topic,
		AvatarURL:          room.AvatarURL,
		CanonicalAlias:     room.CanonicalAlias,
		Aliases:            aliases,
		NumJoined:          room.JoinedCount,
		WorldReadable:      room.HistoryVisibility == matrix.HistoryWorldReadable,
		GuestCanJoin:       room.GuestAccess == matrix.GuestCanJoin,
		JoinRule:           room.JoinRule,
		RoomVersion:        room.Version,
		MembershipOf:       opts.Membership,
		AllowedRoomVersion: room.Version,
	}, nil
}

// formatRoomCursor is the paging cursor of a list ordered by room ID.
func formatRoomCursor(roomID string) string { return roomID }

// membersAt answers /members for a reader whose access ended at a known position, from the
// room's state at that moment.
func (s *Service) membersAt(ctx context.Context, req MembersRequest, cutoff int64) ([]json.RawMessage, error) {
	snapshot, err := s.store.Events.StateSnapshot(ctx, req.RoomID, cutoff)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(snapshot))
	for i := range snapshot {
		row := snapshot[i]
		if row.Type != matrix.EventTypeMember {
			continue
		}
		var content struct {
			Membership string `json:"membership"`
		}
		if err := json.Unmarshal([]byte(row.JSON), &content); err != nil {
			continue
		}
		if req.Membership != "" && content.Membership != req.Membership {
			continue
		}
		if !matchesNotMembership(req.NotMembership, content.Membership) {
			continue
		}
		event, err := s.Render(ctx, &row, req.Reader)
		if err != nil {
			return nil, err
		}
		if req.Terse {
			event = terseEvent(event)
		}
		out = append(out, event)
	}
	return out, nil
}

// matchesNotMembership reports whether a membership value survives a not_membership filter.
func matchesNotMembership(notMemberships []string, membership string) bool {
	for _, excluded := range notMemberships {
		if excluded == membership {
			return false
		}
	}
	return true
}

// RelationsRequest is a request for the relations of one event.
type RelationsRequest struct {
	RoomID  string
	EventID string
	// RelType restricts the answer to one kind of relation (m.thread, m.replace, an annotation
	// marker). Empty means every kind.
	RelType string
	// EventType restricts the answer to one event type. `*` and empty both mean "any".
	EventType string
	From      string
	To        string
	Direction string
	Limit     int
	Reader    RenderOptions
}

// RelationsResponse is the response body.
type RelationsResponse struct {
	Chunk []json.RawMessage `json:"chunk"`
	// NextBatch is the token to continue with, omitted once the end has been reached — the same
	// contract as /messages, and the same reason: a client that is always handed a token never
	// stops asking.
	NextBatch string          `json:"next_batch,omitempty"`
	PrevBatch string          `json:"prev_batch,omitempty"`
	Original  json.RawMessage `json:"original_event,omitempty"`
}

// Relations answers GET /rooms/{roomID}/relations/{eventID}.
//
// A relation is a claim an event makes about another event — a reply, an edit, a reaction — and this
// endpoint is the only way to gather them, because a thread is not a container: it is whatever a
// bunch of events happened to point at. Two consequences shape the implementation. The target event
// may have been redacted, or may never have existed here at all, and neither is a reason to answer
// that the relations do not exist: an edit of a redacted message is still an edit people can read.
// And the caller sees only the relations it is allowed to see, which means a reply from a room it
// cannot read is not answerable at all.
func (s *Service) Relations(ctx context.Context, req RelationsRequest) (*RelationsResponse, error) {
	if err := s.checkReadable(ctx, req.RoomID, req.Reader); err != nil {
		if apiErr, ok := matrix.AsAPIError(err); ok && apiErr.Status == http.StatusNotFound {
			// As with /messages, an unknown room is not an existence oracle.
			return nil, matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
				"You are not permitted to read the relations of this room")
		}
		return nil, err
	}
	if req.EventID == "" {
		return nil, matrix.NewInvalidParam("event ID is required")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 100 {
		limit = 100
	}

	direction := req.Direction
	if direction == "" {
		// Backward by default, matching /messages: a client asking for the relations of an event
		// almost always wants the newest replies first, which is the order a thread is read in.
		direction = DirectionBackward
	}
	if direction != DirectionBackward && direction != DirectionForward {
		return nil, matrix.NewInvalidParam("dir must be %q or %q", DirectionForward, DirectionBackward)
	}

	from, err := parseStreamToken(req.From)
	if err != nil {
		return nil, err
	}
	// An absent `from` means "wherever this direction starts": the end of the timeline reading
	// backwards, the beginning reading forwards. Defaulting both to the same place would make one
	// of them wrong — a backward read from position zero is a request for events before anything
	// happened, and a forward read from the tip is a request for events that have not been sent yet.
	if req.From == "" || from < 0 {
		from = 0
		if direction == DirectionBackward {
			from, err = s.store.Events.MaxPositionInRoom(ctx, req.RoomID)
			if err != nil {
				return nil, err
			}
		}
	}
	to, err := parseStreamToken(req.To)
	if err != nil {
		return nil, err
	}

	// One extra row is fetched to find out whether anything is left after this page. Asking the
	// caller to make an extra round trip to discover it has reached the end is how clients end up
	// showing a "load more" button that loads nothing.
	query := store.EventQuery{
		RoomIDs:          []string{req.RoomID},
		RelatesToEventID: req.EventID,
		RelType:          req.RelType,
		Limit:            limit + 1,
	}
	if req.EventType != "" && req.EventType != "*" {
		query.Types = []string{req.EventType}
	}
	if direction == DirectionBackward {
		query.Before = from + 1
		query.OrderDesc = true
	} else {
		query.After = from
		if to > 0 {
			query.Before = to + 1
		}
	}

	rows, err := s.store.Events.Range(ctx, query)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	if direction == DirectionBackward {
		// The chunk is answered oldest-first regardless of the direction walked, so that a client
		// can append it to a thread without reversing it itself.
		for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
			rows[left], rows[right] = rows[right], rows[left]
		}
	}

	// A relation the caller may not see is dropped rather than answered: the same visibility rule
	// that governs /messages governs this, or a thread becomes a side channel for leaking events a
	// reader was not allowed to read directly.
	visible := make([]store.EventRow, 0, len(rows))
	for _, row := range rows {
		if s.visibleTo(ctx, req.RoomID, &row, req.Reader) {
			visible = append(visible, row)
		}
	}

	events := make([]json.RawMessage, 0, len(visible))
	for i := range visible {
		event, err := s.Render(ctx, &visible[i], req.Reader)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	response := &RelationsResponse{Chunk: events}
	if hasMore && len(rows) > 0 {
		// The continuation token is the edge of the timeline this page touched, one step beyond
		// it, so the next page continues from there without serving an event twice. The chunk is
		// answered oldest-first in both directions, so which end is "the edge" depends on the
		// direction walked, not on the order of the list.
		edge := rows[len(rows)-1].ID
		if direction == DirectionBackward {
			edge = rows[0].ID - 1
		}
		response.NextBatch = formatStreamToken(edge)
		response.PrevBatch = formatStreamToken(from)
	}
	return response, nil
}

// maxFilteredEventScan bounds the rows read in order to answer a content filter.
//
// A filter that is evaluated after the database has answered has to read more than a page to fill
// one. The bound exists because a room with a year of history would otherwise be read in full to
// answer "the five newest images", which is a way for one client to make the server do work
// proportional to everything it has ever stored.
const maxFilteredEventScan = 1000

// boundedEventQuery returns a copy of a query capped at [maxFilteredEventScan] rows.
func boundedEventQuery(query store.EventQuery) store.EventQuery {
	query.Limit = maxFilteredEventScan
	return query
}

// eventHasURL reports whether an event's content carries something to fetch.
//
// Both spellings count: `url` for a file or image, and the `file` object of an encrypted media
// event, whose URL is a JWK rather than a URI. A client that lists "things I can open" asks for
// either, and an encrypted attachment is not less of an attachment for being unreadable here.
func eventHasURL(row store.EventRow) bool {
	// The stored JSON is the whole event, so the URLs live inside `content`.
	var event struct {
		Content struct {
			URL  string `json:"url"`
			File *struct {
				URL string `json:"url"`
			} `json:"file"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(row.JSON), &event); err != nil {
		return false
	}
	if event.Content.URL != "" {
		return true
	}
	return event.Content.File != nil && event.Content.File.URL != ""
}
