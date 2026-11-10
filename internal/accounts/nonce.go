package accounts

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// NonceValidity is how long a shared-secret registration nonce stays usable. Five minutes
// is generous for a registration script and short enough that a leaked nonce is not a
// standing invitation.
const NonceValidity = 5 * time.Minute

// ErrUnknownNonce is returned when a nonce was never issued or has been used.
var ErrUnknownNonce = matrix.NewAPIError(http.StatusUnauthorized, matrix.ErrUnauthorized,
	"Auth version mismatch or invalid nonce")

// NonceStore issues and redeems shared-secret registration nonces.
//
// A nonce is single-use by construction: redeeming it removes it. Without that, a captured
// (nonce, mac) pair would be replayable for as long as the nonce lived, and the mac for a
// given user and password does not depend on the server's state.
type NonceStore struct {
	mu       sync.Mutex
	nonces   map[string]time.Time
	validity time.Duration
	now      func() time.Time
}

// NewNonceStore returns a store with the default validity.
func NewNonceStore() *NonceStore {
	return &NonceStore{
		nonces:   make(map[string]time.Time),
		validity: NonceValidity,
		now:      time.Now,
	}
}

// Issue returns a fresh nonce.
func (s *NonceStore) Issue() (string, error) {
	// 18 random bytes, base64'd: the nonce travels in JSON and in a shell argument, so it
	// must survive both without escaping games.
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating registration nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.collectLocked()
	s.nonces[nonce] = s.now().Add(s.validity)
	return nonce, nil
}

// Consume redeems a nonce, failing if it is unknown, expired or already used.
func (s *NonceStore) Consume(nonce string) error {
	if nonce == "" {
		return ErrUnknownNonce
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collectLocked()

	expires, ok := s.nonces[nonce]
	if !ok || s.now().After(expires) {
		return ErrUnknownNonce
	}
	delete(s.nonces, nonce)
	return nil
}

// collectLocked drops expired nonces. The caller holds the lock.
func (s *NonceStore) collectLocked() {
	now := s.now()
	for nonce, expires := range s.nonces {
		if now.After(expires) {
			delete(s.nonces, nonce)
		}
	}
}

// ErrNotImplemented marks a homeserver feature that is configured for but not built.
var ErrNotImplemented = errors.New("accounts: not implemented")
