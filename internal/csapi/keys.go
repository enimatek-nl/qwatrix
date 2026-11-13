package csapi

import (
	"encoding/json"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/keyserver"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// registerKeyRoutes registers the end-to-end encryption key endpoints.
func registerKeyRoutes(mux *httpx.RouteTable, deps Deps) {
	for _, path := range versionedPath("keys/upload") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleUploadKeys(deps)))
	}
	for _, path := range versionedPath("keys/query") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleQueryKeys(deps)))
	}
	// `GET /keys/status` is the cheapest thing a client can poll to decide whether to work: it answers
	// how many one-time keys this device has left, and a device that runs out cannot start a new
	// Olm session with anybody until it is online again. It is served as a map keyed by algorithm, so
	// a client that only understands signed_curve25519 does not have to know what else exists.
	for _, path := range versionedPath("keys/status") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleKeyStatus(deps)))
	}
	for _, path := range versionedPath("keys/changes") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleKeyChanges(deps)))
	}
	for _, path := range versionedPath("keys/claim") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleClaimKeys(deps)))
	}
	// The user directory lives beside the key server here because both answer questions about
	// other users' identities, and both are filtered by who is asking.
	for _, path := range versionedPath("user_directory/search") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleSearchUserDirectory(deps)))
	}
}

// handleUploadKeys implements POST /keys/upload.
func handleUploadKeys(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var req keyserver.UploadRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		counts, err := deps.Keys.Upload(r.Context(),
			matrix.UserID(session.Localpart, deps.Config.ServerName), session.DeviceID, req)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"one_time_key_counts": counts})
	}
}

// handleQueryKeys implements POST /keys/query.
func handleQueryKeys(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceKeys map[string]json.RawMessage `json:"device_keys"`
		}
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		query, err := parseDeviceList(body.DeviceKeys)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		keys, err := deps.Keys.DeviceKeysFor(r.Context(), query)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"device_keys": keys})
	}
}

// parseDeviceList reads the `device_keys` of a query: a user ID mapped to a list of device IDs,
// where an empty list means every device that user has.
//
// The shape is checked rather than inferred because the mistake is common and silent: an object of
// `{"device_id": true}` looks reasonable and decodes into nothing usable, and a server that
// answers it with "no keys" tells the client that the user's devices are gone.
func parseDeviceList(raw map[string]json.RawMessage) (map[string][]string, error) {
	out := make(map[string][]string, len(raw))
	for userID, value := range raw {
		var devices []string
		if err := json.Unmarshal(value, &devices); err != nil {
			return nil, matrix.NewBadJSON(
				"device_keys must map a user ID to a list of device IDs")
		}
		out[userID] = devices
	}
	return out, nil
}

// handleClaimKeys implements POST /keys/claim.
func handleClaimKeys(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body struct {
			OneTimeKeys map[string]map[string]string `json:"one_time_keys"`
		}
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		claimed, err := deps.Keys.Claim(r.Context(),
			matrix.UserID(session.Localpart, deps.Config.ServerName), body.OneTimeKeys)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"one_time_keys": claimed})
	}
}

// handleSearchUserDirectory implements POST /user_directory/search.
func handleSearchUserDirectory(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var req struct {
			SearchTerm string `json:"search_term"`
			Limit      int    `json:"limit"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		results, limited, err := deps.Directory.Search(r.Context(),
			session.Localpart, req.SearchTerm, req.Limit)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"results": results,
			"limited": limited,
		})
	}
}

// handleKeyStatus implements GET /keys/status.
func handleKeyStatus(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		// The counts are this device's, from the device's own session — and no device may read
		// another's, because "how many keys does this device have left" says something about when it
		// was last online, which is somebody else's business.
		counts, err := deps.Keys.Counts(r.Context(), session.UserID, session.DeviceID)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if counts == nil {
			// A device that has never uploaded anything holds zero of each, and the specification
			// answers an empty map rather than null: a client reading `counts["signed_curve25519"]`
			// should get 0 and top up, not crash on a missing key.
			counts = map[string]int64{}
		}
		httpx.RespondJSON(w, http.StatusOK, counts)
	}
}

// handleKeyChanges answers GET /keys/changes?from=&to=.
//
// This is the endpoint a client uses after a gap: it has a token from before, a token from now, and
// wants to know whose encryption keys to re-fetch rather than re-fetching everybody's. The answer is
// the same log /sync reads, queried between two positions, so the two cannot disagree about what
// happened in an interval — a client that missed the syncs still gets the same list.
func handleKeyChanges(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		changed, left, err := deps.DeviceLists.Changes(r.Context(), session.Localpart,
			r.URL.Query().Get("from"), r.URL.Query().Get("to"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if changed == nil {
			changed = []string{}
		}
		if left == nil {
			left = []string{}
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"changed": changed, "left": left})
	}
}
