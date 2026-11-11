package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// Membership values as they appear in m.room.member content.
const (
	MembershipJoin   = "join"
	MembershipInvite = "invite"
	MembershipLeave  = "leave"
	MembershipBan    = "ban"
	MembershipKnock  = "knock"
)

// Membership is the current membership of a user in a room, denormalised from the
// m.room.member state event.
//
// The state table already holds the same fact; this table exists because authorisation and
// /sync ask "which rooms is this user in, and with what membership" millions of times, and
// answering that with a state scan is the difference between a millisecond and a second.
// The room server writes both in the same transaction, so they cannot drift.
type Membership struct {
	RoomID    string `gorm:"size:512;primaryKey:membership_pk" json:"room_id"`
	Localpart string `gorm:"size:255;primaryKey:membership_pk;index:idx_membership_user" json:"localpart"`
	// UserID is the full user ID, kept for the same reason users store it.
	UserID string `gorm:"size:512;index" json:"user_id"`
	// Membership is one of the constants above.
	Membership string `gorm:"size:16;index:idx_membership_user" json:"membership"`
	// EventID is the m.room.member event this reflects.
	EventID string `gorm:"size:256" json:"event_id"`
	// StreamPos is that event's stream position, used for membership-scoped sync.
	StreamPos int64 `gorm:"index" json:"stream_pos"`
	// Sender is who performed the membership change (m.room.member's sender).
	Sender string `gorm:"size:512" json:"sender,omitempty"`
	// Forgotten implements POST /rooms/{id}/forget: a forgotten room is dropped from the
	// user's /sync without changing their membership in the room.
	Forgotten bool `gorm:"index" json:"forgotten"`
	// AvatarURL and DisplayName cache the member's profile at the time of the event so
	// that /members and /hierarchy do not need a profile lookup per row.
	AvatarURL   string `gorm:"size:512" json:"avatar_url,omitempty"`
	DisplayName string `gorm:"size:256" json:"display_name,omitempty"`
}

// TableName pins the table name.
func (Membership) TableName() string { return "memberships" }

// MembershipRepo is the membership repository.
type MembershipRepo interface {
	// Upsert writes the current membership for a user in a room.
	Upsert(ctx context.Context, tx *gorm.DB, membership *Membership) error
	// Get returns one membership, or [ErrNotFound] when the user has never been in the
	// room (which the spec treats as "leave").
	Get(ctx context.Context, roomID, localpart string) (*Membership, error)
	// List returns the members of a room. An empty memberships list means "any".
	List(ctx context.Context, roomID string, memberships []string, notMemberships []string) ([]Membership, error)
	// ListInTX reads the same list through an open transaction.
	ListInTX(ctx context.Context, tx *gorm.DB, roomID string, memberships []string, notMemberships []string) ([]Membership, error)
	// ListByUser returns every membership row of a user (rooms they have not forgotten),
	// which is what /sync needs: the room list together with each room's membership value
	// and stream position, in one query.
	ListByUser(ctx context.Context, localpart string, notMemberships ...string) ([]Membership, error)
	// ListForgottenByUser returns the forgotten membership rows of a user.
	//
	// /sync needs these for one case only, and it is not a nicety: a client whose token
	// predates the user's leave still has to be told the room is gone. Answering that sync from
	// not-forgotten rows alone silently deletes the room from a device that has no other way
	// to find out, so it keeps a room it cannot open, leave, or forget.
	ListForgottenByUser(ctx context.Context, localpart string) ([]Membership, error)
	// RoomsForUser returns the room IDs where the user holds one of the given
	// memberships and has not forgotten the room.
	RoomsForUser(ctx context.Context, localpart string, memberships ...string) ([]string, error)
	// CountByMembership returns how many members a room has per membership value.
	CountByMembership(ctx context.Context, roomID string) (map[string]int, error)
	// MarkForgotten sets or clears the forgotten flag.
	MarkForgotten(ctx context.Context, roomID, localpart string, forgotten bool) error
	// RefreshProfile rewrites the cached display name and avatar URL on every non-forgotten
	// membership row of a local user, returning how many rows changed.
	//
	// The cache is the point: /members and /joined_members answer from these columns rather
	// than doing a profile lookup per row, so a profile change that only touched the users
	// table leaves every room showing the old face until the member writes a new event —
	// which a user who simply changed their avatar has no reason to do.
	RefreshProfile(ctx context.Context, localpart, displayName, avatarURL string) (int64, error)
	// DeleteForgetUser removes the row entirely (a forgotten user rejoining later starts
	// from a clean slate; /forget also allows the server to drop the association).
	DeleteForgetUser(ctx context.Context, roomID, localpart string) error
}

// MembershipRepoImpl is the GORM-backed [MembershipRepo].
type MembershipRepoImpl struct{ db *gorm.DB }

// Upsert implements [MembershipRepo].
func (r *MembershipRepoImpl) Upsert(ctx context.Context, tx *gorm.DB, membership *Membership) error {
	db := session(ctx, r.db)
	if tx != nil {
		db = tx.WithContext(ctx)
	}
	if membership.StreamPos == 0 {
		membership.StreamPos = nowMS()
	}
	err := db.WithContext(ctx).Save(membership).Error
	if err != nil {
		return fmt.Errorf("writing membership %s in %q: %w", membership.Localpart, membership.RoomID, err)
	}
	return nil
}

// Get implements [MembershipRepo].
func (r *MembershipRepoImpl) Get(ctx context.Context, roomID, localpart string) (*Membership, error) {
	var membership Membership
	err := session(ctx, r.db).
		Where("room_id = ? AND localpart = ?", roomID, localpart).
		First(&membership).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("membership of %q in %q: %w", localpart, roomID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading membership in %q: %w", roomID, err)
	}
	return &membership, nil
}

// List implements [MembershipRepo].
func (r *MembershipRepoImpl) List(ctx context.Context, roomID string, memberships, notMemberships []string) ([]Membership, error) {
	return r.list(ctx, r.db, roomID, memberships, notMemberships)
}

// ListInTX implements [MembershipRepo].
//
// Reading the members from inside a write transaction is what lets a decision about *whom* to tell be
// made in the same transaction as the event that changed the room. The same connection has to be used:
// a second one would either block on the open write or read the room as it was before the event, and
// the two differ exactly when it matters.
func (r *MembershipRepoImpl) ListInTX(ctx context.Context, tx *gorm.DB, roomID string, memberships, notMemberships []string) ([]Membership, error) {
	return r.list(ctx, tx, roomID, memberships, notMemberships)
}

func (r *MembershipRepoImpl) list(ctx context.Context, db *gorm.DB, roomID string, memberships, notMemberships []string) ([]Membership, error) {
	q := session(ctx, db).Where("room_id = ?", roomID)
	if len(memberships) > 0 {
		q = q.Where("membership IN ?", memberships)
	}
	if len(notMemberships) > 0 {
		q = q.Where("membership NOT IN ?", notMemberships)
	}
	var rows []Membership
	if err := q.Order("localpart ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing members of %q: %w", roomID, err)
	}
	return rows, nil
}

// ListByUser implements [MembershipRepo].
func (r *MembershipRepoImpl) ListByUser(ctx context.Context, localpart string, notMemberships ...string) ([]Membership, error) {
	db := session(ctx, r.db).Where("localpart = ? AND forgotten = ?", localpart, false)
	if len(notMemberships) > 0 {
		db = db.Where("membership NOT IN (?)", notMemberships)
	}
	var rows []Membership
	if err := db.Order("stream_pos ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing memberships of %q: %w", localpart, err)
	}
	return rows, nil
}

// ListForgottenByUser implements [MembershipRepo].
func (r *MembershipRepoImpl) ListForgottenByUser(ctx context.Context, localpart string) ([]Membership, error) {
	var rows []Membership
	if err := session(ctx, r.db).
		Where("localpart = ? AND forgotten = ?", localpart, true).
		Order("stream_pos ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing forgotten memberships of %q: %w", localpart, err)
	}
	return rows, nil
}

// RoomsForUser implements [MembershipRepo].
func (r *MembershipRepoImpl) RoomsForUser(ctx context.Context, localpart string, memberships ...string) ([]string, error) {
	q := session(ctx, r.db).Model(&Membership{}).
		Where("localpart = ? AND forgotten = ?", localpart, false)
	if len(memberships) > 0 {
		q = q.Where("membership IN ?", memberships)
	}
	var roomIDs []string
	if err := q.Order("room_id ASC").Pluck("room_id", &roomIDs).Error; err != nil {
		return nil, fmt.Errorf("listing rooms of %q: %w", localpart, err)
	}
	return roomIDs, nil
}

// CountByMembership implements [MembershipRepo].
func (r *MembershipRepoImpl) CountByMembership(ctx context.Context, roomID string) (map[string]int, error) {
	type row struct {
		Membership string
		Count      int
	}
	var rows []row
	err := session(ctx, r.db).Model(&Membership{}).
		Select("membership, COUNT(*) AS count").
		Where("room_id = ?", roomID).
		Group("membership").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("counting members of %q: %w", roomID, err)
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.Membership] = r.Count
	}
	return counts, nil
}

// MarkForgotten implements [MembershipRepo].
func (r *MembershipRepoImpl) MarkForgotten(ctx context.Context, roomID, localpart string, forgotten bool) error {
	res := session(ctx, r.db).Model(&Membership{}).
		Where("room_id = ? AND localpart = ?", roomID, localpart).
		Update("forgotten", forgotten)
	if res.Error != nil {
		return fmt.Errorf("marking %q as forgotten in %q: %w", localpart, roomID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("membership of %q in %q: %w", localpart, roomID, ErrNotFound)
	}
	return nil
}

// RefreshProfile implements [MembershipRepo].
func (r *MembershipRepoImpl) RefreshProfile(ctx context.Context, localpart, displayName, avatarURL string) (int64, error) {
	updates := map[string]any{
		"display_name": displayName,
		"avatar_url":   avatarURL,
	}
	res := session(ctx, r.db).Model(&Membership{}).
		Where("localpart = ? AND forgotten = ?", localpart, false).
		Updates(updates)
	if res.Error != nil {
		return 0, fmt.Errorf("refreshing profile cache for %q: %w", localpart, res.Error)
	}
	return res.RowsAffected, nil
}

// DeleteForgetUser implements [MembershipRepo].
func (r *MembershipRepoImpl) DeleteForgetUser(ctx context.Context, roomID, localpart string) error {
	err := session(ctx, r.db).
		Where("room_id = ? AND localpart = ?", roomID, localpart).
		Delete(&Membership{}).Error
	if err != nil {
		return fmt.Errorf("removing membership of %q in %q: %w", localpart, roomID, err)
	}
	return nil
}

// NewMembershipRepo returns the [MembershipRepo] implementation.
func NewMembershipRepo(db *gorm.DB) MembershipRepo { return &MembershipRepoImpl{db: db} }
