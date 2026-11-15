package csapi

// The client-facing door onto MSC4186, the sliding sync.
//
// One POST, on two URLs: the stable `/v4/sync` and the unstable prefix Element X still uses while
// servers catch up. Both are mounted because a client that has to guess which one a server answers on
// is a client that shows a spinner, and because the MSC names both.
//
// The door is deliberately thin. The endpoint's whole difficulty is stateful — what this connection
// has been shown, which `pos` belongs to it, what counts as news — and all of that lives in
// internal/slidingsync, where it can be reasoned about without an HTTP request in the way. What stays
// here is the part that only a handler can do: turn a session into an identity, cap a long-poll, and
// translate this package's errors into the spec's.
//
// An application-service token is refused, as it is for `/v3/sync`: such a token speaks for a program
// rather than a device, and a connection is defined by being one device's memory of what it has seen.

import (
	"net/http"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/slidingsync"
)

// registerSlidingSyncRoutes mounts the sliding-sync endpoint.
//
// Two URLs, one handler. The unstable prefix is what clients in the field call today and has to keep
// answering after the stable path exists, because a client that learned this server's address
// mid-upgrade is not going to re-read /versions before its next sync — and a server that answers only
// the stable URL while advertising nothing is a server whose clients sit on a spinner.
func registerSlidingSyncRoutes(mux *httpx.RouteTable, deps Deps) {
	mux.HandleFunc("POST /_matrix/client/v4/sync", deps.requireAuth(handleSlidingSync(deps)))
	mux.HandleFunc("POST /_matrix/client/unstable/org.matrix.simplified_msc3575/sync",
		deps.requireAuth(handleSlidingSync(deps)))
}

// handleSlidingSync answers one request on one connection.
func handleSlidingSync(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if err := refuseApplicationServiceSync(session); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if deps.SlidingSync == nil {
			httpx.RespondError(w, r, matrix.NewUnrecognized("this server does not serve sliding sync"))
			return
		}

		request := slidingsync.Request{
			// The connection belongs to a user *and a device*, and both come from the session. Neither
			// can be influenced by the body: an access token that could name another device would turn
			// this endpoint into a way to read that device's rooms.
			Localpart:  session.Localpart,
			DeviceID:   session.DeviceID,
			MaxTimeout: time.Duration(deps.Config.SyncTimeoutMax),
		}
		if err := httpx.DecodeJSON(r, &request); err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		response, err := deps.SlidingSync.Sync(r.Context(), &request)
		if err != nil {
			if answer := slidingsync.AnswerFor(err); answer != nil {
				httpx.RespondError(w, r, answer)
				return
			}
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}
