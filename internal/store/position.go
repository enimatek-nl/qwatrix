package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// StreamPosition is the one clock the whole server writes to.
//
// Everything a client may be told about — a room event, a read receipt, a presence change, a
// message sent to a device, a client's own account data — carries a position, and `/sync` decides
// what a client has already seen by comparing one token against all of them. That only works if
// the positions come from one clock: a single `since` cannot order two sequences that advance on
// their own, and a write whose position does not outrank the token already in the client's hand
// is a write that client is never told about.
//
// The row is seeded at zero and every allocation takes the maximum of itself and the event table,
// so a database written before this existed heals on first use: the clock jumps past whatever
// events it already holds rather than colliding with them.
type StreamPosition struct {
	// ID is always 1; there is one stream, and the single-row table is how a counter is kept
	// without inventing a table per kind of change.
	ID int `gorm:"primaryKey"`
	// Position is the last position handed out.
	Position int64 `gorm:"column:position"`
}

// TableName pins the table name.
func (StreamPosition) TableName() string { return "stream_position" }

// nextPosition takes the position after everything written so far.
//
// It reads the event table alongside its own counter because an event's position is the event's
// primary key, assigned by SQLite rather than here: an event can therefore be ahead of the
// counter, and taking a position that collides with it would let a client walk straight past one
// of the two. Inside a transaction, the read and the write are atomic against the single writer,
// so no two allocations share a position.
func nextPosition(ctx context.Context, db *gorm.DB) (int64, error) {
	session := positionSession(ctx, db)
	var highest int64
	err := session.Raw(`SELECT MAX(s.position, COALESCE((SELECT MAX(e.id) FROM events e), 0)) ` +
		`FROM stream_position s WHERE s.id = 1`).Scan(&highest).Error
	if err != nil {
		return 0, fmt.Errorf("reading the stream position: %w", err)
	}
	next := highest + 1
	err = session.Exec(`INSERT INTO stream_position (id, position) VALUES (1, ?) `+
		`ON CONFLICT(id) DO UPDATE SET position = excluded.position`, next).Error
	if err != nil {
		return 0, fmt.Errorf("advancing the stream position: %w", err)
	}
	return next, nil
}

// NextStreamPosition takes the next position on the one stream, for a service that stamps a row
// outside the repositories — a device-list change, a push-rule touch. Use it instead of reading the
// tip and adding one: with events taking their keys from this same clock, `MaxPosition() + 1` lands
// exactly where the next event does, and a client that has been told one of the two never learns of
// the other.
func (s *Store) NextStreamPosition(ctx context.Context) (int64, error) {
	return nextPosition(ctx, s.db)
}

// currentPosition reports the end of the stream without taking a position from it, which is what
// a sync response reports as `next_batch`.
func currentPosition(ctx context.Context, db *gorm.DB) (int64, error) {
	session := positionSession(ctx, db)
	var highest int64
	err := session.Raw(`SELECT COALESCE(MAX(s.position, COALESCE((SELECT MAX(e.id) FROM events e), 0)), 0) ` +
		`FROM stream_position s WHERE s.id = 1`).Scan(&highest).Error
	if err != nil {
		return 0, fmt.Errorf("reading the end of the stream: %w", err)
	}
	return highest, nil
}

// positionSession picks the transaction to work in, following the convention the other
// repositories use: an explicit transaction wins, then the one the context carries.
func positionSession(ctx context.Context, db *gorm.DB) *gorm.DB {
	return session(ctx, db)
}

// seedStreamPosition makes sure the counter's row exists, so the allocations above never have to
// handle the empty case.
func seedStreamPosition(db *gorm.DB) error {
	if err := db.Exec(`INSERT INTO stream_position (id, position) VALUES (1, 0) ON CONFLICT(id) DO NOTHING`).Error; err != nil {
		return errors.New("seeding the stream position: " + err.Error())
	}
	return nil
}
