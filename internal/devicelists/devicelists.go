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
	"log/slog"
	"sort"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Service is the device-list log.
type Service struct {
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
func (s *Service) Announce(ctx context.Context, userID string, left bool) error {
	localpart, _, err := matrix.ParseUserID(userID, s.serverName)
	if err != nil {
		// A remote user's device list is somebody else's to announce.
		return nil
	}
	observers, err := s.observers(ctx, localpart)
	if err != nil {
		return err
	}
	if len(observers) == 0 {
		return nil
	}
	position, err := s.store.Events.MaxPosition(ctx)
	if err != nil {
		return err
	}
	// One past the tip, so a change made while a sync is in flight still counts as after that
	// sync's token. Anything less answers a client that is waiting for news of the login it just
	// performed with "nothing happened".
	position++
	for _, observer := range observers {
		change := store.DeviceListChange{UserID: userID, Observer: observer, Left: left, StreamPos: position}
		if err := s.store.DeviceLists.Append(ctx, &change); err != nil {
			return err
		}
	}
	if s.notifier != nil {
		s.notifier.Publish()
	}
	slog.Debug("device lists: announced a change", "user_id", userID, "left", left, "observers", len(observers))
	return nil
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
func (s *Service) observers(ctx context.Context, localpart string) ([]string, error) {
	seen := map[string]bool{localpart: true}
	rooms, err := s.store.Memberships.RoomsForUser(ctx, localpart)
	if err != nil {
		return nil, err
	}
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
