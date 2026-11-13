// Package store is the data-access layer: one SQLite database reached through GORM, with a
// repository per aggregate.
//
// The event row's integer primary key doubles as the room-independent *stream position*
// that /sync and /messages page over, which is why it is an int64 autoincrement rather than
// the uint GORM would pick by default.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Tx is a transaction handle passed to [Store.Do]. It is an alias rather than a named type
// so that repositories keep taking *gorm.DB, while services never need to import GORM to
// write a transaction body.
type Tx = *gorm.DB

// ErrNotFound is returned by repositories when a requested row does not exist. Callers
// translate it into M_NOT_FOUND (or whatever the endpoint's spec says), never back to the
// caller directly.
var ErrNotFound = errors.New("store: not found")

// IsNotFound reports whether err is or wraps [ErrNotFound].
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// Store owns the database handle and every repository over it.
type Store struct {
	db *gorm.DB

	Users       UserRepo
	Devices     DeviceRepo
	Tokens      TokenRepo
	Rooms       RoomRepo
	Events      EventRepo
	State       StateRepo
	Memberships MembershipRepo
	Aliases     AliasRepo
	Txns        TxnRepo
	Presence    PresenceRepo
	// Receipts, AccountData, and their repositories hold the per-reader state that is not
	// room history: read receipts, the fully-read marker, and clients' own key/value data.
	Receipts    ReceiptRepo
	AccountData AccountDataRepo
	// Filters are clients' saved event filters.
	Filters FilterRepo
	// Keys holds devices' published identity and their prepaid message keys. End-to-end
	// encryption is built on a client fetching another user's device keys and spending one of
	// their one-time keys, and this server is where both are kept for its own devices.
	Keys KeyRepo
	// DeviceLists is the log of device-list changes addressed to observers.
	DeviceLists DeviceListRepo
	// Media is the uploaded-file metadata index.
	Media MediaRepo
	// ToDevice is the queue of messages sent to devices rather than rooms.
	ToDevice ToDeviceRepo
	// FederationOutbox is the ledger of peer copies this server still owes: events persisted here
	// whose delivery to another homeserver has not been confirmed yet.
	FederationOutbox FederationOutboxRepo
	// PushRules are the per-account notification rules.
	PushRules PushRuleRepo
	// Directory is the user directory: the names local users go by, per room.
	Directory DirectoryRepo

	// dml serialises write transactions. SQLite has one writer at a time and stream
	// positions must be handed out in commit order, so the homeserver enforces the same
	// discipline the database would rather than discovering it as SQLITE_BUSY.
	dml sync.Mutex
}

// Open opens (creating if necessary) the database at path and migrates the schema.
//
// path may be a file name, or ":memory:" for a shared-cache in-memory database (used by
// tests that do not care about persistence).
func Open(path string) (*Store, error) {
	dsn, err := dsnForPath(path)
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(gormlite.Open(dsn), &gorm.Config{
		// Silent by default: the SQL of a compliance failure is useful, the SQL of
		// every successful request is noise. slog carries the server's own logging.
		Logger: logger.Default.LogMode(logger.Silent),
		// Matrix identifiers arrive as JSON, not as table names; naming them
		// explicitly keeps the schema readable in a sqlite3 shell.
		NamingStrategy: nil,
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("opening database %q: %w", path, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("accessing underlying database: %w", err)
	}
	if err := configure(db, sqlDB); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}

	return &Store{
		db:               db,
		Users:            NewUserRepo(db),
		Devices:          NewDeviceRepo(db),
		Tokens:           NewTokenRepo(db),
		Rooms:            NewRoomRepo(db),
		Events:           NewEventRepo(db),
		State:            NewStateRepo(db),
		Memberships:      NewMembershipRepo(db),
		Aliases:          NewAliasRepo(db),
		Txns:             NewTxnRepo(db),
		Presence:         NewPresenceRepo(db),
		Receipts:         NewReceiptRepo(db),
		Filters:          NewFilterRepo(db),
		AccountData:      NewAccountDataRepo(db),
		Keys:             NewKeyRepo(db),
		DeviceLists:      NewDeviceListRepo(db),
		Media:            NewMediaRepo(db),
		ToDevice:         NewToDeviceRepo(db),
		FederationOutbox: NewFederationOutboxRepo(db),
		PushRules:        NewPushRuleRepo(db),
		Directory:        NewDirectoryRepo(db),
	}, nil
}

// _txlockImmediate is written out separately so a test can substitute a different value and
// see the locking behaviour change.
const _txlockImmediate = "?_txlock=immediate"

// lockingParameters are the DSN settings that decide how connections share one writer.
//
// `_txlock=immediate` is the important one and the fix for a real failure: a transaction
// opened the default deferred way takes no lock at BEGIN, so it reads first (a read lock) and
// only discovers it needs the write lock when it inserts. At that point SQLite refuses with
// SQLITE_BUSY *immediately* — busy_timeout does not apply, because waiting could never
// succeed against a reader that is now this same transaction. Under Complement's parallel
// subtests that turned ordinary joins and invites into 500s. Taking the write lock at BEGIN
// means a writer waits, which is what busy_timeout is for.
const lockingParameters = _txlockImmediate +
	"&_pragma=busy_timeout(30000)" +
	"&_pragma=journal_mode(WAL)" +
	"&_pragma=foreign_keys(1)" +
	"&_pragma=synchronous(NORMAL)"

// dsnForPath turns a plain file name into a URI with the pragmas that must be set at open
// time rather than per connection.
func dsnForPath(path string) (string, error) {
	switch path {
	case "":
		return "", fmt.Errorf("database path must not be empty")
	case ":memory:":
		// A shared cache makes the in-memory database visible to every connection the
		// pool opens, which is what "in-memory for this process" should mean. The pragmas
		// are the same as for a file so that a test exercises the locking behaviour the
		// deployed server has, rather than a laxer one that only exists in-process.
		// The separator differs because this URI already carries its query.
		return "file:qwatrix?mode=memory&cache=shared" + strings.Replace(lockingParameters, "?", "&", 1), nil
	default:
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolving database path: %w", err)
		}
		if !strings.Contains(abs, "/") {
			abs = "./" + abs
		}
		return "file:" + abs + lockingParameters, nil
	}
}

// configure applies connection-pool and PRAGMA settings.
func configure(db *gorm.DB, sqlDB *sql.DB) error {
	// Several connections, one writer: writes are serialised by [Store.Do]'s mutex, while
	// readers run freely. That is the shape SQLite under WAL wants — and the reason a
	// repository must never open a second connection inside a write transaction, which
	// [session] guarantees by handing it the transaction instead.
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxLifetime(0)

	// The pragmas above are per-connection where they are not persistent; verify the
	// durable ones so a mis-set journal mode shows up at start-up rather than as
	// unexplained locking.
	var journal string
	if err := db.Raw("PRAGMA journal_mode").Scan(&journal).Error; err != nil {
		return fmt.Errorf("reading journal_mode: %w", err)
	}
	if !strings.EqualFold(journal, "wal") && !strings.Contains(journal, "memory") {
		slog.Warn("database is not in WAL mode", "journal_mode", journal)
	}
	var foreignKeys int
	if err := db.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil {
		return fmt.Errorf("reading foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("foreign keys are not enabled (pragma read %d)", foreignKeys)
	}
	return nil
}

// migrate creates or updates every table. It is run on start-up, in dependency-free order;
// GORM adds columns but never removes them, which is the conservative direction for a
// single-file database operators keep backups of.
func migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&User{},
		&Device{},
		&AccessToken{},
		&Room{},
		&EventRow{},
		&RoomState{},
		&Membership{},
		&RoomAlias{},
		&TxnID{},
		&Presence{},
		&Receipt{},
		&Filter{},
		&AccountData{},
		&DeviceKey{},
		&OneTimeKey{},
		&DirectoryEntry{},
		&PushRule{},
		&ToDeviceMessage{},
		&DeviceMessageReceipt{},
		&MediaFile{},
		&DeviceListChange{},
		&FederationOutbox{},
	); err != nil {
		return fmt.Errorf("migrating schema: %w", err)
	}
	return migrateTransactionScope(db)
}

// migrateTransactionScope rebuilds `txn_ids` when it still has the old primary key.
//
// Transaction IDs are scoped to a room as well as to a device, and that changes the primary key —
// the one schema change GORM will not make for you, because SQLite has no ALTER TABLE for
// constraints. So the rebuild is explicit: copy the rows across, drop the old table only once the
// new one is in place and populated, and rename rather than delete along the way so that a failure
// part-way through leaves the data reachable instead of gone.
func migrateTransactionScope(db *gorm.DB) error {
	if !db.Migrator().HasTable(&TxnID{}) {
		return nil
	}
	var definition string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'txn_ids'").
		Scan(&definition).Error; err != nil {
		return fmt.Errorf("reading the txn_ids schema: %w", err)
	}
	// The new key names room_id inside its PRIMARY KEY clause; the old one does not.
	index := strings.Index(definition, "PRIMARY KEY")
	if index < 0 || strings.Contains(definition[index:], "room_id") {
		return nil
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Migrator().RenameTable("txn_ids", "txn_ids_pre_room_scope"); err != nil {
			return err
		}
		if err := tx.AutoMigrate(&TxnID{}); err != nil {
			return err
		}
		if err := tx.Exec(`INSERT OR IGNORE INTO txn_ids
				(localpart, device_id, room_id, txn_id, event_id, event_type, payload_hash, created_ms)
			SELECT localpart, device_id, room_id, txn_id, event_id, event_type, payload_hash, created_ms
			FROM txn_ids_pre_room_scope`).Error; err != nil {
			return err
		}
		return tx.Migrator().DropTable("txn_ids_pre_room_scope")
	})
	if err != nil {
		return fmt.Errorf("re-scoping txn_ids: %w", err)
	}
	return nil
}

// DB exposes the raw handle to sibling packages that need a transaction they can thread
// through several repositories. New code should prefer [Store.Do].
func (s *Store) DB() *gorm.DB { return s.db }

// Do runs fn inside a write transaction, holding the writer lock for its duration.
//
// fn is handed a context that carries the transaction, and every repository call made with
// that context joins the transaction. The context is passed in rather than documented,
// because a callback that reached for the caller's context would silently run its reads on a
// different connection — seeing none of the rows it had just written, and its writes waiting
// on the lock the transaction itself was holding. That failure mode is invisible to the
// compiler and looks like mysterious missing data, so the signature makes it impossible.
func (s *Store) Do(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	s.dml.Lock()
	defer s.dml.Unlock()

	// The [Store.dml] mutex already serialises this process's writers, so a lock refusal can
	// only come from something the mutex cannot see: a read on another pooled connection, a
	// checkpoint, or the container's filesystem. SQLite would rather refuse the statement than
	// wait for those, and a refused write transaction is rolled back and safe to repeat — the
	// reason the retry belongs here rather than in the handlers, where a caller cannot know
	// whether its write happened.
	//
	// An error from fn itself is returned untouched: it is the caller's verdict, not a
	// transient one, and replaying it would duplicate side effects.
	var lastErr error
	for attempt := 0; attempt < writeAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(2<<attempt) * time.Millisecond
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		err := s.doOnce(ctx, fn)
		if err == nil {
			return nil
		}
		if !IsLockError(err) {
			return err
		}
		lastErr = err
		slog.Warn("write transaction hit a lock; retrying",
			"attempt", attempt+1, "error", err)
	}
	return fmt.Errorf("after %d attempts: %w", writeAttempts, lastErr)
}

// writeAttempts is how often a write transaction is replayed after the database refused a
// lock. The number is small because the waits in front of it are long: a lock that survives a
// few hundred milliseconds is a real problem, not a queue.
const writeAttempts = 5

// doOnce runs one attempt of a write transaction.
func (s *Store) doOnce(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("beginning transaction: %w", tx.Error)
	}
	ctx = context.WithValue(ctx, txContextKey{}, tx)
	if err := fn(ctx, tx); err != nil {
		if rollback := tx.Rollback().Error; rollback != nil {
			slog.Error("rolling back transaction failed", "error", rollback, "cause", err)
		}
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// IsLockError reports whether an error is the database refusing a lock rather than rejecting
// the work: the first is worth retrying, the second is a fact about the request.
func IsLockError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") ||
		strings.Contains(text, "database table is locked") ||
		strings.Contains(text, "sqlite_busy") ||
		strings.Contains(text, "database is being attached")
}

// Close releases the database handle.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// txContextKey keys the in-flight transaction inside a context.
type txContextKey struct{}

// session returns the GORM handle a repository should use for this call: the transaction
// carried by ctx when the call happens inside [Store.Do], and the shared handle otherwise.
func session(ctx context.Context, db *gorm.DB) *gorm.DB {
	if tx, ok := ctx.Value(txContextKey{}).(*gorm.DB); ok && tx != nil {
		// The transaction already carries the context; re-using it keeps the statement on
		// the transaction's connection.
		return tx
	}
	return db.WithContext(ctx)
}

// nowMS is the millisecond clock every timestamp in the store uses, because Matrix speaks
// milliseconds since the epoch and mixing the two is a class of bug worth centralising.
func nowMS() int64 { return time.Now().UnixMilli() }
