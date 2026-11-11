package csapi

// The sync endpoint and the room-route registration table.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/filters"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/enimatek-nl/qwatrix/internal/syncapi"
)

func handleSync(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		query := r.URL.Query()

		request := syncapi.Request{
			Localpart: session.Localpart,
			DeviceID:  session.DeviceID,
			Since:     query.Get("since"),
			Timeout:   time.Duration(deps.Config.SyncTimeoutMax),
			FullState: isTrue(query.Get("full_state")),
			// set_presence is how a sync doubles as a heartbeat; "offline" is the spec's
			// spelling of "do not mark me active", and anything else is the caller
			// declaring a presence.
			SetPresence: query.Get("set_presence"),
		}
		if raw := query.Get("timeout"); raw != "" {
			milliseconds, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || milliseconds < 0 {
				httpx.RespondError(w, r, matrix.NewInvalidParam("timeout must be a non-negative integer"))
				return
			}
			request.Timeout = time.Duration(milliseconds) * time.Millisecond
			// The server caps the long-poll: a client may ask for an hour, but a homeserver
			// that holds a connection open for an hour ties up a descriptor for a request
			// that will time out at every proxy in between.
			if request.Timeout > time.Duration(deps.Config.SyncTimeoutMax) {
				request.Timeout = time.Duration(deps.Config.SyncTimeoutMax)
			}
		}
		if raw := query.Get("not_membership"); raw != "" {
			request.NotMembership = append(request.NotMembership, splitComma(raw)...)
		}
		// `pos` is the MSC3575 spelling of `since`; a server that speaks the sliding-sync
		// adjacency accepts it so that a client probing both paths gets consistent answers.
		if request.Since == "" {
			request.Since = query.Get("pos")
		}

		// A filter is either a saved filter's ID or, for a client that would rather not make a
		// second round trip, the JSON object itself. Both spellings arrive in the same
		// parameter, so the only way to tell them apart is that a JSON object starts with a
		// brace — which is why an unknown ID must be reported rather than treated as an
		// unparsable object and quietly ignored.
		if raw := query.Get("filter"); raw != "" {
			definition := json.RawMessage(raw)
			if trimmed := strings.TrimSpace(raw); trimmed == "" || trimmed[0] != '{' {
				stored, err := deps.Store.Filters.Get(r.Context(), session.Localpart, raw)
				if err != nil {
					if store.IsNotFound(err) {
						httpx.RespondError(w, r, matrix.NewAPIError(http.StatusBadRequest,
							matrix.ErrInvalidParam, "Unknown filter"))
						return
					}
					httpx.RespondError(w, r, err)
					return
				}
				definition = json.RawMessage(stored.Definition)
			}
			parsed, err := filters.Parse(definition)
			if err != nil {
				httpx.RespondError(w, r, matrix.NewInvalidParam("filter is not valid JSON"))
				return
			}
			request.Filter = parsed
			if limit, ok := parsed.TimelineLimit(); ok {
				request.TimelineLimit = limit
			}
		}

		response, err := deps.Sync.Sync(r.Context(), request)
		if err != nil {
			if mapped := syncapi.BadRequestFor(err); mapped != nil {
				httpx.RespondError(w, r, mapped)
				return
			}
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}

func isTrue(value string) bool { return value == "true" || value == "1" }

func splitComma(value string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(value); i++ {
		if value[i] == ',' {
			if i > start {
				out = append(out, value[start:i])
			}
			start = i + 1
		}
	}
	if start < len(value) {
		out = append(out, value[start:])
	}
	return out
}

// registerRoomRoutes registers the room, event, membership, alias and directory endpoints.
func registerRoomRoutes(mux *httpx.RouteTable, deps Deps) {
	// Room creation.
	for _, path := range versionedPath("createRoom") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleCreateRoom(deps)))
	}

	// The room summary, which is a v1 resource and deliberately not aliased under v3: it was added
	// after the version prefixes stopped being incremented, and clients ask for it by its v1 name.
	mux.HandleFunc("GET /_matrix/client/v1/room_summary/{roomIDOrAlias}", deps.requireAuth(handleRoomSummary(deps)))

	// Timeline and state writes. The transaction ID is part of the URL, which is what makes
	// a retried send idempotent rather than duplicated.
	for _, path := range versionedPath("rooms/{roomID}/send/{eventType}/{txnID}") {
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleSendMessage(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/state/{tail...}") {
		// One multi-segment pattern serves every spelling of a state write: the empty state
		// key written as `/state/m.room.name`, as `/state/m.room.name/`, or not at all, and a
		// state key that itself contains a slash (m.space.child names a room ID, which
		// contains a colon, and clients have been known to leave the slash in).
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleSetState(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/redact/{eventID}") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleRedact(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/redact/{eventID}/{txnID}") {
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleRedact(deps)))
	}

	// Reads.
	for _, path := range versionedPath("rooms/{roomID}/state") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetState(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/state/{tail...}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetStateEvent(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/event/{eventID}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetEvent(deps)))
	}
	// Relations. The optional trailing segments are separate routes because Go's mux matches a
	// fixed number of segments; a client asking for a relation type and an event type is asking a
	// narrower question, not a different one.
	for _, path := range versionedPath("rooms/{roomID}/relations/{eventID}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleRelations(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/relations/{eventID}/{relType}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleRelations(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/relations/{eventID}/{relType}/{eventType}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleRelations(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/messages") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleMessages(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/members") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleMembers(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/joined_members") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleJoinedMembers(deps)))
	}
	for _, path := range versionedPath("rooms/{roomID}/aliases") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleRoomAliases(deps)))
	}
	for _, path := range versionedPath("joined_rooms") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleJoinedRooms(deps)))
	}

	// Membership transitions.
	membership := []struct {
		suffix  string
		handler http.HandlerFunc
	}{
		{"rooms/{roomID}/join", handleJoinRoom(deps)},
		// The other spelling of joining, which is also how a client joins through an
		// alias. Both are in the specification, and clients do not agree on which to use.
		{"join/{roomIDOrAlias}", handleJoinRoom(deps)},
		{"rooms/{roomID}/leave", handleLeaveRoom(deps)},
		{"rooms/{roomID}/forget", handleForgetRoom(deps)},
		{"rooms/{roomID}/invite", handleInvite(deps)},
		{"rooms/{roomID}/kick", handleKick(deps)},
		{"rooms/{roomID}/ban", handleBan(deps)},
		{"rooms/{roomID}/unban", handleUnban(deps)},
	}
	for _, route := range membership {
		for _, path := range versionedPath(route.suffix) {
			mux.HandleFunc("POST "+path, deps.requireAuth(route.handler))
		}
	}

	// Aliases and the room directory.
	for _, path := range versionedPath("directory/room/{alias}") {
		mux.HandleFunc("GET "+path, deps.optionalAuth(handleResolveAlias(deps)))
		// PUT and DELETE on this path create and remove an alias. The specification also
		// spells the same two operations as /directories/alias/{alias}, and clients pick
		// one or the other, so serving only half of them looks like a broken server.
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleCreateAlias(deps)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteAlias(deps)))
	}
	for _, path := range versionedPath("directories/alias/{alias}") {
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleCreateAlias(deps)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteAlias(deps)))
	}
	for _, path := range versionedPath("publicRooms") {
		// GET with query parameters is the original form; POST with a filter body is how a
		// client searches the directory. A server that answers only one makes the other
		// client path look broken.
		mux.HandleFunc("GET "+path, deps.optionalAuth(handlePublicRooms(deps)))
		mux.HandleFunc("POST "+path, deps.optionalAuth(handlePublicRooms(deps)))
	}
	for _, path := range versionedPath("capabilities") {
		// Authenticated even though it looks like discovery: capabilities can depend on
		// who is asking, and the specification answers 401 to an anonymous caller.
		mux.HandleFunc("GET "+path, deps.requireAuth(handleCapabilities(deps)))
	}
	for _, path := range versionedPath("directory/list/room/{roomID}") {
		mux.HandleFunc("GET "+path, deps.optionalAuth(handleRoomDirectoryVisibility(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleRoomDirectoryVisibility(deps)))
	}
}

// registerSyncRoutes registers /sync.
func registerSyncRoutes(mux *httpx.RouteTable, deps Deps) {
	for _, path := range versionedPath("sync") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleSync(deps)))
	}
}
