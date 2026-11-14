package csapi

// The client's half of push: register a device to be told about things, and read back what this
// server said was worth telling.
//
// `POST /pushers/set` is one endpoint doing two operations, chosen by whether `kind` is present —
// which is what the specification says, and worth a comment because it reads at first like an
// oversight. A client that has lost its own state (a reinstall, a token refresh, a push token that
// changed under it) needs to be able to express "make the world look like this" in one request, and
// an upsert plus an implicit delete is what lets it do that without first asking what it had.
//
// Neither endpoint returns anything on success. That is not laziness: a client that asked for a
// pusher to exist does not need it described back to it, and a body here would be a second place
// for the answer to disagree with `GET /pushers`.

import (
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/push"
)

// registerPusherRoutes registers the pusher and notification-listing endpoints.
func registerPusherRoutes(mux *httpx.RouteTable, deps Deps) {
	for _, path := range versionedPath("pushers") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetPushers(deps)))
	}
	for _, path := range versionedPath("pushers/set") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleSetPusher(deps)))
	}
	for _, path := range versionedPath("notifications") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetNotifications(deps)))
	}
}

// setPusherRequest is POST /pushers/set's body.
//
// `Kind` is a pointer because its absence is the request: no kind means "withdraw this pusher", and
// a plain string would make an explicit `"kind": null` and an omitted field the same thing when a
// client is entitled to mean something by one of them.
type setPusherRequest struct {
	Kind     *string        `json:"kind"`
	AppID    string         `json:"app_id"`
	PushKey  string         `json:"pushkey"`
	Data     map[string]any `json:"data"`
	Append   *bool          `json:"append"`
	AppendTo string         `json:"append_to"`

	AppDisplayName    string `json:"app_display_name"`
	DeviceDisplayName string `json:"device_display_name"`
	ProfileTag        string `json:"profile_tag"`
	Lang              string `json:"lang"`
}

// handleSetPusher answers POST /pushers/set.
func handleSetPusher(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body setPusherRequest
		// The body is required: a pusher request with no body is not asking for a pusher to
		// exist, and treating an empty one as "withdraw" would delete a pusher because a client
		// dropped a request on the floor.
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		kind := ""
		if body.Kind != nil {
			kind = *body.Kind
		}
		if kind == push.Kind && session.AppService {
			// An application service token speaks for a program, not for a device, and a pusher
			// is bound to the session that registered it — so that a pusher registered this way
			// could never be revoked by the user signing out, and the service would not know
			// which user's messages it was being told about.
			httpx.RespondError(w, r, matrix.NewInvalidParam(
				"an application service token cannot register a pusher: a pusher belongs to one device session"))
			return
		}
		appendRequested := body.Append != nil && *body.Append
		if err := deps.Push.Register(r.Context(), push.Registration{
			Localpart:         session.Localpart,
			AccessToken:       session.AccessToken,
			DeviceID:          session.DeviceID,
			Kind:              kind,
			AppID:             body.AppID,
			PushKey:           body.PushKey,
			Data:              body.Data,
			Append:            appendRequested,
			AppendTo:          body.AppendTo,
			AppDisplayName:    body.AppDisplayName,
			DeviceDisplayName: body.DeviceDisplayName,
			ProfileTag:        body.ProfileTag,
			Lang:              body.Lang,
		}); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleGetPushers answers GET /pushers.
func handleGetPushers(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		pushers, err := deps.Push.Pushers(r.Context(), session.Localpart)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// `pushers` is always an array, never null: a client counts it.
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"pushers": pushers})
	}
}

// handleGetNotifications answers GET /notifications.
func handleGetNotifications(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		query := r.URL.Query()
		size, err := pageSize(query.Get("page_size"), "page_size")
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		recentN, err := pageSize(query.Get("recent_n"), "recent_n")
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		notifications, next, err := deps.Push.Notifications(r.Context(), session.Localpart,
			query.Get("page"), size, query.Get("only"), recentN)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		response := map[string]any{"notifications": notifications}
		if next != "" {
			// `next_token` is present only when there is a page after this one. A client that
			// paged to the end and got a token back would page again for nothing, and the
			// difference between "no more" and "ask me again" should not depend on the client
			// recognising an empty string.
			response["next_token"] = next
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}

// pageSize reads a paging parameter that must be a positive integer if it is there at all.
//
// An unparsable value is refused rather than read as zero, because zero and absent mean different
// things here: `page_size=0` is a client that has made a mistake, and answering it with the default
// page lets the mistake live on in a client that never notices it is being ignored.
func pageSize(raw, name string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := positiveInt(raw)
	if err != nil {
		return 0, matrix.NewInvalidParam("%s must be an integer, got %q", name, raw)
	}
	if value < 0 {
		return 0, matrix.NewInvalidParam("%s must not be negative, got %d", name, value)
	}
	return value, nil
}
