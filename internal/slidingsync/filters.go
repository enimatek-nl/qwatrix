package slidingsync

// A list's filters: which rooms a client is actually looking at.
//
// A filter is a conjunction — direct chats, encrypted, not spaces — whose values *inside* one field are
// disjoined (`spaces: [a, b]` means a or b). Applied before the range, which is the whole point: a
// client that asked for twenty direct chats wants twenty direct chats, not whichever twenty rooms spoke
// last, of which it is then told three were direct.
//
// The rules that decide what a filter may read are worth stating because they are the difference between
// a filter and a way to read rooms:
//
//   - `is_dm` and the tag filters read the caller's own account data, which is where the specification
//     puts both. The server keeps no notion of its own about who counts as a contact; a client's
//     `m.direct` is the fact and this reads it.
//   - `spaces` reads the `m.space.child` state of the spaces the client named *and the caller is joined
//     to*, and never walks into subspaces. Named spaces the caller is not in are ignored rather than
//     followed, which is what stops a filter being a way to enumerate the contents of rooms over
//     somebody's shoulder, and what keeps one list request from becoming a recursive crawl over
//     federation.
//   - Everything else reads the room itself: its membership, its create event, its encryption state.
//
// A filter the server does not know is ignored, not refused. The alternative is a client whose list
// comes back empty because one field of it was a word this version of the server had not met, which is
// a worse failure than a list with too much in it — and the MSC adds filter fields without asking
// servers to catch up first.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// filterSet is what this answer's filters need to know, gathered once per request.
//
// Every field is optional and read on demand: a request with no filters costs this package nothing
// extra, which matters because that is the common request — an app opening onto its room list asks for
// rooms, not for a query.
type filterSet struct {
	// dmRooms is the set of rooms the caller's m.direct names, in none.
	dmRooms map[string]bool
	// tagsByRoom maps a room to the tags it carries in its m.tag account data.
	tagsByRoom map[string]map[string]bool
	// typesByRoom maps a room to its create event's content.type; a room with no type is absent.
	typesByRoom map[string]string
	// encryptedRooms is the set of rooms with an m.room.encryption event.
	encryptedRooms map[string]bool
}

// wants reports which of the reads this request's lists need, so that one filter does not cost the
// account data, the create events and the state of every space.
type wantsFlags struct {
	dm, tags, types, encrypted bool
}

// filtersNeeded reads a request's lists once and says what has to be looked up.
func filtersNeeded(lists map[string]*ListConfig) wantsFlags {
	var need wantsFlags
	for _, list := range lists {
		if list == nil || list.Filters == nil {
			continue
		}
		filter := list.Filters
		if filter.IsDM != nil {
			need.dm = true
		}
		if len(filter.Tags) > 0 || len(filter.NotTags) > 0 {
			need.tags = true
		}
		if len(filter.RoomTypes) > 0 || len(filter.NotRoomTypes) > 0 {
			need.types = true
		}
		if filter.IsEncrypted != nil {
			need.encrypted = true
		}
	}
	return need
}

// gather reads what the filters need about these rooms.
func (s *Service) gather(ctx context.Context, localpart string, rooms []candidate, need wantsFlags) (*filterSet, error) {
	set := &filterSet{
		dmRooms:        map[string]bool{},
		tagsByRoom:     map[string]map[string]bool{},
		typesByRoom:    map[string]string{},
		encryptedRooms: map[string]bool{},
	}
	if !need.dm && !need.tags && !need.types && !need.encrypted {
		return set, nil
	}
	roomIDs := make([]string, 0, len(rooms))
	for _, room := range rooms {
		roomIDs = append(roomIDs, room.roomID)
	}

	if need.dm {
		if err := s.readDirectRooms(ctx, localpart, set); err != nil {
			return nil, err
		}
	}
	if need.tags {
		if err := s.readRoomTags(ctx, localpart, roomIDs, set); err != nil {
			return nil, err
		}
	}
	if need.types || need.encrypted {
		if err := s.readRoomOrigins(ctx, roomIDs, set, need.types, need.encrypted); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// spaceChildren reads which rooms the given spaces name as children.
//
// A space the caller is not joined to is skipped, including one this server has never heard of: the MSC
// says unknown and non-member spaces are ignored, and the rule is load-bearing — following a space the
// caller is not in would let anybody ask a homeserver what is inside somebody else's space by naming it
// in a filter. The server does not walk into subspaces either; it is the client's job to name the
// spaces it wants, which is also what keeps one list request from becoming a crawl.
func (s *Service) spaceChildren(ctx context.Context, localpart string, spaces []string) (map[string]bool, error) {
	if len(spaces) == 0 {
		return nil, nil
	}
	children := map[string]bool{}
	for _, spaceID := range spaces {
		membership, err := s.store.Memberships.Get(ctx, spaceID, localpart)
		if err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if membership.Membership != matrix.MembershipJoin {
			continue
		}
		entries, err := s.store.State.All(ctx, spaceID)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Type == "m.space.child" && entry.StateKey != "" {
				children[entry.StateKey] = true
			}
		}
	}
	return children, nil
}

// readDirectRooms reads the caller's m.direct, which is the only list of direct rooms there is.
func (s *Service) readDirectRooms(ctx context.Context, localpart string, set *filterSet) error {
	entry, err := s.store.AccountData.Get(ctx, localpart, "", matrix.EventTypeDirect)
	if err != nil {
		if store.IsNotFound(err) {
			// No m.direct is not an error: it is an account whose client has never marked anybody as
			// a contact, and the honest answer is that no room is a direct room.
			return nil
		}
		return err
	}
	var direct map[string][]string
	if err := json.Unmarshal([]byte(entry.Content), &direct); err != nil {
		// A malformed m.direct is the client's own data; a filter that cannot read it says "no room
		// matches", which sends the client looking at its account data rather than at a room list
		// full of things it excluded by accident.
		return nil
	}
	for _, rooms := range direct {
		for _, roomID := range rooms {
			set.dmRooms[roomID] = true
		}
	}
	return nil
}

// readRoomTags reads the rooms' m.tag account data in one query.
func (s *Service) readRoomTags(ctx context.Context, localpart string, roomIDs []string, set *filterSet) error {
	entries, err := s.store.AccountData.ListByRooms(ctx, localpart, roomIDs, 0)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type != "m.tag" {
			continue
		}
		var document struct {
			Tags map[string]json.RawMessage `json:"tags"`
		}
		if err := json.Unmarshal([]byte(entry.Content), &document); err != nil {
			continue
		}
		tags := make(map[string]bool, len(document.Tags))
		for tag := range document.Tags {
			tags[tag] = true
		}
		set.tagsByRoom[entry.RoomID] = tags
	}
	return nil
}

// readRoomOrigins reads the create and encryption state of the candidate rooms in one pass over the
// event table.
//
// The create event is read rather than cached on the room row because the answer has to be right for
// rooms this server created before any such column existed, and because a cached copy of state that can
// disagree with the state is a second source of truth nobody can diff.
func (s *Service) readRoomOrigins(ctx context.Context, roomIDs []string, set *filterSet, types, encrypted bool) error {
	if encrypted {
		// The encryption state is on the room row already — the room server caches it for exactly
		// this kind of list read — so the cheap source is used for it.
		rooms, err := s.store.Rooms.GetMany(ctx, roomIDs)
		if err != nil {
			return err
		}
		for roomID, room := range rooms {
			if room.Encrypted {
				set.encryptedRooms[roomID] = true
			}
		}
	}
	if !types {
		return nil
	}
	rows, err := s.store.Events.Range(ctx, store.EventQuery{
		RoomIDs: roomIDs,
		Types:   []string{matrix.EventTypeCreate},
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		var document struct {
			Content struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(row.JSON), &document); err != nil {
			continue
		}
		if document.Content.Type != "" {
			set.typesByRoom[row.RoomID] = document.Content.Type
		}
	}
	return nil
}

// matches reports whether a room passes one list's filters.
func (set *filterSet) matches(filter *RoomFilters, room candidate) bool {
	if filter == nil {
		return true
	}
	if filter.IsDM != nil && set.dmRooms[room.roomID] != *filter.IsDM {
		return false
	}
	if filter.IsEncrypted != nil && set.encryptedRooms[room.roomID] != *filter.IsEncrypted {
		return false
	}
	if filter.IsInvited != nil && (room.membership.Membership == matrix.MembershipInvite) != *filter.IsInvited {
		return false
	}
	if !matchesTypes(filter, set.typesByRoom[room.roomID], room.roomID) {
		return false
	}
	return matchesTags(filter, set.tagsByRoom[room.roomID])
}

// matchesTypes applies the room-type pair. `null` in either list stands for "a room with no type",
// which is how a client asks for the rooms that are not spaces; and a type named in both lists is
// excluded, because `not_room_types` is the one that wins.
func matchesTypes(filter *RoomFilters, roomType, roomID string) bool {
	if len(filter.RoomTypes) == 0 && len(filter.NotRoomTypes) == 0 {
		return true
	}
	for _, excluded := range filter.NotRoomTypes {
		if typeEquals(excluded, roomType) {
			return false
		}
	}
	if len(filter.RoomTypes) == 0 {
		return true
	}
	for _, wanted := range filter.RoomTypes {
		if typeEquals(wanted, roomType) {
			return true
		}
	}
	return false
}

// typeEquals compares one filter element against a room's type, where a JSON null and an absent type
// are the same thing: a room that is not a space or some other named kind.
func typeEquals(element *string, roomType string) bool {
	if element == nil {
		return roomType == ""
	}
	return *element == roomType
}

// matchesTags applies the tag pair, with `not_tags` winning as the MSC says — which is the only
// ordering that lets a client draw a favourites list and a general list that do not repeat rooms.
func matchesTags(filter *RoomFilters, tags map[string]bool) bool {
	if len(filter.Tags) == 0 && len(filter.NotTags) == 0 {
		return true
	}
	for _, excluded := range filter.NotTags {
		if tags[excluded] {
			return false
		}
	}
	if len(filter.Tags) == 0 {
		return true
	}
	for _, wanted := range filter.Tags {
		if tags[wanted] {
			return true
		}
	}
	return false
}

// ErrFilterUnsupported documents what this package does with a filter field it does not know: it
// ignores it, and this is the reason recorded where somebody might be tempted to change it.
var ErrFilterUnsupported = fmt.Errorf("slidingsync: unknown filter")
