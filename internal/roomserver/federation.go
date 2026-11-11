package roomserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// JoinTemplate is the unsigned `m.room.member` event a peer is invited to build a join on.
//
// The room's own server supplies it because only this server knows where the room's DAG currently
// ends and which state authorises the join. The peer fills in its own sender, signs the result with
// its own key, and sends it back to be accepted — so every field that must be right for the join to
// be acceptable at all (depth, previous events, authorising events) comes from here, and a wrong one
// turns a join into a rejection the peer has no way to debug.
type JoinTemplate struct {
	// RoomVersion is the version the join event must be built to: the peer needs it to pick the
	// right event format and the right algorithm for its event ID.
	RoomVersion gomatrixserverlib.RoomVersion
	// Event is the template, unsigned and without a sender.
	Event gomatrixserverlib.ProtoEvent
	// AuthEventIDs is kept alongside the template for the caller's diagnostics; it is also
	// inside Event.
	AuthEventIDs []string
}

// MakeJoinTemplate answers `GET /_matrix/federation/v1/make_join/{roomID}/{userID}`.
//
// `requested` is the peer's list of room versions it can build, in its own order of preference; it
// may be empty, which means "surprise me". An empty list is not a refusal, and a version this server
// cannot handle is answered with the version it *would* have to use, because that is the only piece
// of information the peer can act on.
func (s *Service) MakeJoinTemplate(ctx context.Context, roomID, userID string, requested []string) (*JoinTemplate, error) {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			// A room this server has never heard of is refused the same way a room the
			// peer may not join is refused, so that make_join cannot be used to ask
			// whether a room ID exists here.
			return nil, matrix.NewForbidden("this server cannot join that room")
		}
		return nil, err
	}
	roomVersion := gomatrixserverlib.RoomVersion(room.Version)
	supportsRoomVersion := false
	if version, err := s.events.RoomVersion(string(roomVersion)); err == nil && version != nil {
		supportsRoomVersion = true
	}
	if len(requested) > 0 {
		allowed := false
		for _, want := range requested {
			if want == string(roomVersion) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, matrix.NewIncompatibleRoomVersion(string(roomVersion))
		}
	}
	if !supportsRoomVersion {
		return nil, matrix.NewIncompatibleRoomVersion(string(roomVersion))
	}

	parsed, err := s.parseUser(userID)
	if err != nil {
		return nil, err
	}

	tip, err := s.store.Events.Tip(ctx, roomID)
	if err != nil && !store.IsNotFound(err) {
		return nil, err
	}
	prevEvents := []string{}
	depth := int64(1)
	if tip != nil {
		prevEvents = append(prevEvents, tip.EventID)
		depth = tip.Depth + 1
	}

	authEventIDs, err := s.joinAuthEventIDs(ctx, roomID, parsed)
	if err != nil {
		return nil, err
	}

	// The content carries only the membership. A peer may set displayname and avatar_url in its
	// own join event; inventing them here would be this server describing somebody it has not
	// asked.
	content, err := json.Marshal(map[string]any{"membership": string(matrix.MembershipJoin)})
	if err != nil {
		return nil, fmt.Errorf("encoding the join template content: %w", err)
	}

	template := JoinTemplate{
		RoomVersion:  roomVersion,
		AuthEventIDs: authEventIDs,
	}
	template.Event = gomatrixserverlib.ProtoEvent{
		Type:     string(matrix.EventTypeMember),
		StateKey: stateKeyOf(userID),
		RoomID:   roomID,
		Depth:    depth,
		// `sender`, `origin` and `origin_server_ts` are deliberately absent. The peer fills in
		// who the event is from and when its own clock said so, and signs it — which is the
		// whole point: this server offers a place in the graph, not an identity.
		//
		// Previous and authorising events are event IDs, the format room version 3 and later
		// use. Room versions 1 and 2 wrap each one with a hash of the event, and this server
		// creates rooms in a modern version, so a v1/v2 room joining in over federation is the
		// case that would need the extra work.
		PrevEvents: prevEvents,
		AuthEvents: authEventIDs,
		Content:    content,
	}
	return &template, nil
}

// joinAuthEventIDs returns the events that authorise this user joining: the room's create event,
// its join rules, its power levels if it has them, and the user's own membership if they have ever
// had one (a re-join is authorised by the membership it replaces).
//
// That is the specification's authorisation table for `m.room.member` with membership `join`, and it
// is also what the peer will put in the signed event — so getting it wrong here does not produce a
// warning, it produces a join this server then refuses to accept.
func (s *Service) joinAuthEventIDs(ctx context.Context, roomID, userID string) ([]string, error) {
	tuples := []struct{ eventType, stateKey string }{
		{string(matrix.EventTypeCreate), ""},
		{string(matrix.EventTypeJoinRules), ""},
		{string(matrix.EventTypePowerLevels), ""},
		{string(matrix.EventTypeMember), userID},
	}
	out := make([]string, 0, len(tuples))
	for _, tuple := range tuples {
		entry, err := s.store.State.Get(ctx, roomID, tuple.eventType, tuple.stateKey)
		if errors.Is(err, store.ErrNotFound) {
			// An optional tuple that is simply not set. Power levels in particular may
			// legitimately be absent, in which case the room version's defaults apply.
			continue
		}
		if err != nil {
			return nil, err
		}
		if entry.EventID != "" {
			out = append(out, entry.EventID)
		}
	}
	return out, nil
}

// parseUser accepts the user ID the peer is joining as and checks it is syntactically a user ID.
//
// The domain is not required to be the peer's own — a server may legitimately be joining a user it
// hosts, and this server checks the *signature* on the resulting event against the key of the domain
// that claims it, which is where the real authorisation lives.
func (s *Service) parseUser(userID string) (string, error) {
	localpart, domain, err := matrix.ParseUserID(userID, s.cfg.ServerName)
	if err != nil || localpart == "" || domain == "" {
		return "", matrix.NewInvalidParam("%q is not a user ID", userID)
	}
	return userID, nil
}

func stateKeyOf(v string) *string { return &v }

// RoomVersionFor returns the room version implementation a federation request must be parsed with.
//
// The version is not negotiable: an event is unmarshalled according to its room version, and parsing
// a room version 11 event with a room version 1 parser produces an event whose ID is wrong — a
// different event by the only definition that matters. An unknown room is reported the way every
// other endpoint reports it, so that no federation route becomes a way to ask which room IDs live
// here.
func (s *Service) RoomVersionFor(ctx context.Context, roomID string) (gomatrixserverlib.IRoomVersion, error) {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
		}
		return nil, err
	}
	return s.events.RoomVersion(room.Version)
}

// JoinAcceptance is what a peer needs back after it has sent us a signed join.
//
// The shape is not a courtesy. A joining server cannot build a room from the join event alone: it has
// no state, no idea what authorises the join, and no way to check either. So the answer carries the
// room's current state and the authentication chain behind it, and the peer verifies both — including
// that the join it just signed is allowed by the state we claim. That is why this data has to come
// from the room's real state rather than from anything convenient.
type JoinAcceptance struct {
	// Origin names the server the join event is attributed to.
	Origin string
	// State is the room's state at the accepted event, as signed event JSON.
	State []json.RawMessage
	// AuthChain is the closure of events that authorise the join.
	AuthChain []json.RawMessage
	// JoinEvent is the accepted event itself, echoed for the peer's restricted-join path.
	JoinEvent json.RawMessage
	// ServersInRoom lists the domains the room's members live on: whom the peer can ask next.
	ServersInRoom []string
}

// AcceptFederationJoin admits a peer's signed join event into a room of ours.
//
// `origin` is the server whose signature the caller has already verified on the request and on the
// event. The checks made here are the ones that turn "a correctly signed event from a real server"
// into "an event this room accepts":
//
//   - the event is a join for this room, and it joins the user whose signature is on it — a server
//     may speak for its own users and for nobody else's;
//   - the events it claims to follow are events we hold, because an event attached to nothing is a
//     fork, not an addition;
//   - the room's own authorisation rules allow it, which is the same check a local join passes
//     through — a private room does not become public because the request arrived on the federation
//     port.
//
// Receiving the same event twice is normal (a peer retrying after a lost response) and returns the
// state again without appending anything.
func (s *Service) AcceptFederationJoin(ctx context.Context, roomID string, origin string, event gomatrixserverlib.PDU) (*JoinAcceptance, error) {
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
		}
		return nil, err
	}
	version, err := s.events.RoomVersion(room.Version)
	if err != nil {
		return nil, err
	}

	if err := checkJoinEvent(event, roomID, origin); err != nil {
		return nil, err
	}

	state, err := s.stateAt(ctx, roomID)
	if err != nil {
		return nil, err
	}

	existing, err := s.store.Events.ByEventID(ctx, event.EventID())
	switch {
	case err == nil:
		// A retry. The peer wants the state, not a second copy of the event.
		_ = existing
	case store.IsNotFound(err):
		if err := s.ingestRemoteEvent(ctx, roomID, state, event); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	// The state is read after the append so that it includes the join itself: the peer is
	// entitled to see the room as it now stands, including the membership it just created.
	stateRows, err := s.stateAt(ctx, roomID)
	if err != nil {
		return nil, err
	}
	acceptance := JoinAcceptance{
		Origin:    origin,
		State:     eventJSON(stateRows),
		AuthChain: eventJSON(nil),
	}
	chain, err := s.authChain(ctx, event)
	if err != nil {
		return nil, err
	}
	acceptance.AuthChain = chain
	acceptance.JoinEvent = json.RawMessage(event.JSON())
	if acceptance.ServersInRoom, err = s.serversInRoom(ctx, roomID); err != nil {
		return nil, err
	}

	// Room version 11 and later describe the room by the hash of its create event rather than by
	// a room ID field, and the peer needs to be told which of the two it is holding.
	_ = version
	s.notifier.Publish()
	return &acceptance, nil
}

// AcceptFederationEvent admits one event that a peer relayed to us about a room we are in.
//
// This is the ingest half of `PUT /_matrix/federation/v1/send/{txnID}`. It differs from a
// `send_join` in one respect that matters: a server relaying events is not necessarily the server
// that sent them. Federation expects peers to forward what they receive, so the origin of the
// transaction is not required to own the sender's domain — the event's own signature, checked by the
// caller against the *sender's* published key, is what says who it came from.
//
// Everything else is the same discipline: the room must be one we hold, the history it follows must
// be history we have, and the room's own rules must allow it.
func (s *Service) AcceptFederationEvent(ctx context.Context, roomID string, event gomatrixserverlib.PDU) error {
	if _, err := s.store.Rooms.Get(ctx, roomID); err != nil {
		if store.IsNotFound(err) {
			return matrix.NewNotFound("Room %q does not exist on this homeserver", roomID)
		}
		return err
	}
	if existing, err := s.store.Events.ByEventID(ctx, event.EventID()); err == nil {
		// A peer retrying a transaction whose response was lost. The event is here; say so.
		_ = existing
		return nil
	} else if !store.IsNotFound(err) {
		return err
	}
	state, err := s.stateAt(ctx, roomID)
	if err != nil {
		return err
	}
	if err := s.ingestRemoteEvent(ctx, roomID, state, event); err != nil {
		return err
	}
	s.notifier.Publish()
	return nil
}

// ingestRemoteEvent checks one remote event against the room and appends it.
func (s *Service) ingestRemoteEvent(ctx context.Context, roomID string, state []gomatrixserverlib.PDU, event gomatrixserverlib.PDU) error {
	if err := s.requireKnownPrevEvents(ctx, event); err != nil {
		return err
	}
	if err := s.events.Authorize(event, state); err != nil {
		return err
	}
	if err := s.appendRemoteEvent(ctx, roomID, event); err != nil {
		return err
	}
	return s.applyRemoteRedaction(ctx, roomID, event)
}

// applyRemoteRedaction carries out the effect of a redaction that arrived from a peer.
//
// A redaction is only half an event: it says "forget what that other event said", and the forgetting
// happens to the event it names. A local redaction applies the effect inside the same request that
// created it; a remote one needs the effect applied here, or the room this server serves keeps
// showing content that the rest of the world has been told to drop.
//
// A redaction of an event this server has never heard of is not an error and is not applied to
// anything: the target may simply be outside what this server keeps. Complement sends exactly this
// case on purpose, to check that the server survives it rather than rejecting a transaction over an
// event nobody can see.
func (s *Service) applyRemoteRedaction(ctx context.Context, roomID string, event gomatrixserverlib.PDU) error {
	redacts := event.Redacts()
	if redacts == "" || event.Type() != string(matrix.EventTypeRedaction) {
		return nil
	}
	target, err := s.store.Events.ByEventID(ctx, redacts)
	if store.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		return err
	}
	redacted, err := s.events.Redact(room.Version, target.JSON)
	if err != nil {
		return err
	}
	return s.store.Events.ReplaceJSON(ctx, redacts, redacted)
}

// gapFillAnchors is where this server stopped following a room, for the purpose of asking a peer to
// reconnect the thread: the predecessors it holds, and the newest event it has for the room.
func (s *Service) gapFillAnchors(ctx context.Context, event gomatrixserverlib.PDU, held []string) []string {
	anchors := append([]string(nil), held...)
	if tip, err := s.store.Events.Tip(ctx, event.RoomID().String()); err == nil && tip != nil {
		if !containsString(anchors, tip.EventID) {
			anchors = append(anchors, tip.EventID)
		}
	}
	return anchors
}

// checkJoinEvent makes sure the event is what a send_join is allowed to carry.
func checkJoinEvent(event gomatrixserverlib.PDU, roomID, origin string) error {
	if event.Type() != string(matrix.EventTypeMember) {
		return matrix.NewForbidden("send_join requires an m.room.member event, got %q", event.Type())
	}
	stateKey := event.StateKey()
	if stateKey == nil || *stateKey == "" {
		return matrix.NewForbidden("a join event must name the user it joins")
	}
	if string(event.SenderID()) != *stateKey {
		// The signature says which server sent this; the state key says who it claims to join.
		// Allowing one server to submit a join for another server's user would let a single
		// compromised peer populate a room with identities it does not hold.
		return matrix.NewForbidden("a join may only speak for the user whose server signed it")
	}
	sender, err := spec.NewUserID(string(event.SenderID()), true)
	if err != nil {
		return matrix.NewForbidden("%q is not a user ID", event.SenderID())
	}
	if string(sender.Domain()) != origin {
		return matrix.NewForbidden("%q may not join as %s: that user belongs to %s",
			origin, *stateKey, sender.Domain())
	}
	if event.RoomID().String() != roomID {
		return matrix.NewForbidden("the event is for room %q, not %q", event.RoomID(), roomID)
	}
	var content struct {
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(event.Content(), &content); err != nil {
		return matrix.NewForbidden("the join event's content cannot be read")
	}
	if content.Membership != string(matrix.MembershipJoin) {
		return matrix.NewForbidden("send_join carries membership %q, not join", content.Membership)
	}
	return nil
}

// MissingPredecessorError names an event this server would have to hold before it could admit another.
//
// It is a typed error and not a sentence, because "go and fetch this" and "refuse" are different
// answers to the same observation and only the transport can fetch. A caller that string-matches an
// error message to decide that is a caller that breaks the first time the wording changes — which
// happens, because error text is written for the log.
type MissingPredecessorError struct {
	// EventID is the event this server is missing.
	EventID string
	// Anchors are event IDs this server holds that the missing events are expected to hang from:
	// the predecessors it does have, and the room's current tip. They are part of the error because
	// they answer the question a peer asks back — "missing what, starting from where?" A gap-fill
	// request with no anchor is a request for the whole room's history, which is not a question worth
	// asking and not one this server could answer either.
	//
	// The tip matters because a gap is usually a run: an event whose single predecessor is missing has
	// no held predecessors at all, and the peer is asked to fill from wherever this server stopped
	// following the room.
	Anchors []string
	// Err is the refusal to give the peer if the gap cannot be filled.
	Err *matrix.APIError
}

func (e *MissingPredecessorError) Error() string { return e.Err.Error() }

// Unwrap keeps the HTTP status and error code reachable through errors.As.
func (e *MissingPredecessorError) Unwrap() error { return e.Err }

// requireKnownPrevEvents refuses an event that does not attach to the history we have.
//
// The alternative is to accept it and hope: an event whose predecessors are unknown sits at the start
// of a branch nobody can reach, and the room's history quietly splits — every later reader walks back
// from the tip and stops short of everything before the hole, so a room assembled this way has
// sections its own members cannot retrieve.
//
// The refusal names the missing event as a MissingPredecessorError so the transport can try to fetch
// it (see internal/fedapi/gapfill.go) before giving up. Refusing is still the answer when nothing can
// be fetched; what must not happen is silently storing the event because fetching is inconvenient.
func (s *Service) requireKnownPrevEvents(ctx context.Context, event gomatrixserverlib.PDU) error {
	prev := event.PrevEventIDs()
	if len(prev) == 0 {
		return nil
	}
	rows, err := s.store.Events.ByEventIDs(ctx, prev)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(rows))
	held := make([]string, 0, len(rows))
	for _, row := range rows {
		known[row.EventID] = true
		held = append(held, row.EventID)
	}
	for _, id := range prev {
		if !known[id] {
			return &MissingPredecessorError{
				EventID: id,
				Anchors: s.gapFillAnchors(ctx, event, held),
				Err:     matrix.NewForbidden("this server does not have the event %q that the join follows", id),
			}
		}
	}
	return nil
}

// appendRemoteEvent stores an event that arrived already built and signed.
//
// The local write path builds and signs an event itself; this one must not. Re-signing a peer's event
// would destroy the signature that proves who sent it, and would make this server the author of
// somebody else's statement.
func (s *Service) appendRemoteEvent(ctx context.Context, roomID string, event gomatrixserverlib.PDU) error {
	req := remoteAppendRequest(roomID, event)
	return s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		writer := &eventWriter{service: s, tx: tx, ctx: ctx}
		return writer.appendPrebuilt(req, event)
	})
}

// authChain returns every event that authorises this one, transitively.
//
// A peer cannot check a join against the join's own `auth_events` alone, because each of those is
// itself authorised by others; the specification therefore sends the closure. Cycles are impossible in
// a correct room graph, but a hostile or buggy one is not, so the walk is bounded by the set of
// events already visited.
func (s *Service) authChain(ctx context.Context, event gomatrixserverlib.PDU) ([]json.RawMessage, error) {
	var (
		out     []json.RawMessage
		visited = map[string]bool{}
		queue   = append([]string(nil), event.AuthEventIDs()...)
	)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		row, err := s.store.Events.ByEventID(ctx, id)
		if store.IsNotFound(err) {
			// An auth event this server does not have. Omitting it is the only honest
			// answer; the peer's own check will report the gap rather than accept less.
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(row.JSON))
		var parent struct {
			AuthEvents []string `json:"auth_events"`
		}
		if err := json.Unmarshal([]byte(row.JSON), &parent); err == nil {
			queue = append(queue, parent.AuthEvents...)
		}
	}
	if out == nil {
		return []json.RawMessage{}, nil
	}
	return out, nil
}

// serversInRoom lists the domains with members in this room, which is whom a peer may ask about it
// next. A brand new member's own server is included: it is in the room now.
func (s *Service) serversInRoom(ctx context.Context, roomID string) ([]string, error) {
	members, err := s.store.Memberships.List(ctx, roomID, []string{"join"}, nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	for _, member := range members {
		_, domain, err := matrix.ParseUserID(member.UserID, s.cfg.ServerName)
		if err != nil || seen[domain] {
			continue
		}
		seen[domain] = true
		out = append(out, domain)
	}
	return out, nil
}

func eventJSON(events []gomatrixserverlib.PDU) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(events))
	for _, event := range events {
		out = append(out, json.RawMessage(event.JSON()))
	}
	return out
}

// appendPrebuilt appends an event that arrived already built, signed, and named by its own event ID.
//
// It is deliberately a separate path from the local append rather than a flag on it. The local path
// chooses the event's predecessors, authorises it, builds it and signs it; a federated event has
// already had all four decided by whoever sent it, and this server's job is to check them, not to
// redo them. Merging the two would eventually mean re-signing a peer's event, which would destroy the
// signature that says who sent it and make this server the author of somebody else's statement.
func (w *eventWriter) appendPrebuilt(req txAppend, event gomatrixserverlib.PDU) error {
	s := w.service
	ctx := w.ctx

	room, err := s.store.Rooms.Get(ctx, req.roomID)
	if err != nil {
		return err
	}

	row := &store.EventRow{
		EventID:        event.EventID(),
		RoomID:         req.roomID,
		Type:           event.Type(),
		StateKey:       event.StateKey(),
		JSON:           string(event.JSON()),
		RoomVersion:    room.Version,
		Depth:          event.Depth(),
		OriginServerTS: int64(event.OriginServerTS()),
		ReceivedMS:     time.Now().UnixMilli(),
		Sender:         string(event.SenderID()),
	}
	row.RelatesToEventID, row.RelType = relationOf(event)
	if raw, err := json.Marshal(event.PrevEventIDs()); err == nil {
		row.PrevEvents = string(raw)
	}
	if raw, err := json.Marshal(event.AuthEventIDs()); err == nil {
		row.AuthEvents = string(raw)
	}

	written, err := s.store.Events.Insert(ctx, w.tx, row)
	if err != nil {
		return err
	}
	if event.StateKey() != nil {
		replaced, err := s.store.State.Upsert(ctx, w.tx, req.roomID, written.EventID,
			req.eventType, req.stateKey, written.ID)
		if err != nil {
			return err
		}
		return s.applyStateSideEffects(ctx, w.tx, room, req, written, replaced)
	}
	return nil
}

// AuthChainFor answers `GET /_matrix/federation/v1/event_auth/{roomID}/{eventID}`: every event that
// authorised the one a peer named, transitively — the event's auth chain, which is not its own event.
//
// A peer checking a history it is being shown cannot evaluate one event from its own
// `auth_events` alone, because each of those was itself authorised by others. The closure is what
// lets the peer run the same rules locally instead of trusting whoever is relaying.
//
// "No such event" and "that event lives in a different room" get the same refusal. This is a read a
// peer can aim at any room and event ID it can guess, and telling the two apart confirms which
// identifiers this server really holds.
func (s *Service) AuthChainFor(ctx context.Context, roomID, eventID string) ([]json.RawMessage, error) {
	row, err := s.store.Events.ByEventID(ctx, eventID)
	if err != nil || row.RoomID != roomID {
		return nil, matrix.NewForbidden("Event %q not found in room %q", eventID, roomID)
	}
	event, err := s.Events().ParseRoomVersioned(row.RoomVersion, json.RawMessage(row.JSON))
	if err != nil {
		// A stored event this server cannot re-read is a broken row, not a caller error; the
		// chain is incomplete either way, so say so rather than hand back a partial answer.
		return nil, matrix.NewUnknown(fmt.Sprintf("the event %q cannot be read back", eventID))
	}
	chain, err := s.authChain(ctx, event)
	if err != nil {
		return nil, err
	}
	// The event itself is not in the answer. An event's auth chain is defined as the closure of the
	// events that authorised it, and it is a set the peer is asked to run rules against: adding the
	// event to the set it authorises would make the answer describe an event that is not part of the
	// decision, and a peer counting on the size of the chain would be counting wrong.
	return chain, nil
}
