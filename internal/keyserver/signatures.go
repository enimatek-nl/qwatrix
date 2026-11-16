package keyserver

// The other half of an identity: the signatures those keys make.
//
// A cross-signing key on its own is a public key with a claim attached. What makes it useful is the
// set of signatures it has produced — this device is mine, this other person is who they say they
// are — and a client makes those signatures locally, in the one place they can be made without
// trusting a server. So the server's job here is a narrow and slightly awkward one: it is asked to
// store signatures it cannot originate and would not be able to forge, and it must be picky enough
// that the answer is still worth anything.
//
// Which is what this file is for. A signature is stored when it is over the key this server holds,
// made by a key this user owns, and addressed to a key it is allowed to sign:
//
//   - your own devices, signed by your self-signing key — the usual case, and the only one a device
//     has for saying "she knows me";
//   - your own master key, signed by your own devices, which is how a master key gets its first
//     believers;
//   - somebody else's master key, signed by your user-signing key, which is what "I verified that
//     person" means on the wire, and the one write a user gets to make about another user.
//
// Anything else is refused per key rather than as a request, because a client uploads a batch and
// handles the failures one at a time.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// SignatureFailure is one key a signature upload could not accept.
type SignatureFailure struct {
	ErrCode matrix.ErrCode `json:"errcode"`
	Error   string         `json:"error"`
}

// UploadSignatures stores the signatures a client made with the user's cross-signing keys, and
// reports the ones it could not take, per user and per key.
func (s *Service) UploadSignatures(ctx context.Context, callerUserID string,
	byUser map[string]map[string]json.RawMessage) (map[string]map[string]SignatureFailure, error) {
	failures := map[string]map[string]SignatureFailure{}
	fail := func(userID, keyID string, err error) {
		if failures[userID] == nil {
			failures[userID] = map[string]SignatureFailure{}
		}
		failures[userID][keyID] = asFailure(err)
	}

	for userID, keys := range byUser {
		for keyID, document := range keys {
			var err error
			if userID == callerUserID {
				err = s.storeOwnSignatures(ctx, userID, keyID, document)
			} else {
				err = s.storeSignatureOfAnotherUser(ctx, callerUserID, userID, keyID, document)
			}
			if err != nil {
				fail(userID, keyID, err)
			}
		}
	}
	if len(failures) > 0 {
		return failures, nil
	}
	return nil, nil
}

// storeOwnSignatures accepts a signature over one of the caller's own keys: a device signed by the
// self-signing key, or the master key signed by one of the devices.
func (s *Service) storeOwnSignatures(ctx context.Context, userID, keyID string, document json.RawMessage) error {
	identity, err := s.OwnCrossSigningIdentity(ctx, userID)
	if err != nil {
		return err
	}
	parsed, err := parseSignedDocument(document)
	if err != nil {
		return err
	}

	// The master key is signed by the user's devices rather than by another cross-signing key, so
	// it is the one case where the signer is looked up somewhere else entirely.
	if identity.Master != nil && masterKeyID(identity.Master) == keyID {
		return s.storeMasterKeySignatures(ctx, userID, document, identity.Master)
	}

	selfSigning, err := masterSigningKey(identity.SelfSigning)
	if err != nil {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"the user has no self-signing key to sign devices with")
	}
	if _, signed := parsed.Signatures[userID][selfSigning.keyID]; !signed {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"a device must be signed by the self-signing key")
	}
	if err := verifySignedBy(userID, selfSigning.keyID, selfSigning.key, document); err != nil {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"the self-signing key did not sign this device: %v", err)
	}

	stored, err := s.store.Keys.Device(ctx, userID, keyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return matrix.NewNotFound("Unknown device %q", keyID)
		}
		return err
	}
	// What was signed has to be the key this server holds. A client signing a document of its own
	// making and posting only the signature would otherwise replace a device key with a rumour.
	if !sameKeyIgnoringSignatures(json.RawMessage(stored.KeyJSON), document) {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"the signed device key is not the one on file")
	}
	merged, err := mergeSignatures(json.RawMessage(stored.KeyJSON), parsed.Signatures)
	if err != nil {
		return err
	}
	stored.KeyJSON = string(merged)
	stored.UploadedMS = time.Now().UnixMilli()
	if err := s.store.Keys.PutDevice(ctx, stored); err != nil {
		return err
	}
	return s.announceIdentityChange(ctx, userID)
}

// storeMasterKeySignatures accepts signatures over the user's own master key, each of which has to
// come from one of that user's own devices.
func (s *Service) storeMasterKeySignatures(ctx context.Context, userID string, document, master json.RawMessage) error {
	parsed, err := parseSignedDocument(document)
	if err != nil {
		return err
	}
	devices, err := s.deviceKeysOf(ctx, userID)
	if err != nil {
		return err
	}
	accepted := map[string]map[string]string{}
	for keyID, signature := range parsed.Signatures[userID] {
		device, known := devices[keyID]
		if !known {
			// A signature from a device this server has never heard of proves nothing about the
			// master key, and storing it would let an old device of a logged-out account keep
			// vouching for identities the user has since replaced.
			return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
				"the master key is signed by an unknown device key %q", keyID)
		}
		if err := verifySignedBy(userID, keyID, device, document); err != nil {
			return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
				"device %q did not sign the master key: %v", keyID, err)
		}
		if accepted[userID] == nil {
			accepted[userID] = map[string]string{}
		}
		accepted[userID][keyID] = signature
	}
	if len(accepted) == 0 {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"no signature was supplied for the master key")
	}
	// Only the signatures move. The master key itself was published through
	// /keys/device_signing/upload, and a signature upload that quietly replaced it would be a way
	// to change an identity without answering for it.
	merged, err := mergeSignatures(master, parsed.Signatures)
	if err != nil {
		return err
	}
	rows, err := s.store.Keys.CrossSigningKeys(ctx, userID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Kind == store.CrossSigningMaster {
			row.KeyJSON = string(merged)
			return s.store.Keys.PutCrossSigningKeys(ctx, []store.CrossSigningKey{row})
		}
	}
	return nil
}

// storeSignatureOfAnotherUser accepts a signature the caller made over somebody else's master key
// with their own user-signing key, which is how "I have checked this person" survives the death of
// the device it was decided on.
//
// It is the one write a user makes about another user, so the checks are the reason it is allowed:
// the signature must come from the *caller's* user-signing key, and it must cover the master key
// this server holds for the target — not for whatever document the caller would like believed.
func (s *Service) storeSignatureOfAnotherUser(ctx context.Context, callerUserID, targetUserID, keyID string, document json.RawMessage) error {
	if !s.hosts(callerUserID) {
		return matrix.NewForbidden("You may not sign keys for another homeserver")
	}
	identity, err := s.OwnCrossSigningIdentity(ctx, callerUserID)
	if err != nil {
		return err
	}
	if identity.UserSigning == nil {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"the caller has no user-signing key to sign other people with")
	}
	signer, err := masterSigningKey(identity.UserSigning)
	if err != nil {
		return err
	}
	parsed, err := parseSignedDocument(document)
	if err != nil {
		return err
	}
	if _, signed := parsed.Signatures[callerUserID][signer.keyID]; !signed {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"another user's master key must be signed by the caller's user-signing key")
	}
	if err := verifySignedBy(callerUserID, signer.keyID, signer.key, document); err != nil {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"the user-signing key did not sign this key: %v", err)
	}

	target, err := s.OwnCrossSigningIdentity(ctx, targetUserID)
	if err != nil {
		return err
	}
	if target.Master == nil {
		return matrix.NewNotFound("User %q has no master key", targetUserID)
	}
	if !sameKeyIgnoringSignatures(target.Master, document) {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"the signed master key is not the one on file for %q", targetUserID)
	}
	rows, err := s.store.Keys.CrossSigningKeys(ctx, targetUserID)
	if err != nil {
		return err
	}
	merged, err := mergeSignatures(target.Master, parsed.Signatures)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Kind == store.CrossSigningMaster {
			row.KeyJSON = string(merged)
			if err := s.store.Keys.PutCrossSigningKeys(ctx, []store.CrossSigningKey{row}); err != nil {
				return err
			}
			return s.announceIdentityChange(ctx, targetUserID)
		}
	}
	return matrix.NewNotFound("User %q has no master key", targetUserID)
}

// announceIdentityChange tells the world that a user's identity moved, which is what makes a client
// go and look again.
func (s *Service) announceIdentityChange(ctx context.Context, userID string) error {
	if s.announce == nil {
		return nil
	}
	return s.announce(ctx, userID)
}

// deviceKeysOf returns the public keys of the user's devices, keyed by the signing key identifier
// ("ed25519:<device_id>"), which is how a signature names the device that made it.
func (s *Service) deviceKeysOf(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := s.store.Keys.Devices(ctx, userID, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, row := range rows {
		var document struct {
			Keys map[string]string `json:"keys"`
		}
		if err := json.Unmarshal([]byte(row.KeyJSON), &document); err != nil {
			continue
		}
		for keyID, key := range document.Keys {
			out[keyID] = key
		}
	}
	return out, nil
}

// signedDocument is a key document read for its signatures.
type signedDocument struct {
	Signatures map[string]map[string]string `json:"signatures"`
}

func parseSignedDocument(document json.RawMessage) (*signedDocument, error) {
	var parsed signedDocument
	if err := json.Unmarshal(document, &parsed); err != nil {
		return nil, matrix.NewBadJSON("signed key: " + err.Error())
	}
	if len(parsed.Signatures) == 0 {
		return nil, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidSignature,
			"a signed key must carry the signature")
	}
	return &parsed, nil
}

// mergeSignatures copies the given signatures into a key document without touching anything else,
// which matters because every other byte of the document is covered by a signature somewhere.
func mergeSignatures(document json.RawMessage, additions map[string]map[string]string) (json.RawMessage, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(document, &keys); err != nil {
		return nil, matrix.NewBadJSON("stored key: " + err.Error())
	}
	signatures := map[string]map[string]string{}
	if raw, present := keys["signatures"]; present {
		if err := json.Unmarshal(raw, &signatures); err != nil {
			return nil, matrix.NewBadJSON("stored signatures: " + err.Error())
		}
	}
	for userID, byKey := range additions {
		if signatures[userID] == nil {
			signatures[userID] = map[string]string{}
		}
		for keyID, signature := range byKey {
			signatures[userID][keyID] = signature
		}
	}
	encoded, err := json.Marshal(signatures)
	if err != nil {
		return nil, err
	}
	keys["signatures"] = encoded
	return json.Marshal(keys)
}

// sameKeyIgnoringSignatures reports whether two key documents are the same key. Signatures are the
// one part allowed to differ between them: two clients may each add one, and neither should be told
// their own device key has changed because the other got there first.
func sameKeyIgnoringSignatures(stored, uploaded json.RawMessage) bool {
	strip := func(document json.RawMessage) string {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(document, &keys); err != nil {
			return ""
		}
		delete(keys, "signatures")
		delete(keys, "unsigned")
		// A field this server does not know about is still part of the signed bytes, so dropping
		// it here would call two different keys the same.
		return canonicalForComparison(mustMarshalFields(keys))
	}
	a, b := strip(stored), strip(uploaded)
	return a != "" && a == b
}

// mustMarshalFields encodes the remaining fields of a key document.
func mustMarshalFields(fields map[string]json.RawMessage) []byte {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return encoded
}

// masterKeyID is the identifier a master key calls itself by: "ed25519:" followed by its public key.
// A signature upload names the key it covers by that identifier, which is how a client says "the
// master key" without the server having to guess which of a user's keys it meant.
func masterKeyID(master json.RawMessage) string {
	parsed, err := parseCrossSigningDocument(master)
	if err != nil {
		return ""
	}
	for keyID := range parsed.Keys {
		return keyID
	}
	return ""
}

// asFailure turns a refusal into the shape the response body reports it in.
func asFailure(err error) SignatureFailure {
	var api *matrix.APIError
	if errors.As(err, &api) {
		return SignatureFailure{ErrCode: api.Code, Error: api.Message}
	}
	return SignatureFailure{ErrCode: matrix.ErrUnknown, Error: err.Error()}
}
