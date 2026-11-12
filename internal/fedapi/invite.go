package fedapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
)

// handleSendInvite serves `PUT /_matrix/federation/v2/invite/{roomID}/{eventID}` and its v1 form,
// which is how an inviter asks this server to co-sign an invitation for one of its own users.
//
// An invite carries two signatures and this endpoint is the second one. The inviter's signature says
// the invitation was issued; this server's says the user in it is somebody it looks after and is
// willing to receive the invitation on their behalf. The invitee will act on that event — see the room
// in their client, join from it, decrypt history with it — and the only party between a user and an
// event about them is the server that user trusts with their account. Peers therefore refuse an invite
// with only one signature, so a server that does not answer this endpoint cannot invite anyone who
// lives elsewhere: its event arrives inside an ordinary transaction, is refused there for the wrong
// number of signatures, and the transaction's 200 answer hides that from everybody.
//
// v1 differs only in envelope: it sends the event and nothing else, and a server receiving it must
// assume room version 1 or 2. Since this server has to know the room version to redact the event the
// right way before signing, v1 asks the room itself, and refuses when the room is unknown rather than
// guessing a version — a signature made under the wrong room version is worse than no signature,
// because it is indistinguishable from a corrupted event.
//
// What this server will not do is sign just anything. The state key has to name one of its own users,
// the event has to be an invite, the identifiers in the path have to agree with the event, and the
// inviter must already have signed it: an endpoint that added a signature to an unsigned event would
// let any peer put an invitation in front of any user with any name in it.
func (h *Handler) handleSendInvite(advertisesRoomVersion bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fedRequest := h.authenticatePeer(w, r)
		if fedRequest == nil {
			return
		}
		if h.opts.Join == nil {
			httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not accept federated events"))
			return
		}
		body := fedRequest.Content()
		roomID := r.PathValue("roomID")
		eventID := r.PathValue("eventID")

		eventJSON := gjson.GetBytes(body, "event").Raw
		if eventJSON == "" {
			// The legacy form puts the event at the top level.
			eventJSON = string(body)
		}
		if !events.IsInvite([]byte(eventJSON)) {
			httpx.RespondError(w, r, matrix.NewInvalidParam(
				"an invite request carries an invitation, not a %q",
				gjson.GetBytes([]byte(eventJSON), "type").String()))
			return
		}
		stateKey := gjson.GetBytes([]byte(eventJSON), "state_key").String()
		if stateKey == "" {
			httpx.RespondError(w, r, matrix.NewInvalidParam("an invite names the user it invites"))
			return
		}
		if _, domain, err := matrix.ParseUserID(stateKey, string(h.opts.ServerName)); err != nil ||
			domain != string(h.opts.ServerName) {
			// The signature being offered says "we will give this to our user". Asked about a user
			// this server does not have, it would attest to a fact about another server's users, and
			// any peer could arrange that with a signature of its own.
			httpx.RespondError(w, r, matrix.NewForbidden(
				"%q is not a user of this homeserver", stateKey))
			return
		}
		if got := gjson.GetBytes([]byte(eventJSON), "room_id").String(); got != roomID {
			httpx.RespondError(w, r, matrix.NewForbidden(
				"the invite names room %q but the request names %q", got, roomID))
			return
		}

		version, held, err := h.roomVersionForInvite(r.Context(), roomID, body, advertisesRoomVersion)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		impl, err := h.opts.Join.Events().RoomVersion(string(version))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// The inviter signs first. Without that check this endpoint would be a signing oracle for any
		// peer with an idea, since the signature this server adds is the one a recipient trusts.
		if reason := h.verifyInviter(r.Context(), impl, eventJSON); reason != "" {
			httpx.RespondError(w, r, matrix.NewForbidden("%s", reason))
			return
		}
		coSigned, err := h.opts.Join.Events().CoSignEvent(string(version), eventJSON)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if got := gjson.GetBytes(coSigned, "event_id").String(); got != "" && got != eventID {
			httpx.RespondError(w, r, matrix.NewInvalidParam(
				"the invite was asked for as %q but is %q", eventID, got))
			return
		}

		// The event this server signs has to be one this server itself accepts: a signature on an
		// event the room would reject is a forgery of this server's judgement.
		if _, reason := h.parseRelayedEvent(r.Context(), coSigned); reason != "" {
			httpx.RespondError(w, r, matrix.NewForbidden("%s", reason))
			return
		}
		// Holding the room means this server can tell its user about the invitation now, which is the
		// other half of accepting an invite. A room it has never heard of is signed all the same: the
		// inviter is owed the signature, and the stripped state in the request exists so that a
		// recipient could describe a room it is being invited to for the first time. Remembering
		// invites for rooms this server does not hold is not implemented, and is recorded as a task
		// rather than done by half.
		if held {
			if err := h.admitCoSignedInvite(r.Context(), roomID, coSigned); err != nil {
				httpx.RespondError(w, r, err)
				return
			}
		}

		answer := map[string]any{"event": json.RawMessage(coSigned), "room_version": string(version)}
		httpx.RespondJSON(w, http.StatusOK, answer)
	}
}

// roomVersionForInvite settles which room version to redact under, since the signature depends on it.
//
// A v2 request states the version; v1 does not, and the specification's fallback ("assume 1 or 2")
// describes what a *recipient* of a legacy invite may infer, not what this server may sign under. So
// the room's own version is asked for, and an unknown room with no version in the request is refused.
func (h *Handler) roomVersionForInvite(
	ctx context.Context, roomID string, body []byte, advertisedInBody bool,
) (version gomatrixserverlib.RoomVersion, held bool, err error) {
	known, roomErr := h.opts.Join.RoomVersionFor(ctx, roomID)
	if roomErr == nil {
		return known.Version(), true, nil
	}
	if advertisedInBody {
		if declared := gjson.GetBytes(body, "room_version").String(); declared != "" {
			resolved, resolveErr := h.opts.Join.Events().ResolveRoomVersion(declared)
			if resolveErr != nil {
				return "", false, resolveErr
			}
			return resolved, false, nil
		}
	}
	return "", false, matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
}

// verifyInviter checks that the server which issued the invitation signed it, and says why not.
//
// This is deliberately the same check the library makes when it verifies an event, rather than a
// hand-rolled one: the document is the room version's reference redaction, the timestamp is the
// event's own, and the key is the one the sender's server publishes at that moment. Anything looser
// accepts a signature over bytes no verifier will check.
func (h *Handler) verifyInviter(
	ctx context.Context, impl gomatrixserverlib.IRoomVersion, eventJSON string,
) string {
	if h.keyRing == nil {
		return "this homeserver cannot verify event signatures"
	}
	sender := gjson.Get(eventJSON, "sender").String()
	user, err := spec.NewUserID(sender, true)
	if err != nil {
		return "the invite names a sender that is not a user ID"
	}
	reference, err := impl.RedactEventJSON([]byte(eventJSON))
	if err != nil {
		return "the invite cannot be redacted for verification"
	}
	results, err := h.keyRing.VerifyJSONs(ctx, []gomatrixserverlib.VerifyJSONRequest{{
		ServerName:           spec.ServerName(user.Domain()),
		AtTS:                 spec.Timestamp(gjson.Get(eventJSON, "origin_server_ts").Int()),
		Message:              reference,
		ValidityCheckingFunc: gomatrixserverlib.StrictValiditySignatureCheck,
	}})
	if err != nil {
		return describeForPeer(err)
	}
	if len(results) == 0 {
		return "the inviter's signature could not be verified"
	}
	if results[0].Error != nil {
		return "the inviter's signature could not be verified"
	}
	return ""
}

// admitCoSignedInvite writes the invitation into the room so that this server's user can see it.
func (h *Handler) admitCoSignedInvite(ctx context.Context, roomID string, eventJSON json.RawMessage) error {
	event, reason := h.parseRelayedEvent(ctx, eventJSON)
	if reason != "" {
		return matrix.NewForbidden("%s", reason)
	}
	return h.opts.Join.AcceptFederationEvent(ctx, roomID, event)
}
