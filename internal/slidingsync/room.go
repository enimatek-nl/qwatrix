package slidingsync

// Describing one room.
//
// A room answer is a small argument about time. Given what this connection has already been shown of
// the room, what is there that it has not seen — and of the things the client now asks for, which does
// it not already have? The three positions MSC4186 keeps apart (the answer's, the connection's, the
// room's) each answer a different part of that sentence, and the fields below are their fingerprints:
// `initial` says the room was never described, `limited` says history was left out,
// `expanded_timeline` says the client's own widening reached back past what it had seen, and
// `num_live` says how much of what is here only just happened.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// roomJob is one room's place in an answer: the rules that pulled it in, the config their union
// produced, and what this connection was last shown of it.
type roomJob struct {
	roomID     string
	membership store.Membership
	lists      []string
	subscribed bool

	initial bool
	tracked store.SlidingSyncRoom

	// timelineLimit and selector are built by widening: the MSC says a room matched by several rules
	// is served the superset of their configs, so a room in both a list and a subscription gets the
	// wider of the two timelines and the union of the two state sets.
	timelineLimit int
	selector      stateSelector
	configured    bool
}

// widen takes in another rule's room config, keeping the superset.
func (job *roomJob) widen(limit int, selector stateSelector) {
	if limit > job.timelineLimit {
		job.timelineLimit = limit
	}
	if !job.configured {
		job.selector = selector.normalised()
		job.configured = true
		return
	}
	job.selector = job.selector.merge(selector)
}

// bumpEventTypes are the events the MSC calls "proper activity", which is what `bump_stamp` reports.
// They are narrower than "any event" on purpose: a rename is not something a user wants a room pulled
// to the top of their list for, which is why the MSC is emphatic that ordering and bumping are two
// different questions.
var bumpEventTypes = []string{
	"m.room.create",
	"m.room.message",
	"m.room.encrypted",
	"m.sticker",
	"m.call.invite",
	"m.poll.start",
	"m.beacon_info",
}

// describe renders one room, or returns nil when this answer has nothing to say about it.
//
// Returning nil rather than an empty object matters: an empty room object in the response tells a
// client "replace what you have of this room with what is here", which is how a room disappears.
func (s *Service) describe(
	ctx context.Context,
	req *Request,
	liveFrom int64,
	job *roomJob,
	currentPos int64,
	bumps map[string]int64,
	cache *roomserver.RenderCache,
) (*RoomResult, error) {
	if job.membership.Membership != matrix.MembershipJoin {
		return s.describeOutsideRoom(ctx, job)
	}

	reader := roomserver.RenderOptions{
		DeviceID:            req.DeviceID,
		Localpart:           req.Localpart,
		Membership:          job.membership.Membership,
		MembershipStreamPos: job.membership.StreamPos,
		Cache:               cache,
	}

	result := &RoomResult{Lists: job.lists}
	if job.initial {
		result.Initial = boolPointer(true)
	}

	// Where this room's history starts: an unseen room starts at the beginning of the stream, a known
	// one where this connection was last left.
	from := int64(0)
	if !job.initial {
		from = job.tracked.SentPos
	}

	window, err := s.window(ctx, job, from, currentPos)
	if err != nil {
		return nil, err
	}
	if window == nil {
		// Nothing new, and no widening that reaches back to something unsent. The room's recorded
		// position must not move: advancing it would make whatever eventually happens in the room
		// look like it had already been reported.
		return nil, nil
	}

	// Membership travels with the news about the room: a client being told about a room it joined is
	// being told that it joined, and `membership` is how it knows what this room is to it.
	if job.initial || job.membership.StreamPos > from {
		result.Membership = job.membership.Membership
	}
	if stamp, ok := bumps[job.roomID]; ok && (job.initial || stamp > from) {
		value := stamp
		result.BumpStamp = &value
	}

	live := 0
	for i := range window.rows {
		// "Live" is measured against the connection's position, not the room's: events the server is
		// only now showing because the client widened a window are history no matter how new they
		// look, and a client that treated them as news would buzz about the past.
		if window.rows[i].ID > liveFrom {
			live++
		}
		event, err := s.rooms.Render(ctx, &window.rows[i], reader)
		if err != nil {
			return nil, err
		}
		result.Timeline = append(result.Timeline, event)
	}
	result.NumLive = live
	result.Limited = boolPointer(window.limited)
	result.PrevBatch = window.prevBatch
	result.ExpandedTimeline = boolPointer(window.expanded)

	state, err := s.stateFor(ctx, job, window, reader)
	if err != nil {
		return nil, err
	}
	result.RequiredState = state

	// The room-list decorations are derived from state and membership, so they are resent exactly when
	// this answer carries the state or the memberships they are derived from — which is also the only
	// moment a client needs them again.
	if job.initial || window.carriesNameChange || window.carriesAvatarChange || window.carriesMemberChange {
		if err := s.decorate(ctx, job, result, window); err != nil {
			return nil, err
		}
	}

	// A widened config that turns out to ask for nothing the client lacked is no news, and no news is
	// a room left out of the answer: an empty room object in a response reads to a client as "replace
	// what you have of this room", which is how a room disappears.
	if job.initial || len(result.Timeline) > 0 || len(result.RequiredState) > 0 ||
		result.Membership != "" || result.BumpStamp != nil {
		return result, nil
	}
	return nil, nil
}

// describeOutsideRoom renders a room the user is not in, which the MSC treats as a different kind of
// thing: an invite or a knock carries only the state that was known when it happened, because a user
// who cannot read a room must not be handed its history for having been asked about it.
func (s *Service) describeOutsideRoom(ctx context.Context, job *roomJob) (*RoomResult, error) {
	from := int64(0)
	if !job.initial {
		from = job.tracked.SentPos
	}
	// Nothing has happened to this membership since the connection was last told about the room, and
	// it has been described before, so this answer has no news about it.
	if !job.initial && job.membership.StreamPos <= from {
		return nil, nil
	}
	result := &RoomResult{
		Initial:    boolPointer(job.initial),
		Membership: job.membership.Membership,
	}
	if stamp := job.membership.StreamPos; stamp > 0 {
		value := stamp
		result.BumpStamp = &value
	}
	state, err := s.strippedState(ctx, job.roomID)
	if err != nil {
		return nil, err
	}
	result.StrippedState = state
	return result, nil
}

// timelineWindow is the slice of a room an answer carries, with the facts about what was left out of
// it and what it therefore implies.
type timelineWindow struct {
	rows      []store.EventRow
	limited   bool
	expanded  bool
	prevBatch string
	// These say whether the window carries the events a room's name, avatar and member list come from,
	// which decides whether the fields derived from them have to be resent.
	carriesNameChange   bool
	carriesAvatarChange bool
	carriesMemberChange bool
}

// window cuts the timeline this room is sent with.
//
// Three cases, and only two of them are a read of "what is new":
//
//   - an unseen room is sent the latest `timeline_limit` events of the room, because a client that has
//     never seen it is not interested in what happened since some token;
//   - a known room is sent what arrived since it was last shown, cut to the same limit, with `limited`
//     saying events were dropped and `prev_batch` saying where to fetch them;
//   - a room whose limit *grew* is sent the latest N events whether or not they are new, because the
//     client is asking for history the server would not give before. If that reaches back past what
//     the connection has seen, `expanded_timeline` says so; if it does not, this is an ordinary delta.
func (s *Service) window(ctx context.Context, job *roomJob, from, currentPos int64) (*timelineWindow, error) {
	all, err := s.store.Events.Range(ctx, store.EventQuery{RoomIDs: []string{job.roomID}})
	if err != nil {
		return nil, err
	}
	if job.timelineLimit == 0 {
		// A config that asks for no timeline is a client that wants a room's state and nothing else,
		// which is a room list with no message preview. It still gets a token, because a client that
		// pages from an empty string asks this server for the room's whole history.
		if job.initial {
			return &timelineWindow{prevBatch: formatPos(currentPos)}, nil
		}
		if positionOf(all) <= from {
			return nil, nil
		}
		return &timelineWindow{prevBatch: formatPos(from)}, nil
	}

	expanding := !job.initial && job.timelineLimit > job.tracked.TimelineLimit

	if job.initial || expanding {
		rows := all
		window := &timelineWindow{}
		if len(rows) > job.timelineLimit {
			rows = rows[len(rows)-job.timelineLimit:]
			window.limited = true
		}
		if expanding {
			// The widening earns the flag only if it reached back over events this connection has
			// already been shown; otherwise it is an ordinary delta and the incremental read below
			// describes it honestly.
			reachedBack := false
			for _, row := range rows {
				if row.ID <= job.tracked.SentPos {
					reachedBack = true
					break
				}
			}
			if !reachedBack {
				return s.incrementalWindow(all, from, job.timelineLimit)
			}
			window.expanded = true
		}
		window.rows = rows
		if len(window.rows) == 0 {
			if job.initial {
				return &timelineWindow{prevBatch: formatPos(currentPos)}, nil
			}
			return nil, nil
		}
		window.prevBatch = formatPos(window.rows[0].ID - 1)
		noteCarriedChanges(window)
		return window, nil
	}

	return s.incrementalWindow(all, from, job.timelineLimit)
}

// incrementalWindow reads what arrived in a room after the connection was last shown it.
func (s *Service) incrementalWindow(all []store.EventRow, from int64, limit int) (*timelineWindow, error) {
	var rows []store.EventRow
	for _, row := range all {
		if row.ID > from {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, nil
	}
	window := &timelineWindow{}
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
		window.limited = true
	}
	window.rows = rows
	window.prevBatch = formatPos(rows[0].ID - 1)
	noteCarriedChanges(window)
	return window, nil
}

// noteCarriedRecords which derived facts this window contains the events of. A rename, an avatar
// change and a membership change are the three things a client's room list draws that are not the
// events themselves, and each of them has to be resent when the event behind it is in this answer —
// otherwise a room is renamed and keeps its old name on the device until something else happens to it.
func noteCarriedChanges(window *timelineWindow) {
	for _, row := range window.rows {
		switch row.Type {
		case "m.room.name":
			window.carriesNameChange = true
		case "m.room.avatar":
			window.carriesAvatarChange = true
		case memberEventType:
			window.carriesMemberChange = true
		}
	}
}

// stateFor chooses the state entries this answer carries.
//
// An unseen room gets everything its config asks for, which is what lets a client draw the room at
// all. A known room gets the entries whose winning event arrived since it was last shown, plus the
// entries its *widened* config now asks for and its old config did not — the MSC's state expansion,
// computed per entry so that a client merely re-ordering its request is not resent the room.
func (s *Service) stateFor(ctx context.Context, job *roomJob, window *timelineWindow, reader roomserver.RenderOptions) ([]json.RawMessage, error) {
	entries, err := s.store.State.All(ctx, job.roomID)
	if err != nil {
		return nil, err
	}
	from := int64(0)
	var previous stateSelector
	if !job.initial {
		from = job.tracked.SentPos
		previous, _ = decodeSelector(job.tracked.StateConfig)
	}

	var wanted []stateChoice
	for _, entry := range entries {
		if !job.selector.matches(entry.Type, entry.StateKey) {
			continue
		}
		if !job.initial {
			changed := entry.StreamPos > from
			widenedTo := !previous.matches(entry.Type, entry.StateKey)
			if !changed && !widenedTo {
				continue
			}
		}
		removed, err := s.isRemovedState(ctx, entry)
		if err != nil {
			return nil, err
		}
		wanted = append(wanted, stateChoice{entry: entry, removed: removed})
	}

	if job.selector.LazyMembers {
		if err := s.lazyMembers(ctx, window, from, entries, &wanted); err != nil {
			return nil, err
		}
	}

	if len(wanted) == 0 {
		return nil, nil
	}
	// Rendered in state-key order, so that two answers about an unchanged room agree field for field
	// and a client's cache comparison — or a maintainer's diff — means something.
	sort.SliceStable(wanted, func(i, j int) bool {
		if wanted[i].entry.Type != wanted[j].entry.Type {
			return wanted[i].entry.Type < wanted[j].entry.Type
		}
		return wanted[i].entry.StateKey < wanted[j].entry.StateKey
	})

	ids := make([]string, 0, len(wanted))
	for _, entry := range wanted {
		if !entry.removed {
			ids = append(ids, entry.entry.EventID)
		}
	}
	rows, err := s.store.Events.ByEventIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.EventRow, len(rows))
	for _, row := range rows {
		byID[row.EventID] = row
	}

	out := make([]json.RawMessage, 0, len(wanted))
	seen := map[string]bool{}
	for _, entry := range wanted {
		key := entry.entry.Type + "\x00" + entry.entry.StateKey
		if seen[key] {
			continue
		}
		seen[key] = true
		if entry.removed {
			document, err := encodeStub(entry.entry.Type, entry.entry.StateKey)
			if err != nil {
				return nil, err
			}
			out = append(out, document)
			continue
		}
		row, ok := byID[entry.entry.EventID]
		if !ok {
			continue
		}
		event, err := s.rooms.Render(ctx, &row, reader)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, nil
}

// stateChoice is one state entry chosen for an answer.
type stateChoice struct {
	entry store.RoomState
	// removed says the entry exists but says nothing, which is reported as a stub rather than as the
	// event, so that a client clears the value instead of redrawing it.
	removed bool
}

// lazyMembers adds the memberships a client needs in order to draw what this answer already contains,
// which is what the MSC's lazy-membership rules are for: a timeline of messages from people whose
// names the client does not know is not a rendered timeline.
//
// The MSC's three sources, and how this server cuts each of them:
//
//   - the senders of the events in the timeline (a user ID, which is also the state key of their
//     membership) and the targets of the membership events in it, so that every line of text can be
//     attributed;
//   - when this answer is not gappy, every membership that moved since the client last saw the room —
//     this is how a client keeps a member list without fetching one, and why the flag is dropped when
//     the answer *is* gappy: half a member list is worse than an acknowledged gap, because a client
//     that believed it had the members of a room would encrypt to the wrong set of devices.
//
// "Excluding memberships previously returned" is cut by the room's recorded position rather than by a
// set of every state key ever sent: an unbounded set for an optimisation, and the worst case of the
// cut is a membership event a client is told twice, which it treats as an update.
func (s *Service) lazyMembers(
	ctx context.Context,
	window *timelineWindow,
	from int64,
	entries []store.RoomState,
	wanted *[]stateChoice,
) error {
	// The senders of the events in this answer are the members a client cannot draw it without.
	need := map[string]bool{}
	if window != nil {
		for _, row := range window.rows {
			need[row.Sender] = true
			if row.Type == memberEventType && row.StateKey != nil {
				need[*row.StateKey] = true
			}
		}
		if !window.limited {
			for _, entry := range entries {
				if entry.Type == memberEventType && entry.StreamPos > from {
					need[entry.StateKey] = true
				}
			}
		}
	}
	chosen := map[string]bool{}
	for _, choice := range *wanted {
		if choice.entry.Type == memberEventType {
			chosen[choice.entry.StateKey] = true
		}
	}
	for _, entry := range entries {
		if entry.Type != memberEventType || !need[entry.StateKey] || chosen[entry.StateKey] {
			continue
		}
		*wanted = append(*wanted, stateChoice{entry: entry})
	}
	return nil
}

// isRemovedState reports whether a state entry exists but says nothing — which is what a redacted room
// name looks like, and what a client can only be told with a stub.
//
// Matrix has no delete for state: taking a room's name away is an `m.room.name` event whose content
// carries no name. Sent as state, that event reads as a name event, and a client keeps drawing the old
// name; the MSC's answer is `{type, state_key}` with no content, which a client reads as "clear it".
func (s *Service) isRemovedState(ctx context.Context, entry store.RoomState) (bool, error) {
	field, ok := valueBearingState[entry.Type]
	if !ok {
		return false, nil
	}
	row, err := s.store.Events.ByEventID(ctx, entry.EventID)
	if err != nil {
		if store.IsNotFound(err) {
			// An entry whose event cannot be read is not evidence that anything was removed.
			return false, nil
		}
		return false, err
	}
	var document struct {
		Content map[string]json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(row.JSON), &document); err != nil {
		return false, nil
	}
	value, present := document.Content[field]
	if !present || string(value) == "null" {
		return true, nil
	}
	return string(value) == `""`, nil
}

// valueBearingState maps the state types whose meaning is one value, and which can therefore be taken
// away: with no value to point at there is nothing for a stub to be about.
var valueBearingState = map[string]string{
	"m.room.name":            "name",
	"m.room.avatar":          "url",
	"m.room.topic":           "topic",
	"m.room.canonical_alias": "alias",
}

// decorate fills in the fields a client draws a room list from without loading state: name, avatar,
// heroes, and how many people are in it.
func (s *Service) decorate(ctx context.Context, job *roomJob, result *RoomResult, window *timelineWindow) error {
	room, err := s.store.Rooms.Get(ctx, job.roomID)
	if err != nil {
		if store.IsNotFound(err) {
			// A room with no row here is one this server knows of but does not hold. Its entry is
			// drawn from the events in this answer instead, which is all a client can be given.
			return nil
		}
		return err
	}
	switch {
	case room.Name != "":
		result.Name = stringValue(room.Name)
	case window != nil && window.carriesNameChange:
		// The name event is in this answer and the room has no name: that is the removal, and an
		// omitted field would mean "unchanged" to a client that was showing something else.
		result.Name = nullValue()
	}
	if room.AvatarURL != "" {
		result.Avatar = stringValue(room.AvatarURL)
	} else if window != nil && window.carriesAvatarChange {
		result.Avatar = nullValue()
	}
	joined, invited := room.JoinedCount, room.InviteCount
	result.JoinedCount = &joined
	result.InvitedCount = &invited

	// Heroes only when the room has no name to draw, which is the MSC's condition and the only case
	// they are worth their bytes.
	if result.Name == nil {
		heroes, err := s.heroes(ctx, job.roomID)
		if err != nil {
			return err
		}
		result.Heroes = heroes
	}
	return nil
}

// maxHeroes is the /v3/sync hero cap, which MSC4186 inherits by pointing at it.
const maxHeroes = 5

// heroes are the users to name a room after, in the order the specification gives: joined, then
// invited, then left, capped at five.
func (s *Service) heroes(ctx context.Context, roomID string) ([]Hero, error) {
	members, err := s.store.Memberships.List(ctx, roomID, nil, nil)
	if err != nil {
		return nil, err
	}
	rank := func(membership string) int {
		switch membership {
		case matrix.MembershipJoin:
			return 0
		case matrix.MembershipInvite:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(members, func(i, j int) bool {
		if rank(members[i].Membership) != rank(members[j].Membership) {
			return rank(members[i].Membership) < rank(members[j].Membership)
		}
		return members[i].UserID < members[j].UserID
	})
	if len(members) > maxHeroes {
		members = members[:maxHeroes]
	}
	heroes := make([]Hero, 0, len(members))
	for _, member := range members {
		if member.UserID == "" {
			continue
		}
		heroes = append(heroes, Hero{
			UserID:      member.UserID,
			DisplayName: member.DisplayName,
			AvatarURL:   member.AvatarURL,
		})
	}
	return heroes, nil
}

// strippedState renders the state a user may see about a room they are not in: the four fields of a
// stripped state event, and nothing else, because a room's state is not theirs to read.
func (s *Service) strippedState(ctx context.Context, roomID string) ([]json.RawMessage, error) {
	entries, err := s.store.State.All(ctx, roomID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.EventID)
	}
	rows, err := s.store.Events.ByEventIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(rows))
	for i := range rows {
		stripped, err := stripEvent(rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, stripped)
	}
	return out, nil
}

// stripEvent reduces a stored event to the four fields a stripped state event carries.
func stripEvent(row store.EventRow) (json.RawMessage, error) {
	var document struct {
		Type     string          `json:"type"`
		StateKey string          `json:"state_key"`
		Sender   string          `json:"sender"`
		Content  json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(row.JSON), &document); err != nil {
		return nil, fmt.Errorf("reading the state of a room the user is not in: %w", err)
	}
	if document.Type == "" {
		// An event with no type cannot be drawn as state, and sending `{}` as a state event would
		// teach a client that the room has a typeless entry it will never be able to clear.
		return nil, nil
	}
	content := document.Content
	if len(content) == 0 {
		content = json.RawMessage("{}")
	}
	stripped, err := json.Marshal(struct {
		Type     string          `json:"type"`
		StateKey string          `json:"state_key"`
		Sender   string          `json:"sender"`
		Content  json.RawMessage `json:"content"`
	}{document.Type, document.StateKey, document.Sender, content})
	if err != nil {
		return nil, fmt.Errorf("encoding stripped state: %w", err)
	}
	return stripped, nil
}

// boolPointer exists because half of this endpoint's meaning is in the difference between false and
// absent, and that difference is a pointer.
func boolPointer(value bool) *bool {
	if !value {
		return nil
	}
	flag := value
	return &flag
}

// positionOf is the newest event's position in a room's events, or 0 when it has none.
func positionOf(rows []store.EventRow) int64 {
	if len(rows) == 0 {
		return 0
	}
	return rows[len(rows)-1].ID
}

// stringValue and nullValue write the two things an optional string field has to say: "here it is",
// and "this was taken away from you", which a nil pointer cannot express — nil means "unchanged", and
// a client that heard "unchanged" about a name that was deleted keeps showing it.
func stringValue(value string) *json.RawMessage {
	document := json.RawMessage(fmt.Sprintf("%q", value))
	return &document
}

func nullValue() *json.RawMessage {
	document := json.RawMessage("null")
	return &document
}
