package store

// Sliding-sync connections, and what each connection has already been shown.
//
// MSC4186's `pos` is a position in this server's one global stream, exactly like a `/sync` token,
// which is why the MSC can require the same token to work in `/messages` and `/relations`. What the
// stream cannot give a sliding sync is the other half of the connection: a memory of which rooms
// this device has already been given, how much of each it saw, and under which room config. Without
// it the endpoint degenerates into resending whole rooms on every request, which is the bandwidth
// this endpoint exists to avoid.
//
// Two tables then, one per fact:
//
//   - a connection is identified by (user, device, conn_id) and remembers the position it was last
//     handed. That record is what lets the server refuse a `pos` it never issued — the MSC's security
//     section is explicit that holding the string must not grant access, so ownership is checked
//     against these rows rather than assumed.
//   - a tracked room remembers, per connection, the position up to which it was sent and the widest
//     room config it was sent under. The first is how a delta is cut, the second is how an expansion
//     is noticed: a client that raises `timeline_limit` is owed history it has never seen, and the
//     only way to know that is to remember what was asked for last time.
//
// Both are disposable. An expired or unrecognised connection is answered with 400 M_UNKNOWN_POS,
// which the MSC defines as an instruction to start again, so nothing here can become load-bearing in
// a way that would corrupt a user's view of their rooms.

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SlidingSyncConn is one live sliding-sync connection.
type SlidingSyncConn struct {
	// Localpart, DeviceID and ConnID are the identity MSC4186 gives a connection: scoped to a
	// user *and device*, so two phones of one account have two memories.
	Localpart string `gorm:"primaryKey:slidingsync_conn_pk;size:255" json:"localpart"`
	DeviceID  string `gorm:"primaryKey:slidingsync_conn_pk;size:255" json:"device_id"`
	ConnID    string `gorm:"primaryKey:slidingsync_conn_pk;size:255" json:"conn_id"`
	// Pos is the stream position last handed to this connection. A request naming anything else
	// is not this connection continuing, and saying so is what stops a client silently resending
	// from a position the server has already moved past.
	Pos int64 `json:"pos"`
	// CreatedMS and UpdatedMS drive expiry: a connection a device stopped using is worth less
	// than the rows it costs.
	CreatedMS int64 `json:"created_ms"`
	UpdatedMS int64 `json:"updated_ms"`
}

// TableName pins the table name.
func (SlidingSyncConn) TableName() string { return "sliding_sync_connections" }

// SlidingSyncRoom is what one connection has been shown of one room.
type SlidingSyncRoom struct {
	// The connection this belongs to. Denormalised rather than keyed by a surrogate ID so that a
	// connection can be deleted, and its memory with it, in one statement.
	Localpart string `gorm:"primaryKey:slidingsync_room_pk;size:255" json:"localpart"`
	DeviceID  string `gorm:"primaryKey:slidingsync_room_pk;size:255" json:"device_id"`
	ConnID    string `gorm:"primaryKey:slidingsync_room_pk;size:255" json:"conn_id"`
	RoomID    string `gorm:"primaryKey:slidingsync_room_pk;size:512" json:"room_id"`
	// SentPos is the stream position this connection was told about this room up to. It is not
	// the connection's position: a room that slid into the window late has been *seen* by the
	// connection without this room having been read up to that point, and conflating the two is
	// how a client ends up preloading history it is then told is news.
	SentPos int64 `json:"sent_pos"`
	// TimelineLimit and StateConfig record the room config the room was last sent under, which is
	// what makes an expansion detectable: "did this client get everything the new config asks for"
	// cannot be answered from the events, only from what was asked for last time. The config is
	// stored whole rather than as a digest so the difference between the old and new state
	// selectors can be computed, and only the state the client has never been shown is resent.
	TimelineLimit int    `json:"timeline_limit"`
	StateConfig   string `gorm:"type:text" json:"state_config"`
	UpdatedMS     int64  `json:"updated_ms"`
}

// TableName pins the table name.
func (SlidingSyncRoom) TableName() string { return "sliding_sync_rooms" }

// SlidingSyncRepo is the connection bookkeeping described above.
type SlidingSyncRepo interface {
	// LoadConn returns the connection, or [ErrNotFound] when this server has no memory of it —
	// which includes the case where it remembered and expired it, and is the difference between
	// continuing a connection and starting one.
	LoadConn(ctx context.Context, localpart, deviceID, connID string) (*SlidingSyncConn, error)
	// SaveConn writes the connection's position, creating it when the request began one.
	SaveConn(ctx context.Context, conn *SlidingSyncConn) error
	// ListRooms returns everything this connection has been shown, keyed by room ID.
	ListRooms(ctx context.Context, localpart, deviceID, connID string) (map[string]SlidingSyncRoom, error)
	// SaveRooms records what this answer sent, so that the next one can be a delta.
	SaveRooms(ctx context.Context, localpart, deviceID, connID string, rooms []SlidingSyncRoom) error
	// DeleteConn drops a connection and everything it remembered.
	DeleteConn(ctx context.Context, localpart, deviceID, connID string) error
	// ForgetRoom drops one room from a connection's memory, which is what happens when a
	// connection remembers more rooms than the server is willing to hold: the room is simply sent
	// again as initial the next time the client asks about it.
	ForgetRoom(ctx context.Context, localpart, deviceID, connID, roomID string) error
	// ListConns returns a device's connections, oldest used first: the caller prunes to a
	// bounded number so that a client that reconnects with a fresh conn_id every minute cannot
	// accumulate rows forever.
	ListConns(ctx context.Context, localpart, deviceID string) ([]SlidingSyncConn, error)
	// ExpireConns deletes connections untouched since the given time and returns how many went.
	ExpireConns(ctx context.Context, olderThanMS int64) (int, error)
	// DeleteForUser drops a user's connections, which account deactivation calls: the rooms are
	// gone from their account, and a connection that outlives it would only earn a refusal.
	DeleteForUser(ctx context.Context, localpart string) error
}

// SlidingSyncRepoImpl is the GORM-backed [SlidingSyncRepo].
type SlidingSyncRepoImpl struct{ db *gorm.DB }

// NewSlidingSyncRepo builds the connection bookkeeping repository.
func NewSlidingSyncRepo(db *gorm.DB) SlidingSyncRepo { return &SlidingSyncRepoImpl{db: db} }

// LoadConn implements [SlidingSyncRepo].
func (r *SlidingSyncRepoImpl) LoadConn(ctx context.Context, localpart, deviceID, connID string) (*SlidingSyncConn, error) {
	var conn SlidingSyncConn
	err := session(ctx, r.db).
		Where("localpart = ? AND device_id = ? AND conn_id = ?", localpart, deviceID, connID).
		First(&conn).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("sliding sync connection %q: %w", connID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading sliding sync connection %q: %w", connID, err)
	}
	return &conn, nil
}

// SaveConn implements [SlidingSyncRepo].
func (r *SlidingSyncRepoImpl) SaveConn(ctx context.Context, conn *SlidingSyncConn) error {
	if conn.CreatedMS == 0 {
		conn.CreatedMS = nowMS()
	}
	conn.UpdatedMS = nowMS()
	if err := session(ctx, r.db).Clauses(connUpsert()).Create(conn).Error; err != nil {
		return fmt.Errorf("saving sliding sync connection %q: %w", conn.ConnID, err)
	}
	return nil
}

// ListRooms implements [SlidingSyncRepo].
func (r *SlidingSyncRepoImpl) ListRooms(ctx context.Context, localpart, deviceID, connID string) (map[string]SlidingSyncRoom, error) {
	var rows []SlidingSyncRoom
	err := session(ctx, r.db).
		Where("localpart = ? AND device_id = ? AND conn_id = ?", localpart, deviceID, connID).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading what a sliding sync connection had been shown: %w", err)
	}
	tracked := make(map[string]SlidingSyncRoom, len(rows))
	for _, row := range rows {
		tracked[row.RoomID] = row
	}
	return tracked, nil
}

// SaveRooms implements [SlidingSyncRepo].
//
// An upsert, not a replace: this answer describes a handful of rooms out of the many a connection
// has been shown, and the rest of that memory has to survive the request.
func (r *SlidingSyncRepoImpl) SaveRooms(ctx context.Context, localpart, deviceID, connID string, rooms []SlidingSyncRoom) error {
	if len(rooms) == 0 {
		return nil
	}
	for i := range rooms {
		rooms[i].Localpart = localpart
		rooms[i].DeviceID = deviceID
		rooms[i].ConnID = connID
		rooms[i].UpdatedMS = nowMS()
	}
	if err := session(ctx, r.db).Clauses(roomUpsert()).CreateInBatches(&rooms, 100).Error; err != nil {
		return fmt.Errorf("recording what a sliding sync answer sent: %w", err)
	}
	return nil
}

// DeleteConn implements [SlidingSyncRepo].
func (r *SlidingSyncRepoImpl) DeleteConn(ctx context.Context, localpart, deviceID, connID string) error {
	err := session(ctx, r.db).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("localpart = ? AND device_id = ? AND conn_id = ?", localpart, deviceID, connID).
			Delete(&SlidingSyncRoom{}).Error; err != nil {
			return fmt.Errorf("dropping a sliding sync connection's rooms: %w", err)
		}
		if err := tx.Where("localpart = ? AND device_id = ? AND conn_id = ?", localpart, deviceID, connID).
			Delete(&SlidingSyncConn{}).Error; err != nil {
			return fmt.Errorf("dropping a sliding sync connection: %w", err)
		}
		return nil
	})
	return err
}

// ForgetRoom implements [SlidingSyncRepo].
func (r *SlidingSyncRepoImpl) ForgetRoom(ctx context.Context, localpart, deviceID, connID, roomID string) error {
	err := session(ctx, r.db).
		Where("localpart = ? AND device_id = ? AND conn_id = ? AND room_id = ?", localpart, deviceID, connID, roomID).
		Delete(&SlidingSyncRoom{}).Error
	if err != nil {
		return fmt.Errorf("forgetting %q for a sliding sync connection: %w", roomID, err)
	}
	return nil
}

// ListConns implements [SlidingSyncRepo].
func (r *SlidingSyncRepoImpl) ListConns(ctx context.Context, localpart, deviceID string) ([]SlidingSyncConn, error) {
	var rows []SlidingSyncConn
	err := session(ctx, r.db).
		Where("localpart = ? AND device_id = ?", localpart, deviceID).
		Order("updated_ms ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading a device's sliding sync connections: %w", err)
	}
	return rows, nil
}

// ExpireConns implements [SlidingSyncRepo].
func (r *SlidingSyncRepoImpl) ExpireConns(ctx context.Context, olderThanMS int64) (int, error) {
	var stale []SlidingSyncConn
	err := session(ctx, r.db).
		Where("updated_ms < ?", olderThanMS).
		Limit(slidingSyncExpireBatch).
		Find(&stale).Error
	if err != nil {
		return 0, fmt.Errorf("looking for idle sliding sync connections: %w", err)
	}
	if len(stale) == 0 {
		return 0, nil
	}
	deleted := 0
	err = session(ctx, r.db).Transaction(func(tx *gorm.DB) error {
		for _, conn := range stale {
			if err := tx.Where("localpart = ? AND device_id = ? AND conn_id = ?",
				conn.Localpart, conn.DeviceID, conn.ConnID).Delete(&SlidingSyncRoom{}).Error; err != nil {
				return fmt.Errorf("dropping the rooms of an expired sliding sync connection: %w", err)
			}
			result := tx.Where("localpart = ? AND device_id = ? AND conn_id = ?",
				conn.Localpart, conn.DeviceID, conn.ConnID).Delete(&SlidingSyncConn{})
			if result.Error != nil {
				return fmt.Errorf("dropping an expired sliding sync connection: %w", result.Error)
			}
			deleted += int(result.RowsAffected)
		}
		return nil
	})
	return deleted, err
}

// DeleteForUser implements [SlidingSyncRepo].
func (r *SlidingSyncRepoImpl) DeleteForUser(ctx context.Context, localpart string) error {
	err := session(ctx, r.db).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("localpart = ?", localpart).Delete(&SlidingSyncRoom{}).Error; err != nil {
			return fmt.Errorf("dropping a user's sliding sync rooms: %w", err)
		}
		if err := tx.Where("localpart = ?", localpart).Delete(&SlidingSyncConn{}).Error; err != nil {
			return fmt.Errorf("dropping a user's sliding sync connections: %w", err)
		}
		return nil
	})
	return err
}

// slidingSyncExpireBatch bounds one sweep of the expiry pass: idle connections are few and
// deleting them is not urgent, so a sweep should not become the longest transaction on the server.
const slidingSyncExpireBatch = 200

// connKey and roomKey name the rows an upsert replaces. They are spelled out rather than inferred
// from the primary-key tags because the two tables have different keys, and an upsert that used the
// wrong one would quietly overwrite a connection's row with a room's.
var (
	connKey = []clause.Column{{Name: "localpart"}, {Name: "device_id"}, {Name: "conn_id"}}
	roomKey = []clause.Column{{Name: "localpart"}, {Name: "device_id"}, {Name: "conn_id"}, {Name: "room_id"}}
)

// connUpsert updates only the position and the clock: a connection that comes back keeps the moment
// it was created, which is the only date on a connection that means anything to a maintainer asking
// how long this device has been reconnecting.
func connUpsert() clause.OnConflict {
	return clause.OnConflict{Columns: connKey, DoUpdates: clause.AssignmentColumns([]string{"pos", "updated_ms"})}
}

// roomUpsert replaces the whole memory of a room, because "what this connection has been shown" is
// exactly what just changed.
func roomUpsert() clause.OnConflict {
	return clause.OnConflict{Columns: roomKey,
		DoUpdates: clause.AssignmentColumns([]string{"sent_pos", "timeline_limit", "state_config", "updated_ms"})}
}
