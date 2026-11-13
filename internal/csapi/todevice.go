package csapi

import (
	"encoding/json"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// registerToDeviceRoutes registers the to-device endpoints.
//
// The transaction ID is in the path and is deliberately ignored: a retried key share would be a
// second copy of the same session key in the recipient's queue, but dropping the duplicate requires
// remembering every transaction ID forever for a message the recipient may never need. Clients
// already deduplicate on their side, and the room-event transaction store is where the guarantee
// actually matters, because there the event becomes part of a timeline that shows the repeat.
func registerToDeviceRoutes(mux *httpx.RouteTable, deps Deps) {
	for _, path := range versionedPath("sendToDevice/{eventType}/{txnID}") {
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleSendToDevice(deps)))
	}
}

// handleSendToDevice answers PUT /sendToDevice/{eventType}/{txnID}.
func handleSendToDevice(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		// The content of a to-device message is opaque, and that is the feature: these messages
		// carry encrypted key shares, so the server forwards bytes it cannot read. Decoding them
		// into a typed struct would be claiming an understanding the server does not have, and
		// would break the first message type it does not know.
		var body struct {
			Messages map[string]map[string]json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}
		if len(body.Messages) == 0 {
			httpx.RespondError(w, r, matrix.NewMissingParam("messages"))
			return
		}
		sender := matrix.UserID(session.Localpart, deps.Config.ServerName)
		// The transaction ID is passed down rather than dropped. This server does not use it to
		// deduplicate its own queue — but the server on the far end of a remote recipient has to be
		// able to tell a retry from a second key share, and the only identifier both sides have for
		// "the same send" is the one the client chose.
		if err := deps.ToDevice.Send(r.Context(), sender, r.PathValue("txnID"),
			r.PathValue("eventType"), body.Messages); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}
