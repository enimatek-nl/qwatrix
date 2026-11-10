package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// Room is one room this server participates in.
type Room struct {
	// ID is the full room ID, "!opaque:example.org". Rooms this server does not host keep
	// their foreign ID, which is why the column holds the whole thing.
	ID string `gorm:"primaryKey;size:512" json:"room_id"`
	// Version is the room version the room follows ("11"). Everything about event
	// structure, redaction and authorisation depends on it.
	Version string `gorm:"size:16" json:"version"`
	// CreatedMS is the origin_server_ts of the create event.
	CreatedMS int64 `json:"created_ms"`
	// CreatorLocalpart is the local part of whoever created the room, when the creator is
	// local. It is kept because /rooms/{id}/summary and upgrade rules want it without
	// parsing state.
	CreatorLocalpart string `gorm:"size:255;index" json:"creator,omitempty"`
	// UpgradedToRoomID is set when the room was upgraded to a successor.
	UpgradedToRoomID string `gorm:"size:512" json:"upgraded_to,omitempty"`
	// Left marks a room this server is no longer in; its history is kept for /messages
	// and for /forget, but it is excluded from /publicRooms and /hierarchy.
	Left bool `gorm:"index" json:"left"`
	// JoinedMS and LeftMS record the server's own membership span, used by the room
	// directory and by /timestamp_to_event.
	JoinedMS int64 `json:"joined_ms,omitempty"`
	LeftMS   int64 `json:"left_ms,omitempty"`
	// UnrestrictedJoinRule caches the room's join rule so the directory can filter
	// without loading state for every room.
	JoinRule string `gorm:"size:32;index" json:"join_rule,omitempty"`
	// HistoryVisibility caches m.room.history_visibility for the same reason.
	HistoryVisibility string `gorm:"size:32" json:"history_visibility,omitempty"`
	// GuestCanRead caches m.room.guest_access ("can_answer"/"can_join"/"forbidden").
	GuestAccess string `gorm:"size:16" json:"guest_access,omitempty"`
	// Predecessor and Successor record the upgrade chain.
	PredecessorRoomID string `gorm:"size:512" json:"predecessor,omitempty"`

	// Name, Topic, AvatarURL and CanonicalAlias mirror the corresponding state events so a
	// room list, /publicRooms and /hierarchy can be built without loading state per room.
	// They are caches: the events remain the truth, and the room server rewrites them
	// whenever the event changes.
	Name           string `gorm:"size:255" json:"name,omitempty"`
	Topic          string `gorm:"size:1000" json:"topic,omitempty"`
	AvatarURL      string `gorm:"size:500" json:"avatar_url,omitempty"`
	CanonicalAlias string `gorm:"size:512" json:"canonical_alias,omitempty"`
	// Encrypted records that the room has an m.room.encryption event.
	Encrypted bool `json:"encrypted,omitempty"`
	// Publishable records whether the room is offered in the public room directory. It is
	// a room property rather than a derived one because the spec lets an operator publish
	// and unpublish a room independently of its join rule, and a room with no alias still
	// has to have an answer.
	Publishable bool `gorm:"index" json:"publishable,omitempty"`
	// RoomType is the `type` of the create event ("m.space" for a space, empty for an
	// ordinary room). It is cached because /hierarchy and /publicRooms filter on it and
	// reading the create event per room would dominate those queries.
	RoomType string `gorm:"size:255;index" json:"room_type,omitempty"`
	// JoinedCount and InviteCount are denormalised member counts, for the same reason the
	// descriptive caches above exist.
	JoinedCount int64 `json:"joined_members,omitempty"`
	InviteCount int64 `json:"invited_members,omitempty"`
}

// TableName pins the table name.
func (Room) TableName() string { return "rooms" }

// RoomRepo is the room repository.
type RoomRepo interface {
	Create(ctx context.Context, room *Room) error
	Get(ctx context.Context, roomID string) (*Room, error)
	Update(ctx context.Context, roomID string, updates map[string]any) error
	// ListLocal returns every room this server is in (not left), ordered by room ID with
	// a cursor for paging.
	ListLocal(ctx context.Context, limit int, after string) ([]Room, error)
	// ListWithJoinRule returns rooms whose cached join rule is one of the given values
	// and which are publishable, for /publicRooms.
	ListWithJoinRule(ctx context.Context, joinRules []string, limit int, after string) ([]Room, error)
}

// RoomRepoImpl is the GORM-backed [RoomRepo].
type RoomRepoImpl struct{ db *gorm.DB }

// Create implements [RoomRepo].
func (r *RoomRepoImpl) Create(ctx context.Context, room *Room) error {
	if room.CreatedMS == 0 {
		room.CreatedMS = nowMS()
	}
	if err := session(ctx, r.db).Create(room).Error; err != nil {
		return fmt.Errorf("creating room %q: %w", room.ID, err)
	}
	return nil
}

// Get implements [RoomRepo].
func (r *RoomRepoImpl) Get(ctx context.Context, roomID string) (*Room, error) {
	var room Room
	err := session(ctx, r.db).Where("id = ?", roomID).First(&room).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("room %q: %w", roomID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading room %q: %w", roomID, err)
	}
	return &room, nil
}

// Update implements [RoomRepo].
func (r *RoomRepoImpl) Update(ctx context.Context, roomID string, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	res := session(ctx, r.db).Model(&Room{}).Where("id = ?", roomID).Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("updating room %q: %w", roomID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("room %q: %w", roomID, ErrNotFound)
	}
	return nil
}

// ListLocal implements [RoomRepo].
func (r *RoomRepoImpl) ListLocal(ctx context.Context, limit int, after string) ([]Room, error) {
	if limit <= 0 {
		limit = 500
	}
	q := session(ctx, r.db).Where("left = ?", false).Order("id ASC").Limit(limit)
	if after != "" {
		q = q.Where("id > ?", after)
	}
	var rooms []Room
	if err := q.Find(&rooms).Error; err != nil {
		return nil, fmt.Errorf("listing rooms: %w", err)
	}
	return rooms, nil
}

// ListWithJoinRule implements [RoomRepo].
func (r *RoomRepoImpl) ListWithJoinRule(ctx context.Context, joinRules []string, limit int, after string) ([]Room, error) {
	if limit <= 0 {
		limit = 1000
	}
	q := session(ctx, r.db).
		Where("left = ?", false).
		Order("id ASC").
		Limit(limit)
	if len(joinRules) > 0 {
		q = q.Where("join_rule IN ?", joinRules)
	}
	if after != "" {
		q = q.Where("id > ?", after)
	}
	var rooms []Room
	if err := q.Find(&rooms).Error; err != nil {
		return nil, fmt.Errorf("listing publishable rooms: %w", err)
	}
	return rooms, nil
}

// NewRoomRepo returns the [RoomRepo] implementation.
func NewRoomRepo(db *gorm.DB) RoomRepo { return &RoomRepoImpl{db: db} }
