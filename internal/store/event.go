package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"gorm.io/gorm"
)

// EventRow is one persisted Matrix event.
//
// ID is the *stream position*: a global, monotonic, gap-tolerant counter that /sync and
// /messages page over. It is not the DAG order — that is Depth/PrevEvents — and the two
// deliberately differ, because a client's notion of "what happened since my last sync" is
// arrival order, while a server's notion of history is graph order.
//
// JSON holds the canonical, signed event exactly as federation would send it, with no
// `unsigned`. Response-time decoration (age, transaction_id, prev_content, redacts) is
// applied on the way out, so what is stored is what the event ID commits to.
type EventRow struct {
	// ID is the stream position (SQLite rowid).
	ID int64 `gorm:"primaryKey;autoIncrement" json:"stream_pos"`
	// EventID is the event ID as it appears on the wire ("$opaque" or "$hash:origin").
	EventID string `gorm:"uniqueIndex;size:256" json:"event_id"`
	// RoomID is the room the event belongs to.
	RoomID string `gorm:"size:512;index:idx_events_room_id" json:"room_id"`
	// Type and StateKey identify a state event's tuple; StateKey is nil for messages.
	Type     string  `gorm:"size:256;index:idx_events_room_state" json:"type"`
	StateKey *string `gorm:"size:256;index:idx_events_room_state" json:"state_key,omitempty"`
	// Sender is the full user ID of the originator.
	Sender string `gorm:"size:512;index" json:"sender"`
	// Depth is the event's depth in the DAG (create event is 1).
	Depth int64 `json:"depth"`
	// OriginServerTS is the origin server's timestamp in milliseconds.
	OriginServerTS int64 `gorm:"index" json:"origin_server_ts"`
	// ReceivedMS is when this server saw the event, used by /timestamp_to_event and for
	// age fallbacks on backfilled events.
	ReceivedMS int64 `json:"received_ms"`
	// Redacts is the event ID this event redacts, for m.room.redaction.
	Redacts string `gorm:"size:256;index" json:"redacts,omitempty"`
	// PrevEvents and AuthEvents are JSON arrays of event IDs. They are stored as text
	// because they are only read when the event itself is sent to a client or a peer.
	PrevEvents string `gorm:"type:text" json:"-"`
	AuthEvents string `gorm:"type:text" json:"-"`
	// RoomVersion is the room version the event was created in. It is denormalised from
	// the room so that parsing an event needs no join, and so that an event keeps parsing
	// the same way even if the room row is forgotten after /forget.
	RoomVersion string `gorm:"size:16;index" json:"-"`
	// JSON is the canonical signed event.
	JSON string `gorm:"type:text" json:"-"`
	// TransactionID is the client transaction ID that produced this event, replayed in
	// `unsigned.transaction_id` for the sender's own echo.
	TransactionID string `gorm:"size:256" json:"-"`
	// DeviceID is which device sent the event. It is stored with TransactionID because
	// `unsigned.transaction_id` may only be echoed back to that device: a transaction ID
	// is scoped per device, and leaking another device's IDs would let a client infer
	// things about its sibling sessions.
	DeviceID string `gorm:"size:64;index" json:"-"`
	// RelatesToEventID and RelType carry the target of an event's m.relates_to, copied out of
	// the event content when the event is written.
	//
	// They exist because /relations is a query, and the alternative is reading the content of
	// every event in the room to find the handful that mention one ID. The cost is a rule to keep:
	// the columns describe the event as it arrived, and a redaction does not clear them — the
	// relation is a fact about what somebody replied to, and deleting it would silently amputate
	// a thread.
	RelatesToEventID string `gorm:"size:255;index:event_rel_idx" json:"-"`
	RelType          string `gorm:"size:255;index:event_rel_idx" json:"-"`
	// RejectedReason is empty for accepted events. A rejected event stays in the
	// database (its peers sent it) but is excluded from every timeline and state query.
	RejectedReason string `gorm:"size:256;index" json:"-"`
	// StateSnapshotOnly marks an event stored purely as a state entry (e.g. state
	// received over federation for a room we cannot see the history of).
	StateSnapshotOnly bool `gorm:"index" json:"-"`
	// Redacted records that this row's JSON is already the redacted form.
	Redacted bool `json:"redacted"`
	// Detached marks an event this server was told about without being told how it connects to the
	// room: history fetched from a peer in answer to "where was this room last March", or a window handed
	// to it by /backfill. Other homeservers call these outliers.
	//
	// They are stored because the alternative is worse twice over: refetching the same history on every
	// question, and answering "I have never heard of that event" about an event the server is holding.
	// They are marked because three things would be untrue if they were stored as ordinary events — that
	// this server judged them against the room's rules (it had no state at that point to judge them
	// with), that they lead up to wherever the room is now, and that a client watching for new activity
	// should be woken by news from last year.
	//
	// So the queries divide by this flag: the ones that page around a room's history include detached
	// events (that is why they were fetched), and the ones that decide what is new, what the room's state
	// is, and where its DAG ends do not.
	Detached bool `gorm:"index;column:detached" json:"-"`
}

// TableName pins the table name.
func (EventRow) TableName() string { return "events" }

// IsRejected reports whether the event is excluded from timelines and state.
func (e *EventRow) IsRejected() bool { return e != nil && e.RejectedReason != "" }

// EventQuery narrows an event-range read. The zero value means "everything, newest last".
type EventQuery struct {
	// RoomIDs restricts the read to one or more rooms; empty means every room.
	RoomIDs []string
	// After is exclusive: only events with stream position greater than After.
	After int64
	// Before is exclusive: only events with stream position less than Before.
	Before int64
	// Limit caps the result; 0 means no cap (use with care).
	Limit int
	// Types / NotTypes / StateKeys filter by event type and state-key presence.
	Types    []string
	NotTypes []string
	// Senders / NotSenders filter by originator (used by ignored users).
	Senders    []string
	NotSenders []string
	// RelatesToEventID / RelType restrict the read to the relations of one event, optionally of
	// one relation type, which is what GET /relations is.
	RelatesToEventID string
	RelType          string
	// IncludeRedacted keeps events whose JSON has been redacted (they are kept by
	// default; the flag exists for /redact, which must see the event it redacts even if
	// already redacted).
	IncludeStateSnapshotOnly bool
	// IncludeDetached asks for history this server fetched rather than received: /messages, /context and
	// a peer's /backfill want it, and /sync and unread badges must not treat it as news. Left false,
	// because a query that has not thought about detached events should tell a client nothing new rather
	// than wake it with last year.
	IncludeDetached bool
	// OrderDesc returns newest first instead of oldest first.
	OrderDesc bool
}

// EventRepo is the event repository: the read side of /sync, /messages and every
// timeline-shaped endpoint.
type EventRepo interface {
	// Insert stores an event and returns it with ID (the stream position) filled in. It
	// must be called inside a write transaction when the caller is also updating state.
	Insert(ctx context.Context, tx *gorm.DB, row *EventRow) (*EventRow, error)
	// ByEventID returns one event, or [ErrNotFound].
	ByEventID(ctx context.Context, eventID string) (*EventRow, error)
	// ByEventIDs returns the listed events in stream order, skipping unknown IDs.
	ByEventIDs(ctx context.Context, eventIDs []string) ([]EventRow, error)
	// Range returns events matching q in stream order.
	Range(ctx context.Context, q EventQuery) ([]EventRow, error)
	// NearestByTimestamp finds the event closest to a moment in a room's history.
	//
	// Forward asks for the earliest event stamped at or after the moment; backward asks for the latest
	// one at or before it. Those are two questions, and a room whose events share a millisecond has two
	// different right answers — so the tie is broken by insertion order, oldest first going forward and
	// newest first going back, rather than by whatever the index happens to yield.
	//
	// maxPosition, when positive, is a ceiling on the row's stream position: a former member's view of a
	// room ends where their membership did, and a lookup that walked past it would answer a question
	// about history that reader cannot have.
	NearestByTimestamp(ctx context.Context, roomID string, ts int64, forward bool, maxPosition int64) (*EventRow, error)
	// Count returns how many events match q.
	Count(ctx context.Context, q EventQuery) (int64, error)
	// MaxPosition returns the highest stream position in the database, or 0 when empty.
	// FirstPositionOfState returns the stream position of the earliest event of a state tuple
	// in a room, or 0 when the room never had one.
	//
	// Events are never deleted, so the earliest member event of a user is the *first* time
	// they were ever mentioned in the room — which is what `history_visibility: invited` needs:
	// the bound is the invite, not the current membership row, because an invitee may read what
	// was sent between being invited and joining.
	FirstPositionOfState(ctx context.Context, roomID, eventType, stateKey string) (int64, error)
	// MembershipTrail lists every membership the named user has had in a room, oldest first.
	MembershipTrail(ctx context.Context, roomID, stateKey string) ([]MembershipChange, error)
	// StateAt returns the event that held a state tuple at or before a stream position, or
	// [ErrNotFound].
	StateAt(ctx context.Context, roomID, eventType, stateKey string, at int64) (*EventRow, error)
	// StateSnapshot returns the room's state as it stood at a stream position.
	StateSnapshot(ctx context.Context, roomID string, at int64) ([]EventRow, error)
	MaxPosition(ctx context.Context) (int64, error)
	// MaxPositionInRoom returns the highest stream position of a room, or 0.
	MaxPositionInRoom(ctx context.Context, roomID string) (int64, error)
	// MinPositionInRoom returns the lowest stream position of a room, or 0.
	MinPositionInRoom(ctx context.Context, roomID string) (int64, error)
	// Tip returns the event with the greatest depth in a room, which is where the next
	// locally-originated event attaches its prev_events.
	Tip(ctx context.Context, roomID string) (*EventRow, error)
	// MarkRejected records a rejection reason for an event (federation deny-lists,
	// failed auth chain), which removes it from every read path.
	MarkRejected(ctx context.Context, eventID, reason string) error
	// ReplaceJSON overwrites an event's JSON and marks it redacted, used by /redact.
	ReplaceJSON(ctx context.Context, eventID, json string) error
}

// EventRepoImpl is the GORM-backed [EventRepo].
type EventRepoImpl struct{ db *gorm.DB }

// Insert implements [EventRepo].
func (r *EventRepoImpl) Insert(ctx context.Context, tx *gorm.DB, row *EventRow) (*EventRow, error) {
	if row.ReceivedMS == 0 {
		row.ReceivedMS = nowMS()
	}
	db := session(ctx, r.db)
	if tx != nil {
		db = tx.WithContext(ctx)
	}
	if err := db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, fmt.Errorf("inserting event %q: %w", row.EventID, err)
	}
	return row, nil
}

// ByEventID implements [EventRepo].
func (r *EventRepoImpl) ByEventID(ctx context.Context, eventID string) (*EventRow, error) {
	var row EventRow
	err := session(ctx, r.db).Where("event_id = ?", eventID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("event %q: %w", eventID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading event %q: %w", eventID, err)
	}
	return &row, nil
}

// ByEventIDs implements [EventRepo].
func (r *EventRepoImpl) ByEventIDs(ctx context.Context, eventIDs []string) ([]EventRow, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	var rows []EventRow
	err := session(ctx, r.db).
		Where("event_id IN ?", eventIDs).
		Order("id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading %d events: %w", len(eventIDs), err)
	}
	return rows, nil
}

// apply adds the query's predicates to a statement.
func (q EventQuery) apply(db *gorm.DB) *gorm.DB {
	if len(q.RoomIDs) == 1 {
		db = db.Where("room_id = ?", q.RoomIDs[0])
	} else if len(q.RoomIDs) > 1 {
		db = db.Where("room_id IN ?", q.RoomIDs)
	}
	if q.After > 0 {
		db = db.Where("id > ?", q.After)
	}
	if q.Before > 0 {
		db = db.Where("id < ?", q.Before)
	}
	if len(q.Types) > 0 {
		db = db.Where("type IN ?", q.Types)
	}
	if len(q.NotTypes) > 0 {
		db = db.Where("type NOT IN ?", q.NotTypes)
	}
	if len(q.Senders) > 0 {
		db = db.Where("sender IN ?", q.Senders)
	}
	if len(q.NotSenders) > 0 {
		db = db.Where("sender NOT IN ?", q.NotSenders)
	}
	if q.RelatesToEventID != "" {
		db = db.Where("relates_to_event_id = ?", q.RelatesToEventID)
	}
	if q.RelType != "" {
		db = db.Where("rel_type = ?", q.RelType)
	}
	if !q.IncludeStateSnapshotOnly {
		db = db.Where("state_snapshot_only = ?", false)
	}
	if !q.IncludeDetached {
		db = db.Where("detached = ?", false)
	}
	// Rejected events are invisible: they were received but never accepted.
	db = db.Where("rejected_reason = '' OR rejected_reason IS NULL")
	if q.Limit > 0 {
		db = db.Limit(q.Limit)
	}
	if q.OrderDesc {
		return db.Order("id DESC")
	}
	return db.Order("id ASC")
}

// Range implements [EventRepo].
func (r *EventRepoImpl) Range(ctx context.Context, q EventQuery) ([]EventRow, error) {
	var rows []EventRow
	if err := q.apply(session(ctx, r.db)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ranging events: %w", err)
	}
	return rows, nil
}

// NearestByTimestamp implements [EventRepo].
// A search that reaches a detached event has reached history some peer handed over rather than history
// this server witnessed. That is the right answer to a question about the past, and it is why this query
// is the one place the flag is not consulted: a server that fetched an event to answer "where was this
// room then" has to be able to find it again when the question is asked twice.
func (r *EventRepoImpl) NearestByTimestamp(ctx context.Context, roomID string, ts int64,
	forward bool, maxPosition int64) (*EventRow, error) {
	db := session(ctx, r.db).Model(&EventRow{}).
		Where("room_id = ?", roomID).
		// The same two exclusions every history read makes: an event this server does not accept is
		// not history, and a state snapshot written for a left room is a bookkeeping copy of state,
		// not something that happened.
		Where("state_snapshot_only = ?", false).
		Where("rejected_reason = '' OR rejected_reason IS NULL")
	if forward {
		db = db.Where("origin_server_ts >= ?", ts).Order("origin_server_ts ASC, id ASC")
	} else {
		db = db.Where("origin_server_ts <= ?", ts).Order("origin_server_ts DESC, id DESC")
	}
	if maxPosition > 0 {
		db = db.Where("id <= ?", maxPosition)
	}
	var rows []EventRow
	if err := db.Limit(1).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("looking for an event near timestamp %d in %q: %w", ts, roomID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("event near timestamp %d in %q: %w", ts, roomID, ErrNotFound)
	}
	return &rows[0], nil
}

// Count implements [EventRepo].
func (r *EventRepoImpl) Count(ctx context.Context, q EventQuery) (int64, error) {
	var count int64
	// LIMIT is a row selector, not a counter: drop it before counting.
	q.Limit = 0
	q.OrderDesc = false
	if err := q.apply(session(ctx, r.db).Model(&EventRow{})).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("counting events: %w", err)
	}
	return count, nil
}

// MaxPosition implements [EventRepo].
// FirstPositionOfState implements [EventRepo].
func (r *EventRepoImpl) FirstPositionOfState(ctx context.Context, roomID, eventType, stateKey string) (int64, error) {
	var position int64
	// COALESCE because an empty aggregate scans into a NULL that has no Go home; and the
	// target must be a plain int64, since scanning an aggregate through a pointer leaves it nil.
	err := session(ctx, r.db).Model(&EventRow{}).
		Where("room_id = ? AND type = ? AND state_key = ?", roomID, eventType, stateKey).
		Select("COALESCE(MIN(id), 0)").Scan(&position).Error
	if err != nil {
		return 0, fmt.Errorf("reading first position of %s/%q in %q: %w", eventType, stateKey, roomID, err)
	}
	return position, nil
}

func (r *EventRepoImpl) MaxPosition(ctx context.Context) (int64, error) {
	var pos int64
	err := session(ctx, r.db).Model(&EventRow{}).Select("COALESCE(MAX(id), 0)").Scan(&pos).Error
	if err != nil {
		return 0, fmt.Errorf("reading maximum stream position: %w", err)
	}
	return pos, nil
}

// MaxPositionInRoom implements [EventRepo].
func (r *EventRepoImpl) MaxPositionInRoom(ctx context.Context, roomID string) (int64, error) {
	var pos int64
	err := session(ctx, r.db).Model(&EventRow{}).
		Where("room_id = ?", roomID).
		Select("COALESCE(MAX(id), 0)").Scan(&pos).Error
	if err != nil {
		return 0, fmt.Errorf("reading maximum stream position of %q: %w", roomID, err)
	}
	return pos, nil
}

// Tip implements [EventRepo].
func (r *EventRepoImpl) Tip(ctx context.Context, roomID string) (*EventRow, error) {
	var row EventRow
	err := session(ctx, r.db).
		Where("room_id = ? AND (rejected_reason = '' OR rejected_reason IS NULL) AND detached = ?", roomID, false).
		Order("depth DESC, id DESC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("tip of room %q: %w", roomID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading tip of room %q: %w", roomID, err)
	}
	return &row, nil
}

// MarkRejected implements [EventRepo].
func (r *EventRepoImpl) MarkRejected(ctx context.Context, eventID, reason string) error {
	res := session(ctx, r.db).Model(&EventRow{}).
		Where("event_id = ?", eventID).
		Update("rejected_reason", reason)
	if res.Error != nil {
		return fmt.Errorf("rejecting event %q: %w", eventID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("event %q: %w", eventID, ErrNotFound)
	}
	return nil
}

// ReplaceJSON implements [EventRepo].
func (r *EventRepoImpl) ReplaceJSON(ctx context.Context, eventID, json string) error {
	res := session(ctx, r.db).Model(&EventRow{}).
		Where("event_id = ?", eventID).
		Updates(map[string]any{"json": json, "redacted": true})
	if res.Error != nil {
		return fmt.Errorf("replacing JSON of event %q: %w", eventID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("event %q: %w", eventID, ErrNotFound)
	}
	return nil
}

// NewEventRepo returns the [EventRepo] implementation.
func NewEventRepo(db *gorm.DB) EventRepo { return &EventRepoImpl{db: db} }

// MembershipChange is one point at which a user's membership in a room changed.
type MembershipChange struct {
	// Position is the stream position of the `m.room.member` event that caused the change,
	// which orders it against every other event in the room.
	Position int64
	// Membership is what that event said in `content.membership`.
	Membership string
}

// MembershipTrail implements [EventRepo].
//
// The history is read from the events and not from the `memberships` table, because that table
// stores only the current answer: it is a cache of "is @bob here now", while MSC4115 asks
// "was @bob here when this event was written", which is a question about a sequence of state
// changes. Events are never deleted here — a redaction removes content, not the event — so the
// trail is complete, and `membership` survives redaction in every room version because the
// redaction rules keep it.
//
// The membership is decoded in Go rather than with json_extract: the number of member events per
// user per room is a handful, and a JSON function is a promise about the SQLite build rather
// than a guarantee of the driver.
func (r *EventRepoImpl) MembershipTrail(ctx context.Context, roomID, stateKey string) ([]MembershipChange, error) {
	var rows []EventRow
	err := session(ctx, r.db).Model(&EventRow{}).
		Select("id", "json").
		Where("room_id = ? AND type = ? AND state_key = ?", roomID, "m.room.member", stateKey).
		Order("id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading membership trail in %q: %w", roomID, err)
	}
	changes := make([]MembershipChange, 0, len(rows))
	for _, row := range rows {
		var document struct {
			Content struct {
				Membership string `json:"membership"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(row.JSON), &document); err != nil {
			continue // an event that cannot be parsed cannot move the membership either
		}
		if document.Content.Membership == "" {
			continue
		}
		changes = append(changes, MembershipChange{
			Position:   row.ID,
			Membership: document.Content.Membership,
		})
	}
	return changes, nil
}

// StateAt implements [EventRepo].
//
// This is "what did the room say about this key back then", and it needs no history table:
// events are append-only, so the superseded value of a state tuple is still the row it was
// written into, and the answer is the newest such row at or before the position in question.
func (r *EventRepoImpl) StateAt(ctx context.Context, roomID, eventType, stateKey string, at int64) (*EventRow, error) {
	var row EventRow
	err := session(ctx, r.db).Model(&EventRow{}).
		Where("room_id = ? AND type = ? AND state_key = ? AND id <= ?", roomID, eventType, stateKey, at).
		Order("id DESC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("state %s/%q of %q at %d: %w", eventType, stateKey, roomID, at, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading state %s/%q of %q at %d: %w", eventType, stateKey, roomID, at, err)
	}
	return &row, nil
}

// StateSnapshot implements [EventRepo].
//
// The room's state at a past point is rebuilt by replaying its state events in stream order and
// keeping the last value of each tuple. That is the same rule the live state table follows, and
// it is why no separate history table is needed: the events *are* the history, and nothing here
// deletes them.
//
// It reads every state event up to the position rather than only the current ones, which is more
// rows than a live-state read. That is the cost of a question the live table cannot answer, and
// it is paid only by a former member reading a room they left — not by a joined member's `/sync`,
// which is why there is no cache table for it.
func (r *EventRepoImpl) StateSnapshot(ctx context.Context, roomID string, at int64) ([]EventRow, error) {
	var rows []EventRow
	err := session(ctx, r.db).Model(&EventRow{}).
		Where("room_id = ? AND state_key IS NOT NULL AND id <= ?", roomID, at).
		Order("id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading state of %q at %d: %w", roomID, at, err)
	}
	latest := map[string]EventRow{}
	keys := []string{}
	for _, row := range rows {
		if row.StateKey == nil {
			continue
		}
		key := row.Type + "\x00" + *row.StateKey
		if _, seen := latest[key]; !seen {
			keys = append(keys, key)
		}
		latest[key] = row
	}
	sort.Strings(keys)
	out := make([]EventRow, 0, len(keys))
	for _, key := range keys {
		out = append(out, latest[key])
	}
	return out, nil
}

// MinPositionInRoom implements [EventRepo].
func (r *EventRepoImpl) MinPositionInRoom(ctx context.Context, roomID string) (int64, error) {
	var position int64
	err := session(ctx, r.db).Model(&EventRow{}).
		Where("room_id = ?", roomID).
		Select("COALESCE(MIN(id), 0)").Scan(&position).Error
	if err != nil {
		return 0, fmt.Errorf("reading the first position of %q: %w", roomID, err)
	}
	return position, nil
}
