package keyserver

// Cross-signing: the keys that make a person, rather than a device.
//
// A device key says "this device is who its signature says it is", which is only worth anything to
// somebody who already trusts the device. Cross-signing closes that gap with three more keys that
// this server stores and never holds the private half of: a master key the user's own devices
// vouch for, a self-signing key the master key stamps onto each of those devices, and a
// user-signing key the master key stamps onto *other people's* master keys. Verification in a
// client is a walk along those signatures, and the homeserver's part is to be the place the walk
// starts from — which is a responsibility, because a server that stores a cross-signing key without
// looking at it is a server where one leaked access token mints an identity.
//
// So this file is mostly the questions to ask before storing one:
//
//   - Is it signed by the key it claims to be signed by? The signed object is the document with
//     `signatures` removed — the ordinary Matrix rule, confirmed against the bundles the Rust SDK
//     produces rather than from memory — and the signing key is `keys["ed25519:<id>"]` from the
//     document itself.
//   - Is it the kind of key it is uploaded as? A master key posted as `self_signing_key` would make
//     the client's walk of the signature chain arrive somewhere the user never meant it to.
//   - Could it be mistaken for one of the user's devices? A cross-signing key is named by its own
//     public key and a device by its device ID, and both end up in the same `keys` map of a key
//     query answer. A key whose ID is also a device ID is how one of those becomes the other.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/matrix-org/gomatrixserverlib"
)

// CrossSigningBundle is the set of keys a client uploads in one request. Any of them may be absent,
// which means "leave that one alone" — except at first-time setup, where a client normally sends all
// three because it has just created all three.
type CrossSigningBundle struct {
	Master      json.RawMessage
	SelfSigning json.RawMessage
	UserSigning json.RawMessage
}

// CrossSigningIdentity is a user's published identity: whichever of the three keys they have.
type CrossSigningIdentity struct {
	Master      json.RawMessage
	SelfSigning json.RawMessage
	UserSigning json.RawMessage
}

// Empty reports whether the bundle asked for nothing at all. A client that lost the response to an
// earlier upload retries with the same keys, and a client that sends none has nothing to change.
func (b CrossSigningBundle) Empty() bool {
	return len(b.Master) == 0 && len(b.SelfSigning) == 0 && len(b.UserSigning) == 0
}

// uploads returns the bundle as (kind, document) pairs in the order a client sends them, which is
// the order the specification checks them in: a self-signing key needs the master key from the same
// request if it is not already stored, so the master key has to be looked at first.
func (b CrossSigningBundle) uploads() []struct {
	kind     string
	document json.RawMessage
} {
	options := []struct {
		kind     string
		document json.RawMessage
	}{
		{store.CrossSigningMaster, b.Master},
		{store.CrossSigningSelfSigning, b.SelfSigning},
		{store.CrossSigningUserSigning, b.UserSigning},
	}
	uploads := make([]struct {
		kind     string
		document json.RawMessage
	}, 0, len(options))
	for _, option := range options {
		if len(option.document) > 0 {
			uploads = append(uploads, option)
		}
	}
	return uploads
}

// HasCrossSigningIdentity reports whether this user has already published a master key, which is
// the difference between "setting up for the first time" and "replacing an identity somebody else
// may be relying on". The first is allowed on the strength of the access token alone (MSC3967);
// the second asks the user to prove they are still them.
func (s *Service) HasCrossSigningIdentity(ctx context.Context, userID string) (bool, error) {
	rows, err := s.store.Keys.CrossSigningKeys(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Kind == store.CrossSigningMaster {
			return true, nil
		}
	}
	return false, nil
}

// CrossSigningUnchanged reports whether every document in the bundle is byte-for-byte the key this
// server already holds for that kind.
//
// It exists for a client that uploaded its identity and never got the answer. Retrying is correct,
// and asking that retry to authenticate again is how a client ends up asking a person for their
// password twice for one act.
func (s *Service) CrossSigningUnchanged(ctx context.Context, userID string, bundle CrossSigningBundle) (bool, error) {
	uploads := bundle.uploads()
	if len(uploads) == 0 {
		return true, nil
	}
	rows, err := s.store.Keys.CrossSigningKeys(ctx, userID)
	if err != nil {
		return false, err
	}
	stored := make(map[string]string, len(rows))
	for _, row := range rows {
		stored[row.Kind] = canonicalForComparison([]byte(row.KeyJSON))
	}
	for _, upload := range uploads {
		if stored[upload.kind] != canonicalForComparison(upload.document) {
			return false, nil
		}
	}
	return true, nil
}

// UploadCrossSigningKeys checks a bundle and stores it.
//
// The checks are all done before the first write, because a half-stored identity is worse than none:
// a user with a master key and no self-signing key has devices nothing vouches for, and a client
// cannot tell that state apart from a user who never set anything up.
func (s *Service) UploadCrossSigningKeys(ctx context.Context, userID string, bundle CrossSigningBundle) error {
	uploads := bundle.uploads()
	if len(uploads) == 0 {
		return nil
	}

	existing, err := s.store.Keys.CrossSigningKeys(ctx, userID)
	if err != nil {
		return err
	}
	stored := make(map[string]json.RawMessage, len(existing))
	for _, row := range existing {
		stored[row.Kind] = json.RawMessage(row.KeyJSON)
	}

	// The master key that signs the other two is whichever one is in force when this request is
	// done: the one being uploaded now, or the one already on file.
	signer := stored[store.CrossSigningMaster]
	rows := make([]store.CrossSigningKey, 0, len(uploads))
	for _, upload := range uploads {
		document := upload.document
		if upload.kind == store.CrossSigningMaster {
			signer = document
		}
		key, err := s.checkCrossSigningDocument(ctx, userID, upload.kind, document, signer)
		if err != nil {
			return err
		}
		rows = append(rows, key)
	}
	if err := s.store.Keys.PutCrossSigningKeys(ctx, rows); err != nil {
		return err
	}

	// A new identity is a device-list change, and a client that does not hear about one keeps
	// encrypting to a set of keys that has already been replaced. The user's own other devices are
	// the ones that need to hear it most: they are the ones that have to start trusting it.
	if s.announce != nil {
		if err := s.announce(ctx, userID); err != nil {
			slog.Warn("keyserver: announcing a cross-signing change failed", "user_id", userID, "error", err)
		}
	}
	return nil
}

// checkCrossSigningDocument validates one key of a bundle and builds the row that stores it.
func (s *Service) checkCrossSigningDocument(ctx context.Context, userID, kind string, document, master json.RawMessage) (store.CrossSigningKey, error) {
	parsed, err := parseCrossSigningDocument(document)
	if err != nil {
		return store.CrossSigningKey{}, err
	}
	if parsed.UserID != "" && parsed.UserID != userID {
		return store.CrossSigningKey{}, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"the %s key claims to belong to %q, not to the authenticated user", kind, parsed.UserID)
	}
	if err := parsed.usageFits(kind); err != nil {
		return store.CrossSigningKey{}, err
	}
	if len(parsed.Keys) != 1 {
		return store.CrossSigningKey{}, matrix.NewBadJSON(
			fmt.Sprintf("a cross-signing key must hold exactly one key, %d were uploaded", len(parsed.Keys)))
	}

	// The document's own key is the one it is named by, and it is the one that has to be able to
	// sign — for a master key, itself; for the other two, the master key.
	for keyID, key := range parsed.Keys {
		if err := s.keyIsNotADevice(ctx, userID, keyID); err != nil {
			return store.CrossSigningKey{}, err
		}
		switch kind {
		case store.CrossSigningMaster:
			if err := verifySignedBy(userID, keyID, key, document); err != nil {
				return store.CrossSigningKey{}, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
					"a master key must be signed by itself: %v", err)
			}
		default:
			signer, err := masterSigningKey(master)
			if err != nil {
				return store.CrossSigningKey{}, err
			}
			if err := verifySignedBy(userID, signer.keyID, signer.key, document); err != nil {
				return store.CrossSigningKey{}, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
					"a %s key must be signed by the master key: %v", kind, err)
			}
		}
		return store.CrossSigningKey{
			UserID:     userID,
			Kind:       kind,
			KeyID:      keyID,
			KeyJSON:    string(document),
			UploadedMS: time.Now().UnixMilli(),
		}, nil
	}
	return store.CrossSigningKey{}, matrix.NewBadJSON("a cross-signing key must hold one key")
}

// keyIsNotADevice refuses a cross-signing key whose identifier is one of the user's device IDs.
//
// The two kinds of key live in the same `keys` map of a key query answer and are told apart by what
// their names look like. A cross-signing key named after a device is a hole in that rule, and the
// client on the other end of the answer has no way to notice it fell in.
//
// The check is against the account's devices rather than against the device keys it has published:
// a device exists from the moment it logs in, and one that has not uploaded its keys yet is not a
// name that is free to take.
func (s *Service) keyIsNotADevice(ctx context.Context, userID, keyID string) error {
	_, identifier, found := strings.Cut(keyID, ":")
	if !found {
		identifier = keyID
	}
	localpart, _, err := matrix.ParseUserID(userID, s.cfg.ServerName)
	if err != nil {
		return matrix.NewInvalidParam("%q is not a valid user ID", userID)
	}
	devices, err := s.store.Devices.List(ctx, localpart)
	if err != nil {
		return err
	}
	for _, device := range devices {
		if device.ID == identifier {
			return matrix.NewForbidden("Key ID in use")
		}
	}
	return nil
}

// crossSigningDocument is the part of a cross-signing key this server has to read. Everything else
// is kept as it arrived, because the signature covers exactly these bytes.
type crossSigningDocument struct {
	UserID string            `json:"user_id"`
	Usage  json.RawMessage   `json:"usage"`
	Keys   map[string]string `json:"keys"`
}

// publicKeys are the keys of a signature this document carries, in no particular order.
type signatureKeys struct {
	keyID string
	key   string
}

// masterSigningKey picks the key identifier and public key out of a master key document.
func masterSigningKey(master json.RawMessage) (signatureKeys, error) {
	if len(master) == 0 {
		return signatureKeys{}, matrix.NewMissingParam("master_key")
	}
	parsed, err := parseCrossSigningDocument(master)
	if err != nil {
		return signatureKeys{}, err
	}
	for keyID, key := range parsed.Keys {
		return signatureKeys{keyID: keyID, key: key}, nil
	}
	return signatureKeys{}, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrMissingParam,
		"no master key is available")
}

func parseCrossSigningDocument(document json.RawMessage) (*crossSigningDocument, error) {
	var parsed crossSigningDocument
	if err := json.Unmarshal(document, &parsed); err != nil {
		return nil, matrix.NewBadJSON("cross-signing key: " + err.Error())
	}
	if len(parsed.Keys) == 0 {
		return nil, matrix.NewBadJSON("a cross-signing key must carry its public key")
	}
	return &parsed, nil
}

// usageFits checks the `usage` field against the kind of key the client uploaded it as.
//
// The field is a string in the older spelling and an array now, and the older names differ too:
// a self-signing key used to say "signing". Both are accepted; a key that names itself something
// else is refused, because the kind is not decoration — it is what a client is allowed to do with
// the key.
func (d *crossSigningDocument) usageFits(kind string) error {
	names := usageNames(d.Usage)
	if len(names) == 0 {
		return matrix.NewBadJSON("a cross-signing key must say what it is for")
	}
	wanted := map[string][]string{
		store.CrossSigningMaster:      {"master"},
		store.CrossSigningSelfSigning: {"self_signing", "signing"},
		store.CrossSigningUserSigning: {"user_signing"},
	}[kind]
	for _, name := range names {
		for _, option := range wanted {
			if name == option {
				return nil
			}
		}
	}
	return matrix.NewBadJSON(fmt.Sprintf(
		"a %s key must say it is for %s", kind, joinOr(wanted)))
}

// usageNames reads the `usage` field whichever way it was spelled.
func usageNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}
	}
	return nil
}

// joinOr spells a small list the way the sentence reads: "master", "self_signing or signing".
func joinOr(options []string) string {
	switch len(options) {
	case 0:
		return ""
	case 1:
		return options[0]
	default:
		return strings.Join(options[:len(options)-1], ", ") + " or " + options[len(options)-1]
	}
}

// canonicalForComparison puts a key document into the form two of them can be compared in. Two
// documents that differ only in whitespace or key order are the same key, and a client that retried
// an upload it had already made should not be asked for its password over a space character.
func canonicalForComparison(document []byte) string {
	canonical, err := gomatrixserverlib.CanonicalJSON(document)
	if err != nil {
		return string(document)
	}
	return string(canonical)
}

// decodeSigningKey turns the base64 public key of a key document into the key itself.
func decodeSigningKey(publicKey string) (ed25519.PublicKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(publicKey)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(publicKey); err != nil {
			return nil, fmt.Errorf("%q is not base64: %w", publicKey, err)
		}
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%q is %d bytes, not an ed25519 key", publicKey, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// verifySignedBy checks that `document` carries a valid signature from the named key. The signed
// object is the document with `signatures` and `unsigned` removed, which is what
// [gomatrixserverlib.VerifyJSON] does before it checks, and what every client does too.
func verifySignedBy(userID, keyID, publicKey string, document json.RawMessage) error {
	key, err := decodeSigningKey(publicKey)
	if err != nil {
		return err
	}
	return gomatrixserverlib.VerifyJSON(userID, gomatrixserverlib.KeyID(keyID), key, document)
}

// CrossSigningFor answers "what is these users' identity?" for a key query.
//
// A user-signing key is returned only to the user it belongs to: it is the key that says "I vouch
// for this other person", and other people do not need it to check a device — the master and
// self-signing keys do that. The specification draws the same line, and a server that showed
// everybody everybody's endorsements would be publishing a social graph nobody agreed to publish.
func (s *Service) CrossSigningFor(ctx context.Context, requester string, userIDs []string) (CrossSigningIdentityByUser, error) {
	out := CrossSigningIdentityByUser{
		Master:      map[string]json.RawMessage{},
		SelfSigning: map[string]json.RawMessage{},
		UserSigning: map[string]json.RawMessage{},
	}
	for _, userID := range userIDs {
		rows, err := s.store.Keys.CrossSigningKeys(ctx, userID)
		if err != nil {
			return out, err
		}
		for _, row := range rows {
			document := json.RawMessage(row.KeyJSON)
			switch row.Kind {
			case store.CrossSigningMaster:
				out.Master[userID] = document
			case store.CrossSigningSelfSigning:
				out.SelfSigning[userID] = document
			case store.CrossSigningUserSigning:
				if userID == requester {
					out.UserSigning[userID] = document
				}
			}
		}
	}
	return out, nil
}

// CrossSigningIdentityByUser is a key query's answer about cross-signing: one map per kind, each
// from user ID to key document. A kind nobody has is an empty map, which the handler leaves out of
// the response rather than answering with a bare `{}` next to three other fields.
type CrossSigningIdentityByUser struct {
	Master      map[string]json.RawMessage
	SelfSigning map[string]json.RawMessage
	UserSigning map[string]json.RawMessage
}

// OwnCrossSigningIdentity returns the caller's own identity, which is what the federation answer
// about a local user carries: a peer needs the master and self-signing keys to check that a device
// really belongs to the user it says it does.
func (s *Service) OwnCrossSigningIdentity(ctx context.Context, userID string) (CrossSigningIdentity, error) {
	rows, err := s.store.Keys.CrossSigningKeys(ctx, userID)
	if err != nil {
		return CrossSigningIdentity{}, err
	}
	var identity CrossSigningIdentity
	for _, row := range rows {
		switch row.Kind {
		case store.CrossSigningMaster:
			identity.Master = json.RawMessage(row.KeyJSON)
		case store.CrossSigningSelfSigning:
			identity.SelfSigning = json.RawMessage(row.KeyJSON)
		case store.CrossSigningUserSigning:
			identity.UserSigning = json.RawMessage(row.KeyJSON)
		}
	}
	return identity, nil
}

// PeerCrossSigningKeys answers another homeserver's question about the identity of *this* server's
// users, in the shape a key query carries it: a field per kind, each from user ID to key document.
//
// The user-signing key is not among them, and that is not an oversight. It is the record of whom the
// user vouches for; a peer asking on behalf of one of its clients has no claim to read it, and the
// asking client does not need it to check a device — the master and self-signing keys do that work.
func (s *Service) PeerCrossSigningKeys(ctx context.Context, userIDs []string) (map[string]map[string]json.RawMessage, error) {
	out := map[string]map[string]json.RawMessage{
		"master_keys":       map[string]json.RawMessage{},
		"self_signing_keys": map[string]json.RawMessage{},
	}
	for _, userID := range userIDs {
		if !s.hosts(userID) {
			// The same rule as the device keys beside these: a server that spoke for another domain
			// would let any peer mint an identity for any user anywhere.
			continue
		}
		identity, err := s.OwnCrossSigningIdentity(ctx, userID)
		if err != nil {
			return nil, err
		}
		if len(identity.Master) > 0 {
			out["master_keys"][userID] = identity.Master
		}
		if len(identity.SelfSigning) > 0 {
			out["self_signing_keys"][userID] = identity.SelfSigning
		}
	}
	return out, nil
}
