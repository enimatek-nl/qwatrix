package roomserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// The room hierarchy is the space tree, walked so that a client can show a space without
// joining everything beneath it.
//
// The tree is not stored as a tree. A space is an ordinary room whose create event carries
// `type: m.space`, and its children are `m.space.child` state events naming other rooms. So
// the hierarchy is computed from state on every request, which is also why it is capped: the
// walk is cheap for the hundred-room space an office keeps and unbounded for a graph with a
// cycle in it, and cycles are permitted by the specification ("there could be loops in the
// returned child events").
const (
	// defaultHierarchyLimit is the page size a caller gets for asking for no page size.
	defaultHierarchyLimit = 50
	// maxHierarchyLimit caps a page, so `?limit=1000000` cannot turn one request into a
	// whole-tree export.
	maxHierarchyLimit = 500
	// maxHierarchyDepth bounds the walk itself, independently of the page size: a space
	// whose children point at each other has no depth to speak of.
	maxHierarchyDepth = 50
	// maxHierarchyRooms bounds the rooms the walk will visit before giving up and
	// returning what it has with a token, which is the difference between a slow request
	// and one that never finishes.
	maxHierarchyRooms = 5000
)

// HierarchyChild is one `m.space.child` event in the stripped form the hierarchy returns.
//
// It is the whole event content rather than a summary because the client needs the link's own
// metadata: `via` to reach the room, `suggested` to know whether the space's administrator
// recommends it, `order` to sort it, and `origin_server_ts` because the specification's
// ordering rules are defined against the timestamp of the link rather than of the room.
type HierarchyChild struct {
	Type           string          `json:"type"`
	StateKey       string          `json:"state_key"`
	Content        json.RawMessage `json:"content"`
	Sender         string          `json:"sender"`
	OriginServerTS int64           `json:"origin_server_ts"`
}

// HierarchyRoom is one room in the hierarchy: the same fields a public-rooms chunk has, plus
// the room's type and the child links it was walked by.
type HierarchyRoom struct {
	RoomID         string           `json:"room_id"`
	Name           string           `json:"name,omitempty"`
	Topic          string           `json:"topic,omitempty"`
	AvatarURL      string           `json:"avatar_url,omitempty"`
	CanonicalAlias string           `json:"canonical_alias,omitempty"`
	Aliases        []string         `json:"aliases,omitempty"`
	WorldReadable  bool             `json:"world_readable"`
	GuestCanJoin   bool             `json:"guest_can_join"`
	NumJoined      int64            `json:"num_joined_members"`
	JoinRule       string           `json:"join_rule,omitempty"`
	RoomVersion    string           `json:"room_version,omitempty"`
	RoomType       string           `json:"room_type,omitempty"`
	AllowedRoomIDs []string         `json:"allowed_room_ids,omitempty"`
	ChildrenState  []HierarchyChild `json:"children_state"`
}

// HierarchyPage is one page of the walk. An empty NextBatch means the tree is finished.
type HierarchyPage struct {
	Rooms     []HierarchyRoom `json:"rooms"`
	NextBatch string          `json:"next_batch,omitempty"`
}

// Hierarchy walks the space tree below rootRoomID in depth-first order and returns one page
// of it.
//
// Two properties are worth stating because they are what makes the answer trustworthy:
//
// A room the caller may not read is left out rather than refused. The walk starts at a room
// the caller can see; below that, a space may contain a private conversation, and answering
// 403 for it would report the existence and the membership rules of a room the caller was not
// allowed to know about. The tree simply does not go there.
//
// A child link that names no server to ask is discarded. `via` is how anybody reaches a room
// that this server does not hold, so a link without it is a broken pointer, and following it
// would mean guessing at a home server from a room ID — which is exactly the sort of guess a
// federation client must not make.
//
// The pagination token is a position in a walk that is recomputed on each request, and it
// carries the depth and suggested-only settings that produced it. Those settings cannot be
// changed mid-walk, because the caller would be counting rooms in one tree while reading
// another and would see rooms twice or never with no way to tell.
func (s *Service) Hierarchy(ctx context.Context, rootRoomID string, opts RenderOptions,
	limit, maxDepth int, suggestedOnly bool, from string) (*HierarchyPage, error) {

	// The root is the one room whose unreadability is an error rather than a gap, and it is
	// refused the same way whether it is forbidden or absent: `404` would confirm that a
	// given room ID exists, which turns this endpoint into a way of asking.
	if err := s.checkReadable(ctx, rootRoomID, opts); err != nil {
		return nil, forbiddenHierarchyRoom(rootRoomID, err)
	}

	// The settings are settled *before* the token is read, because the token records them in
	// this same settled form. Comparing a caller's "I did not say" against a token's "the
	// default was 50" would refuse the second page of every walk that used a default.
	if limit <= 0 {
		limit = defaultHierarchyLimit
	}
	if limit > maxHierarchyLimit {
		limit = maxHierarchyLimit
	}
	if maxDepth < 0 {
		maxDepth = maxHierarchyDepth
	}
	if maxDepth > maxHierarchyDepth {
		maxDepth = maxHierarchyDepth
	}

	offset := 0
	if from != "" {
		decoded, err := decodeHierarchyToken(from)
		if err != nil {
			return nil, err
		}
		if decoded.maxDepth != maxDepth || decoded.suggestedOnly != suggestedOnly {
			return nil, matrix.NewInvalidParam(
				"the pagination token was issued for a different walk of this space: max_depth and " +
					"suggested_only must stay the same across a page of the hierarchy")
		}
		offset = decoded.offset
	}

	walked, err := s.walkHierarchy(ctx, rootRoomID, maxDepth, suggestedOnly, opts)
	if err != nil {
		return nil, err
	}
	if offset > len(walked) {
		// The tree shrank since the last page — a room was forgotten or a link removed —
		// and there is nothing honest to return but the end of the walk.
		return &HierarchyPage{Rooms: []HierarchyRoom{}}, nil
	}
	page := &HierarchyPage{}
	end := offset + limit
	if end >= len(walked) {
		page.Rooms = walked[offset:]
	} else {
		page.Rooms = walked[offset:end]
		if next := encodeHierarchyToken(hierarchyToken{end, maxDepth, suggestedOnly}); next != "" {
			page.NextBatch = next
		}
	}
	if page.Rooms == nil {
		page.Rooms = []HierarchyRoom{}
	}
	return page, nil
}

// forbiddenHierarchyRoom reports a root room the caller cannot walk. The specification asks
// for M_FORBIDDEN in both directions — unreadable and nonexistent — so that the difference
// cannot be probed.
func forbiddenHierarchyRoom(roomID string, err error) error {
	if apiErr, ok := err.(*matrix.APIError); ok {
		switch apiErr.Code {
		case matrix.ErrNotFound:
			// Reported as a refusal rather than a 404 for the reason above.
			return matrix.NewForbidden("Room %q does not exist on this homeserver, or may not be walked", roomID)
		case matrix.ErrForbidden, matrix.ErrMissingToken, matrix.ErrUnknownToken:
			return apiErr
		}
	}
	return matrix.NewForbidden("Room %q may not be walked", roomID)
}

// walkHierarchy visits the tree below root and returns every readable room in it, depth-first,
// stopping at maxDepth and at maxHierarchyRooms.
func (s *Service) walkHierarchy(ctx context.Context, root string, maxDepth int,
	suggestedOnly bool, opts RenderOptions) ([]HierarchyRoom, error) {

	type frame struct {
		roomID string
		depth  int
	}
	// Depth-first, and the children are pushed in reverse so that the first child of a
	// space is the first room out. The specification asks for depth-first traversal so that
	// a page cut in the middle still shows one branch of the tree rather than a shallow
	// survey of all of it.
	remaining := []frame{{roomID: root, depth: 0}}
	visited := map[string]bool{}
	out := []HierarchyRoom{}

	for len(remaining) > 0 && len(out) < maxHierarchyRooms {
		current := remaining[len(remaining)-1]
		remaining = remaining[:len(remaining)-1]
		if visited[current.roomID] {
			// Loops are allowed in a space graph, and this is the only thing standing
			// between a client's request and a server walking the same three rooms until
			// the process runs out of memory.
			continue
		}
		visited[current.roomID] = true

		room, children, err := s.hierarchyRoomAt(ctx, current.roomID, current.depth, maxDepth,
			suggestedOnly, opts)
		if err != nil {
			// Below the root, a room this caller may not read is not an error: it is a
			// branch that is not there. Anything else is a real failure and is returned.
			if isForbidden(err) {
				continue
			}
			return nil, err
		}
		out = append(out, *room)

		// Only a space has children to walk, and the deepest room returned is not expanded:
		// the specification says the deepest depth does not include children events, which is
		// how a caller asking for one level gets one level and not a hint of what is below.
		if room.RoomType != matrix.RoomTypeSpace {
			continue
		}
		for i := len(children) - 1; i >= 0; i-- {
			remaining = append(remaining, frame{roomID: children[i], depth: current.depth + 1})
		}
	}
	return out, nil
}

// hierarchyRoomAt describes one room for the hierarchy, along with the child rooms it links to.
func (s *Service) hierarchyRoomAt(ctx context.Context, roomID string, depth, maxDepth int,
	suggestedOnly bool, opts RenderOptions) (*HierarchyRoom, []string, error) {

	// Permission first, before any of the room's details are read: a summary of a room the
	// caller cannot see is a way of reading its name, its member count and its join rules.
	if err := s.checkReadable(ctx, roomID, opts); err != nil {
		return nil, nil, err
	}
	room, err := s.store.Rooms.Get(ctx, roomID)
	if err != nil {
		if store.IsNotFound(err) {
			// A child link can name a room this server has forgotten. It is not the
			// caller's problem, and it is not a reason to fail the whole walk.
			return nil, nil, matrix.NewForbidden("Room %q is not known to this homeserver", roomID)
		}
		return nil, nil, err
	}

	entry := &HierarchyRoom{
		RoomID:         room.ID,
		Name:           room.Name,
		Topic:          room.Topic,
		AvatarURL:      room.AvatarURL,
		CanonicalAlias: room.CanonicalAlias,
		RoomType:       room.RoomType,
		RoomVersion:    room.Version,
		JoinRule:       room.JoinRule,
		NumJoined:      room.JoinedCount,
		WorldReadable:  room.HistoryVisibility == matrix.HistoryWorldReadable,
		GuestCanJoin:   room.GuestAccess == matrix.GuestCanJoin,
		// Always non-nil: the field is a list in the response even when the space is empty,
		// and `null` would make every client check for two kinds of nothing.
		ChildrenState: []HierarchyChild{},
	}
	if aliases, err := s.AliasesOfRoom(ctx, roomID); err == nil && len(aliases) > 0 {
		entry.Aliases = aliases
	}
	if room.JoinRule == matrix.JoinRuleRestricted || room.JoinRule == matrix.JoinRuleKnockRestricted {
		if allowed, err := s.allowedRoomIDs(ctx, roomID, opts); err == nil {
			entry.AllowedRoomIDs = allowed
		}
	}

	entries, err := s.store.State.All(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	type link struct {
		child    HierarchyChild
		roomID   string
		orderKey string
	}
	var links []link
	for _, stateEntry := range entries {
		if stateEntry.Type != matrix.EventTypeSpaceChild {
			continue
		}
		child, ok := s.spaceChildLink(ctx, stateEntry)
		if !ok {
			continue
		}
		// `via` is what makes a child reachable, and `suggested` is what the space's
		// administrator says about it. Both are read out of the link rather than the room
		// because a room can be suggested in one space and hidden in another.
		if !spaceChildIsReachable(child.Content) {
			continue
		}
		if suggestedOnly && !spaceChildIsSuggested(child.Content) {
			continue
		}
		links = append(links, link{
			child:    child,
			roomID:   child.StateKey,
			orderKey: spaceChildOrderKey(child.Content),
		})
	}
	// Sorted by the space's own `order`, then by the link's age, then by room ID so that two
	// spaces written at the same millisecond still produce the same page twice.
	sort.SliceStable(links, func(i, j int) bool {
		if links[i].orderKey != links[j].orderKey {
			return links[i].orderKey < links[j].orderKey
		}
		if links[i].child.OriginServerTS != links[j].child.OriginServerTS {
			return links[i].child.OriginServerTS < links[j].child.OriginServerTS
		}
		return links[i].roomID < links[j].roomID
	})

	children := make([]string, 0, len(links))
	expanded := depth < maxDepth
	for _, link := range links {
		if expanded {
			entry.ChildrenState = append(entry.ChildrenState, link.child)
			children = append(children, link.roomID)
		}
	}
	return entry, children, nil
}

// spaceChildLink reads one `m.space.child` state entry into the stripped event the hierarchy
// returns. It reports false for an entry whose event cannot be read, which is a state row
// pointing at an event this server no longer holds.
func (s *Service) spaceChildLink(ctx context.Context, stateEntry store.RoomState) (HierarchyChild, bool) {
	row, err := s.store.Events.ByEventID(ctx, stateEntry.EventID)
	if err != nil {
		slog.Debug("roomserver: a space child link names an event that cannot be read",
			"room_id", stateEntry.RoomID, "state_key", stateEntry.StateKey, "error", err)
		return HierarchyChild{}, false
	}
	content := stateEventContent(row.JSON)
	return HierarchyChild{
		Type:           row.Type,
		StateKey:       stateEntry.StateKey,
		Content:        content,
		Sender:         row.Sender,
		OriginServerTS: row.OriginServerTS,
	}, true
}

// stateEventContent pulls the `content` object out of a stored event.
func stateEventContent(raw string) json.RawMessage {
	var event struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &event); err != nil || len(event.Content) == 0 {
		return json.RawMessage("{}")
	}
	return event.Content
}

// spaceChildIsReachable reports whether a child link names at least one server to ask.
func spaceChildIsReachable(content json.RawMessage) bool {
	var parsed struct {
		Via []string `json:"via"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		return false
	}
	return len(parsed.Via) > 0
}

// spaceChildIsSuggested reports whether the space's administrator recommends this child. An
// absent `suggested` is not suggested: the flag exists so that a space can hold rooms without
// putting them in front of everybody.
func spaceChildIsSuggested(content json.RawMessage) bool {
	var parsed struct {
		Suggested *bool `json:"suggested"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil || parsed.Suggested == nil {
		return false
	}
	return *parsed.Suggested
}

// spaceChildOrderKey is the sort key from a child link's `order`, which the specification
// defines as a space-separated list of Unicode code points to compare by.
//
// Only the field's practical use is implemented: the first token, as the space's author wrote
// it, which is what "put this one first" means in practice. Interpreting the escape syntax
// fully would let a malformed order turn into a panic in a client that has to do the same
// arithmetic to sort the same list, and this is a display order rather than a security
// boundary.
func spaceChildOrderKey(content json.RawMessage) string {
	var parsed struct {
		Order string `json:"order"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		return ""
	}
	order := strings.TrimSpace(parsed.Order)
	if order == "" {
		return ""
	}
	first, _, _ := strings.Cut(order, " ")
	// A space that ordered its children puts the empty order last, which is what a list of
	// "these ones first, then everything else" needs.
	return "\xff" + first
}

// hierarchyToken is a position in a walk. It carries the settings that shaped the walk so a
// caller cannot change them mid-page and be handed a mixture of two trees.
type hierarchyToken struct {
	offset        int
	maxDepth      int
	suggestedOnly bool
}

const hierarchyTokenVersion = "h1"

func encodeHierarchyToken(token hierarchyToken) string {
	raw := fmt.Sprintf("%s|%d|%d|%t", hierarchyTokenVersion, token.offset, token.maxDepth, token.suggestedOnly)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeHierarchyToken(from string) (hierarchyToken, error) {
	raw, err := base64.RawURLEncoding.DecodeString(from)
	if err != nil {
		return hierarchyToken{}, matrix.NewInvalidParam("that hierarchy pagination token is not one this server issued")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 || parts[0] != hierarchyTokenVersion {
		return hierarchyToken{}, matrix.NewInvalidParam("that hierarchy pagination token is not one this server issued")
	}
	offset, err := strconv.Atoi(parts[1])
	if err != nil || offset < 0 {
		return hierarchyToken{}, matrix.NewInvalidParam("that hierarchy pagination token is not one this server issued")
	}
	depth, err := strconv.Atoi(parts[2])
	if err != nil {
		return hierarchyToken{}, matrix.NewInvalidParam("that hierarchy pagination token is not one this server issued")
	}
	suggested, err := strconv.ParseBool(parts[3])
	if err != nil {
		return hierarchyToken{}, matrix.NewInvalidParam("that hierarchy pagination token is not one this server issued")
	}
	return hierarchyToken{offset: offset, maxDepth: depth, suggestedOnly: suggested}, nil
}

// isForbidden reports whether a failure is the "not allowed to see this" kind, which the
// hierarchy treats as a branch that is not there.
func isForbidden(err error) bool {
	apiErr, ok := err.(*matrix.APIError)
	if !ok {
		return false
	}
	switch apiErr.Code {
	case matrix.ErrForbidden, matrix.ErrNotFound, matrix.ErrMissingToken, matrix.ErrUnknownToken:
		return true
	}
	return false
}
