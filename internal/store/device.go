package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// Device is one logged-in session of a user. It is what an access token belongs to, and
// what end-to-end encryption keys are claimed against.
type Device struct {
	// ID is the device identifier, unique across the whole server (the spec only
	// requires per-user uniqueness, but a global key is simpler and never ambiguous in
	// federation or device-list updates).
	ID string `gorm:"primaryKey;size:64" json:"device_id"`
	// Localpart is the owning user's local part.
	Localpart string `gorm:"index;size:255" json:"localpart"`
	// DisplayName is the client-supplied device name shown in session lists.
	DisplayName string `gorm:"size:256" json:"display_name,omitempty"`
	// ClientID and ClientIP record where the device first appeared, which is what a
	// "log in everywhere else" screen shows.
	ClientID string `gorm:"size:512" json:"client_id,omitempty"`
	ClientIP string `gorm:"size:128" json:"client_ip,omitempty"`
	// CreatedMS and LastSeenMS are milliseconds since the epoch.
	CreatedMS  int64 `json:"created_ms"`
	LastSeenMS int64 `json:"last_seen_ms"`
	// LastSeenUA is the user agent that last used this token.
	LastSeenUA string `gorm:"size:512" json:"last_seen_user_agent,omitempty"`
	// ToDevicePos is how far this device has been served from the to-device queue.
	//
	// The cursor lives on the server rather than in the client's sync token. The specification
	// would have the token carry it, which needs a token naming two streams at once, and this
	// server's tokens are plain integers shared with /messages and /context — every one of those
	// readers would have to learn a second syntax to gain a redelivery guarantee nobody can
	// observe. What this does mean is documented at [todevice.Service]: a device that receives a
	// message and then loses it is not handed it again.
	ToDevicePos int64 `gorm:"column:to_device_pos" json:"-"`
}

// TableName pins the table name.
func (Device) TableName() string { return "devices" }

// DeviceRepo is the device repository.
type DeviceRepo interface {
	// Create inserts a device.
	Create(ctx context.Context, device *Device) error
	// Get returns one device by ID, or [ErrNotFound].
	Get(ctx context.Context, deviceID string) (*Device, error)
	// List returns every device of a user, oldest first.
	List(ctx context.Context, localpart string) ([]Device, error)
	// Cursor returns how far a device has been served from the to-device queue.
	Cursor(ctx context.Context, deviceID string) (int64, error)
	// SetCursor advances a device's to-device cursor.
	SetCursor(ctx context.Context, deviceID string, position int64) error
	// Update writes the mutable fields of a device owned by localpart. A nil
	// displayName leaves the stored name untouched.
	Update(ctx context.Context, localpart, deviceID, displayName string) error
	// Delete removes the listed devices of a user.
	Delete(ctx context.Context, localpart string, deviceIDs []string) error
	// DeleteByUser removes every device of a user (account deactivation).
	DeleteByUser(ctx context.Context, localpart string) error
	// Touch records that the device just used an access token.
	Touch(ctx context.Context, deviceID, ip, userAgent string, whenMS int64) error
	// Count returns how many devices a user has.
	Count(ctx context.Context, localpart string) (int64, error)
}

// DeviceRepoImpl is the GORM-backed [DeviceRepo].
type DeviceRepoImpl struct{ db *gorm.DB }

// Create implements [DeviceRepo].
func (r *DeviceRepoImpl) Create(ctx context.Context, device *Device) error {
	if device.CreatedMS == 0 {
		device.CreatedMS = nowMS()
	}
	if device.LastSeenMS == 0 {
		device.LastSeenMS = device.CreatedMS
	}
	err := session(ctx, r.db).Create(device).Error
	if err != nil {
		return fmt.Errorf("creating device %q: %w", device.ID, err)
	}
	return nil
}

// Get implements [DeviceRepo].
func (r *DeviceRepoImpl) Get(ctx context.Context, deviceID string) (*Device, error) {
	var device Device
	err := session(ctx, r.db).Where("id = ?", deviceID).First(&device).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("device %q: %w", deviceID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading device %q: %w", deviceID, err)
	}
	return &device, nil
}

// List implements [DeviceRepo].
func (r *DeviceRepoImpl) List(ctx context.Context, localpart string) ([]Device, error) {
	var devices []Device
	err := session(ctx, r.db).
		Where("localpart = ?", localpart).
		Order("created_ms ASC").
		Find(&devices).Error
	if err != nil {
		return nil, fmt.Errorf("listing devices of %q: %w", localpart, err)
	}
	return devices, nil
}

// Update implements [DeviceRepo].
func (r *DeviceRepoImpl) Update(ctx context.Context, localpart, deviceID, displayName string) error {
	updates := map[string]any{"display_name": displayName}
	res := session(ctx, r.db).Model(&Device{}).
		Where("id = ? AND localpart = ?", deviceID, localpart).
		Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("updating device %q: %w", deviceID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("device %q: %w", deviceID, ErrNotFound)
	}
	return nil
}

// Delete implements [DeviceRepo].
func (r *DeviceRepoImpl) Delete(ctx context.Context, localpart string, deviceIDs []string) error {
	if len(deviceIDs) == 0 {
		return nil
	}
	err := session(ctx, r.db).
		Where("localpart = ? AND id IN ?", localpart, deviceIDs).
		Delete(&Device{}).Error
	if err != nil {
		return fmt.Errorf("deleting devices: %w", err)
	}
	return nil
}

// DeleteByUser implements [DeviceRepo].
func (r *DeviceRepoImpl) DeleteByUser(ctx context.Context, localpart string) error {
	if err := session(ctx, r.db).Where("localpart = ?", localpart).Delete(&Device{}).Error; err != nil {
		return fmt.Errorf("deleting devices of %q: %w", localpart, err)
	}
	return nil
}

// Touch implements [DeviceRepo].
func (r *DeviceRepoImpl) Touch(ctx context.Context, deviceID, ip, userAgent string, whenMS int64) error {
	// A touch must never fail a request: an unknown device here means the token is
	// stale, and the authentication path has already decided the token is valid.
	err := session(ctx, r.db).Model(&Device{}).
		Where("id = ?", deviceID).
		Update("last_seen_ms", whenMS).Error
	if err != nil {
		return fmt.Errorf("touching device %q: %w", deviceID, err)
	}
	return nil
}

// Count implements [DeviceRepo].
func (r *DeviceRepoImpl) Count(ctx context.Context, localpart string) (int64, error) {
	var count int64
	err := session(ctx, r.db).Model(&Device{}).Where("localpart = ?", localpart).Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("counting devices of %q: %w", localpart, err)
	}
	return count, nil
}

// NewDeviceRepo returns the [DeviceRepo] implementation.
func NewDeviceRepo(db *gorm.DB) DeviceRepo { return &DeviceRepoImpl{db: db} }

// Cursor returns how far a device has been served from the to-device queue, or 0 for a device
// this server has never heard from.
func (r *DeviceRepoImpl) Cursor(ctx context.Context, deviceID string) (int64, error) {
	var device Device
	err := session(ctx, r.db).Where("id = ?", deviceID).First(&device).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading device %q: %w", deviceID, err)
	}
	return device.ToDevicePos, nil
}

// SetCursor advances a device's to-device cursor.
func (r *DeviceRepoImpl) SetCursor(ctx context.Context, deviceID string, position int64) error {
	err := session(ctx, r.db).Model(&Device{}).Where("id = ?", deviceID).
		Update("to_device_pos", position).Error
	if err != nil {
		return fmt.Errorf("advancing the to-device cursor of %q: %w", deviceID, err)
	}
	return nil
}
