// Package fedapi serves the Matrix server-server (federation) API.
//
// Federation is a second protocol with its own trust model, and it is served on its own TLS
// listener for that reason: everything here is authenticated by signatures over canonical JSON
// rather than by an access token, and a request that arrives unauthenticated is a peer asking who
// this server is.
//
// The API is being built in stages. What is served today is the part a peer needs before it can
// believe anything else: what implementation this is, and which signing key the peer must believe
// when it checks an event. Later stages add state, events, the join handshake, and transaction
// ingest; each one arrives with the verification the previous stage makes possible.
package fedapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/identity"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/matrix-org/gomatrixserverlib"
	fclient "github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// JoinServer is what the join handshake needs from the room server: the place in the graph a peer
// should attach its join event to, and (in a later stage) the acceptance of the signed result.
type JoinServer interface {
	MakeJoinTemplate(ctx context.Context, roomID, userID string, requested []string) (*roomserver.JoinTemplate, error)
	AcceptFederationJoin(ctx context.Context, roomID string, origin string, event gomatrixserverlib.PDU) (*roomserver.JoinAcceptance, error)
	// AcceptFederationEvent admits one event a peer relayed about a room this server holds. Unlike
	// a join, the event need not have originated at this peer, so the room server is not told who
	// relayed it — the event's own signature is what identifies its sender.
	AcceptFederationEvent(ctx context.Context, roomID string, event gomatrixserverlib.PDU) error
	// RoomVersionFor and Events give the transport the two things it needs to turn a request body
	// into a checked event: the room's own version, and the mapping from a sender ID back to the
	// user ID it stands for.
	RoomVersionFor(ctx context.Context, roomID string) (gomatrixserverlib.IRoomVersion, error)
	Events() *events.Service
	// AuthChainFor answers a peer's request for the events that authorised one event.
	AuthChainFor(ctx context.Context, roomID, eventID string) ([]json.RawMessage, error)
}

// Options configures the server-server handler.
type Options struct {
	// ServerName is the identity this server federates as. It is repeated in the key
	// response because that document is the thing a peer signs its trust in, and a peer
	// that could not tell which server it came from could replay it anywhere.
	ServerName spec.ServerName
	// Version names this implementation to a peer. The specification leaves the string's
	// shape to us; peers display it and use it to guess which quirks to expect.
	Version string
	// Peer is this server's outbound federation client, which doubles as the way its verifier
	// obtains a peer's signing key: to check that a peer is who it claims, this server asks that
	// peer's own server. It may be nil, in which case signed requests are refused rather than
	// accepted on trust.
	Peer *Peer
	// Devices answers a peer's question about the device lists of this server's own users. It may be
	// nil, in which case the endpoint refuses rather than answering that those users have no devices —
	// a peer that hears "no devices" stops trying to encrypt to them, which is a quiet way of deleting
	// somebody's conversations.
	Devices DeviceListSource
	// DeviceLists records that a peer told us one of its users changed their encryption devices. It
	// may be nil, in which case an incoming device-list update is logged and dropped.
	DeviceLists DeviceListAnnouncer
	// Media serves this server's own files to peers. It may be nil, in which case the media endpoint
	// answers that this server does not serve media over federation — a refusal a peer can read, and
	// the right answer for a server whose media repository is switched off.
	Media MediaSource
	// Join accepts federated joins into this server's rooms. It may be nil, in which case the
	// join handshake answers that this server does not take federated joins — a refusal the
	// peer can read, rather than a route it has to guess the meaning of.
	Join JoinServer
}

// Handler serves the server-server API for one homeserver.
type Handler struct {
	identity *identity.Identity
	opts     Options
	mux      *http.ServeMux
	// keyRing verifies peers' signatures by fetching their published keys. Nil when this server
	// has no signing identity to ask with.
	keyRing *gomatrixserverlib.KeyRing

	// mu guards the transaction cache. Peers retry, and each request is served on its own
	// goroutine, so two deliveries of one transaction can arrive at the same moment.
	mu sync.Mutex
	// transactions remembers how each transaction from each origin was answered, so a retry
	// reports the same result instead of applying the same events a second time.
	transactions map[string]transactionRecord
	// transactionOrder is the insertion order behind the cache's bound.
	transactionOrder []string
}

// transactionRecord is one remembered answer to a transaction.
type transactionRecord struct {
	result fclient.RespSend
	seenAt time.Time
}

// New builds the server-server handler. The identity is required: a server that cannot publish a
// signing key cannot be believed by a peer, so serving an anonymous key would be worse than
// refusing to start.
func New(id *identity.Identity, opts Options) http.Handler {
	h := &Handler{identity: id, opts: opts, mux: http.NewServeMux()}
	if opts.Peer != nil {
		h.keyRing = NewKeyRing(opts.Peer, id)
	}
	h.mux.HandleFunc("GET /_matrix/federation/v1/version", h.handleVersion)
	h.mux.HandleFunc("GET /_matrix/federation/v1/make_join/{roomID}/{userID}", h.handleMakeJoin)
	h.mux.HandleFunc("PUT /_matrix/federation/v1/send_join/{roomID}/{eventID}", h.handleSendJoin(true))
	h.mux.HandleFunc("PUT /_matrix/federation/v2/send_join/{roomID}/{eventID}", h.handleSendJoin(false))
	h.mux.HandleFunc("PUT /_matrix/federation/v1/send_leave/{roomID}/{eventID}", h.handleSendLeave)
	h.mux.HandleFunc("PUT /_matrix/federation/v2/send_leave/{roomID}/{eventID}", h.handleSendLeave)
	h.mux.HandleFunc("GET /_matrix/federation/v1/event_auth/{roomID}/{eventID}", h.handleEventAuth(false))
	h.mux.HandleFunc("GET /_matrix/federation/v2/event_auth/{roomID}/{eventID}", h.handleEventAuth(true))
	h.mux.HandleFunc("PUT /_matrix/federation/v1/invite/{roomID}/{eventID}", h.handleSendInvite(false))
	h.mux.HandleFunc("PUT /_matrix/federation/v2/invite/{roomID}/{eventID}", h.handleSendInvite(true))
	h.mux.HandleFunc("PUT /_matrix/federation/v1/send/{transactionID}", h.handleSendTransaction)
	h.mux.HandleFunc("PUT /_matrix/federation/v2/send/{transactionID}", h.handleSendTransaction)
	h.mux.HandleFunc("GET /_matrix/federation/v1/user/devices/{userID}", h.handleUserDevices)
	h.mux.HandleFunc("GET /_matrix/federation/v1/download/{serverName}/{mediaID}", h.handleFederationMediaByServerName)
	h.mux.HandleFunc("GET /_matrix/federation/v1/media/download/{mediaID}", h.handleFederationMedia)
	h.mux.HandleFunc("POST /_matrix/key/v2/query", h.handleKeyQuery)
	h.mux.HandleFunc("GET /_matrix/key/v2/server", h.handleKeys)
	h.mux.HandleFunc("GET /_matrix/key/v2/server/{keyID}", h.handleKeyByID)
	h.mux.HandleFunc("/", h.handleUnknown)
	return h
}

// ServeHTTP makes the handler an http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// handleMakeJoin answers `GET /_matrix/federation/v1/make_join/{roomID}/{userID}`.
//
// The response is an unsigned join event: this server names the place in the graph (previous event,
// depth) and the events that authorise the join, and the peer fills in its own sender, signs it, and
// sends the result back for acceptance. Nothing here is a permission slip — `make_join` is a
// description of what a valid join would look like, and the answer to "may this user join?" is asked
// again, of the signed event, at send_join.
func (h *Handler) handleMakeJoin(w http.ResponseWriter, r *http.Request) {
	if h.opts.Join == nil {
		httpx.RespondError(w, r, matrix.NewForbidden("this server does not accept federated joins"))
		return
	}
	roomID := r.PathValue("roomID")
	userID := r.PathValue("userID")
	template, err := h.opts.Join.MakeJoinTemplate(r.Context(), roomID, userID, r.URL.Query()["ver"])
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	event, err := marshalTemplate(template.Event)
	if err != nil {
		slog.Error("fedapi: encoding a join template", "error", err)
		httpx.RespondError(w, r, matrix.NewUnknown("could not offer a join template"))
		return
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]any{
		"room_version": string(template.RoomVersion),
		"event":        event,
	})
}

// marshalTemplate writes the template event with the fields a peer expects. `sender` is dropped when
// empty rather than sent as "": an empty sender is not the same statement as no sender, and a peer
// that reads the template as an event draft should not have to know that this server means "you fill
// this in".
func marshalTemplate(event gomatrixserverlib.ProtoEvent) (json.RawMessage, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if sender, ok := fields["sender"]; ok && string(sender) == `""` {
		delete(fields, "sender")
	}
	return json.Marshal(fields)
}

// handleVersion answers the peer's first question. There is no route to give a more useful
// answer: a peer that cannot ask this has no way to distinguish a server that does not federate
// from one that has simply been misdirected.
func (h *Handler) handleVersion(w http.ResponseWriter, _ *http.Request) {
	httpx.RespondJSON(w, http.StatusOK, map[string]string{"version": h.opts.Version})
}

// verifyKeyEntry is one entry of `verify_keys`. The key type is the specification's base64
// encoding — unpadded standard base64 — rather than Go's, because a peer decoding this with the
// alphabet the specification names is not going to tolerate padding.
type verifyKeyEntry struct {
	Key spec.Base64Bytes `json:"key"`
}

// keyResponse is the signed server-key document.
type keyResponse struct {
	ServerName spec.ServerName `json:"server_name"`
	// VerifyKeys are the keys a peer may use to check this server's signatures now.
	VerifyKeys map[string]verifyKeyEntry `json:"verify_keys"`
	// OldVerifyKeys are kept for verifying older events. They are always present and
	// usually empty; omitting the field leaves a peer guessing whether the answer was
	// "none" or "not answered".
	OldVerifyKeys map[string]oldVerifyKeyEntry `json:"old_verify_keys"`
	// ValidUntilTS is how far ahead a peer may cache this response.
	ValidUntilTS spec.Timestamp `json:"valid_until_ts"`
	// Signatures is added by signing, which is why it is absent from the built document.
	Signatures map[string]map[string]string `json:"signatures,omitempty"`
}

type oldVerifyKeyEntry struct {
	Key       spec.Base64Bytes `json:"key"`
	ExpiredTS spec.Timestamp   `json:"expired_ts"`
}

// keys serves `GET /_matrix/key/v2/server`.
//
// The response is signed by the key it publishes, which is what lets a peer that has never seen
// this server check the document without trusting an intermediary: the key ID inside it names the
// key whose public half is also inside it, and the signature is over everything but the signature.
// A peer therefore has to believe nothing but the certificate of the TLS connection it opened.
func (h *Handler) keys() (keyResponse, error) {
	keyID, verifyKey := h.identity.NotaryKey()
	body := keyResponse{
		ServerName: h.opts.ServerName,
		VerifyKeys: map[string]verifyKeyEntry{
			string(keyID): {Key: verifyKey.Key},
		},
		OldVerifyKeys: map[string]oldVerifyKeyEntry{},
		ValidUntilTS:  h.identity.ValidUntil(),
	}
	return body, nil
}

func (h *Handler) handleKeys(w http.ResponseWriter, _ *http.Request) {
	h.respondKeys(w)
}

// handleKeyByID answers a peer that already knows which key it wants, which is the normal case:
// an event names the key that signed it, and that is the key the peer comes to look up.
//
// An unknown key ID is a 404 rather than an empty key set. The difference matters to the caller:
// "this server never had that key" is a fact a peer can act on by forgetting events signed with
// it, while an empty 200 is indistinguishable from a server that has not finished answering.
func (h *Handler) handleKeyByID(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("keyID")
	if keyID == "" {
		h.handleKeys(w, r)
		return
	}
	current, _ := h.identity.NotaryKey()
	if keyID != string(current) {
		httpx.RespondError(w, r, matrix.NewNotFound("no signing key %q is published by this server", keyID))
		return
	}
	h.respondKeys(w)
}

// respondKeys signs and writes the key document. Signing is done over the marshalled body with
// the signature fields empty, which is what the verifier undoes: it removes `signatures` and
// `unsigned` and canonicalises what is left.
func (h *Handler) respondKeys(w http.ResponseWriter) {
	signed, err := h.signedKeyDocument()
	if err != nil {
		slog.Error("fedapi: could not publish the signing key", "error", err)
		httpx.RespondError(w, nil, matrix.NewUnknown("could not publish the signing key"))
		return
	}
	httpx.RespondRawJSON(w, http.StatusOK, signed)
}

// signedKeyDocument is this server's published key document, signed by the key it publishes. Both key
// endpoints answer with it: publishing and answering a query about yourself are the same fact, and two
// implementations of it would eventually disagree about who this server is.
func (h *Handler) signedKeyDocument() (json.RawMessage, error) {
	body, err := h.keys()
	if err != nil {
		return nil, fmt.Errorf("building the server-key response: %w", err)
	}
	unsigned, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding the server-key response: %w", err)
	}
	signed, err := h.identity.SignJSON(unsigned)
	if err != nil {
		// A server that cannot sign its own key has a broken identity file, and saying so
		// is better than publishing an unsigned document a peer would reject anyway.
		return nil, fmt.Errorf("signing the published key: %w", err)
	}
	return signed, nil
}

// handleKeyQuery answers `POST /_matrix/key/v2/query`, the batched form of "whose key is this?", in
// which the responder signs the key document it returns so the asker need not reach the origin.
//
// This server is a notary for one server name: its own. The keys it could sign are the keys it holds,
// and it holds nobody else's — relaying another server's document would mean fetching and caching it,
// which is a cache with a security boundary in it and is not this. The specification fits that shape
// exactly: a server whose keys are not known is left out of the result, and an empty result is a
// correct answer rather than an error, because the asker is expected to go and ask somebody else.
//
// Refusing the whole batch over one unknown name would be worse than silence in a different way: a
// peer usually asks for several servers at once, and one stranger would cost it the keys it could
// have had.
func (h *Handler) handleKeyQuery(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ServerKeys map[string]map[string]json.RawMessage `json:"server_keys"`
	}
	if err := httpx.DecodeJSON(r, &request); err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	documents := []json.RawMessage{}
	if _, asked := request.ServerKeys[string(h.opts.ServerName)]; asked {
		signed, err := h.signedKeyDocument()
		if err != nil {
			slog.Error("fedapi: could not answer a key query about ourselves", "error", err)
			httpx.RespondError(w, r, matrix.NewUnknown("could not publish the signing key"))
			return
		}
		documents = append(documents, signed)
	}
	if len(request.ServerKeys) > len(documents) {
		// Not a warning: this is the ordinary case for a peer that has been introduced to more
		// of the federation than we have.
		slog.Debug("fedapi: a key query asked about servers this one holds no keys for",
			"asked_for", len(request.ServerKeys), "answered", len(documents))
	}
	body, err := json.Marshal(map[string]any{"server_keys": documents})
	if err != nil {
		httpx.RespondError(w, r, matrix.NewUnknown("could not answer the key query"))
		return
	}
	httpx.RespondRawJSON(w, http.StatusOK, body)
}

// handleUnknown answers the routes this stage has not implemented yet. The reply is a Matrix
// error rather than Go's plain 404 so a peer can tell "this server does not speak that" from a
// reverse proxy that has lost the request.
func (h *Handler) handleUnknown(w http.ResponseWriter, r *http.Request) {
	slog.Debug("fedapi: unimplemented federation route", "method", r.Method, "path", r.URL.Path)
	httpx.RespondError(w, r, matrix.NewUnrecognized("this server does not implement %s %s", r.Method, r.URL.Path))
}

// DeviceListAnnouncer is where an incoming device-list update is recorded, so that local users who
// share a room with the subject learn that the keys they hold moved or went away.
type DeviceListAnnouncer interface {
	Announce(ctx context.Context, userID string, left bool) error
}
