package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// TxnID remembers the event a client transaction ID produced, which is what makes
// PUT /rooms/{id}/send/{type}/{txnId} idempotent: a retry answers with the event the first
// attempt created instead of creating a second one.
//
// The scope is (user, device, room, transaction ID).
//
// Two devices of the same user may reuse a transaction ID independently — a client's counter
// belongs to a device, and this is what makes a retried request idempotent without serialising
// a user's two phones. The room is part of the key for the same reason: a client that numbers
// its transactions "1", "2", "3" per device will happily send "2" to a second room, and a server
// that treats that as a replay either refuses a legitimate send or, worse, answers with an event
// from a room the client is not talking to in this request. Complement pins both dimensions.
type TxnID struct {
	Localpart string `gorm:"size:255;primaryKey:txn_pk" json:"localpart"`
	DeviceID  string `gorm:"size:64;primaryKey:txn_pk" json:"device_id"`
	RoomID    string `gorm:"size:512;primaryKey:txn_pk" json:"room_id"`
	TxnID     string `gorm:"size:256;primaryKey:txn_pk" json:"txn_id"`
	// EventID is the event the transaction produced.
	EventID string `gorm:"size:256" json:"event_id"`
	// EventType records what it was, for diagnostics.
	EventType string `gorm:"size:256" json:"event_type"`
	// PayloadHash is a hash of the request body. Reusing a transaction ID with different
	// content is a client bug the spec wants reported (M_TRANSACTION_FAILED), not
	// silently ignored.
	PayloadHash string `gorm:"size:64" json:"payload_hash"`
	// CreatedMS is when the transaction was first seen.
	CreatedMS int64 `json:"created_ms"`
}

// TableName pins the table name.
func (TxnID) TableName() string { return "txn_ids" }

// TxnRepo is the client-transaction repository.
type TxnRepo interface {
	// Put records a transaction. Repeating the same key is an [ErrAlreadyExists].
	Put(ctx context.Context, tx *gorm.DB, txn *TxnID) error
	// Get returns the transaction a device used in one room, or [ErrNotFound].
	Get(ctx context.Context, localpart, deviceID, roomID, txnID string) (*TxnID, error)
	// DeleteByDevice removes the transactions of a device that is being deleted.
	DeleteByDevice(ctx context.Context, localpart, deviceID string) error
}

// TxnRepoImpl is the GORM-backed [TxnRepo].
type TxnRepoImpl struct{ db *gorm.DB }

// Put implements [TxnRepo].
func (r *TxnRepoImpl) Put(ctx context.Context, tx *gorm.DB, txn *TxnID) error {
	if txn.CreatedMS == 0 {
		txn.CreatedMS = nowMS()
	}
	db := session(ctx, r.db)
	if tx != nil {
		db = tx.WithContext(ctx)
	}
	err := db.WithContext(ctx).Create(txn).Error
	switch {
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return fmt.Errorf("txn %q on device %q: %w", txn.TxnID, txn.DeviceID, ErrAlreadyExists)
	case err != nil:
		return fmt.Errorf("recording txn %q: %w", txn.TxnID, err)
	}
	return nil
}

// Get implements [TxnRepo].
func (r *TxnRepoImpl) Get(ctx context.Context, localpart, deviceID, roomID, txnID string) (*TxnID, error) {
	var row TxnID
	err := session(ctx, r.db).
		Where("localpart = ? AND device_id = ? AND room_id = ? AND txn_id = ?",
			localpart, deviceID, roomID, txnID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("txn %q: %w", txnID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading txn %q: %w", txnID, err)
	}
	return &row, nil
}

// DeleteByDevice implements [TxnRepo].
func (r *TxnRepoImpl) DeleteByDevice(ctx context.Context, localpart, deviceID string) error {
	err := session(ctx, r.db).
		Where("localpart = ? AND device_id = ?", localpart, deviceID).
		Delete(&TxnID{}).Error
	if err != nil {
		return fmt.Errorf("deleting txns of device %q: %w", deviceID, err)
	}
	return nil
}

// NewTxnRepo returns the [TxnRepo] implementation.
func NewTxnRepo(db *gorm.DB) TxnRepo { return &TxnRepoImpl{db: db} }
