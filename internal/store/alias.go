package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// RoomAlias maps a room alias to a room. The directory listing flag lives here as well:
// an alias is what the room directory publishes, and the canonical alias is a state event,
// so this table answers "can I join by this name" and "is it listed" together.
type RoomAlias struct {
	// Alias is the full alias, "#room:example.org".
	Alias string `gorm:"primaryKey;size:512" json:"alias"`
	// RoomID is the room the alias points at.
	RoomID string `gorm:"size:512;index" json:"room_id"`
	// CreatorLocalpart is who created the alias; only they or a room admin may delete it.
	CreatorLocalpart string `gorm:"size:255" json:"creator,omitempty"`
	// ListedInDirectory is the /directory/list/room/{roomId} flag (defaults to listed).
	ListedInDirectory bool `gorm:"index" json:"listed"`
	// CreatedMS is when the alias was created, in milliseconds since the epoch.
	CreatedMS int64 `json:"created_ms"`
	// RoomVersions is the room version the alias was created for; kept for the
	// /hierarchy response where the alias is the room's public name.
	RoomVersion string `gorm:"size:16" json:"-"`
}

// TableName pins the table name.
func (RoomAlias) TableName() string { return "room_aliases" }

// AliasRepo is the room-alias repository.
type AliasRepo interface {
	// Create points an alias at a room, failing with [ErrAlreadyExists] when the alias
	// is taken.
	Create(ctx context.Context, alias *RoomAlias) error
	// Get resolves an alias, or [ErrNotFound].
	Get(ctx context.Context, alias string) (*RoomAlias, error)
	// ListByRoom returns every alias of a room, canonical first.
	ListByRoom(ctx context.Context, roomID string) ([]RoomAlias, error)
	// Delete removes an alias; unknown aliases are not an error, matching the spec's
	// DELETE semantics for a non-existent alias.
	Delete(ctx context.Context, alias string) error
	// DeleteByRoom removes all aliases of a room.
	DeleteByRoom(ctx context.Context, roomID string) error
	// SetListedInDirectory updates the directory flag for every alias of a room.
	SetListedInDirectory(ctx context.Context, roomID string, listed bool) error
	// ListedRooms returns aliases that are listed in the public directory.
	ListedRooms(ctx context.Context, limit int, after string) ([]RoomAlias, error)
}

// AliasRepoImpl is the GORM-backed [AliasRepo].
type AliasRepoImpl struct{ db *gorm.DB }

// Create implements [AliasRepo].
func (r *AliasRepoImpl) Create(ctx context.Context, alias *RoomAlias) error {
	if alias.CreatedMS == 0 {
		alias.CreatedMS = nowMS()
	}
	err := session(ctx, r.db).Create(alias).Error
	switch {
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return fmt.Errorf("alias %q: %w", alias.Alias, ErrAlreadyExists)
	case err != nil:
		return fmt.Errorf("creating alias %q: %w", alias.Alias, err)
	}
	return nil
}

// Get implements [AliasRepo].
func (r *AliasRepoImpl) Get(ctx context.Context, alias string) (*RoomAlias, error) {
	var row RoomAlias
	err := session(ctx, r.db).Where("alias = ?", alias).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("alias %q: %w", alias, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading alias %q: %w", alias, err)
	}
	return &row, nil
}

// ListByRoom implements [AliasRepo].
func (r *AliasRepoImpl) ListByRoom(ctx context.Context, roomID string) ([]RoomAlias, error) {
	var rows []RoomAlias
	err := session(ctx, r.db).
		Where("room_id = ?", roomID).
		Order("alias ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("listing aliases of %q: %w", roomID, err)
	}
	return rows, nil
}

// Delete implements [AliasRepo].
func (r *AliasRepoImpl) Delete(ctx context.Context, alias string) error {
	if err := session(ctx, r.db).Where("alias = ?", alias).Delete(&RoomAlias{}).Error; err != nil {
		return fmt.Errorf("deleting alias %q: %w", alias, err)
	}
	return nil
}

// DeleteByRoom implements [AliasRepo].
func (r *AliasRepoImpl) DeleteByRoom(ctx context.Context, roomID string) error {
	if err := session(ctx, r.db).Where("room_id = ?", roomID).Delete(&RoomAlias{}).Error; err != nil {
		return fmt.Errorf("deleting aliases of %q: %w", roomID, err)
	}
	return nil
}

// SetListedInDirectory implements [AliasRepo].
func (r *AliasRepoImpl) SetListedInDirectory(ctx context.Context, roomID string, listed bool) error {
	res := session(ctx, r.db).Model(&RoomAlias{}).
		Where("room_id = ?", roomID).
		Update("listed_in_directory", listed)
	if res.Error != nil {
		return fmt.Errorf("setting directory flag for %q: %w", roomID, res.Error)
	}
	return nil
}

// ListedRooms implements [AliasRepo].
func (r *AliasRepoImpl) ListedRooms(ctx context.Context, limit int, after string) ([]RoomAlias, error) {
	if limit <= 0 {
		limit = 1000
	}
	q := session(ctx, r.db).
		Where("listed_in_directory = ?", true).
		Order("alias ASC").
		Limit(limit)
	if after != "" {
		q = q.Where("alias > ?", after)
	}
	var rows []RoomAlias
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing directory aliases: %w", err)
	}
	return rows, nil
}

// NewAliasRepo returns the [AliasRepo] implementation.
func NewAliasRepo(db *gorm.DB) AliasRepo { return &AliasRepoImpl{db: db} }
