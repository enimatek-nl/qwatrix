package csapi

// Presence: what a user tells the server about being there, and what the server will tell
// other users.

import (
	"net/http"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/presence"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// registerPresenceRoutes registers the two presence endpoints.
func registerPresenceRoutes(mux *httpx.RouteTable, deps Deps) {
	for _, path := range versionedPath("presence/{userID}/status") {
		// Both are authenticated. Reading is authenticated because presence is only
		// meaningful between people who share a room, and an anonymous reader shares one
		// with nobody; writing is authenticated because it is a statement about yourself.
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetPresence(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutPresence(deps)))
	}
}

// handleGetPresence answers GET /presence/{userID}/status.
func handleGetPresence(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := deps.localOfPathUser(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		row, err := deps.Presence.Get(r.Context(), localpart)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		response := map[string]any{
			"presence": row.Presence,
			// The answer is a duration because a client renders "last seen …" and should
			// not have to trust this server's clock to do the subtraction.
			"last_active_ago":  maxInt64(nowMS()-row.LastActiveMS, 0),
			"currently_active": row.Presence == store.PresenceOnline,
		}
		if row.StatusMessage != "" {
			response["status_msg"] = row.StatusMessage
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}

// handlePutPresence answers PUT /presence/{userID}/status.
func handlePutPresence(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		localpart, err := deps.localOfPathUser(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if localpart != session.Localpart {
			// Presence is a first-person statement. Letting a device set somebody else's
			// status would turn every compromised token into a way to lie about a
			// colleague's availability.
			httpx.RespondError(w, r, matrix.NewForbidden("You can only set your own presence"))
			return
		}

		var body struct {
			Presence      string `json:"presence"`
			StatusMessage string `json:"status_msg"`
		}
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if body.Presence != "" && !presence.Valid(body.Presence) {
			httpx.RespondError(w, r, matrix.NewInvalidParam("Unknown presence %q", body.Presence))
			return
		}
		if len(body.StatusMessage) > maxPresenceStatusLength {
			httpx.RespondError(w, r, matrix.NewInvalidParam("status_msg is longer than %d characters", maxPresenceStatusLength))
			return
		}

		if err := deps.Presence.Set(r.Context(), localpart, body.Presence, body.StatusMessage); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// maxPresenceStatusLength bounds a status message. The specification leaves it open, and an
// unbounded one becomes a way to fill every room member's sync response.
const maxPresenceStatusLength = 255

// nowMS is the wall clock in milliseconds, the unit every "…ago" field in the protocol is
// computed against.
func nowMS() int64 { return time.Now().UnixMilli() }

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
