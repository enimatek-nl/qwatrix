package fedapi

import (
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/tidwall/gjson"
)

// handleSendLeave serves `PUT /_matrix/federation/v1/send_leave/{roomID}/{eventID}`, the older
// federation API's way of ending a membership.
//
// Room v11 moved leaves into the ordinary event stream, so a leave is now pushed inside a
// transaction like any other event. This endpoint predates that and is still what homeservers that
// have not caught up — and complement's reference federation server — use to say that a user is out
// of a room. Refusing it does not make the leave absent: the peer keeps a leave event this server has
// never seen, and the two servers go on with different member lists, which shows up much later as
// events that are refused for members the other side believes left.
//
// The verification is the same as for a relayed event, because it is the same object: a signed event
// whose sender's key says who it is from, checked against the keys that origin publishes. What the
// narrower envelope adds is two path parameters that have to agree with the event, and a promise
// about what kind of event this is. Without the second half the endpoint would be a second `/send`
// with fewer checks behind it, reachable by anybody who can sign for any domain a peer trusts.
//
// The answer is an empty object. There is nothing to report: a leave a server refuses would leave
// the member list on the two sides disagreeing, which is the thing this endpoint is for.
func (h *Handler) handleSendLeave(w http.ResponseWriter, r *http.Request) {
	fedRequest := h.authenticatePeer(w, r)
	if fedRequest == nil {
		return
	}
	origin := string(fedRequest.Origin())
	if h.opts.Join == nil {
		httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not accept federated events"))
		return
	}
	roomID := r.PathValue("roomID")
	eventID := r.PathValue("eventID")

	// Content, not r.Body: verifying the request consumed the body.
	event, reason := h.parseRelayedEvent(r.Context(), fedRequest.Content())
	if reason != "" {
		httpx.RespondError(w, r, matrix.NewForbidden("%s", reason))
		return
	}
	if event.EventID() != eventID {
		httpx.RespondError(w, r, matrix.NewInvalidParam(
			"the event was sent for %q but is %q", eventID, event.EventID()))
		return
	}
	if event.RoomID().String() != roomID {
		// The path is the room the peer is trusted in. A body naming another one is a peer
		// trying to write state where it was not admitted, and the signature cannot help it:
		// the signature says which server sent it, not which rooms that server is in.
		httpx.RespondError(w, r, matrix.NewForbidden(
			"the event names room %q but the request names %q", event.RoomID().String(), roomID))
		return
	}
	if event.Type() != "m.room.member" || event.StateKey() == nil {
		httpx.RespondError(w, r, matrix.NewInvalidParam(
			"send_leave takes a membership event, not %q", event.Type()))
		return
	}
	switch membershipOf(fedRequest.Content()) {
	case "leave", "ban", "knock":
		// The three ways this endpoint ends a membership: on its own, by being told to go,
		// and by withdrawing a knock.
	default:
		httpx.RespondError(w, r, matrix.NewInvalidParam(
			"send_leave takes a leave, ban or knock, not a membership of %q", membershipOf(fedRequest.Content())))
		return
	}

	// The same admission a relayed event goes through, gaps and all: a leave that arrives before
	// the join it follows is still a leave the room needs, and refusing it for a missing
	// predecessor is how two servers stay out of step forever.
	if err := h.admitWithGapFill(r.Context(), origin, roomID, event); err != nil {
		httpx.RespondError(w, r, matrix.NewForbidden("%s", describeForPeer(err)))
		return
	}
	httpx.RespondJSON(w, http.StatusOK, struct{}{})
}

// handleEventAuth serves `GET /_matrix/federation/v1/event_auth/{roomID}/{eventID}` and its v2.
//
// A peer that is being shown an event it has never seen needs the rules that allowed it, and cannot
// evaluate an event from its own `auth_events` alone — each of those was itself authorised by others.
// So this endpoint hands back the closure: the event, and everything that authorised it, and
// everything that authorised those. Without it a server receiving a history has to either believe
// whoever is relaying or refuse the room outright, and neither is federation.
//
// v2 adds a `rooms` field for auth events that are not in the room any more. This server's chain is
// read out of the events it stores, so it has no such leftovers to describe and answers with an empty
// set — which the specification allows, and which a peer reads as "nothing more is coming", not as an
// error.
func (h *Handler) handleEventAuth(withRooms bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authenticatePeer(w, r) == nil {
			return
		}
		if h.opts.Join == nil {
			httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not serve federation reads"))
			return
		}
		roomID := r.PathValue("roomID")
		eventID := r.PathValue("eventID")
		chain, err := h.opts.Join.AuthChainFor(r.Context(), roomID, eventID)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		answer := map[string]any{"auth_chain": chain}
		if withRooms {
			answer["rooms"] = map[string]any{}
		}
		httpx.RespondJSON(w, http.StatusOK, answer)
	}
}

// membershipOf reads the membership out of an event body. It is read from the raw JSON rather than
// the parsed event because a membership content type would have to be trusted to be well-formed
// first, and this is a refusal path as much as an accept path.
func membershipOf(raw []byte) string {
	return gjson.GetBytes(raw, "content.membership").String()
}
