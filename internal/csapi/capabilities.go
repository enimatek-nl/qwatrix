package csapi

// Server capabilities: what this server says it can do.

import (
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
)

// handleCapabilities answers GET /capabilities.
//
// The endpoint is authenticated even though its contents look like public discovery,
// because Complement and the specification both treat it as per-user (capabilities can
// depend on what the account is allowed to do) and answer 401 to an anonymous caller.
func handleCapabilities(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		available := map[string]string{}
		for _, version := range events.RoomVersionsSupportedForCreation() {
			available[version] = "stable"
		}

		// `m.room_versions` is what a client reads before creating a room, so the default
		// here must be the version the server actually creates rooms in — a capability that
		// disagrees with the server's behaviour is worse than no capability at all.
		defaultVersion := deps.Config.DefaultRoomVersion
		if _, ok := available[defaultVersion]; !ok {
			defaultVersion = "11"
		}

		capabilities := map[string]any{
			"m.room_versions": map[string]any{
				"default":   defaultVersion,
				"available": available,
			},
			// A client offers "change password" in its UI only if this says the server
			// can do it; qwatrix implements POST /account/password, so it is enabled.
			"m.change_password": map[string]any{"enabled": true},
			// Room version 10's knock_restricted join rule is understood, so clients can
			// offer it.
			"m.knockRestricted": map[string]any{"enabled": true},
			// Server-to-server federation of rooms is not implemented yet; saying so here
			// is what stops a client offering "share to a federated room".
			"m.server_notices": map[string]any{"enabled": false},
		}

		response := map[string]any{"capabilities": capabilities}
		if deps.Config.EnableRegistration {
			// MSC3866-style registration types, advertised so that a client can build the
			// form before it starts the User-Interactive exchange.
			response["registration_enabled"] = true
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}
