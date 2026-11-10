package unit_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"

	"github.com/enimatek-nl/qwatrix/internal/store"
)

// oldTxnID is the pre-room-scope table: the same columns, but a primary key that leaves the room
// out. A database written by that build has to be upgradable in place, because it is the file an
// operator has on disk and a transaction-ID table cannot simply be recreated empty on a live
// server without letting replays through.
type oldTxnID struct {
	Localpart   string `gorm:"size:255;primaryKey:txn_pk"`
	DeviceID    string `gorm:"size:64;primaryKey:txn_pk"`
	TxnID       string `gorm:"size:256;primaryKey:txn_pk"`
	EventID     string `gorm:"size:256"`
	RoomID      string `gorm:"size:512"`
	EventType   string `gorm:"size:256"`
	PayloadHash string `gorm:"size:64"`
	CreatedMS   int64
}

func (oldTxnID) TableName() string { return "txn_ids" }

// TestOpeningADatabaseWithTheOldTransactionKey checks the migration that widens the key.
func TestOpeningADatabaseWithTheOldTransactionKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old-txn-scope.db")

	db, err := gorm.Open(gormlite.Open(dbPath), &gorm.Config{
		Logger: glogger.Default.LogMode(glogger.Silent),
	})
	if err != nil {
		t.Fatalf("opening the scratch database: %v", err)
	}
	if err := db.AutoMigrate(&oldTxnID{}); err != nil {
		t.Fatalf("creating the old table: %v", err)
	}
	if err := db.Create(&oldTxnID{
		Localpart: "alice", DeviceID: "DEVONE", TxnID: "1",
		EventID: "$original", RoomID: "!room:test.local:8448", EventType: "m.room.message",
		CreatedMS: 1,
	}).Error; err != nil {
		t.Fatalf("writing an old-shaped row: %v", err)
	}
	raw, err := db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='txn_ids'").
		Rows()
	if err == nil {
		defer func() { _ = raw.Close() }()
		for raw.Next() {
			var definition string
			if err := raw.Scan(&definition); err == nil &&
				strings.Contains(definition, "PRIMARY KEY") &&
				strings.Contains(definition[indexOfRoomID(definition):], "txn_id") {
				t.Log("old schema: " + definition)
			}
		}
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("closing: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	// Opening the store is what runs the migration.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening the store over the old schema: %v", err)
	}
	ctx := t.Context()

	// The row survived the rebuild.
	recorded, err := st.Txns.Get(ctx, "alice", "DEVONE", "!room:test.local:8448", "1")
	if err != nil {
		t.Fatalf("the pre-existing transaction was not carried across: %v", err)
	}
	if recorded.EventID != "$original" {
		t.Errorf("event ID = %q, want $original", recorded.EventID)
	}

	// And the new dimension works: the same transaction ID in another room is its own record.
	if err := st.Txns.Put(ctx, nil, &store.TxnID{
		Localpart: "alice", DeviceID: "DEVONE", RoomID: "!other:test.local:8448", TxnID: "1",
		EventID: "$second", EventType: "m.room.message",
	}); err != nil {
		t.Errorf("the widened key still rejects a second room: %v", err)
	}
	other, err := st.Txns.Get(ctx, "alice", "DEVONE", "!other:test.local:8448", "1")
	if err != nil || other.EventID != "$second" {
		t.Errorf("second-room transaction = %+v (err %v), want $second", other, err)
	}
	first, err := st.Txns.Get(ctx, "alice", "DEVONE", "!room:test.local:8448", "1")
	if err != nil || first.EventID != "$original" {
		t.Errorf("widening the key changed the first room's answer: %+v (err %v)", first, err)
	}
}

// indexOfRoomID keeps the schema-printing above honest without asserting on SQLite's exact
// quoting, which has changed between releases.
func indexOfRoomID(definition string) int {
	if i := strings.Index(definition, "PRIMARY KEY"); i >= 0 {
		return i
	}
	return 0
}
