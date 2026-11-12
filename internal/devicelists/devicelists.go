// Package devicelists keeps people told when each other's devices change.
//
// End-to-end encryption makes a device's identity keys a fact other people depend on: to start an
// encrypted conversation with a colleague, a client has to know every device that colleague has. When
// the colleague buys a phone, that list changes, and a client that does not find out keeps encrypting
// for the old set — the new device silently never receives anything, which is a bug the sender cannot
// observe.
//
// So a change is written to a log addressed to each observer who cares: anybody who shares a room with
// the user, plus the user's own other devices. The log is stamped with the event stream position so a
// client's existing sync token says which changes it has already been told about, and `/keys/changes`
// answers the same question between two tokens.
package devicelists

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Service is the device-list log.
type Service struct {
	// peer is how a change reaches the servers that share rooms with a local user. Nil means this
	// server is not federating, and a device list is then a purely local fact.
	peer       DeviceListPeer
	store      *store.Store
	notifier   *notifier.Notifier
	serverName string
}

// Options configures a [Service].
type Options struct {
	// ServerName decides whose device lists this server may announce.
	ServerName string
}

// New builds the device-list log.
func New(st *store.Store, n *notifier.Notifier, opts Options) *Service {
	return &Service{store: st, notifier: n, serverName: opts.ServerName}
}

// Announce records that one user's device list moved, and tells the people who depend on it.
//
// `left` is a different message from `changed`: a client should drop the keys of somebody who left a
// shared room and re-query the keys of somebody who is still in it. Getting that backwards either
// keeps trusting a device that is gone or throws away keys still in use.
// DeviceListPeer carries a change to a local user's devices to the servers that share rooms with them.
//
// It is a seam rather than a call into the federation layer for the reason every other seam here
// exists: a device-list change is a fact about this server's users, and it stays true whether or not
// anything can be told about it.
type DeviceListPeer interface {
	// SendDeviceListUpdate delivers one `m.device_list_update` content to one destination.
	SendDeviceListUpdate(ctx context.Context, destination string, content json.RawMessage) error
}

// WithPeer installs the federation transport and returns the service for chaining.
func (s *Service) WithPeer(peer DeviceListPeer) *Service {
	s.peer = peer
	return s
}

// AnnounceRoom reports a membership change in one named room, which is the scope a membership change
// actually has.
//
// The room is not a convenience. The people who need to hear that Bob's device list went out of scope
// are the people he left behind in that room, and after the leave is written Bob is in no rooms at all
// — so asking "which rooms is he in" at that moment answers the wrong question, finds none, and the one
// notification that must be delivered is silently dropped.
func (s *Service) AnnounceRoom(ctx context.Context, userID, roomID string, left bool) error {
	return s.announce(ctx, userID, left, []string{roomID})
}

// Announce reports a change to a user's devices — a new login, a key upload — to the people who share
// a room with them.
//
// The subject is named the way the membership table names it, and that matters in both
// directions. A remote user's join has to be announceable at all — a client that is never told
// about a member it has just acquired quietly stops being able to encrypt to them — and it must
// not be announced under a bare local part, because "@alice:remote.example" and the local
// "@alice" would then be the same announcement: bob would be told to re-fetch somebody else's
// keys and would encrypt to the wrong device.
func (s *Service) Announce(ctx context.Context, userID string, left bool) error {
	return s.announce(ctx, userID, left, nil)
}

// announce records a change for every observer of the subject. With no rooms given, the scope is
// wherever the subject currently is; a membership change names the room it happened in.
func (s *Service) announce(ctx context.Context, userID string, left bool, rooms []string) error {
	key := matrix.MembershipKey(userID, s.serverName)
	local := !strings.Contains(key, ":")
	if len(rooms) == 0 {
		live, err := s.store.Memberships.RoomsForUser(ctx, key, string(matrix.MembershipJoin))
		if err != nil {
			return err
		}
		rooms = live
	}
	observers, err := s.observersIn(ctx, key, rooms)
	if err != nil {
		return err
	}
	position, err := s.store.Events.MaxPosition(ctx)
	if err != nil {
		return err
	}
	// One past the tip, so a change made while a sync is in flight still counts as after that
	// sync's token. Anything less answers a client that is waiting for the login it just performed
	// with "nothing happened".
	position++
	if len(observers) > 0 {
		for _, observer := range observers {
			change := store.DeviceListChange{UserID: userID, Observer: observer, Left: left, StreamPos: position}
			if err := s.store.DeviceLists.Append(ctx, &change); err != nil {
				return err
			}
		}
	}
	// The local observers are not the whole audience. A user's own server is the only one allowed to
	// speak for that user's devices, so the servers down the far end of a shared room have to hear it
	// from here — and they have to hear it even when this server has nobody local who cares, which is
	// exactly the shape of a room whose only other members are remote.
	if local && s.peer != nil {
		s.tellPeers(ctx, userID, rooms, left, position)
	}
	if s.notifier != nil {
		s.notifier.Publish()
	}
	slog.Debug("device lists: announced a change", "user_id", userID, "left", left, "observers", len(observers))
	return nil
}

// tellPeers sends this server's users' device-list changes to the servers that share a room with them.
//
// Best-effort, and deliberately so: the change is recorded here whatever a peer makes of it, and a
// server that is unreachable is not a reason to fail the key upload that prompted the announcement.
// What is lost when a send fails is one notification, and the peer's own next `/keys/query` for this
// user picks the truth up.
func (s *Service) tellPeers(ctx context.Context, userID string, rooms []string, left bool, position int64) {
	destinations, err := s.remoteDestinations(ctx, rooms)
	if err != nil {
		slog.Warn("device lists: could not work out which peers care about a device change",
			"user_id", userID, "error", err)
		return
	}
	if len(destinations) == 0 {
		return
	}
	devices, err := s.store.Devices.List(ctx, userID)
	if err != nil {
		slog.Warn("device lists: could not list the devices a change concerns", "user_id", userID, "error", err)
		return
	}
	// One EDU per device, because that is what the update is about: a peer is being told that the
	// keys of one device moved, and a batch would make it guess which. A deletion is sent even with
	// no devices left to name — the peer is holding keys for a device that is gone, and that is the
	// one message it cannot work out for itself.
	updates := make([]map[string]any, 0, max(len(devices), 1))
	for _, device := range devices {
		updates = append(updates, map[string]any{"user_id": userID, "device_id": device.ID})
	}
	if len(updates) == 0 {
		updates = append(updates, map[string]any{"user_id": userID})
	}
	for _, update := range updates {
		update["stream_id"] = position
		update["origin_server_ts"] = position
		update["deleted"] = left
		content, err := json.Marshal(update)
		if err != nil {
			slog.Warn("device lists: could not encode an update for a peer", "user_id", userID, "error", err)
			return
		}
		for _, destination := range destinations {
			if err := s.peer.SendDeviceListUpdate(ctx, destination, content); err != nil {
				slog.Warn("device lists: a peer could not be told about a device change",
					"user_id", userID, "destination", destination, "error", err)
			}
		}
	}
}

// remoteDestinations lists the other homeservers represented in a set of rooms — the servers that have
// a member in common with the user whose devices changed, and nobody else.
func (s *Service) remoteDestinations(ctx context.Context, rooms []string) ([]string, error) {
	seen := map[string]bool{}
	for _, roomID := range rooms {
		members, err := s.store.Memberships.List(ctx, roomID, []string{string(matrix.MembershipJoin)}, nil)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			_, domain, err := matrix.ParseUserID(member.UserID, s.serverName)
			if err != nil || domain == "" || domain == s.serverName {
				continue
			}
			seen[domain] = true
		}
	}
	out := make([]string, 0, len(seen))
	for domain := range seen {
		out = append(out, domain)
	}
	sort.Strings(out)
	return out, nil
}

// Delta returns what an observer has not been told yet, split the way /sync wants it.
//
// Both lists are deduplicated and sorted: a user who logged in on four devices moved their list four
// times, and a client presented with the same ID four times is being asked to re-query the same keys
// four times.
func (s *Service) Delta(ctx context.Context, observer string, since int64) (changed, left []string, err error) {
	rows, err := s.store.DeviceLists.List(ctx, observer, since)
	if err != nil {
		return nil, nil, err
	}
	return split(rows)
}

// Changes answers GET /keys/changes between two tokens.
//
// `to` is widened by one because of how changes are stamped: one past the event tip, so that a change
// made while a sync is in flight still counts as after that sync's token. The sync that reports such a
// change hands out the token naming the tip, which is one *below* the change — so an interval ending at
// that token has to reach one further, or the client that missed the sync would be told there was
// nothing to catch up on.
func (s *Service) Changes(ctx context.Context, observer, from, to string) (changed, left []string, err error) {
	fromPos := parseToken(from)
	toPos := parseToken(to)
	if toPos > 0 {
		toPos++
	}
	rows, err := s.store.DeviceLists.Range(ctx, observer, fromPos, toPos)
	if err != nil {
		return nil, nil, err
	}
	return split(rows)
}

// observers lists the local users that care about one user's devices: everyone who shares a room with
// them, plus their own other devices.
// observers returns the membership keys of everyone who shares a room with the subject.
//
// Only local users are returned: a change is something a /sync reads, and a remote user never syncs
// here, so recording changes for them would be rows nobody will ever ask for.
func (s *Service) observersIn(ctx context.Context, localpart string, rooms []string) ([]string, error) {
	seen := map[string]bool{localpart: true}
	for _, roomID := range rooms {
		members, err := s.store.Memberships.List(ctx, roomID, []string{string(matrix.MembershipJoin)}, nil)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			seen[member.Localpart] = true
		}
	}
	out := make([]string, 0, len(seen))
	for observer := range seen {
		// A membership key carrying a domain belongs to a remote user, who will never read a change
		// made here.
		if strings.Contains(observer, ":") {
			continue
		}
		out = append(out, observer)
	}
	return out, nil
}

// split turns log rows into the two sorted, deduplicated lists a response carries.
func split(rows []store.DeviceListChange) (changed, left []string, err error) {
	seenChanged := map[string]bool{}
	seenLeft := map[string]bool{}
	for _, row := range rows {
		if row.Left {
			seenLeft[row.UserID] = true
			continue
		}
		seenChanged[row.UserID] = true
	}
	for userID := range seenChanged {
		changed = append(changed, userID)
	}
	for userID := range seenLeft {
		left = append(left, userID)
	}
	sort.Strings(changed)
	sort.Strings(left)
	return changed, left, nil
}

// parseToken reads a sync token as a position, tolerating anything unreadable as zero: an
// unreadable token on this endpoint means "from the beginning", not an error, because the client
// can only recover by re-querying every key it has.
func parseToken(token string) int64 {
	var position int64
	for i, c := range token {
		if i == 0 && (c == 's' || c == 'S') {
			continue
		}
		if c < '0' || c > '9' {
			return 0
		}
		position = position*10 + int64(c-'0')
	}
	return position
}
