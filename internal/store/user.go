package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// User is an account on this homeserver.
//
// The local part is the primary key because it is what the protocol keys on: access tokens,
// device lists, memberships and account data all hang off it. The fully qualified user ID
// is stored too, since it is what goes on the wire and what federation carries; keeping it
// denormalised avoids assembling strings in queries.
type User struct {
	// Localpart is the user ID local part, without "@" or the server name.
	Localpart string `gorm:"primaryKey;size:255" json:"localpart"`
	// UserID is the fully qualified user ID, e.g. "@alice:example.org".
	UserID string `gorm:"uniqueIndex;size:512" json:"user_id"`
	// PasswordHash is a bcrypt hash, empty when the account has no password (e.g. one
	// created by an application service).
	PasswordHash string `gorm:"size:255" json:"-"`
	// DisplayName and AvatarURL are the profile as the user set it.
	DisplayName string `gorm:"size:256" json:"display_name,omitempty"`
	AvatarURL   string `gorm:"size:512" json:"avatar_url,omitempty"`
	// Admin grants administrator rights (used by shared-secret registration).
	Admin bool `json:"admin"`
	// Deactivated accounts cannot log in and answer M_USER_DISABLED.
	Deactivated bool `json:"deactivated"`
	// Locked accounts cannot be modified; application-service managed accounts are
	// locked so that a password change through the client API is refused.
	Locked bool `json:"locked"`
	// CreatedMS and UpdatedMS are milliseconds since the epoch.
	CreatedMS int64 `json:"created_ms"`
	// UserVersion is bumped whenever anything a client must re-sync for changes
	// (profile, devices, push rules): it is what a /sync with no "since" can compare.
	UserVersion int64 `gorm:"index" json:"user_version"`
	// MasterPublishKey/ID and the rest of the cross-signing state arrive with the
	// key manager; the columns are left out until then on purpose.
}

// TableName pins the table name so a refactor of the Go type name cannot rewrite the
// operator's database.
func (User) TableName() string { return "users" }

// UserRepo is the account repository.
type UserRepo interface {
	// Create inserts a user, returning an error that matches errors.ErrAlreadyExists
	// semantics through IsAlreadyExists when the local part is taken.
	Create(ctx context.Context, user *User) error
	// Get returns the user with this local part, or [ErrNotFound].
	Get(ctx context.Context, localpart string) (*User, error)
	// List returns up to limit users, ordered by local part, starting after after.
	List(ctx context.Context, limit int, after string) ([]User, error)
	// Update writes the mutable profile fields of an existing user.
	Update(ctx context.Context, localpart string, updates map[string]any) error
	// Count returns how many accounts exist (used by /register's "first user is admin"
	// style decisions and by tests).
	Count(ctx context.Context) (int64, error)
}

// UserRepoImpl is the GORM-backed [UserRepo].
type UserRepoImpl struct{ db *gorm.DB }

// Create implements [UserRepo].
func (r *UserRepoImpl) Create(ctx context.Context, user *User) error {
	if user.CreatedMS == 0 {
		user.CreatedMS = nowMS()
	}
	// TranslateError (set on the gorm.Config) turns the unique-constraint violation into
	// gorm.ErrDuplicatedKey, which is the signal callers branch on.
	err := session(ctx, r.db).Create(user).Error
	switch {
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return fmt.Errorf("user %q: %w", user.UserID, ErrAlreadyExists)
	case err != nil:
		return fmt.Errorf("creating user %q: %w", user.UserID, err)
	}
	return nil
}

// Get implements [UserRepo].
func (r *UserRepoImpl) Get(ctx context.Context, localpart string) (*User, error) {
	var user User
	err := session(ctx, r.db).Where("localpart = ?", localpart).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("user %q: %w", localpart, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading user %q: %w", localpart, err)
	}
	return &user, nil
}

// List implements [UserRepo].
func (r *UserRepoImpl) List(ctx context.Context, limit int, after string) ([]User, error) {
	if limit <= 0 {
		limit = 100
	}
	q := session(ctx, r.db).Order("localpart ASC").Limit(limit)
	if after != "" {
		q = q.Where("localpart > ?", after)
	}
	var users []User
	if err := q.Find(&users).Error; err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	return users, nil
}

// Update implements [UserRepo].
func (r *UserRepoImpl) Update(ctx context.Context, localpart string, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	res := session(ctx, r.db).Model(&User{}).Where("localpart = ?", localpart).Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("updating user %q: %w", localpart, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("user %q: %w", localpart, ErrNotFound)
	}
	return nil
}

// Count implements [UserRepo].
func (r *UserRepoImpl) Count(ctx context.Context) (int64, error) {
	var count int64
	if err := session(ctx, r.db).Model(&User{}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("counting users: %w", err)
	}
	return count, nil
}

// NewUserRepo returns the [UserRepo] implementation.
func NewUserRepo(db *gorm.DB) UserRepo { return &UserRepoImpl{db: db} }

// ErrAlreadyExists is returned by Create when the row's identity is taken. Repositories
// return it rather than a driver-specific error so services can branch on it.
var ErrAlreadyExists = errors.New("store: already exists")

// IsAlreadyExists reports whether err is or wraps [ErrAlreadyExists].
func IsAlreadyExists(err error) bool { return errors.Is(err, ErrAlreadyExists) }
