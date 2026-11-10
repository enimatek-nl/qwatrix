package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Filter is a saved event filter.
//
// Filters are saved rather than re-sent because a client applies the same one on every sync: an
// inline object on a request it makes hundreds of times a day would be paid for each time, and a
// filter is also what makes a client's `since` token meaningful — changing the filter changes what
// the stream means, so the server has to be able to tell the two apart.
type Filter struct {
	// Localpart and ID are the key. The ID is only unique within an account, and a lookup
	// always carries the account, so one user cannot reach another's filter by guessing.
	Localpart string `gorm:"primaryKey;size:256"`
	ID        string `gorm:"primaryKey;size:64"`

	// Definition is the filter as the client wrote it, kept verbatim. A client that downloads
	// its own filter expects to read back what it stored, including keys this server does not
	// understand yet — dropping them would make a round-trip lossy.
	Definition string `gorm:"type:text"`
	CreatedMS  int64
}

// TableName pins the table name.
func (Filter) TableName() string { return "filters" }

// FilterRepo stores saved filters.
type FilterRepo interface {
	// Create stores a filter and returns it with its assigned ID.
	Create(ctx context.Context, localpart, definition string) (*Filter, error)
	Get(ctx context.Context, localpart, id string) (*Filter, error)
	List(ctx context.Context, localpart string) ([]Filter, error)
	Delete(ctx context.Context, localpart, id string) error
}

// FilterRepoImpl is the [FilterRepo] on the shared handle.
type FilterRepoImpl struct{ db *gorm.DB }

// NewFilterRepo builds a repository on an open handle.
func NewFilterRepo(db *gorm.DB) *FilterRepoImpl { return &FilterRepoImpl{db: db} }

// Create implements [FilterRepo].
func (r *FilterRepoImpl) Create(ctx context.Context, localpart, definition string) (*Filter, error) {
	// IDs are allocated per account, continuing from the highest existing numeric one. A UUID
	// would also work, but a short numeric ID keeps sync URLs readable when a client inlines
	// nothing and an operator has to read a log line.
	//
	// COALESCE and an int64 target are both load-bearing: MAX over no rows is NULL, and scanning
	// a NULL into a Go string fails the whole query rather than yielding an empty account.
	next, err := r.highestID(ctx, localpart)
	if err != nil {
		return nil, err
	}
	next++
	filter := &Filter{
		Localpart:  localpart,
		ID:         fmt.Sprintf("%d", next),
		Definition: definition,
		CreatedMS:  time.Now().UnixMilli(),
	}
	// Retrying on a collision keeps this correct if two devices of one account save their
	// first filter at the same instant; both get a filter, neither overwrites the other.
	for attempt := 0; attempt < 5; attempt++ {
		err := session(ctx, r.db).Create(filter).Error
		if err == nil {
			return filter, nil
		}
		if !isUniqueViolation(err) {
			return nil, fmt.Errorf("storing a filter: %w", err)
		}
		maxID, err := r.highestID(ctx, localpart)
		if err != nil {
			return nil, err
		}
		filter.ID = fmt.Sprintf("%d", maxID+1)
	}
	return nil, errors.New("could not allocate a filter ID")
}

// isUniqueViolation reports whether an error is SQLite's duplicate-key failure. GORM surfaces the
// driver message rather than a typed error, so the text is what is available.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "UNIQUE constraint failed") ||
		strings.Contains(message, "duplicated key not allowed")
}

// highestID returns the largest numeric filter ID an account holds, or 0 when it holds none.
func (r *FilterRepoImpl) highestID(ctx context.Context, localpart string) (int64, error) {
	var highest int64
	err := session(ctx, r.db).Model(&Filter{}).
		Where("localpart = ? AND id GLOB '[0-9]*'", localpart).
		Select("COALESCE(MAX(CAST(id AS INTEGER)), 0)").Scan(&highest).Error
	if err != nil {
		return 0, fmt.Errorf("reading the highest filter ID of %q: %w", localpart, err)
	}
	return highest, nil
}

// Get implements [FilterRepo].
func (r *FilterRepoImpl) Get(ctx context.Context, localpart, id string) (*Filter, error) {
	var filter Filter
	err := session(ctx, r.db).
		Where("localpart = ? AND id = ?", localpart, id).First(&filter).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("filter %q of %q: %w", id, localpart, ErrNotFound)
		}
		return nil, fmt.Errorf("reading filter %q: %w", id, err)
	}
	return &filter, nil
}

// List implements [FilterRepo].
func (r *FilterRepoImpl) List(ctx context.Context, localpart string) ([]Filter, error) {
	var filters []Filter
	err := session(ctx, r.db).Where("localpart = ?", localpart).
		Order("id asc").Find(&filters).Error
	if err != nil {
		return nil, fmt.Errorf("listing filters: %w", err)
	}
	return filters, nil
}

// Delete implements [FilterRepo].
func (r *FilterRepoImpl) Delete(ctx context.Context, localpart, id string) error {
	err := session(ctx, r.db).
		Where("localpart = ? AND id = ?", localpart, id).Delete(&Filter{}).Error
	if err != nil {
		return fmt.Errorf("deleting filter %q: %w", id, err)
	}
	return nil
}
