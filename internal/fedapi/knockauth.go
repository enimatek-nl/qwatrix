package fedapi

// Letting a peer ask to be let in.
//
// `make_knock` and `send_knock` are the knock's two halves, and they carry the trust in opposite hands.
//
// `make_knock` asks this server for a place in a room's graph for a user it has never seen. It is served
// unsigned and it decides nothing: the answer is a draft, and a draft becomes a knock only when the
// knocking server signs it as its own.
//
// `send_knock` is where a claim arrives, and it is the reason a knock is not a private note. A knock is a
// membership event that the room's members will see and can answer, so this server checks that the event
// is a knock, that it speaks for the user whose server signed it, and that the room's own join rule lets
// the knock happen at all. Only then is it written — and the peer goes away holding the room's front door
// (`knock_room_state`), which is what the knocking user's client shows them while they wait.

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/tidwall/gjson"
)

// handleMakeKnock answers `GET /_matrix/federation/v1/make_knock/{roomID}/{userID}` and its v2.
//
// Signed, like `make_leave` and for the same reason: the answer names this room's version and where its
// history ends, and asking about a room is not the same as being admitted to it.
func (h *Handler) handleMakeKnock(w http.ResponseWriter, r *http.Request) {
	if h.opts.Join == nil {
		httpx.RespondError(w, r, matrix.NewForbidden("this server does not take federated knocks"))
		return
	}
	if h.authenticatePeer(w, r) == nil {
		return
	}
	template, err := h.opts.Join.MakeKnockTemplate(r.Context(), r.PathValue("roomID"),
		r.PathValue("userID"), r.URL.Query()["ver"])
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	event, err := marshalTemplate(template.Event)
	if err != nil {
		slog.Error("fedapi: encoding a knock template", "error", err)
		httpx.RespondError(w, r, matrix.NewUnknown("could not offer a knock template"))
		return
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]any{
		"room_version": string(template.RoomVersion),
		"event":        event,
	})
}

// handleSendKnock serves `PUT /_matrix/federation/v1/send_knock/{roomID}/{eventID}` and its v2.
//
// The body is read in either of the two shapes peers actually send. The specification wraps the event in
// an envelope (`origin`, `origin_server_ts`, `event`, and in v2 the `state` the knocker already holds);
// gomatrixserverlib's own client, and complement's reference federation server, put the event itself in
// the body and name the origin in the request signature instead. Both say the same thing, and the second
// one is what this server will actually receive, so a handler that insisted on the envelope would answer
// every real knock with a 400.
func (h *Handler) handleSendKnock(w http.ResponseWriter, r *http.Request) {
	if h.opts.Join == nil {
		httpx.RespondError(w, r, matrix.NewForbidden("this server does not take federated knocks"))
		return
	}
	fedRequest := h.authenticatePeer(w, r)
	if fedRequest == nil {
		return
	}
	origin := string(fedRequest.Origin())
	roomID := r.PathValue("roomID")
	eventID := r.PathValue("eventID")

	content := fedRequest.Content()
	if wrapped := gjson.GetBytes(content, "event"); wrapped.Exists() &&
		!gjson.GetBytes(content, "type").Exists() {
		content = json.RawMessage(wrapped.Raw)
	}

	// Shape first, and 400 for it: an event that is not a knock is a bad event whatever it turns out
	// being signed by, and a peer told "forbidden" about it will resend it unchanged forever.
	if reason := membershipShapeProblem(content, "send_knock", "knock"); reason != "" {
		httpx.RespondError(w, r, matrix.NewInvalidParam("%s", reason))
		return
	}

	event, reason := h.parseRelayedEvent(r.Context(), content)
	if reason != "" {
		httpx.RespondError(w, r, matrix.NewForbidden("%s", reason))
		return
	}
	if event.EventID() != eventID {
		httpx.RespondError(w, r, matrix.NewForbidden("the event ID in the path does not match the event"))
		return
	}

	// Who the knock speaks for, and whether this room's join rule admits knocking at all, is decided by
	// the room server — the same rules a peer applies when this server knocks on its door.
	if err := h.opts.Join.CheckKnock(r.Context(), roomID, origin, event); err != nil {
		httpx.RespondError(w, r, err)
		return
	}

	// The same admission a relayed event goes through, gaps and all: a knock whose predecessor has not
	// arrived yet is still a knock this room should record, and refusing it for a missing predecessor is
	// how two servers stay out of step forever.
	if err := h.admitWithGapFill(r.Context(), nil, origin, roomID, event); err != nil {
		httpx.RespondError(w, r, err)
		return
	}

	// What the peer asked for, in the shape it expects: the room's front door, for a user who is not in
	// the room. An empty list is a legitimate answer — a room that has been knocked on before it has any
	// state at all — so it is written as `[]` rather than dropped or left null, because a peer that has to
	// distinguish the two will eventually guess wrong.
	state, err := h.opts.Join.KnockStateForUser(r.Context(), roomID, string(event.SenderID()))
	if err != nil {
		slog.Warn("fedapi: could not assemble the room state for a knock",
			"room_id", roomID, "user_id", string(event.SenderID()), "error", err)
		state = []json.RawMessage{}
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]any{
		"knock_room_state": state,
	})
}
