package fedapi

// Knocking on a room held by somebody else.
//
// This is the mirror of `handleSendKnock`, and the trust runs the other way: here this server asks a
// stranger for a place in a room it has never seen, signs what comes back, and then believes the
// stranger's account of what the room looks like.
//
// Two things limit the damage. The event this server signs is checked field by field against what it
// asked for before a signature is put on it — a template that describes another user, another room, or a
// different membership is a request to forge a membership with this server's key. And of everything the
// peer says about the room, this server keeps only the front-door set: the events a knocking user is
// entitled to be shown anyway. A peer that hands over a member list, a power structure, or a message will
// find it dropped, because none of those are things a room tells somebody it has not let in.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// RemoteKnockOptions is what the outbound knock needs: an identity to sign with, a transport, and the
// event builder for the room's own version.
type RemoteKnockOptions struct {
	ServerName spec.ServerName
	Peer       *Peer
	Events     *events.Service
}

// RemoteKnocker knocks on rooms held by other servers.
type RemoteKnocker struct {
	opts RemoteKnockOptions
}

// NewRemoteKnocker builds the outbound knock client.
func NewRemoteKnocker(opts RemoteKnockOptions) *RemoteKnocker {
	return &RemoteKnocker{opts: opts}
}

// KnockRemoteRoom asks a server that holds the room to take this server's user's knock.
//
// The servers are tried in order and the first that completes the handshake wins: which server holds a
// room is guesswork from this side, and a `?via=` list naming a dead server should not cost the user the
// knock.
func (c *RemoteKnocker) KnockRemoteRoom(ctx context.Context, userID, roomID, reason string,
	servers []string) (*roomserver.RemoteKnock, error) {
	candidates := candidateServers(c.opts.ServerName, roomID, servers)
	if len(candidates) == 0 {
		return nil, matrix.NewNotFound(
			"Room %q does not exist on this homeserver and no server could be asked about it", roomID)
	}

	var lastErr error
	for _, server := range candidates {
		knock, err := c.knockVia(ctx, userID, roomID, reason, server)
		if err == nil {
			return knock, nil
		}
		lastErr = err
		slog.Debug("fedapi: could not knock on a room through this server",
			"room_id", roomID, "server", server, "error", err)
	}
	// A peer that refused is not a peer that was unreachable, and the difference is the whole of what
	// the user can do next.
	if refusal := peerRefusal(lastErr); refusal != nil {
		return nil, refusal
	}
	if apiErr, ok := lastErr.(*matrix.APIError); ok {
		return nil, apiErr
	}
	return nil, matrix.NewNotFound("Room %q could not be knocked on over federation: %s", roomID,
		errorText(lastErr))
}

// knockVia runs the handshake against one server.
func (c *RemoteKnocker) knockVia(ctx context.Context, userID, roomID, reason string,
	server spec.ServerName) (*roomserver.RemoteKnock, error) {
	if c.opts.Peer == nil {
		return nil, matrix.NewUnknown("this homeserver is not configured to federate")
	}

	// 1. Ask where a knock would go. The peer chooses the predecessors, the auth events and the depth,
	// because it is the one holding the graph — and this server holds nothing of the room at all.
	template, err := c.opts.Peer.Signed().MakeKnock(ctx, c.opts.ServerName, server, roomID, userID, nil)
	if err != nil {
		return nil, fmt.Errorf("%s could not offer a knock: %w", server, err)
	}
	if _, err := c.opts.Events.RoomVersion(string(template.RoomVersion)); err != nil {
		return nil, unsupportedRoomVersion(fmt.Sprintf(
			"the peer says room %q follows version %s, which this server does not support", roomID,
			template.RoomVersion))
	}

	// 2. Read the draft before signing it, for the reason the join handshake reads its template: what
	// comes back here is about to carry this server's signature, after which it is indistinguishable
	// from a knock this server chose to make.
	if err := checkKnockTemplate(template.KnockEvent, userID, roomID); err != nil {
		return nil, err
	}
	prevEvents, ok := stringList(template.KnockEvent.PrevEvents)
	if !ok {
		return nil, fmt.Errorf("the peer's knock draft names predecessors this server cannot read")
	}
	authEvents, ok := stringList(template.KnockEvent.AuthEvents)
	if !ok {
		return nil, fmt.Errorf("the peer's knock draft names authorising events this server cannot read")
	}

	// 3. Build and sign the knock here. It is this server's user asking, and their reason travels in the
	// event, which is why the signing cannot be delegated to the peer.
	content := map[string]any{"membership": string(matrix.MembershipKnock)}
	if reason != "" {
		content["reason"] = reason
	}
	knockEvent, err := c.opts.Events.Build(events.Draft{
		RoomVersion:  template.RoomVersion,
		RoomID:       roomID,
		Type:         string(matrix.EventTypeMember),
		StateKey:     &userID,
		Sender:       userID,
		Content:      content,
		PrevEvents:   prevEvents,
		AuthEventIDs: authEvents,
		Depth:        template.KnockEvent.Depth,
	})
	if err != nil {
		return nil, fmt.Errorf("building the knock event: %w", err)
	}

	// 4. Offer it. Either the room takes the knock and tells this server what its front door looks
	// like, or it refuses and the user is told.
	response, err := c.opts.Peer.Signed().SendKnock(ctx, c.opts.ServerName, server, knockEvent)
	if err != nil {
		return nil, fmt.Errorf("%s refused the knock: %w", server, err)
	}

	state, err := strippedStateAsRawMessages(response.KnockRoomState)
	if err != nil {
		return nil, fmt.Errorf("reading the room state a peer offered with its knock answer: %w", err)
	}
	return &roomserver.RemoteKnock{
		Origin:      string(server),
		RoomVersion: string(template.RoomVersion),
		KnockEvent:  knockEvent,
		RoomState:   state,
	}, nil
}

// checkKnockTemplate refuses a knock draft that does not describe the knock this server asked for.
//
// The fields are the ones that decide whose knock it is: the type, the room, the state key, a sender the
// peer pre-filled if it dared, and the membership. A draft for `@someone:else`, for another room, or with
// membership `leave`, is an invitation to sign a forgery — and a signed forgery is a statement every other
// server will believe this server made.
func checkKnockTemplate(proto gomatrixserverlib.ProtoEvent, userID, roomID string) error {
	if proto.Type != string(matrix.EventTypeMember) {
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer offered a knock template of type %q instead of a membership event", proto.Type))
	}
	if proto.StateKey == nil || *proto.StateKey != userID {
		offered := ""
		if proto.StateKey != nil {
			offered = *proto.StateKey
		}
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer offered a knock for %q when this server asked for %q", offered, userID))
	}
	if declared := proto.RoomID; declared != "" && declared != roomID {
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer offered a knock for room %q when this server asked for %q", declared, roomID))
	}
	if sender := string(proto.SenderID); sender != "" && sender != userID {
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer pre-filled the knock sender as %q instead of %q", sender, userID))
	}
	var content struct {
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(proto.Content, &content); err != nil {
		return matrix.NewUnknown("a peer offered a knock template whose content cannot be read")
	}
	if content.Membership != "" && content.Membership != string(matrix.MembershipKnock) {
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer offered a knock template with membership %q", content.Membership))
	}
	return nil
}

// strippedStateAsRawMessages re-reads the stripped state a peer answered with as bytes.
//
// The transport types carry it as decoded state events with no signatures — that is what "stripped" means
// — and this server stores and serves them as bytes, so the round trip through JSON is the conversion. A
// document that will not re-read is refused rather than half kept: a knock screen built from a
// partially-parsed description of a foreign room is a screen nobody can trust later.
func strippedStateAsRawMessages(state []gomatrixserverlib.InviteStrippedState) ([]json.RawMessage, error) {
	if len(state) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(encoded, &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// peerRefusal turns a peer's refusal into this server's own refusal, or returns nil when the error is not
// a refusal at all.
//
// It matters because of what the alternative says. Without this, a room that answers "you may not knock
// here" and a server that is simply unreachable both arrive at the client as "no such room" — and a client
// that hears a room does not exist stops looking, while a user who was refused wants to be told they were
// refused. The room's own reason comes along for the same reason: "this room is invite-only" is actionable
// and "the federation handshake failed" is not.
//
// The federation transport reports a non-2xx answer as a formatted error carrying the peer's body, so that
// is what is parsed here, defensively: if the shape is not recognised this server says nothing on the
// peer's behalf rather than inventing a status code for it.
func peerRefusal(err error) error {
	if err == nil {
		return nil
	}
	var pointer *spec.MatrixError
	if errors.As(err, &pointer) {
		return apiErrorForPeerRefusal(pointer)
	}
	var value spec.MatrixError
	if errors.As(err, &value) {
		return apiErrorForPeerRefusal(&value)
	}
	// The transport does not hand back a peer's error document; it wraps it in prose, and the shape of
	// that prose has changed between versions of the library — `HTTP 403 : {"errcode":...}` in one,
	// `contents=[123 34 ...]` (the body printed as a byte list) in another. Both are read here, and the
	// *code* is what is trusted: the status is then this server's own, derived from the code the peer
	// named, which is the part the peer chose deliberately rather than the part its HTTP layer emitted.
	if match := peerRefusalPattern.FindAllStringSubmatch(err.Error(), -1); len(match) > 0 {
		last := match[len(match)-1]
		status, conversion := strconv.Atoi(last[1])
		if conversion == nil && status >= 400 && status <= 599 {
			if apiErr := refusalForBody(last[2], status, err); apiErr != nil {
				return apiErr
			}
		}
	}
	if match := peerContentsPattern.FindAllStringSubmatch(err.Error(), -1); len(match) > 0 {
		if apiErr := refusalForBody(decodeByteList(match[len(match)-1][1]), 0, err); apiErr != nil {
			return apiErr
		}
	}
	return nil
}

// refusalForBody reads a peer's error document and restates it, or returns nil when the document says
// nothing this server can act on. `status` of 0 means the transport did not report one usefully, and the
// code's own status is used.
func refusalForBody(body string, status int, transport error) error {
	var document struct {
		ErrCode string `json:"errcode"`
		Message string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &document) != nil || document.ErrCode == "" {
		return nil
	}
	code := matrix.ErrCode(document.ErrCode)
	if status == 0 {
		status = statusForPeerErrCode(code)
	}
	return matrix.NewAPIError(status, code, "%s", refusalText(document.Message, transport.Error()))
}

// decodeByteList turns `[123 34 101 ...]` — a byte slice printed by Go's default formatter — back into
// text. It is here because the alternative is losing a peer's reason for refusing, which is the one thing
// a user can act on.
func decodeByteList(list string) string {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(list), "[]"))
	bytes := make([]byte, 0, len(fields))
	for _, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 || value > 255 {
			return ""
		}
		bytes = append(bytes, byte(value))
	}
	return string(bytes)
} // apiErrorForPeerRefusal restates a typed peer error, mapping its code to the status the specification
// gives that code rather than trusting whatever the peer's HTTP layer happened to use.
func apiErrorForPeerRefusal(peerErr *spec.MatrixError) error {
	code := matrix.ErrCode(string(peerErr.ErrCode))
	return matrix.NewAPIError(statusForPeerErrCode(code), code, "%s", peerErr.Err)
}

// refusalText prefers the peer's own sentence, which names the rule, and falls back on the transport's
// message so that a refusal is never reported as an empty string.
func refusalText(peerMessage, transport string) string {
	if strings.TrimSpace(peerMessage) != "" {
		return peerMessage
	}
	return transport
}

// statusForPeerErrCode restates a peer's error code with the status the specification gives it.
//
// A peer's HTTP status is not always the one its own code implies — implementations disagree, and a proxy
// in front of one can change it — while the code is what the peer meant. `M_UNKNOWN` is reported as a bad
// gateway rather than a client error: when a peer says it failed, this server has failed to consult it, and
// telling the user their request was malformed would send them looking for a mistake they did not make.
func statusForPeerErrCode(code matrix.ErrCode) int {
	switch code {
	case matrix.ErrForbidden:
		return http.StatusForbidden
	case matrix.ErrNotFound, matrix.ErrUnrecognized:
		return http.StatusNotFound
	case matrix.ErrInvalidParam, matrix.ErrBadJSON, matrix.ErrNotJSON,
		matrix.ErrIncompatibleVersion, matrix.ErrMissingParam:
		return http.StatusBadRequest
	case matrix.ErrUnknownToken, matrix.ErrMissingToken:
		return http.StatusUnauthorized
	case matrix.ErrLimitExceeded:
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

// peerRefusalPattern finds a peer's own error document inside a transport error: the status the peer
// answered with, and the JSON body beneath it.
var peerRefusalPattern = regexp.MustCompile(`HTTP ([0-9]{3}) : (\{.*?\})`)

// peerContentsPattern is the same document as a printed byte list, which is how a newer transport reports
// the body it refused with.
var peerContentsPattern = regexp.MustCompile(`contents=\[([0-9 ]+)\]`)
