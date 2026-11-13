// Package config holds the runtime configuration of the homeserver.
//
// Nothing operational is hard-coded: every value a operator or a test harness needs to
// change (identity, listen addresses, database location, registration secret, room version
// default, limits) is a field here, filled from command-line flags and/or environment
// variables by [Config.ApplyFlags] and [Config.ApplyEnv].
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is prepended to every environment variable read by [Config.ApplyEnv], so that
// a container orchestrator's environment does not collide with unrelated variables.
const EnvPrefix = "QWATRIX_"

// Config is the complete set of knobs the homeserver reads at start-up.
type Config struct {
	// ServerName is the Matrix identity of this homeserver (the part after the colon in
	// user IDs). It is not necessarily the DNS name: federation resolution may point
	// elsewhere. Required.
	ServerName string
	// ClientAddr is the address the client-server API listens on (plain HTTP).
	ClientAddr string
	// FederationAddr is the address the server-server API listens on (HTTPS). Empty
	// disables the federation listener.
	FederationAddr string
	// TLSCertFile and TLSKeyFile are the federation listener's certificate and key. When
	// empty and federation is enabled the server generates a self-signed pair in DataDir.
	TLSCertFile string
	TLSKeyFile  string
	// FederationInsecureSkipTLSVerify turns off peer certificate verification for federation.
	//
	// It removes one of the two anchors federation rests on. A peer's signing key is published by
	// the peer itself, so the TLS certificate on that connection is the only thing distinguishing
	// "here is my key" from "here is a key of the attacker's choosing". Turning this off is
	// therefore a deliberate act, meant for a test network where peers present self-signed
	// certificates that no deployment could have been told about in advance; the server says so in
	// the log when it starts.
	FederationInsecureSkipTLSVerify bool
	// TrustedCACertFile is an additional CA to trust for outbound federation (used by
	// test environments that mint their own certificates).
	TrustedCACertFile string

	// DataDir is where the server keeps everything it owns: database, media, keys.
	DataDir string
	// DatabasePath is the SQLite file. Empty derives it from DataDir.
	DatabasePath string

	// RegistrationSecret enables Synapse-compatible shared-secret registration
	// (GET/POST /_synapse/admin/v1/register) with this secret. Complement requires the
	// value "complement". Empty disables the endpoint.
	RegistrationSecret string
	// EnableRegistration enables open registration via POST /register.
	EnableRegistration bool
	// EnableRegistrationRequiresGuest is unused placeholder for future UIA guest flows.

	// AppServiceConfigFiles lists the application-service registration files this server trusts,
	// separated by commas. Each entry may be a glob, because bridges are usually provisioned by
	// dropping a file into a directory rather than by editing a command line.
	//
	// An application service is a delegation of trust: it may act as every user named in the
	// namespaces its file carries. With no files configured there is no delegation at all, and an
	// as_token is just an unknown token.
	AppServiceConfigFiles string

	// DefaultRoomVersion is used when POST /createRoom does not name one.
	DefaultRoomVersion string
	// UnsupportedRoomVersions are room versions to omit from /capabilities.

	// MediaSizeLimitBytes caps one uploaded file.
	MediaSizeLimitBytes int64
	// ThumbnailsEnabled turns on on-the-fly thumbnail generation.
	ThumbnailsEnabled bool

	// RateLimitEnabled turns rate limiting on. It is on by default; tests that need
	// deterministic bursts of requests may disable it.
	RateLimitEnabled bool
	// RateLimitBurstsPerSecond and RateLimitBurst configure the token buckets.
	RateLimitBurstsPerSecond float64
	RateLimitBurst           int

	// SyncTimeoutMax is the longest a client may ask /sync to wait.
	SyncTimeoutMax time.Duration
	// SyncDefaultTimelinelimit is the default timeline batch size.
	SyncDefaultTimelineLimit int

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// Version is reported by /_matrix/federation/v1/version and the Server header.
	Version string
}

// Default returns a configuration that is valid for development on a loopback interface.
//
// A homeserver whose identity is not set is not a homeserver anyone can register on, so
// ServerName is deliberately filled with the conventional development value rather than
// left blank; everything else is chosen so that `qwatrix` with no arguments works.
func Default() *Config {
	return &Config{
		ServerName:               "localhost:8008",
		ClientAddr:               "127.0.0.1:8008",
		FederationAddr:           "",
		DataDir:                  ".",
		DatabasePath:             "",
		EnableRegistration:       true,
		DefaultRoomVersion:       "11",
		MediaSizeLimitBytes:      100 << 20,
		ThumbnailsEnabled:        true,
		RateLimitEnabled:         true,
		RateLimitBurstsPerSecond: 20,
		RateLimitBurst:           60,
		SyncTimeoutMax:           60 * time.Second,
		SyncDefaultTimelineLimit: 10,
		LogLevel:                 "info",
		Version:                  "0.1.0-dev",
	}
}

// AppServiceConfigPatterns splits the configured registration files into the patterns to expand.
func (c *Config) AppServiceConfigPatterns() []string {
	if c == nil {
		return nil
	}
	var patterns []string
	for _, pattern := range strings.Split(c.AppServiceConfigFiles, ",") {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

// ApplyFlags binds every setting to a flag on fs, using the same names as the environment
// variables in [Config.ApplyEnv] without the prefix.
func (c *Config) ApplyFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.ServerName, "server-name", c.ServerName, "Matrix server name this homeserver answers for")
	fs.StringVar(&c.ClientAddr, "addr", c.ClientAddr, "address the client-server API listens on")
	fs.StringVar(&c.FederationAddr, "federation-addr", c.FederationAddr, "address the server-server API listens on (empty disables federation)")
	fs.StringVar(&c.TLSCertFile, "tls-cert", c.TLSCertFile, "PEM certificate for the federation listener")
	fs.StringVar(&c.TLSKeyFile, "tls-key", c.TLSKeyFile, "PEM key for the federation listener")
	fs.StringVar(&c.TrustedCACertFile, "ca-cert", c.TrustedCACertFile, "additional CA certificate to trust when federating")
	fs.BoolVar(&c.FederationInsecureSkipTLSVerify, "federation-insecure-skip-tls-verify", c.FederationInsecureSkipTLSVerify,
		"do not verify peer TLS certificates when federating (test networks only; this removes a security boundary)")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "directory for the database, media and signing keys")
	fs.StringVar(&c.DatabasePath, "db", c.DatabasePath, "SQLite database path (defaults to <data-dir>/qwatrix.db)")
	fs.StringVar(&c.RegistrationSecret, "registration-secret", c.RegistrationSecret, "shared secret for /_synapse/admin/v1/register (empty disables)")
	fs.BoolVar(&c.EnableRegistration, "registration", c.EnableRegistration, "allow POST /register")
	fs.StringVar(&c.AppServiceConfigFiles, "app-service-config-files", c.AppServiceConfigFiles,
		"comma-separated application service registration files (globs allowed)")
	fs.StringVar(&c.DefaultRoomVersion, "room-version", c.DefaultRoomVersion, "default room version for POST /createRoom")
	fs.Int64Var(&c.MediaSizeLimitBytes, "media-limit", c.MediaSizeLimitBytes, "maximum size in bytes of one uploaded file")
	fs.BoolVar(&c.ThumbnailsEnabled, "thumbnails", c.ThumbnailsEnabled, "enable thumbnail generation")
	fs.BoolVar(&c.RateLimitEnabled, "rate-limit", c.RateLimitEnabled, "enable rate limiting")
	fs.Float64Var(&c.RateLimitBurstsPerSecond, "rate-limit-rate", c.RateLimitBurstsPerSecond, "sustained requests per second per subject")
	fs.IntVar(&c.RateLimitBurst, "rate-limit-burst", c.RateLimitBurst, "burst size for a rate-limited subject")
	fs.DurationVar(&c.SyncTimeoutMax, "sync-max-timeout", c.SyncTimeoutMax, "longest a client may ask /sync to long-poll")
	fs.IntVar(&c.SyncDefaultTimelineLimit, "sync-timeline-limit", c.SyncDefaultTimelineLimit, "default /sync timeline batch size")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "log level: debug, info, warn or error")
	fs.StringVar(&c.Version, "version", c.Version, "version string reported to clients and peers")
}

// ApplyEnv reads QWATRIX_* environment variables over the current values. Flags take
// precedence over the environment, so callers apply the environment first.
func (c *Config) ApplyEnv(getenv func(string) (string, bool)) error {
	if getenv == nil {
		getenv = os.LookupEnv
	}
	str := func(name string, dst *string) {
		if v, ok := getenv(EnvPrefix + name); ok {
			*dst = v
		}
	}
	boolean := func(name string, dst *bool) error {
		v, ok := getenv(EnvPrefix + name)
		if !ok {
			return nil
		}
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("environment variable %s%s: %w", EnvPrefix, name, err)
		}
		*dst = parsed
		return nil
	}
	integer := func(name string, dst *int64) error {
		v, ok := getenv(EnvPrefix + name)
		if !ok {
			return nil
		}
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("environment variable %s%s: %w", EnvPrefix, name, err)
		}
		*dst = parsed
		return nil
	}

	str("SERVER_NAME", &c.ServerName)
	str("APP_SERVICE_CONFIG_FILES", &c.AppServiceConfigFiles)
	str("ADDR", &c.ClientAddr)
	str("FEDERATION_ADDR", &c.FederationAddr)
	str("TLS_CERT", &c.TLSCertFile)
	str("TLS_KEY", &c.TLSKeyFile)
	str("CA_CERT", &c.TrustedCACertFile)
	if err := boolean("FEDERATION_INSECURE_SKIP_TLS_VERIFY", &c.FederationInsecureSkipTLSVerify); err != nil {
		return err
	}
	str("DATA_DIR", &c.DataDir)
	str("DB", &c.DatabasePath)
	str("REGISTRATION_SECRET", &c.RegistrationSecret)
	str("ROOM_VERSION", &c.DefaultRoomVersion)
	str("LOG_LEVEL", &c.LogLevel)
	str("VERSION", &c.Version)
	if err := boolean("REGISTRATION", &c.EnableRegistration); err != nil {
		return err
	}
	if err := boolean("THUMBNAILS", &c.ThumbnailsEnabled); err != nil {
		return err
	}
	if err := boolean("RATE_LIMIT", &c.RateLimitEnabled); err != nil {
		return err
	}
	if err := integer("MEDIA_LIMIT", &c.MediaSizeLimitBytes); err != nil {
		return err
	}
	return nil
}

// DatabaseFile returns the effective database path, derived from DataDir when unset.
func (c *Config) DatabaseFile() string {
	if c.DatabasePath != "" {
		return c.DatabasePath
	}
	return filepath.Join(c.DataDir, "qwatrix.db")
}

// MediaDir returns the directory uploaded content is stored in.
func (c *Config) MediaDir() string {
	return filepath.Join(c.DataDir, "media")
}

// SigningKeyFile returns the file holding the server's federation signing key.
func (c *Config) SigningKeyFile() string {
	return filepath.Join(c.DataDir, "signing.json")
}

// Validate reports the first reason the configuration cannot be used.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.ServerName) == "" {
		return fmt.Errorf("server name must not be empty")
	}
	if strings.ContainsAny(c.ServerName, " /") || strings.HasPrefix(c.ServerName, "@") {
		return fmt.Errorf("server name %q is not a valid Matrix server name", c.ServerName)
	}
	if strings.TrimSpace(c.ClientAddr) == "" {
		return fmt.Errorf("client address must not be empty")
	}
	if c.DefaultRoomVersion == "" {
		return fmt.Errorf("default room version must not be empty")
	}
	if c.SyncDefaultTimelineLimit <= 0 {
		return fmt.Errorf("sync timeline limit must be positive, got %d", c.SyncDefaultTimelineLimit)
	}
	if c.MediaSizeLimitBytes <= 0 {
		return fmt.Errorf("media size limit must be positive, got %d", c.MediaSizeLimitBytes)
	}
	if c.FederationAddr != "" {
		if c.FederationAddr == c.ClientAddr {
			return fmt.Errorf("federation address must differ from the client address")
		}
		// The server-server API answers for the identity in ServerName, so a server that
		// federates without one would publish an unnameable signing key.
		if c.ServerName == "" {
			return fmt.Errorf("a server-server address was configured but no server name was")
		}
	}
	return nil
}
