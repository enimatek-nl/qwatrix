package fedapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/matrix-org/gomatrixserverlib"
	fclient "github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// RemoteJoinOptions is what the outbound join needs: an identity to sign with, a transport to reach a
// peer, and keys to believe about everybody else.
type RemoteJoinOptions struct {
	ServerName spec.ServerName
	Peer       *Peer
	Events     *events.Service
	// KeyRing resolves the signing keys of the servers whose events this join will be handed. It is
	// not the same trust as the transport's: verifying the *request* signature proves who answered,
	// and verifying each *event* proves what the answer says, and only the second one survives to be
	// checked by the next server.
	KeyRing *gomatrixserverlib.KeyRing
}

// RemoteJoiner joins rooms on other servers.
//
// It is the mirror of `handleSendJoin`, and the trust situation is the opposite. Serving a join means
// deciding whether to let a stranger's user into a room this server keeps. Making one means believing a
// stranger about a room this server has never heard of, and then signing a statement about it.
type RemoteJoiner struct {
	opts RemoteJoinOptions
}

// NewRemoteJoiner builds the outbound join client.
func NewRemoteJoiner(opts RemoteJoinOptions) *RemoteJoiner {
	return &RemoteJoiner{opts: opts}
}

// JoinRemoteRoom asks a peer to let this server's user into one of its rooms.
//
// The servers are tried in order and the first convincing answer wins: which server holds a room is
// guesswork from this side, and a room directory that names a dead one should not cost the user the
// join. "Convincing" is doing a lot of work in that sentence — see verifyJoinAnswer for what an answer
// has to survive before it is returned.
//
// When none of them answer, the failure names the room. "No server could be reached for room X" is a
// fact a client can act on by naming another server; a bare internal error is not.
func (c *RemoteJoiner) JoinRemoteRoom(ctx context.Context, userID, roomID, reason string,
	servers []string) (*roomserver.RemoteJoin, error) {
	candidates := c.joinServers(roomID, servers)
	if len(candidates) == 0 {
		return nil, matrix.NewNotFound(
			"Room %q does not exist on this homeserver and no server could be asked about it", roomID)
	}

	var lastErr error
	for _, server := range candidates {
		remote, err := c.joinVia(ctx, userID, roomID, reason, server)
		if err == nil {
			return remote, nil
		}
		lastErr = err
		slog.Debug("fedapi: could not join a room through this server",
			"room_id", roomID, "server", server, "error", err)
	}
	// A room that refused is not a room that does not exist. Mapping every failure to "not found" made a
	// room that said "you are not invited" indistinguishable from a dead server, and a client that
	// believes a room is gone stops offering the user the one thing that would work — asking for an
	// invite.
	if refusal := peerRefusal(lastErr); refusal != nil {
		return nil, refusal
	}
	if apiErr, ok := lastErr.(*matrix.APIError); ok {
		return nil, apiErr
	}
	return nil, matrix.NewNotFound("Room %q could not be joined over federation: %s", roomID,
		errorText(lastErr))
}

// joinVia runs the handshake against one server.
func (c *RemoteJoiner) joinVia(ctx context.Context, userID, roomID, reason string,
	server spec.ServerName) (*roomserver.RemoteJoin, error) {
	if c.opts.Peer == nil {
		return nil, matrix.NewUnknown("this homeserver is not configured to federate")
	}

	// 1. Ask where a join would go. The peer chooses the predecessors, the auth events and the
	// depth, because it is the one holding the graph.
	template, err := c.opts.Peer.Signed().MakeJoin(ctx, c.opts.ServerName, server, roomID, userID)
	if err != nil {
		return nil, fmt.Errorf("%s could not offer a join: %w", server, err)
	}
	version, err := c.opts.Events.RoomVersion(string(template.RoomVersion))
	if err != nil {
		return nil, unsupportedRoomVersion(fmt.Sprintf(
			"the peer says room %q follows version %s, which this server does not support", roomID,
			template.RoomVersion))
	}
	proto := template.GetJoinEvent()

	// 2. Read the template before signing it.
	//
	// This is the moment where a peer can do the most damage with the least effort: whatever comes
	// back here is about to carry this server's signature, and after that it is indistinguishable
	// from a join this server chose. So every field that decides *whose* join it is gets checked
	// against what this server asked for, rather than taken on faith.
	if err := checkJoinTemplate(proto, userID, roomID); err != nil {
		return nil, err
	}
	prevEvents, ok := stringList(proto.PrevEvents)
	if !ok {
		return nil, c.unbuildableVersion(template.RoomVersion, "prev_events")
	}
	authEvents, ok := stringList(proto.AuthEvents)
	if !ok {
		return nil, c.unbuildableVersion(template.RoomVersion, "auth_events")
	}

	// 3. Build and sign the join here. The reason a client gave travels in the event, which is
	// another reason the signing cannot be delegated: it is this server's user asking.
	content := map[string]any{"membership": string(matrix.MembershipJoin)}
	if reason != "" {
		content["reason"] = reason
	}
	joinEvent, err := c.opts.Events.Build(events.Draft{
		RoomVersion:  template.RoomVersion,
		RoomID:       roomID,
		Type:         string(matrix.EventTypeMember),
		StateKey:     &userID,
		Sender:       userID,
		Content:      content,
		PrevEvents:   prevEvents,
		AuthEventIDs: authEvents,
		Depth:        proto.Depth,
	})
	if err != nil {
		return nil, fmt.Errorf("building the join event: %w", err)
	}

	// 4. Offer it. The peer either accepts the join into its room or refuses; either way it answers
	// with the room as it now stands.
	response, err := c.opts.Peer.Signed().SendJoin(ctx, c.opts.ServerName, server, joinEvent)
	if err != nil {
		return nil, fmt.Errorf("%s refused the join: %w", server, err)
	}

	// 5. Verify the answer before any of it is believed.
	state, authChain, err := c.verifyJoinAnswer(ctx, roomID, version, joinEvent, response)
	if err != nil {
		return nil, err
	}
	if response.MembersOmitted {
		// The peer says its member list is incomplete and expects this server to ask somebody
		// else. Saying nothing would leave the local user with a member list quietly missing
		// people, which is the failure mode worth naming: the room looks smaller than it is.
		slog.Warn("fedapi: a peer returned a federated join with members omitted; the member list "+
			"of this room may be incomplete until another server is asked",
			"room_id", roomID, "server", server)
	}

	return &roomserver.RemoteJoin{
		Origin:        string(response.Origin),
		RoomVersion:   string(template.RoomVersion),
		JoinEvent:     joinEvent,
		State:         state,
		AuthChain:     authChain,
		ServersInRoom: response.ServersInRoom,
	}, nil
}

// checkJoinTemplate refuses a join template that does not describe the join this server asked for.
//
// The fields checked are exactly the ones that decide who is joining what: the type, the room, the
// state key, the sender the peer pre-filled (if it did), and the membership. A template for
// `@someone:else`, or for another room, or with membership `leave`, is a request for this server to
// sign a forgery — and a signed forgery from this server is a statement other servers will believe.
func checkJoinTemplate(proto gomatrixserverlib.ProtoEvent, userID, roomID string) error {
	if proto.Type != string(matrix.EventTypeMember) {
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer offered a join template of type %q instead of a membership event", proto.Type))
	}
	if proto.StateKey == nil || *proto.StateKey != userID {
		offered := ""
		if proto.StateKey != nil {
			offered = *proto.StateKey
		}
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer offered a join for %q when this server asked for %q", offered, userID))
	}
	if declared := proto.RoomID; declared != "" && declared != roomID {
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer offered a join for room %q when this server asked for %q", declared, roomID))
	}
	if sender := string(proto.SenderID); sender != "" && sender != userID {
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer pre-filled the join sender as %q instead of %q", sender, userID))
	}
	var content struct {
		Membership string `json:"membership"`
	}
	// The template's content is raw JSON; an unreadable one is no better than a wrong one, because
	// this server is about to replace it with content of its own and sign the result.
	if err := json.Unmarshal(proto.Content, &content); err != nil {
		return matrix.NewUnknown("a peer offered a join template whose content cannot be read")
	}
	if content.Membership != "" && content.Membership != string(matrix.MembershipJoin) {
		return matrix.NewUnknown(fmt.Sprintf(
			"a peer offered a join template with membership %q", content.Membership))
	}
	return nil
}

// verifyJoinAnswer reads and checks the state a peer returned, and returns it as events.
//
// Every event must carry a signature that holds up against its sender's published key. This is not
// belt-and-braces duplication of the request's own signature: the request signature says which server
// sent this answer and expires with the moment it was made, while an event's signature is what every
// later reader — including a third server this room is later federated to — will check. Storing
// mis-signed events would make this server the one that spreads them, and this server is the one the
// next reader has to believe.
//
// Each event must also belong to this room. A peer that supplies state from another room is
// describing rules that were never made for this one.
//
// Finally the join's own auth events must be present: without them the join cannot be re-checked, and
// a room whose entry ticket cannot be verified is a room this server is agreeing to remember without
// agreeing to.
func (c *RemoteJoiner) verifyJoinAnswer(ctx context.Context, roomID string,
	version gomatrixserverlib.IRoomVersion, joinEvent gomatrixserverlib.PDU,
	response fclient.RespSendJoin) ([]gomatrixserverlib.PDU, []gomatrixserverlib.PDU, error) {
	if c.opts.KeyRing == nil {
		return nil, nil, matrix.NewUnknown("this homeserver cannot verify federation signatures")
	}
	userIDForSender := c.opts.Events.UserIDForSender()

	state, err := c.readEvents(ctx, roomID, version, response.StateEvents, userIDForSender, "state")
	if err != nil {
		return nil, nil, err
	}
	authChain, err := c.readEvents(ctx, roomID, version, response.AuthEvents, userIDForSender, "auth chain")
	if err != nil {
		return nil, nil, err
	}

	present := make(map[string]bool, len(state))
	seenStateKey := make(map[string]bool, len(state))
	for _, event := range state {
		// A room's state holds one event per (type, state_key), so two of them is not a room with
		// extra detail — it is the peer sending two answers to the same question, and accepting the
		// list would make the winner whichever entry happens to be read last. A ban can be hidden
		// behind a join, or a stranger's power-levels event behind the room's own, and every later
		// decision this server makes about the room would be taken against the surviving copy.
		//
		// The join this answer is about is the one exception, and it is not an exception to bend: a
		// send_join answer carries the state as it stood *before* the join plus the join event itself,
		// so a user who was invited, or who had left, legitimately appears twice. Everything else —
		// two power-levels events, two join rules, two membership events for somebody other than the
		// joiner — has no reading that is not a contradiction, and is refused.
		if stateKey := event.StateKey(); stateKey != nil && event.EventID() != joinEvent.EventID() {
			key := event.Type() + "\x00" + *stateKey
			if seenStateKey[key] {
				return nil, nil, matrix.NewUnknown(fmt.Sprintf(
					"the peer's state holds two %s events for %q, which is not a room",
					event.Type(), *stateKey))
			}
			seenStateKey[key] = true
		}
		present[event.EventID()] = true
	}
	for _, id := range joinEvent.AuthEventIDs() {
		if !present[id] {
			return nil, nil, matrix.NewUnknown(fmt.Sprintf(
				"the peer's answer is missing %s, which authorises the join", id))
		}
	}
	return state, authChain, nil
}

// readEvents parses and verifies one half of a send_join answer.
func (c *RemoteJoiner) readEvents(ctx context.Context, roomID string, version gomatrixserverlib.IRoomVersion,
	raws gomatrixserverlib.EventJSONs, userIDForSender spec.UserIDForSender,
	which string) ([]gomatrixserverlib.PDU, error) {
	events := make([]gomatrixserverlib.PDU, 0, len(raws))
	for _, raw := range raws {
		event, err := version.NewEventFromUntrustedJSON(raw)
		if err != nil {
			return nil, matrix.NewUnknown(fmt.Sprintf(
				"the peer's %s contains an event that cannot be read: %s", which, err))
		}
		if declared := event.RoomID().String(); declared != roomID {
			return nil, matrix.NewUnknown(fmt.Sprintf(
				"the peer's %s contains an event from room %q, not %q", which, declared, roomID))
		}
		if err := gomatrixserverlib.VerifyEventSignatures(ctx, event, c.opts.KeyRing, userIDForSender); err != nil {
			return nil, matrix.NewUnknown(fmt.Sprintf(
				"the peer's %s contains an event whose signature does not verify (%s): %s",
				which, event.EventID(), err))
		}
		events = append(events, event)
	}
	return events, nil
}

// joinServers is the list of servers worth asking, in the order they should be asked.
//
// The specification's `server_name` (and the `via` list a knock or invite carries) comes first because
// a client or a peer usually names a server that actually holds the room. When nothing is named, the
// room ID's own home server is the only remaining candidate: room IDs are minted by the server that
// created the room, which makes them a weak but honest pointer.
func (c *RemoteJoiner) joinServers(roomID string, servers []string) []spec.ServerName {
	return candidateServers(c.opts.ServerName, roomID, servers)
}

// candidateServers is the same rule as a free function, for the handshakes that are not joins: which
// server holds a room is guesswork, and every handshake has to guess it the same way.
func candidateServers(self spec.ServerName, roomID string, servers []string) []spec.ServerName {
	seen := map[string]bool{}
	candidates := make([]spec.ServerName, 0, len(servers)+1)
	appendCandidate := func(server string) {
		if server == "" || seen[server] || server == string(self) {
			// This server is never worth asking: if it held the room, the handshake would not be
			// federated, and asking it would be a loop with extra steps.
			return
		}
		seen[server] = true
		candidates = append(candidates, spec.ServerName(server))
	}
	for _, server := range servers {
		appendCandidate(server)
	}
	if len(candidates) == 0 {
		if _, domain, err := matrix.ParseRoomID(roomID); err == nil {
			appendCandidate(domain)
		}
	}
	return candidates
}

// unbuildableVersion refuses a room version whose event shape this server cannot construct a join for.
//
// Room versions 1 and 2 name auth and predecessor events by `{type, key}` references rather than by
// event ID, and this server builds events from IDs. Guessing at the conversion would produce an event
// that is subtly not the one the peer asked for — with a signature on it — so the refusal says which
// version is unsupported instead.
func (c *RemoteJoiner) unbuildableVersion(version gomatrixserverlib.RoomVersion, field string) error {
	return unsupportedRoomVersion(fmt.Sprintf(
		"room version %s encodes %s as event references, which this server cannot yet build a join for",
		version, field))
}

// unsupportedRoomVersion is the answer that tells a client which version it cannot speak, rather than
// a generic failure that leaves it guessing whether the room exists at all.
func unsupportedRoomVersion(message string) error {
	return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrIncompatibleVersion, "%s", message)
}

// stringList reads a JSON array of event IDs, which is the shape room version 3 and later use.
func stringList(value any) ([]string, bool) {
	if value == nil {
		return nil, true
	}
	list, ok := value.([]any)
	if !ok {
		if asStrings, isStrings := value.([]string); isStrings {
			return asStrings, true
		}
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			// An object in this position is a `{type, key}` reference: an older room version.
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

// errorText keeps a failure printable when it is nil-safe by construction.
func errorText(err error) string {
	if err == nil {
		return "no server was reachable"
	}
	return err.Error()
}
