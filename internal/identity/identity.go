// Package identity owns the homeserver's own cryptographic identity: the ed25519 key pair
// it signs events and federation requests with, and the key validity periods it publishes
// to peers.
//
// The key is created on first start and stored beside the database. Losing it is as
// serious as losing the database: every event this server signed becomes unverifiable,
// which is why the file is written with 0600 and why a corrupt file is a fatal start-up
// error rather than a reason to generate a new key silently.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// KeyValidity is how far ahead a published signing key stays valid. Seven days matches
// Synapse: long enough that a peer's cached copy survives a weekend partition, short enough
// that a rotated key stops being trusted quickly.
const KeyValidity = 7 * 24 * time.Hour

// Identity is the server's signing identity.
type Identity struct {
	// ServerName is the Matrix name this identity speaks for.
	ServerName spec.ServerName
	// KeyID is the signing key identifier, e.g. "ed25519:aBcDe".
	KeyID gomatrixserverlib.KeyID
	// PrivateKey is the ed25519 key pair.
	PrivateKey ed25519.PrivateKey

	// OldKeys are retired key pairs that must still be published so that events signed
	// with them remain verifiable. They are kept for the whole validity window of every
	// event they signed, which for a local event is forever in practice, so they are never
	// dropped.
	oldKeys map[gomatrixserverlib.KeyID]ed25519.PublicKey

	// now is a seam for tests that need deterministic validity windows.
	now func() time.Time

	mu sync.RWMutex
}

// persistedKey is the on-disk shape. Field names are stable because operators back the file
// up and because a key that cannot be read back is a lost identity.
type persistedKey struct {
	ServerName string            `json:"server_name"`
	KeyID      string            `json:"key_id"`
	PrivateKey string            `json:"private_key"` // base64 (64-byte seed or full key)
	OldKeys    map[string]string `json:"old_keys,omitempty"`
}

// Load reads the identity from path, creating and saving a new key pair when the file does
// not exist yet.
func Load(path string, serverName string) (*Identity, error) {
	if path == "" {
		return nil, errors.New("identity: key file path must not be empty")
	}
	if serverName == "" {
		return nil, errors.New("identity: server name must not be empty")
	}

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return createAndSave(path, serverName)
	case err != nil:
		return nil, fmt.Errorf("reading signing key %q: %w", path, err)
	}

	identity, err := parse(raw, serverName)
	if err != nil {
		return nil, fmt.Errorf("signing key %q is unusable: %w", path, err)
	}
	return identity, nil
}

// parse decodes a persisted key, tolerating the two encodings of a Go ed25519 private key
// (the 32-byte seed and the 64-byte expanded key) because both appear in the wild.
func parse(raw []byte, serverName string) (*Identity, error) {
	var stored persistedKey
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("decoding signing key: %w", err)
	}
	if stored.KeyID == "" || stored.PrivateKey == "" {
		return nil, errors.New("signing key file is missing key_id or private_key")
	}
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(stored.PrivateKey))
	if err != nil {
		// Accept padded encoding too; the point is to never refuse a real key because of
		// an encoding detail.
		decoded, err = base64.StdEncoding.DecodeString(strings.TrimSpace(stored.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("decoding the private key: %w", err)
		}
	}
	var privateKey ed25519.PrivateKey
	switch len(decoded) {
	case ed25519.SeedSize:
		privateKey = ed25519.NewKeyFromSeed(decoded)
	case ed25519.PrivateKeySize:
		privateKey = ed25519.PrivateKey(decoded)
	default:
		return nil, fmt.Errorf("private key has length %d, want %d or %d bytes",
			len(decoded), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
	if !strings.HasPrefix(stored.KeyID, "ed25519:") {
		return nil, fmt.Errorf("key ID %q is not of the form ed25519:<version>", stored.KeyID)
	}

	identity := &Identity{
		ServerName: spec.ServerName(serverName),
		KeyID:      gomatrixserverlib.KeyID(stored.KeyID),
		PrivateKey: privateKey,
		oldKeys:    map[gomatrixserverlib.KeyID]ed25519.PublicKey{},
		now:        time.Now,
	}
	for keyID, encoded := range stored.OldKeys {
		key, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil || len(key) != ed25519.PublicKeySize {
			// An unreadable old key is a publishing problem, not a reason to refuse to
			// start: log it and continue without it.
			slog.Warn("ignoring an unreadable old signing key", "key_id", keyID, "path_hint", "old_keys")
			continue
		}
		identity.oldKeys[gomatrixserverlib.KeyID(keyID)] = ed25519.PublicKey(key)
	}
	return identity, nil
}

// createAndSave generates the first key pair for this server.
func createAndSave(path, serverName string) (*Identity, error) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating a signing key: %w", err)
	}
	identity := &Identity{
		ServerName: spec.ServerName(serverName),
		// The key version is the random part of the key ID, per the specification's
		// "ed25519:<key_version>" form.
		KeyID:      gomatrixserverlib.KeyID("ed25519:" + matrix.RandomID(5)),
		PrivateKey: private,
		oldKeys:    map[gomatrixserverlib.KeyID]ed25519.PublicKey{},
		now:        time.Now,
	}
	_ = pub // the public half is derived from the private key

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil && filepath.Dir(path) != "" {
		return nil, fmt.Errorf("creating the directory for the signing key: %w", err)
	}
	if err := identity.Save(path); err != nil {
		return nil, err
	}
	slog.Info("created a new signing key", "key_id", identity.KeyID, "file", path)
	return identity, nil
}

// Save writes the identity to path atomically, so a crash during start-up cannot leave a
// half-written key behind.
func (i *Identity) Save(path string) error {
	i.mu.RLock()
	old := make(map[string]string, len(i.oldKeys))
	for keyID, key := range i.oldKeys {
		old[string(keyID)] = base64.RawStdEncoding.EncodeToString(key)
	}
	stored := persistedKey{
		ServerName: string(i.ServerName),
		KeyID:      string(i.KeyID),
		// The seed is written rather than the expanded key: it is half the size and
		// unambiguous.
		PrivateKey: base64.RawStdEncoding.EncodeToString(i.PrivateKey.Seed()),
		OldKeys:    old,
	}
	i.mu.RUnlock()

	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the signing key: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return fmt.Errorf("writing the signing key: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replacing the signing key file: %w", err)
	}
	return nil
}

// PublicKey returns the ed25519 public half of the current signing key.
func (i *Identity) PublicKey() ed25519.PublicKey {
	return i.PrivateKey.Public().(ed25519.PublicKey)
}

// NotaryKey builds the `verify_keys` entry for this identity's current key, base64-encoded
// as the key server API requires.
func (i *Identity) NotaryKey() (keyID gomatrixserverlib.KeyID, key gomatrixserverlib.VerifyKey) {
	return i.KeyID, gomatrixserverlib.VerifyKey{Key: spec.Base64Bytes(i.PublicKey())}
}

// ValidUntil is when this identity's published key stops being valid. The window is
// published so peers can cache it; it keeps advancing rather than expiring, because events
// this server signed years ago must remain verifiable.
func (i *Identity) ValidUntil() spec.Timestamp {
	return spec.AsTimestamp(i.now().Add(KeyValidity))
}

// SignJSON signs an arbitrary JSON document (an event, a server-key response, a federation
// query) with the current key under the current key ID.
func (i *Identity) SignJSON(document []byte) ([]byte, error) {
	signed, err := gomatrixserverlib.SignJSON(string(i.ServerName), i.KeyID, i.PrivateKey, document)
	if err != nil {
		return nil, fmt.Errorf("signing JSON as %s: %w", i.ServerName, err)
	}
	return signed, nil
}
