package csapi

import (
	"encoding/json"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/keyserver"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/uia"
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
	// Cross-signing: the two halves of publishing an identity. The keys themselves go to
	// `/keys/device_signing/upload`, and the signatures those keys make go to
	// `/keys/signatures/upload` — a client signs its own device with its self-signing key and posts
	// the result back, which is the only way another user's client can tell that this device really
	// belongs to this person.
	for _, path := range versionedPath("keys/device_signing/upload") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleUploadCrossSigningKeys(deps)))
	}
	for _, path := range versionedPath("keys/signatures/upload") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleUploadSignatures(deps)))
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

// handleUploadSignatures implements POST /keys/signatures/upload.
//
// It answers 200 even when some keys were refused: the client uploaded a batch and deals with the
// refusals one key at a time, from the `failures` map, which is why the specification puts them in
// the body of a success rather than in a status code.
func handleUploadSignatures(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		userID := string(matrix.UserID(session.Localpart, deps.Config.ServerName))
		var req map[string]map[string]json.RawMessage
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		failures, err := deps.Keys.UploadSignatures(r.Context(), userID, req)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if failures == nil {
			failures = map[string]map[string]keyserver.SignatureFailure{}
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"failures": failures})
	}
}

// handleUploadCrossSigningKeys implements POST /keys/device_signing/upload.
//
// The authentication rule is the one the specification and MSC3967 settle on, and it is the difference
// between a client that works and a client that hangs: setting an identity up for the first time
// needs nothing beyond the access token the client already has, because there is no identity yet to
// be protecting. *Replacing* one is a different act — somebody else may be relying on the key being
// replaced — and so it asks the user to prove they are who the access token says, with their
// password.
//
// A request that repeats what is already stored is answered with a bare success and no challenge, for
// the client that uploaded its identity and never received the answer: retrying is correct, and being
// asked for a password for the privilege of retrying is how a setup flow ends up stuck.
func handleUploadCrossSigningKeys(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		userID := string(matrix.UserID(session.Localpart, deps.Config.ServerName))
		var req struct {
			Auth        *uia.AuthBody   `json:"auth"`
			MasterKey   json.RawMessage `json:"master_key"`
			SelfSigning json.RawMessage `json:"self_signing_key"`
			UserSigning json.RawMessage `json:"user_signing_key"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		bundle := keyserver.CrossSigningBundle{
			Master:      req.MasterKey,
			SelfSigning: req.SelfSigning,
			UserSigning: req.UserSigning,
		}

		setUp, err := deps.Keys.HasCrossSigningIdentity(r.Context(), userID)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if setUp {
			unchanged, err := deps.Keys.CrossSigningUnchanged(r.Context(), userID, bundle)
			if err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			if unchanged {
				httpx.RespondJSON(w, http.StatusOK, map[string]any{})
				return
			}
			// An identity is being replaced. This is the one place in the endpoint where the
			// answer to "who are you?" is not the access token.
			done, err := deps.authenticatePassword(w, r, session.UserID, req.Auth, session.Localpart)
			if err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			if !done {
				return
			}
		}
		if err := deps.Keys.UploadCrossSigningKeys(r.Context(), userID, bundle); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
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
		response := map[string]any{"device_keys": keys}
		// A device key says what a device is; a cross-signing key says who it belongs to. A client
		// building a picture of a person needs both halves in one answer, which is why they are served
		// together: a query that answered only the devices leaves a client staring at an unverified
		// device and an encryption warning it has no way to resolve.
		caller := string(matrix.UserID(httpx.MustSession(r).Localpart, deps.Config.ServerName))
		identity, err := deps.Keys.CrossSigningFor(r.Context(), caller, usersOfQuery(query))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if len(identity.Master) > 0 {
			response["master_keys"] = identity.Master
		}
		if len(identity.SelfSigning) > 0 {
			response["self_signing_keys"] = identity.SelfSigning
		}
		if len(identity.UserSigning) > 0 {
			response["user_signing_keys"] = identity.UserSigning
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}

// usersOfQuery is the list of user IDs a key query named, which is what the cross-signing half of the
// answer is looked up for.
func usersOfQuery(query map[string][]string) []string {
	users := make([]string, 0, len(query))
	for userID := range query {
		users = append(users, userID)
	}
	return users
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
		claimed, failures, err := deps.Keys.Claim(r.Context(),
			matrix.UserID(session.Localpart, deps.Config.ServerName), body.OneTimeKeys)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// `failures` travels beside the keys rather than only in the log, because the two ways a claim
		// comes back empty are not the same news: a device that has run out of prepaid keys needs the
		// owner to open it, while a homeserver that could not be reached needs nothing from anybody and
		// the client should simply ask again. A client that is told neither gives up on an encrypted
		// conversation that would work a minute later.
		if failures == nil {
			failures = map[string]any{}
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"one_time_keys": claimed,
			"failures":      failures,
		})
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
