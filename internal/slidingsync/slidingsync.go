// Package slidingsync implements MSC4186, the sync endpoint Element X's Rust SDK speaks and no
// other client has any reason to call.
//
// The package exists because /v3/sync answers a question this endpoint does not ask. /v3/sync says
// "what happened since this token", which costs a server an amount of work proportional to how long
// the client was away and a client a payload proportional to how many rooms it is in. MSC4186 asks
// instead "what is new in the rooms I am looking at, and what did I last see of them" — which is a
// better question and a stateful one. Everything in this file follows from holding that state
// honestly.
//
// Three positions are in play, and conflating any two of them is the way to get this wrong:
//
//   - the stream position of the answer (`pos`), which the client sends back and which is also a
//     valid token for /messages, because qwatrix tokens name a position in one global stream;
//   - the connection's own position, the one in the request, which decides what counts as *live* —
//     an event that happened after the client last asked, as opposed to history the server is only
//     now showing it;
//   - a per-room position, kept by the server, of how far this connection has been *shown* that
//     room, which decides what is a delta and what is an expansion.
//
// A room that slid into the client's window illustrates all three: it is sent whole (`initial`),
// with `num_live: 0` because none of it happened since the client asked, and with its own history
// limit rather than the connection's token deciding where it starts.
//
// State is held in the database (see internal/store/slidingsync.go) and is disposable: an
// unrecognised or expired `pos` is answered with M_UNKNOWN_POS, which the MSC defines as "start
// again", so a server that forgets costs a client a re-sync and never a wrong view of its rooms.
package slidingsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/devicelists"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/presence"
	"github.com/enimatek-nl/qwatrix/internal/readstate"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/enimatek-nl/qwatrix/internal/todevice"
)

// Bounds the server enforces on one request or one connection.
//
// Every one of these is the MSC's number where the MSC gives one: it states the list and subscription
// caps outright, and it asks servers to bound what they keep per user, because a connection is
// created by a client with a string and costs rows.
const (
	maxLists            = 100
	maxSubscriptions    = 100
	maxConnectionPerDev = 8
	// maxTrackedRooms caps what one connection may remember. Beyond it the oldest memories go, and
	// a room whose memory was dropped is simply sent again as initial — which is a bandwidth cost,
	// not a correctness one.
	maxTrackedRooms = 2000
	// connectionLifetime is how long a connection may go without being used before the server would
	// rather the client started again. It has to be long enough that a phone locked overnight comes
	// back to a delta, and short enough that a device which uninstalled the app stops costing rows.
	connectionLifetime = 30 * time.Minute
	// maxTimelineLimit caps a room config's timeline. The MSC lets a server cap it; this one picks a
	// number larger than any client in the field asks for, so that the cap only ever catches a
	// mistake rather than a use case.
	maxTimelineLimit = 100
	// defaultRange is the window a list gets when it sends no range: the MSC says "not limited by
	// range", and the server still has to stop somewhere.
	defaultRangeTo = 200
)

// ErrUnknownPos is returned for a `pos` this server did not issue to this user and device, or for a
// connection it has expired. The MSC's own error code, and its own status: 400 rather than 401,
// because the token is not an authentication failure — the client is who it says it is and simply
// has to start its sync again from the beginning.
var ErrUnknownPos = errors.New("slidingsync: unknown pos")

// Service serves the sliding-sync endpoint.
type Service struct {
	store      *store.Store
	rooms      *roomserver.Service
	presence   *presence.Service
	notifier   *notifier.Notifier
	serverName string
	// The extension sources. Each is nil-tolerant: a server built without one answers without that
	// extension rather than failing the connection, because a device that cannot be told who is typing
	// should still be told what its rooms contain.
	readState   *readstate.Service
	toDevice    *todevice.Service
	deviceLists *devicelists.Service
	// now is a seam for expiry tests; production reads the wall clock.
	now func() time.Time
}

// New builds the service.
func New(st *store.Store, rooms *roomserver.Service, pres *presence.Service, read *readstate.Service,
	toDevice *todevice.Service, deviceLists *devicelists.Service, nf *notifier.Notifier, serverName string) *Service {
	return &Service{
		store: st, rooms: rooms, presence: pres, readState: read, toDevice: toDevice,
		deviceLists: deviceLists, notifier: nf, serverName: serverName, now: time.Now,
	}
}

// Request is a parsed POST /v4/sync body, with the identity of the caller already filled in by the
// handler: a connection belongs to a user *and a device*, so both come from the session and neither
// can be influenced by the body.
type Request struct {
	Localpart string
	DeviceID  string

	ConnID        string                       `json:"conn_id,omitempty"`
	Pos           string                       `json:"pos,omitempty"`
	TimeoutMS     *int64                       `json:"timeout,omitempty"`
	SetPresence   *string                      `json:"set_presence,omitempty"`
	Lists         map[string]*ListConfig       `json:"lists,omitempty"`
	Subscriptions map[string]*RoomSubscription `json:"room_subscriptions,omitempty"`
	// Extensions are the streams that are not rooms: to-device messages, encryption bookkeeping,
	// typing, receipts and account data.
	Extensions map[string]*ExtensionConfig `json:"extensions,omitempty"`

	// MaxTimeout is the server's cap on a long-poll, set by the handler from its configuration: a
	// client may ask for an hour, and every proxy between here and it will have given up long before.
	MaxTimeout time.Duration `json:"-"`

	// timeout is the caller's long-poll budget after that cap.
	timeout time.Duration
}

// ListConfig is one client-side room list.
type ListConfig struct {
	TimelineLimit *int                  `json:"timeline_limit"`
	RequiredState *RequiredStateRequest `json:"required_state"`
	// Range is inclusive and 0-indexed. A pointer to an array, so that "absent" (all rooms) is
	// distinguishable from [0,0] (one room).
	Range   *[2]int      `json:"range,omitempty"`
	Filters *RoomFilters `json:"filters,omitempty"`
}

// RoomSubscription is the room config of an explicitly tracked room.
type RoomSubscription struct {
	TimelineLimit *int                  `json:"timeline_limit"`
	RequiredState *RequiredStateRequest `json:"required_state"`
}

// RequiredStateRequest says which state a client wants with each room.
//
// An element matches by omission: no `type` matches every type, no `state_key` matches every state
// key, and `{}` therefore matches all of a room's state — which is how a client asks for "everything
// except members" (include `{}`, lazy_members true).
type RequiredStateRequest struct {
	Include     []StatePattern `json:"include,omitempty"`
	Exclude     []StatePattern `json:"exclude,omitempty"`
	LazyMembers bool           `json:"lazy_members,omitempty"`
}

// StatePattern is one include/exclude element.
type StatePattern struct {
	Type     *string `json:"type,omitempty"`
	StateKey *string `json:"state_key,omitempty"`
}

// RoomFilters is a list's filter set. Parsed now, applied in the filter milestone.
type RoomFilters struct {
	IsDM         *bool     `json:"is_dm,omitempty"`
	IsEncrypted  *bool     `json:"is_encrypted,omitempty"`
	IsInvited    *bool     `json:"is_invited,omitempty"`
	Spaces       []string  `json:"spaces,omitempty"`
	RoomTypes    []*string `json:"room_types,omitempty"`
	NotRoomTypes []*string `json:"not_room_types,omitempty"`
	Tags         []string  `json:"tags,omitempty"`
	NotTags      []string  `json:"not_tags,omitempty"`
}

// Response is the sliding-sync body.
type Response struct {
	Pos        string                 `json:"pos"`
	Lists      map[string]*ListResult `json:"lists,omitempty"`
	Rooms      map[string]*RoomResult `json:"rooms,omitempty"`
	Extensions map[string]any         `json:"extensions,omitempty"`

	// sentPositions is not on the wire: it is what this answer taught the connection about each room
	// it described, which is what the next answer diffs against.
	sentPositions map[string]store.SlidingSyncRoom
}

// ListResult is what a list answers: how many rooms match, which is the only number a client needs
// to know when to stop widening its range. MSC3575's `ops` are deliberately absent — the MSC dropped
// them, and a client tracks list membership itself.
type ListResult struct {
	Count int64 `json:"count"`
}

// RoomResult describes one room, either whole (`Initial`) or as a delta in which an omitted field
// means "unchanged".
type RoomResult struct {
	Initial          *bool             `json:"initial,omitempty"`
	ExpandedTimeline *bool             `json:"expanded_timeline,omitempty"`
	Limited          *bool             `json:"limited,omitempty"`
	NumLive          int               `json:"num_live"`
	PrevBatch        string            `json:"prev_batch,omitempty"`
	BumpStamp        *int64            `json:"bump_stamp,omitempty"`
	Membership       string            `json:"membership,omitempty"`
	Lists            []string          `json:"lists,omitempty"`
	Name             *json.RawMessage  `json:"name,omitempty"`
	Avatar           *json.RawMessage  `json:"avatar,omitempty"`
	Heroes           []Hero            `json:"heroes,omitempty"`
	JoinedCount      *int64            `json:"joined_count,omitempty"`
	InvitedCount     *int64            `json:"invited_count,omitempty"`
	RequiredState    []json.RawMessage `json:"required_state,omitempty"`
	Timeline         []json.RawMessage `json:"timeline,omitempty"`
	StrippedState    []json.RawMessage `json:"stripped_state,omitempty"`
}

// Hero is one user to name a room after when it has no name: the MSC4186 shape, which carries the
// display name and avatar so a client can draw a room list without a profile lookup per room.
type Hero struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// ErrBadRequest is returned for a request the MSC says a server must refuse. The handler turns it
// into 400 M_INVALID_PARAM.
type ErrBadRequest struct{ Reason string }

// Error implements error.
func (e ErrBadRequest) Error() string { return "slidingsync: " + e.Reason }

// Sync performs one sliding sync, long-polling up to the request's timeout.
//
// The loop is the same shape as /v3/sync's for the same reason: read the stream position before the
// data, so that anything arriving mid-read is above the token this answer reports and shows up next
// time. What differs is what counts as "nothing to say": a sliding sync has said something if any
// room has a delta, and a room whose window widened but which has no new events still counts, because
// the client is owed history it has not seen.
func (s *Service) Sync(ctx context.Context, req *Request) (*Response, error) {
	if err := s.validate(req); err != nil {
		return nil, err
	}
	if err := s.markPresence(ctx, req); err != nil {
		return nil, err
	}
	conn, err := s.resolveConnection(ctx, req)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(req.timeout)
	for {
		generation := s.notifier.Generation()
		response, empty, err := s.build(ctx, req, conn)
		if err != nil {
			return nil, err
		}
		if !empty || req.timeout == 0 || time.Now().After(deadline) {
			return response, s.commit(ctx, conn, response, req)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return response, s.commit(ctx, conn, response, req)
		}
		if !s.notifier.WaitForChange(ctx, generation, remaining) {
			return response, s.commit(ctx, conn, response, req)
		}
	}
}

// validate enforces the request limits the MSC states as numbers, plus the ones it leaves to the
// server. A refusal here is always about a request that cannot be honoured, never about a client
// being quirky: an empty connection is legitimate, a thousand lists are not.
func (s *Service) validate(req *Request) error {
	if len(req.Lists) > maxLists {
		return ErrBadRequest{Reason: fmt.Sprintf("a request may name at most %d lists", maxLists)}
	}
	if len(req.Subscriptions) > maxSubscriptions {
		return ErrBadRequest{Reason: fmt.Sprintf("a request may name at most %d room subscriptions", maxSubscriptions)}
	}
	if req.SetPresence != nil {
		value := *req.SetPresence
		// "" is a client asking to be left alone; the MSC keeps /v3/sync's vocabulary here.
		if value != "" && !presence.Valid(value) {
			return ErrBadRequest{Reason: fmt.Sprintf("set_presence %q is not a presence", value)}
		}
	}
	for name, list := range req.Lists {
		if list == nil {
			return ErrBadRequest{Reason: fmt.Sprintf("list %q is empty", name)}
		}
		if list.Range != nil {
			from, to := (*list.Range)[0], (*list.Range)[1]
			if from < 0 || to < from {
				return ErrBadRequest{Reason: fmt.Sprintf("list %q has the range [%d,%d]", name, from, to)}
			}
		}
	}
	if req.TimeoutMS != nil && *req.TimeoutMS < 0 {
		return ErrBadRequest{Reason: "timeout must not be negative"}
	}

	// The long-poll budget. A request with no `pos` cannot wait for anything — there is no connection
	// to hold still — and `timeout` is documented as ignored in that case, which is also what a client
	// doing a sequence of window-widening requests relies on.
	req.timeout = 0
	if req.Pos != "" && req.TimeoutMS != nil {
		req.timeout = time.Duration(*req.TimeoutMS) * time.Millisecond
		if req.MaxTimeout > 0 && req.timeout > req.MaxTimeout {
			req.timeout = req.MaxTimeout
		}
	}
	return nil
}

// markPresence marks the user here. Unlike /v3/sync, where an absent `set_presence` means "leave my
// presence alone", MSC4186 says a sliding sync *does* mark the client online unless told otherwise:
// the endpoint is a long-lived connection, so the server learns about the user's presence from having
// the connection at all rather than from being told each time.
func (s *Service) markPresence(ctx context.Context, req *Request) error {
	if s.presence == nil {
		return nil
	}
	value := store.PresenceOnline
	if req.SetPresence != nil {
		value = *req.SetPresence
	}
	switch value {
	case "", store.PresenceOffline:
		return nil
	}
	if !presence.Valid(value) {
		return nil
	}
	if err := s.presence.Set(ctx, req.Localpart, value, ""); err != nil {
		return fmt.Errorf("recording presence for a sliding sync: %w", err)
	}
	return nil
}

// resolveConnection turns the request's `pos` into a connection, checking that it belongs to this
// user and device.
//
// The rules are:
//
//   - no `pos` at all is a new connection. An existing one with the same `conn_id` is reset, because
//     a client that throws its token away has decided to start over and would be wrong to be told
//     "you are already connected".
//   - a `pos` equal to the connection's current one is accepted. The MSC allows a client to cancel a
//     request and send another with the same `pos`, so replaying the newest token is legitimate; what
//     it forbids is going backwards, which would ask the server to describe rooms as new that it has
//     already described.
//   - anything else — unknown, older, newer, or issued to somebody else — is M_UNKNOWN_POS. Checking
//     ownership here rather than trusting the token is what the MSC's security section requires: a
//     `pos` is not a secret, and possession of one must not read rooms.
func (s *Service) resolveConnection(ctx context.Context, req *Request) (*store.SlidingSyncConn, error) {
	existing, err := s.store.SlidingSync.LoadConn(ctx, req.Localpart, req.DeviceID, req.ConnID)
	if err != nil && !store.IsNotFound(err) {
		return nil, err
	}
	if req.Pos == "" {
		if existing != nil {
			if err := s.store.SlidingSync.DeleteConn(ctx, req.Localpart, req.DeviceID, req.ConnID); err != nil {
				return nil, err
			}
		}
		return &store.SlidingSyncConn{Localpart: req.Localpart, DeviceID: req.DeviceID, ConnID: req.ConnID}, nil
	}
	pos, err := parsePos(req.Pos)
	if err != nil {
		return nil, ErrUnknownPos
	}
	if existing == nil || existing.Pos != pos {
		return nil, ErrUnknownPos
	}
	return existing, nil
}

// commit advances the connection to the position this answer reported and records what the answer
// sent, which is what turns the next request into a delta.
//
// Both writes happen once the answer is known, and both are allowed to fail loudly: a connection that
// did not learn what it was shown will resend rooms as initial, which is a waste of bandwidth, and a
// connection that did not move its position will hand the client a token it will then refuse.
func (s *Service) commit(ctx context.Context, conn *store.SlidingSyncConn, response *Response, req *Request) error {
	position, err := parsePos(response.Pos)
	if err != nil {
		return err
	}
	conn.Pos = position
	conn.UpdatedMS = s.now().UnixMilli()
	if err := s.store.SlidingSync.SaveConn(ctx, conn); err != nil {
		return err
	}
	if err := s.store.SlidingSync.SaveRooms(ctx, req.Localpart, req.DeviceID, req.ConnID, sentRooms(response)); err != nil {
		return err
	}
	return s.prune(ctx, req)
}

// prune keeps a device and the server's whole connection table bounded.
func (s *Service) prune(ctx context.Context, req *Request) error {
	conns, err := s.store.SlidingSync.ListConns(ctx, req.Localpart, req.DeviceID)
	if err != nil {
		return err
	}
	// The list is ordered by last use, so the ones over the cap are the ones nobody has come back
	// for. The connection answering this request is by definition the most recently used of them.
	if excess := len(conns) - maxConnectionPerDev; excess > 0 {
		for _, conn := range conns[:excess] {
			if err := s.store.SlidingSync.DeleteConn(ctx, conn.Localpart, conn.DeviceID, conn.ConnID); err != nil {
				return err
			}
		}
	}
	if _, err := s.store.SlidingSync.ExpireConns(ctx, s.now().Add(-connectionLifetime).UnixMilli()); err != nil {
		return err
	}
	tracked, err := s.store.SlidingSync.ListRooms(ctx, req.Localpart, req.DeviceID, req.ConnID)
	if err != nil {
		return err
	}
	excess := len(tracked) - maxTrackedRooms
	if excess <= 0 {
		return nil
	}
	// Dropping the oldest memories keeps the rooms a client is most likely still looking at, and
	// costs a room that gets forgotten nothing worse than being sent again in full later.
	oldest := make([]store.SlidingSyncRoom, 0, len(tracked))
	for _, row := range tracked {
		oldest = append(oldest, row)
	}
	sort.Slice(oldest, func(i, j int) bool { return oldest[i].UpdatedMS < oldest[j].UpdatedMS })
	// The store has no "drop the oldest N rooms of a connection" because the answer to "why are
	// there more than the cap" is always a client misbehaving, and a bounded number of deletes is
	// easier to reason about than a DELETE with a LIMIT that SQLite only sometimes supports.
	for _, row := range oldest[:excess] {
		if err := s.store.SlidingSync.ForgetRoom(ctx, req.Localpart, req.DeviceID, req.ConnID, row.RoomID); err != nil {
			return err
		}
	}
	return nil
}

// AnswerFor maps this package's errors onto the spec's error body, so that the handler stays a
// translation layer rather than a place where error semantics are invented. It returns nil for an
// error this package has no opinion about, which the caller reports as an internal failure.
func AnswerFor(err error) *matrix.APIError {
	var bad ErrBadRequest
	switch {
	case errors.Is(err, ErrUnknownPos):
		return matrix.NewUnknownPos()
	case errors.As(err, &bad):
		return matrix.NewInvalidParam("%s", bad.Reason)
	default:
		return nil
	}
}

// parsePos reads a pos token: a stream position with the same "s" prefix /v3/sync uses, because it
// names the same thing — which is also what lets one token work in /messages, as the MSC requires.
//
// The parse is exact rather than lenient. "s12abc" is not position 12 to this server: a client that
// sent a token this server did not issue is told so, and a server that read a prefix of it would
// answer a question the client did not ask.
func parsePos(token string) (int64, error) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return 0, errors.New("slidingsync: empty pos")
	}
	position, err := strconv.ParseInt(strings.TrimPrefix(trimmed, "s"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("slidingsync: unparsable pos %q: %w", token, err)
	}
	if position < 0 {
		return 0, fmt.Errorf("slidingsync: negative pos %q", token)
	}
	return position, nil
}

// formatPos writes a pos token.
func formatPos(position int64) string {
	if position < 0 {
		position = 0
	}
	return "s" + strconv.FormatInt(position, 10)
}

// sentRooms turns an answer's rooms into the memory of what it sent: how far each was shown, and under
// which config. Only rooms in the answer move, because a room this answer skipped is owed to the next
// one.
func sentRooms(response *Response) []store.SlidingSyncRoom {
	if response.Rooms == nil {
		return nil
	}
	// The per-room positions were computed while building the answer and are carried on the response
	// rather than recomputed here, which is why they travel in sentPositions.
	out := make([]store.SlidingSyncRoom, 0, len(response.Rooms))
	for roomID, position := range response.sentPositions {
		out = append(out, position)
		out[len(out)-1].RoomID = roomID
	}
	return out
}
