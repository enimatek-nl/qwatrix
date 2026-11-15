package csapi

// The client-facing doors onto server-side room key backup.
//
// Fourteen routes, which is what one operation at three depths looks like: the same write is
// addressed as "the backup", "this room's sessions in the backup", or "this one session", and each
// depth has its own path because a client that has just one session to upload should not have to wrap
// it in the shape of a whole backup. The three depths are the same three handlers, reading whichever
// path parameters their route happened to declare.
//
// Two things about these doors are load-bearing enough to say out loud:
//
//   - Every endpoint is authenticated and every one is scoped to the calling account. The payloads
//     are encrypted to a key this server does not hold, so a leak would not read as plaintext — but a
//     person's backup still says which rooms they are in, how many sessions they hold and since when,
//     which is a map of their life. There is no admin read path for any of it, and adding one would
//     turn this server into something that can at least answer "does this person have keys for room
//     X".
//   - `version` is a query parameter on the key endpoints, not a path segment, and it is required.
//     That is the specification's shape, and it is also the mechanism that stops a client writing
//     into a backup it has not looked at: the version it names has to be the current one, or the
//     write is refused with the version it should have used.
//
// Not implemented: MSC3270's `?replacement_room=`, which copies a room's sessions to a new room
// during an upgrade. It is an optimisation over re-uploading, and it is the one part of these
// endpoints where a server writes keys on a client's behalf into a room the client did not name.

import (
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/roomkeys"
)

// registerRoomKeyRoutes mounts the room key backup endpoints.
func registerRoomKeyRoutes(mux *httpx.RouteTable, deps Deps) {
	for _, path := range versionedPath("room_keys/version") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetKeyBackupVersion(deps)))
		mux.HandleFunc("POST "+path, deps.requireAuth(handleCreateKeyBackupVersion(deps)))
	}
	for _, path := range versionedPath("room_keys/version/{version}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetKeyBackupVersion(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleUpdateKeyBackupVersion(deps)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteKeyBackupVersion(deps)))
	}
	// One handler per method across all three depths: a route that does not declare {roomID}
	// simply yields an empty one, and an empty room means "the whole backup".
	for _, path := range versionedPath("room_keys/keys") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetRoomKeys(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutRoomKeys(deps)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteRoomKeys(deps)))
	}
	for _, path := range versionedPath("room_keys/keys/{roomID}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetRoomKeys(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutRoomKeys(deps)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteRoomKeys(deps)))
	}
	for _, path := range versionedPath("room_keys/keys/{roomID}/{sessionID}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetRoomKeys(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutRoomKeys(deps)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteRoomKeys(deps)))
	}
}

// handleGetKeyBackupVersion answers GET /room_keys/version and GET /room_keys/version/{version}.
//
// A 404 here is not a failure: it is how a client learns it has no backup yet, which is the state
// every account starts in and the trigger for offering to make one.
func handleGetKeyBackupVersion(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		version := r.PathValue("version")
		var (
			info *roomkeys.VersionInfo
			err  error
		)
		if version == "" {
			info, err = deps.KeyBackup.CurrentVersion(r.Context(), session.Localpart)
		} else {
			info, err = deps.KeyBackup.Version(r.Context(), session.Localpart, version)
		}
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// The etag of the *keys* is also an HTTP entity tag on this response, so a client can
		// guard a later write with If-Match without parsing the body it was told.
		w.Header().Set("ETag", `"`+info.ETag+`"`)
		httpx.RespondJSON(w, http.StatusOK, info)
	}
}

// handleCreateKeyBackupVersion answers POST /room_keys/version.
func handleCreateKeyBackupVersion(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body roomkeys.VersionRequest
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		version, err := deps.KeyBackup.CreateVersion(r.Context(), session.Localpart, body)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"version": version})
	}
}

// handleUpdateKeyBackupVersion answers PUT /room_keys/version/{version}, which exists for the one
// thing a client may legitimately change after creating a backup: its auth data, when the device
// signs it with a fresh signature.
func handleUpdateKeyBackupVersion(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body roomkeys.VersionRequest
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if err := deps.KeyBackup.UpdateVersion(r.Context(), session.Localpart,
			r.PathValue("version"), body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// An empty object, not an empty body: the specification says every endpoint answers with a
		// JSON object, and a client that decodes `null` into a struct is a client that crashes on a
		// server being technically correct.
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleDeleteKeyBackupVersion answers DELETE /room_keys/version/{version}.
func handleDeleteKeyBackupVersion(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if err := deps.KeyBackup.DeleteVersion(r.Context(), session.Localpart,
			r.PathValue("version")); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleGetRoomKeys answers the three GET depths of /room_keys/keys.
func handleGetRoomKeys(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		result, err := deps.KeyBackup.GetKeys(r.Context(), session.Localpart,
			r.URL.Query().Get("version"), r.PathValue("roomID"), r.PathValue("sessionID"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, result)
	}
}

// handlePutRoomKeys answers the three PUT depths of /room_keys/keys.
func handlePutRoomKeys(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		body, err := httpx.ReadRawJSON(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		result, err := deps.KeyBackup.PutKeys(r.Context(), session.Localpart,
			r.URL.Query().Get("version"), r.PathValue("roomID"), r.PathValue("sessionID"),
			body, r.Header.Get("If-Match"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, result)
	}
}

// handleDeleteRoomKeys answers the three DELETE depths of /room_keys/keys.
func handleDeleteRoomKeys(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		result, err := deps.KeyBackup.DeleteKeys(r.Context(), session.Localpart,
			r.URL.Query().Get("version"), r.PathValue("roomID"), r.PathValue("sessionID"),
			r.Header.Get("If-Match"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, result)
	}
}
