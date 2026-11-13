// Package appservice reads the application-service registrations this homeserver has been told to
// trust, and answers the two questions the rest of the server asks about them: which service owns
// this access token, and may that service act as this user.
//
// An application service is a program the server delegates a slice of its user namespace to — a
// bridge to another network, an importer, a bot with the right to send as other people. The
// delegation is the entire security story: the registration file says who the service may be, and a
// service that could act as anybody is a way to read any room. So the namespace match is anchored
// rather than substring-matched, and a registration that cannot be understood is a start-up error
// rather than something to discover at the first request.
//
// The file format is Synapse's because that is what every existing registration is written in, and
// what complement copies into the container at /complement/appservice/<id>.yaml. Unknown keys are
// tolerated: services carry MSC-prefixed keys that this server has no opinion about, and refusing a
// file because it mentions one would be a way to make this server unusable with working bridges.
package appservice

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Namespace is one entry of a service's user, room or alias namespace.
type Namespace struct {
	// Exclusive says this service is the only one allowed to claim the match. It widens nothing:
	// a match grants the service the right either way, and exclusivity restricts everybody else.
	Exclusive bool `yaml:"exclusive"`
	// Regex is the pattern, matched against a full user ID, room ID or alias.
	Regex string `yaml:"regex"`
}

// Namespaces is the delegation itself.
type Namespaces struct {
	Users   []Namespace `yaml:"users"`
	Rooms   []Namespace `yaml:"rooms"`
	Aliases []Namespace `yaml:"aliases"`
}

// Registration is one application service.
type Registration struct {
	ID              string     `yaml:"id"`
	HSToken         string     `yaml:"hs_token"`
	ASToken         string     `yaml:"as_token"`
	URL             string     `yaml:"url"`
	SenderLocalpart string     `yaml:"sender_localpart"`
	RateLimited     *bool      `yaml:"rate_limited"`
	Namespaces      Namespaces `yaml:"namespaces"`

	// compiled mirrors Namespaces, with each pattern anchored to the whole identifier. Matching
	// the raw pattern is how a namespace for "alice" would otherwise cover "alice-evil".
	compiledUsers   []*regexp.Regexp
	compiledRooms   []*regexp.Regexp
	compiledAliases []*regexp.Regexp
}

// UserID is the account this service sends its own events as.
func (r *Registration) UserID(serverName string) string {
	return "@" + r.SenderLocalpart + ":" + serverName
}

// AllowsUser reports whether this service may act as the given user — either as its own sender, or
// as anybody inside a user namespace it was given.
func (r *Registration) AllowsUser(userID string) bool {
	for _, pattern := range r.compiledUsers {
		if pattern.MatchString(userID) {
			return true
		}
	}
	return false
}

// ManagesRoom reports whether a room ID is inside the service's room namespace, which is what an
// output bridge checks before claiming a room it created.
func (r *Registration) ManagesRoom(roomID string) bool {
	for _, pattern := range r.compiledRooms {
		if pattern.MatchString(roomID) {
			return true
		}
	}
	return false
}

// ManagesAlias reports whether an alias is inside the service's alias namespace.
func (r *Registration) ManagesAlias(alias string) bool {
	for _, pattern := range r.compiledAliases {
		if pattern.MatchString(alias) {
			return true
		}
	}
	return false
}

// Registry is the set of services this server trusts, indexed by the token they present.
type Registry struct {
	byToken map[string]*Registration
	all     []*Registration
}

// Load reads every configured path. A path may be a glob, because the files are usually dropped into
// a directory by whatever provisions the bridges and nobody wants to redeploy a server to add one.
//
// A file that cannot be read, parsed, or understood fails the load: a registration that is silently
// missing is a service that cannot connect, and a registration that is silently *wrong* is worse —
// it is a grant nobody read.
func Load(patterns []string, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	registry := &Registry{byToken: map[string]*Registration{}}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		paths, err := expand(pattern)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			registration, err := parse(path)
			if err != nil {
				return nil, err
			}
			if existing, taken := registry.byToken[registration.ASToken]; taken {
				// Two services sharing a token means one of them can act with the other's
				// namespace, which neither operator agreed to.
				return nil, fmt.Errorf("application service %s: as_token is also used by %s", path, existing.ID)
			}
			registry.all = append(registry.all, registration)
			registry.byToken[registration.ASToken] = registration
			log.Info("loaded application service registration",
				"id", registration.ID, "file", path,
				"sender", registration.SenderLocalpart, "namespaces", len(registration.compiledUsers))
		}
	}
	return registry, nil
}

// expand turns a configured path into the files it names. A pattern with no glob characters has to
// exist — a typo in a filename should be an error at start-up, not an empty trust list forever.
func expand(pattern string) ([]string, error) {
	if !strings.ContainsAny(pattern, "*?[") {
		if _, err := os.Stat(pattern); err != nil {
			return nil, fmt.Errorf("application service configuration %s: %w", pattern, err)
		}
		return []string{pattern}, nil
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("application service configuration pattern %s: %w", pattern, err)
	}
	return matches, nil
}

func parse(path string) (*Registration, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading application service registration: %w", err)
	}
	registration := &Registration{}
	if err := yaml.Unmarshal(body, registration); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if registration.ID == "" {
		return nil, fmt.Errorf("%s: id is required", path)
	}
	if registration.ASToken == "" {
		return nil, fmt.Errorf("%s: as_token is required — without it the service cannot authenticate", path)
	}
	if registration.SenderLocalpart == "" {
		return nil, fmt.Errorf("%s: sender_localpart is required — the service needs a user to send as", path)
	}
	if strings.ContainsAny(registration.SenderLocalpart, "@:") {
		return nil, fmt.Errorf("%s: sender_localpart must be a local part, not a user ID", path)
	}
	if err := compile(&registration.compiledUsers, registration.Namespaces.Users, path, "users"); err != nil {
		return nil, err
	}
	if err := compile(&registration.compiledRooms, registration.Namespaces.Rooms, path, "rooms"); err != nil {
		return nil, err
	}
	if err := compile(&registration.compiledAliases, registration.Namespaces.Aliases, path, "aliases"); err != nil {
		return nil, err
	}
	return registration, nil
}

func compile(dst *[]*regexp.Regexp, namespaces []Namespace, path, which string) error {
	for _, namespace := range namespaces {
		pattern := namespace.Regex
		if pattern == "" {
			continue
		}
		anchored := pattern
		if !strings.HasPrefix(anchored, "^") {
			anchored = "^" + anchored
		}
		if !strings.HasSuffix(anchored, "$") {
			anchored = anchored + "$"
		}
		compiled, err := regexp.Compile(anchored)
		if err != nil {
			return fmt.Errorf("%s: %s namespace %q is not a valid pattern: %w", path, which, pattern, err)
		}
		*dst = append(*dst, compiled)
	}
	return nil
}

// LookupByToken finds the service whose as_token is this string.
func (r *Registry) LookupByToken(token string) (*Registration, bool) {
	if r == nil || token == "" {
		return nil, false
	}
	registration, ok := r.byToken[token]
	return registration, ok
}

// Len is how many services are trusted, which is what the start-up log and a health check want.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.all)
}

// Registrations lists the loaded services in configuration order.
func (r *Registry) Registrations() []*Registration {
	if r == nil {
		return nil
	}
	return r.all
}
