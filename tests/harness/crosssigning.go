package harness

// Builders for the cross-signing documents a real client uploads, signed for real.
//
// A cross-signing key is only believed when its signature checks out, so a test that wants a
// homeserver to accept one has to sign it. The rules these follow were derived from the bundle
// the matrix-rust-sdk (the one Element Web ships) produces: the signed object is the key document
// with `signatures` removed, and the signing key is `keys["ed25519:<id>"]` — for a cross-signing
// key the ID is the key's own base64 public key, for a device the device ID.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
)

// base64Unpadded encodes a key the way Matrix encodes every binary value: standard base64 with
// the "=" padding taken off.
func base64Unpadded(raw []byte) string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString(raw), "=")
}

// SigningKey is an ed25519 key pair standing in for one of a user's cross-signing keys.
type SigningKey struct {
	Private ed25519.PrivateKey
	// ID is the key identifier the client uses: "ed25519:" followed by the unpadded base64
	// public key, which is what a cross-signing key calls its own key.
	ID string
	// PublicKey is that same base64 public key without the algorithm prefix.
	PublicKey string
}

// NewSigningKey generates a fresh signing key. Tests do not need deterministic keys; they need
// signatures that verify.
func NewSigningKey(t *testing.T) *SigningKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}
	raw := base64Unpadded(pub)
	return &SigningKey{Private: priv, ID: "ed25519:" + raw, PublicKey: raw}
}

// MasterKey builds a user's master key: self-signed, and additionally signed by the device that
// is uploading it, which is what the Rust SDK produces.
func MasterKey(t *testing.T, userID string, master, device *SigningKey) json.RawMessage {
	t.Helper()
	document := map[string]any{
		"user_id": userID,
		"usage":   []string{"master"},
		"keys":    map[string]string{master.ID: master.PublicKey},
	}
	signed := sign(t, userID, master, document)
	if device != nil {
		signed = addSignature(t, signed, userID, device)
	}
	return signed
}

// SelfSigningKey builds the key a user signs their own devices with. It has to be signed by the
// master key, which is the whole point of it.
func SelfSigningKey(t *testing.T, userID string, master, self *SigningKey) json.RawMessage {
	t.Helper()
	return sign(t, userID, master, map[string]any{
		"user_id": userID,
		"usage":   []string{"self_signing"},
		"keys":    map[string]string{self.ID: self.PublicKey},
	})
}

// UserSigningKey builds the key a user signs *other people's* master keys with.
func UserSigningKey(t *testing.T, userID string, master, user *SigningKey) json.RawMessage {
	t.Helper()
	return sign(t, userID, master, map[string]any{
		"user_id": userID,
		"usage":   []string{"user_signing"},
		"keys":    map[string]string{user.ID: user.PublicKey},
	})
}

// SignedByKey builds a cross-signing-shaped document signed by `signer` but carrying `signer`'s
// own public key. It exists to produce the documents that must be refused: an unsigned key, a key
// signed by the wrong master key, a key whose signature does not cover what it claims to.
func SignedByKey(t *testing.T, userID string, signer *SigningKey, usage string, key *SigningKey) json.RawMessage {
	t.Helper()
	return sign(t, userID, signer, map[string]any{
		"user_id": userID,
		"usage":   []string{usage},
		"keys":    map[string]string{key.ID: key.PublicKey},
	})
}

// UnsignedDocument is a key document with no signature on it at all.
func UnsignedDocument(t *testing.T, userID string, key *SigningKey) json.RawMessage {
	t.Helper()
	return mustJSON(t, map[string]any{
		"user_id": userID,
		"usage":   []string{"master"},
		"keys":    map[string]string{key.ID: key.PublicKey},
	})
}

// DeviceKeyDocument builds a device key document carrying `signer`'s signature over it, which is
// what a client posts to `/keys/signatures/upload` after it has signed its own device with its
// self-signing key.
func DeviceKeyDocument(t *testing.T, userID, deviceID string, signer *SigningKey) map[string]any {
	t.Helper()
	document := map[string]any{
		"user_id":    userID,
		"device_id":  deviceID,
		"algorithms": []string{"m.olm.v1.curve25519-aes-sha2", "m.megolm.v1.aes-sha2"},
		"keys": map[string]string{
			"curve25519:" + deviceID: "EkHGWHinmF1EhRsmETGErH+aGK1E4Fg4Z4X1rEUGlEA",
			"ed25519:" + deviceID:    "Ksh1HbVLdrmBcGwNlBAVfaXZLYeyLhFzDqjL1IKmEt8",
		},
	}
	if signer == nil {
		document["signatures"] = map[string]any{}
		return document
	}
	signed := sign(t, userID, signer, document)
	var out map[string]any
	if err := json.Unmarshal(signed, &out); err != nil {
		t.Fatalf("decoding a signed device document: %v", err)
	}
	return out
}

// sign canonicalises the document, signs it, and returns it with `signatures` added.
func sign(t *testing.T, userID string, signer *SigningKey, document map[string]any) json.RawMessage {
	t.Helper()
	signed, err := gomatrixserverlib.SignJSON(userID, gomatrixserverlib.KeyID(signer.ID),
		signer.Private, mustJSON(t, document))
	if err != nil {
		t.Fatalf("signing with %s: %v", signer.ID, err)
	}
	return json.RawMessage(signed)
}

// addSignature adds a second signature over an already-signed document.
func addSignature(t *testing.T, document json.RawMessage, userID string, signer *SigningKey) json.RawMessage {
	t.Helper()
	signed, err := gomatrixserverlib.SignJSON(userID, gomatrixserverlib.KeyID(signer.ID),
		signer.Private, document)
	if err != nil {
		t.Fatalf("adding a signature from %s: %v", signer.ID, err)
	}
	return json.RawMessage(signed)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %v: %v", value, err)
	}
	return encoded
}

// KeyDocumentWithID builds a master-key document whose key identifier is chosen by the caller, with
// no signature on it. It exists for the case where the identifier itself is the problem: a
// cross-signing key named after one of the user's devices.
func KeyDocumentWithID(t *testing.T, userID, keyID string) json.RawMessage {
	t.Helper()
	publicKey := strings.TrimPrefix(keyID, "ed25519:")
	return mustJSON(t, map[string]any{
		"user_id": userID,
		"usage":   []string{"master"},
		"keys":    map[string]string{keyID: publicKey},
	})
}

// SignAnotherUsersMasterKey signs `target`'s master key with the caller's user-signing key, which is
// what "I checked this person" looks like on the wire. The document is the target's key as published
// plus one signature made by somebody else — so the signing name is the *caller's* user ID, not the
// target's, which is the one place in the protocol where a key carries a signature from a name that
// does not own it.
func SignAnotherUsersMasterKey(t *testing.T, targetUserID string, targetMaster *SigningKey,
	callerUserID string, callerUserSigning *SigningKey) json.RawMessage {
	t.Helper()
	document := map[string]any{
		"user_id": targetUserID,
		"usage":   []string{"master"},
		"keys":    map[string]string{targetMaster.ID: targetMaster.PublicKey},
	}
	signed, err := gomatrixserverlib.SignJSON(callerUserID, gomatrixserverlib.KeyID(callerUserSigning.ID),
		callerUserSigning.Private, mustJSON(t, document))
	if err != nil {
		t.Fatalf("signing %s's master key as %s: %v", targetUserID, callerUserID, err)
	}
	return json.RawMessage(signed)
}
