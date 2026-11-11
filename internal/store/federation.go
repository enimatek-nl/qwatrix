package store

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// FederationOutbox is one event waiting to be delivered to one homeserver.
//
// Federation is asynchronous and the internet is unreliable, so a homeserver that sends a peer's copy
// of an event inline either makes its own users wait for somebody else's server or loses the event the
// moment the send fails. Both are unacceptable for a message: the sender's client was told it was sent.
// So the obligation to deliver is written down beside the event, in the same transaction, and a worker
// keeps trying until the peer has it.
//
// That co-location is the whole point. An outbox written after the event commits can be lost between
// the two, and the room then has an event that some members will never be told about — a permanent,
// silent divergence, which is the one failure a homeserver must not be able to produce.
type FederationOutbox struct {
	ID int64 `gorm:"primaryKey;autoIncrement" json:"-"`
	// Destination is the homeserver this copy is for, taken from a member's user ID rather than
	// guessed: a server is told about events in rooms it has members in, and no others.
	Destination string `gorm:"size:255;index:outbox_due" json:"destination"`
	RoomID      string `gorm:"size:512" json:"-"`
	EventID     string `gorm:"size:255;index" json:"-"`
	// JSON is the event as it was persisted — already signed by this server, because re-signing a
	// stored event would produce a different event ID from the one everybody else has.
	JSON string `gorm:"type:text" json:"-"`
	// Attempts counts the sends that failed, which bounds how long a peer that does not want us can
	// keep us asking.
	Attempts  int   `json:"-"`
	NextMS    int64 `gorm:"index:outbox_due,priority:2;index" json:"-"`
	CreatedMS int64 `json:"-"`
	// LastError is the most recent failure, kept for the operator. It is not a transcript: a peer's
	// whole error body on every retry would fill a disk.
	LastError string `gorm:"size:256" json:"-"`
}

// TableName pins the table name.
func (FederationOutbox) TableName() string { return "federation_outbox" }

// FederationOutboxRepo is the delivery ledger.
type FederationOutboxRepo interface {
	// Append queues copies for a set of destinations inside the caller's transaction, so an event and
	// the duty to deliver it cannot exist apart.
	Append(ctx context.Context, tx *gorm.DB, rows []*FederationOutbox) error
	// Due returns queued copies whose next attempt has arrived, oldest first and for no more distinct
	// destinations than the limit: one transaction per peer per pass, because a peer that is down
	// should cost one failing request, not one per event.
	Due(ctx context.Context, nowMS int64, destinations int) ([]FederationOutbox, error)
	// Deferred records a failed attempt and when to try again.
	Deferred(ctx context.Context, ids []int64, nextMS int64, reason string) error
	// Delete removes delivered copies.
	Delete(ctx context.Context, ids []int64) error
	// Pending counts what is still waiting, which is the number an operator watches when a peer has
	// been unreachable: it is how far behind this server is.
	Pending(ctx context.Context) (int64, error)
	// DropOld removes copies this server has given up on, and reports how many.
	DropOld(ctx context.Context, olderThanMS int64, attempts int) (int64, error)
}

// FederationOutboxRepoImpl is the GORM-backed [FederationOutboxRepo].
type FederationOutboxRepoImpl struct{ db *gorm.DB }

// NewFederationOutboxRepo builds the delivery ledger repository.
func NewFederationOutboxRepo(db *gorm.DB) *FederationOutboxRepoImpl {
	return &FederationOutboxRepoImpl{db: db}
}

// Append implements [FederationOutboxRepo].
func (r *FederationOutboxRepoImpl) Append(ctx context.Context, tx *gorm.DB, rows []*FederationOutbox) error {
	if len(rows) == 0 {
		return nil
	}
	if tx == nil {
		tx = r.db
	}
	if err := session(ctx, tx).Create(&rows).Error; err != nil {
		return fmt.Errorf("queueing %d federation deliveries: %w", len(rows), err)
	}
	return nil
}

// Due implements [FederationOutboxRepo].
func (r *FederationOutboxRepoImpl) Due(ctx context.Context, nowMS int64, destinations int) ([]FederationOutbox, error) {
	if destinations <= 0 {
		return nil, nil
	}
	// One pass per destination rather than per event: a room with forty members on two remote servers
	// needs two transactions, not eighty.
	var targets []string
	if err := session(ctx, r.db).
		Model(&FederationOutbox{}).
		Where("next_ms <= ?", nowMS).
		Distinct("destination").
		Limit(destinations).
		Order("destination ASC").
		Pluck("destination", &targets).Error; err != nil {
		return nil, fmt.Errorf("choosing federation deliveries: %w", err)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	var rows []FederationOutbox
	if err := session(ctx, r.db).
		Where("destination IN ? AND next_ms <= ?", targets, nowMS).
		Order("id ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading federation deliveries: %w", err)
	}
	return rows, nil
}

// Deferred implements [FederationOutboxRepo].
func (r *FederationOutboxRepoImpl) Deferred(ctx context.Context, ids []int64, nextMS int64, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(reason) > 256 {
		reason = reason[:256]
	}
	if err := session(ctx, r.db).Model(&FederationOutbox{}).
		Where("id IN ?", ids).
		Updates(map[string]any{
			"attempts":   gorm.Expr("attempts + 1"),
			"next_ms":    nextMS,
			"last_error": reason,
		}).Error; err != nil {
		return fmt.Errorf("deferring federation deliveries: %w", err)
	}
	return nil
}

// Delete implements [FederationOutboxRepo].
func (r *FederationOutboxRepoImpl) Delete(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if err := session(ctx, r.db).Where("id IN ?", ids).Delete(&FederationOutbox{}).Error; err != nil {
		return fmt.Errorf("clearing delivered federation events: %w", err)
	}
	return nil
}

// Pending implements [FederationOutboxRepo].
func (r *FederationOutboxRepoImpl) Pending(ctx context.Context) (int64, error) {
	var count int64
	if err := session(ctx, r.db).Model(&FederationOutbox{}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("counting pending federation deliveries: %w", err)
	}
	return count, nil
}

// DropOld implements [FederationOutboxRepo].
func (r *FederationOutboxRepoImpl) DropOld(ctx context.Context, olderThanMS int64, attempts int) (int64, error) {
	result := session(ctx, r.db).
		Where("created_ms < ? AND attempts >= ?", olderThanMS, attempts).
		Delete(&FederationOutbox{})
	if result.Error != nil {
		return 0, fmt.Errorf("discarding abandoned federation deliveries: %w", result.Error)
	}
	return result.RowsAffected, nil
}
