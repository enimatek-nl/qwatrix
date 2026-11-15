// Package roomkeys is server-side room key backup: the encrypted Megolm sessions a user keeps on
// this server so that a new device can read conversations it did not take part in making.
//
// The server's role here is deliberately small, and it is worth being precise about what it is not.
// It does not hold any key that opens these sessions — the payload is encrypted to a recovery key
// this server never sees, which is what makes the backup worth having rather than a copy of the
// conversation on the operator's disk. It cannot tell a genuine session from nonsense, cannot help a
// user who has lost their recovery key, and cannot be compelled to read a conversation it is
// holding. What it does hold is the ability to say "you have not backed up yet", which is the
// difference between a person losing a phone and losing their history.
//
// Four decisions shape the code:
//
//   - A backup is addressed by a version number this server assigns. Clients propose keys, not names.
//     Two devices setting up a backup at the same moment must end up with one backup between them,
//     or each encrypts its keys to a different recovery key under a name only it knows.
//   - Reads look backwards through old versions, writes do not. A client still holding the previous
//     backup can fetch what it uploaded; it cannot add to it, because a session written into a backup
//     whose recovery key it no longer matches is a session nobody can recover.
//   - Session payloads are stored and returned as submitted. A client that adds a field this server
//     has never heard of gets it back; a server that projected its own idea of the format onto the
//     backup would corrupt the one copy of a person's history that survives their laptop.
//   - A password change does not touch a backup. A pusher is a session's permission to be told
//     things, so it dies with the session; a backup exists precisely for the case where the person
//     has lost the sessions, and destroying it at the moment they change their password would remove
//     the thing they most need. The asymmetry is intentional and documented on both sides.
package roomkeys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Service answers the room-key-backup endpoints.
type Service struct {
	store *store.Store
}

// New builds the service over an open store.
func New(st *store.Store) *Service { return &Service{store: st} }

// VersionInfo is a backup as a client sees it.
type VersionInfo struct {
	// Version is the server-assigned number, as a string, because that is what the wire says.
	Version string `json:"version"`
	// Algorithm names how the sessions inside are encrypted. This server stores the string and
	// never acts on it: the formats are negotiated between a client and its own recovery key.
	Algorithm string `json:"algorithm"`
	// AuthData is the client's own object — a public key and its signatures, for the formats in
	// use — returned exactly as it was written.
	AuthData json.RawMessage `json:"auth_data"`
	// Count is how many sessions are stored in this backup. It is what lets a client tell "you
	// backed up nothing" from "there is no backup", which the two look identical without it.
	Count int64 `json:"count"`
	// ETag changes when the sessions change. A client that reads, then writes, can be told it is
	// about to overwrite another of its own devices rather than doing it silently.
	ETag string `json:"etag"`
	// LastUpdateMS is when the backup last changed, in milliseconds since the epoch.
	LastUpdateMS int64 `json:"last_update,omitempty"`
}

// VersionRequest is the body of the two endpoints that create or update a backup.
type VersionRequest struct {
	Algorithm string          `json:"algorithm"`
	AuthData  json.RawMessage `json:"auth_data"`
	// Version appears in an update's body, where it must name the version in the path. It is a
	// redundancy the specification carries, and honouring it catches a client that has mixed up
	// which of its own backups it is editing.
	Version string `json:"version,omitempty"`
}

// WriteResult is what a write to a backup answers: how much is now in it, and the new etag.
type WriteResult struct {
	Count int64  `json:"count"`
	ETag  string `json:"etag"`
}

// CurrentVersion answers GET /room_keys/version.
func (s *Service) CurrentVersion(ctx context.Context, localpart string) (*VersionInfo, error) {
	row, err := s.store.KeyBackup.CurrentKeyBackupVersion(ctx, localpart)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Not an error worth alarming a client over: "no backup yet" is the state every
			// account starts in, and it is what makes a client offer to create one.
			return nil, matrix.NewNotFound("No backup found")
		}
		return nil, err
	}
	return s.versionInfo(ctx, row)
}

// Version answers GET /room_keys/version/{version}.
func (s *Service) Version(ctx context.Context, localpart, version string) (*VersionInfo, error) {
	row, err := s.store.KeyBackup.GetKeyBackupVersion(ctx, localpart, version)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, matrix.NewNotFound("No backup found")
		}
		return nil, err
	}
	return s.versionInfo(ctx, row)
}

// CreateVersion answers POST /room_keys/version, returning the number the server handed out.
func (s *Service) CreateVersion(ctx context.Context, localpart string, request VersionRequest) (string, error) {
	algorithm := strings.TrimSpace(request.Algorithm)
	if algorithm == "" {
		return "", matrix.NewMissingParam("algorithm")
	}
	if len(request.AuthData) == 0 || string(request.AuthData) == "null" {
		return "", matrix.NewMissingParam("auth_data")
	}
	if !json.Valid(request.AuthData) {
		return "", matrix.NewBadJSON("auth_data is not valid JSON")
	}
	// The algorithm is deliberately not checked against a list. This server implements none of
	// them — it stores the payload and hands it back — so a list here would be a guess about the
	// future of a format it has no stake in, and refusing a client would be the cost of guessing
	// wrong. What is checked is that the client named what it did, since a backup with no algorithm
	// is a backup whose sessions cannot be decrypted by anybody, including its owner.
	row, err := s.store.KeyBackup.CreateKeyBackupVersion(ctx, &store.KeyBackupVersion{
		Localpart: localpart,
		Algorithm: algorithm,
		AuthData:  string(request.AuthData),
		UpdatedMS: nowMS(),
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", row.Version), nil
}

// UpdateVersion answers PUT /room_keys/version/{version}.
func (s *Service) UpdateVersion(ctx context.Context, localpart, version string, request VersionRequest) error {
	if request.Version != "" && request.Version != version {
		return matrix.NewInvalidParam("the version in the body (%s) does not match the version in the path (%s)",
			request.Version, version)
	}
	if len(request.AuthData) == 0 || string(request.AuthData) == "null" {
		return matrix.NewMissingParam("auth_data")
	}
	if !json.Valid(request.AuthData) {
		return matrix.NewBadJSON("auth_data is not valid JSON")
	}
	current, err := s.store.KeyBackup.GetKeyBackupVersion(ctx, localpart, version)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return matrix.NewNotFound("No backup found")
		}
		return err
	}
	if request.Algorithm != "" && request.Algorithm != current.Algorithm {
		// The algorithm belongs to the backup, not to the request: the sessions already inside
		// were encrypted under it, and a client that could change it would be able to leave a
		// backup that claims to hold something it cannot possibly decrypt.
		return matrix.NewInvalidParam("the algorithm of a key backup cannot be changed")
	}
	_, err = s.store.KeyBackup.UpdateKeyBackupVersion(ctx, localpart, version,
		string(request.AuthData), current.ETag)
	return err
}

// DeleteVersion answers DELETE /room_keys/version/{version}: the backup and everything in it.
func (s *Service) DeleteVersion(ctx context.Context, localpart, version string) error {
	err := s.store.KeyBackup.DeleteKeyBackupVersion(ctx, localpart, version)
	if errors.Is(err, store.ErrNotFound) {
		return matrix.NewNotFound("No backup found")
	}
	return err
}

// PutKeys answers the three PUT /room_keys/keys endpoints, which differ only in how much of the
// address is in the path.
func (s *Service) PutKeys(ctx context.Context, localpart, version string,
	roomID, sessionID string, body json.RawMessage, ifMatch string) (*WriteResult, error) {
	if version == "" {
		return nil, matrix.NewMissingParam("version")
	}
	current, err := s.store.KeyBackup.CurrentKeyBackupVersion(ctx, localpart)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, matrix.NewNotFound("No backup found")
		}
		return nil, err
	}
	if version != current.VersionString() {
		return nil, s.wrongVersion(ctx, localpart, version, current)
	}
	expected, requireMatch, err := parseETag(ifMatch)
	if err != nil {
		return nil, err
	}

	sessions, err := sessionsFromBody(roomID, sessionID, body)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, matrix.NewMissingParam("sessions")
	}
	rows := make([]store.KeyBackupKey, 0, len(sessions))
	for _, session := range sessions {
		rows = append(rows, session.row(localpart, current.Version))
	}
	_, etag, err := s.store.KeyBackup.PutKeyBackupKeys(ctx, rows, expected, requireMatch)
	if err != nil {
		if errors.Is(err, store.ErrETagMismatch) {
			return nil, s.wrongVersion(ctx, localpart, version, current)
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, matrix.NewNotFound("No backup found")
		}
		return nil, err
	}
	return s.writeResult(ctx, localpart, version, etag)
}

// GetKeys answers the three GET /room_keys/keys endpoints.
//
// A room with nothing in it answers empty rather than 404, and a session that is not there answers
// 404. That asymmetry is the client's: a client restoring a backup walks rooms it knows about and has
// no way to know which of them were backed up, while a client asked for one session needs to know
// that this particular one is missing.
func (s *Service) GetKeys(ctx context.Context, localpart, version, roomID, sessionID string) (any, error) {
	if version == "" {
		return nil, matrix.NewMissingParam("version")
	}
	if _, err := s.store.KeyBackup.GetKeyBackupVersion(ctx, localpart, version); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, matrix.NewNotFound("Unknown backup version")
		}
		return nil, err
	}
	rows, err := s.store.KeyBackup.ListKeyBackupKeys(ctx, localpart, version, roomID, sessionID)
	if err != nil {
		return nil, err
	}
	if sessionID != "" {
		if len(rows) == 0 {
			return nil, matrix.NewNotFound("No room_keys found")
		}
		return decodePayload(rows[0].Payload)
	}
	if roomID != "" {
		sessions := map[string]json.RawMessage{}
		for _, row := range rows {
			sessions[row.SessionID] = json.RawMessage(row.Payload)
		}
		return map[string]any{"sessions": sessions}, nil
	}
	rooms := map[string]map[string]any{}
	for _, row := range rows {
		room, found := rooms[row.RoomID]
		if !found {
			room = map[string]any{"sessions": map[string]json.RawMessage{}}
			rooms[row.RoomID] = room
		}
		room["sessions"].(map[string]json.RawMessage)[row.SessionID] = json.RawMessage(row.Payload)
	}
	return map[string]any{"rooms": rooms}, nil
}

// DeleteKeys answers the three DELETE /room_keys/keys endpoints.
func (s *Service) DeleteKeys(ctx context.Context, localpart, version, roomID, sessionID string,
	ifMatch string) (*WriteResult, error) {
	if version == "" {
		return nil, matrix.NewMissingParam("version")
	}
	current, err := s.store.KeyBackup.GetKeyBackupVersion(ctx, localpart, version)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, matrix.NewNotFound("Unknown backup version")
		}
		return nil, err
	}
	expected, requireMatch, err := parseETag(ifMatch)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.KeyBackup.DeleteKeyBackupKeys(ctx, localpart, version, roomID, sessionID,
		expected, requireMatch); err != nil {
		if errors.Is(err, store.ErrETagMismatch) {
			return nil, s.wrongVersion(ctx, localpart, version, current)
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, matrix.NewNotFound("Unknown backup version")
		}
		return nil, err
	}
	count, err := s.store.KeyBackup.CountKeyBackupKeys(ctx, localpart, version)
	if err != nil {
		return nil, err
	}
	updated, err := s.store.KeyBackup.GetKeyBackupVersion(ctx, localpart, version)
	if err != nil {
		return nil, err
	}
	return &WriteResult{Count: count, ETag: fmt.Sprintf("%d", updated.ETag)}, nil
}

// DeleteForUser removes a departed account's backups. An encrypted blob belonging to a person who
// has asked not to be a user of this server any longer is not ours to keep, whatever it is addressed
// with.
func (s *Service) DeleteForUser(ctx context.Context, localpart string) error {
	return s.store.KeyBackup.DeleteKeyBackupForUser(ctx, localpart)
}

// wrongVersion explains a write to a backup that is not the current one, and names the current one
// so the client can switch to it instead of retrying into the same refusal forever.
func (s *Service) wrongVersion(ctx context.Context, localpart, version string,
	current *store.KeyBackupVersion) error {
	if _, err := s.store.KeyBackup.GetKeyBackupVersion(ctx, localpart, version); err != nil {
		// The version asked for does not exist at all, so there is nothing to switch to and no
		// reason to talk about versions: the resource is simply not there.
		return matrix.NewNotFound("Version %q not found", version)
	}
	return matrix.NewWrongVersion(current.VersionString())
}

// writeResult reports the state of a backup after a write.
func (s *Service) writeResult(ctx context.Context, localpart, version string, etag int64) (*WriteResult, error) {
	count, err := s.store.KeyBackup.CountKeyBackupKeys(ctx, localpart, version)
	if err != nil {
		return nil, err
	}
	return &WriteResult{Count: count, ETag: fmt.Sprintf("%d", etag)}, nil
}

// versionInfo reads the one piece of information a version's row does not carry: how full it is.
func (s *Service) versionInfo(ctx context.Context, row *store.KeyBackupVersion) (*VersionInfo, error) {
	count, err := s.store.KeyBackup.CountKeyBackupKeys(ctx, row.Localpart, row.VersionString())
	if err != nil {
		return nil, err
	}
	authData := json.RawMessage(row.AuthData)
	if len(authData) == 0 {
		authData = json.RawMessage("{}")
	}
	return &VersionInfo{
		Version:      row.VersionString(),
		Algorithm:    row.Algorithm,
		AuthData:     authData,
		Count:        count,
		ETag:         fmt.Sprintf("%d", row.ETag),
		LastUpdateMS: row.UpdatedMS,
	}, nil
}

// session is one submitted Megolm session, kept as both its comparable claims and the object they
// came in.
type session struct {
	roomID    string
	sessionID string
	// payload is the submitted object verbatim. The three fields below are read out of it for the
	// replacement rule and are never re-serialised into the response, so that a field this server
	// does not model still survives the round trip.
	payload json.RawMessage
	// row's fields are the claims, defaulted: an absent count is zero and an absent "verified" is
	// not verified, which is the conservative direction for both.
	firstMessageIndex int64
	forwardedCount    int64
	isVerified        bool
}

// row turns the session into the row to store.
func (s session) row(localpart string, version int64) store.KeyBackupKey {
	return store.KeyBackupKey{
		Localpart:         localpart,
		Version:           version,
		RoomID:            s.roomID,
		SessionID:         s.sessionID,
		FirstMessageIndex: s.firstMessageIndex,
		ForwardedCount:    s.forwardedCount,
		IsVerified:        s.isVerified,
		Payload:           string(s.payload),
		UpdatedMS:         nowMS(),
	}
}

// sessionsFromBody reads any of the three submission shapes into a flat list.
//
// The three endpoints are the same operation at three different addresses, and the bodies nest to
// match: an address that names a session carries the session, one that names a room carries a map of
// them, one that names nothing carries a map of rooms. Sharing one reader for all three is what keeps
// the validation in one place, so a session cannot slip past a check by being uploaded through the
// bulk endpoint.
func sessionsFromBody(roomID, sessionID string, body json.RawMessage) ([]session, error) {
	if len(body) == 0 {
		return nil, matrix.NewNotJSON("a key backup upload needs a body")
	}
	switch {
	case sessionID != "":
		// The address names the session, so the body is the session — and the address wins over
		// anything the body says its own ID is, because the caller has no way to name a session
		// twice with two different answers and mean the second one.
		one, err := decodeSession(roomID, sessionID, body)
		if err != nil {
			return nil, err
		}
		return []session{one}, nil
	case roomID != "":
		var wrapper struct {
			Sessions map[string]json.RawMessage `json:"sessions"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil {
			return nil, matrix.DecodeFailure("sessions", err)
		}
		out := make([]session, 0, len(wrapper.Sessions))
		for id, raw := range wrapper.Sessions {
			one, err := decodeSession(roomID, id, raw)
			if err != nil {
				return nil, err
			}
			out = append(out, one)
		}
		return out, nil
	default:
		var wrapper struct {
			Rooms map[string]struct {
				Sessions map[string]json.RawMessage `json:"sessions"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil {
			return nil, matrix.DecodeFailure("rooms", err)
		}
		var out []session
		for id, room := range wrapper.Rooms {
			for session, raw := range room.Sessions {
				one, err := decodeSession(id, session, raw)
				if err != nil {
					return nil, err
				}
				out = append(out, one)
			}
		}
		return out, nil
	}
}

// decodeSession validates one submitted session.
func decodeSession(roomID, sessionID string, raw json.RawMessage) (session, error) {
	if !json.Valid(raw) || len(raw) == 0 {
		return session{}, matrix.NewBadJSON("a session is not a JSON object")
	}
	var fields struct {
		FirstMessageIndex *int64           `json:"first_message_index"`
		ForwardedCount    *int64           `json:"forwarded_count"`
		IsVerified        *bool            `json:"is_verified"`
		SessionData       *json.RawMessage `json:"session_data"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return session{}, matrix.DecodeFailure("session "+sessionID, err)
	}
	if fields.SessionData == nil || string(*fields.SessionData) == "null" {
		// Without this the server would happily store an entry that can never be decrypted and
		// report the backup as full, which is the one thing a backup must not do: look like it
		// worked.
		return session{}, matrix.NewMissingParam("session_data")
	}
	if roomID == "" {
		return session{}, matrix.NewMissingParam("room_id")
	}
	return session{
		roomID:            roomID,
		sessionID:         sessionID,
		payload:           raw,
		firstMessageIndex: dereference(fields.FirstMessageIndex),
		forwardedCount:    dereference(fields.ForwardedCount),
		isVerified:        fields.IsVerified != nil && *fields.IsVerified,
	}, nil
}

// decodePayload renders a stored payload, refusing to answer with something a client could not parse
// rather than serving a corrupt session out of a backup.
func decodePayload(payload string) (json.RawMessage, error) {
	if !json.Valid([]byte(payload)) {
		return nil, fmt.Errorf("the stored session is not JSON")
	}
	return json.RawMessage(payload), nil
}

// parseETag reads an If-Match header. Weak tags ("W/\\"1\\"") are accepted and compared by value:
// a backup's etag is opaque either way.
func parseETag(header string) (int64, bool, error) {
	header = strings.TrimSpace(header)
	if header == "" || header == "*" {
		return 0, false, nil
	}
	header = strings.Trim(header, `"'`)
	header = strings.TrimPrefix(header, "W/")
	header = strings.Trim(header, `"'`)
	value, err := strconv.ParseInt(header, 10, 64)
	if err != nil {
		return 0, false, matrix.NewInvalidParam("If-Match %q is not a backup etag", header)
	}
	return value, true, nil
}

// dereference reads an optional integer, defaulting it to zero.
func dereference(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// nowMS is the unit every timestamp on this server's rows is kept in.
func nowMS() int64 { return time.Now().UnixMilli() }
