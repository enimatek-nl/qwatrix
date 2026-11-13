package fedapi

import (
	"log/slog"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// handleMakeLeave answers `GET /_matrix/federation/v1/make_leave/{roomID}/{userID}` and its v2 — the
// draft a leaving server signs before it hands the leave to `send_leave` below.
//
// It is unsigned on purpose, like `make_join`. The answer is a place in this room's graph and nothing
// else: the leaving user's own server decides when the leave happened and signs that, which is why this
// handler can be reached before either server has any reason to trust the other. A draft that arrived
// *signed* by this server would be this server stating a fact about somebody else's user.
func (h *Handler) handleMakeLeave(w http.ResponseWriter, r *http.Request) {
	if h.opts.Join == nil {
		httpx.RespondError(w, r, matrix.NewForbidden("this server does not take federated leaves"))
		return
	}
	// Signed, unlike `make_join`. Every federation request is signed in practice, so this costs nothing
	// real, and what this endpoint reveals is a room's membership and where its history ends — a leave
	// draft also confirms that the named user *is* in the room, which is a fact about a person rather
	// than an invitation to come in.
	if h.authenticatePeer(w, r) == nil {
		return
	}
	template, err := h.opts.Join.MakeLeaveTemplate(r.Context(), r.PathValue("roomID"), r.PathValue("userID"))
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	event, err := marshalTemplate(template.Event)
	if err != nil {
		slog.Error("fedapi: encoding a leave template", "error", err)
		httpx.RespondError(w, r, matrix.NewUnknown("could not offer a leave template"))
		return
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]any{
		"room_version": string(template.RoomVersion),
		"event":        event,
	})
}

// handleSendLeave serves `PUT /_matrix/federation/v1/send_leave/{roomID}/{eventID}` and its v2, the
// older federation API's way of ending a membership.
//
// Both are registered because peers ask them in the opposite order to the numbering. v1 is one of the
// endpoints whose federation answer used to be the unsigned `[200, body]` pair, which is why v2 exists
// with the object on its own; gomatrixserverlib's client asks v2 first and only falls back to v1 on a
// 404, parsing v1's answer as that legacy pair. Serving v1 alone therefore reads, to a modern peer, as
// a broken endpoint: its fallback takes this server's honest `{}` and fails to see two elements in
// it, after the leave was already accepted.
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

	// Shape before signature. An event that could not be a leave no matter who signed it is a bad event,
	// and a peer told "forbidden" about a bad event reads a permanent policy about something it could have
	// built correctly — and will keep retrying it forever. The checks that need the verified event, that
	// its ID and room match the path, follow below.
	if reason := membershipShapeProblem(fedRequest.Content(), "send_leave", "leave"); reason != "" {
		httpx.RespondError(w, r, matrix.NewInvalidParam("%s", reason))
		return
	}
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
		// 400 because the event is the thing that is wrong: a peer told "forbidden" reads that as a
		// permanent answer about an event it could perfectly well have built correctly, and stops trying.
		httpx.RespondError(w, r, matrix.NewInvalidParam(
			"send_leave takes a membership event, not %q", event.Type()))
		return
	}
	// This endpoint takes a leave and nothing else; the shape gate above is where that is enforced.
	//
	// It used to take a ban or a knock as well, on the
	// reasoning that all three end a membership — which is true of the effect and false of the protocol:
	// a ban is state that travels in a transaction, a knock has its own endpoints, and neither has been
	// through the handshake that makes the sender entitled to write that membership. `send_leave` is the
	// narrow door with no state exchange and no co-signing behind it, so the narrower the promise the more
	// it can be trusted; this is also exactly what the specification says and what Synapse enforces.

	// The same admission a relayed event goes through, gaps and all: a leave that arrives before
	// the join it follows is still a leave the room needs, and refusing it for a missing
	// predecessor is how two servers stay out of step forever.
	// A nil budget: this is one event per request, so the request and the transaction are the same
	// thing and the walk's own ceiling is the only one that can be reached.
	if err := h.admitWithGapFill(r.Context(), nil, origin, roomID, event); err != nil {
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
		fedRequest := h.authenticatePeer(w, r)
		if fedRequest == nil {
			return
		}
		if h.opts.Join == nil {
			httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not serve federation reads"))
			return
		}
		roomID := r.PathValue("roomID")
		eventID := r.PathValue("eventID")
		chain, err := h.opts.Join.AuthChainFor(r.Context(), roomID, eventID, string(fedRequest.Origin()))
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
