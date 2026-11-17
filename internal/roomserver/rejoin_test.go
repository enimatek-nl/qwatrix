package roomserver

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// TestARoomRejoinedRemembersWhenItCameBack is about the room row, which is this server's own note about
// itself rather than anything a peer told it.
//
// Leaving a room and coming back later is an ordinary day. The row for a room this server never forgot
// is not re-created — a rejoin is an event in a room still held, not a federated join that would write a
// fresh row — so `joined_ms` kept the original join, and the room went on claiming to have been joined
// hours or days before it came back. `/hierarchy` reads that field, and so does anything else asking when
// this server got here. `left` is the same fact from the other end: a room just joined is not a room this
// server is not in, whatever a knock wrote into that column beforehand.
func TestARoomRejoinedRemembersWhenItCameBack(t *testing.T) {
	ctx := context.Background()
	const (
		roomID      = "!room:home.example"
		roomVersion = "11"
	)

	cfg := config.Default()
	cfg.ServerName = "home.example"
	st, err := store.Open(t.TempDir() + "/roomserver.db")
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	rooms := &Service{store: st, cfg: cfg, notifier: notifier.New()}

	joinedAgo := time.Now().Add(-6 * time.Hour)
	if err := st.Rooms.Create(ctx, &store.Room{
		ID: roomID, Version: roomVersion, Left: true, JoinedMS: joinedAgo.UnixMilli(),
	}); err != nil {
		t.Fatalf("recording the left room: %v", err)
	}

	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the signing key: %v", err)
	}
	version, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(roomVersion))
	if err != nil {
		t.Fatalf("room version %q: %v", roomVersion, err)
	}
	member := func(stateKey, membership string) gomatrixserverlib.PDU {
		t.Helper()
		key := stateKey
		built, err := version.NewEventBuilderFromProtoEvent(&gomatrixserverlib.ProtoEvent{
			RoomID:   roomID,
			SenderID: stateKey,
			Type:     string(matrix.EventTypeMember),
			StateKey: &key,
			Depth:    7,
			Content:  rawJSON(t, map[string]any{"membership": membership}),
		}).Build(time.Now(), "home.example", gomatrixserverlib.KeyID("ed25519:home"), private)
		if err != nil {
			t.Fatalf("building the membership of %s: %v", stateKey, err)
		}
		return built
	}

	// Somebody else's join is not this server joining: a room whose only members are remote is a room
	// this server holds history for, and it did not arrive anywhere.
	if err := st.Memberships.Upsert(ctx, nil, &store.Membership{
		RoomID: roomID, Localpart: "@far:elsewhere.example", UserID: "@far:elsewhere.example",
		Membership: matrix.MembershipJoin,
	}); err != nil {
		t.Fatalf("recording the remote member: %v", err)
	}
	appendEvent(ctx, t, st, rooms, roomID, member("@far:elsewhere.example", matrix.MembershipJoin))
	if room := readRoom(ctx, t, st, roomID); !room.Left || room.JoinedMS != joinedAgo.UnixMilli() {
		t.Errorf("a remote user's join moved this server's own participation: left=%v joined_ms=%d",
			room.Left, room.JoinedMS)
	}

	// This server's own user coming back moves it.
	appendEvent(ctx, t, st, rooms, roomID, member("@tim:home.example", matrix.MembershipJoin))
	room := readRoom(ctx, t, st, roomID)
	if room.Left {
		t.Error("the room is still marked left after this server's own user joined it")
	}
	if room.JoinedMS <= joinedAgo.UnixMilli() {
		t.Errorf("joined_ms is still %d, the join from before the gap; a rejoin has to move it",
			room.JoinedMS)
	}
}

// appendEvent writes one prebuilt state event through the path that keeps the room's denormalised rows in
// step, which is the half worth pinning: the membership row is one thing and the room's own note about
// itself is another.
func appendEvent(ctx context.Context, t *testing.T, st *store.Store, rooms *Service, roomID string,
	event gomatrixserverlib.PDU) {
	t.Helper()

	err := st.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		writer := &eventWriter{service: rooms, tx: tx, ctx: ctx}
		return writer.appendPrebuilt(remoteAppendRequest(roomID, event), event)
	})
	if err != nil {
		t.Fatalf("appending %s: %v", event.Type(), err)
	}
}

func readRoom(ctx context.Context, t *testing.T, st *store.Store, roomID string) *store.Room {
	t.Helper()

	room, err := st.Rooms.Get(ctx, roomID)
	if err != nil {
		t.Fatalf("reading the room back: %v", err)
	}
	return room
}
