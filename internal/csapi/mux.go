// Package csapi implements the Matrix client-server API: one file per specification area,
// plus the mux that registers every route.
//
// Handlers are thin. They decode a request struct, call a service, and encode a response
// struct; anything that could fail in an interesting way lives in a service package where
// it can be tested without HTTP. Because the specification is itself an OpenAPI document,
// this package deliberately does not generate a second one — see docs/ENDPOINTS.md for the
// endpoint list with links to the spec sections.
package csapi

import (
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/accounts"
	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/devicelists"
	"github.com/enimatek-nl/qwatrix/internal/directory"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/keyserver"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/media"
	"github.com/enimatek-nl/qwatrix/internal/presence"
	"github.com/enimatek-nl/qwatrix/internal/pushrules"
	"github.com/enimatek-nl/qwatrix/internal/readstate"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/enimatek-nl/qwatrix/internal/syncapi"
	"github.com/enimatek-nl/qwatrix/internal/todevice"
	"github.com/enimatek-nl/qwatrix/internal/uia"
)

// Deps is what a handler set needs to do its job. It is a struct rather than a dozen
// parameters so that adding a service does not touch every handler signature.
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Accounts *accounts.Service
	// UIA is the user-interactive authentication session store.
	UIA *uia.Manager
	// SharedSecrets issues and redeems shared-secret registration nonces.
	SharedSecrets *accounts.NonceStore
	// Rooms is the room server: creation, event writes, state and room reads.
	Rooms *roomserver.Service
	// Sync serves GET /sync.
	Sync *syncapi.Service
	// Presence keeps this server's users' online status.
	Presence *presence.Service
	// ReadState holds receipts, typing indicators, read markers, and account data.
	ReadState *readstate.Service
	// Keys is the key server: device keys and one-time keys for this server's own devices.
	Keys *keyserver.Service
	// Directory is the user directory: who goes by what name, and who may know.
	Directory *directory.Service
	PushRules *pushrules.Service
	// ToDevice is the queue of messages addressed to devices.
	ToDevice *todevice.Service
	// Media is the file repository.
	Media *media.Service
	// DeviceLists tells people that each other's encryption devices changed.
	DeviceLists *devicelists.Service
}

// requireAuth wraps a handler so that the caller must present a valid access token.
func (d Deps) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	resolver := httpx.RequireAuth(d.Accounts, next)
	return func(w http.ResponseWriter, r *http.Request) { resolver.ServeHTTP(w, r) }
}

// optionalAuth wraps a handler that answers differently when the caller is known.
func (d Deps) optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	resolver := httpx.OptionalAuth(d.Accounts, next)
	return func(w http.ResponseWriter, r *http.Request) { resolver.ServeHTTP(w, r) }
}

// NewMux builds the complete client-server API handler.
//
// Routes are registered with the standard library's method-and-pattern syntax; the
// specification's /_matrix/client/{v3,r0,r0.6.1}/ prefixes are aliases of the same handler
// and are added by [prefixes] rather than repeated per endpoint.
func NewMux(deps Deps) http.Handler {
	mux := http.NewServeMux()

	// GET /_matrix/client/versions is the only endpoint served outside the versioned
	// prefix, and the one every client and test harness calls first.
	mux.HandleFunc("GET /_matrix/client/versions", handleVersions(deps))

	registerAuthRoutes(mux, deps)
	registerRoomRoutes(mux, deps)
	registerSyncRoutes(mux, deps)
	registerPresenceRoutes(mux, deps)
	registerReadStateRoutes(mux, deps)
	registerFilterRoutes(mux, deps)
	registerKeyRoutes(mux, deps)
	registerPushRuleRoutes(mux, deps)
	registerToDeviceRoutes(mux, deps)
	registerMediaRoutes(mux, deps)

	// Unmatched paths under /_matrix/ answer with M_UNRECOGNIZED, which is how Matrix
	// distinguishes "this server does not speak that" from "nothing is here". These
	// catch-alls are the lowest-precedence patterns in the mux, so they only fire when no
	// endpoint matched.
	unrecognized := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.RespondError(w, r, matrix.NewUnrecognized("Unrecognized request: %s %s", r.Method, r.URL.Path))
	})
	mux.Handle("/_matrix/", unrecognized)
	mux.Handle("/_synapse/", unrecognized)
	mux.Handle("/.well-known/", unrecognized)
	// Anything else is not a Matrix endpoint at all, so it gets a plain 404 rather than a
	// Matrix error body — a request that never asked for Matrix should not be answered in
	// its vocabulary.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	})

	return mux
}

// versionedPath builds the list of paths an endpoint is served on. The specification
// settled on /v3 and older clients still use /r0 and the point releases, so the same
// handler answers all of them.
func versionedPath(suffix string) []string {
	return []string{
		"/_matrix/client/v3/" + suffix,
		"/_matrix/client/v1/" + suffix,
		"/_matrix/client/r0/" + suffix,
		"/_matrix/client/r0.6.1/" + suffix,
	}
}

// handleVersions answers GET /_matrix/client/versions.
//
// With authentication the server may advertise per-user unstable features; qwatrix
// advertises only implemented ones, so the answer is the same either way.
func handleVersions(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.RespondJSON(w, http.StatusOK, versionsResponse{
			Versions:         matrix.ClientVersions,
			UnstableFeatures: matrix.UnstableFeatures(),
		})
	}
}

// versionsResponse is the wire shape of GET /_matrix/client/versions.
type versionsResponse struct {
	Versions         []string        `json:"versions"`
	UnstableFeatures map[string]bool `json:"unstable_features,omitempty"`
}
