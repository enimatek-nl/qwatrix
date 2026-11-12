// Package roomserver is the authoritative owner of rooms: creating them, appending events
// to them, maintaining their state, and answering questions about them.
//
// The central invariant of this package is that an event becomes part of a room in one
// step: it is built, authorised against the state that precedes it, written, and — when it
// is a state event — becomes the room's state, all inside one store transaction. There is
// no intermediate "event received, state pending" queue, because a queue would let a reader
// see an event whose state had not landed, and every bug of that shape in this protocol
// shows up as a client rendering a room incorrectly.
//
// The second invariant is that the protocol's rules are applied by the library that
// implements them ([gomatrixserverlib.Allowed]) rather than by hand here. Authorization is
// the part of Matrix where a mistake is a security problem, and that library's rules are the
// ones the compliance suite is written against.
package roomserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/matrix-org/gomatrixserverlib"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Presets, as named by the specification's m.create_room_request.
const (
	PresetPrivateChat        = "private_chat"
	PresetPublicChat         = "public_chat"
	PresetTrustedPrivateChat = "trusted_private_chat"
)

// Sender identifies who is performing an action: the authenticated user, plus the device
// that transaction IDs are scoped to.
type Sender struct {
	Localpart string
	UserID    string
	DeviceID  string
}

// Service is the room server.
type Service struct {
	store    *store.Store
	cfg      *config.Config
	events   *events.Service
	notifier *notifier.Notifier
	// names is the optional user-directory publisher.
	names           NamePublisher
	announceDevices DeviceListAnnouncer
	// observePresence is told about users who join rooms, so that presence has something to
	// report to the people they just met. Nil means the feature is off.
	observePresence PresenceObserver
	// federationJoiner performs the part of joining that only the federation transport can do:
	// asking a peer for a room this server does not hold. Nil means a room this server does not
	// hold does not exist, which is the correct answer for a homeserver that does not federate.
	federationJoiner FederationJoiner
	// federatesOutbound decides whether a locally originated event creates an obligation to tell
	// other homeservers about it. On by default, because a homeserver that does not send events to
	// peers is not federating, and the honest failure of a deployment that turns it off is a queue it
	// knows about rather than one it discovers.
	federatesOutbound bool
}

// New builds the room server.
func New(st *store.Store, cfg *config.Config, eventService *events.Service, nf *notifier.Notifier) *Service {
	return &Service{store: st, cfg: cfg, events: eventService, notifier: nf, federatesOutbound: true}
}

// WithoutFederationOutbound stops locally originated events from being queued for peers, and returns
// the service for chaining.
//
// It exists for a deployment that is walled off from other homeservers. The alternative — queueing
// deliveries nothing will ever attempt — turns every room with a remote member into growing storage for
// an event nobody will read.
func (s *Service) WithoutFederationOutbound() *Service {
	s.federatesOutbound = false
	return s
}

// Store exposes the store to the read-side services that share this package's data.
func (s *Service) Store() *store.Store { return s.store }

// Events exposes the event domain.
func (s *Service) Events() *events.Service { return s.events }

// Config exposes the configuration to the handlers that report limits.
func (s *Service) Config() *config.Config { return s.cfg }

// Publish wakes long-pollers after a write that did not go through [Service.append].
func (s *Service) Publish() { s.notifier.Publish() }

// InitialStateEvent is one state event supplied by the client at creation time.
type InitialStateEvent struct {
	Type     string          `json:"type"`
	StateKey string          `json:"state_key"`
	Content  json.RawMessage `json:"content"`
}

// CreateRequest is a normalised POST /createRoom payload.
type CreateRequest struct {
	// Creator is the full user ID of the room creator.
	Creator string
	// DeviceID scopes the creation's transaction ID.
	DeviceID string
	// RoomVersionName is the requested version; empty means the server default.
	RoomVersionName string
	// Visibility is "private" or "public".
	Visibility string
	// AliasLocalpart is the requested local part of the room's alias.
	AliasLocalpart string
	// Name, Topic and AvatarURL are the initial-state shortcuts the payload offers.
	Name      string
	Topic     string
	AvatarURL string
	// Preset selects the initial join rules, history visibility and guest access.
	Preset string
	// CreationContent is merged into m.room.create's content, which is how a client makes
	// an m.space or sets a predecessor.
	CreationContent map[string]any
	// PowerLevelContentOverride is merged into the default m.room.power_levels.
	PowerLevelContentOverride map[string]any
	// InitialState is arbitrary extra state.
	InitialState []InitialStateEvent
	// Invite lists user IDs to invite as part of creation.
	Invite []string
	// TransactionID makes creation idempotent per device.
	TransactionID string
	// IsDirect marks the room as a direct message.
	IsDirect bool
}

// roomTypeOfCreationContent reads the `type` field a create event is given, which is what makes
// a room a space (`m.space`) rather than a conversation. An absent or non-string type is an
// ordinary room, which is the common case and not worth an error.
func roomTypeOfCreationContent(content map[string]any) string {
	roomType, ok := content["type"].(string)
	if !ok {
		return ""
	}
	return roomType
}

// CreateRoom creates a room and returns its ID.
//
// The initial state is written in the order the authorization rules require: the create
// event first (nothing can be authorised without it), then power levels (so that everything
// after is judged against the intended levels), then the rules that define membership, then
// the creator's own join, then whatever the client asked for. Each event is built,
// authorised and inserted before the next is built, so each one's auth events are read back
// from the database rather than tracked in a parallel map — a handful of extra queries per
// room creation in exchange for removing the possibility of the two disagreeing.
func (s *Service) CreateRoom(ctx context.Context, req CreateRequest) (string, error) {
	version, err := s.events.ResolveRoomVersion(req.RoomVersionName)
	if err != nil {
		return "", err
	}
	if req.Visibility == "" {
		req.Visibility = matrix.VisibilityPrivate
	}
	if req.Visibility != matrix.VisibilityPrivate && req.Visibility != matrix.VisibilityPublic {
		return "", matrix.NewInvalidParam("visibility must be %q or %q",
			matrix.VisibilityPrivate, matrix.VisibilityPublic)
	}
	switch req.Preset {
	case PresetPrivateChat, PresetPublicChat, PresetTrustedPrivateChat, "":
	default:
		return "", matrix.NewInvalidParam("preset must be one of %q, %q or %q",
			PresetPrivateChat, PresetPublicChat, PresetTrustedPrivateChat)
	}
	if err := validateRoomField("name", req.Name, matrix.MaxRoomNameLength); err != nil {
		return "", err
	}
	if err := validateRoomField("topic", req.Topic, matrix.MaxRoomTopicLength); err != nil {
		return "", err
	}
	creatorLocalpart, _, err := matrix.ParseUserID(req.Creator, s.cfg.ServerName)
	if err != nil {
		return "", err
	}

	roomID, err := s.newRoomID()
	if err != nil {
		return "", err
	}

	// An alias is a namespace claim, so it is reserved before any event exists: claiming it
	// after building the room would leave the room half-published if the claim failed.
	alias := ""
	if req.AliasLocalpart != "" {
		if err := ValidAliasLocalpart(req.AliasLocalpart); err != nil {
			return "", err
		}
		alias = matrix.Alias(req.AliasLocalpart, s.cfg.ServerName)
	}

	initial := describeInitialRoom(req)

	err = s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		if alias != "" {
			if _, err := s.store.Aliases.Get(ctx, alias); err == nil {
				return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrRoomInUse,
					"Room alias %q is already in use", alias)
			} else if !store.IsNotFound(err) {
				return err
			}
		}

		room := &store.Room{
			ID:                roomID,
			Version:           string(version),
			CreatedMS:         time.Now().UnixMilli(),
			CreatorLocalpart:  creatorLocalpart,
			JoinRule:          initial.joinRule,
			HistoryVisibility: initial.historyVisibility,
			GuestAccess:       initial.guestAccess,
			Name:              truncate(req.Name, 255),
			Topic:             truncate(req.Topic, 1000),
			AvatarURL:         req.AvatarURL,
			// A room is "left" only once this server has nothing to do with it; a
			// newly-created room is joined by its creator.
			Left: false,
			// Making a room publicly joinable publishes it unless the creator asks
			// otherwise; the directory flag is then theirs to change.
			Publishable: initial.listed,
			// The room's type is a property of the room, not of the create event's
			// journey through this transaction, so it is read here — before the row is
			// written. Assigning it after `Rooms.Create` mutated a struct that had already
			// been inserted, which meant a space was stored as an ordinary room: the
			// directory's space flag was wrong, and a walk of the space tree refused to go
			// below the root, because only a space has children.
			RoomType: roomTypeOfCreationContent(req.CreationContent),
		}
		if err := s.store.Rooms.Create(ctx, room); err != nil {
			return err
		}

		writer := &eventWriter{service: s, tx: tx, ctx: ctx}

		// 1. The create event.
		createContent := map[string]any{}
		for key, value := range req.CreationContent {
			createContent[key] = value
		}
		// `creator` is the historical form of the room creator; from room version 11 the
		// create event has no content-derived identity and the sender is the creator.
		if impl, versionErr := s.events.RoomVersion(string(version)); versionErr == nil && impl.CreatorInCreateEvent() {
			createContent["creator"] = req.Creator
		}
		createContent["room_version"] = string(version)
		if _, ok := createContent["m.federate"]; !ok {
			createContent["m.federate"] = true
		}
		if _, err := writer.append(txAppend{
			roomID:    roomID,
			eventType: matrix.EventTypeCreate,
			stateKey:  "",
			isState:   true,
			sender:    req.Creator,
			content:   createContent,
			// Nothing can authorise the first event of a room: the room's own existence is
			// its authority.
			skipAuthorization: true,
		}); err != nil {
			return err
		}

		// 2. The creator's own join, immediately after the create event.
		//
		// The position is not cosmetic. Until somebody is a member, every event would fail
		// the "sender must be in the room" rule that applies to all but the first few
		// events of a room, and the protocol's exemption is specific: the creator may join
		// as the event directly following the create event. Anything after that has to be
		// authorized by power levels that do not exist yet.
		if _, err := writer.append(txAppend{
			roomID: roomID, eventType: matrix.EventTypeMember, stateKey: req.Creator, isState: true,
			sender: req.Creator, content: map[string]any{"membership": matrix.MembershipJoin},
		}); err != nil {
			return err
		}

		// 3. Power levels, so everything after this point is judged against them.
		powerLevels := events.DefaultPowerLevels(req.Creator, roomVersionIsPrivileged(version), nil)
		for key, value := range req.PowerLevelContentOverride {
			powerLevels[key] = value
		}
		if _, err := writer.append(txAppend{
			roomID: roomID, eventType: matrix.EventTypePowerLevels, stateKey: "", isState: true,
			sender: req.Creator, content: powerLevels,
		}); err != nil {
			return err
		}

		// 4. The state that defines membership rules.
		for _, state := range []struct {
			eventType string
			content   any
		}{
			{matrix.EventTypeJoinRules, map[string]any{"join_rule": initial.joinRule}},
			{matrix.EventTypeHistoryVisibility, map[string]any{"history_visibility": initial.historyVisibility}},
			{matrix.EventTypeGuestAccess, map[string]any{"guest_access": initial.guestAccess}},
		} {
			if _, err := writer.append(txAppend{
				roomID: roomID, eventType: state.eventType, stateKey: "", isState: true,
				sender: req.Creator, content: state.content,
			}); err != nil {
				return err
			}
		}

		// 5. Client-supplied initial state, applied *before* the descriptive fields below.
		//
		// The order is the point: `topic`, `name` and `avatar_url` are named by the request, so
		// the request wins over a state document that happens to mention the same thing. Written
		// the other way round, a client that sends both silently loses the field it named, and
		// the room opens with somebody's fallback text in the place of the topic that was asked
		// for by name.
		for _, initialEvent := range req.InitialState {
			if initialEvent.Type == "" {
				return matrix.NewInvalidParam("initial_state entries must have a type")
			}
			if initialEvent.Type == matrix.EventTypeCreate || initialEvent.Type == matrix.EventTypePowerLevels {
				// Replacing the create event would make the room's own identity
				// client-controlled; replacing power levels would let a creator hand the
				// room to somebody else in the same breath as creating it.
				return matrix.NewInvalidParam("initial_state may not replace %q", initialEvent.Type)
			}
			content := initialEvent.Content
			if len(content) == 0 {
				content = json.RawMessage("{}")
			}
			if _, err := writer.append(txAppend{
				roomID: roomID, eventType: initialEvent.Type, stateKey: initialEvent.StateKey,
				isState: true, sender: req.Creator, content: json.RawMessage(content),
			}); err != nil {
				return err
			}
		}

		// 6. The descriptive state the payload offered directly.
		descriptive := []struct {
			eventType string
			content   any
			enabled   bool
		}{
			{matrix.EventTypeName, map[string]any{"name": req.Name}, req.Name != ""},
			{matrix.EventTypeTopic, topicContent(req.Topic), req.Topic != ""},
			{matrix.EventTypeAvatar, map[string]any{"url": req.AvatarURL}, req.AvatarURL != ""},
		}
		for _, state := range descriptive {
			if !state.enabled {
				continue
			}
			if _, err := writer.append(txAppend{
				roomID: roomID, eventType: state.eventType, stateKey: "", isState: true,
				sender: req.Creator, content: state.content,
			}); err != nil {
				return err
			}
		}

		// 7. The room's alias.
		if alias != "" {
			if _, err := writer.append(txAppend{
				roomID: roomID, eventType: matrix.EventTypeCanonicalAlias, stateKey: "", isState: true,
				sender: req.Creator, content: map[string]any{"alias": alias},
			}); err != nil {
				return err
			}
			if err := s.store.Aliases.Create(ctx, &store.RoomAlias{
				Alias:             alias,
				RoomID:            roomID,
				CreatorLocalpart:  creatorLocalpart,
				ListedInDirectory: initial.listed,
			}); err != nil {
				return err
			}
		}

		// 8. Invites. A user the creator may not invite is skipped rather than failing the
		// creation, because the specification creates the room and reports the failures,
		// and a typo in one invite should not destroy the room the user just made.
		for _, invited := range req.Invite {
			invitedLocalpart, invitedDomain, parseErr := matrix.ParseUserID(invited, s.cfg.ServerName)
			if parseErr != nil {
				return parseErr
			}
			// An invite may name a remote user; the state key is always the full user ID
			// and this server simply cannot check a domain that is not its own.
			_ = invitedLocalpart
			inviteTarget := invited
			if invitedDomain == "" {
				inviteTarget = matrix.UserID(invited, s.cfg.ServerName)
			}
			if _, inviteErr := writer.append(txAppend{
				roomID: roomID, eventType: matrix.EventTypeMember, stateKey: inviteTarget, isState: true,
				sender: req.Creator,
				content: map[string]any{
					"membership":   matrix.MembershipInvite,
					"display_name": s.displayNameOf(ctx, invitedLocalpart),
				},
			}); inviteErr != nil {
				var apiErr *matrix.APIError
				if errors.As(inviteErr, &apiErr) && apiErr.Code == matrix.ErrForbidden {
					slog.Debug("skipping an invite the creator was not allowed to send",
						"room_id", roomID, "invite", invited, "reason", apiErr.Message)
					continue
				}
				return inviteErr
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	s.notifier.Publish()
	return roomID, nil
}

// initialRoom describes the state a preset asks for.
type initialRoom struct {
	joinRule          string
	historyVisibility string
	guestAccess       string
	listed            bool
}

// describeInitialRoom maps visibility and preset onto the room's initial rules.
//
// The defaults are the specification's: an unspecified preset behaves like private_chat, and
// `visibility: public` means the room is joinable and listed even when no preset was given.
func describeInitialRoom(req CreateRequest) initialRoom {
	initial := initialRoom{
		joinRule:          matrix.JoinRuleInvite,
		historyVisibility: matrix.HistoryJoined,
		guestAccess:       matrix.GuestCanJoin,
		listed:            false,
	}
	if req.Visibility == matrix.VisibilityPublic {
		initial.joinRule = matrix.JoinRulePublic
		initial.historyVisibility = matrix.HistoryShared
		initial.guestAccess = matrix.GuestForbidden
		initial.listed = true
	}
	switch req.Preset {
	case PresetPublicChat:
		initial.joinRule = matrix.JoinRulePublic
		initial.historyVisibility = matrix.HistoryShared
		initial.guestAccess = matrix.GuestForbidden
		initial.listed = true
	case PresetPrivateChat, PresetTrustedPrivateChat:
		initial.joinRule = matrix.JoinRuleInvite
		initial.historyVisibility = matrix.HistoryJoined
		initial.guestAccess = matrix.GuestCanJoin
	}
	return initial
}

// displayNameOf reads a user's display name for an invite event's content.
func (s *Service) displayNameOf(ctx context.Context, localpart string) string {
	user, err := s.store.Users.Get(ctx, localpart)
	if err != nil {
		return ""
	}
	return user.DisplayName
}

// txAppend describes one event to append to a room.
type txAppend struct {
	roomID    string
	eventType string
	stateKey  string
	isState   bool
	sender    string
	content   any
	// redacts is the event this one redacts.
	redacts string
	// txnID and deviceID scope a client transaction ID for idempotency.
	txnID    string
	deviceID string
	// skipAuthorization is used only for the create event and the power levels that come
	// with it, which no rule can judge because there is no prior state.
	skipAuthorization bool
	// expectedPrevEvent turns the append into a compare-and-set, for a client that names
	// the event it believes is the tip.
	expectedPrevEvent string
	// membership is the membership value when this is an m.room.member event; it is needed
	// to keep the room's member counters honest.
	membership string
}

// eventWriter appends events to a room inside one transaction.
type eventWriter struct {
	service *Service
	tx      store.Tx
	ctx     context.Context
}

// append is the single path by which an event joins a room.
// maxEventSizeBytes is the largest event this server will accept, counted over the event
// content the client sent.
//
// The number is the federation limit that every homeserver applies, not a preference: an
// event larger than this cannot be relayed by any other server, so accepting it would leave
// a room whose history cannot leave this building. The specification answers with 413
// M_TOO_LARGE.
const maxEventSizeBytes = 65536

func (w *eventWriter) append(req txAppend) (*store.EventRow, error) {
	s := w.service
	ctx := w.ctx

	if content, err := events.CanonicalJSONOf(req.content); err == nil && len(content) > maxEventSizeBytes {
		return nil, matrix.NewAPIError(http.StatusRequestEntityTooLarge, matrix.ErrTooLarge,
			"Event content is %d bytes, the limit is %d", len(content), maxEventSizeBytes)
	}

	room, err := s.store.Rooms.Get(ctx, req.roomID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
				"Room %q does not exist on this homeserver", req.roomID)
		}
		return nil, err
	}
	if room.UpgradedToRoomID != "" && !req.skipAuthorization && req.eventType != matrix.EventTypeTombstone {
		// A room that has been replaced refuses everything but a repeated tombstone: the
		// upgrade is the room's last word, and accepting more events would split its
		// history across two rooms.
		return nil, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrCannotLeaveServer,
			"Room %q has been replaced by %q", req.roomID, room.UpgradedToRoomID)
	}

	roomVersion, err := s.events.RoomVersion(room.Version)
	if err != nil {
		return nil, err
	}

	state, err := s.stateAt(ctx, req.roomID)
	if err != nil {
		return nil, err
	}

	tip, err := s.store.Events.Tip(ctx, req.roomID)
	if err != nil && !store.IsNotFound(err) {
		return nil, err
	}
	prevEvents := []string{}
	depth := int64(1)
	if tip != nil {
		prevEvents = []string{tip.EventID}
		depth = tip.Depth + 1
	}
	if req.expectedPrevEvent != "" && len(prevEvents) > 0 && prevEvents[0] != req.expectedPrevEvent {
		// The client's optimistic-concurrency check: its idea of the tip is stale, so its
		// event would be built on state it has not seen.
		return nil, matrix.NewAPIError(http.StatusConflict, matrix.ErrRoomInUse,
			"The room's latest event is not the one your client expected")
	}

	var stateKey *string
	if req.isState {
		key := req.stateKey
		stateKey = &key
	}

	content, err := normaliseContent(req.content)
	if err != nil {
		return nil, err
	}

	// A state write that changes nothing is not a change, and it must not become an event.
	//
	// Re-writing the same state is normal traffic: a client reconnecting and re-asserting its
	// name, a join retried because the response was lost, a server propagating a profile it
	// has already propagated. Creating a new event for each turns an idempotent operation into
	// one that grows the room's history every time it is repeated, and returns a *different*
	// event ID for a request the client already made — so the client's own deduplication, and
	// every peer's, has nothing to work with. The answer is the event that already says this.
	if req.isState && !req.skipAuthorization {
		if existing, err := s.unchangedState(ctx, state, req.roomID, req.eventType, req.stateKey, content); err != nil {
			return nil, err
		} else if existing != nil {
			return existing, nil
		}
	}

	// `unsigned` is deliberately not written here. A transaction ID belongs to the device
	// that sent the event; storing it in the event would hand it to every other reader and
	// would make the signed form differ depending on who asked. It is stored in its own
	// column and re-attached at render time, to the owning device only.
	var unsigned map[string]any

	// Room version 11 and later leave the room ID out of the create event, because there the
	// room ID is derived from that event's hash.
	eventRoomID := req.roomID
	if roomVersion.DomainlessRoomIDs() && req.eventType == matrix.EventTypeCreate {
		eventRoomID = ""
	}

	event, err := s.events.Build(events.Draft{
		RoomVersion: gomatrixserverlib.RoomVersion(room.Version),
		RoomID:      eventRoomID,
		Type:        req.eventType,
		StateKey:    stateKey,
		Sender:      req.sender,
		Content:     content,
		PrevEvents:  prevEvents,
		AuthState:   state,
		Depth:       depth,
		Redacts:     req.redacts,
		Unsigned:    unsigned,
	})
	if err != nil {
		return nil, err
	}

	if !req.skipAuthorization {
		if err := s.events.Authorize(event, state); err != nil {
			return nil, err
		}
	}

	row := &store.EventRow{
		EventID:        event.EventID(),
		RoomID:         req.roomID,
		Type:           req.eventType,
		StateKey:       stateKey,
		JSON:           string(event.JSON()),
		RoomVersion:    room.Version,
		Depth:          event.Depth(),
		OriginServerTS: int64(event.OriginServerTS()),
		ReceivedMS:     time.Now().UnixMilli(),
		Redacts:        req.redacts,
		TransactionID:  req.txnID,
		DeviceID:       req.deviceID,
		Sender:         req.sender,
	}
	row.RelatesToEventID, row.RelType = relationOf(event)
	if raw, marshalErr := json.Marshal(event.PrevEventIDs()); marshalErr == nil {
		row.PrevEvents = string(raw)
	}
	if raw, marshalErr := json.Marshal(event.AuthEventIDs()); marshalErr == nil {
		row.AuthEvents = string(raw)
	}
	written, err := s.store.Events.Insert(ctx, w.tx, row)
	if err != nil {
		return nil, err
	}

	if req.isState {
		replaced, err := s.store.State.Upsert(ctx, w.tx, req.roomID, written.EventID,
			req.eventType, req.stateKey, written.ID)
		if err != nil {
			return nil, err
		}
		if err := s.applyStateSideEffects(ctx, w.tx, room, req, written, replaced); err != nil {
			return nil, err
		}
	}

	// The event and the duty to deliver it are written together, so that a crash between the two
	// cannot leave a room whose members disagree about what happened.
	if err := s.queueFederationDelivery(ctx, w.tx, req.roomID, written); err != nil {
		return nil, err
	}

	return written, nil
}

// applyStateSideEffects keeps the denormalised tables in step with a new state event.
//
// Those tables are caches of room state, not a second source of truth: every field written
// here can be recomputed from the events. They exist because /sync, /publicRooms and
// /hierarchy ask for exactly these values for many rooms at once, and reading state events
// per room turns one list query into thousands of small ones.
func (s *Service) applyStateSideEffects(ctx context.Context, tx store.Tx, room *store.Room, req txAppend, row *store.EventRow, replacedEventID string) error {
	content, ok := req.content.(json.RawMessage)
	if !ok {
		if raw, err := json.Marshal(req.content); err == nil {
			content = raw
		}
	}
	fields := jsonFields(content)
	updates := map[string]any{}

	switch req.eventType {
	case matrix.EventTypeMember:
		membership := fields.stringAt("membership")
		if membership == "" {
			membership = matrix.MembershipLeave
		}
		// An m.room.member event's state key is a full user ID. The membership table is
		// keyed by local part for this server's own users, and by the whole user ID for
		// remote ones — the alternative, storing a remote "@bob:other.org" under "bob",
		// would let a remote user collide with a local one of the same name.
		memberKey := membershipKey(req.stateKey, s.cfg.ServerName)
		previous, memberErr := s.store.Memberships.Get(ctx, req.roomID, memberKey)
		wasJoined := memberErr == nil && previous.Membership == matrix.MembershipJoin
		s.publishMemberName(ctx, req.roomID, req.stateKey, membership, fields)
		if err := s.store.Memberships.Upsert(ctx, tx, &store.Membership{
			RoomID:      req.roomID,
			Localpart:   memberKey,
			UserID:      req.stateKey,
			Membership:  membership,
			StreamPos:   row.ID,
			DisplayName: fields.stringAt("displayname"),
			AvatarURL:   fields.stringAt("avatar_url"),
		}); err != nil {
			return err
		}
		// Both caches move only when this user crosses the boundary the counter counts. An
		// event that restates the same membership — a retried join, which clients send
		// routinely — must leave the number alone, or a room's member count drifts upward
		// until it stops resembling the room.
		isJoined := membership == matrix.MembershipJoin
		if isJoined != wasJoined {
			joined := room.JoinedCount
			if isJoined {
				joined++
			} else if joined > 0 {
				joined--
			}
			updates["joined_count"] = joined
		}
		// The invited count has to come back down as well as go up. An invitation that was
		// accepted, rejected, or superseded by a ban is no longer outstanding, and a client
		// drawing a "waiting for 1 person" badge from a count that only ever grows is being
		// told a lie about the room.
		isInvited := membership == matrix.MembershipInvite
		wasInvitedNow := wasInvited(previous, memberErr)
		if isInvited != wasInvitedNow {
			invited := room.InviteCount
			if isInvited {
				invited++
			} else if invited > 0 {
				invited--
			}
			updates["invite_count"] = invited
		}
		// Announced only now that the membership row exists. Deciding who cares means asking
		// which rooms this user is in, and a join announced before the join is recorded has no
		// room to find — so the one event that most needs announcing, a new member whose keys
		// everybody in the room must start trusting, would announce to nobody.
		s.announceMembership(ctx, req.roomID, req.stateKey, membership)
		s.observeMembershipPresence(ctx, req.stateKey, membership)

	case matrix.EventTypeName:
		updates["name"] = truncate(fields.stringAt("name"), 255)
	case matrix.EventTypeTopic:
		updates["topic"] = truncate(fields.stringAt("topic"), 1000)
	case matrix.EventTypeAvatar:
		updates["avatar_url"] = fields.stringAt("url")
	case matrix.EventTypeJoinRules:
		rule := fields.stringAt("join_rule")
		updates["join_rule"] = rule
		// Making a room publicly joinable also publishes it. Synapse does the same and
		// clients rely on it: a public room that never appears in the directory is
		// indistinguishable from a broken server.
		updates["publishable"] = rule == matrix.JoinRulePublic || rule == matrix.JoinRuleKnock ||
			rule == matrix.JoinRuleKnockRestricted
		if rule == matrix.JoinRulePublic || rule == matrix.JoinRuleKnock || rule == matrix.JoinRuleKnockRestricted {
			// Making a room publicly joinable also publishes it. Synapse does the same and
			// clients rely on it: a public room that never appears in the directory is
			// indistinguishable from a broken server.
			if err := s.store.Aliases.SetListedInDirectory(ctx, req.roomID, true); err != nil {
				return err
			}
		} else {
			if err := s.store.Aliases.SetListedInDirectory(ctx, req.roomID, false); err != nil {
				return err
			}
		}
	case matrix.EventTypeHistoryVisibility:
		updates["history_visibility"] = fields.stringAt("history_visibility")
	case matrix.EventTypeGuestAccess:
		updates["guest_access"] = fields.stringAt("guest_access")
	case matrix.EventTypeEncryption:
		updates["encrypted"] = true
	case matrix.EventTypeCanonicalAlias:
		updates["canonical_alias"] = fields.stringAt("alias")
	case matrix.EventTypeTombstone:
		replacement := fields.stringAt("replacement_room")
		if replacement != "" {
			updates["upgraded_to_room_id"] = replacement
			if err := s.store.Rooms.Update(ctx, replacement, map[string]any{
				"predecessor_room_id": req.roomID,
			}); err != nil && !store.IsNotFound(err) {
				// The replacement room may live on another server, in which case there is
				// nothing local to record.
				slog.Debug("not recording a predecessor for a remote upgraded-to room",
					"room_id", replacement, "error", err)
			}
		}
	}

	if len(updates) > 0 {
		if err := s.store.Rooms.Update(ctx, req.roomID, updates); err != nil {
			return err
		}
	}
	_ = replacedEventID
	return nil
}

// stateAt loads a room's current state as events: the state a new event is authorised
// against and the source of its auth events.
func (s *Service) stateAt(ctx context.Context, roomID string) ([]gomatrixserverlib.PDU, error) {
	state, err := s.store.State.All(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if len(state) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(state))
	for _, entry := range state {
		ids = append(ids, entry.EventID)
	}
	rows, err := s.store.Events.ByEventIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.EventRow, len(rows))
	for _, row := range rows {
		byID[row.EventID] = row
	}
	pdus := make([]gomatrixserverlib.PDU, 0, len(state))
	for _, entry := range state {
		row, ok := byID[entry.EventID]
		if !ok {
			// State pointing at a missing event is corruption, and the safe reading of
			// corruption is "that state does not exist" rather than "the room is broken".
			slog.Warn("room state references a missing event",
				"room_id", roomID, "type", entry.Type, "state_key", entry.StateKey,
				"event_id", entry.EventID)
			continue
		}
		event, err := s.events.ParseRoomVersioned(row.RoomVersion, json.RawMessage(row.JSON))
		if err != nil {
			slog.Error("stored event could not be parsed; treating it as absent from state",
				"event_id", row.EventID, "error", err)
			continue
		}
		pdus = append(pdus, event)
	}
	return pdus, nil
}

// newRoomID generates a room ID.
//
// The local part is 16 random base64url bytes rather than 32 hex characters: room IDs
// appear in every event, every URL and every federation PDU, so the shorter form that still
// carries 128 bits of entropy is the better trade.
func (s *Service) newRoomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a room ID: %w", err)
	}
	// Padding is trimmed because "=" is not in the grammar the specification allows in a
	// room ID's local part, and URL-encoding it would make every room ID longer to read.
	return matrix.RoomID(strings.TrimRight(base64.URLEncoding.EncodeToString(raw), "="), s.cfg.ServerName), nil
}

// normaliseContent turns any content value into canonical JSON.
func normaliseContent(value any) (json.RawMessage, error) {
	switch typed := value.(type) {
	case nil:
		return json.RawMessage("{}"), nil
	case json.RawMessage:
		return events.CanonicalJSON(typed)
	case []byte:
		return events.CanonicalJSON(typed)
	case string:
		return events.CanonicalJSON(json.RawMessage(typed))
	default:
		return events.CanonicalJSONOf(typed)
	}
}

// contentFields is a decoded content block, read once per side effect.
type contentFields struct {
	values map[string]json.RawMessage
}

func jsonFields(content json.RawMessage) contentFields {
	decoded := contentFields{values: map[string]json.RawMessage{}}
	if len(content) == 0 {
		return decoded
	}
	// A content block that will not decode is not fatal here: the cache keeps its previous
	// value and the event itself is stored verbatim, which is the truth.
	if err := json.Unmarshal(content, &decoded.values); err != nil {
		decoded.values = map[string]json.RawMessage{}
	}
	return decoded
}

// stringAt reads a string field, returning "" when it is absent, null or not a string.
func (c contentFields) stringAt(field string) string {
	raw, ok := c.values[field]
	if !ok {
		return ""
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// ValidAliasLocalpart validates the local part of a room alias.
//
// Aliases allow more characters than user IDs do (notably `.` and `-` are universal, and
// the specification permits anything a room can name itself with), so this is deliberately
// not [matrix.ValidLocalpart].
func ValidAliasLocalpart(localpart string) error {
	if localpart == "" {
		return matrix.NewInvalidParam("a room alias must not be empty")
	}
	if len(localpart) > matrix.MaxRoomAliasLength {
		return matrix.NewInvalidParam("a room alias must be at most %d characters", matrix.MaxRoomAliasLength)
	}
	if strings.ContainsAny(localpart, "#:@") {
		return matrix.NewInvalidParam("a room alias may not contain '#', ':' or '@'")
	}
	for _, r := range localpart {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r >= '\u0080': // the spec allows any non-control character outside ASCII
		case r == '=' || r == '_' || r == '-' || r == '/':
		case r == '.':
		default:
			return matrix.NewInvalidParam("a room alias may not contain %q", r)
		}
	}
	return nil
}

func truncate(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

func validateRoomField(name, value string, max int) error {
	if len([]rune(value)) > max {
		return matrix.NewInvalidParam("%s must be at most %d characters", name, max)
	}
	return nil
}

// membershipKey turns an m.room.member state key into the key this server stores the
// membership under.
func membershipKey(stateKey, serverName string) string {
	return matrix.MembershipKey(stateKey, serverName)
}

// memberStateKey is the inverse: the state key for a local part.
func memberStateKey(localpart, serverName string) string {
	if strings.HasPrefix(localpart, "@") {
		// Already a user ID, which is how a remote member arrives.
		return localpart
	}
	return matrix.UserID(localpart, serverName)
}

// roomVersionIsPrivileged reports whether a room version grants the creator their power
// implicitly, which changes what a legal m.room.power_levels event may contain.
func roomVersionIsPrivileged(version gomatrixserverlib.RoomVersion) bool {
	impl, err := gomatrixserverlib.GetRoomVersion(version)
	if err != nil {
		return false
	}
	return impl.PrivilegedCreators()
}

// wasInvited reports whether a prior membership row already counted as an invite, so that
// re-sending an invite does not inflate the room's invite count.
func wasInvited(membership *store.Membership, err error) bool {
	return err == nil && membership != nil && membership.Membership == matrix.MembershipInvite
}

// unchangedState returns the event that already holds this state, when the room's current event
// for (eventType, stateKey) says exactly what is being written.
//
// The comparison is over parsed content rather than bytes: two clients writing the same object
// differ in key order and whitespace, and treating that as a change would make idempotency depend
// on serialization details no client controls.
func (s *Service) unchangedState(ctx context.Context, state []gomatrixserverlib.PDU, roomID, eventType, stateKey string, content any) (*store.EventRow, error) {
	for _, event := range state {
		if event.Type() != eventType {
			continue
		}
		key := event.StateKey()
		if key == nil || *key != stateKey {
			continue
		}
		var current any
		if err := json.Unmarshal(event.Content(), &current); err != nil {
			return nil, nil // a stored event this server cannot parse is not a match to make
		}
		var replacement any
		encoded, err := events.CanonicalJSONOf(content)
		if err != nil {
			return nil, nil
		}
		if err := json.Unmarshal(encoded, &replacement); err != nil {
			return nil, nil
		}
		if !reflect.DeepEqual(current, replacement) {
			return nil, nil
		}
		row, err := s.store.Events.ByEventID(ctx, event.EventID())
		if err != nil {
			// The state table points at an event that cannot be read. Writing a fresh event
			// is the honest fallback: the client asked for this state, and it is now or will
			// be shortly true either way.
			return nil, nil //nolint:nilerr // deliberately not propagated to the client
		}
		if row.RoomID != roomID {
			return nil, nil
		}
		return row, nil
	}
	return nil, nil
}

// topicContent builds an m.room.topic event's content from the request's plain-text topic.
//
// The plain `topic` string is duplicated into the structured `m.topic` field because a topic is
// not always plain text, and a client that wants to render markup or a different language cannot
// invent the structure afterwards without racing the next writer. Writing it here means the two
// spellings of the same topic are produced from one value and cannot disagree — which is the only
// reason a second copy of the text is acceptable at all.
func topicContent(topic string) map[string]any {
	return map[string]any{
		"topic": topic,
		"m.topic": map[string]any{
			"m.text": []any{
				map[string]any{
					"body":     topic,
					"mimetype": "text/plain",
				},
			},
		},
	}
}

// NamePublisher is where a room's view of a user's name is published, so that the user directory
// can be searched by the name a room actually uses. It is an interface and optional for the same
// reason as every other derived index here: the member event is the truth, the directory is a copy,
// and a copy that fails must not turn a successful join into an error.
type NamePublisher interface {
	PublishRoomName(ctx context.Context, roomID, userID, membership, displayName, avatarURL string) error
}

// WithNamePublisher installs the publisher and returns the service for chaining.
func (s *Service) WithNamePublisher(publisher NamePublisher) *Service {
	s.names = publisher
	return s
}

// publishMemberName feeds the room's member event into the name publisher.
func (s *Service) publishMemberName(ctx context.Context, roomID, userID, membership string, fields contentFields) {
	if s.names == nil {
		return
	}
	if err := s.names.PublishRoomName(ctx, roomID, userID, membership,
		fields.stringAt("displayname"), fields.stringAt("avatar_url")); err != nil {
		slog.Warn("roomserver: could not publish a member name to the user directory",
			"room_id", roomID, "user_id", userID, "error", err)
	}
}

// PresenceObserver is told that a user was just seen in a room, which is the server's licence to
// call them online.
type PresenceObserver interface {
	ObserveUser(ctx context.Context, userID string)
}

// DeviceListAnnouncer is where a membership change is reported, so that the room's other members
// learn to re-query (or forget) that user's encryption keys. Optional and best-effort for the same
// reason as every other seam here: the membership event is the truth and the announcement is a
// notification about it.
type DeviceListAnnouncer interface {
	Announce(ctx context.Context, userID string, left bool) error
	// AnnounceRoom reports the change to the members of one room, which is the scope a membership
	// event has — and the only scope that still exists once a leave has been written, because the
	// user who left is then in no rooms at all.
	AnnounceRoom(ctx context.Context, userID, roomID string, left bool) error
}

// WithPresenceObserver installs the presence observer and returns the service for chaining.
func (s *Service) WithPresenceObserver(observer PresenceObserver) *Service {
	s.observePresence = observer
	return s
}

// WithDeviceListAnnouncer installs the announcer and returns the service for chaining.
func (s *Service) WithDeviceListAnnouncer(announcer DeviceListAnnouncer) *Service {
	s.announceDevices = announcer
	return s
}

// announceMembership tells a room's other members that somebody's device list is now worth
// re-querying (a join or an invite) or throwing away (a leave or a ban).
// observeMembershipPresence records a joining user as seen. A person who walks into a room has
// demonstrated they are there, and the people already in it cannot be expected to wait for the
// stranger's next sync before they can tell whether they are talking to somebody who is listening.
func (s *Service) observeMembershipPresence(ctx context.Context, userID, membership string) {
	if s.observePresence == nil || membership != matrix.MembershipJoin {
		return
	}
	s.observePresence.ObserveUser(ctx, userID)
}

func (s *Service) announceMembership(ctx context.Context, roomID, userID, membership string) {
	if s.announceDevices == nil {
		return
	}
	switch membership {
	case string(matrix.MembershipJoin), string(matrix.MembershipInvite):
		// A fall-through to the announce below.
	case string(matrix.MembershipLeave), string(matrix.MembershipBan), string(matrix.MembershipKnock):
		left := membership != string(matrix.MembershipKnock)
		if err := s.announceDevices.AnnounceRoom(ctx, userID, roomID, left); err != nil {
			slog.Warn("roomserver: could not announce a device-list change",
				"user_id", userID, "membership", membership, "error", err)
		}
		return
	default:
		return
	}
	if err := s.announceDevices.AnnounceRoom(ctx, userID, roomID, false); err != nil {
		slog.Warn("roomserver: could not announce a device-list change",
			"user_id", userID, "membership", membership, "error", err)
	}
}

// relationOf reads the relation target out of an event's content.
//
// Only an event that points at another event has anything to return: a plain message, a state event,
// or a content with `m.relates_to: null` (which is how a client clears a relation) all yield nothing.
// Decoding failures return nothing too rather than an error, because an unreadable relation on an
// event that is otherwise valid must not stop the event being written — it costs a missing entry in
// one query, where refusing the event costs the sender their message.
func relationOf(event gomatrixserverlib.PDU) (eventID, relType string) {
	content := event.Content()
	if len(content) == 0 {
		return "", ""
	}
	var relates struct {
		RelatesTo *struct {
			EventID string `json:"event_id"`
			RelType string `json:"rel_type"`
		} `json:"m.relates_to"`
	}
	if err := json.Unmarshal(content, &relates); err != nil || relates.RelatesTo == nil {
		return "", ""
	}
	return relates.RelatesTo.EventID, relates.RelatesTo.RelType
}
