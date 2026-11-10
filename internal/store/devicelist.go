package store

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// DeviceListChange records that one user's device list moved, for one person who should hear about it.
//
// Device lists are why a chat client knows to re-download a colleague's identity keys. The event that
// causes the move — a new login, a key upload, a join, a leave — has nothing to do with the person who
// cares, which is why this is an explicit log addressed to observers rather than something derived at
// read time: deriving it would mean asking, for every user in every room the reader is in, whether
// their keys changed since a token, which is a query per room per sync.
//
// One row per (change, observer) is wasteful in a room of thousands and correct in all the cases that
// matter here; the log is bounded by pruning old positions, the same way the to-device queue is.
type DeviceListChange struct {
	// ID is the log position.
	ID int64 `gorm:"primaryKey;autoIncrement" json:"-"`
	// UserID is the person whose device list moved, as a full user ID: a device list is a
	// cross-server concept, and an observer asks about a user, not a localpart.
	UserID string `gorm:"size:255;index:devicelist_idx" json:"user_id"`
	// Observer is the localpart that has to be told.
	Observer string `gorm:"size:255;index:devicelist_idx" json:"-"`
	// Left distinguishes "their devices changed" from "they are gone from a room you share",
	// which a client treats differently: the first means re-query, the second means throw away.
	Left bool `json:"-"`
	// StreamPos places the change in the same numbering as events, so a client's existing sync
	// token says which changes it has already been told.
	StreamPos int64 `gorm:"index" json:"-"`
}

// TableName pins the table name.
func (DeviceListChange) TableName() string { return "device_list_changes" }

// DeviceListRepo is the device-list log repository.
type DeviceListRepo interface {
	// Append writes one change and returns its position.
	Append(ctx context.Context, change *DeviceListChange) error
	// List returns an observer's changes after a position, oldest first.
	List(ctx context.Context, observer string, after int64) ([]DeviceListChange, error)
	// Range returns an observer's changes in a position range, which is what /keys/changes asks.
	Range(ctx context.Context, observer string, from, to int64) ([]DeviceListChange, error)
	// Prune drops changes at or below a position.
	Prune(ctx context.Context, atOrBelow int64) (int64, error)
}

// DeviceListRepoImpl is the GORM-backed [DeviceListRepo].
type DeviceListRepoImpl struct{ db *gorm.DB }

// NewDeviceListRepo builds the device-list log repository.
func NewDeviceListRepo(db *gorm.DB) *DeviceListRepoImpl { return &DeviceListRepoImpl{db: db} }

// Append implements [DeviceListRepo].
func (r *DeviceListRepoImpl) Append(ctx context.Context, change *DeviceListChange) error {
	if err := session(ctx, r.db).Create(change).Error; err != nil {
		return fmt.Errorf("recording a device-list change: %w", err)
	}
	return nil
}

// List implements [DeviceListRepo].
func (r *DeviceListRepoImpl) List(ctx context.Context, observer string, after int64) ([]DeviceListChange, error) {
	return r.Range(ctx, observer, after, 0)
}

// Range implements [DeviceListRepo].
func (r *DeviceListRepoImpl) Range(ctx context.Context, observer string, from, to int64) ([]DeviceListChange, error) {
	var rows []DeviceListChange
	query := session(ctx, r.db).Where("observer = ? AND stream_pos > ?", observer, from)
	if to > 0 {
		query = query.Where("stream_pos <= ?", to)
	}
	if err := query.Order("stream_pos ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading device-list changes of %q: %w", observer, err)
	}
	return rows, nil
}

// Prune implements [DeviceListRepo].
func (r *DeviceListRepoImpl) Prune(ctx context.Context, atOrBelow int64) (int64, error) {
	result := session(ctx, r.db).Where("stream_pos <= ?", atOrBelow).Delete(&DeviceListChange{})
	if result.Error != nil {
		return 0, fmt.Errorf("pruning device-list changes: %w", result.Error)
	}
	return result.RowsAffected, nil
}
