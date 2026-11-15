package slidingsync

// Choosing which rooms an answer describes.
//
// The MSC's rule is a conjunction: the user may see the room, the room matches at least one rule,
// and the room has either never been sent on this connection or has updates since it was last sent.
// The list-building below is that sentence, in that order, plus the two things the MSC says about
// which rooms belong in a user's list at all:
//
//   - joined, invited and knocked rooms are in it;
//   - rooms the user *left* are in it only if this connection has been told about them before —
//     otherwise a year-old leave is news forever, and a client that left one room would see it
//     drift past on every sync;
//   - rooms the user was kicked or banned from are always in it, because that is something that
//     happened to them and they may not have seen it;
//   - a ban from a room the user never joined is not, which is the one rule this server cannot read
//     off its membership table and has to ask about — see `mayBeToldAbout` below.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// candidate is a room this connection may be told about, with the facts the ordering and the
// permission checks need.
type candidate struct {
	roomID string
	// membership is the user's row for this room. It is never absent: a room with no membership
	// row is a room the user has never been in, and the MSC keeps those out of the response
	// whatever the client subscribed to.
	membership store.Membership
	// activity is the stream position of the last event received in the room, which is what rooms
	// are ordered by — deliberately not `bump_stamp`, which is a different, narrower thing.
	activity int64
}

// build assembles one answer, reporting whether it carried anything: that flag is what tells the
// long-poll whether to keep waiting.
func (s *Service) build(ctx context.Context, req *Request, conn *store.SlidingSyncConn) (*Response, bool, error) {
	// The position is read before the data, exactly as /v3/sync reads it: an event that lands
	// during this build is above the position this answer reports, so the client is told about it
	// next time rather than missing it.
	currentPos, err := s.store.Events.MaxPosition(ctx)
	if err != nil {
		return nil, false, err
	}

	tracked, err := s.store.SlidingSync.ListRooms(ctx, req.Localpart, req.DeviceID, req.ConnID)
	if err != nil {
		return nil, false, err
	}

	memberships, err := s.roomsOf(ctx, req, tracked)
	if err != nil {
		return nil, false, err
	}

	candidates, err := s.order(ctx, req, memberships, tracked)
	if err != nil {
		return nil, false, err
	}

	response := &Response{Pos: formatPos(currentPos), Lists: map[string]*ListResult{}}

	// Which rooms each rule wants, and which rules each room matches. Both directions are needed:
	// the response carries a per-room `lists` field, and the count has to be computed over the whole
	// filtered list rather than over the window.
	wanted := map[string]*roomJob{}
	order := []string{}
	claim := func(roomID string) *roomJob {
		job, ok := wanted[roomID]
		if !ok {
			job = &roomJob{roomID: roomID}
			wanted[roomID] = job
			order = append(order, roomID)
		}
		return job
	}

	// What the filters need about these rooms, read once for the request: the caller's m.direct, its
	// room tags, the rooms' create events and encryption. A request with no filters reads none of it.
	filterSet, err := s.gather(ctx, req.Localpart, candidates, filtersNeeded(req.Lists))
	if err != nil {
		return nil, false, err
	}

	for name, list := range req.Lists {
		filtered, err := s.filter(ctx, req.Localpart, candidates, list, filterSet)
		if err != nil {
			return nil, false, err
		}
		response.Lists[name] = &ListResult{Count: int64(len(filtered))}
		from, to := listBounds(list)
		for index, room := range filtered {
			if index < from || index > to {
				continue
			}
			job := claim(room.roomID)
			job.membership = room.membership
			job.lists = append(job.lists, name)
			job.widen(maxTimeline(list.TimelineLimit), selectorFor(list.RequiredState))
		}
	}

	for roomID, subscription := range req.Subscriptions {
		membership, ok := memberships[roomID]
		if !ok {
			// A room the user has never been in, or one this server has never heard of: the MSC says
			// to ignore it, and ignoring is also what keeps a subscription from being a way to read
			// a room.
			continue
		}
		job := claim(roomID)
		job.membership = membership
		job.subscribed = true
		job.widen(maxTimeline(subscription.TimelineLimit), selectorFor(subscription.RequiredState))
	}

	// Rooms are described in activity order so that a response that had to stop somewhere stopped
	// with the least interesting rooms, and so that a client reading a log of two responses sees the
	// same order twice.
	sort.SliceStable(order, func(i, j int) bool {
		return activityOf(candidates, order[i]) > activityOf(candidates, order[j])
	})

	// `bump_stamp` is the MSC's narrower notion of activity — messages rather than any event — so it
	// is its own query over the rooms about to be described, once, rather than one per room.
	bumps, err := s.store.Events.LatestActivityByRoom(ctx, order, bumpEventTypes)
	if err != nil {
		return nil, false, err
	}

	// What counts as "just happened". For a connection in progress that is the position it last
	// heard; for the first request of a connection there is no earlier moment to compare to, so
	// nothing in the answer is live — a client that has just opened the app is being given the state
	// of its world, and buzzing about all of it is the bug this rule exists to prevent.
	liveFrom := conn.Pos
	if req.Pos == "" {
		liveFrom = currentPos
	}

	renderCache := s.rooms.NewRenderCache()
	response.sentPositions = map[string]store.SlidingSyncRoom{}
	empty := true
	for _, roomID := range order {
		job := wanted[roomID]
		if job.membership.RoomID == "" {
			// Reached only by a room that matched nothing; it has no business in the answer.
			continue
		}
		// A room this connection has never been given is sent whole; one it has is sent as a delta,
		// and the MSC's expansion rules — a config the client has since outgrown — are decided while
		// reading, because whether a widening actually *found* anything is a question about the room's
		// events, not about the configs.
		previous, seen := tracked[roomID]
		job.initial = !seen
		job.tracked = previous

		result, err := s.describe(ctx, req, liveFrom, job, currentPos, bumps, renderCache)
		if err != nil {
			return nil, false, err
		}
		if result == nil {
			// Nothing to say about this room on this request. Its memory must not move: advancing it
			// would make the events this answer skipped invisible to the next one.
			continue
		}
		if response.Rooms == nil {
			response.Rooms = map[string]*RoomResult{}
		}
		response.Rooms[roomID] = result
		empty = false
		// What this answer taught the connection. It is written onto the response rather than
		// returned separately because the two are the same fact seen from two places: what the client
		// was shown, and therefore what the next answer has to diff against.
		response.sentPositions[roomID] = store.SlidingSyncRoom{
			RoomID:        roomID,
			SentPos:       currentPos,
			TimelineLimit: job.timelineLimit,
			StateConfig:   job.selector.encodes(),
		}
	}
	// The streams that are not rooms. They are read over the rooms this answer was *asked about* —
	// the window and the subscriptions, whether or not the rooms themselves changed — because a read
	// marker or a typing indicator is precisely the news that leaves a room otherwise untouched, and a
	// client cannot show either for a room it was told about three answers ago.
	extensions, hasNews, err := s.extensions(ctx, req, order, liveFrom, currentPos)
	if err != nil {
		return nil, false, err
	}
	response.Extensions = extensions
	if hasNews {
		empty = false
	}
	return response, empty, nil
}

// roomsOf gathers the rooms this connection may be told about, keyed by room ID.
//
// Forgotten rooms are read as well as kept ones, and then kept only if this connection already knows
// them: forgetting is a user asking not to be shown a room again, which is not a licence to rewrite
// an answer already given on a live connection.
func (s *Service) roomsOf(ctx context.Context, req *Request, tracked map[string]store.SlidingSyncRoom) (map[string]store.Membership, error) {
	rows, err := s.store.Memberships.ListByUser(ctx, req.Localpart)
	if err != nil {
		return nil, err
	}
	byRoom := make(map[string]store.Membership, len(rows))
	for _, row := range rows {
		byRoom[row.RoomID] = row
	}
	forgotten, err := s.store.Memberships.ListForgottenByUser(ctx, req.Localpart)
	if err != nil {
		return nil, err
	}
	for _, row := range forgotten {
		if _, known := tracked[row.RoomID]; !known {
			continue
		}
		if _, live := byRoom[row.RoomID]; !live {
			byRoom[row.RoomID] = row
		}
	}
	return byRoom, nil
}

// order turns those rooms into the sequence a list indexes into: most recent activity first.
func (s *Service) order(ctx context.Context, req *Request, memberships map[string]store.Membership, tracked map[string]store.SlidingSyncRoom) ([]candidate, error) {
	roomIDs := make([]string, 0, len(memberships))
	for roomID := range memberships {
		roomIDs = append(roomIDs, roomID)
	}
	activity, err := s.store.Events.LatestActivityByRoom(ctx, roomIDs, nil)
	if err != nil {
		return nil, err
	}
	candidates := make([]candidate, 0, len(memberships))
	for _, roomID := range roomIDs {
		membership := memberships[roomID]
		allowed, err := s.mayBeToldAbout(ctx, membership, tracked)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		position := activity[roomID]
		if position == 0 {
			// A room with no events this server has kept — an invite to a remote room, most often —
			// is ordered by when the user's own membership happened, which is the only moment in it
			// the server can point at.
			position = membership.StreamPos
		}
		candidates = append(candidates, candidate{roomID: roomID, membership: membership, activity: position})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].activity != candidates[j].activity {
			return candidates[i].activity > candidates[j].activity
		}
		return candidates[i].roomID < candidates[j].roomID
	})
	return candidates, nil
}

// mayBeToldAbout applies the MSC's rules about which rooms belong in a user's list.
func (s *Service) mayBeToldAbout(ctx context.Context, membership store.Membership, tracked map[string]store.SlidingSyncRoom) (bool, error) {
	switch membership.Membership {
	case matrix.MembershipJoin, matrix.MembershipInvite, matrix.MembershipKnock:
		return true, nil
	case matrix.MembershipBan:
		if _, seen := tracked[membership.RoomID]; seen {
			return true, nil
		}
		// "Banned from, but never joined" is the case the MSC excludes, and this server's membership
		// table holds only the current value, so the question is asked where it can be answered: of
		// the m.room.member events about this user in this room, did any say join. Bans are rare, so
		// this costs one query per banned room rather than per room.
		joined, err := s.everJoined(ctx, membership)
		if err != nil {
			return false, err
		}
		return joined, nil
	case matrix.MembershipLeave:
		if _, seen := tracked[membership.RoomID]; seen {
			return true, nil
		}
		// A kick is a leave the user did not choose, and the MSC wants those always: without it a
		// client holding a room is never told it stopped being in it, which it can only discover by
		// trying to speak.
		return membership.Sender != "" && membership.Sender != membership.UserID, nil
	default:
		return false, nil
	}
}

// everJoined reports whether a user's membership trail in a room ever said join.
func (s *Service) everJoined(ctx context.Context, membership store.Membership) (bool, error) {
	trail, err := s.store.Events.MembershipTrail(ctx, membership.RoomID, membership.UserID)
	if err != nil {
		return false, err
	}
	for _, change := range trail {
		if change.Membership == matrix.MembershipJoin {
			return true, nil
		}
	}
	return false, nil
}

// filter applies one list's filters to the candidate sequence, before the range is cut.
//
// The whole filtered sequence is kept, not just the window, because the response has to report `count`:
// the number of rooms matching the filters is what tells a client whether to widen its range.
func (s *Service) filter(ctx context.Context, localpart string, candidates []candidate, list *ListConfig, set *filterSet) ([]candidate, error) {
	if list == nil || list.Filters == nil {
		return candidates, nil
	}
	children, err := s.spaceChildren(ctx, localpart, list.Filters.Spaces)
	if err != nil {
		return nil, err
	}
	kept := make([]candidate, 0, len(candidates))
	for _, room := range candidates {
		if children != nil && !children[room.roomID] {
			continue
		}
		if !set.matches(list.Filters, room) {
			continue
		}
		kept = append(kept, room)
	}
	return kept, nil
}

// listBounds reads a list's range, defaulting to "from the start, as far as this server will go".
func listBounds(list *ListConfig) (int, int) {
	if list.Range == nil {
		return 0, defaultRangeTo
	}
	return (*list.Range)[0], (*list.Range)[1]
}

// maxTimeline reads a room config's timeline limit, distinguishing "0, no timeline" from "absent",
// which the MSC treats as the same thing: a client that sends neither asks for no timeline.
func maxTimeline(limit *int) int {
	if limit == nil || *limit < 0 {
		return 0
	}
	if *limit > maxTimelineLimit {
		return maxTimelineLimit
	}
	return *limit
}

// activityOf looks a candidate's ordering key back up, for the final sort of the answer.
func activityOf(candidates []candidate, roomID string) int64 {
	for _, room := range candidates {
		if room.roomID == roomID {
			return room.activity
		}
	}
	return 0
}

// ErrNothingToSend documents the case a caller may want to distinguish: a build that produced a legal,
// empty answer. It is not an error, and this exists only to keep the phrase in one place.
var ErrNothingToSend = fmt.Errorf("slidingsync: nothing to send")

// stub is a state entry with no content, sent when a room's state was taken away.
type stub struct {
	Type     string `json:"type"`
	StateKey string `json:"state_key"`
}

// encodeStub renders one.
func encodeStub(eventType, stateKey string) (json.RawMessage, error) {
	document, err := json.Marshal(stub{Type: eventType, StateKey: stateKey})
	if err != nil {
		return nil, fmt.Errorf("encoding the removal of %s/%s: %w", eventType, stateKey, err)
	}
	return document, nil
}
