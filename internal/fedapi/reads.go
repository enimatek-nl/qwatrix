package fedapi

// The read side of federation: a peer asking this server for something it does not hold.
//
// Sending is the easy half of federation. These endpoints are the half a room's history depends on,
// because a server that joined a room yesterday holds today's state and none of yesterday's messages —
// everything it can show its user about the past is a question it asks a resident server.
//
// They share one permission rule, stated once in the room server: **a server may read what its users are
// in.** Event IDs are not secret, so "may I have that event?" would otherwise be a way to read a room
// whose existence the answer is not supposed to admit. A refusal therefore says nothing about whether
// the event exists, and an event this server holds but will not show is not answered with a copy of it.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// handleFederationEvent answers `GET /_matrix/federation/v1/event/{eventID}`.
func (h *Handler) handleFederationEvent() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fedRequest := h.authenticatePeer(w, r)
		if fedRequest == nil {
			return
		}
		if h.opts.Join == nil {
			httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not serve room history to peers"))
			return
		}
		raw, err := h.opts.Join.FederatedEvent(r.Context(), string(fedRequest.Origin()), r.PathValue("eventID"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// The envelope is the specification's: the origin is this server, the timestamp is now (it
		// describes this answer, not the event), and the event travels in a list even though only one
		// is ever in it.
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"origin":           string(h.opts.ServerName),
			"origin_server_ts": time.Now().UnixMilli(),
			"pdus":             []any{raw},
		})
	}
}

// handleFederationTimestampToEvent answers `GET /_matrix/federation/v1/timestamp_to_event/{roomID}`,
// the MSC3059 question a peer asks when one of its users jumps to a date in a room whose history it does
// not hold.
//
// The directions are the endpoint: "the oldest event at or after this moment" and "the newest event at or
// before it" are different questions with different answers, and a search that runs past the beginning or
// the end of the room says so with a 404 rather than clamping. A peer is about to tell its user there is
// nothing there, and an event from 1970 would be a confident lie about a moment this room knows nothing
// about.
func (h *Handler) handleFederationTimestampToEvent() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fedRequest := h.authenticatePeer(w, r)
		if fedRequest == nil {
			return
		}
		if h.opts.Join == nil {
			httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not serve room history to peers"))
			return
		}
		rawTS := r.URL.Query().Get("ts")
		milliseconds, err := strconv.ParseInt(rawTS, 10, 64)
		if err != nil {
			httpx.RespondError(w, r, matrix.NewInvalidParam("ts must be an integer number of milliseconds since the Unix epoch"))
			return
		}
		direction := r.URL.Query().Get("dir")
		var forward bool
		switch direction {
		case "f":
			forward = true
		case "b":
			forward = false
		default:
			// Not guessed at: defaulting a missing direction to "backwards" would answer a question
			// nobody asked.
			httpx.RespondError(w, r, matrix.NewInvalidParam(`dir must be one of "f" or "b"`))
			return
		}

		event, originServerTS, err := h.opts.Join.FederatedEventAtTimestamp(r.Context(),
			string(fedRequest.Origin()), r.PathValue("roomID"), milliseconds, forward)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// `origin_server_ts` in the envelope is the moment of the event found, not of this answer: the
		// peer is looking for a place in time, and the pair of the event and where it actually sat is
		// what it needs to page around.
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"origin":           string(h.opts.ServerName),
			"origin_server_ts": originServerTS,
			"pdu":              raw2json(event),
		})
	}
}

// raw2json keeps a nil event from being encoded as `null` where the specification wants an object; an
// empty answer is a 404 from the room server, so this is a guard rather than a path.
func raw2json(raw []byte) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	return json.RawMessage(raw)
}
