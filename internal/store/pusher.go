package store

// Push delivery has three tables because it has three lifetimes.
//
// A `pushers` row is a standing instruction: "tell this device about things, at this URL". It
// outlives the request that made it and dies when the session that registered it does — a pusher
// is a device's permission for its own notifications, and a password change that revokes a session
// has to take the pusher with it, or a device the user just signed out of keeps receiving their
// messages forever.
//
// A `notifications` row is a recorded judgement: "this event was worth interrupting this person
// for". It is what `GET /notifications` reads and what an unread count is summed from, so it exists
// whether or not anybody has a device to deliver it to.
//
// A `pending_pushes` row is an unpaid debt: "this gateway has not confirmed this event yet". It is
// the only one of the three the delivery loop deletes, and it is why a homeserver that was briefly
// offline from a push gateway still delivers: the intent outlives the attempt.
//
// The fourth table, `push_cursors`, holds one row: how far the evaluator has walked the event
// stream. It is a cursor rather than a per-event flag because the alternative is writing a
// notification marker into every event's write path, which charges every message on the server for
// a feature most of its users have not switched on.

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Pusher is one registered push-notification endpoint.
//
// The identity of a pusher is (account, app ID, push key), which is what the specification's
// "set a pusher" upsert keys on: an app re-registering its pusher after a token refresh updates the
// row rather than adding a second one, and a server that appended instead would notify twice — an
// error the user experiences as their phone buzzing twice for every message.
type Pusher struct {
	// Localpart is whose pusher this is. It is the local part, not the user ID: every pusher
	// belongs to a user of this server, because a pusher is only ever about delivering to
	// somebody this server is responsible for.
	Localpart string `gorm:"size:255;primaryKey:pusher_pk" json:"-"`
	// AppID is the application's own identifier (a reverse-domain name, or a UnifiedPush
	// provider's URL). It is stored as the client wrote it.
	AppID string `gorm:"size:255;primaryKey:pusher_pk;column:app_id" json:"app_id"`
	// PushKey identifies the device at the gateway: an FCM token, an APNs token, a UnifiedPush
	// topic path. The gateway, not this server, decides whether it is valid — which is why a
	// rejected push key arrives on a response to a delivery rather than at registration.
	PushKey string `gorm:"size:512;primaryKey:pusher_pk" json:"push_key"`
	// Kind is the delivery mechanism. `http` is the only one the specification defines.
	Kind string `gorm:"size:16;column:kind" json:"kind"`
	// AccessToken is the session that registered the pusher. It is not the owner — the account
	// is — but it is what the pusher is bound to, and the reason the row is alive: when the
	// token is revoked, the pusher goes with it. Every read of this table filters on that, so
	// the rule cannot be forgotten by a caller who forgot to prune.
	AccessToken string `gorm:"size:128;index;column:access_token" json:"-"`
	// DeviceID is that session's device, reported back to clients so a person can see which of
	// their sessions a notification app is registered on.
	DeviceID string `gorm:"size:64;index" json:"device_id,omitempty"`
	// Data is the client's own object: the gateway URL, `event_id_only`, `format`, and anything
	// the app wants echoed back to it. It is kept as JSON rather than as columns because it is
	// opaque to this server apart from the two keys the delivery loop has to read, and
	// inventing a column for each would mean a client's own field of view being narrowed by
	// whatever this server happened to predict.
	Data string `gorm:"type:text" json:"-"`
	// AppDisplayName, DeviceDisplayName, ProfileTag and Lang are the human-facing and locale
	// fields a gateway puts in the notification ("Message from X on Y's phone", in the language
	// the user's phone speaks).
	AppDisplayName    string `gorm:"size:255" json:"app_display_name,omitempty"`
	DeviceDisplayName string `gorm:"size:255" json:"device_display_name,omitempty"`
	ProfileTag        string `gorm:"size:255" json:"profile_tag,omitempty"`
	Lang              string `gorm:"size:32" json:"lang,omitempty"`
	// UpdatedMS is when the pusher was last registered or refreshed, in milliseconds since the
	// epoch. Gateways know it as `pushkey_ts`, in seconds.
	UpdatedMS int64 `json:"-"`
}

// TableName pins the table name.
func (Pusher) TableName() string { return "pushers" }

// Notification is one event that a push rule found worth bringing to somebody's attention.
//
// The actions are stored rather than recomputed on read for two reasons. They describe the rules at
// the moment the event arrived — a person who mutes a room today should not have yesterday's
// notifications rewritten as uninteresting — and `GET /notifications` answers with them, so a read
// path that re-evaluated rules would be a read path that could not answer after a rule edit.
//
// What is *not* stored is whether the person has read it. That is a receipt, and receipts are the
// single source of truth for how far somebody has read; a second copy here would be a second
// opinion, and the two disagree the first time a phone reads a message and a laptop does not.
type Notification struct {
	Localpart string `gorm:"size:255;primaryKey:notification_pk" json:"-"`
	EventID   string `gorm:"size:256;primaryKey:notification_pk" json:"event_id"`
	RoomID    string `gorm:"size:512;index" json:"room_id"`
	// Type and Sender are kept for the two places that need them without the event: the
	// `only=mentions` filter, and telling a notification from a highlight when the caller asks.
	Type   string `gorm:"size:256" json:"type"`
	Sender string `gorm:"size:512" json:"sender"`
	// ThreadID is the thread the event belongs to, when it has one, because whether a threaded
	// notification has been read is answered by that thread's receipt and nothing else.
	ThreadID string `gorm:"size:256" json:"thread_id,omitempty"`
	// Actions is the winning rule's action list as a JSON array.
	Actions string `gorm:"type:text" json:"-"`
	// Position is the event's stream position: what orders notifications, what a page token is
	// made from, and what a read receipt is compared against.
	Position  int64 `gorm:"index" json:"-"`
	CreatedMS int64 `json:"-"`
}

// TableName pins the table name.
func (Notification) TableName() string { return "notifications" }

// PendingPush is one delivery this server owes a push gateway.
//
// It is per (pusher, event) rather than per event because a gateway's rejection names a push key,
// and because two devices of one user can have pushers at different gateways — one at Element's,
// one at the user's own ntfy server — and one of them being unreachable says nothing about the
// other.
type PendingPush struct {
	ID        int64  `gorm:"primaryKey;autoIncrement" json:"-"`
	Localpart string `gorm:"size:255;index:pending_pusher_idx" json:"-"`
	AppID     string `gorm:"size:255;index:pending_pusher_idx" json:"-"`
	PushKey   string `gorm:"size:512;index:pending_pusher_idx" json:"-"`
	EventID   string `gorm:"size:256;index" json:"-"`
	RoomID    string `gorm:"size:512" json:"-"`
	// Attempts counts the deliveries that did not succeed, and is what eventually lets a dead
	// gateway stop being this server's problem: a pusher whose gateway has refused for long
	// enough is dropped rather than retried until the disk fills.
	Attempts int64 `json:"-"`
	// NextAttemptMS is when this row may be tried again, in milliseconds since the epoch. It is
	// indexed because the delivery loop's question is exactly "what is due", and ordering the
	// retries by that index is what keeps a broken gateway from being retried in a tight loop.
	NextAttemptMS int64  `gorm:"index" json:"-"`
	LastError     string `gorm:"size:512" json:"-"`
	CreatedMS     int64  `json:"-"`
}

// TableName pins the table name.
func (PendingPush) TableName() string { return "pending_pushes" }

// PushCursor is the evaluator's position in the event stream. One row, keyed by name, so that a
// future second consumer (an e-mail notifier, say) starts from its own place rather than inheriting
// a peer's progress and silently skipping everything in between.
type PushCursor struct {
	Name     string `gorm:"size:64;primaryKey" json:"-"`
	Position int64  `json:"-"`
}

// TableName pins the table name.
func (PushCursor) TableName() string { return "push_cursors" }

// pusherConflict is the pusher upsert: same account, same app, same device key means the same
// pusher, and the newer registration wins outright.
var pusherConflict = clause.OnConflict{
	Columns:   []clause.Column{{Name: "localpart"}, {Name: "app_id"}, {Name: "push_key"}},
	UpdateAll: true,
}

// livePushers is the condition that separates a pusher that may be used from one whose session is
// gone.
//
// It is a fragment rather than a prune-on-read because a prune can be forgotten: expressed as part
// of every read, a revoked session's pusher cannot be delivered to even in the window before the
// janitor runs, and cannot be listed to the user either.
const livePushers = "EXISTS (SELECT 1 FROM access_tokens WHERE access_tokens.token = pushers.access_token)"

// PushRepo is the push-delivery repository.
type PushRepo interface {
	// PutPusher registers or refreshes a pusher.
	PutPusher(ctx context.Context, pusher *Pusher) error
	// DeletePusher removes one pusher and reports whether anything was removed.
	DeletePusher(ctx context.Context, localpart, appID, pushKey string) (bool, error)
	// DeletePushersOfSession removes the pushers a revoked session left behind.
	DeletePushersOfSession(ctx context.Context, accessToken string) (int, error)
	// GetPusher returns one live pusher, or [ErrNotFound].
	GetPusher(ctx context.Context, localpart, appID, pushKey string) (*Pusher, error)
	// ListPushers returns an account's live pushers, oldest registration first. This includes
	// pushers registered by the account's other sessions, which is what the specification asks
	// for and what lets a person see the phone they no longer hold.
	ListPushers(ctx context.Context, localpart string) ([]Pusher, error)
	// ListAllPushers returns every live pusher, for the delivery loop.
	ListAllPushers(ctx context.Context) ([]Pusher, error)
	// CountPushers counts an account's live pushers, which is what bounds how many a single
	// account may register.
	CountPushers(ctx context.Context, localpart string) (int, error)
	// PrunePushers deletes pushers whose session no longer exists, and reports how many.
	PrunePushers(ctx context.Context) (int, error)

	// Cursor returns how far the named evaluator has walked the event stream. An unseen name
	// starts at 0, which means "from the beginning of time" — the conservative answer for a
	// consumer that has never run.
	Cursor(ctx context.Context, name string) (int64, error)
	// SetCursor records that progress.
	SetCursor(ctx context.Context, name string, position int64) error

	// AddNotification records that an event is worth this person's attention. Repeating it for
	// the same event is a no-op, because delivery retries must not multiply notifications.
	AddNotification(ctx context.Context, notification *Notification) error
	// GetNotification returns the recorded judgement for one event and one person, or
	// [ErrNotFound]. Delivery needs it for the rule's actions — a gateway is told how to present
	// a notification, not merely that one happened — and asking the stored row rather than
	// re-running the rules is what makes "mute this room" not rewrite the past.
	GetNotification(ctx context.Context, localpart, eventID string) (*Notification, error)
	// ListNotifications returns an account's notifications newest first, for
	// GET /notifications. Before, when positive, excludes newer rows than it: a page token is a
	// stream position, so paging is a comparison rather than an offset that shifts under a
	// concurrent write.
	ListNotifications(ctx context.Context, localpart string, before int64, limit int, onlyMentions bool) ([]Notification, error)
	// ListRecentNotifications returns an account's newest notifications for counting, capped by
	// limit, without the mentions filter.
	ListRecentNotifications(ctx context.Context, localpart string, limit int) ([]Notification, error)

	// QueuePushes adds deliveries.
	QueuePushes(ctx context.Context, rows []PendingPush) error
	// DuePushes returns deliveries whose retry time has passed, oldest debt first, along with
	// the pusher each belongs to. A delivery whose pusher has died is not returned — it is
	// nobody's debt any more.
	DuePushes(ctx context.Context, nowMS int64, limit int) ([]PendingPush, error)
	// DeletePushes removes deliveries that succeeded.
	DeletePushes(ctx context.Context, ids []int64) error
	// RetryPush records a failed attempt and when to try again.
	RetryPush(ctx context.Context, id int64, nextMS int64, reason string) error
	// DeletePushesOfPusher drops the deliveries of a push key a gateway rejected.
	DeletePushesOfPusher(ctx context.Context, localpart, appID, pushKey string) error
	// DropOldPushes removes deliveries older than the given age in attempts, returning how many
	// were given up on.
	DropOldPushes(ctx context.Context, maxAttempts int64) (int, error)
}

// PushRepoImpl is the GORM-backed [PushRepo].
type PushRepoImpl struct{ db *gorm.DB }

// NewPushRepo builds the push repository.
func NewPushRepo(db *gorm.DB) *PushRepoImpl { return &PushRepoImpl{db: db} }

// PutPusher implements [PushRepo].
func (r *PushRepoImpl) PutPusher(ctx context.Context, pusher *Pusher) error {
	row := *pusher
	if row.Data == "" {
		row.Data = "{}"
	}
	if err := session(ctx, r.db).Clauses(pusherConflict).Create(&row).Error; err != nil {
		return fmt.Errorf("registering pusher of %q: %w", pusher.Localpart, err)
	}
	return nil
}

// DeletePusher implements [PushRepo].
func (r *PushRepoImpl) DeletePusher(ctx context.Context, localpart, appID, pushKey string) (bool, error) {
	out := session(ctx, r.db).
		Where("localpart = ? AND app_id = ? AND push_key = ?", localpart, appID, pushKey).
		Delete(&Pusher{})
	if out.Error != nil {
		return false, fmt.Errorf("deleting pusher of %q: %w", localpart, out.Error)
	}
	return out.RowsAffected > 0, nil
}

// DeletePushersOfSession implements [PushRepo].
func (r *PushRepoImpl) DeletePushersOfSession(ctx context.Context, accessToken string) (int, error) {
	if accessToken == "" {
		return 0, nil
	}
	out := session(ctx, r.db).Where("access_token = ?", accessToken).Delete(&Pusher{})
	if out.Error != nil {
		return 0, fmt.Errorf("deleting pushers of a revoked session: %w", out.Error)
	}
	return int(out.RowsAffected), nil
}

// GetPusher implements [PushRepo].
func (r *PushRepoImpl) GetPusher(ctx context.Context, localpart, appID, pushKey string) (*Pusher, error) {
	var row Pusher
	err := session(ctx, r.db).
		Where("localpart = ? AND app_id = ? AND push_key = ? AND "+livePushers, localpart, appID, pushKey).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("pusher %q of %q: %w", appID, localpart, ErrNotFound)
		}
		return nil, fmt.Errorf("reading pusher of %q: %w", localpart, err)
	}
	return &row, nil
}

// ListPushers implements [PushRepo].
func (r *PushRepoImpl) ListPushers(ctx context.Context, localpart string) ([]Pusher, error) {
	var rows []Pusher
	err := session(ctx, r.db).
		Where("localpart = ? AND "+livePushers, localpart).
		Order("updated_ms ASC, app_id ASC, push_key ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("listing pushers of %q: %w", localpart, err)
	}
	return rows, nil
}

// ListAllPushers implements [PushRepo].
func (r *PushRepoImpl) ListAllPushers(ctx context.Context) ([]Pusher, error) {
	var rows []Pusher
	err := session(ctx, r.db).
		Where(livePushers).
		Order("localpart ASC, app_id ASC, push_key ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("listing pushers: %w", err)
	}
	return rows, nil
}

// CountPushers implements [PushRepo].
func (r *PushRepoImpl) CountPushers(ctx context.Context, localpart string) (int, error) {
	var count int64
	err := session(ctx, r.db).Model(&Pusher{}).
		Where("localpart = ? AND "+livePushers, localpart).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("counting pushers of %q: %w", localpart, err)
	}
	return int(count), nil
}

// PrunePushers implements [PushRepo].
func (r *PushRepoImpl) PrunePushers(ctx context.Context) (int, error) {
	out := session(ctx, r.db).Where("NOT " + livePushers).Delete(&Pusher{})
	if out.Error != nil {
		return 0, fmt.Errorf("pruning dead pushers: %w", out.Error)
	}
	return int(out.RowsAffected), nil
}

// Cursor implements [PushRepo].
func (r *PushRepoImpl) Cursor(ctx context.Context, name string) (int64, error) {
	var row PushCursor
	err := session(ctx, r.db).Where("name = ?", name).Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading push cursor %q: %w", name, err)
	}
	return row.Position, nil
}

// SetCursor implements [PushRepo].
func (r *PushRepoImpl) SetCursor(ctx context.Context, name string, position int64) error {
	row := PushCursor{Name: name, Position: position}
	err := session(ctx, r.db).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "name"}},
		UpdateAll: true,
	}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("saving push cursor %q: %w", name, err)
	}
	return nil
}

// AddNotification implements [PushRepo].
func (r *PushRepoImpl) AddNotification(ctx context.Context, notification *Notification) error {
	row := *notification
	if row.Actions == "" {
		row.Actions = "[]"
	}
	// Do nothing on conflict: the row says the event was worth a notification, and a second
	// judgement to the same effect adds nothing that needs storing.
	err := session(ctx, r.db).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("recording notification for %q: %w", notification.EventID, err)
	}
	return nil
}

// GetNotification implements [PushRepo].
func (r *PushRepoImpl) GetNotification(ctx context.Context, localpart, eventID string) (*Notification, error) {
	var row Notification
	err := session(ctx, r.db).
		Where("localpart = ? AND event_id = ?", localpart, eventID).Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("notification of %q for %q: %w", localpart, eventID, ErrNotFound)
		}
		return nil, fmt.Errorf("reading notification of %q: %w", localpart, err)
	}
	return &row, nil
}

// ListNotifications implements [PushRepo].
func (r *PushRepoImpl) ListNotifications(ctx context.Context, localpart string, before int64,
	limit int, onlyMentions bool) ([]Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	query := session(ctx, r.db).Where("localpart = ?", localpart)
	if before > 0 {
		query = query.Where("position < ?", before)
	}
	// `only=mentions` means "the notifications that used to be a badge of their own": the rule
	// that decided is recorded, so this is a filter on the stored actions rather than a
	// re-evaluation of the event.
	if onlyMentions {
		query = query.Where("actions LIKE ?", `%highlight%`)
	}
	var rows []Notification
	err := query.Order("position DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("listing notifications of %q: %w", localpart, err)
	}
	return rows, nil
}

// ListRecentNotifications implements [PushRepo].
func (r *PushRepoImpl) ListRecentNotifications(ctx context.Context, localpart string, limit int) ([]Notification, error) {
	if limit <= 0 {
		limit = 1000
	}
	var rows []Notification
	err := session(ctx, r.db).
		Where("localpart = ?", localpart).
		Order("position DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("counting notifications of %q: %w", localpart, err)
	}
	return rows, nil
}

// QueuePushes implements [PushRepo].
func (r *PushRepoImpl) QueuePushes(ctx context.Context, rows []PendingPush) error {
	if len(rows) == 0 {
		return nil
	}
	// One statement per batch rather than one per row: a message in a room with twenty of this
	// server's users is twenty deliveries, and taking the write lock twenty times for it would
	// make the notification path the most expensive part of sending a message.
	err := session(ctx, r.db).CreateInBatches(&rows, 100).Error
	if err != nil {
		return fmt.Errorf("queueing push deliveries: %w", err)
	}
	return nil
}

// DuePushes implements [PushRepo].
func (r *PushRepoImpl) DuePushes(ctx context.Context, nowMS int64, limit int) ([]PendingPush, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []PendingPush
	err := session(ctx, r.db).
		Joins("JOIN pushers ON pushers.localpart = pending_pushes.localpart AND "+
			"pushers.app_id = pending_pushes.app_id AND pushers.push_key = pending_pushes.push_key").
		// The join is what makes a delivery that owes nothing invisible: a pusher whose session
		// was revoked has no live row, and a message for a device the user signed out of is not
		// a debt.
		Where(livePushers).
		Where("pending_pushes.next_attempt_ms <= ?", nowMS).
		Order("pending_pushes.id ASC").Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading due push deliveries: %w", err)
	}
	return rows, nil
}

// DeletePushes implements [PushRepo].
func (r *PushRepoImpl) DeletePushes(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if err := session(ctx, r.db).Delete(&PendingPush{}, ids).Error; err != nil {
		return fmt.Errorf("removing completed push deliveries: %w", err)
	}
	return nil
}

// RetryPush implements [PushRepo].
func (r *PushRepoImpl) RetryPush(ctx context.Context, id int64, nextMS int64, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	out := session(ctx, r.db).Model(&PendingPush{}).Where("id = ?", id).
		Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "next_attempt_ms": nextMS,
			"last_error": reason})
	if out.Error != nil {
		return fmt.Errorf("recording a failed push delivery: %w", out.Error)
	}
	return nil
}

// DeletePushesOfPusher implements [PushRepo].
func (r *PushRepoImpl) DeletePushesOfPusher(ctx context.Context, localpart, appID, pushKey string) error {
	err := session(ctx, r.db).
		Where("localpart = ? AND app_id = ? AND push_key = ?", localpart, appID, pushKey).
		Delete(&PendingPush{}).Error
	if err != nil {
		return fmt.Errorf("dropping deliveries of a rejected push key: %w", err)
	}
	return nil
}

// DropOldPushes implements [PushRepo].
func (r *PushRepoImpl) DropOldPushes(ctx context.Context, maxAttempts int64) (int, error) {
	out := session(ctx, r.db).Where("attempts >= ?", maxAttempts).Delete(&PendingPush{})
	if out.Error != nil {
		return 0, fmt.Errorf("giving up on stale push deliveries: %w", out.Error)
	}
	return int(out.RowsAffected), nil
}
