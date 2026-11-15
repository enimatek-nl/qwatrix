package store

// Server-side room key backup, in two tables.
//
// A backup is a container with a server-assigned version number. A user has at most one *current*
// backup — the highest version — and every write goes there, while older backups stay readable so a
// client that is still holding the old version can fetch what it already uploaded instead of losing
// it. That asymmetry is the whole design: reads are allowed backwards, writes are not, because the
// thing being protected is a person's ability to decrypt their own history after losing every device
// that could.
//
// Version numbers are integers assigned in order, one per account, and returned to clients as
// strings. They are not client-chosen: two devices setting up a backup for the same account at the
// same moment would otherwise produce two backups with different keys and no way to tell which one
// holds the keys, and the version number is the only thing a client uses to decide where its keys
// live.
//
// Deleting a version deletes its keys and its row outright. A version number can therefore be handed
// out again after a deletion, which is safe here and worth saying why: the keys that went with the
// old number are gone too, and a client can only ever write to the current version, so a stale
// client writes into the backup that is current rather than into a grave.
//
// What the rows hold. `session_data` and the other per-session fields are an opaque payload as far
// as this server is concerned — the encryption is between the client and its own recovery key, and
// the server's job is to store a blob without corrupting it and without reading it. So each session
// row keeps the whole submitted object rather than a projection of the three fields this server
// understands, and returns it unchanged: a client that adds a field this server has never heard of
// gets it back, which is the difference between a server that is agnostic to the backup format and a
// server that silently discards parts of it.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// KeyBackupVersion is one backup container: the metadata that says how its keys are encrypted, and
// an etag for the keys themselves.
type KeyBackupVersion struct {
	// Localpart is whose backup this is. Backups are per account, not per device: the point of
	// one is that a different device can restore the keys.
	Localpart string `gorm:"size:255;primaryKey:keybackup_pk" json:"-"`
	// Version is the server-assigned number. It is an integer here and a string on the wire,
	// which is the specification's choice, not this one.
	Version int64 `gorm:"primaryKey:keybackup_pk" json:"version"`
	// Algorithm names how the session payloads are encrypted. This server does not implement any
	// of them and does not need to: it stores the payload and hands it back.
	Algorithm string `gorm:"size:255;column:algorithm" json:"algorithm"`
	// AuthData is the client's own object — a public key and signatures, for the algorithms in
	// use — kept as submitted for the same reason the session payloads are.
	AuthData string `gorm:"type:text" json:"-"`
	// ETag counts changes to the keys in this backup. It is what `If-Match` is compared against,
	// so a client that read the backup, then found somebody else had written to it, can be told
	// rather than overwrite them.
	ETag int64 `gorm:"column:etag" json:"etag"`
	// UpdatedMS is when the keys last changed, in milliseconds since the epoch; the wire calls it
	// `last_update` and puts it in seconds.
	UpdatedMS int64 `json:"-"`
}

// TableName pins the table name.
func (KeyBackupVersion) TableName() string { return "key_backup_versions" }

// VersionString renders the version the way the wire does: an integer, in a string.
func (v KeyBackupVersion) VersionString() string { return strconv.FormatInt(v.Version, 10) }

// KeyBackupKey is one backed-up Megolm session.
type KeyBackupKey struct {
	Localpart string `gorm:"size:255;primaryKey:keybackupkey_pk" json:"-"`
	// Version is the backup this session belongs to, not a revision of the session: sessions are
	// copied forward when a backup is recreated, and each backup holds its own.
	Version   int64  `gorm:"primaryKey:keybackupkey_pk" json:"-"`
	RoomID    string `gorm:"size:512;primaryKey:keybackupkey_pk" json:"-"`
	SessionID string `gorm:"size:255;primaryKey:keybackupkey_pk" json:"-"`
	// FirstMessageIndex, ForwardedCount and IsVerified are the only fields this server reads, and
	// it reads them only to decide which of two copies of a session to keep. They are what the
	// client claims, not what this server verifies — the payload is encrypted to the user's own
	// key, so nobody here could check them.
	FirstMessageIndex int64 `json:"first_message_index"`
	ForwardedCount    int64 `json:"forwarded_count"`
	IsVerified        bool  `json:"is_verified"`
	// Payload is the whole session object as submitted, including the opaque session_data and any
	// fields this server does not know.
	Payload string `gorm:"type:text" json:"-"`
	// UpdatedMS is when this row was written.
	UpdatedMS int64 `json:"-"`
}

// TableName pins the table name.
func (KeyBackupKey) TableName() string { return "key_backup_keys" }

// KeyBackupRepo is the storage surface for room key backups.
type KeyBackupRepo interface {
	// CreateKeyBackupVersion assigns the next version number and stores the row, returning it.
	CreateKeyBackupVersion(ctx context.Context, version *KeyBackupVersion) (*KeyBackupVersion, error)
	// CurrentKeyBackupVersion is the highest version, which is the one writes go to.
	CurrentKeyBackupVersion(ctx context.Context, localpart string) (*KeyBackupVersion, error)
	// GetKeyBackupVersion reads one version by its wire (string) number.
	GetKeyBackupVersion(ctx context.Context, localpart, version string) (*KeyBackupVersion, error)
	// UpdateKeyBackupVersion replaces a version's auth data, and its etag when one is given.
	UpdateKeyBackupVersion(ctx context.Context, localpart, version, authData string, etag int64) (*KeyBackupVersion, error)
	// DeleteKeyBackupVersion removes a version and every key inside it.
	DeleteKeyBackupVersion(ctx context.Context, localpart, version string) error
	// ListKeyBackupKeys reads sessions, optionally narrowed to one room and one session.
	ListKeyBackupKeys(ctx context.Context, localpart, version, roomID, sessionID string) ([]KeyBackupKey, error)
	// CountKeyBackupKeys is the `count` a client uses to tell an empty backup from a missing one.
	CountKeyBackupKeys(ctx context.Context, localpart, version string) (int64, error)
	// PutKeyBackupKeys merges sessions into a backup under the replacement rule, in one write,
	// and reports how many were taken and the new etag. When requireMatch is set, the write only
	// happens if the backup still carries the etag the caller read, and [ErrETagMismatch] says it
	// did not.
	PutKeyBackupKeys(ctx context.Context, keys []KeyBackupKey, etag int64, requireMatch bool) (accepted int, newETag int64, err error)
	// DeleteKeyBackupKeys removes sessions, narrowed the same way the reads are, with the same
	// optional etag guard.
	DeleteKeyBackupKeys(ctx context.Context, localpart, version, roomID, sessionID string, etag int64, requireMatch bool) (int64, error)
	// DeleteKeyBackupForUser is the account-deletion path: an encrypted blob belonging to a
	// person who has gone is not theirs to keep.
	DeleteKeyBackupForUser(ctx context.Context, localpart string) error
}

// KeyBackupRepoImpl is the GORM-backed implementation.
type KeyBackupRepoImpl struct{ db *gorm.DB }

// NewKeyBackupRepo builds the repository over an open store.
func NewKeyBackupRepo(db *gorm.DB) *KeyBackupRepoImpl { return &KeyBackupRepoImpl{db: db} }

func (r *KeyBackupRepoImpl) CreateKeyBackupVersion(ctx context.Context, version *KeyBackupVersion) (*KeyBackupVersion, error) {
	// The number is assigned inside the write because two devices setting up a backup at the same
	// moment must not both be told they are version 1: they would each encrypt their keys to a
	// different recovery key under the same name, and one of them would find its backup empty when
	// it most needed it.
	created := version
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var top int64
		if err := tx.Model(&KeyBackupVersion{}).
			Where("localpart = ?", version.Localpart).
			Select("COALESCE(MAX(version), 0)").Scan(&top).Error; err != nil {
			return fmt.Errorf("reading the highest backup version: %w", err)
		}
		created.Version = top + 1
		created.ETag = 0
		if err := tx.Create(created).Error; err != nil {
			return fmt.Errorf("creating a key backup version: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (r *KeyBackupRepoImpl) CurrentKeyBackupVersion(ctx context.Context, localpart string) (*KeyBackupVersion, error) {
	var row KeyBackupVersion
	err := r.db.WithContext(ctx).
		Where("localpart = ?", localpart).
		Order("version DESC").
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading the current key backup: %w", err)
	}
	return &row, nil
}

func (r *KeyBackupRepoImpl) GetKeyBackupVersion(ctx context.Context, localpart, version string) (*KeyBackupVersion, error) {
	number, ok := parseBackupVersion(version)
	if !ok {
		// A version this server never issued. Its numbers are integers, so anything else is
		// simply not one of this account's backups — reported as absent rather than invalid,
		// because the client's next move is the same either way: fetch the current version.
		return nil, ErrNotFound
	}
	var row KeyBackupVersion
	err := r.db.WithContext(ctx).
		Where("localpart = ? AND version = ?", localpart, number).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading key backup version %s: %w", version, err)
	}
	return &row, nil
}

func (r *KeyBackupRepoImpl) UpdateKeyBackupVersion(ctx context.Context, localpart, version, authData string, etag int64) (*KeyBackupVersion, error) {
	number, ok := parseBackupVersion(version)
	if !ok {
		return nil, ErrNotFound
	}
	current, err := r.GetKeyBackupVersion(ctx, localpart, version)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{"etag": etag}
	if authData != "" {
		fields["auth_data"] = authData
	}
	if err := r.db.WithContext(ctx).Model(&KeyBackupVersion{}).
		Where("localpart = ? AND version = ?", localpart, number).
		Updates(fields).Error; err != nil {
		return nil, fmt.Errorf("updating key backup version %s: %w", version, err)
	}
	if authData != "" {
		current.AuthData = authData
	}
	current.ETag = etag
	return current, nil
}

func (r *KeyBackupRepoImpl) DeleteKeyBackupVersion(ctx context.Context, localpart, version string) error {
	number, ok := parseBackupVersion(version)
	if !ok {
		return ErrNotFound
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The container is looked up before anything is removed, so that deleting a backup that
		// is not there is a refusal rather than a success. Answering 200 twice to two clients
		// that both decided to delete would be honest about the second one's outcome — the
		// backup is indeed gone — and wrong about its premise: it asked to delete a backup it
		// believed it had, which is the fact worth telling it was mistaken about.
		var current KeyBackupVersion
		if err := tx.Where("localpart = ? AND version = ?", localpart, number).
			First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("reading the key backup to delete: %w", err)
		}
		if err := tx.Where("localpart = ? AND version = ?", localpart, number).
			Delete(&KeyBackupKey{}).Error; err != nil {
			return fmt.Errorf("deleting the keys in key backup %s: %w", version, err)
		}
		// The container goes in the same transaction as its contents, because a backup with no
		// container would be an invisible pile of encrypted blobs that nobody can address,
		// delete or decrypt.
		if err := tx.Where("localpart = ? AND version = ?", localpart, number).
			Delete(&KeyBackupVersion{}).Error; err != nil {
			return fmt.Errorf("deleting key backup version %s: %w", version, err)
		}
		return nil
	})
}

func (r *KeyBackupRepoImpl) ListKeyBackupKeys(ctx context.Context, localpart, version, roomID, sessionID string) ([]KeyBackupKey, error) {
	number, ok := parseBackupVersion(version)
	if !ok {
		return nil, nil
	}
	query := r.db.WithContext(ctx).
		Where("localpart = ? AND version = ?", localpart, number)
	if roomID != "" {
		query = query.Where("room_id = ?", roomID)
		if sessionID != "" {
			query = query.Where("session_id = ?", sessionID)
		}
	}
	var rows []KeyBackupKey
	if err := query.Order("room_id ASC, session_id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading the keys in key backup %s: %w", version, err)
	}
	return rows, nil
}

func (r *KeyBackupRepoImpl) CountKeyBackupKeys(ctx context.Context, localpart, version string) (int64, error) {
	number, ok := parseBackupVersion(version)
	if !ok {
		return 0, nil
	}
	var count int64
	if err := r.db.WithContext(ctx).Model(&KeyBackupKey{}).
		Where("localpart = ? AND version = ?", localpart, number).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("counting the keys in key backup %s: %w", version, err)
	}
	return count, nil
}

func (r *KeyBackupRepoImpl) PutKeyBackupKeys(ctx context.Context, keys []KeyBackupKey,
	etag int64, requireMatch bool) (int, int64, error) {
	if len(keys) == 0 {
		return 0, 0, nil
	}
	// Every session of one upload lands in one transaction, which on this server is not an
	// optimisation but the requirement: one writer at a time, and a client uploading its first
	// backup submits hundreds of sessions at once. A loop of single-row transactions would charge
	// the whole server for each of them.
	var (
		accepted int
		newETag  int64
	)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		localpart := keys[0].Localpart
		version := keys[0].Version
		var current KeyBackupVersion
		if err := tx.Where("localpart = ? AND version = ?", localpart, version).
			First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("reading the backup being written to: %w", err)
		}

		if requireMatch && current.ETag != etag {
			return ErrETagMismatch
		}

		existing := map[string]KeyBackupKey{}
		{
			var rows []KeyBackupKey
			query := tx.Where("localpart = ? AND version = ?", localpart, version)
			var pairs []string
			args := make([]any, 0, 2*len(keys))
			for _, key := range keys {
				pairs = append(pairs, "(room_id = ? AND session_id = ?)")
				args = append(args, key.RoomID, key.SessionID)
			}
			if err := query.Where("("+strings.Join(pairs, " OR ")+")", args...).
				Find(&rows).Error; err != nil {
				return fmt.Errorf("reading the sessions this upload touches: %w", err)
			}
			for _, row := range rows {
				existing[sessionKey(row.RoomID, row.SessionID)] = row
			}
		}

		written := make([]KeyBackupKey, 0, len(keys))
		for _, key := range keys {
			if before, found := existing[sessionKey(key.RoomID, key.SessionID)]; found &&
				!ShouldReplaceKey(before, key) {
				// Somebody already stored a better copy of this session. Dropping this one is
				// the correct outcome, not a failure — and it is why the response counts what
				// was *stored* rather than echoing what was submitted.
				continue
			}
			written = append(written, key)
		}
		accepted = len(written)
		if len(written) == 0 {
			// Nothing was taken, so nothing changed, so the etag stays where it is. Bumping it
			// would make every other client re-download a backup that is byte-identical.
			newETag = current.ETag
			return nil
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "localpart"}, {Name: "version"}, {Name: "room_id"}, {Name: "session_id"}},
			UpdateAll: true,
		}).CreateInBatches(&written, 100).Error; err != nil {
			return fmt.Errorf("storing %d backed-up sessions: %w", len(written), err)
		}
		newETag = current.ETag + 1
		if err := tx.Model(&KeyBackupVersion{}).
			Where("localpart = ? AND version = ?", localpart, version).
			Updates(map[string]any{"etag": newETag, "updated_ms": written[0].UpdatedMS}).Error; err != nil {
			return fmt.Errorf("updating the backup's etag: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return accepted, newETag, nil
}

func (r *KeyBackupRepoImpl) DeleteKeyBackupKeys(ctx context.Context, localpart, version, roomID,
	sessionID string, etag int64, requireMatch bool) (int64, error) {
	number, ok := parseBackupVersion(version)
	if !ok {
		return 0, ErrNotFound
	}
	var removed int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Where("localpart = ? AND version = ?", localpart, number)
		if roomID != "" {
			query = query.Where("room_id = ?", roomID)
			if sessionID != "" {
				query = query.Where("session_id = ?", sessionID)
			}
		}
		result := query.Delete(&KeyBackupKey{})
		if result.Error != nil {
			return fmt.Errorf("deleting backed-up sessions: %w", result.Error)
		}
		removed = result.RowsAffected
		var current KeyBackupVersion
		if err := tx.Where("localpart = ? AND version = ?", localpart, number).
			First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("reading the backup being deleted from: %w", err)
		}
		if requireMatch && current.ETag != etag {
			return ErrETagMismatch
		}
		// The etag moves even when nothing was deleted, only conservatively: a client holding an
		// old etag is told the backup changed, and re-reads it, which is merely wasteful. The
		// alternative — leaving the etag alone so the client's write proceeds — is how two
		// clients end up overwriting each other.
		return tx.Model(&KeyBackupVersion{}).
			Where("localpart = ? AND version = ?", localpart, number).
			Updates(map[string]any{"etag": current.ETag + 1}).Error
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

func (r *KeyBackupRepoImpl) DeleteKeyBackupForUser(ctx context.Context, localpart string) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("localpart = ?", localpart).Delete(&KeyBackupKey{}).Error; err != nil {
			return fmt.Errorf("deleting a departed user's backed-up sessions: %w", err)
		}
		if err := tx.Where("localpart = ?", localpart).Delete(&KeyBackupVersion{}).Error; err != nil {
			return fmt.Errorf("deleting a departed user's key backups: %w", err)
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return err
}

// ErrETagMismatch reports that a write was guarded by an etag the backup no longer carries: somebody
// else changed it after the caller looked.
var ErrETagMismatch = errors.New("the backup changed since it was read")

// ShouldReplaceKey decides which of two copies of a session a backup keeps.
//
// The rule is the one the ecosystem's servers use, and it is a set of independent wins rather than
// a cascade of comparisons: a verified copy beats an unverified one, a copy claiming to reach
// further back into the room's history wins, and a copy that claims to have been forwarded fewer
// times wins. Two clients of one account upload the same session within seconds of each other, with
// whatever they each happened to know at the time, and every one of those claims is made by a client
// that cannot see the others'.
//
// So the rule has to be a rule about which claim is worth keeping rather than about which upload is
// newer — and it is deliberately not a security check. These are the owner's own claims about their
// own keys; the server cannot verify any of them, since the payload is encrypted to the owner.
func ShouldReplaceKey(current, incoming KeyBackupKey) bool {
	if incoming.IsVerified && !current.IsVerified {
		return true
	}
	if incoming.FirstMessageIndex < current.FirstMessageIndex {
		return true
	}
	return incoming.ForwardedCount < current.ForwardedCount
}

// parseBackupVersion reads a wire version number. Backups are addressed by the integers this server
// handed out, so anything else is not one of them — and it is reported as absent rather than
// malformed, because a client holding a version this server never issued takes the same next step
// either way: ask for the current version and start over.
func parseBackupVersion(version string) (int64, bool) {
	number, err := strconv.ParseInt(version, 10, 64)
	if err != nil || number <= 0 {
		// ParseInt is exact, which is the point: "12abc" must not name version 12. A client that
		// mangled a version number should be told the backup does not exist, not be quietly
		// pointed at a different backup belonging to the same person.
		return 0, false
	}
	return number, true
}

// sessionKey is the identity of a backed-up session inside one backup.
func sessionKey(roomID, sessionID string) string {
	return roomID + "\x00" + sessionID
}
