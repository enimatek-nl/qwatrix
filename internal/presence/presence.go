// Package presence keeps and distributes the online status of local users.
//
// Presence is the part of the client-server API that is allowed to be approximate: it is
// advisory, it decays, and no correctness property depends on it. What must not be
// approximate is who is told about it — a user's status travels only to the users who share
// a room with them, because presence is otherwise a way to watch somebody's working hours.
package presence

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Service owns presence state.
type Service struct {
	store    *store.Store
	cfg      *config.Config
	notifier *notifier.Notifier
}

// New builds the presence service. The notifier must be the one the sync long-pollers wait
// on, or a presence change would not wake anybody.
func New(st *store.Store, cfg *config.Config, nf *notifier.Notifier) *Service {
	return &Service{store: st, cfg: cfg, notifier: nf}
}

// Valid reports whether a value is one of the presence states the specification defines.
func Valid(value string) bool {
	switch value {
	case store.PresenceOnline, store.PresenceUnavailable, store.PresenceOffline:
		return true
	default:
		return false
	}
}

// Set records a user's presence.
//
// An empty presence leaves the stored state alone and updates only the status message, which
// is what the specification's PUT allows; an empty status message clears it.
func (s *Service) Set(ctx context.Context, localpart, presence, statusMessage string) error {
	userID := matrix.UserID(localpart, s.cfg.ServerName)

	row, err := s.store.Presence.Get(ctx, localpart)
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	if row == nil {
		// A user who has never set a presence is not offline — that word is reserved for
		// somebody the server has seen and then lost. "unavailable" is the honest answer
		// for a user whose activity this server has never observed.
		row = &store.Presence{
			Localpart:    localpart,
			UserID:       userID,
			Presence:     store.PresenceUnavailable,
			LastActiveMS: s.nowMS(),
		}
	}

	now := s.nowMS()
	if presence != "" && presence != row.Presence {
		if row.Presence == store.PresenceOnline {
			row.PreviouslyActiveMS = row.LastActiveMS
		}
		row.Presence = presence
	}
	if presence != "" || statusMessage != "" {
		row.StatusMessage = statusMessage
	}
	if presence == store.PresenceOnline || presence == "" {
		row.LastActiveMS = now
	}
	row.UserID = userID

	if err := s.store.Presence.Set(ctx, row); err != nil {
		return err
	}
	// Presence travels to other users through their long-polls, so a change has to wake them
	// the same way a message does.
	s.notifier.Publish()
	return nil
}

// MarkSeen records that this server has just seen the user, which for now means: a user with no
// presence at all becomes online.
//
// Presence has to start somewhere, and a server that never says so leaves every client in the room
// rendering a stranger whose status is unknown forever. Marking activity is deliberately the only
// thing this does: a user who has said "unavailable" or set a status message keeps exactly what they
// said, because a sync is not entitled to overwrite a person's own words. It is also what makes the
// record change hands — writing it stamps a stream position, and that position is what puts the news
// in a room-mate's next sync.
func (s *Service) MarkSeen(ctx context.Context, localpart string) error {
	if localpart == "" {
		return nil
	}
	if _, err := s.store.Presence.Get(ctx, localpart); err == nil {
		return nil
	} else if !store.IsNotFound(err) {
		return err
	}
	return s.Set(ctx, localpart, store.PresenceOnline, "")
}

// ObserveUser is the room server's view of a user: a full user ID, which may belong to another
// server. Presence is only ever this server's to report, so a remote user is ignored; a local one is
// marked seen.
func (s *Service) ObserveUser(ctx context.Context, userID string) {
	localpart, isLocal := s.localOfUser(userID)
	if !isLocal {
		return
	}
	if err := s.MarkSeen(ctx, localpart); err != nil {
		slog.Warn("presence: recording activity", "user_id", userID, "error", err)
	}
}

// Get returns a user's presence. A user who has never set any is reported as unavailable
// rather than as an error: the endpoint asks what somebody's status is, and "unknown" is
// answerable as unavailable without turning the question into a 404.
func (s *Service) Get(ctx context.Context, localpart string) (*store.Presence, error) {
	row, err := s.store.Presence.Get(ctx, localpart)
	if store.IsNotFound(err) {
		return &store.Presence{
			Localpart: localpart,
			UserID:    matrix.UserID(localpart, s.cfg.ServerName),
			Presence:  store.PresenceUnavailable,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// Events returns the m.presence events a viewer should be told about.
//
// The set is limited to users who share a room with the viewer, which is the privacy rule of
// presence: without it, a client could watch a colleague's working day by syncing. The
// viewer's own presence is left out because a client already knows what it just set.
func (s *Service) Events(ctx context.Context, viewerLocalpart string, since int64) ([]json.RawMessage, error) {
	recent, refreshed, err := s.peerLocalparts(ctx, viewerLocalpart, since)
	if err != nil {
		return nil, err
	}

	// Two kinds of peer. Someone whose presence *changed* since the client's token is news in
	// the ordinary way. Someone who only became a peer — a stranger who just joined a room the
	// client is in — has a presence row older than that token, and a client that is never told
	// about them renders that person as unknown for as long as they stay quiet. So a room whose
	// membership moved gets its whole presence picture re-sent, which stops by itself as soon
	// as the client's token passes the join.
	var rows []store.Presence
	if len(recent) > 0 {
		changed, err := s.store.Presence.UpdatedSince(ctx, recent, since)
		if err != nil {
			return nil, err
		}
		rows = append(rows, changed...)
	}
	if len(refreshed) > 0 {
		// The negative `since` is "no bound": stream positions count up from one, so every row
		// is after it. Only peers who became new since the last sync reach this list, so a quiet
		// room re-sends nothing.
		seen := make(map[string]bool, len(rows))
		for _, row := range rows {
			seen[row.Localpart] = true
		}
		current, err := s.store.Presence.UpdatedSince(ctx, refreshed, -1)
		if err != nil {
			return nil, err
		}
		for _, row := range current {
			if !seen[row.Localpart] {
				rows = append(rows, row)
			}
		}
	}
	if len(rows) == 0 {
		return nil, nil
	}

	now := s.nowMS()
	out := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if row.Localpart == viewerLocalpart {
			continue
		}
		content := map[string]any{
			"presence": row.Presence,
			// last_active_ago is a duration, not a timestamp, so that a client does not
			// have to trust this server's clock to render "last seen 5 minutes ago".
			"last_active_ago":  maxInt64(now-row.LastActiveMS, 0),
			"currently_active": row.Presence == store.PresenceOnline,
		}
		if row.StatusMessage != "" {
			content["status_msg"] = row.StatusMessage
		}
		event, err := json.Marshal(map[string]any{
			"type":    "m.presence",
			"sender":  row.UserID,
			"content": content,
		})
		if err != nil {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

// peerLocalparts are the other local users who share at least one joined room with the viewer,
// split into those whose own presence is the news and those who are new to the viewer.
//
// Two queries and a map: presence is read on every sync, so it must not scale with the size of the
// room graph beyond the caller's own rooms.
func (s *Service) peerLocalparts(ctx context.Context, viewerLocalpart string, since int64) (recent, refreshed []string, err error) {
	// Every room the viewer is in, then the rooms they are actually joined to: the
	// repository's variadic argument *excludes* memberships, so filtering here is the only
	// way to ask for "joined" without accidentally asking for "not joined".
	allRooms, err := s.store.Memberships.ListByUser(ctx, viewerLocalpart)
	if err != nil {
		return nil, nil, err
	}

	seen := map[string]bool{viewerLocalpart: true}
	// A peer is judged by every room the two share, not by whichever room is read first: somebody
	// whose presence was recorded long ago in an old room is still news when they walk into a new
	// one, and "new" is a property of the room, not of the person.
	moved := map[string]bool{}
	ordered := make([]string, 0, len(allRooms))
	for _, room := range allRooms {
		if room.Membership != store.MembershipJoin {
			continue
		}
		members, err := s.store.Memberships.List(ctx, room.RoomID, []string{store.MembershipJoin}, nil)
		if err != nil {
			return nil, nil, err
		}
		// The viewer's own membership position counts too: somebody who has just joined a room
		// knows nothing about the people already in it, and their token is newer than every
		// presence change in there.
		roomMoved := room.StreamPos > since
		for _, member := range members {
			if member.StreamPos > since {
				roomMoved = true
			}
		}
		for _, member := range members {
			// Only local users have presence here; a remote user's status belongs to their
			// own server, and inventing it would be a lie about somebody else's building.
			localpart, isLocal := s.localOfUser(member.UserID)
			if !isLocal {
				continue
			}
			if !seen[localpart] {
				seen[localpart] = true
				ordered = append(ordered, localpart)
			}
			moved[localpart] = moved[localpart] || roomMoved
		}
	}
	// Sorted, so that two readers in identical rooms are answered identically and a sync
	// response for the same state is byte for byte the same twice.
	sort.Strings(ordered)
	recent = make([]string, 0, len(ordered))
	refreshed = make([]string, 0, len(ordered))
	for _, localpart := range ordered {
		if moved[localpart] {
			refreshed = append(refreshed, localpart)
		} else {
			recent = append(recent, localpart)
		}
	}
	return recent, refreshed, nil
}

func (s *Service) localOfUser(userID string) (string, bool) {
	localpart, domain, err := matrix.ParseUserID(userID, s.cfg.ServerName)
	if err != nil || !strings.EqualFold(domain, string(s.cfg.ServerName)) {
		return "", false
	}
	return localpart, true
}

func (s *Service) nowMS() int64 { return time.Now().UnixMilli() }

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
