package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// Presence values as they appear on the wire.
const (
	PresenceOnline      = "online"
	PresenceUnavailable = "unavailable"
	PresenceOffline     = "offline"
)

// Presence is the current presence of a local user.
//
// Presence is deliberately a single row per user rather than a log of state changes: the
// protocol only ever asks "what is it now" and "what changed since this position", and a
// client that missed a transition wants the current value, not the one it missed.
type Presence struct {
	// Localpart is the primary key because presence is only ever stored for this server's
	// own users — a remote user's presence belongs to their own server.
	Localpart string `gorm:"primaryKey;size:255" json:"localpart"`
	// UserID is the fully qualified ID, denormalised the way users and memberships do it.
	UserID string `gorm:"size:512;index" json:"user_id"`
	// Presence is one of the constants above.
	Presence string `gorm:"size:16" json:"presence"`
	// StatusMessage is the free-text status a user sets alongside it.
	StatusMessage string `gorm:"size:255" json:"status_msg,omitempty"`
	// LastActiveMS is the last moment the user was seen, in milliseconds since the epoch.
	// It backs last_active_ago, which is a duration the client computes for itself.
	LastActiveMS int64 `gorm:"index" json:"last_active_ms"`
	// StreamPos places the last change on the sync stream so that "changed since token X"
	// is one indexed comparison.
	StreamPos int64 `gorm:"index" json:"stream_pos"`
	// PreviouslyActiveMS is when the user was last online before the current state, which
	// is what current_state=false in a presence event is about.
	PreviouslyActiveMS int64 `json:"previously_active_ms"`
}

// TableName pins the table name.
func (Presence) TableName() string { return "presence" }

// PresenceRepo is the presence repository.
type PresenceRepo interface {
	// Get returns a user's presence, or [ErrNotFound] when they have never set it.
	Get(ctx context.Context, localpart string) (*Presence, error)
	// Set writes the current presence and gives the change a fresh stream position, which is
	// what makes it visible to a client syncing from an older token. An empty presence
	// leaves the stored state alone, which is what a bare status_msg update means.
	Set(ctx context.Context, presence *Presence) error
	// UpdatedSince returns the presence of the named users whose last change is at or after
	// the given stream position. An empty localparts list returns nothing: it means the
	// caller shares a room with nobody, not "everybody".
	UpdatedSince(ctx context.Context, localparts []string, since int64) ([]Presence, error)
}

type presenceRepo struct{ db *gorm.DB }

// NewPresenceRepo builds the presence repository.
func NewPresenceRepo(db *gorm.DB) PresenceRepo { return &presenceRepo{db: db} }

func (r *presenceRepo) session(ctx context.Context) *gorm.DB { return session(ctx, r.db) }

func (r *presenceRepo) Get(ctx context.Context, localpart string) (*Presence, error) {
	var row Presence
	err := r.session(ctx).Where("localpart = ?", localpart).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("presence %q: %w", localpart, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *presenceRepo) Set(ctx context.Context, row *Presence) error {
	db := r.session(ctx)

	// Every write takes a new stream position, including a write that left the value alone.
	// The position comes from the event stream — one greater than the highest position
	// issued — so a client's ordinary `since` token answers "what presence have I not been
	// told about" without a second token to keep in sync.
	//
	// The trade-off is deliberate: a presence change can land on the same position as a
	// concurrent event, in which case a client sees it twice. A duplicated presence is
	// harmless, because presence is the current value rather than a fact about the past; a
	// missed one would leave a client showing a stale dot forever.
	position, err := maxStreamPosition(ctx, db)
	if err != nil {
		return err
	}
	row.StreamPos = position + 1
	return db.Save(row).Error
}

func (r *presenceRepo) UpdatedSince(ctx context.Context, localparts []string, since int64) ([]Presence, error) {
	if len(localparts) == 0 {
		return nil, nil
	}
	var rows []Presence
	err := r.session(ctx).
		Where("localpart IN ? AND stream_pos > ?", localparts, since).
		Order("stream_pos ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// maxStreamPosition is the highest event stream position in the database, and 0 when there
// are no events yet.
func maxStreamPosition(ctx context.Context, db *gorm.DB) (int64, error) {
	var position int64
	err := session(ctx, db).Model(&EventRow{}).Select("COALESCE(MAX(id), 0)").Scan(&position).Error
	if err != nil {
		return 0, err
	}
	return position, nil
}
