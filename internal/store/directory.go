package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DirectoryEntry is one name a user goes by, and the room they go by it in.
//
// RoomID is empty for the profile name — the one a user sets on their account and every room
// would otherwise copy. Keeping the room-specific names alongside it, rather than overwriting one
// field, is what makes "Alice at work" and "Alice at home" both searchable by the people who can
// see each of them, and invisible to the people who can see neither.
type DirectoryEntry struct {
	UserID      string `gorm:"size:512;primaryKey:directory_pk" json:"user_id"`
	RoomID      string `gorm:"size:512;primaryKey:directory_pk" json:"room_id"`
	DisplayName string `gorm:"size:512;index:directory_name" json:"display_name"`
	AvatarURL   string `gorm:"size:512" json:"avatar_url"`
	// Localpart is stored, not derived, so that a search of the user-ID form does not have to
	// re-parse every row.
	Localpart string `gorm:"size:255;index" json:"localpart"`
}

// TableName pins the table name.
func (DirectoryEntry) TableName() string { return "user_directory" }

var directoryConflict = clause.OnConflict{
	Columns:   []clause.Column{{Name: "user_id"}, {Name: "room_id"}},
	UpdateAll: true,
}

// DirectoryRepo is the user directory repository.
type DirectoryRepo interface {
	// Put stores or replaces one name of one user.
	Put(ctx context.Context, entry *DirectoryEntry) error
	// Delete removes a room's view of a user, used when they leave or are banned.
	Delete(ctx context.Context, userID, roomID string) error
	// DeleteUser removes everything about a user.
	DeleteUser(ctx context.Context, userID string) error
	// Search returns the rows whose user ID or display name matches, best first.
	Search(ctx context.Context, term string, limit int) ([]DirectoryEntry, error)
	// EntriesOf returns every name a user goes by.
	EntriesOf(ctx context.Context, userID string) ([]DirectoryEntry, error)
}

// DirectoryRepoImpl is the GORM-backed [DirectoryRepo].
type DirectoryRepoImpl struct{ db *gorm.DB }

// NewDirectoryRepo builds the user-directory repository.
func NewDirectoryRepo(db *gorm.DB) *DirectoryRepoImpl { return &DirectoryRepoImpl{db: db} }

// Put implements [DirectoryRepo].
func (r *DirectoryRepoImpl) Put(ctx context.Context, entry *DirectoryEntry) error {
	entry.DisplayName = strings.TrimSpace(entry.DisplayName)
	if entry.Localpart == "" {
		if localpart, _, err := splitLocalUserID(entry.UserID); err == nil {
			entry.Localpart = localpart
		}
	}
	err := session(ctx, r.db).Clauses(directoryConflict).Save(entry).Error
	if err != nil {
		return fmt.Errorf("storing directory entry for %q: %w", entry.UserID, err)
	}
	return nil
}

// Delete implements [DirectoryRepo].
func (r *DirectoryRepoImpl) Delete(ctx context.Context, userID, roomID string) error {
	err := session(ctx, r.db).
		Where("user_id = ? AND room_id = ?", userID, roomID).
		Delete(&DirectoryEntry{}).Error
	if err != nil {
		return fmt.Errorf("deleting directory entry of %q in %q: %w", userID, roomID, err)
	}
	return nil
}

// DeleteUser implements [DirectoryRepo].
func (r *DirectoryRepoImpl) DeleteUser(ctx context.Context, userID string) error {
	err := session(ctx, r.db).Where("user_id = ?", userID).Delete(&DirectoryEntry{}).Error
	if err != nil {
		return fmt.Errorf("deleting directory entries of %q: %w", userID, err)
	}
	return nil
}

// Search implements [DirectoryRepo].
//
// The match is a case-insensitive substring of the user ID or the display name. The specification
// leaves the ranking of results open, and matching on whole-word prefixes as well would be a
// better answer for a large server; what matters here is that the match is the same one the
// ranking below uses, so that a result cannot be visible by one rule and invisible by another.
func (r *DirectoryRepoImpl) Search(ctx context.Context, term string, limit int) ([]DirectoryEntry, error) {
	if limit <= 0 {
		limit = 10
	}
	// SQLite's LIKE is already case-insensitive for ASCII, and its collation is the only one
	// this server's indexes use, so the match the search applies is the match the ranking
	// below applies too. `ESCAPE` keeps a term containing a percent sign a literal.
	like := "%" + escapeLike(strings.TrimSpace(term)) + "%"
	var rows []DirectoryEntry
	err := session(ctx, r.db).
		Where(`display_name LIKE ? ESCAPE '\' OR user_id LIKE ? ESCAPE '\'`, like, like).
		Order("display_name ASC").
		Limit(limit * 4). // more rows than results: several rooms can hold the same user
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("searching the user directory: %w", err)
	}
	return rows, nil
}

// EntriesOf implements [DirectoryRepo].
func (r *DirectoryRepoImpl) EntriesOf(ctx context.Context, userID string) ([]DirectoryEntry, error) {
	var rows []DirectoryEntry
	err := session(ctx, r.db).Where("user_id = ?", userID).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading directory entries of %q: %w", userID, err)
	}
	return rows, nil
}

// escapeLike neutralises the wildcards of a LIKE pattern in a user-supplied term, so that a
// search for "100%" does not match everything.
func escapeLike(term string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(term)
}

// splitLocalUserID splits "@name:host" without importing the matrix package, which knows about
// server names this package has no business knowing.
func splitLocalUserID(userID string) (string, string, error) {
	rest := strings.TrimPrefix(userID, "@")
	localpart, domain, found := strings.Cut(rest, ":")
	if !found || localpart == "" {
		return "", "", errors.New("not a user ID")
	}
	return localpart, domain, nil
}
