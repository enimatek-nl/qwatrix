package store

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// ToDeviceMessage is a message sent to a device rather than to a room.
//
// To-device messages are the transport for end-to-end key sharing: a room's members never see them,
// they are addressed to one device of one user, and they are not part of any timeline. That makes
// them a queue rather than history, and the queue needs its own monotone position so a device's sync
// token can say "I have taken everything up to here".
type ToDeviceMessage struct {
	// ID is the queue position, which doubles as the stream position a device's token refers to.
	ID int64 `gorm:"primaryKey;autoIncrement" json:"-"`
	// Type is the event type the recipient sees, e.g. m.room.encrypted.
	Type string `gorm:"size:255;index" json:"type"`
	// Sender is the user ID the message claims to come from. It is stored rather than trusted:
	// the recipient decides what to make of who sent it.
	Sender          string `gorm:"size:255;index" json:"-"`
	Recipient       string `gorm:"size:255;index:tokd_pk" json:"-"`
	RecipientDevice string `gorm:"size:255;index:tokd_pk" json:"-"`
	Content         string `gorm:"type:text" json:"-"`
	// StreamPos mirrors ID into the same numbering as events so one /sync token orders both.
	StreamPos int64 `gorm:"index" json:"-"`
}

// TableName pins the table name.
func (ToDeviceMessage) TableName() string { return "to_device_messages" }

// ToDeviceRepo is the to-device queue repository.
type ToDeviceRepo interface {
	// Append queues one message and returns its position.
	Append(ctx context.Context, message *ToDeviceMessage) (int64, error)
	// List returns a device's messages after a position, oldest first. A device ID of "*"
	// matches any device of the user, which is what a sender asks for when it does not know
	// which of the recipient's devices are still around.
	List(ctx context.Context, localpart, deviceID string, after int64, limit int) ([]ToDeviceMessage, error)
	// LastPosition returns the highest position a device has been given, or the value its token
	// should carry when there is nothing newer.
	LastPosition(ctx context.Context, localpart, deviceID string) (int64, error)
	// Prune removes messages a device has demonstrably moved past.
	Prune(ctx context.Context, below int64) (int64, error)
}

// ToDeviceRepoImpl is the GORM-backed [ToDeviceRepo].
type ToDeviceRepoImpl struct{ db *gorm.DB }

// NewToDeviceRepo builds the to-device queue repository.
func NewToDeviceRepo(db *gorm.DB) *ToDeviceRepoImpl { return &ToDeviceRepoImpl{db: db} }

// Append implements [ToDeviceRepo].
func (r *ToDeviceRepoImpl) Append(ctx context.Context, message *ToDeviceMessage) (int64, error) {
	if err := session(ctx, r.db).Create(message).Error; err != nil {
		return 0, fmt.Errorf("queueing to-device message: %w", err)
	}
	if message.StreamPos == 0 {
		message.StreamPos = message.ID
		if err := session(ctx, r.db).Model(&ToDeviceMessage{}).
			Where("id = ?", message.ID).Update("stream_pos", message.StreamPos).Error; err != nil {
			return 0, fmt.Errorf("stamping to-device message: %w", err)
		}
	}
	return message.ID, nil
}

// List implements [ToDeviceRepo].
func (r *ToDeviceRepoImpl) List(ctx context.Context, localpart, deviceID string, after int64, limit int) ([]ToDeviceMessage, error) {
	var rows []ToDeviceMessage
	query := session(ctx, r.db).
		Where("recipient = ? AND stream_pos > ?", localpart, after).
		Order("stream_pos ASC")
	if deviceID != "" {
		// A device asks for its own mail and for anything addressed to the user as a whole,
		// which is how a sender that does not know the recipient's device IDs reaches them.
		query = query.Where("recipient_device IN ?", []string{deviceID, "*"})
	}
	if limit > 0 {
		query = query.Limit(limit)
	}
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading to-device messages of %q: %w", localpart, err)
	}
	return rows, nil
}

// LastPosition implements [ToDeviceRepo].
func (r *ToDeviceRepoImpl) LastPosition(ctx context.Context, localpart, deviceID string) (int64, error) {
	var position int64
	query := session(ctx, r.db).Model(&ToDeviceMessage{}).Where("recipient = ?", localpart)
	if deviceID != "" {
		query = query.Where("recipient_device IN ?", []string{deviceID, "*"})
	}
	// COALESCE because MAX over no rows is NULL, and NULL does not fit into a Go integer.
	if err := query.Select("COALESCE(MAX(stream_pos), 0)").Scan(&position).Error; err != nil {
		return 0, fmt.Errorf("reading the to-device position of %q: %w", localpart, err)
	}
	return position, nil
}

// Prune implements [ToDeviceRepo].
func (r *ToDeviceRepoImpl) Prune(ctx context.Context, below int64) (int64, error) {
	result := session(ctx, r.db).Where("stream_pos < ?", below).Delete(&ToDeviceMessage{})
	if result.Error != nil {
		return 0, fmt.Errorf("pruning to-device messages: %w", result.Error)
	}
	return result.RowsAffected, nil
}
