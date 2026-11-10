// Package readstate holds what each reader has read and who is typing.
//
// None of this is room history. A receipt says "this account had read up to here", a fully-read
// marker says "and everything before here is finished", and a typing indicator says "somebody is
// composing right now, probably". All three are per-reader, all three must reach a reader's other
// devices, and none of them may reach anybody else — which is why they cannot be room events,
// even though they travel over the same `/sync` stream.
package readstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Service coordinates receipts, read markers, typing indicators, and account data.
type Service struct {
	store   *store.Store
	cfg     *config.Config
	typing  *store.TypingTracker
	changes *notifier.Notifier
	// classifier decides which events are worth a notification. It is injected rather than
	// imported because what counts as news is a push-rule question, and this package's job is
	// knowing how far the reader has got.
	classifier BadgeClassifier
}

// New builds the service. changes is the same notifier the /sync long-pollers wait on, so a
// receipt or a typing indicator wakes the readers who are waiting for it instead of waiting for a
// timeout to expire.
func New(st *store.Store, cfg *config.Config, changes *notifier.Notifier) *Service {
	s := &Service{store: st, cfg: cfg, changes: changes}
	// A typing change is not an event, so it borrows a position from the end of the event
	// stream. That keeps one token ordering a client's history and its typing indicators, which
	// is what lets an incremental sync say "the typing list moved after where you are".
	s.typing = store.NewTypingTracker(func() { s.changes.Publish() }, func() int64 {
		position, err := st.Events.MaxPosition(context.Background())
		if err != nil {
			return 0
		}
		return position + 1
	})
	return s
}

// ReceiptType is a receipt's kind. The spec defines m.read; threaded receipts reuse it with a
// thread ID.
type ReceiptType string

const (
	ReceiptRead        ReceiptType = "m.read"
	ReceiptReadPrivate ReceiptType = "m.read.private"
	// FullyRead is the "finished reading up to here" marker. It is account data, not a receipt,
	// because it is only ever for the reader's own benefit and must never be shared.
	FullyRead = "m.fully_read"
)

// ValidReceiptType reports whether a receipt type is one this server understands.
func ValidReceiptType(value string) bool {
	switch ReceiptType(value) {
	case ReceiptRead, ReceiptReadPrivate:
		return true
	}
	return false
}

// MemberLocalparts lists the local users joined to a room, which is who an ephemeral event may
// reach.
func (s *Service) MemberLocalparts(ctx context.Context, roomID string) ([]string, error) {
	members, err := s.store.Memberships.List(ctx, roomID, []string{store.MembershipJoin}, nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(members))
	for _, member := range members {
		names = append(names, member.Localpart)
	}
	return names, nil
}

// SetTyping records that a user started or stopped typing.
//
// Only a joined member may type in a room: an ephemeral event is not an event and cannot be
// authorised after the fact, so the membership check has to happen here, or a stranger could put
// their name in a room's typing list and every member's client would show them composing.
func (s *Service) SetTyping(ctx context.Context, roomID, localpart string, typing bool, timeoutMS int64) error {
	if roomID == "" {
		return matrix.NewInvalidParam("room ID is required")
	}
	if _, err := s.store.Rooms.Get(ctx, roomID); err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", roomID)
		}
		return err
	}
	member, err := s.store.Memberships.Get(ctx, roomID, localpart)
	if err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
				"You must join this room before typing in it")
		}
		return err
	}
	if member.Membership != store.MembershipJoin {
		return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
			"You must join this room before typing in it")
	}
	s.typing.Set(roomID, localpart, typing, timeoutMS)
	return nil
}

// TypingUserIDs lists who is typing in a room, as full user IDs.
func (s *Service) TypingUserIDs(roomID string) []string {
	names := s.typing.Typists(roomID)
	userIDs := make([]string, 0, len(names))
	for _, name := range names {
		userIDs = append(userIDs, matrix.UserID(name, s.cfg.ServerName))
	}
	return userIDs
}

// TypingEvent renders a room's typing indicator as the ephemeral event /sync delivers, and
// reports whether there is one to deliver: only when the list moved after the reader's token.
//
// The event deliberately has no room_id: the room is the section of the sync response it appears
// in, so repeating it would give one fact two spellings for a client to reconcile — and the spec
// says the field is for federation only, where the event travels on its own.
func (s *Service) TypingEvent(roomID string, since int64) (json.RawMessage, bool, error) {
	if !s.typing.ChangedAfter(roomID, since) {
		return nil, false, nil
	}
	userIDs := s.TypingUserIDs(roomID)
	if userIDs == nil {
		// An empty list is still an event. It is how a client learns that typing *stopped*,
		// which silence cannot say: no event and "nobody is typing" look identical to a sync
		// that has not caught up yet.
		userIDs = []string{}
	}
	event, err := wrapEphemeral("m.typing", map[string]any{"user_ids": userIDs})
	return event, err == nil, err
}

// AddReceipt records a read receipt for an event.
func (s *Service) AddReceipt(ctx context.Context, roomID, localpart, receiptType, eventID, threadID string) error {
	if !ValidReceiptType(receiptType) {
		return matrix.NewInvalidParam("%q is not a supported receipt type", receiptType)
	}
	if eventID == "" {
		return matrix.NewInvalidParam("event ID is required")
	}
	if _, err := s.store.Rooms.Get(ctx, roomID); err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", roomID)
		}
		return err
	}
	// The receipt must name an event in this room. Without this check a typo would be stored as
	// the reader's position, and clients reading that position would be told they had read
	// something that is not in the room at all.
	row, err := s.store.Events.ByEventID(ctx, eventID)
	if err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Event %q does not exist", eventID)
		}
		return err
	}
	if row.RoomID != roomID {
		return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"Event %q does not exist in this room", eventID)
	}
	member, err := s.store.Memberships.Get(ctx, roomID, localpart)
	if err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
				"You must join this room before leaving receipts in it")
		}
		return err
	}
	if member.Membership != store.MembershipJoin {
		return matrix.NewAPIError(http.StatusForbidden, matrix.ErrForbidden,
			"You must join this room before leaving receipts in it")
	}

	_, err = s.store.Receipts.Put(ctx, &store.Receipt{
		RoomID:      roomID,
		Localpart:   localpart,
		ReceiptType: receiptType,
		ThreadID:    threadID,
		EventID:     eventID,
	})
	if err != nil {
		return err
	}
	s.changes.Publish()
	return nil
}

// ReceiptEvents renders a room's receipts as the ephemeral events /sync delivers, one per
// receipt. The content is keyed by event ID and then by receipt type, which is how a client
// finding its own reads in a busy room avoids scanning every receipt in it.
func (s *Service) ReceiptEvents(ctx context.Context, roomID string, after int64) ([]json.RawMessage, error) {
	receipts, err := s.store.Receipts.ListByRoom(ctx, roomID, after)
	if err != nil {
		return nil, err
	}
	events := make([]json.RawMessage, 0, len(receipts))
	for _, receipt := range receipts {
		position := map[string]any{"ts": receipt.TS}
		// `thread_id` is present only for a receipt that is about a thread. An unthreaded
		// receipt is a different fact — "I have read up to here, overall" — and a client that
		// sees an empty thread ID cannot tell that apart from a thread it has never heard of,
		// which is how a read marker ends up applied to the wrong conversation.
		if receipt.ThreadID != "" {
			position["thread_id"] = receipt.ThreadID
		}
		content := map[string]any{
			receipt.EventID: map[string]any{
				receipt.ReceiptType: map[string]any{
					matrix.UserID(receipt.Localpart, s.cfg.ServerName): position,
				},
			},
		}
		event, err := wrapEphemeral("m.receipt", content)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// ReadMarkersRequest is a `POST /rooms/{roomID}/read_markers` body.
type ReadMarkersRequest struct {
	// FullyRead is what the reader has finished with; it is private to them.
	FullyRead string `json:"m.fully_read"`
	// Read is the public read receipt the marker also moves, because clients send both together
	// and a marker that drifted from the receipt would show two read positions in one room.
	Read string `json:"m.read"`
	// ThreadID scopes the receipt this marker moves to one thread.
	ThreadID string `json:"thread_id"`
}

// SetReadMarkers moves a reader's position in a room.
//
// m.fully_read is stored as room account data rather than as a receipt because it is the reader's
// own bookmark: unlike a read receipt it is never shown to anybody else, and the spec keeps it in
// account data for exactly that reason.
func (s *Service) SetReadMarkers(ctx context.Context, roomID, localpart string, req ReadMarkersRequest) error {
	if req.FullyRead == "" && req.Read == "" {
		return matrix.NewMissingParam("m.fully_read or m.read")
	}
	if req.Read != "" {
		if err := s.AddReceipt(ctx, roomID, localpart, string(ReceiptRead), req.Read, req.ThreadID); err != nil {
			return err
		}
	}
	if req.FullyRead == "" {
		return nil
	}
	row, err := s.store.Events.ByEventID(ctx, req.FullyRead)
	if err != nil {
		if store.IsNotFound(err) {
			return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Event %q does not exist", req.FullyRead)
		}
		return err
	}
	if row.RoomID != roomID {
		return matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"Event %q does not exist in this room", req.FullyRead)
	}
	content, err := json.Marshal(map[string]any{"event_id": req.FullyRead})
	if err != nil {
		return err
	}
	return s.PutRoomAccountData(ctx, localpart, roomID, FullyRead, content)
}

// PutRoomAccountData stores one of a reader's per-room keys.
func (s *Service) PutRoomAccountData(ctx context.Context, localpart, roomID, dataType string, content json.RawMessage) error {
	if !json.Valid(content) {
		return matrix.NewBadJSON("account data must be a JSON value")
	}
	// An overwrite that failed half-way would leave the client's view of the room wrong with
	// nothing to show it, so the write and the stream position happen in one transaction.
	err := s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		return s.store.AccountData.Put(ctx, &store.AccountData{
			Localpart: localpart,
			RoomID:    roomID,
			Type:      dataType,
			Content:   string(content),
		})
	})
	if err != nil {
		return err
	}
	s.changes.Publish()
	return nil
}

// PutAccountData stores one of a reader's global keys.
func (s *Service) PutAccountData(ctx context.Context, localpart, dataType string, content json.RawMessage) error {
	if !json.Valid(content) {
		return matrix.NewBadJSON("account data must be a JSON value")
	}
	err := s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		return s.store.AccountData.Put(ctx, &store.AccountData{
			Localpart: localpart,
			Type:      dataType,
			Content:   string(content),
		})
	})
	if err != nil {
		return err
	}
	s.changes.Publish()
	return nil
}

// RoomAccountData lists a reader's per-room keys, optionally only those that changed after a
// sync token.
func (s *Service) RoomAccountData(ctx context.Context, localpart string, roomIDs []string, after int64) (map[string][]json.RawMessage, error) {
	rows, err := s.store.AccountData.ListByRooms(ctx, localpart, roomIDs, after)
	if err != nil {
		return nil, err
	}
	byRoom := map[string][]json.RawMessage{}
	for _, row := range rows {
		event, err := wrapEphemeral(row.Type, json.RawMessage(row.Content))
		if err != nil {
			return nil, err
		}
		byRoom[row.RoomID] = append(byRoom[row.RoomID], event)
	}
	return byRoom, nil
}

// AccountData lists a reader's global keys.
func (s *Service) AccountData(ctx context.Context, localpart string, after int64) ([]json.RawMessage, error) {
	rows, err := s.store.AccountData.List(ctx, localpart, "")
	if err != nil {
		return nil, err
	}
	events := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if after > 0 && row.StreamPos <= after {
			continue
		}
		event, err := wrapEphemeral(row.Type, json.RawMessage(row.Content))
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// GetAccountData reads one key, global when roomID is empty.
func (s *Service) GetAccountData(ctx context.Context, localpart, roomID, dataType string) (json.RawMessage, error) {
	row, err := s.store.AccountData.Get(ctx, localpart, roomID, dataType)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"account_data %q does not exist", dataType)
		}
		return nil, err
	}
	return json.RawMessage(row.Content), nil
}

// DeleteAccountData removes one key. Deleting something that is not there succeeds: the caller
// wanted it gone.
func (s *Service) DeleteAccountData(ctx context.Context, localpart, roomID, dataType string) error {
	err := s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		return s.store.AccountData.Delete(ctx, localpart, roomID, dataType)
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.changes.Publish()
	return nil
}

// ReceiptSummary counts unread messages per room from the reader's own receipts, which is what
// `unread_notifications` reports.
type ReceiptSummary struct {
	// NotificationCount is the badge a client shows; HighlightCount counts mentions.
	NotificationCount int64 `json:"notification_count"`
	HighlightCount    int64 `json:"highlight_count"`
}

// wrapEphemeral renders an ephemeral event: type plus content, and nothing else. Room ID, sender
// and event ID are deliberately absent — ephemeral events are not events in the room's sense,
// they have no ID to cite and no sender to attribute.
func wrapEphemeral(dataType string, content any) (json.RawMessage, error) {
	raw, err := json.Marshal(struct {
		Type    string `json:"type"`
		Content any    `json:"content"`
	}{Type: dataType, Content: content})
	if err != nil {
		return nil, fmt.Errorf("rendering ephemeral %s: %w", dataType, err)
	}
	return raw, nil
}
