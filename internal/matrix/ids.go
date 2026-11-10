package matrix

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"sync"

	"github.com/matrix-org/gomatrixserverlib/spec"
)

// Localpart rules. The specification permits a smaller set than Synapse historically
// accepted; qwatrix writes down exactly what the spec allows and nothing else, so that
// user IDs it mints can be read by every other implementation.
const (
	// MaxLocalpartLength is the specification's 255-character limit, counted in
	// characters, not bytes.
	MaxLocalpartLength = 255
	// MinLocalpartLength is 1: an empty localpart is not an identity.
	MinLocalpartLength = 1
)

// localpartRegexp is the grammar the specification allows in a user ID local part:
// lowercase letters, digits, and "._=-/+". Underscore-prefixed names are legal (they are
// how historical and application-service-managed IDs are written), so they are not
// rejected here.
var localpartRegexp = regexp.MustCompile(`^[a-z0-9._=\-/+]+$`)

// ValidLocalpart reports whether localpart may be used as the local part of a user ID on
// this server.
func ValidLocalpart(localpart string) bool {
	if len(localpart) < MinLocalpartLength || len(localpart) > MaxLocalpartLength {
		return false
	}
	// ":" and "@" cannot appear: they are the delimiters of the grammar, and a local part
	// carrying one would be a different user ID once somebody parsed it.
	if strings.ContainsAny(localpart, ":@") {
		return false
	}
	return localpartRegexp.MatchString(localpart)
}

// UserID builds the canonical user ID for a localpart on a server.
func UserID(localpart, serverName string) string {
	return "@" + localpart + ":" + serverName
}

// ParseUserID validates a user ID and returns its localpart and domain.
func ParseUserID(id, serverName string) (localpart, domain string, err error) {
	parsed, err := spec.NewUserID(id, true)
	if err != nil {
		return "", "", fmt.Errorf("user ID %q: %w", id, err)
	}
	return parsed.Local(), string(parsed.Domain()), nil
}

// ParseLocalpartOrUserID accepts either a bare localpart or a full user ID and returns the
// localpart plus the domain it belongs to (which the caller must compare against its own
// server name before treating the user as local).
func ParseLocalpartOrUserID(value, serverName string) (localpart, domain string, err error) {
	if !strings.HasPrefix(value, "@") {
		if !strings.Contains(value, ":") {
			return value, serverName, nil
		}
		return "", "", fmt.Errorf("%q is not a valid user ID", value)
	}
	return ParseUserID(value, serverName)
}

// RoomID builds a room ID from the opaque part generated at creation and the server that
// created it.
func RoomID(opaque, serverName string) string {
	return "!" + opaque + ":" + serverName
}

// ParseRoomID validates a room ID and returns its opaque part and domain.
func ParseRoomID(id string) (opaque, domain string, err error) {
	parsed, err := spec.NewRoomID(id)
	if err != nil {
		return "", "", fmt.Errorf("room ID %q: %w", id, err)
	}
	return parsed.OpaqueID(), string(parsed.Domain()), nil
}

// Alias builds a room alias from a local part and server.
func Alias(localpart, serverName string) string {
	if !strings.HasPrefix(localpart, "#") {
		return "#" + localpart + ":" + serverName
	}
	return localpart + ":" + serverName
}

// ParseAlias splits "#local:domain".
func ParseAlias(alias string) (localpart, domain string, err error) {
	if !strings.HasPrefix(alias, "#") {
		return "", "", fmt.Errorf("alias %q must start with '#'", alias)
	}
	body := strings.TrimPrefix(alias, "#")
	name, host, found := strings.Cut(body, ":")
	if !found || name == "" || host == "" {
		return "", "", fmt.Errorf("alias %q is not of the form #local:server", alias)
	}
	return name, host, nil
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

var (
	idRand   = rand.New(rand.NewSource(timeNowUnixNano()))
	idRandMu sync.Mutex
)

// RandomID returns n characters from the URL-safe alphabet used for opaque Matrix
// identifiers (rooms, events, media, devices).
func RandomID(n int) string {
	idRandMu.Lock()
	defer idRandMu.Unlock()
	out := make([]byte, n)
	for i := range out {
		out[i] = idAlphabet[idRand.Intn(len(idAlphabet))]
	}
	return string(out)
}

// RandomOpaqueID returns a 32-character identifier, matching the length Dendrite and
// Synapse use for room and media identifiers.
func RandomOpaqueID() string { return RandomID(32) }

// EventID returns an opaque ("$" prefixed) event ID, the format used by room versions 2
// and later. Domain-less event IDs are what the current room versions write, and GMSL's
// event builder produces the hashes for them.
func EventID() string {
	return "$" + RandomID(22)
}

// DeviceID returns a new device identifier.
func DeviceID() string { return RandomID(10) }

// AccessToken returns a new opaque access token. Tokens are prefixed so that a token
// found in a log or a config file is recognisable as a qwatrix token.
func AccessToken() string {
	return "qw_" + RandomID(48)
}

// Nonce returns a value for the interactive-auth / shared-secret registration nonce.
func Nonce() string { return RandomID(32) }
