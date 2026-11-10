package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// deviceKeyConflict is an explicit upsert: [gorm.DB.Save] decides insert-versus-update by
	// testing the primary key for zero values, and a composite key of strings has no zero that
	// a real row cannot also be, so the answer has to be stated rather than inferred.
	deviceKeyConflict = clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "device_id"}},
		UpdateAll: true,
	}
)

// DeviceKey is a device's published identity: its curve25519 and ed25519 public keys, the
// algorithms it speaks, and its self-signature.
//
// The document is stored as the client wrote it. A device key is *signed by its own device*, so
// any normalisation here — reordering keys, dropping an unknown field, re-encoding numbers —
// breaks the signature over the very bytes this server hands to the next client. The key server's
// job is to remember and relay it, not to interpret it.
type DeviceKey struct {
	UserID   string `gorm:"size:512;primaryKey:key_pk" json:"user_id"`
	DeviceID string `gorm:"size:64;primaryKey:key_pk" json:"device_id"`
	// KeyJSON is the device_keys object as uploaded.
	KeyJSON string `gorm:"type:text" json:"-"`
	// VerifyKey and SigningKey are the curve25519 and ed25519 key IDs, indexed for the
	// cross-signing and device-list lookups that find a device by what it signed with.
	VerifyKey  string `gorm:"size:255;index" json:"-"`
	SigningKey string `gorm:"size:255;index" json:"-"`
	UploadedMS int64  `json:"uploaded_ms"`
	// DeletedFlag marks a published key withdrawn by a logout. It is a flag rather than a row
	// delete so that a client which already queried the key can be told, honestly, that the
	// device no longer has keys — but it is not indexed: nothing queries by it alone, and an
	// index on a mostly-false boolean is a tax paid for nothing.
	DeletedFlag bool `gorm:"column:deleted;index" json:"deleted"`
}

// TableName pins the table name.
func (DeviceKey) TableName() string { return "device_keys" }

// OneTimeKey is a prepaid message key a device uploaded so others can start a conversation with
// it. Each one is spent exactly once, which is the whole reason it is a separate row instead of a
// map inside the device key.
type OneTimeKey struct {
	// ID orders uploads, and a claim must hand keys out oldest-first: a client that uploaded
	// four keys and had one claimed should not care which, but a client that retried a claim
	// and lost the response must not be handed a key it has already thrown away. Complement
	// pins the order by uploading two keys a second apart.
	ID         int64  `gorm:"primaryKey;autoIncrement" json:"-"`
	UserID     string `gorm:"size:512;index:key_user_device" json:"user_id"`
	DeviceID   string `gorm:"size:64;index:key_user_device" json:"device_id"`
	KeyID      string `gorm:"size:256" json:"key_id"`
	Algorithm  string `gorm:"size:256;index" json:"algorithm"`
	KeyJSON    string `gorm:"type:text" json:"-"`
	UploadedMS int64  `json:"uploaded_ms"`
	// Fallback marks an MSC3391 fallback key: reusable, so it is not deleted on claim, and used
	// only when the device has run out of one-time keys.
	Fallback bool `json:"fallback"`
}

// TableName pins the table name.
func (OneTimeKey) TableName() string { return "one_time_keys" }

// KeyRepo is the device- and one-time-key repository.
type KeyRepo interface {
	// PutDevice stores or replaces a device's published keys.
	PutDevice(ctx context.Context, key *DeviceKey) error
	// Device returns one device's published keys, or [ErrNotFound].
	Device(ctx context.Context, userID, deviceID string) (*DeviceKey, error)
	// Devices returns the published keys of a user's devices, or of every device when the
	// list is empty.
	Devices(ctx context.Context, userID string, deviceIDs []string) ([]DeviceKey, error)
	// AddOneTimeKeys stores uploaded keys, skipping the ones this device already has. It
	// returns how many were new.
	AddOneTimeKeys(ctx context.Context, keys []OneTimeKey) (int, error)
	// OneTimeKeyCounts returns the stored-key count per algorithm, and the count of keys
	// that may still be claimed.
	OneTimeKeyCounts(ctx context.Context, userID, deviceID string) (map[string]int64, error)
	// ClaimOneTimeKeys spends and returns up to limit keys of one algorithm, oldest first.
	ClaimOneTimeKeys(ctx context.Context, userID, deviceID, algorithm string, limit int) ([]OneTimeKey, error)
	// DeleteDevices removes the published keys of sessions that have been logged out.
	DeleteDevices(ctx context.Context, userID string, deviceIDs []string) error
}

// KeyRepoImpl is the GORM-backed [KeyRepo].
type KeyRepoImpl struct{ db *gorm.DB }

// NewKeyRepo builds the device- and one-time-key repository.
func NewKeyRepo(db *gorm.DB) *KeyRepoImpl { return &KeyRepoImpl{db: db} }

// PutDevice implements [KeyRepo].
func (r *KeyRepoImpl) PutDevice(ctx context.Context, key *DeviceKey) error {
	err := session(ctx, r.db).Clauses(deviceKeyConflict).Save(key).Error
	if err != nil {
		return fmt.Errorf("storing device keys of %s/%s: %w", key.UserID, key.DeviceID, err)
	}
	return nil
}

// Device implements [KeyRepo].
func (r *KeyRepoImpl) Device(ctx context.Context, userID, deviceID string) (*DeviceKey, error) {
	var row DeviceKey
	err := session(ctx, r.db).
		Where("user_id = ? AND device_id = ?", userID, deviceID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("device keys of %s/%s: %w", userID, deviceID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading device keys of %s/%s: %w", userID, deviceID, err)
	}
	if row.DeletedFlag {
		// A deleted key is not a key. Telling a client "this device has no keys" is true;
		// handing it a key whose owner logged out is not.
		return nil, fmt.Errorf("device keys of %s/%s: %w", userID, deviceID, ErrNotFound)
	}
	return &row, nil
}

// Devices implements [KeyRepo].
func (r *KeyRepoImpl) Devices(ctx context.Context, userID string, deviceIDs []string) ([]DeviceKey, error) {
	query := session(ctx, r.db).Where("user_id = ? AND deleted = ?", userID, false)
	if len(deviceIDs) > 0 {
		query = query.Where("device_id IN ?", deviceIDs)
	}
	var rows []DeviceKey
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading device keys of %q: %w", userID, err)
	}
	return rows, nil
}

// AddOneTimeKeys implements [KeyRepo].
//
// Re-uploading the same key is a no-op rather than a second copy. A client that never received
// the response re-uploads its whole batch, and storing duplicates would let one key be claimed
// twice by two different senders — and a one-time key given to two parties is no longer one-time.
func (r *KeyRepoImpl) AddOneTimeKeys(ctx context.Context, keys []OneTimeKey) (int, error) {
	added := 0
	for _, key := range keys {
		var existing OneTimeKey
		err := session(ctx, r.db).
			Where("user_id = ? AND device_id = ? AND key_id = ? AND fallback = ?",
				key.UserID, key.DeviceID, key.KeyID, key.Fallback).
			First(&existing).Error
		switch {
		case err == nil:
			continue
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return added, fmt.Errorf("checking one-time key %q: %w", key.KeyID, err)
		}
		if err := session(ctx, r.db).Create(&key).Error; err != nil {
			return added, fmt.Errorf("storing one-time key %q: %w", key.KeyID, err)
		}
		added++
	}
	return added, nil
}

// OneTimeKeyCounts implements [KeyRepo].
func (r *KeyRepoImpl) OneTimeKeyCounts(ctx context.Context, userID, deviceID string) (map[string]int64, error) {
	type row struct {
		Algorithm string
		Count     int64
	}
	var rows []row
	err := session(ctx, r.db).Model(&OneTimeKey{}).
		Select("algorithm, COUNT(*) AS count").
		Where("user_id = ? AND device_id = ? AND fallback = ?", userID, deviceID, false).
		Group("algorithm").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("counting one-time keys of %s/%s: %w", userID, deviceID, err)
	}
	counts := map[string]int64{}
	for _, entry := range rows {
		if entry.Count > 0 {
			counts[entry.Algorithm] = entry.Count
		}
	}
	return counts, nil
}

// ClaimOneTimeKeys implements [KeyRepo].
func (r *KeyRepoImpl) ClaimOneTimeKeys(ctx context.Context, userID, deviceID, algorithm string, limit int) ([]OneTimeKey, error) {
	if limit <= 0 {
		limit = 1
	}
	// The claim has to be atomic against a concurrent claimant: two senders that both take the
	// oldest key would each be told it is theirs, and the Double Ratchet only works if it was
	// handed to one of them. The write lock serialises claims per device.
	var claimed []OneTimeKey
	err := r.db.Transaction(func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND device_id = ? AND fallback = ?", userID, deviceID, false)
		if algorithm != "" && algorithm != "*" {
			query = query.Where("algorithm = ?", algorithm)
		}
		var rows []OneTimeKey
		if err := query.Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]int64, 0, len(rows))
		for _, entry := range rows {
			ids = append(ids, entry.ID)
		}
		if err := tx.Where("id IN ?", ids).Delete(&OneTimeKey{}).Error; err != nil {
			return fmt.Errorf("spending one-time keys of %s/%s: %w", userID, deviceID, err)
		}
		claimed = rows
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claiming one-time keys of %s/%s: %w", userID, deviceID, err)
	}
	return claimed, nil
}

// DeleteDevices implements [KeyRepo].
func (r *KeyRepoImpl) DeleteDevices(ctx context.Context, userID string, deviceIDs []string) error {
	if len(deviceIDs) == 0 {
		return nil
	}
	err := session(ctx, r.db).
		Where("user_id = ? AND device_id IN ?", userID, deviceIDs).
		Delete(&DeviceKey{}).Error
	if err != nil {
		return fmt.Errorf("deleting device keys of %q: %w", userID, err)
	}
	return session(ctx, r.db).
		Where("user_id = ? AND device_id IN ?", userID, deviceIDs).
		Delete(&OneTimeKey{}).Error
}
