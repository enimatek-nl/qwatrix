package fedapi

// Filling a peer's gap in this server's room history.
//
// A peer asks this question when an event it is being shown names history this server holds and it does
// not. Refusing is not a neutral answer: the peer cannot accept the event it was told about, so it refuses
// it, and both servers are then holding different versions of the same room — a divergence that shows up
// much later as messages that never arrive, with no error anywhere to point at.
//
// The request names two ends rather than a range, and the answer is everything in between, oldest first.
// The room server owns the walk and the room's own rules about what the asking server may see; this file is
// the envelope.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// MissingEventSource is the half of gap filling that answers the question rather than asking it.
type MissingEventSource interface {
	// MissingEventsForPeer returns the events between two ends of a room's history, oldest first,
	// bounded by what the asking server is entitled to read.
	MissingEventsForPeer(ctx context.Context, roomID, origin string, earliest, latest []string,
		limit int, minDepth int64) ([]json.RawMessage, error)
}

// handleGetMissingEvents serves `POST /_matrix/federation/v1/get_missing_events/{roomID}`.
func (h *Handler) handleGetMissingEvents(w http.ResponseWriter, r *http.Request) {
	if h.opts.MissingEvents == nil {
		// A refusal a peer can read, rather than an empty list: "this server cannot fill gaps" and "there
		// was nothing missing" are different facts, and the second tells the peer its history is complete.
		httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not fill history gaps over federation"))
		return
	}
	fedRequest := h.authenticatePeer(w, r)
	if fedRequest == nil {
		return
	}
	var request struct {
		EarliestEvents []string `json:"earliest_events"`
		LatestEvents   []string `json:"latest_events"`
		Limit          int      `json:"limit"`
		MinDepth       int64    `json:"min_depth"`
	}
	if err := json.Unmarshal(fedRequest.Content(), &request); err != nil {
		httpx.RespondError(w, r, matrix.NewNotJSON("a missing-events request must name the events it is between"))
		return
	}
	roomID := r.PathValue("roomID")
	events, err := h.opts.MissingEvents.MissingEventsForPeer(r.Context(), roomID,
		string(fedRequest.Origin()), request.EarliestEvents, request.LatestEvents,
		request.Limit, request.MinDepth)
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	if events == nil {
		// An empty array, because the peer is about to iterate the answer.
		events = []json.RawMessage{}
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]any{"events": events})
}
