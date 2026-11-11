package roomserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/matrix-org/gomatrixserverlib"
)

// RemoteJoin is the answer a peer gave to a federated join, described in the room server's own terms.
//
// It deliberately does not carry the federation transport's types: the room server is the component
// that decides what a room is, and making it depend on the transport's wire shapes would mean a change
// in how a peer words its answer becomes a change in how this server stores history.
type RemoteJoin struct {
	// Origin is the server whose answer this is.
	Origin string
	// RoomVersion is the version the peer says the room follows. It decides the event ID format,
	// the authorisation rules and the redaction rules for everything below, so it is not
	// interchangeable with this server's default.
	RoomVersion string
	// JoinEvent is this server's own join event — built here, signed here, accepted there.
	JoinEvent gomatrixserverlib.PDU
	// State is the room as the peer describes it, including JoinEvent.
	State []gomatrixserverlib.PDU
	// AuthChain is the set of events that authorises that state.
	AuthChain []gomatrixserverlib.PDU
	// ServersInRoom names the other servers the peer believes are in the room.
	ServersInRoom []string
}

// FederationJoiner is what the room server needs in order to join a room it does not hold.
//
// The seam is declared here, at the consumer, so that the room server does not import the federation
// transport: the transport needs the room server already, and a cycle is only avoided by someone
// giving way. It is also the honest shape — the room server's question is "get me a room to join", not
// "issue these five HTTP requests in this order".
type FederationJoiner interface {
	// JoinRemoteRoom asks one of the named servers (or the room's own home server when none are
	// named) for a join of the room, and returns the room as that peer describes it.
	JoinRemoteRoom(ctx context.Context, userID, roomID, reason string, servers []string) (*RemoteJoin, error)
}

// WithFederationJoiner connects the room server to the federation transport. Without it a room this
// server does not hold simply does not exist, which is the correct answer for a homeserver that does
// not federate.
func (s *Service) WithFederationJoiner(joiner FederationJoiner) *Service {
	s.federationJoiner = joiner
	return s
}

// JoinRoomVia joins a room, using the named servers when the room is one this server does not hold.
//
// The two cases look the same to a client — POST /join/{room} either way — and are not the same
// operation at all. A local join writes an event into a room this server already keeps; a federated
// join asks a stranger what the room is, signs the stranger's answer, and then decides whether to
// believe it. This is where the two paths part.
func (s *Service) JoinRoomVia(ctx context.Context, sender Sender, roomID, reason string,
	extra map[string]json.RawMessage, servers []string) (string, error) {
	if _, err := s.store.Rooms.Get(ctx, roomID); err == nil {
		return s.JoinRoom(ctx, sender, roomID, reason, extra)
	} else if !store.IsNotFound(err) {
		return "", err
	}

	// A room ID whose home server is this one and which this server does not have is an absent room,
	// not a remote one. Asking a peer to join it would be asking somebody else to vouch for an
	// identity this server owns, and whatever they answered would be a lie by construction.
	if _, domain, err := matrix.ParseRoomID(roomID); err == nil && domain == s.cfg.ServerName {
		return "", matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
	}
	if s.federationJoiner == nil {
		return "", matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
	}
	return s.joinRemoteRoom(ctx, sender, roomID, reason, servers)
}

// joinRemoteRoom performs the federated half of a join and stores the result.
//
// The room server does not take the peer's answer on trust because the transport delivered it. Two
// checks are repeated here even though the transport also makes them:
//
// The room's own rules, as the peer itself described them, must allow this join. A peer that says
// "welcome" while handing back a state in which this user is banned is describing a room this user is
// not in, and storing that state would put this server's users in a room whose rules say otherwise —
// while this server goes on enforcing those same rules against its own members.
//
// And the state must contain the join it is answering for. An answer that leaves the join out is not a
// description of the room after the join; it is a way of making a member appear absent in the very
// state that proves them present.
//
// Nothing is stored unless both hold, and the whole room is stored in one transaction. A room
// persisted half — state without memberships, memberships without the create event — would be read by
// every later decision as a room that really exists.
func (s *Service) joinRemoteRoom(ctx context.Context, sender Sender, roomID, reason string,
	servers []string) (string, error) {
	remote, err := s.federationJoiner.JoinRemoteRoom(ctx, sender.UserID, roomID, reason, servers)
	if err != nil {
		return "", err
	}
	if remote == nil || remote.JoinEvent == nil {
		return "", matrix.NewUnknown("the answer to the federated join carried no join event")
	}
	if joined := remote.JoinEvent.RoomID().String(); joined != roomID {
		return "", matrix.NewUnknown(fmt.Sprintf(
			"the peer answered a join for room %q when this server asked for %q", joined, roomID))
	}
	// The peer's answer is checked against the room's own rules before a byte of it is stored: the
	// auth chain has to be complete, and the state has to actually allow this join.
	// The room's rules are applied to the state as it was *before* this join, not to the state as the
	// peer handed it over. The difference is not cosmetic: a send_join answer includes the join event
	// itself, and a membership event found in the authorising state is read as the user's *previous*
	// membership. Authorising against the answer as given therefore tells the room "this user is
	// already a member, so let them in" — a rule that admits anybody, in any room, on the strength of
	// the very event being questioned.
	prior := stateBefore(remote.State, remote.JoinEvent.EventID())
	if err := s.requireCompleteAuthChain(remote.JoinEvent, prior); err != nil {
		return "", err
	}
	if err := s.events.Authorize(remote.JoinEvent, prior); err != nil {
		return "", err
	}
	if err := s.persistFederatedJoin(ctx, roomID, remote); err != nil {
		return "", err
	}
	s.notifier.Publish()
	return remote.JoinEvent.EventID(), nil
}

// stateBefore returns the peer's state without the join event being made, which is the state the join
// has to be authorised against.
//
// The event is not necessarily in that list. Synapse answers send_join with the room's state
// *including* the join it accepted; complement's reference federation server deliberately builds the
// list before inserting it, so the join it has just accepted is absent. Both are peers saying yes, and
// whether the event rides inside the array says nothing this server does not already hold: the join was
// built from the peer's own make_join template and verified against the peer's signing key. Treating
// its absence as a refusal was therefore wrong, and refusing a peer that returned 200 is a way of being
// unable to join rooms that are perfectly willing to have you.
func stateBefore(state []gomatrixserverlib.PDU, eventID string) []gomatrixserverlib.PDU {
	prior := make([]gomatrixserverlib.PDU, 0, len(state))
	for _, event := range state {
		if event.EventID() != eventID {
			prior = append(prior, event)
		}
	}
	return prior
}

// requireCompleteAuthChain refuses a join whose auth events leave out what the room's rules require.
//
// Authorisation reads the auth events an event *names*, not the room's whole state — which is correct,
// and also the way a peer can arrange for a rule to go unread. A join that names only the create and
// power-levels events is checked against no join rule at all, and the absence of a join rule is not a
// refusal: it reads as "public", so a user is waved into an invite-only room by an event that never
// mentioned the door.
//
// So the auth events the room's state *would* require for this event are computed here and each one is
// looked for in the ones the event carries. A missing one is a peer either badly built or carefully
// built, and this server cannot tell which, so it refuses both the same way.
func (s *Service) requireCompleteAuthChain(join gomatrixserverlib.PDU, state []gomatrixserverlib.PDU) error {
	provider, err := s.events.AuthEvents(state)
	if err != nil {
		return err
	}
	needed, err := gomatrixserverlib.StateNeededForProtoEvent(&gomatrixserverlib.ProtoEvent{
		Type:     join.Type(),
		SenderID: string(join.SenderID()),
		StateKey: join.StateKey(),
		Content:  join.Content(),
	})
	if err != nil {
		return fmt.Errorf("working out what authorises the federated join: %w", err)
	}
	required, err := needed.AuthEventReferences(provider)
	if err != nil {
		return fmt.Errorf("collecting the auth events a federated join needs: %w", err)
	}
	declared := make(map[string]bool, len(join.AuthEventIDs()))
	for _, id := range join.AuthEventIDs() {
		declared[id] = true
	}
	named := make(map[string]string, len(state))
	for _, event := range state {
		if stateKey := event.StateKey(); stateKey != nil {
			named[event.EventID()] = event.Type() + "/" + *stateKey
		} else {
			named[event.EventID()] = event.Type()
		}
	}
	for _, id := range required {
		if !declared[id] {
			return matrix.NewUnknown(fmt.Sprintf(
				"the peer's join leaves out %s (%s), which this room's rules require to authorise it",
				named[id], id))
		}
	}
	return nil
}

// persistFederatedJoin writes a room this server is now part of, from the state a peer described.
//
// Ordering is the whole care taken here. Auth events are stored before the events that need them,
// because an event whose auth chain is absent cannot be re-checked later — and re-checking is what
// every subsequent join, redaction and permission question in this room will do. State is stored in
// depth order so each event's predecessors are already present, and this server's own join goes last:
// the room's forward extremity is what the next local event is built on, and a join stored before the
// state it authorises would leave this server appending history to the wrong end of the graph.
func (s *Service) persistFederatedJoin(ctx context.Context, roomID string, remote *RemoteJoin) error {
	return s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		room := &store.Room{
			ID:        roomID,
			Version:   remote.RoomVersion,
			CreatedMS: createdMS(remote.State),
			// This server joined. `Left` is the flag that takes a room out of the directory and
			// out of /hierarchy, and a room just joined is the opposite.
			Left:     false,
			JoinedMS: time.Now().UnixMilli(),
		}
		if err := s.store.Rooms.Create(ctx, room); err != nil {
			return err
		}

		writer := &eventWriter{service: s, tx: tx, ctx: ctx}
		stored := map[string]bool{remote.JoinEvent.EventID(): true}

		for _, event := range remote.AuthChain {
			if stored[event.EventID()] {
				continue
			}
			stored[event.EventID()] = true
			if err := writer.appendPrebuilt(remoteAppendRequest(roomID, event), event); err != nil {
				return err
			}
		}
		for _, event := range sortByDepth(remote.State, remote.JoinEvent.EventID()) {
			if stored[event.EventID()] {
				continue
			}
			stored[event.EventID()] = true
			if err := writer.appendPrebuilt(remoteAppendRequest(roomID, event), event); err != nil {
				return err
			}
		}
		// Last, so it is the newest event in the room and the one history is read forward from.
		return writer.appendPrebuilt(remoteAppendRequest(roomID, remote.JoinEvent), remote.JoinEvent)
	})
}

// remoteAppendRequest describes a peer's event for the append path that stores events this server did
// not build. Membership is read out of the content because the append path keeps it denormalised — it
// is what the member counters and the membership rows are written from.
func remoteAppendRequest(roomID string, event gomatrixserverlib.PDU) txAppend {
	stateKey := ""
	if key := event.StateKey(); key != nil {
		stateKey = *key
	}
	req := txAppend{
		roomID:    roomID,
		eventType: event.Type(),
		stateKey:  stateKey,
		isState:   event.StateKey() != nil,
		sender:    string(event.SenderID()),
		content:   json.RawMessage(event.Content()),
	}
	if event.Type() == string(matrix.EventTypeMember) {
		var content struct {
			Membership string `json:"membership"`
		}
		if err := json.Unmarshal(event.Content(), &content); err == nil {
			req.membership = content.Membership
		}
	}
	return req
}

// sortByDepth returns the state in dependency order, skipping the event named skip (which the caller
// stores last).
func sortByDepth(state []gomatrixserverlib.PDU, skip string) []gomatrixserverlib.PDU {
	sorted := make([]gomatrixserverlib.PDU, 0, len(state))
	for _, event := range state {
		if event.EventID() == skip {
			continue
		}
		sorted = append(sorted, event)
	}
	// Insertion sort: room state is tens of events, not thousands, and this keeps the ordering
	// stable without pulling in a sort of a closure over an interface method.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Depth() < sorted[j-1].Depth(); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted
}

// createdMS is the room's age, taken from the create event rather than the clock.
//
// A room list sorted by creation time would otherwise reorder every time a room is joined from
// elsewhere, because the join is when this server heard of it and not when it began.
func createdMS(state []gomatrixserverlib.PDU) int64 {
	for _, event := range state {
		if event.Type() == string(matrix.EventTypeCreate) {
			return int64(event.OriginServerTS())
		}
	}
	return time.Now().UnixMilli()
}
