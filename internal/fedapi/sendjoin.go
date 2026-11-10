package fedapi

// Accepting a federated join.
//
// `make_join` offers a shape; `send_join` is where a peer returns the signed event and asks to be let
// in. Everything that makes federation worth trusting is concentrated in this one handler:
//
//   - the request is authenticated by an ed25519 signature over the method, the URI, the sender, the
//     receiver and the body — not by a TLS connection, and not by a header that merely claims an
//     origin;
//   - the event itself carries a second signature, over the event, because the event outlives the
//     request that delivered it and has to remain checkable by servers that were never party to this
//     conversation;
//   - the event is then checked against the room's own rules by the room server, which does not care
//     how politely the request was signed.
//
// Two response shapes are served, because both are in use. Version 2 answers with the object the
// specification documents. Version 1 answers with the legacy transaction envelope, `[200, body]`,
// which is what a pre-v2 peer parses; a v1 peer that received a bare object would read the whole
// answer as a status code.

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// sendJoinResponse is the body both versions agree on.
//
// `state` and `auth_chain` are arrays of signed event JSON. The joining server verifies every event
// in them and then checks that the join it signed is allowed by the state it was sent — which is the
// mechanism that stops a server being told one set of rules by a peer that has since been ignored.
type sendJoinResponse struct {
	State         []json.RawMessage `json:"state"`
	AuthChain     []json.RawMessage `json:"auth_chain"`
	Origin        string            `json:"origin"`
	Event         json.RawMessage   `json:"event,omitempty"`
	ServersInRoom []string          `json:"servers_in_room,omitempty"`
}

// handleSendJoin accepts a signed join event into one of this server's rooms.
//
// `legacyEnvelope` selects the v1 answer. The two routes share everything else, including every
// check: a peer that asked the older question is entitled to the same scrutiny, not to an older and
// weaker one.
func (h *Handler) handleSendJoin(legacyEnvelope bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.opts.Join == nil {
			httpx.RespondError(w, r, matrix.NewForbidden("this server does not accept federated joins"))
			return
		}
		fedRequest := h.authenticatePeer(w, r)
		if fedRequest == nil {
			return
		}

		origin := string(fedRequest.Origin())
		body := fedRequest.Content()
		if len(body) == 0 {
			httpx.RespondError(w, r, matrix.NewNotJSON("send_join requires the join event in the request body"))
			return
		}

		roomID := r.PathValue("roomID")
		eventID := r.PathValue("eventID")
		room, err := h.opts.Join.RoomVersionFor(r.Context(), roomID)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		event, err := room.NewEventFromUntrustedJSON(body)
		if err != nil {
			slog.Debug("fedapi: could not parse a join event", "room_id", roomID, "error", err)
			httpx.RespondError(w, r, matrix.NewBadJSON("the join event cannot be read"))
			return
		}
		if event.EventID() != eventID {
			// The path names the event the peer is submitting. A mismatch means the two are
			// not the same event, and honouring the body while the peer believes it sent
			// something else is how rooms acquire members nobody intended.
			httpx.RespondError(w, r, matrix.NewInvalidParam("the event in the body is %q, not %q",
				event.EventID(), eventID))
			return
		}

		// The second signature: the event's own, checked against the key its server publishes.
		// A correctly signed *request* only proves who sent it; this proves the event was not
		// altered, and that the sender's server really stands behind it.
		if err := gomatrixserverlib.VerifyEventSignatures(r.Context(), event, h.keyRing,
			h.opts.Join.Events().UserIDForSender()); err != nil {
			slog.Warn("fedapi: refusing a join event whose signature does not check out",
				"room_id", roomID, "event_id", event.EventID(), "error", err)
			httpx.RespondError(w, r, matrix.NewForbidden("the join event is not signed by its server"))
			return
		}

		acceptance, err := h.opts.Join.AcceptFederationJoin(r.Context(), roomID, origin, event)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		payload := sendJoinResponse{
			State:         acceptance.State,
			AuthChain:     acceptance.AuthChain,
			Origin:        acceptance.Origin,
			Event:         acceptance.JoinEvent,
			ServersInRoom: acceptance.ServersInRoom,
		}
		if legacyEnvelope {
			// The legacy transaction format: a two-element array of status and body.
			httpx.RespondJSON(w, http.StatusOK, []any{http.StatusOK, payload})
			return
		}
		httpx.RespondJSON(w, http.StatusOK, payload)
	}
}

// isLocalServerName reports the server names this deployment answers for. It is handed to the request
// verifier so a peer that addressed its request to one of this server's other names — a legacy name it
// still federates under, say — is not refused for the trouble.
func (h *Handler) isLocalServerName(name spec.ServerName) bool {
	return name == h.opts.ServerName
}
