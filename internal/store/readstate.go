package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// receiptConflict and accountDataConflict spell the upsert out explicitly.
//
// GORM decides whether a Save is an insert or an update by checking the primary key for zero
// values, and both of these rows legitimately hold an empty string in one key column — no room
// for global account data, no thread for an unthreaded receipt. GORM reads that empty string as
// "no key yet", inserts, and SQLite answers "duplicated key not allowed", so a client updating a
// receipt or an account-data key for the second time would get an internal server error. Naming
// the columns takes the guesswork out of it.
var (
	receiptConflict = clause.OnConflict{
		Columns: []clause.Column{{Name: "room_id"}, {Name: "localpart"},
			{Name: "receipt_type"}, {Name: "thread_id"}},
		UpdateAll: true,
	}
	accountDataConflict = clause.OnConflict{
		Columns:   []clause.Column{{Name: "localpart"}, {Name: "room_id"}, {Name: "type"}},
		UpdateAll: true,
	}
)

// Receipt is one read receipt: "this user had read up to this event".
//
// Receipts are persisted rather than kept in memory, unlike typing indicators, because a receipt
// is a fact a client expects to still be true after a restart and across its own devices: a phone
// that marks a room read must stop the laptop showing a badge, and a server that forgot receipts
// on restart would re-light every badge in the account.
type Receipt struct {
	// Composite key: a user has at most one receipt of a given kind per room, and updating it
	// in place is what keeps "the room's current receipts" a bounded query no matter how long
	// the room has been read and re-read.
	RoomID      string `gorm:"primaryKey;size:512"`
	Localpart   string `gorm:"primaryKey;size:256"`
	ReceiptType string `gorm:"primaryKey;size:64"`
	// ThreadID is part of the key so a threaded receipt (`m.read` for a thread) can coexist
	// with the room's unthreaded one, which is what `TestThreadedReceipts` checks.
	ThreadID string `gorm:"primaryKey;size:256;default:''"`

	EventID   string `gorm:"size:512;index"`
	StreamPos int64  `gorm:"index"`
	TS        int64  `json:"ts"`
}

// TableName pins the table name so the composite key stays readable in queries.
func (Receipt) TableName() string { return "receipts" }

// ReceiptRepo stores read receipts.
type ReceiptRepo interface {
	// Put records a receipt, moving an existing one into place. It returns the stored row so
	// the caller can report what changed without a second read.
	Put(ctx context.Context, receipt *Receipt) (*Receipt, error)
	// ListByRoom returns each user's receipt of each kind, which for a fresh sync is the
	// room's current read state. Only the newest per (user, type, thread) exists by
	// construction, so this cannot grow with the room's history.
	ListByRoom(ctx context.Context, roomID string, after int64) ([]Receipt, error)
}

// ReceiptRepoImpl is the [ReceiptRepo] on the shared handle.
type ReceiptRepoImpl struct{ db *gorm.DB }

// NewReceiptRepo builds a repository on an open handle.
func NewReceiptRepo(db *gorm.DB) *ReceiptRepoImpl { return &ReceiptRepoImpl{db: db} }

// Put implements [ReceiptRepo].
func (r *ReceiptRepoImpl) Put(ctx context.Context, receipt *Receipt) (*Receipt, error) {
	if receipt.TS == 0 {
		receipt.TS = time.Now().UnixMilli()
	}
	// The stream position ties the receipt to the event stream so an incremental /sync can
	// tell a receipt that happened after its token from one it already reported. Reusing the
	// current maximum is deliberate: receipts are not events and must not advance the stream,
	// or every receipt would look like new room history to every other client.
	if receipt.StreamPos == 0 {
		var position int64
		if err := session(ctx, r.db).Model(&EventRow{}).
			Select("COALESCE(MAX(id), 0)").Scan(&position).Error; err != nil {
			return nil, errors.New("reading the stream position for a receipt: " + err.Error())
		}
		receipt.StreamPos = position
	}
	err := session(ctx, r.db).Clauses(receiptConflict).Save(receipt).Error
	if err != nil {
		return nil, errors.New("storing a receipt: " + err.Error())
	}
	return receipt, nil
}

// ListByRoom implements [ReceiptRepo].
func (r *ReceiptRepoImpl) ListByRoom(ctx context.Context, roomID string, after int64) ([]Receipt, error) {
	var receipts []Receipt
	query := session(ctx, r.db).Where("room_id = ?", roomID)
	if after > 0 {
		query = query.Where("stream_pos > ?", after)
	}
	err := query.Order("stream_pos asc").Find(&receipts).Error
	if err != nil {
		return nil, errors.New("listing receipts for " + roomID + ": " + err.Error())
	}
	return receipts, nil
}

// AccountData is a client's own key/value state, globally or scoped to one room.
//
// This is deliberately not modelled as room events: account data is private to the account, is
// never part of the room's history or state, and must not reach other members. Storing it as an
// upserted row per (account, room, type) mirrors how clients use it — overwrite the whole blob.
type AccountData struct {
	Localpart string `gorm:"primaryKey;size:256"`
	// RoomID is empty for global account data. It is part of the key rather than nullable so
	// that "global" and "for this room" cannot be confused by a missing value.
	RoomID string `gorm:"primaryKey;size:512;default:''"`
	Type   string `gorm:"primaryKey;size:256"`

	Content string `gorm:"type:text"`
	// StreamPos lets /sync decide whether this changed after the client's token.
	StreamPos int64 `gorm:"index"`
}

// TableName pins the table name.
func (AccountData) TableName() string { return "account_data" }

// AccountDataRepo stores per-account key/value state.
type AccountDataRepo interface {
	Put(ctx context.Context, data *AccountData) error
	Get(ctx context.Context, localpart, roomID, dataType string) (*AccountData, error)
	// List returns the account's entries; a roomID of "" asks for global account data, while
	// ListByRooms below is how a sync collects the per-room sections in one query.
	List(ctx context.Context, localpart, roomID string) ([]AccountData, error)
	ListByRooms(ctx context.Context, localpart string, roomIDs []string, after int64) ([]AccountData, error)
	Delete(ctx context.Context, localpart, roomID, dataType string) error
}

// AccountDataRepoImpl is the [AccountDataRepo] on the shared handle.
type AccountDataRepoImpl struct{ db *gorm.DB }

// NewAccountDataRepo builds a repository on an open handle.
func NewAccountDataRepo(db *gorm.DB) *AccountDataRepoImpl { return &AccountDataRepoImpl{db: db} }

// Put implements [AccountDataRepo].
func (r *AccountDataRepoImpl) Put(ctx context.Context, data *AccountData) error {
	if data.Content == "" || !json.Valid([]byte(data.Content)) {
		return errors.New("account data content must be a JSON object")
	}
	if data.StreamPos == 0 {
		var position int64
		if err := session(ctx, r.db).Model(&EventRow{}).
			Select("COALESCE(MAX(id), 0)").Scan(&position).Error; err != nil {
			return errors.New("reading the stream position for account data: " + err.Error())
		}
		data.StreamPos = position
	}
	err := session(ctx, r.db).Clauses(accountDataConflict).Save(data).Error
	if err != nil {
		return errors.New("storing account data: " + err.Error())
	}
	return nil
}

// Get implements [AccountDataRepo].
func (r *AccountDataRepoImpl) Get(ctx context.Context, localpart, roomID, dataType string) (*AccountData, error) {
	var data AccountData
	err := session(ctx, r.db).
		Where("localpart = ? AND room_id = ? AND type = ?", localpart, roomID, dataType).
		First(&data).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("account data %q/%q/%q: %w", localpart, roomID, dataType, ErrNotFound)
		}
		return nil, errors.New("reading account data: " + err.Error())
	}
	return &data, nil
}

// List implements [AccountDataRepo].
func (r *AccountDataRepoImpl) List(ctx context.Context, localpart, roomID string) ([]AccountData, error) {
	var rows []AccountData
	err := session(ctx, r.db).
		Where("localpart = ? AND room_id = ?", localpart, roomID).
		Order("type asc").Find(&rows).Error
	if err != nil {
		return nil, errors.New("listing account data: " + err.Error())
	}
	return rows, nil
}

// ListByRooms implements [AccountDataRepo].
func (r *AccountDataRepoImpl) ListByRooms(ctx context.Context, localpart string, roomIDs []string, after int64) ([]AccountData, error) {
	if len(roomIDs) == 0 {
		return nil, nil
	}
	var rows []AccountData
	query := session(ctx, r.db).
		Where("localpart = ? AND room_id IN ?", localpart, roomIDs)
	if after > 0 {
		query = query.Where("stream_pos > ?", after)
	}
	err := query.Order("stream_pos asc").Find(&rows).Error
	if err != nil {
		return nil, errors.New("listing room account data: " + err.Error())
	}
	return rows, nil
}

// Delete implements [AccountDataRepo].
func (r *AccountDataRepoImpl) Delete(ctx context.Context, localpart, roomID, dataType string) error {
	err := session(ctx, r.db).
		Where("localpart = ? AND room_id = ? AND type = ?", localpart, roomID, dataType).
		Delete(&AccountData{}).Error
	if err != nil {
		return errors.New("deleting account data: " + err.Error())
	}
	return nil
}

// Typing is who is typing right now.
//
// Typing indicators are kept in memory on purpose. They are not history: the spec says they
// expire after a timeout, they are not persisted or federated as events, and a client that
// reconnects does not want to be told somebody was typing four minutes ago. Keeping them in the
// database would make a restart claim that typing is happening when the person has long stopped.
type Typing struct {
	RoomID    string
	Localpart string
	// ExpiresMS is when the indicator lapses; a typing indicator with no explicit stop must
	// still disappear, or a client would show a spinner forever after a crashed browser tab.
	ExpiresMS int64
}

// TypingTracker holds the current typing indicators.
type TypingTracker struct {
	mu      sync.Mutex
	entries map[string]map[string]int64 // room ID -> localpart -> expiry
	// changed holds, per room, the stream position the typing list last moved. A sync compares
	// it against its token: a typing indicator is reported when it changes, not on every sync.
	changedPositions map[string]int64
	nowMS            func() int64
	changed          func()
	// position hands out a position in the event stream for a change that is not itself an
	// event, so that one token orders room history, presence, and typing together.
	position func() int64
}

// NewTypingTracker builds an empty tracker. changed is called after every accepted update so
// waiting /sync long-pollers wake up and find it; without that, a typing indicator would only be
// seen when something else happened to wake the poll.
func NewTypingTracker(changed func(), position func() int64) *TypingTracker {
	if position == nil {
		position = func() int64 { return 0 }
	}
	return &TypingTracker{
		entries:          map[string]map[string]int64{},
		changedPositions: map[string]int64{},
		nowMS:            func() int64 { return time.Now().UnixMilli() },
		changed:          changed,
		position:         position,
	}
}

// Set records that a user is typing, or stops them when typing is false. A timeout of zero or
// less with typing true means "no expiry hint given", and is given a short default so a client
// that forgets to stop still stops.
func (t *TypingTracker) Set(roomID, localpart string, typing bool, timeoutMS int64) {
	if t == nil {
		return
	}
	const defaultTypingTimeoutMS = 30000
	t.mu.Lock()
	if typing {
		if timeoutMS <= 0 {
			timeoutMS = defaultTypingTimeoutMS
		}
		room := t.entries[roomID]
		if room == nil {
			room = map[string]int64{}
			t.entries[roomID] = room
		}
		room[localpart] = t.nowMS() + timeoutMS
	} else if room := t.entries[roomID]; room != nil {
		delete(room, localpart)
		if len(room) == 0 {
			delete(t.entries, roomID)
		}
	}
	// Every accepted update moves the room's position, including one that emptied the list: a
	// client has to be able to tell "typing stopped" apart from "nothing has happened".
	t.changedPositions[roomID] = t.position()
	t.mu.Unlock()
	if t.changed != nil {
		t.changed()
	}
}

// ChangedAfter reports whether a room's typing list moved after a sync token's position.
//
// Typing is edge-triggered deliberately. Emitting the indicator on every sync would make every
// joined room look like it had something new on every sync, so a client would re-send for all of
// them forever; and a client that is told nothing is not left guessing, because the spec's model
// is that a typing indicator lapses on its own after its timeout.
func (t *TypingTracker) ChangedAfter(roomID string, since int64) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	position, ok := t.changedPositions[roomID]
	if !ok {
		return false
	}
	return position > since
}

// Typists returns the users still typing in a room, dropping whoever has expired.
func (t *TypingTracker) Typists(roomID string) []string {
	if t == nil {
		return nil
	}
	now := t.nowMS()
	t.mu.Lock()
	defer t.mu.Unlock()
	room := t.entries[roomID]
	var names []string
	for localpart, expiry := range room {
		if expiry > now {
			names = append(names, localpart)
			continue
		}
		delete(room, localpart)
	}
	if len(room) == 0 {
		delete(t.entries, roomID)
	}
	// Sorted so two syncs of an unchanged room agree; a map's order would make a client think
	// the typing list keeps changing.
	sort.Strings(names)
	return names
}
