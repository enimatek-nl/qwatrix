// Package syncapi implements GET /sync: the endpoint that turns the event stream into a
// client's view of the world.
//
// The whole design follows from one idea in the specification: a sync token names a
// position in a single global stream, and a sync answers "what happened between token T and
// now". qwatrix makes that idea literal — the token is the global stream position of an
// event row — which is why there is no delta table, no per-user position bookkeeping, and no
// way for a client's view to be recomputed differently twice.
//
// Two consequences are worth stating because they shape the code below:
//
//   - A client that has never synced (no `since`) is not special-cased into a different
//     algorithm; it is the same read with the position set to the beginning of the stream.
//   - Long-polling waits on a notification rather than on a timer: the response is rebuilt
//     from the database after each wake-up, so a wake-up cannot lose an event and a spurious
//     one costs one extra read.
package syncapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/devicelists"
	"github.com/enimatek-nl/qwatrix/internal/filters"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/presence"
	"github.com/enimatek-nl/qwatrix/internal/pushrules"
	"github.com/enimatek-nl/qwatrix/internal/readstate"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/enimatek-nl/qwatrix/internal/todevice"
)

// Service serves /sync.
type Service struct {
	store       *store.Store
	cfg         *config.Config
	rooms       *roomserver.Service
	presence    *presence.Service
	readState   *readstate.Service
	pushRules   *pushrules.Service
	toDevice    *todevice.Service
	deviceLists *devicelists.Service
	notifier    *notifier.Notifier
}

// New builds the sync service.
func New(st *store.Store, cfg *config.Config, rooms *roomserver.Service, pres *presence.Service, read *readstate.Service, push *pushrules.Service, device *todevice.Service, lists *devicelists.Service, nf *notifier.Notifier) *Service {
	return &Service{store: st, cfg: cfg, rooms: rooms, presence: pres, readState: read, pushRules: push, toDevice: device, deviceLists: lists, notifier: nf}
}

// Request is a parsed GET /sync.
type Request struct {
	// Localpart is the authenticated user.
	Localpart string
	// DeviceID is used to echo transaction IDs and to scope to-device messages.
	DeviceID string
	// Since is the client's token; empty means an initial sync.
	Since string
	// Timeout is how long to long-poll. The server caps it.
	Timeout time.Duration
	// FullState asks for the whole current state regardless of what changed.
	FullState bool
	// NotMembership excludes rooms whose membership is one of these.
	NotMembership []string
	// TimelineLimit overrides the default timeline window (a filter's
	// room.timeline.limit); a zero value means "no timeline events".
	TimelineLimit *int
	// Filter is the decoded event filter, or nil when the client sent none. The limit above is
	// one field of it, kept separately because the sync builder needs it on every room.
	Filter *filters.Filter
	// IncludeLeaveRooms is the inverse of not_membership=leave.
	IncludeLeaveRooms bool
	// RenderCache shares the per-room reads of one response; filled in by the builder when a
	// caller left it empty.
	RenderCache *roomserver.RenderCache
	// SetPresence is the ?set_presence= query parameter. "offline" is the specification's
	// spelling of "do not mark me as active", and any other valid value is the caller
	// declaring its presence.
	SetPresence string
}

// Response is the sync body.
type Response struct {
	NextBatch        string          `json:"next_batch"`
	Rooms            RoomsDelta      `json:"rooms"`
	Presence         *EventsBatch    `json:"presence,omitempty"`
	AccountData      *EventsBatch    `json:"account_data,omitempty"`
	ToDevice         *EventsBatch    `json:"to_device,omitempty"`
	DeviceLists      *DeviceLists    `json:"device_lists,omitempty"`
	OTKCounts        map[string]int  `json:"device_one_time_keys_count,omitempty"`
	FallbackKeys     map[string]bool `json:"device_fallback_keys,omitempty"`
	NextBatchPresent bool            `json:"-"`
	// DelayedMS is written by the handler for logging, never on the wire.
	DelayedMS int64 `json:"-"`
}

// EventsBatch is the `{ "events": [...] }` wrapper the spec uses everywhere.
type EventsBatch struct {
	Events []json.RawMessage `json:"events"`
}

// RoomsDelta is the `rooms` object.
type RoomsDelta struct {
	Join   map[string]*JoinedRoom  `json:"join,omitempty"`
	Invite map[string]*InvitedRoom `json:"invite,omitempty"`
	Leave  map[string]*LeftRoom    `json:"leave,omitempty"`
	Knock  map[string]*KnockedRoom `json:"knock,omitempty"`
}

// Timeline is a room's slice of the stream.
type Timeline struct {
	Events  []json.RawMessage `json:"events"`
	Limited bool              `json:"limited"`
	// PrevBatch is where a client continues backwards to reach what came before this
	// window, and is the reason /messages and /sync can be mixed without gaps.
	PrevBatch string `json:"prev_batch"`
}

// JoinedRoom is one room in the `join` section.
type JoinedRoom struct {
	Timeline    Timeline     `json:"timeline"`
	State       EventsBatch  `json:"state"`
	Ephemeral   *EventsBatch `json:"ephemeral,omitempty"`
	AccountData *EventsBatch `json:"account_data,omitempty"`
	Unread      *Unread      `json:"unread_notifications,omitempty"`
	// UnreadThreads is the same badge split per thread, present only on a request that asked
	// for it; on every other request Unread covers the whole room, threads included.
	UnreadThreads map[string]Unread `json:"unread_thread_notifications,omitempty"`
	Summary       *Summary          `json:"summary,omitempty"`
	Membership    string            `json:"membership,omitempty"`
	// PrevBatch is the token a client uses for /messages on this room's oldest returned
	// event; it duplicates timeline.prev_batch because the spec has both.
	PrevBatch string `json:"prev_batch,omitempty"`
}

// InvitedRoom is one room in the `invite` section.
type InvitedRoom struct {
	InviteState EventsBatch `json:"invite_state"`
}

// LeftRoom is one room in the `leave` (or `knock`) section.
type LeftRoom struct {
	Timeline *Timeline    `json:"timeline,omitempty"`
	State    *EventsBatch `json:"state,omitempty"`
}

// KnockedRoom is one room in the `knock` section.
type KnockedRoom struct {
	KnockState EventsBatch `json:"knock_state"`
}

// Unread is the read-receipt derived unread count.
type Unread struct {
	NotificationCount int64 `json:"notification_count"`
	HighlightCount    int64 `json:"highlight_count"`
}

// Summary is the room summary a sync carries so a client can draw a room list without
// asking for state.
//
// The names are the spec's namespaced keys. They are unusual enough to be worth a comment:
// `m.heroes` is the list of users to name a room after when it has no name, and it is
// present only in the summary, never in state.
type Summary struct {
	Heroes             []string `json:"m.heroes,omitempty"`
	InvitedMemberCount *int64   `json:"m.invited_member_count,omitempty"`
	JoinedMemberCount  *int64   `json:"m.joined_member_count,omitempty"`
}

// DeviceLists reports cross-key changes; the fields are part of every incremental sync a
// client uses to decide whether to refetch device keys.
type DeviceLists struct {
	Changed []string `json:"changed,omitempty"`
	Left    []string `json:"left,omitempty"`
}

// ErrBadSinceToken is returned for an unparsable `since`, which the spec answers with
// 401 M_UNKNOWN_TOKEN so the client knows to start over.
var ErrBadSinceToken = errors.New("syncapi: unparsable since token")

// Sync performs one sync, long-polling up to the request's timeout.
//
// The loop reads the stream position *before* reading events. That ordering is the entire
// correctness argument: if an event lands while the read is in flight, its position is
// above the token this response reports, so the client sees it next time; and if the
// position advanced before the read, the read already included it. Either way nothing is
// skipped, which is the property a "missing message in a room" bug report always comes back
// to.
func (s *Service) Sync(ctx context.Context, req Request) (*Response, error) {
	since, err := s.parseSince(req.Since)
	if err != nil {
		return nil, err
	}

	// A sync doubles as a heartbeat: unless the caller said otherwise, the act of syncing is
	// how a user tells the server they are here. It happens before the read so that a peer
	// long-polling on this same call sees the change.
	switch value := req.SetPresence; value {
	case "":
		// Absent means "leave my presence alone", which is what a background sync does.
	case store.PresenceOffline:
	default:
		if presence.Valid(value) {
			if err := s.presence.Set(ctx, req.Localpart, value, ""); err != nil {
				return nil, err
			}
		}
	}

	deadline := time.Now().Add(req.Timeout)
	for {
		generation := s.notifier.Generation()
		response, isEmpty, err := s.build(ctx, req, since)
		if err != nil {
			return nil, err
		}
		if !isEmpty || time.Now().After(deadline) {
			return response, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return response, nil
		}
		if !s.notifier.WaitForChange(ctx, generation, remaining) {
			// Either the caller went away or the timeout passed; both mean "answer with
			// what you have", which is an empty sync.
			return response, nil
		}
	}
}

// build assembles one response and reports whether it carried anything, which is what tells
// the long-poll whether to keep waiting.
func (s *Service) build(ctx context.Context, req Request, since int64) (*Response, bool, error) {
	if req.RenderCache == nil {
		req.RenderCache = s.rooms.NewRenderCache()
	}
	// next_batch is the end of the stream as it stood when the read started.
	position, err := s.store.Events.MaxPosition(ctx)
	if err != nil {
		return nil, false, err
	}

	memberships, err := s.store.Memberships.ListByUser(ctx, req.Localpart, req.NotMembership...)
	if err != nil {
		return nil, false, err
	}

	// A forgotten room is invisible to a new sync and unavoidable to an old one. If this
	// client's token predates the leave, the leave is a fact it has not been told yet, and
	// withholding it leaves the device holding a room with no message that would ever remove
	// it. A room forgotten *before* this token is left out, which is what forgetting means.
	if since > 0 {
		forgotten, err := s.store.Memberships.ListForgottenByUser(ctx, req.Localpart)
		if err != nil {
			return nil, false, err
		}
		for _, membership := range forgotten {
			hidden := false
			for _, exclude := range req.NotMembership {
				if membership.Membership == exclude {
					hidden = true
				}
			}
			if !hidden && membership.StreamPos > since {
				memberships = append(memberships, membership)
			}
		}
	}

	// Asking for a sync is proof the user is in front of their client, so it is the moment the
	// server may call them online — for a user whose presence it has never observed at all. The
	// write is what carries the news to their room-mates, whose next sync asks the question
	// "who around me is present?" and cannot answer it about a stranger the server never saw.
	if err := s.presence.MarkSeen(ctx, req.Localpart); err != nil {
		slog.Warn("sync: recording presence", "error", err)
	}

	response := &Response{
		NextBatch: formatToken(position),
		Rooms:     RoomsDelta{},
		Presence:  &EventsBatch{Events: []json.RawMessage{}},
	}

	// Presence joins the response through the same stream position as everything else, so a
	// client's single `since` token decides what presence it has already been told.
	if events, err := s.readState.AccountData(ctx, req.Localpart, since); err == nil && len(events) > 0 {
		response.AccountData = &EventsBatch{Events: events}
	}
	// Push rules ride along as `m.push_rules` account data. They are the configuration that
	// decides whether a message ever reaches a person's phone, so a client that has just come
	// back online has to be given them — and given them again the moment they change, which is
	// why a rule write stamps the account rather than waiting for the client to ask.
	if batch := s.pushRuleAccountData(ctx, req.Localpart, since); batch != nil {
		if response.AccountData == nil {
			response.AccountData = &EventsBatch{Events: []json.RawMessage{}}
		}
		response.AccountData.Events = append(response.AccountData.Events, batch...)
	}
	// To-device messages are addressed to a device, so they only exist for a request that came
	// in on one: an access token without a device has no queue to drain.
	if s.toDevice != nil && req.DeviceID != "" {
		if events, _, err := s.toDevice.Take(ctx, req.Localpart, req.DeviceID); err == nil && len(events) > 0 {
			response.ToDevice = &EventsBatch{Events: events}
		} else if err != nil {
			slog.Warn("sync: reading to-device messages", "error", err)
		}
	}
	if events, err := s.presence.Events(ctx, req.Localpart, since); err == nil && len(events) > 0 {
		response.Presence.Events = events
	} else if err != nil {
		// Presence is advisory; a failure to read it must not fail the sync, but it is
		// worth the log line because silently dropping it looks like a broken client.
		slog.Warn("sync: reading presence", "error", err)
	}

	timelineLimit := s.cfg.SyncDefaultTimelineLimit
	if req.TimelineLimit != nil {
		timelineLimit = *req.TimelineLimit
	}

	var joinedCount, invitedCount int
	empty := true

	for _, membership := range memberships {
		switch membership.Membership {
		case matrix.MembershipJoin:
			room, isEmptyRoom, err := s.joinedRoom(ctx, req, membership, since, position, timelineLimit)
			if err != nil {
				return nil, false, err
			}
			// A room with nothing new is left out of an incremental sync. Including it forces
			// every client to diff a room it has just been told is unchanged, and a client that
			// trusts the presence of the key rather than its contents will repaint it. `full_state`
			// asks for the opposite, so it is honoured: that request is a client saying it wants
			// the whole picture whether or not anything moved.
			if isEmptyRoom && since > 0 && !req.FullState {
				break
			}
			if response.Rooms.Join == nil {
				response.Rooms.Join = map[string]*JoinedRoom{}
			}
			response.Rooms.Join[membership.RoomID] = room
			joinedCount++
			if !isEmptyRoom {
				empty = false
			}
		case matrix.MembershipInvite:
			room, err := s.invitedRoom(ctx, membership.RoomID, since)
			if err != nil {
				return nil, false, err
			}
			if response.Rooms.Invite == nil {
				response.Rooms.Invite = map[string]*InvitedRoom{}
			}
			response.Rooms.Invite[membership.RoomID] = room
			invitedCount++
			empty = false
		case matrix.MembershipLeave:
			room, isEmptyRoom, include, err := s.leftRoom(ctx, req, membership, since, timelineLimit)
			if err != nil {
				return nil, false, err
			}
			if !include {
				break
			}
			if response.Rooms.Leave == nil {
				response.Rooms.Leave = map[string]*LeftRoom{}
			}
			response.Rooms.Leave[membership.RoomID] = room
			if !isEmptyRoom {
				empty = false
			}
		case matrix.MembershipKnock:
			room, err := s.knockedRoom(ctx, membership.RoomID, since)
			if err != nil {
				return nil, false, err
			}
			if response.Rooms.Knock == nil {
				response.Rooms.Knock = map[string]*KnockedRoom{}
			}
			response.Rooms.Knock[membership.RoomID] = room
			empty = false
		}
	}

	// The device streams are part of every incremental sync. The objects are always present, so
	// that a client's "should I refetch keys" logic sees a shape it expects, and the lists carry
	// the people whose encryption devices moved since this client's token: a colleague who bought
	// a phone is invisible to a sender that keeps encrypting for the old set of devices, and the
	// symptom — one device never receiving anything — is not something the sender can observe.
	response.DeviceLists = &DeviceLists{Changed: []string{}, Left: []string{}}
	if s.deviceLists != nil {
		if changed, left, err := s.deviceLists.Delta(ctx, req.Localpart, since); err == nil {
			response.DeviceLists.Changed = changed
			response.DeviceLists.Left = left
		} else {
			slog.Warn("sync: reading device-list changes", "error", err)
		}
	}
	response.OTKCounts = map[string]int{}

	_ = joinedCount
	_ = invitedCount
	return response, empty, nil
}

// section names which part of a filter applies to a batch of events.
type section int

const (
	timelineSection section = iota
	stateSection
)

// filterEvents drops the events a client's filter excludes.
//
// A missing filter is not a filter that excludes nothing, it is the common case, so this is on
// the hot path of every room in every sync and returns the input untouched when there is nothing
// to do.
func filterEvents(rows []store.EventRow, filter *filters.Filter, which section) []store.EventRow {
	if filter == nil {
		return rows
	}
	var rule *filters.EventFilter
	switch which {
	case timelineSection:
		if filter.Room == nil {
			return rows
		}
		rule = filter.Room.Timeline.Embedded()
	case stateSection:
		if filter.Room == nil {
			return rows
		}
		rule = filter.Room.State
	}
	if rule == nil {
		return rows
	}
	kept := make([]store.EventRow, 0, len(rows))
	for i := range rows {
		if rule.Allows(rows[i].Type, rows[i].Sender) {
			kept = append(kept, rows[i])
		}
	}
	return kept
}

// joinedRoom builds one room's `join` entry.
func (s *Service) joinedRoom(ctx context.Context, req Request, membership store.Membership, since, position int64, timelineLimit int) (*JoinedRoom, bool, error) {
	reader := roomserver.RenderOptions{
		DeviceID:            req.DeviceID,
		Localpart:           req.Localpart,
		Membership:          matrix.MembershipJoin,
		MembershipStreamPos: membership.StreamPos,
		Cache:               req.RenderCache,
	}

	room := &JoinedRoom{
		Timeline:    Timeline{Events: []json.RawMessage{}},
		State:       EventsBatch{Events: []json.RawMessage{}},
		Ephemeral:   &EventsBatch{Events: []json.RawMessage{}},
		AccountData: &EventsBatch{Events: []json.RawMessage{}},
		Membership:  matrix.MembershipJoin,
		Unread:      &Unread{},
	}

	// Typing only when it moved after this token: an always-present indicator would make every
	// room look changed on every sync, and a client would then re-ask for all of them forever.
	if typing, ok, err := s.readState.TypingEvent(membership.RoomID, since); err == nil && ok {
		room.Ephemeral.Events = append(room.Ephemeral.Events, typing)
	}
	// Receipts, by contrast, are persisted facts: reporting the ones this token has not seen
	// yet is both bounded and repeatable, and it means a reader's other device picks the read
	// position up on its next sync.
	if receipts, err := s.readState.ReceiptEvents(ctx, membership.RoomID, since); err == nil {
		room.Ephemeral.Events = append(room.Ephemeral.Events, receipts...)
	}
	if perRoom, err := s.readState.RoomAccountData(ctx, req.Localpart,
		[]string{membership.RoomID}, since); err == nil {
		if events := perRoom[membership.RoomID]; len(events) > 0 {
			room.AccountData.Events = append(room.AccountData.Events, events...)
		}
	}

	// The timeline window. The spec wants the most recent `limit` events, so a window that
	// had to be cut short reports `limited` and the client pages back for the rest.
	query := store.EventQuery{RoomIDs: []string{membership.RoomID}, After: since, Limit: 0}
	all, err := s.store.Events.Range(ctx, query)
	if err != nil {
		return nil, false, err
	}

	// The filter runs before the window is cut, not after. A client that asked for ten
	// `m.room.message` events in a room of fifty state changes wants ten messages, and if the
	// window were cut first it would get whichever ten events happened to be newest and then be
	// filtered down to two — with `limited` reporting a truncation that never happened and a
	// client paging back for events it was never eligible to see.
	all = filterEvents(all, req.Filter, timelineSection)
	limited := false
	if timelineLimit > 0 && len(all) > timelineLimit {
		all = all[len(all)-timelineLimit:]
		limited = true
	} else if timelineLimit == 0 {
		all = nil
	}

	for i := range all {
		event, err := s.rooms.Render(ctx, &all[i], reader)
		if err != nil {
			return nil, false, err
		}
		room.Timeline.Events = append(room.Timeline.Events, event)
	}
	// A room the client has only just been allowed into cannot be delivered whole. Its token
	// names a moment before the join, the room already has a history from that moment, and the
	// server is not going to hand over events the client was not entitled to at the time — so the
	// timeline starts partway through. `limited` is how the server admits that, and it is the flag
	// that makes a client page back with /messages instead of concluding it has seen everything.
	// Only an incremental sync can be limited this way: an initial sync has no token to fall
	// short of.
	if since > 0 && membership.StreamPos > since {
		limited = true
	}
	room.Timeline.Limited = limited

	// prev_batch is the token just before the oldest event returned. A client that asked
	// for the window and then pages backwards must see the events this response dropped —
	// which is why an empty timeline still needs a real token rather than an empty string.
	switch {
	case len(all) > 0:
		room.Timeline.PrevBatch = formatToken(all[0].ID - 1)
	case since > 0:
		room.Timeline.PrevBatch = formatToken(since)
	default:
		room.Timeline.PrevBatch = formatToken(position)
	}
	room.PrevBatch = room.Timeline.PrevBatch

	// The state delta: state entries whose winning event arrived after the client's token.
	// On an initial sync (since == 0) that is the whole current state, which is what the
	// spec wants and what makes a fresh client render the room at all.
	stateEvents, err := s.stateDelta(ctx, membership.RoomID, since, req.FullState, reader, req.Filter)
	if err != nil {
		return nil, false, err
	}
	room.State.Events = stateEvents

	if summary, err := s.summary(ctx, membership.RoomID); err == nil {
		room.Summary = summary
	}

	// The unread badge, measured from the reader's receipts rather than from their token: it says
	// what the person has not read, which a second device logging in must not change.
	if s.readState != nil {
		counts, threads, err := s.readState.Badge(ctx, req.Localpart, membership.RoomID, wantsThreadBadges(req.Filter))
		if err != nil {
			// A badge is a summary of facts the rest of this response already carries, so a
			// failure here reports zero rather than failing the sync.
			slog.Warn("syncapi: could not count the unread events of a room",
				"room_id", membership.RoomID, "error", err)
			counts, threads = &readstate.BadgeCounts{}, nil
		}
		room.Unread = &Unread{
			NotificationCount: counts.NotificationCount,
			HighlightCount:    counts.HighlightCount,
		}
		if threads != nil {
			room.UnreadThreads = map[string]Unread{}
			for root, thread := range threads {
				room.UnreadThreads[root] = Unread{
					NotificationCount: thread.NotificationCount,
					HighlightCount:    thread.HighlightCount,
				}
			}
		}
	}

	// "Empty" means this sync would tell the client nothing it did not already know. Receipts,
	// typing, and per-room account data count: they are the only content some syncs have.
	//
	// So does an unread badge. A room the person has things to read in is never finished with,
	// however quiet it is — and a client that polls again from an later token would otherwise be
	// told nothing at all about a room still showing unread messages, losing the badge until
	// somebody typed.
	isEmpty := len(room.Timeline.Events) == 0 && len(room.State.Events) == 0 &&
		len(room.Ephemeral.Events) == 0 && len(room.AccountData.Events) == 0 &&
		!room.HasUnread()
	return room, isEmpty, nil
}

// HasUnread reports whether this room is still waiting to be read.
func (r *JoinedRoom) HasUnread() bool {
	if r.Unread != nil && (r.Unread.NotificationCount > 0 || r.Unread.HighlightCount > 0) {
		return true
	}
	for _, thread := range r.UnreadThreads {
		if thread.NotificationCount > 0 || thread.HighlightCount > 0 {
			return true
		}
	}
	return false
}

// wantsThreadBadges reports whether a request asked for the per-thread split.
//
// The flag is `{"room":{"timeline":{"unread_thread_notifications":true}}}`. It is opt-in because the
// default answer changes meaning when it is present: an unsplit badge counts the whole room, a split
// one counts the main timeline and hands the threads over separately.
func wantsThreadBadges(filter *filters.Filter) bool {
	return filter != nil && filter.Room != nil && filter.Room.Timeline != nil &&
		filter.Room.Timeline.UnreadThreadNotifications
}

// stateDelta returns the state entries a client has not seen, or the whole state when
// `fullState` is set.
func (s *Service) stateDelta(ctx context.Context, roomID string, since int64, fullState bool, reader roomserver.RenderOptions, filter *filters.Filter) ([]json.RawMessage, error) {
	var (
		entries []store.RoomState
		err     error
	)
	if fullState || since == 0 {
		entries, err = s.store.State.All(ctx, roomID)
	} else {
		entries, err = s.store.State.AllAfter(ctx, roomID, since)
	}
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return []json.RawMessage{}, nil
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
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
	// A state filter applies to the events, so it runs after they are loaded and before they
	// are rendered. A client asking for `state.types: []` wants the room's current state to
	// stay out of its sync entirely — that is how a client that already caches state avoids
	// refetching it on every join.
	kept := filterEvents(rows, filter, stateSection)
	if len(kept) != len(rows) {
		byID = make(map[string]store.EventRow, len(kept))
		for _, row := range kept {
			byID[row.EventID] = row
		}
	}
	out := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		row, ok := byID[entry.EventID]
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

// invitedRoom builds one room's `invite` entry.
//
// The state sent with an invite is the state as of the invite event — the "invite state" —
// which for a room this server created is the room's current state, since nothing can have
// happened in it while the invitee was not a member.
func (s *Service) invitedRoom(ctx context.Context, roomID string, since int64) (*InvitedRoom, error) {
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
	room := &InvitedRoom{InviteState: EventsBatch{Events: []json.RawMessage{}}}
	for i := range rows {
		event, err := s.rooms.Render(ctx, &rows[i], roomserver.RenderOptions{SkipTransactionID: true})
		if err != nil {
			return nil, err
		}
		room.InviteState.Events = append(room.InviteState.Events, event)
	}
	return room, nil
}

// leftRoom builds one room's `leave` entry.
//
// A room appears here when the user's membership is leave but the server still owes them
// the events that happened, most often the leave event itself. A user who left long ago and
// has caught up gets nothing, which is what stops a sync from carrying every room they have
// ever touched forever.
// leftRoom builds the leave section for one room, or reports that the room does not belong in it.
//
// Two rules make this endpoint correct, and both are about not telling a client something twice.
//
// A room belongs in the leave section of an incremental sync only while the leave itself is news.
// Once the client's token is past the leave, the room has been reported, and repeating it on every
// sync for as long as other people keep talking in it means a client that left one room sees it
// scroll past forever — and, worse, has to guess which of the repetitions is the real one.
//
// What the client is then shown is the room as it stood when the reader left. Events written after
// that are not this reader's to see (`history_visibility: joined` means "whoever was here may read
// that stretch", not "anybody may read everything"), so the timeline stops at the leave event.
//
// The state section carries something only when the timeline carries nothing. A timeline is
// self-explaining; state beside it would be a second, staler copy of the same facts. When the
// timeline is empty — a client that asked for zero events, or a room the reader left before anything
// happened — the state is the only way to show what the room looked like at the door on the way out.
func (s *Service) leftRoom(ctx context.Context, req Request, membership store.Membership, since int64, timelineLimit int) (*LeftRoom, bool, bool, error) {
	if since > 0 && membership.StreamPos != 0 && membership.StreamPos <= since {
		// The leave is behind this client's token, so this room has already been reported.
		return nil, true, false, nil
	}
	reader := roomserver.RenderOptions{
		DeviceID:            req.DeviceID,
		Localpart:           req.Localpart,
		Membership:          matrix.MembershipLeave,
		MembershipStreamPos: membership.StreamPos,
		Cache:               req.RenderCache,
	}
	room := &LeftRoom{}

	rows, err := s.store.Events.Range(ctx, store.EventQuery{RoomIDs: []string{membership.RoomID}, After: since})
	if err != nil {
		return nil, false, false, err
	}
	// The room's history ends where the reader's membership ended. Events at or after the leave
	// are not visible; the leave event itself is, because "you are no longer in here" is the one
	// fact about this room the client needs to hear.
	if membership.StreamPos > 0 {
		bounded := rows[:0]
		for _, row := range rows {
			if row.ID <= membership.StreamPos {
				bounded = append(bounded, row)
			}
		}
		rows = bounded
	}
	rows = filterEvents(rows, req.Filter, timelineSection)
	if timelineLimit > 0 && len(rows) > timelineLimit {
		rows = rows[len(rows)-timelineLimit:]
	} else if timelineLimit == 0 {
		rows = nil
	}
	if len(rows) == 0 {
		// The timeline is still an empty list rather than an absent object. "I have no events
		// for you" is an answer; "here is nothing" is not, and a client that has to handle both
		// ends up treating the absence of a key as a state it has to guess at.
		room.Timeline = &Timeline{Events: []json.RawMessage{}}
		// Nothing to show in the timeline, so the state has to carry the room instead: it is the
		// only way a client that asked for no events learns what it walked away from, and it is
		// where the leave event itself has to appear.
		state, err := s.stateAtLeave(ctx, membership.RoomID, membership.StreamPos, reader, req.Filter)
		if err != nil {
			return nil, false, false, err
		}
		room.State = &EventsBatch{Events: state}
		// On an initial sync an empty leave entry is still the answer — it is how a client learns
		// it is not in this room any more. On an incremental one the caller has already been told,
		// and reporting a room that changed nothing would wake the long-poll for it forever.
		return room, true, since == 0, nil
	}

	room.Timeline = &Timeline{Events: []json.RawMessage{}, PrevBatch: formatToken(rows[0].ID - 1)}
	for i := range rows {
		event, err := s.rooms.Render(ctx, &rows[i], reader)
		if err != nil {
			return nil, false, false, err
		}
		room.Timeline.Events = append(room.Timeline.Events, event)
	}
	if req.FullState {
		// A client that asked for the full state gets the room's state as of the leave, which is
		// the only state this reader has a right to.
		state, err := s.stateAtLeave(ctx, membership.RoomID, membership.StreamPos, reader, req.Filter)
		if err != nil {
			return nil, false, false, err
		}
		room.State = &EventsBatch{Events: state}
	}
	return room, false, true, nil
}

// stateAtLeave renders the room's state as it stood at a reader's leave, which is what the leave
// section shows instead of a timeline.
func (s *Service) stateAtLeave(ctx context.Context, roomID string, at int64, reader roomserver.RenderOptions, filter *filters.Filter) ([]json.RawMessage, error) {
	if at <= 0 {
		at, _ = s.store.Events.MaxPositionInRoom(ctx, roomID)
	}
	rows, err := s.store.Events.StateSnapshot(ctx, roomID, at)
	if err != nil {
		return nil, err
	}
	rows = filterEvents(rows, filter, stateSection)
	out := make([]json.RawMessage, 0, len(rows))
	for i := range rows {
		event, err := s.rooms.Render(ctx, &rows[i], reader)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, nil
}

// knockedRoom builds one room's `knock` entry.
func (s *Service) knockedRoom(ctx context.Context, roomID string, since int64) (*KnockedRoom, error) {
	state, err := s.rooms.StateEventRows(ctx, roomID, roomserver.RenderOptions{SkipTransactionID: true})
	if err != nil {
		// A knocked-on room whose state this server cannot read still has to appear, or the
		// client would never learn the knock exists.
		state = []json.RawMessage{}
	}
	return &KnockedRoom{KnockState: EventsBatch{Events: state}}, nil
}

// summary is the small room description a sync carries.
func (s *Service) summary(ctx context.Context, roomID string) (*Summary, error) {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	joined, invited := room.JoinedCount, room.InviteCount
	// Both counts travel because a client draws a room list from them and has no other way to
	// tell "a room with two people in it" from "a room waiting for one person to answer". The
	// invited count matters most in the moment it becomes zero: an invite that was accepted has
	// to stop looking like an outstanding invitation, or the client keeps offering a decision
	// that has already been made.
	return &Summary{InvitedMemberCount: &invited, JoinedMemberCount: &joined}, nil
}

// parseSince turns a sync token into a stream position.
func (s *Service) parseSince(token string) (int64, error) {
	switch strings.TrimSpace(token) {
	case "":
		return 0, nil
	case "s0", "0":
		return 0, nil
	}
	trimmed := strings.TrimPrefix(strings.TrimSpace(token), "s")
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%w: %q", ErrBadSinceToken, token)
	}
	return value, nil
}

// formatToken writes a sync or pagination token.
func formatToken(position int64) string {
	if position < 0 {
		position = 0
	}
	return "s" + strconv.FormatInt(position, 10)
}

// ErrUnknownFilter is returned when a filter ID names no stored filter.
var ErrUnknownFilter = errors.New("syncapi: unknown filter")

// BadRequestFor maps this package's errors onto the spec's error bodies, so that the handler
// stays a translation layer rather than a place where error semantics are invented.
func BadRequestFor(err error) *matrix.APIError {
	switch {
	case errors.Is(err, ErrBadSinceToken):
		// The spec is explicit that a bad `since` is an unknown-token 401: the client must
		// drop its token and start again, and only a 401 makes a well-written client do that.
		return matrix.NewAPIError(http.StatusUnauthorized, matrix.ErrUnknownToken,
			"Bad 'since' token parameter")
	case errors.Is(err, ErrUnknownFilter):
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidParam,
			"Unknown filter")
	case errors.Is(err, context.Canceled):
		return matrix.NewUnknown("request cancelled")
	default:
		return nil
	}
}

// pushRuleAccountData renders the account's rules as an account-data event, but only when they
// changed after the client's token.
//
// The rules are regenerated rather than stored twice, so there is a real question of when to send
// them: a client that already holds the current set must not be handed a duplicate on every sync,
// and a client whose rule write landed while its sync was in flight must not be told nothing
// happened. The rule service stamps the account when its rules move, and that stamp is what both
// halves compare against.
func (s *Service) pushRuleAccountData(ctx context.Context, localpart string, since int64) []json.RawMessage {
	if s.pushRules == nil {
		return nil
	}
	changed, err := s.pushRules.LastPosition(ctx, localpart)
	if err != nil {
		slog.Warn("sync: reading the push-rule change position", "error", err)
		return nil
	}
	// An initial sync always carries the rules: a client that has just been installed has no set
	// to compare against, and one that receives none concludes that the account is configured to
	// be silent.
	if since != 0 && changed <= since {
		return nil
	}
	content, err := s.pushRules.AccountData(ctx, localpart)
	if err != nil {
		slog.Warn("sync: rendering push rules", "error", err)
		return nil
	}
	event, err := json.Marshal(map[string]any{
		"type":    pushrules.EventType,
		"content": json.RawMessage(content),
	})
	if err != nil {
		slog.Warn("sync: encoding push rules", "error", err)
		return nil
	}
	return []json.RawMessage{event}
}
