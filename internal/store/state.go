package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// StateKeyTuple identifies a state event's slot: its type and state key.
type StateKeyTuple struct {
	Type     string `gorm:"column:type;size:256" json:"type"`
	StateKey string `gorm:"column:state_key;size:256" json:"state_key"`
}

// Tuple returns the tuple for a (type, state key) pair.
func Tuple(eventType, stateKey string) StateKeyTuple {
	return StateKeyTuple{Type: eventType, StateKey: stateKey}
}

// RoomState is the current resolved state of a room: one row per state tuple, pointing at
// the event that won.
//
// Storing the resolved state rather than deriving it from the DAG on every read is the
// trade that makes /sync cheap. It is correct as long as every write goes through the room
// server (which owns the per-room lock), and it is why this table is rebuilt from the event
// rows if a migration ever needs it.
type RoomState struct {
	RoomID   string `gorm:"size:512;primaryKey:state_pk" json:"room_id"`
	Type     string `gorm:"size:256;primaryKey:state_pk" json:"type"`
	StateKey string `gorm:"size:256;primaryKey:state_pk" json:"state_key"`
	// EventID is the winning event.
	EventID string `gorm:"size:256;index" json:"event_id"`
	// StreamPos is the stream position of the winning event, so a client that last synced
	// at position P can be told which state entries it has not seen.
	StreamPos int64 `gorm:"index" json:"stream_pos"`
	// ReplacesEventID is the event this one replaced, surfaced as
	// `unsigned.replaces_state` together with `unsigned.prev_content`.
	ReplacesEventID string `gorm:"size:256" json:"replaces_event_id,omitempty"`
}

// TableName pins the table name.
func (RoomState) TableName() string { return "room_state" }

// StateRepo is the current-state repository.
type StateRepo interface {
	// Upsert sets one state tuple to an event, returning the event ID it replaced (empty
	// when the tuple was unset).
	Upsert(ctx context.Context, tx *gorm.DB, roomID, eventID, eventType, stateKey string, streamPos int64) (replacedEventID string, err error)
	// Get returns the entry for one tuple, or [ErrNotFound].
	Get(ctx context.Context, roomID, eventType, stateKey string) (*RoomState, error)
	// All returns every state entry of a room, ordered by type and state key.
	All(ctx context.Context, roomID string) ([]RoomState, error)
	// AllAfter returns the state entries of a room whose winning event arrived after a
	// stream position — the "state the client has not seen yet" set.
	AllAfter(ctx context.Context, roomID string, after int64) ([]RoomState, error)
	// Tuples returns the (type, state key) pairs currently set in a room.
	Tuples(ctx context.Context, roomID string) ([]StateKeyTuple, error)
	// DeleteByRoom removes all state (used when a room is upgraded away or forgotten).
	DeleteByRoom(ctx context.Context, roomID string) error
}

// StateRepoImpl is the GORM-backed [StateRepo].
type StateRepoImpl struct{ db *gorm.DB }

// Upsert implements [StateRepo].
func (r *StateRepoImpl) Upsert(ctx context.Context, tx *gorm.DB, roomID, eventID, eventType, stateKey string, streamPos int64) (string, error) {
	db := session(ctx, r.db)
	if tx != nil {
		db = tx.WithContext(ctx)
	}
	var existing RoomState
	err := db.WithContext(ctx).
		Where("room_id = ? AND type = ? AND state_key = ?", roomID, eventType, stateKey).
		First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		entry := RoomState{RoomID: roomID, Type: eventType, StateKey: stateKey, EventID: eventID, StreamPos: streamPos}
		if err := db.WithContext(ctx).Create(&entry).Error; err != nil {
			return "", fmt.Errorf("setting state %s/%q in %q: %w", eventType, stateKey, roomID, err)
		}
		return "", nil
	case err != nil:
		return "", fmt.Errorf("reading state %s in %q: %w", eventType, roomID, err)
	}

	replaced := existing.EventID
	updates := map[string]any{"event_id": eventID, "stream_pos": streamPos, "replaces_event_id": replaced}
	err = db.WithContext(ctx).Model(&RoomState{}).
		Where("room_id = ? AND type = ? AND state_key = ?", roomID, eventType, stateKey).
		Updates(updates).Error
	if err != nil {
		return "", fmt.Errorf("updating state %s in %q: %w", eventType, roomID, err)
	}
	return replaced, nil
}

// Get implements [StateRepo].
func (r *StateRepoImpl) Get(ctx context.Context, roomID, eventType, stateKey string) (*RoomState, error) {
	var entry RoomState
	err := session(ctx, r.db).
		Where("room_id = ? AND type = ? AND state_key = ?", roomID, eventType, stateKey).
		First(&entry).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("state %s/%q in %q: %w", eventType, stateKey, roomID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading state %s in %q: %w", eventType, roomID, err)
	}
	return &entry, nil
}

// All implements [StateRepo].
func (r *StateRepoImpl) All(ctx context.Context, roomID string) ([]RoomState, error) {
	var entries []RoomState
	err := session(ctx, r.db).
		Where("room_id = ?", roomID).
		Order("type ASC, state_key ASC").
		Find(&entries).Error
	if err != nil {
		return nil, fmt.Errorf("reading state of %q: %w", roomID, err)
	}
	return entries, nil
}

// AllAfter implements [StateRepo].
func (r *StateRepoImpl) AllAfter(ctx context.Context, roomID string, after int64) ([]RoomState, error) {
	var entries []RoomState
	err := session(ctx, r.db).
		Where("room_id = ? AND stream_pos > ?", roomID, after).
		Order("stream_pos ASC").
		Find(&entries).Error
	if err != nil {
		return nil, fmt.Errorf("reading changed state of %q: %w", roomID, err)
	}
	return entries, nil
}

// Tuples implements [StateRepo].
func (r *StateRepoImpl) Tuples(ctx context.Context, roomID string) ([]StateKeyTuple, error) {
	var tuples []StateKeyTuple
	err := session(ctx, r.db).Model(&RoomState{}).
		Where("room_id = ?", roomID).
		Select("type", "state_key").
		Find(&tuples).Error
	if err != nil {
		return nil, fmt.Errorf("reading state tuples of %q: %w", roomID, err)
	}
	return tuples, nil
}

// DeleteByRoom implements [StateRepo].
func (r *StateRepoImpl) DeleteByRoom(ctx context.Context, roomID string) error {
	if err := session(ctx, r.db).Where("room_id = ?", roomID).Delete(&RoomState{}).Error; err != nil {
		return fmt.Errorf("deleting state of %q: %w", roomID, err)
	}
	return nil
}

// NewStateRepo returns the [StateRepo] implementation.
func NewStateRepo(db *gorm.DB) StateRepo { return &StateRepoImpl{db: db} }
