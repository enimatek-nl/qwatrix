package fedapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/matrix-org/gomatrixserverlib"
	fclient "github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
)

// transactionIDPattern is the shape the specification allows a transaction ID to have. It is also
// a path segment, so refusing anything else keeps a malformed ID from being a routing problem.
var transactionIDPattern = regexp.MustCompile(`^[0-9A-Za-z\-_]{1,255}$`)

// transactionCacheEntries bounds the remembered transaction results.
const transactionCacheEntries = 1024

// handleSendTransaction serves PUT /_matrix/federation/v1/send/{transactionID} (and v2).
//
// This is how the rest of the federation tells this server that something happened: a room event
// a peer sent, a member joining, a redaction, a typing indicator. There is no discovery and no
// subscription — peers push, and this server decides event by event whether what it was given is
// one it will keep.
//
// Two properties make this endpoint worth being careful with.
//
// It is **idempotent by transaction ID**. A peer that does not get a response — a timeout, a
// dropped connection — will send the same transaction again, and this server must not apply the
// same event twice. The specification asks for the cached answer; serving it also stops a retry
// from being charged as new work.
//
// Each event is accepted or refused **on its own**. A transaction whose third event is bogus is
// not a transaction that failed: the other two are kept and reported as kept, and the peer is told
// which one did not land and why. Refusing the whole transaction would make one bad event from one
// user a permanent block on a room's progress.
func (h *Handler) handleSendTransaction(w http.ResponseWriter, r *http.Request) {
	fedRequest := h.authenticatePeer(w, r)
	if fedRequest == nil {
		return
	}
	origin := string(fedRequest.Origin())
	transactionID := r.PathValue("transactionID")
	if !transactionIDPattern.MatchString(transactionID) {
		httpx.RespondError(w, r, matrix.NewInvalidParam("transaction ID %q is not a valid identifier", transactionID))
		return
	}
	if h.opts.Join == nil {
		httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not accept federated events"))
		return
	}

	// Content, not r.Body: verifying the request consumed the body.
	var transaction gomatrixserverlib.Transaction
	if err := json.Unmarshal(fedRequest.Content(), &transaction); err != nil && !errors.Is(err, io.EOF) {
		httpx.RespondError(w, r, matrix.NewBadJSON("the transaction cannot be read"))
		return
	}
	// The signed origin is the identity; a body claiming to be from somebody else is refused
	// rather than believed, because the per-event signatures below are checked against keys
	// fetched from *that* origin's well-known endpoint.
	if string(transaction.Origin) != origin {
		httpx.RespondError(w, r, matrix.NewForbidden("the transaction names %q but was signed by %q",
			string(transaction.Origin), origin))
		return
	}

	// A retry gets the previous answer. Peers reuse transaction IDs only for retries, so a hit is
	// not a coincidence worth re-processing: the events are already stored, and re-running them
	// would re-notify every client for work already done.
	if cached, ok := h.cachedTransaction(origin, transactionID); ok {
		httpx.RespondJSON(w, http.StatusOK, cached)
		return
	}

	h.handleEDUs(r.Context(), transaction)
	result := h.ingestPDUs(r.Context(), origin, transaction)
	h.rememberTransaction(origin, transactionID, result)
	httpx.RespondJSON(w, http.StatusOK, result)
}

// ingestPDUs admits each event of a transaction separately, which is what the answer reports.
func (h *Handler) ingestPDUs(ctx context.Context, origin string, transaction gomatrixserverlib.Transaction) fclient.RespSend {
	result := fclient.RespSend{PDUs: make(map[string]fclient.PDUResult, len(transaction.PDUs))}
	for _, raw := range transaction.PDUs {
		event, reason := h.parseRelayedEvent(ctx, raw)
		if reason != "" {
			result.PDUs[h.eventIDHint(ctx, raw)] = fclient.PDUResult{Error: reason}
			continue
		}
		roomID := event.RoomID().String()
		// Gap filling first: an event that follows history this server has never been told about is
		// worth one bounded round of fetching before it is refused.
		if err := h.admitWithGapFill(ctx, origin, roomID, event); err != nil {
			slog.Debug("fedapi: refused a federated event",
				"room_id", roomID, "event_id", event.EventID(),
				"origin", origin, "error", err)
			result.PDUs[event.EventID()] = fclient.PDUResult{Error: describeForPeer(err)}
			continue
		}
		result.PDUs[event.EventID()] = fclient.PDUResult{}
	}
	return result
}

// parseRelayedEvent reads one event of a transaction and checks the signature that says who sent
// it.
//
// The distinction from a `send_join` is worth stating out loud: a relayed event need not have
// originated at the server delivering it. Federation expects peers to forward what they receive, so
// the transaction's origin is not required to own the sender's domain — the event's own signature,
// checked against the sender's published key, is what says who it came from.
//
// A reason to refuse is returned rather than an error because the caller must report it per event.
func (h *Handler) parseRelayedEvent(ctx context.Context, raw json.RawMessage) (gomatrixserverlib.PDU, string) {
	if len(raw) == 0 {
		return nil, "the event is empty"
	}
	roomID := gjson.GetBytes(raw, "room_id").String()
	roomVersion, err := h.opts.Join.RoomVersionFor(ctx, roomID)
	if err != nil {
		slog.Debug("fedapi: could not find the room for a federated event", "room_id", roomID, "error", err)
		return nil, "the room is not one this homeserver has"
	}
	event, err := roomVersion.NewEventFromUntrustedJSON(raw)
	if err != nil {
		return nil, "the event cannot be read"
	}
	if h.keyRing == nil {
		// Without a key ring there is nothing to check a signature against. An event this server
		// cannot attribute is not one it should keep.
		return nil, "this homeserver cannot verify event signatures"
	}
	if err := gomatrixserverlib.VerifyEventSignatures(ctx, event, h.keyRing, h.opts.Join.Events().UserIDForSender()); err != nil {
		slog.Debug("fedapi: a federated event's signature did not verify",
			"event_id", event.EventID(), "sender", string(event.SenderID()), "error", err)
		// The wording is deliberately vague to an outside peer: whether a key is missing or simply
		// does not match is not this server's business, and it says more than needed about ours.
		return nil, "the event's signature could not be verified"
	}
	return event, ""
}

// handleEDUs applies the ephemeral part of a transaction.
//
// Ephemeral events are explicitly not persisted and not part of the room graph — typing, receipts,
// to-device messages, presence. They are also unacknowledged: the response has no per-EDU result, so
// a peer is told nothing about whether one landed. That makes an unimplemented one easy to accept
// silently, which is exactly the failure worth naming: the sender believes it told this server
// something and this server has thrown it away.
//
// None of them are applied by this server yet. Every one is parsed — a transaction containing a
// malformed EDU must still deliver its events — and every one is dropped with a debug log naming its
// type, and the work is tracked as task T706. What must not happen is the silence:
// a peer cannot be told an ephemeral event failed, so a server that quietly discards typing
// indicators looks, from the other side, like a server where nobody is ever typing. A debug line
// naming the type is the only trace that this server chose not to act rather than acting and losing
// it somewhere else.
func (h *Handler) handleEDUs(ctx context.Context, transaction gomatrixserverlib.Transaction) {
	for _, edu := range transaction.EDUs {
		if edu.Type != deviceListUpdateEDU {
			slog.Debug("fedapi: ignored an ephemeral event this homeserver does not act on",
				"edu_type", edu.Type, "origin", transaction.Origin)
			continue
		}
		h.applyDeviceListUpdate(ctx, transaction.Origin, edu.Content)
	}
}

// applyDeviceListUpdate records that a peer's user has changed their encryption devices, so that the
// people who share a room with them find out on their next sync.
//
// The sender is checked, not believed. A device list belongs to the server that hosts the user, and an
// update about `@alice:other.example` arriving from anywhere else is either a mistake or an attempt to
// have this server throw away Alice's keys on somebody's say-so. Note what is *not* checked here: the
// update is not signed by the device it describes, which is what the specification asks for and what
// this server does not yet verify because it does not yet send it either (see [Outbox.SendDeviceListUpdate]).
func (h *Handler) applyDeviceListUpdate(ctx context.Context, origin spec.ServerName, content spec.RawJSON) {
	if h.opts.DeviceLists == nil || len(content) == 0 {
		return
	}
	userID := gjson.GetBytes(content, "user_id").String()
	if userID == "" {
		slog.Debug("fedapi: a device-list update named nobody", "origin", origin)
		return
	}
	_, domain, err := matrix.ParseUserID(userID, string(h.opts.ServerName))
	if err != nil {
		slog.Debug("fedapi: a device-list update named an unusable user", "origin", origin, "user_id", userID)
		return
	}
	if string(domain) != string(origin) {
		// The claim and the identity behind it disagree, and only one of them can be right.
		slog.Warn("fedapi: a device-list update arrived from a server that does not host the user",
			"origin", origin, "user_id", userID)
		return
	}
	deleted := gjson.GetBytes(content, "deleted").Bool()
	if err := h.opts.DeviceLists.Announce(ctx, userID, deleted); err != nil {
		slog.Warn("fedapi: could not record a device-list update from a peer",
			"origin", origin, "user_id", userID, "error", err)
		return
	}
	slog.Debug("fedapi: recorded a device-list update from a peer",
		"origin", origin, "user_id", userID, "deleted", deleted)
}

// describeForPeer turns an ingest error into what a peer server is told.
//
// A Matrix error already carries a message written for a reader who is not this process; anything
// else is this server's own problem — a database failure, a nil pointer — and that stays in the log.
func describeForPeer(err error) string {
	var apiErr *matrix.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	slog.Error("fedapi: accepting a federated event failed", "error", err)
	return "internal error"
}

// eventIDHint labels a PDU this server is refusing without having accepted it — and, from room
// version 3 onwards, an event does not contain its own ID. The ID is a hash *of* the event, computed
// by the rules of the room's version, which is precisely the information missing from the refusals
// that need a label most: an event for a room this server cannot find has no known version.
//
// So the answer falls back to the version this server creates rooms with. That is a label on a
// response and not a security decision: if it is wrong, the peer receives a result keyed by an ID it
// does not recognise, which is strictly more useful than the empty key it would otherwise get and
// which it cannot match to anything either. What must not happen is a refusal going unreported
// because an ID could not be named — a peer would read that as success and stop retrying.
func (h *Handler) eventIDHint(ctx context.Context, raw json.RawMessage) string {
	if id := gjson.GetBytes(raw, "event_id").String(); id != "" {
		// The oldest room versions do carry the ID, because back then it was an ID and not a hash.
		return id
	}
	fallback, err := gomatrixserverlib.GetRoomVersion(h.opts.Join.Events().DefaultRoomVersion())
	if err != nil {
		return ""
	}
	event, err := fallback.NewEventFromUntrustedJSON(raw)
	if err != nil {
		// Genuinely unreadable JSON. There is nothing to call it, and an empty key at least tells the
		// peer that something in the transaction was not an event.
		return ""
	}
	return event.EventID()
}

// cachedTransaction returns the answer previously given for this origin's transaction ID.
func (h *Handler) cachedTransaction(origin, transactionID string) (fclient.RespSend, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	cached, ok := h.transactions[origin+"\x00"+transactionID]
	return cached.result, ok
}

// rememberTransaction stores an answer for a retry.
//
// The cache is bounded and evicts in insertion order. It is in-process on purpose: a restart loses
// it, and a peer that retries after a restart re-sends events this server still holds, where the
// per-event "already have it" check does the idempotency work the cache was only making faster.
func (h *Handler) rememberTransaction(origin, transactionID string, result fclient.RespSend) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.transactions == nil {
		h.transactions = make(map[string]transactionRecord, transactionCacheEntries)
		h.transactionOrder = nil
	}
	key := origin + "\x00" + transactionID
	if _, exists := h.transactions[key]; exists {
		return
	}
	h.transactions[key] = transactionRecord{result: result, seenAt: time.Now()}
	h.transactionOrder = append(h.transactionOrder, key)
	for len(h.transactionOrder) > transactionCacheEntries {
		oldest := h.transactionOrder[0]
		h.transactionOrder = h.transactionOrder[1:]
		delete(h.transactions, oldest)
	}
}
