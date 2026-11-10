package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// AccessToken is one issued access token. Its presence *is* validity: logging out deletes
// the row, so a token cannot outlive the session that minted it. That is why access tokens
// here are opaque random strings in a table rather than self-verifying tokens — a
// self-verifying token cannot be revoked, and /logout has to revoke.
type AccessToken struct {
	// Token is the opaque bearer credential.
	Token string `gorm:"primaryKey;size:128" json:"token"`
	// DeviceID is the device the token belongs to. Deleting the device must delete the
	// token, which is enforced here as well as by the device repository.
	DeviceID string `gorm:"index;size:64" json:"device_id"`
	// Localpart duplicates the device's owner so that authenticating a request needs one
	// query rather than a join.
	Localpart string `gorm:"index;size:255" json:"localpart"`
	// CreatedMS is when the token was issued, in milliseconds since the epoch.
	CreatedMS int64 `json:"created_ms"`
	// ExpiryMS is when the token stops working; 0 means it does not expire.
	ExpiryMS int64 `json:"expiry_ms"`
}

// TableName pins the table name.
func (AccessToken) TableName() string { return "access_tokens" }

// TokenRepo is the access-token repository.
type TokenRepo interface {
	// Create inserts a token.
	Create(ctx context.Context, token *AccessToken) error
	// Get returns the token row, or [ErrNotFound]. It does not second-guess validity
	// beyond existence and expiry; the accounts service decides.
	Get(ctx context.Context, token string) (*AccessToken, error)
	// Delete removes one token (POST /logout).
	Delete(ctx context.Context, token string) error
	// DeleteByDevice removes every token of one device.
	DeleteByDevice(ctx context.Context, deviceID string) error
	// DeleteByUser removes every token of one user (POST /logout/all, password change,
	// deactivation).
	DeleteByUser(ctx context.Context, localpart string) error
	// DeleteByUserExcept removes every token of a user except the given ones. Used by a
	// password change, which must not log out the device that just proved the password.
	DeleteByUserExcept(ctx context.Context, localpart string, exceptDeviceIDs []string) error
	// ListByUser returns the tokens of a user with their devices, for the "where am I
	// logged in" listing.
	ListByUser(ctx context.Context, localpart string) ([]AccessToken, error)
}

// TokenRepoImpl is the GORM-backed [TokenRepo].
type TokenRepoImpl struct{ db *gorm.DB }

// Create implements [TokenRepo].
func (r *TokenRepoImpl) Create(ctx context.Context, token *AccessToken) error {
	if token.CreatedMS == 0 {
		token.CreatedMS = nowMS()
	}
	if err := session(ctx, r.db).Create(token).Error; err != nil {
		return fmt.Errorf("storing access token: %w", err)
	}
	return nil
}

// Get implements [TokenRepo].
func (r *TokenRepoImpl) Get(ctx context.Context, token string) (*AccessToken, error) {
	var row AccessToken
	err := session(ctx, r.db).Where("token = ?", token).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("access token: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading access token: %w", err)
	}
	return &row, nil
}

// Delete implements [TokenRepo].
func (r *TokenRepoImpl) Delete(ctx context.Context, token string) error {
	if err := session(ctx, r.db).Where("token = ?", token).Delete(&AccessToken{}).Error; err != nil {
		return fmt.Errorf("deleting access token: %w", err)
	}
	return nil
}

// DeleteByDevice implements [TokenRepo].
func (r *TokenRepoImpl) DeleteByDevice(ctx context.Context, deviceID string) error {
	if err := session(ctx, r.db).Where("device_id = ?", deviceID).Delete(&AccessToken{}).Error; err != nil {
		return fmt.Errorf("deleting tokens of device %q: %w", deviceID, err)
	}
	return nil
}

// DeleteByUser implements [TokenRepo].
func (r *TokenRepoImpl) DeleteByUser(ctx context.Context, localpart string) error {
	if err := session(ctx, r.db).Where("localpart = ?", localpart).Delete(&AccessToken{}).Error; err != nil {
		return fmt.Errorf("deleting tokens of %q: %w", localpart, err)
	}
	return nil
}

// DeleteByUserExcept implements [TokenRepo].
func (r *TokenRepoImpl) DeleteByUserExcept(ctx context.Context, localpart string, exceptDeviceIDs []string) error {
	q := session(ctx, r.db).Where("localpart = ?", localpart)
	if len(exceptDeviceIDs) > 0 {
		q = q.Where("device_id NOT IN ?", exceptDeviceIDs)
	}
	if err := q.Delete(&AccessToken{}).Error; err != nil {
		return fmt.Errorf("revoking tokens of %q: %w", localpart, err)
	}
	return nil
}

// ListByUser implements [TokenRepo].
func (r *TokenRepoImpl) ListByUser(ctx context.Context, localpart string) ([]AccessToken, error) {
	var rows []AccessToken
	err := session(ctx, r.db).Where("localpart = ?", localpart).Order("created_ms ASC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("listing tokens of %q: %w", localpart, err)
	}
	return rows, nil
}

// NewTokenRepo returns the [TokenRepo] implementation.
func NewTokenRepo(db *gorm.DB) TokenRepo { return &TokenRepoImpl{db: db} }
