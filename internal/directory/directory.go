// Package directory answers "who is Alice?" for the users of this homeserver.
//
// The user directory is not a list of accounts. It is a list of *names*, each with the room it was
// said in, because a display name is a per-room fact: a person may legitimately be "Alice Cooper"
// to the world and "Freddy" to one colleague, and the second name is not information the rest of
// the server gets to publish. So every row records where it came from, and every search is
// filtered by what the searcher is allowed to have seen — which is the difference between a
// directory and a leak.
package directory

import (
	"context"
	"sort"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Service is the user directory.
type Service struct {
	store *store.Store
	cfg   *config.Config
}

// New builds the service.
func New(st *store.Store, cfg *config.Config) *Service {
	return &Service{store: st, cfg: cfg}
}

// Result is one entry of a search response.
type Result struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// PublishProfile records a user's own profile name, the one that is visible to anybody allowed to
// find them at all.
func (s *Service) PublishProfile(ctx context.Context, localpart, displayName, avatarURL string) error {
	return s.store.Directory.Put(ctx, &store.DirectoryEntry{
		UserID:      matrix.UserID(localpart, s.cfg.ServerName),
		RoomID:      "",
		DisplayName: displayName,
		AvatarURL:   avatarURL,
		Localpart:   localpart,
	})
}

// PublishRoomName records the name a user goes by in one room, from their member event. An empty
// name removes the row: a member event that no longer carries a display name means the room has
// stopped calling them anything of its own.
func (s *Service) PublishRoomName(ctx context.Context, roomID, userID, membership, displayName, avatarURL string) error {
	if displayName == "" && avatarURL == "" {
		return s.store.Directory.Delete(ctx, userID, roomID)
	}
	// Somebody who is no longer in the room is not somebody the room has a name for.
	if membership != matrix.MembershipJoin && membership != matrix.MembershipInvite {
		return s.store.Directory.Delete(ctx, userID, roomID)
	}
	localpart, _, err := matrix.ParseUserID(userID, s.cfg.ServerName)
	if err != nil {
		return nil // a remote user's name is not this directory's to publish
	}
	return s.store.Directory.Put(ctx, &store.DirectoryEntry{
		UserID:      userID,
		RoomID:      roomID,
		DisplayName: displayName,
		AvatarURL:   avatarURL,
		Localpart:   localpart,
	})
}

// RemoveRoom forgets a user's room-specific name, used when they leave or are banned.
func (s *Service) RemoveRoom(ctx context.Context, userID, roomID string) error {
	return s.store.Directory.Delete(ctx, userID, roomID)
}

// Search answers POST /user_directory/search.
//
// Who may be found is the interesting half of this endpoint. A name said in a private room is
// visible only to people who were in that room; a profile name is visible to anybody, but only
// because the user put it on an account that is in a public room — that is the public part of the
// server's user list, and it is the only part that is. And the searcher never sees themself, which
// is not a privacy rule but a usefulness one: a directory that returns the person using it is
// noise.
func (s *Service) Search(ctx context.Context, seekerLocalpart, term string, limit int) (results []Result, limited bool, err error) {
	if strings.TrimSpace(term) == "" {
		return []Result{}, false, nil
	}
	if limit <= 0 {
		limit = 10
	}
	entries, err := s.store.Directory.Search(ctx, term, limit)
	if err != nil {
		return nil, false, err
	}
	seeker := matrix.UserID(seekerLocalpart, s.cfg.ServerName)

	// The rooms the searcher is in, and the users who have made themselves public. Both are
	// read once and then asked about per candidate row, which is the difference between two
	// queries per search and one per candidate.
	sharedRooms, sharedUsers, err := s.roomsAndPeopleOf(ctx, seekerLocalpart)
	if err != nil {
		return nil, false, err
	}
	publicUsers, err := s.usersInPublicRooms(ctx)
	if err != nil {
		return nil, false, err
	}

	// One result per user: the same person may match through six rooms' names, and six rows for
	// Alice in a list of ten is a directory nobody can read.
	best := map[string]Result{}
	rank := map[string]int{}
	for _, entry := range entries {
		if entry.UserID == seeker {
			continue
		}
		visibility, score := visibilityOf(entry, sharedRooms, sharedUsers, publicUsers)
		if !visibility {
			continue
		}
		if existing, seen := rank[entry.UserID]; seen && existing >= score {
			continue
		}
		rank[entry.UserID] = score
		best[entry.UserID] = Result{
			UserID:      entry.UserID,
			DisplayName: entry.DisplayName,
			AvatarURL:   entry.AvatarURL,
		}
	}

	out := make([]Result, 0, len(best))
	for _, result := range best {
		out = append(out, result)
	}
	// Ranked by score, then by user ID so that two identical servers answer identically.
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].UserID] != rank[out[j].UserID] {
			return rank[out[i].UserID] > rank[out[j].UserID]
		}
		return out[i].UserID < out[j].UserID
	})
	if len(out) > limit {
		limited = true
		out = out[:limit]
	}
	return out, limited, nil
}

// visibilityOf decides whether a searcher may see one directory row, and how good a match it is.
//
// The score orders a user's own profile name above a name one room gave them, which is what makes
// a search for a profile name show the profile name — a person finding their colleague should not
// be shown the nickname a third room invented.
func visibilityOf(entry store.DirectoryEntry, sharedRooms, sharedUsers, publicUsers map[string]bool) (bool, int) {
	if entry.RoomID == "" {
		// The profile name. Public when the user has put themselves in a public room;
		// otherwise visible only to people who already share a room with them, which is the
		// same rule as the room-specific names below.
		if publicUsers[entry.UserID] {
			return true, 3
		}
		if sharedUsers[entry.UserID] {
			return true, 2
		}
		return false, 0
	}
	if !sharedRooms[entry.RoomID] {
		return false, 0
	}
	return true, 1
}

// roomsAndPeopleOf lists the rooms a user is in, and the other users they share a room with —
// every membership counts, because a room you left is still a room whose names you were told.
func (s *Service) roomsAndPeopleOf(ctx context.Context, localpart string) (rooms map[string]bool, users map[string]bool, err error) {
	memberships, err := s.store.Memberships.ListByUser(ctx, localpart)
	if err != nil {
		return nil, nil, err
	}
	rooms = make(map[string]bool, len(memberships))
	users = map[string]bool{}
	for _, membership := range memberships {
		rooms[membership.RoomID] = true
		peers, err := s.store.Memberships.List(ctx, membership.RoomID, nil, nil)
		if err != nil {
			return nil, nil, err
		}
		for _, peer := range peers {
			users[peer.UserID] = true
		}
	}
	delete(users, matrix.UserID(localpart, s.cfg.ServerName))
	return rooms, users, nil
}

// publicUsers lists the users who appear in a public room, and whose profile name is therefore
// publishable. A server whose user directory only knows about accounts nobody joined would not be
// a directory, and Synapse publishes the same set.
func (s *Service) usersInPublicRooms(ctx context.Context) (map[string]bool, error) {
	rooms, err := s.store.Rooms.ListWithJoinRule(ctx, []string{matrix.JoinRulePublic}, 0, "")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, room := range rooms {
		members, err := s.store.Memberships.List(ctx, room.ID,
			[]string{matrix.MembershipJoin}, nil)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			out[member.UserID] = true
		}
	}
	return out, nil
}
