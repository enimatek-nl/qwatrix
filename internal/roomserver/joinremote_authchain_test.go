package roomserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// TestAPeersAuthChainIsStoredWithoutBecomingRoomState is about the difference between what authorised
// a room and what the room is.
//
// A peer answering a federated join sends two lists. `state` is the room as it stands; `auth_chain` is
// the transitive closure of the events that authorised the join, and in a room older than a week that
// closure is a museum: joins that were later left, invites nobody accepted, bans long lifted. Stored as
// state, those museum pieces come alive. They appear in /members and in the member count, they make
// this server offer every later event to the homeservers they name — including the ones that were
// switched off and had their DNS removed years ago, at a full peer timeout per attempt, inside the join
// a client is still waiting on — and where the peer's `state` says nothing about that state key at all,
// the stale membership is never corrected by anything and simply becomes this server's belief about the
// room.
//
// So: auth-chain events are stored, because the next server that asks "what authorised this?" has to be
// answered, and applied to nothing.
func TestAPeersAuthChainIsStoredWithoutBecomingRoomState(t *testing.T) {
	ctx := context.Background()
	const (
		roomID      = "!room:peer.example"
		roomVersion = "11"
		peerName    = "peer.example"
	)

	cfg := config.Default()
	cfg.ServerName = "home.example"
	st, err := store.Open(t.TempDir() + "/roomserver.db")
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	rooms := &Service{store: st, cfg: cfg, notifier: notifier.New()}

	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the peer's signing key: %v", err)
	}
	version, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(roomVersion))
	if err != nil {
		t.Fatalf("room version %q: %v", roomVersion, err)
	}
	build := func(at time.Time, event gomatrixserverlib.ProtoEvent) gomatrixserverlib.PDU {
		t.Helper()
		event.RoomID = roomID
		built, err := version.NewEventBuilderFromProtoEvent(&event).Build(
			at, peerName, gomatrixserverlib.KeyID("ed25519:peer"), private)
		if err != nil {
			t.Fatalf("building %s: %v", event.Type, err)
		}
		return built
	}
	member := func(at time.Time, depth int64, stateKey, membership string) gomatrixserverlib.PDU {
		key := stateKey
		return build(at, gomatrixserverlib.ProtoEvent{
			SenderID:   stateKey,
			Type:       string(matrix.EventTypeMember),
			StateKey:   &key,
			Depth:      depth,
			PrevEvents: []string{roomID},
			Content:    rawJSON(t, map[string]any{"membership": membership}),
		})
	}

	now := time.Now()
	createKey := ""
	create := build(now.AddDate(-3, 0, 0), gomatrixserverlib.ProtoEvent{
		SenderID: "@owner:peer.example",
		Type:     string(matrix.EventTypeCreate),
		StateKey: &createKey,
		Depth:    1,
		Content: rawJSON(t, map[string]any{
			"creator": "@owner:peer.example", "room_version": roomVersion,
		}),
	})
	powerKey := ""
	power := build(now.AddDate(-3, 0, 0), gomatrixserverlib.ProtoEvent{
		SenderID: "@owner:peer.example",
		Type:     string(matrix.EventTypePowerLevels),
		StateKey: &powerKey,
		Depth:    2,
		Content: rawJSON(t, map[string]any{
			"users": map[string]any{"@owner:peer.example": 100}, "users_default": 0,
		}),
	})

	// What the room says now: @left is gone, and this server's own user is in.
	leftLeave := member(now.AddDate(0, -1, 0), 40, "@left:dead.example", matrix.MembershipLeave)
	own := member(now, 60, "@tim:home.example", matrix.MembershipJoin)
	// What the chain still carries: the join that leave replaced, and a member event for somebody the
	// peer's current state does not mention at all — the one case nothing else would ever correct.
	leftJoin := member(now.AddDate(-2, 0, 0), 10, "@left:dead.example", matrix.MembershipJoin)
	phantomJoin := member(now.AddDate(-1, 0, 0), 20, "@phantom:gone.example", matrix.MembershipJoin)

	remote := &RemoteJoin{
		Origin:      peerName,
		RoomVersion: roomVersion,
		JoinEvent:   own,
		State:       []gomatrixserverlib.PDU{create, power, leftLeave, own},
		AuthChain:   []gomatrixserverlib.PDU{create, power, leftJoin, phantomJoin},
	}
	if err := rooms.persistFederatedJoin(ctx, roomID, remote); err != nil {
		t.Fatalf("persisting the join the peer described: %v", err)
	}

	// Kept, because a peer checking the history of this room will ask for the chain.
	for _, event := range []gomatrixserverlib.PDU{leftJoin, phantomJoin} {
		if _, err := st.Events.ByEventID(ctx, event.EventID()); err != nil {
			t.Errorf("the auth-chain event %s was not stored: %v", event.EventID(), err)
		}
	}

	// Not current state, in either direction: the superseded join did not overwrite the leave, and the
	// membership nobody has did not become state at all.
	entry, err := st.State.Get(ctx, roomID, string(matrix.EventTypeMember), "@left:dead.example")
	if err != nil {
		t.Errorf("reading the membership of the user who left: %v", err)
	} else if entry.EventID != leftLeave.EventID() {
		t.Errorf("the room believes %s for @left:dead.example, want the leave %s — an event out of the "+
			"auth chain was applied as state", entry.EventID, leftLeave.EventID())
	}
	if _, err := st.State.Get(ctx, roomID, string(matrix.EventTypeMember), "@phantom:gone.example"); !store.IsNotFound(err) {
		t.Errorf("an auth-chain membership nobody holds is current state (err = %v)", err)
	}

	// And the overlap is the trap in the other direction. A room's create event, its power levels and
	// the memberships that authorise a join are named by the chain as much as they are described by the
	// state, and being mentioned as authorisation must not cost an event its place in the room — a server
	// whose copy is missing its peers' memberships cannot tell who is allowed to be told anything.
	for _, event := range []gomatrixserverlib.PDU{create, power, leftLeave, own} {
		stateKey := ""
		if key := event.StateKey(); key != nil {
			stateKey = *key
		}
		entry, err := st.State.Get(ctx, roomID, event.Type(), stateKey)
		if err != nil {
			t.Errorf("%s/%s never became current state: %v", event.Type(), stateKey, err)
			continue
		}
		if entry.EventID != event.EventID() {
			t.Errorf("%s/%s holds %s, want %s", event.Type(), stateKey, entry.EventID, event.EventID())
		}
	}

	// And not the membership tables either, which are what delivery and /members are built from.
	if got, err := st.Memberships.Get(ctx, roomID, "@left:dead.example"); err == nil {
		if got.Membership != matrix.MembershipLeave {
			t.Errorf("@left:dead.example is stored as %q, want %q", got.Membership, matrix.MembershipLeave)
		}
	} else if !store.IsNotFound(err) {
		t.Errorf("reading @left:dead.example's membership: %v", err)
	}
	if got, err := st.Memberships.Get(ctx, roomID, "@phantom:gone.example"); err == nil {
		t.Errorf("@phantom:gone.example is a member as %q; only the peer's state may say who is in a room",
			got.Membership)
	} else if !store.IsNotFound(err) {
		t.Errorf("reading @phantom:gone.example's membership: %v", err)
	}

	// The member count is the same denormalisation, and drifts the same way.
	room, err := st.Rooms.Get(ctx, roomID)
	if err != nil {
		t.Fatalf("reading the room back: %v", err)
	}
	if room.JoinedCount != 1 {
		t.Errorf("the room counts %d joined members, want 1 — the one this server sent", room.JoinedCount)
	}
}

func rawJSON(t *testing.T, value any) spec.RawJSON {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %v: %v", value, err)
	}
	return encoded
}
