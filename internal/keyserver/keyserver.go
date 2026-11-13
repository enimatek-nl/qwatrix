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
	// remote obtains the device lists of users this server does not have. It may be nil, in which
	// case a query for a remote user answers that their devices are unknown rather than pretending
	// they have none.
	remote RemoteKeySource
	// remoteClaim asks another homeserver to spend a one-time key for one of its devices. Nil in a
	// deployment that does not federate, where the honest answer is a named failure rather than silence.
	remoteClaim RemoteKeyClaimer
}

// RemoteKeySource is the outbound half of device-key federation: another homeserver's answer to
// "what devices does your user have?".
//
// It is a seam rather than a federation import because this package's job is the keys themselves —
// who may read them, how they are stored, what happens when they are stale — and the transport that
// asks a stranger is somebody else's business. It is also what keeps a homeserver from being able to
// be told anything about anybody: the caller names the server to ask, so a query for `@alice:hs1`
// can only ever be answered by `hs1`.
type RemoteKeySource interface {
	UserDevices(ctx context.Context, serverName, userID string) ([]json.RawMessage, error)
}

// RemoteKeyClaimer is the outbound half of a one-time-key claim across a federation border: it asks the
// server a user lives on for a key to start a session with one of their devices.
//
// It is a separate seam from RemoteKeySource rather than another method on it because the two have
// different consequences. Asking for a device list reads; this spends a key on somebody else's server,
// and a server that got it wrong could quietly exhaust a remote user's whole stock of one-time keys —
// leaving every device that tries to reach them unable to start a session at all. Keeping the seam narrow
// keeps that permission question visible.
type RemoteKeyClaimer interface {
	// ClaimKeys asks `serverName` to spend one-time keys for the devices it names, and returns them in
	// the shape the local claim produces: user, then device, then that device's key document.
	ClaimKeys(ctx context.Context, serverName string,
		oneTimeKeys map[string]map[string]string) (map[string]map[string]json.RawMessage, error)
}

// WithRemoteKeySource installs the federation source and returns the service for chaining.
func (s *Service) WithRemoteKeySource(remote RemoteKeySource) *Service {
	s.remote = remote
	return s
}

// WithRemoteKeyClaimer installs the transport that spends a remote device's key, and returns the service
// for chaining.
func (s *Service) WithRemoteKeyClaimer(claim RemoteKeyClaimer) *Service {
	s.remoteClaim = claim
	return s
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
	remote := s.remoteDeviceKeys(ctx, query)
	for userID, deviceIDs := range query {
		if keys, askedElsewhere := remote[userID]; askedElsewhere {
			// A user who lives elsewhere is answered by the server in their user ID. Their own
			// homeserver is the only one that can say which devices they have, and a server that
			// answered for somebody else's domain would let any peer mint keys for any user on the
			// planet — which is the entire security model of Megolm, thrown away.
			out[userID] = keys
			continue
		}
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

// remoteDeviceKeys asks other homeservers about the users of theirs that this query names.
//
// A failure is not fatal and not silent either: the user comes back with no devices, which a client
// reads as "cannot start an encrypted session with them yet", and the reason is logged. Answering the
// whole query with a 500 because one remote server is down would turn one unreachable homeserver into
// an outage for every participant of a shared room.
func (s *Service) remoteDeviceKeys(ctx context.Context, query map[string][]string) map[string]map[string]json.RawMessage {
	if s.remote == nil {
		return nil
	}
	out := map[string]map[string]json.RawMessage{}
	for userID, deviceIDs := range query {
		_, domain, err := matrix.ParseUserID(userID, s.cfg.ServerName)
		if err != nil || domain == s.cfg.ServerName {
			continue
		}
		documents, err := s.remote.UserDevices(ctx, domain, userID)
		if err != nil {
			slog.Warn("keyserver: could not ask a peer for a user's devices",
				"user_id", userID, "server", domain, "error", err)
			out[userID] = map[string]json.RawMessage{}
			continue
		}
		entries := map[string]json.RawMessage{}
		for _, document := range documents {
			// The device ID comes from the document rather than the peer's envelope, because the
			// document is the signed thing: an entry that disagrees with itself is not a device.
			var key struct {
				DeviceID string `json:"device_id"`
				UserID   string `json:"user_id"`
			}
			if err := json.Unmarshal(document, &key); err != nil || key.DeviceID == "" {
				continue
			}
			if key.UserID != userID {
				// A peer answering for `@alice:hs1` with a key that says `@mallory:hs1` is either
				// broken or malicious, and either way the key does not belong to the caller's list.
				slog.Warn("keyserver: a peer published a device key for a different user",
					"requested", userID, "published", key.UserID, "server", domain)
				continue
			}
			if len(deviceIDs) > 0 && !contains(deviceIDs, key.DeviceID) {
				continue
			}
			entries[key.DeviceID] = document
		}
		out[userID] = entries
	}
	return out
}

// contains reports whether a list holds a value.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// UserDevices answers a peer's `GET /_matrix/federation/v1/user/devices/{userID}`.
//
// Only this server's own users are answered, and that is not a courtesy: the endpoint is how a
// homeserver learns whom to encrypt to, so one that would speak for another domain hands out the power
// to insert a device into a user nobody asked it about.
func (s *Service) UserDevices(ctx context.Context, userID string) (map[string]any, error) {
	_, domain, err := matrix.ParseUserID(userID, s.cfg.ServerName)
	if err != nil {
		return nil, matrix.NewInvalidParam("%q is not a valid user ID", userID)
	}
	if domain != s.cfg.ServerName {
		return nil, matrix.NewNotFound("User %q does not live on this homeserver", userID)
	}
	rows, err := s.store.Keys.Devices(ctx, userID, nil)
	if err != nil {
		return nil, err
	}
	devices := make([]json.RawMessage, 0, len(rows))
	for i := range rows {
		if !json.Valid([]byte(rows[i].KeyJSON)) {
			// Publishing something a client cannot parse is worse than publishing one device fewer:
			// the client discards the whole response, including the keys that were fine.
			slog.Warn("keyserver: a stored device key is not valid JSON and is being withheld",
				"user_id", userID, "device_id", rows[i].DeviceID)
			continue
		}
		devices = append(devices, json.RawMessage(rows[i].KeyJSON))
	}
	// `device_lists` is the field name the specification kept from an older shape where it was
	// per-device deltas; it is a list of device keys, which is what every implementation reads.
	// Cross-signing keys would appear as `master_key` and `self_signing_key` beside it; this server
	// does not store cross-signing yet, so they are absent rather than empty.
	return map[string]any{"user_id": userID, "device_lists": devices}, nil
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
func (s *Service) Claim(ctx context.Context, _ string, request map[string]map[string]string) (map[string]map[string]json.RawMessage, map[string]any, error) {
	out := map[string]map[string]json.RawMessage{}
	// Failures are reported per server under its own name, which is what the specification's `failures`
	// map is keyed by and what a client uses to tell "this device has run out of keys" from "that
	// homeserver could not be asked". Returning nothing for an unreachable user is the worst answer
	// available: a client cannot distinguish it from an exhausted device, stops trying, and the
	// conversation quietly never becomes encrypted at all.
	failures := map[string]any{}

	local, remote, unreadable := s.splitClaimByOwner(request)
	for _, userID := range unreadable {
		slog.Debug("keyserver: a claim named something that is not a user ID", "user_id", userID)
	}
	for destination, byUser := range remote {
		if s.remoteClaim == nil {
			// A deployment that does not federate cannot ask, and the place to say so is the answer the
			// client reads rather than only the log it will never open.
			failures[destination] = map[string]any{"error": "this homeserver is not federating"}
			continue
		}
		claimed, err := s.remoteClaim.ClaimKeys(ctx, destination, byUser)
		if err != nil {
			slog.Warn("keyserver: could not ask a peer for a device's one-time keys",
				"server", destination, "users", len(byUser), "error", err)
			failures[destination] = map[string]any{"error": err.Error()}
			continue
		}
		for userID, devices := range claimed {
			out[userID] = devices
		}
	}

	for userID, devices := range local {
		for deviceID, algorithm := range devices {
			// A device may claim its own one-time keys. Refusing would be tempting — it spends
			// keys the uploader presumably wanted — but complement's own suite does it to
			// observe the spending, and the keys belong to the account asking.
			claimed, err := s.store.Keys.ClaimOneTimeKeys(ctx, userID, deviceID, algorithm, 1)
			if err != nil {
				return nil, nil, err
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
	return out, failures, nil
}

// splitClaimByOwner sorts a claim request into this server's own users, the peers that have to be asked,
// and the names that are not user IDs at all.
//
// Splitting on the domain rather than trying the local store first is the point: device IDs are chosen by
// whoever registers them, so two servers hand out the same identifier all the time, and a lookup that
// fell through to a stranger's device would spend a local key for a remote request and tell the client it
// had started a session with somebody it has never met.
func (s *Service) splitClaimByOwner(request map[string]map[string]string) (local map[string]map[string]string, remote map[string]map[string]map[string]string, unreadable []string) {
	local = map[string]map[string]string{}
	remote = map[string]map[string]map[string]string{}
	for userID, devices := range request {
		_, domain, err := matrix.ParseUserID(userID, s.cfg.ServerName)
		if err != nil {
			unreadable = append(unreadable, userID)
			continue
		}
		if string(domain) == s.cfg.ServerName {
			local[userID] = devices
			continue
		}
		if remote[string(domain)] == nil {
			remote[string(domain)] = map[string]map[string]string{}
		}
		remote[string(domain)][userID] = devices
	}
	return local, remote, unreadable
}

// PeerDeviceKeys answers another homeserver's question about the devices of *this* server's users,
// which is the served half of `POST /_matrix/federation/v1/user/keys/query`.
//
// A name whose domain is not this server is dropped rather than chased. That is not shyness: a server
// that answered for somebody else's domain would let any peer mint keys for any user on the planet, and
// a client starting an Olm session has no way to tell an answer from an forgery — it trusts the keys it
// is given because they came from the domain in the user ID. Chasing a third server for it would also
// turn this server into a relay for key questions it cannot vouch for.
//
// A user who lives here and has no devices is answered with an empty set rather than omitted, because
// "no devices" is information the asking server needs in order to stop waiting for a key share, while
// an absent entry is indistinguishable from "not my problem".
func (s *Service) PeerDeviceKeys(ctx context.Context, query map[string][]string) (map[string]map[string]json.RawMessage, error) {
	out := make(map[string]map[string]json.RawMessage, len(query))
	for userID, deviceIDs := range query {
		if !s.hosts(userID) {
			slog.Debug("keyserver: a peer asked for the devices of a user this homeserver does not host",
				"user_id", userID)
			continue
		}
		entries := map[string]json.RawMessage{}
		rows, err := s.store.Keys.Devices(ctx, userID, deviceIDs)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			entries[rows[i].DeviceID] = json.RawMessage(rows[i].KeyJSON)
		}
		out[userID] = entries
	}
	return out, nil
}

// PeerClaimKeys spends one-time keys of this server's devices for a peer, which is the served half of
// `POST /_matrix/federation/v1/user/keys/claim`. This is how an Olm session with a user on another
// homeserver begins: without it, a device on the far side has nothing to encrypt its Megolm key to, and
// the conversation stays in plaintext or never starts.
//
// The same rule as the query, and it matters more here: keys are spent by being handed over, so serving
// one for a user this server does not host would be spending a secret this server does not own.
func (s *Service) PeerClaimKeys(ctx context.Context, request map[string]map[string]string) (map[string]map[string]json.RawMessage, error) {
	out := map[string]map[string]json.RawMessage{}
	for userID, devices := range request {
		if !s.hosts(userID) {
			slog.Debug("keyserver: a peer asked to claim the keys of a user this homeserver does not host",
				"user_id", userID)
			continue
		}
		for deviceID, algorithm := range devices {
			claimed, err := s.store.Keys.ClaimOneTimeKeys(ctx, userID, deviceID, algorithm, 1)
			if err != nil {
				return nil, err
			}
			if len(claimed) == 0 {
				// Out of prepaid keys. The asking device learns by absence, which is how the
				// specification puts it: an empty claim is a normal answer, not an error.
				continue
			}
			keys := map[string]json.RawMessage{}
			for _, key := range claimed {
				keys[key.KeyID] = json.RawMessage(key.KeyJSON)
			}
			entries, ok := out[userID]
			if !ok {
				entries = map[string]json.RawMessage{}
				out[userID] = entries
			}
			entries[deviceID] = json.RawMessage(mustJSON(keys))
		}
	}
	return out, nil
}

// hosts reports whether a user ID names this server.
func (s *Service) hosts(userID string) bool {
	_, domain, err := matrix.ParseUserID(userID, s.cfg.ServerName)
	return err == nil && string(domain) == s.cfg.ServerName
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
