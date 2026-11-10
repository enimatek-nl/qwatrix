// Package keyserver stores the end-to-end encryption keys this server's own devices publish.
//
// A homeserver cannot read the conversations its users have encrypted — that is the point — but it
// holds the two things a stranger needs in order to start one: each device's published identity,
// and a pile of prepaid message keys waiting to be spent. Both are stored as the client wrote them,
// because a device key carries its own signature over its exact bytes: a server that "tidied" one
// would hand out a key that fails to verify, and the failure would surface as somebody else's
// client refusing to talk to them.
package keyserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Service is the key server.
type Service struct {
	store   *store.Store
	cfg     *config.Config
	changes *notifier.Notifier
	// announce tells a user's room-mates that their device list moved. It is a seam rather than a
	// dependency on the device-list service so that this package does not have to know who cares.
	announce func(ctx context.Context, userID string) error
}

// WithDeviceListAnnouncer installs the device-list hook and returns the service for chaining.
func (s *Service) WithDeviceListAnnouncer(announce func(ctx context.Context, userID string) error) *Service {
	s.announce = announce
	return s
}

// New builds the service. changes is the notifier the /sync long-pollers wait on, so uploading
// keys wakes the devices that are polling to find out whether a key they asked for appeared.
func New(st *store.Store, cfg *config.Config, changes *notifier.Notifier) *Service {
	return &Service{store: st, cfg: cfg, changes: changes}
}

// DeviceKeys returns the published keys of the named users' devices, in the shape `/keys/query`
// answers with: a map from user ID to a map from device ID to the device key document.
//
// Every queried user gets an entry even when they have no keys, because a client reads this
// response as an authoritative statement about a list it asked for. Omitting a user would be
// indistinguishable from a server that did not understand the query, and the client's only
// recourse would be to retry forever.
func (s *Service) DeviceKeysFor(ctx context.Context, query map[string][]string) (map[string]map[string]json.RawMessage, error) {
	out := make(map[string]map[string]json.RawMessage, len(query))
	for userID, deviceIDs := range query {
		entries := map[string]json.RawMessage{}
		rows, err := s.store.Keys.Devices(ctx, userID, deviceIDs)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			var document map[string]json.RawMessage
			if err := json.Unmarshal([]byte(rows[i].KeyJSON), &document); err != nil {
				// A stored document that cannot be parsed cannot be handed to another client
				// either; dropping it is better than answering 500 for the whole query.
				continue
			}
			entries[rows[i].DeviceID] = json.RawMessage(rows[i].KeyJSON)
		}
		out[userID] = entries
	}
	return out, nil
}

// UploadRequest is the body of POST /keys/upload.
type UploadRequest struct {
	DeviceKeys json.RawMessage `json:"device_keys"`
	// OneTimeKeys maps a key ID like "signed_curve25519:AAAAHQ" to its key object.
	OneTimeKeys map[string]json.RawMessage `json:"one_time_keys"`
	// FallbackKeys are MSC3391's reusable stand-ins, used when a device has run out of
	// one-time keys. They arrive with the key ID's leading "+" stripped.
	FallbackKeys map[string]json.RawMessage `json:"fallback_keys"`
}

// Upload stores a device's keys and returns the counts a client uses to decide whether to upload
// more.
//
// The device key document is validated only for the fields the protocol cannot do without. Its
// signature is *not* verified here: an identity server or a backup may legitimately upload a key
// this server has no way to check, and complement's own clients upload keys they signed by hand.
// Verification happens where the key is used, by the client that has a reason to care.
func (s *Service) Upload(ctx context.Context, callerUserID, deviceID string, req UploadRequest) (map[string]int64, error) {
	announcedDeviceKeys := false
	if len(req.DeviceKeys) > 0 && !bytes.Equal(bytes.TrimSpace(req.DeviceKeys), []byte("null")) {
		key, err := s.parseDeviceKeys(callerUserID, deviceID, req.DeviceKeys)
		if err != nil {
			return nil, err
		}
		if err := s.store.Keys.PutDevice(ctx, key); err != nil {
			return nil, err
		}
		announcedDeviceKeys = true
	}

	keys := make([]store.OneTimeKey, 0, len(req.OneTimeKeys)+len(req.FallbackKeys))
	userID := callerUserID
	for keyID, document := range req.OneTimeKeys {
		keys = append(keys, s.oneTimeKey(userID, deviceID, keyID, document, false))
	}
	for keyID, document := range req.FallbackKeys {
		// MSC3391 spells a fallback key with a leading "+" in the upload and strips it when the
		// key is returned; storing the stripped form means every reader sees one spelling.
		keys = append(keys, s.oneTimeKey(userID, deviceID,
			strings.TrimPrefix(keyID, "+"), document, true))
	}
	if len(keys) > 0 {
		// Uploaded in a stable order so the oldest-first claim order matches what the client
		// sent rather than Go's map iteration order.
		sort.Slice(keys, func(i, j int) bool { return keys[i].KeyID < keys[j].KeyID })
		if _, err := s.store.Keys.AddOneTimeKeys(ctx, keys); err != nil {
			return nil, err
		}
		s.changes.Publish()
	}
	// Uploading device identity keys *is* a device-list change. A colleague's client cannot tell
	// a first upload from a replacement, and does not need to: either way it must re-query, or it
	// keeps encrypting for a device list that no longer exists.
	if announcedDeviceKeys && s.announce != nil {
		if err := s.announce(ctx, callerUserID); err != nil {
			slog.Warn("keyserver: announcing a device-list change failed", "user_id", callerUserID, "error", err)
		}
	}
	return s.store.Keys.OneTimeKeyCounts(ctx, userID, deviceID)
}

// oneTimeKey builds a stored row, splitting the algorithm off the key ID the way the spec's
// "algorithm:key_id" form asks.
func (s *Service) oneTimeKey(userID, deviceID, keyID string, document json.RawMessage, fallback bool) store.OneTimeKey {
	algorithm := keyID
	if _, after, found := strings.Cut(keyID, ":"); found {
		algorithm = keyID[:len(keyID)-len(after)-1]
	}
	return store.OneTimeKey{
		UserID:     userID,
		DeviceID:   deviceID,
		KeyID:      keyID,
		Algorithm:  algorithm,
		KeyJSON:    string(document),
		UploadedMS: time.Now().UnixMilli(),
		Fallback:   fallback,
	}
}

// parseDeviceKeys checks the parts of a device key document that this server has to rely on.
//
// `algorithms`, `keys` and `signatures` are required by the specification, and a document without
// them is a client bug worth a 400: a key with no signature verifies for nobody, and a client that
// uploaded one would spend its life wondering why it was never queried.
func (s *Service) parseDeviceKeys(callerUserID, deviceID string, raw json.RawMessage) (*store.DeviceKey, error) {
	var document struct {
		UserID     string            `json:"user_id"`
		DeviceID   string            `json:"device_id"`
		Algorithms json.RawMessage   `json:"algorithms"`
		Keys       map[string]string `json:"keys"`
		Signatures json.RawMessage   `json:"signatures"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, matrix.NewBadJSON("device_keys: " + err.Error())
	}
	if document.UserID == "" || document.DeviceID == "" {
		return nil, matrix.NewBadJSON("device_keys must name a user_id and a device_id")
	}
	if document.UserID != callerUserID {
		// Publishing keys for a device that is not the caller's would let any account mint
		// identity for somebody else's device — and the victim's client would have no way to
		// tell, because the keys look like they came from the victim's own upload.
		return nil, matrix.NewBadJSON(fmt.Sprintf(
			"device_keys claim to belong to %q, not to the authenticated user", document.UserID))
	}
	if document.Algorithms == nil || document.Keys == nil || document.Signatures == nil {
		return nil, matrix.NewBadJSON(
			"device_keys requires algorithms, keys and signatures")
	}
	if document.DeviceID != deviceID {
		return nil, matrix.NewBadJSON(fmt.Sprintf(
			"device_keys claim to belong to device %q, but this session is %q",
			document.DeviceID, deviceID))
	}
	return &store.DeviceKey{
		UserID:     document.UserID,
		DeviceID:   document.DeviceID,
		KeyJSON:    string(raw),
		VerifyKey:  document.Keys["curve25519:"+document.DeviceID],
		SigningKey: document.Keys["ed25519:"+document.DeviceID],
		UploadedMS: time.Now().UnixMilli(),
	}, nil
}

// Claim spends one-time keys for the requesting device to start sessions with.
//
// Keys go out oldest first, and a claimed key is gone: a key handed to two senders stops being a
// one-time key, and the Double Ratchet's forward secrecy depends on it having been spent once.
// A caller who gets nothing back is told so by their device being absent from the response rather
// than by an error, because "this device has run out of prepaid keys and must be online for you to
// reach it" is a normal outcome of the protocol, not a failure of the request.
func (s *Service) Claim(ctx context.Context, _ string, request map[string]map[string]string) (map[string]map[string]json.RawMessage, error) {
	out := map[string]map[string]json.RawMessage{}
	for userID, devices := range request {
		for deviceID, algorithm := range devices {
			// A device may claim its own one-time keys. Refusing would be tempting — it spends
			// keys the uploader presumably wanted — but complement's own suite does it to
			// observe the spending, and the keys belong to the account asking.
			claimed, err := s.store.Keys.ClaimOneTimeKeys(ctx, userID, deviceID, algorithm, 1)
			if err != nil {
				return nil, err
			}
			if len(claimed) == 0 {
				continue
			}
			entries, ok := out[userID]
			if !ok {
				entries = map[string]json.RawMessage{}
				out[userID] = entries
			}
			keys := map[string]json.RawMessage{}
			for _, key := range claimed {
				keys[key.KeyID] = json.RawMessage(key.KeyJSON)
			}
			entries[deviceID] = json.RawMessage(mustJSON(keys))
		}
	}
	return out, nil
}

// mustJSON encodes a map of raw JSON, which cannot fail.
func mustJSON(value map[string]json.RawMessage) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// Counts reports how many one-time keys a device holds, which is what a client polls to decide
// whether to top up.
func (s *Service) Counts(ctx context.Context, userID, deviceID string) (map[string]int64, error) {
	return s.store.Keys.OneTimeKeyCounts(ctx, userID, deviceID)
}

// DeleteDevices forgets the published keys of sessions that have been logged out.
func (s *Service) DeleteDevices(ctx context.Context, userID string, deviceIDs []string) error {
	return s.store.Keys.DeleteDevices(ctx, userID, deviceIDs)
}
