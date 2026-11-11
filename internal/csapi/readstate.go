package csapi

import (
	"encoding/json"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/readstate"
)

// registerReadStateRoutes registers typing, receipts, read markers, and account data.
func registerReadStateRoutes(mux *httpx.RouteTable, deps Deps) {
	// Typing. The user ID is in the path because the endpoint is the one a client uses to
	// report *somebody else's* typing on their behalf when relaying over federation; locally it
	// may only ever be the caller, which the handler enforces.
	for _, path := range versionedPath("rooms/{roomID}/typing/{userID}") {
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutTyping(deps)))
	}

	// Receipts. The receipt type is in the path so a client can send a kind this server does
	// not know without changing the endpoint's shape.
	for _, path := range versionedPath("rooms/{roomID}/receipt/{receiptType}/{eventID}") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handlePostReceipt(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/read_markers") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handlePostReadMarkers(deps)))
	}

	// Account data, global and per-room. The path names the user, so the handler checks that it
	// is the caller: these are private keys, and a second device writing them is fine while
	// somebody else's device doing so is not.
	for _, path := range versionedPath("user/{userID}/account_data/{type}") {
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutAccountData(deps, false)))
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetAccountData(deps, false)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteAccountData(deps, false)))
	}
	for _, path := range versionedPath("rooms/{roomID}/account_data/{type}") {
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutAccountData(deps, true)))
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetAccountData(deps, true)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteAccountData(deps, true)))
	}
	// The older spelling of room account data, which names the user as well as the room:
	// `/user/{userID}/rooms/{roomID}/account_data/{type}`. The current spec path is the one
	// above, but clients written against the older text of the spec — and Complement exercises
	// both — send this one, and it means the same thing. A server that answers 404 has not
	// declined a deprecated request, it has lost a client's data.
	for _, path := range versionedPath("user/{userID}/rooms/{roomID}/account_data/{type}") {
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutAccountData(deps, true)))
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetAccountData(deps, true)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteAccountData(deps, true)))
	}
}

// handlePutTyping answers PUT /rooms/{roomID}/typing/{userID}.
func handlePutTyping(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		localpart, err := deps.localOfPathUser(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if localpart != session.Localpart {
			// A typing indicator is a first-person, present-tense claim. Accepting another
			// user's name here would let any member make every other member's client show a
			// person typing who is nowhere near a keyboard.
			httpx.RespondError(w, r, matrix.NewForbidden("You can only set your own typing status"))
			return
		}

		// The body is optional in the same way presence's is: `{"typing": false}` is a stop and
		// an absent body reads as one rather than as a parse error.
		var body struct {
			Typing  *bool `json:"typing"`
			Timeout int64 `json:"timeout"`
		}
		if err := httpx.DecodeJSONIfPresent(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		typing := body.Typing != nil && *body.Typing

		if err := deps.ReadState.SetTyping(r.Context(), r.PathValue("roomID"),
			session.Localpart, typing, body.Timeout); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}

// handlePostReceipt answers POST /rooms/{roomID}/receipt/{receiptType}/{eventID}.
func handlePostReceipt(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		// The body carries only optional thread metadata; an empty one is the common case.
		var body struct {
			ThreadID string `json:"thread_id"`
		}
		if err := httpx.DecodeJSONIfPresent(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if err := deps.ReadState.AddReceipt(r.Context(), r.PathValue("roomID"), session.Localpart,
			r.PathValue("receiptType"), r.PathValue("eventID"), body.ThreadID); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}

// handlePostReadMarkers answers POST /rooms/{roomID}/read_markers.
func handlePostReadMarkers(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var req readstate.ReadMarkersRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}
		if err := deps.ReadState.SetReadMarkers(r.Context(), r.PathValue("roomID"),
			session.Localpart, req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}

// accountDataScope is the difference between the global and the per-room endpoints: the room ID
// comes from the path in one case and is empty in the other.
func accountDataScope(r *http.Request, perRoom bool) string {
	if perRoom {
		return r.PathValue("roomID")
	}
	return ""
}

// accountDataOwner resolves who a request is acting as.
//
// Only the per-user endpoints name an owner in the path; the per-room ones are addressed by room
// and belong to whoever is authenticated. Resolving a path user that is not there would fail, so
// the distinction has to be made here rather than in each handler.
func accountDataOwner(deps Deps, r *http.Request, perRoom bool) (string, error) {
	session := httpx.MustSession(r)
	if !perRoom {
		localpart, err := deps.localOfPathUser(r)
		if err != nil {
			return "", err
		}
		if localpart != session.Localpart {
			// Account data is private to the account. A second device of the same user is
			// fine because it authenticates as them; a different user is not.
			return "", matrix.NewForbidden("You can only change your own account data")
		}
	}
	return session.Localpart, nil
}

// handlePutAccountData answers PUT …/account_data/{type}.
func handlePutAccountData(deps Deps, perRoom bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := accountDataOwner(deps, r, perRoom)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		// Account data is an overwrite of the whole key, so the body is required and must be
		// the value itself. Accepting a missing body would silently delete what the client
		// meant to replace.
		var content json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}
		dataType := r.PathValue("type")
		var writeErr error
		if perRoom {
			writeErr = deps.ReadState.PutRoomAccountData(r.Context(), localpart,
				r.PathValue("roomID"), dataType, content)
		} else {
			writeErr = deps.ReadState.PutAccountData(r.Context(), localpart, dataType, content)
		}
		if writeErr != nil {
			httpx.RespondError(w, r, writeErr)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}

// handleGetAccountData answers GET …/account_data/{type}.
func handleGetAccountData(deps Deps, perRoom bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := accountDataOwner(deps, r, perRoom)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		content, err := deps.ReadState.GetAccountData(r.Context(), localpart,
			accountDataScope(r, perRoom), r.PathValue("type"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondRawJSON(w, http.StatusOK, content)
	}
}

// handleDeleteAccountData answers DELETE …/account_data/{type}.
func handleDeleteAccountData(deps Deps, perRoom bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := accountDataOwner(deps, r, perRoom)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if err := deps.ReadState.DeleteAccountData(r.Context(), localpart,
			accountDataScope(r, perRoom), r.PathValue("type")); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}
