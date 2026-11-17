package fedapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	fclient "github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// RemoteDirectoryOptions is what asking a stranger about its public information needs: an identity
// to sign with, and a transport to reach it.
type RemoteDirectoryOptions struct {
	ServerName spec.ServerName
	Peer       *Peer
	// PublicDirectory reaches a peer's *client* API without a signature. It is used for exactly one
	// question, described on [RemoteDirectory.LookupAlias], and only when federation refuses to
	// answer it at all.
	PublicDirectory *http.Client
}

// RemoteDirectory asks another server two questions about its front door: which rooms it publishes,
// and what its aliases point at.
//
// Both are read-only, both are public, and neither can be answered about this server — which is why
// they live next to the transport rather than in the room server. What makes them interesting is
// how badly implementations agree about the shape of the answers, which is what the fallback chains
// below are for. A server that publishes a room nobody can find is, to the person typing its name,
// the same bug as a server that has no rooms.
type RemoteDirectory struct {
	opts RemoteDirectoryOptions
}

// NewRemoteDirectory builds the outbound directory client.
func NewRemoteDirectory(opts RemoteDirectoryOptions) *RemoteDirectory {
	if opts.PublicDirectory == nil {
		opts.PublicDirectory = &http.Client{Timeout: 15 * time.Second}
	}
	return &RemoteDirectory{opts: opts}
}

// PublicRooms fetches a page of another server's published room list.
//
// The specification spells this endpoint twice — a GET with the parameters in the query, and a
// newer POST that carries them in a body so a filter object fits — and servers are found which
// implement only one of the two. The GET is tried first because it is the older half and therefore
// the more likely to exist; a peer that says it does not recognise it is asked again by POST.
func (d *RemoteDirectory) PublicRooms(ctx context.Context, server string,
	req roomserver.PublicRoomsRequest) (*roomserver.PublicRoomsPage, error) {
	if server == "" {
		return nil, matrix.NewInvalidParam("no server to ask for a room directory")
	}
	target := spec.ServerName(server)

	page, err := d.publicRoomsByGET(ctx, target, req)
	if err == nil {
		return page, nil
	}
	if !isUnrecognized(err) {
		return nil, err
	}
	slog.Debug("fedapi: peer does not serve GET /v1/publicRooms, trying POST",
		"server", server, "error", err)
	return d.publicRoomsByPOST(ctx, target, req)
}

func (d *RemoteDirectory) publicRoomsByGET(ctx context.Context, server spec.ServerName,
	req roomserver.PublicRoomsRequest) (*roomserver.PublicRoomsPage, error) {
	query := url.Values{}
	if req.Limit > 0 {
		query.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Since != "" {
		query.Set("since", req.Since)
	}
	if req.Filter != "" {
		// A GET has no body, so the filter object travels as an encoded string in a parameter.
		encoded, err := json.Marshal(map[string]any{"generic_search_term": req.Filter})
		if err != nil {
			return nil, fmt.Errorf("federation: encoding the room-directory filter: %w", err)
		}
		query.Set("filter", string(encoded))
	}
	if req.GuestAccess != "" {
		query.Set("third_party_instance_id", req.GuestAccess)
		query.Set("include_all_networks", "false")
	}
	path := "/_matrix/federation/v1/publicRooms"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var answer federationRoomDirectory
	if err := d.opts.Peer.GetSignedJSON(ctx, server, path, &answer); err != nil {
		return nil, peerAnswerError(err)
	}
	return answer.page(), nil
}

func (d *RemoteDirectory) publicRoomsByPOST(ctx context.Context, server spec.ServerName,
	req roomserver.PublicRoomsRequest) (*roomserver.PublicRoomsPage, error) {
	body := map[string]any{}
	if req.Limit > 0 {
		body["limit"] = req.Limit
	}
	if req.Since != "" {
		body["since"] = req.Since
	}
	if req.Filter != "" {
		body["filter"] = map[string]any{"generic_search_term": req.Filter}
	}
	if req.GuestAccess != "" {
		body["third_party_instance_id"] = req.GuestAccess
		body["include_all_networks"] = false
	}
	request := fclient.NewFederationRequest(http.MethodPost, d.opts.ServerName, server,
		"/_matrix/federation/v1/publicRooms")
	identity, err := signingIdentity(d.opts.Peer.opts)
	if err != nil {
		return nil, fmt.Errorf("federation: %s", err)
	}
	if err := request.SetContent(body); err != nil {
		return nil, fmt.Errorf("federation: encoding the room-directory request: %w", err)
	}
	if err := request.Sign(identity.ServerName, identity.KeyID, identity.PrivateKey); err != nil {
		return nil, fmt.Errorf("federation: signing the room-directory request for %s: %w", server, err)
	}
	httpRequest, err := request.HTTPRequest()
	if err != nil {
		return nil, fmt.Errorf("federation: building the room-directory request for %s: %w", server, err)
	}
	httpRequest = httpRequest.WithContext(ctx)
	response, err := d.opts.Peer.transport.RoundTrip(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("federation: asking %s for its room directory: %w", server, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return nil, peerAnswerError(peerRefused(server, httpRequest.URL.Path, response))
	}
	var answer federationRoomDirectory
	if err := json.NewDecoder(io.LimitReader(response.Body, maxPeerDocumentBytes)).Decode(&answer); err != nil {
		return nil, fmt.Errorf("federation: %s's room directory is not JSON: %w", server, err)
	}
	return answer.page(), nil
}

// federationRoomDirectory is a peer's room-directory page, read loosely on purpose.
//
// `total_rooms` is a number in the client API and an estimate under a different name in the
// federation one, and some servers answer the older `rooms` where the specification now says
// `chunk`. Neither difference is worth failing a user's search over.
type federationRoomDirectory struct {
	Chunk     []roomserver.PublicRoom `json:"chunk"`
	Rooms     []roomserver.PublicRoom `json:"rooms"`
	NextBatch string                  `json:"next_batch"`
	PrevBatch string                  `json:"prev_batch"`
	Estimate  int64                   `json:"total_room_count_estimate"`
	Total     int64                   `json:"total_rooms"`
}

func (d federationRoomDirectory) page() *roomserver.PublicRoomsPage {
	page := &roomserver.PublicRoomsPage{
		Chunk:      d.Chunk,
		NextBatch:  d.NextBatch,
		PrevBatch:  d.PrevBatch,
		TotalRooms: d.Total,
	}
	if len(page.Chunk) == 0 {
		page.Chunk = d.Rooms
	}
	if page.TotalRooms == 0 {
		page.TotalRooms = d.Estimate
	}
	if page.Chunk == nil {
		page.Chunk = []roomserver.PublicRoom{}
	}
	return page
}

// LookupAlias asks the server an alias names for the room it points at.
//
// Three spellings are tried, in this order, and each is tried only because the one before it said
// it does not serve that endpoint at all (404 M_UNRECOGNIZED) rather than because it refused the
// question:
//
//  1. GET /_matrix/federation/v1/resolve/{alias} — the endpoint the current specification defines.
//  2. GET /_matrix/federation/v1/query/directory?room_alias={alias} — the spelling gomatrixserverlib
//     and the servers built on it still speak, and which newer servers answer too.
//  3. GET /_matrix/client/v3/directory/room/{alias} — the *client* API, unsigned.
//
// The third deserves a word of explanation, because a federation API should not need it. The
// room-directory endpoints are the part of federation implementers get to last, and a server can
// answer its own aliases perfectly well over the client API while never having heard of
// `/_matrix/federation/v1/resolve`. Refusing to ask means the person typing `#room:example.org`
// is told the room does not exist, which is not a thing this server knows. What the answer is used
// for is only ever *which room to ask about*: whether that room turns out to be real, and whether
// this server's user may join it, is decided by the events the resident server signs — so the
// unsigned answer can waste a join, and cannot grant one.
func (d *RemoteDirectory) LookupAlias(ctx context.Context, alias string) (string, error) {
	_, host, err := matrix.ParseAlias(alias)
	if err != nil {
		return "", matrix.NewInvalidParam("that is not a room alias")
	}
	server := spec.ServerName(host)

	var firstErr error
	for _, ask := range []func(context.Context, spec.ServerName, string) (string, error){
		d.resolveOnFederation,
		d.queryOnFederation,
		d.resolveOnClientAPI,
	} {
		roomID, err := ask(ctx, server, alias)
		if err == nil {
			if !strings.HasPrefix(roomID, "!") {
				return "", fmt.Errorf("federation: %s said %q is a room but did not name a room ID", server, alias)
			}
			return roomID, nil
		}
		if !isUnrecognized(err) {
			// The peer understood the question and answered it. "No such alias" is an answer, and
			// asking the same server the same question in a different voice will not improve it.
			if firstErr == nil {
				return "", err
			}
			return "", firstErr
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = matrix.NewNotFound("Alias %q could not be resolved", alias)
	}
	return "", peerAnswerError(firstErr)
}

func (d *RemoteDirectory) resolveOnFederation(ctx context.Context, server spec.ServerName, alias string) (string, error) {
	var answer aliasAnswer
	path := "/_matrix/federation/v1/resolve/" + url.PathEscape(alias)
	if err := d.opts.Peer.GetSignedJSON(ctx, server, path, &answer); err != nil {
		return "", err
	}
	return answer.roomID()
}

func (d *RemoteDirectory) queryOnFederation(ctx context.Context, server spec.ServerName, alias string) (string, error) {
	var answer aliasAnswer
	path := "/_matrix/federation/v1/query/directory?room_alias=" + url.QueryEscape(alias)
	if err := d.opts.Peer.GetSignedJSON(ctx, server, path, &answer); err != nil {
		return "", err
	}
	return answer.roomID()
}

func (d *RemoteDirectory) resolveOnClientAPI(ctx context.Context, server spec.ServerName, alias string) (string, error) {
	target := "https://" + string(server) + "/_matrix/client/v3/directory/room/" + url.PathEscape(alias)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("federation: asking %s about %s: %w", server, alias, err)
	}
	response, err := d.opts.PublicDirectory.Do(request)
	if err != nil {
		return "", fmt.Errorf("federation: asking %s about %s: %w", server, alias, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return "", peerRefused(server, request.URL.Path, response)
	}
	var answer aliasAnswer
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&answer); err != nil {
		return "", fmt.Errorf("federation: %s's answer about %s is not JSON: %w", server, alias, err)
	}
	return answer.roomID()
}

// aliasAnswer is an alias resolution from any of the three spellings: the older ones name `room_id`,
// the current one names a `rooms` array and every server in it puts the room first.
type aliasAnswer struct {
	RoomID string   `json:"room_id"`
	Rooms  []string `json:"rooms"`
}

func (a aliasAnswer) roomID() (string, error) {
	if a.RoomID != "" {
		return a.RoomID, nil
	}
	if len(a.Rooms) > 0 && a.Rooms[0] != "" {
		return a.Rooms[0], nil
	}
	return "", fmt.Errorf("federation: an alias resolution came back without a room")
}

// peerAnswerError turns a peer's refusal into a Matrix error a client can be told.
//
// The alternative is that a stranger's HTTP failure reaches a user as this server's own 500, which
// reads as a fault here. The peer's own error code is passed through where it has one — "not found"
// and "forbidden" mean the same thing whichever server said them — and everything else becomes a
// bad gateway that says who failed to answer.
func peerAnswerError(err error) error {
	var refusal *peerRefusedError
	if !errors.As(err, &refusal) {
		return err
	}
	code := matrix.ErrUnknown
	if refusal.Code != "" {
		code = refusal.Code
	}
	status := http.StatusBadGateway
	switch refusal.Status {
	case http.StatusNotFound, http.StatusForbidden, http.StatusTooManyRequests, http.StatusRequestTimeout:
		status = refusal.Status
	}
	message := refusal.Message
	if message == "" {
		message = "did not answer"
	}
	return matrix.NewAPIError(status, code, "%s %s: %s", refusal.Server, refusal.Path, message)
}

// isUnrecognized reports whether a peer's refusal was "I do not serve this endpoint".
func isUnrecognized(err error) bool {
	var refusal *peerRefusedError
	return errors.As(err, &refusal) && refusal.unrecognized()
}
