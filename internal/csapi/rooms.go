package csapi

// Room, event, membership, alias and directory endpoints.
//
// These handlers are translation only: read the request into a service call, map the
// session onto a roomserver.Sender, write the response. The reason they are kept this thin
// is that every decision in this file is a decision about who may do what, and a decision
// made in a service is testable without a web server while a decision made between reading a
// URL and writing a status code is not.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// senderOf maps an authenticated session onto the identity the room server reasons about.
func senderOf(session *httpx.Session) roomserver.Sender {
	return roomserver.Sender{
		Localpart: session.Localpart,
		UserID:    session.UserID,
		DeviceID:  session.DeviceID,
	}
}

// readerOf is how a reader's relationship to a room is expressed. The membership is read
// per request rather than assumed, because history visibility depends on it and a cached
// guess is exactly how a server ends up leaking a private room's history to a banned user.
func (d Deps) readerOf(r *http.Request, roomID string, session *httpx.Session) roomserver.RenderOptions {
	reader := roomserver.RenderOptions{
		DeviceID:  session.DeviceID,
		Localpart: session.Localpart,
		// One cache per request: `unsigned.membership` needs the reader's membership trail,
		// and a paginated /messages renders a hundred events against the same handful of
		// membership changes.
		Cache: d.Rooms.NewRenderCache(),
	}
	if roomID == "" {
		return reader
	}
	membership, err := d.Store.Memberships.Get(r.Context(), roomID, session.Localpart)
	switch err {
	case nil:
		reader.Membership = membership.Membership
		reader.MembershipStreamPos = membership.StreamPos
		reader.Forgotten = membership.Forgotten
	default:
		// No membership row means the caller has never been in the room, which the
		// room server treats as "leave" for history purposes.
		reader.Membership = ""
	}
	return reader
}

// createRoomRequest is the POST /createRoom body.
type createRoomRequest struct {
	Visibility                string            `json:"visibility"`
	RoomAliasName             string            `json:"room_alias_name"`
	Name                      string            `json:"name"`
	Topic                     string            `json:"topic"`
	RoomAvatarURL             string            `json:"room_avatar_url"`
	AvatarURL                 string            `json:"avatar_url"`
	Preset                    string            `json:"preset"`
	CreationContent           map[string]any    `json:"creation_content"`
	PowerLevelContentOverride map[string]any    `json:"power_level_content_override"`
	InitialState              []initialStateRaw `json:"initial_state"`
	Invite                    []string          `json:"invite"`
	Invite3PID                []map[string]any  `json:"invite_3pid"`
	RoomVersionName           string            `json:"room_version_name"`
	RoomVersion               string            `json:"room_version"`
	IsDirect                  bool              `json:"is_direct"`
	CreationMetadata          json.RawMessage   `json:"creation"`
	PresetHistory             string            `json:"-"`
}

// initialStateRaw is one initial_state entry; `content` is kept raw so that a client
// sending numbers or nested structures does not have them rewritten on the way through.
type initialStateRaw struct {
	Type     string          `json:"type"`
	StateKey string          `json:"state_key"`
	Content  json.RawMessage `json:"content"`
}

func handleCreateRoom(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body createRoomRequest
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if len(body.CreationMetadata) > 0 {
			// `creation` (MSC3288-style) and `creation_content` mean the same thing; the
			// spec spells the field `creation_content`, and accepting both costs one line
			// where refusing one costs a client.
			if body.CreationContent == nil {
				var creation map[string]any
				if err := json.Unmarshal(body.CreationMetadata, &creation); err == nil {
					body.CreationContent = creation
				}
			}
		}

		initial := make([]roomserver.InitialStateEvent, 0, len(body.InitialState))
		for _, state := range body.InitialState {
			initial = append(initial, roomserver.InitialStateEvent{
				Type:     state.Type,
				StateKey: state.StateKey,
				Content:  state.Content,
			})
		}
		if len(body.Invite3PID) > 0 {
			// Identity-server integration is out of scope for this server; the room is
			// still created, which is what the spec asks of a server that cannot complete a
			// 3PID invite.
			slog.Info("ignoring invite_3pid: identity server integration is not enabled",
				"user_id", session.UserID)
		}

		roomID, err := deps.Rooms.CreateRoom(r.Context(), roomserver.CreateRequest{
			Creator:                   session.UserID,
			DeviceID:                  session.DeviceID,
			RoomVersionName:           firstNonEmpty(body.RoomVersionName, body.RoomVersion),
			Visibility:                body.Visibility,
			AliasLocalpart:            body.RoomAliasName,
			Name:                      body.Name,
			Topic:                     body.Topic,
			AvatarURL:                 firstNonEmpty(body.RoomAvatarURL, body.AvatarURL),
			Preset:                    body.Preset,
			CreationContent:           body.CreationContent,
			PowerLevelContentOverride: body.PowerLevelContentOverride,
			InitialState:              initial,
			Invite:                    body.Invite,
			IsDirect:                  body.IsDirect,
		})
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		if body.IsDirect {
			// is_direct marks the room as a direct message. In the specification the
			// inviter's client records that in its m.direct account data, so the server's
			// job here is only to create the room — recording a second, server-side copy
			// would give two sources of truth for the same fact.
			_ = roomID
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"room_id": roomID})
	}
}

func handleSendMessage(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		eventType := r.PathValue("eventType")
		txnID := r.PathValue("txnID")

		// The body of a send is the event's content itself, not wrapped in a `content`
		// field — unlike PUT /state, where the body is an object that *has* a content field.
		// Getting these two the same way round is a protocol error every client notices,
		// because every message the client sends would arrive nested one level too deep.
		content, err := httpx.ReadEventBody(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// Content is checked for the two things a JSON decoder accepts and the protocol
		// forbids: a body that is not an object, and a number no other server can reproduce
		// byte-for-byte.
		if err := matrix.ValidateEventContent(content); err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		eventID, replayed, err := deps.Rooms.SendMessage(r.Context(), senderOf(session),
			roomID, eventType, txnID, content)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// A replayed transaction answers identically to the original; that is the point of
		// idempotency, and a client that retried cannot tell the difference.
		_ = replayed
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"event_id": eventID})
	}
}

func handleSetState(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		eventType, stateKey := stateTupleOf(r)

		// The specification wraps a state write in a `content` field, but clients do not
		// always send it that way: a body that is the content itself is what a client writes
		// when it treats this endpoint like /send, and Synapse accepts it. Both spellings are
		// read here, and the wrapped form wins when a body has both, because that is what the
		// client that sent a wrapper meant.
		raw, err := httpx.ReadEventBody(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		content := raw
		var document map[string]json.RawMessage
		if err := json.Unmarshal(raw, &document); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON("the request body must be a JSON object"))
			return
		}
		if wrapped, ok := document["content"]; ok {
			content = wrapped
		}
		if len(content) == 0 || string(content) == "null" {
			httpx.RespondError(w, r, matrix.NewBadJSON("the request body must contain a content object"))
			return
		}
		// The same content rules apply whichever way the body was wrapped: a state event is
		// signed and federated exactly like a message.
		if err := matrix.ValidateEventContent(content); err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		eventID, err := deps.Rooms.SendStateEvent(r.Context(), senderOf(session),
			roomID, eventType, stateKey, content)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"event_id": eventID})
	}
}

func handleGetState(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		reader := dReader(deps, r, roomID, session)
		events, err := deps.Rooms.StateEventRows(r.Context(), roomID, reader)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, events)
	}
}

func handleGetStateEvent(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		eventType, stateKey := stateTupleOf(r)
		reader := dReader(deps, r, roomID, session)
		event, err := deps.Rooms.StateEventRow(r.Context(), roomID, eventType, stateKey, reader)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		// ?format=event asks for the whole event instead of its content. It is not a
		// convenience: a client that wants to know *who* set a state event, or to verify it,
		// needs the signed envelope, and the content alone has neither sender nor hashes.
		if r.URL.Query().Get("format") == "event" {
			httpx.RespondRawJSON(w, http.StatusOK, event)
			return
		}

		// The response is the event's *content*, not the event: this endpoint predates the
		// general state endpoints and clients parse it as a bare object.
		var document map[string]json.RawMessage
		if err := json.Unmarshal(event, &document); err != nil {
			httpx.RespondError(w, r, matrix.NewUnknown("could not read the state event"))
			return
		}
		content, ok := document["content"]
		if !ok {
			content = json.RawMessage("{}")
		}
		httpx.RespondRawJSON(w, http.StatusOK, content)
	}
}

func handleGetEvent(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		reader := dReader(deps, r, roomID, session)
		event, err := deps.Rooms.EventInRoom(r.Context(), roomID, r.PathValue("eventID"), reader)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondRawJSON(w, http.StatusOK, event)
	}
}

func handleMessages(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		query := r.URL.Query()

		limit := 10
		if raw := query.Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				httpx.RespondError(w, r, matrix.NewInvalidParam("limit must be an integer"))
				return
			}
			limit = parsed
		}

		filter := query.Get("filter")
		_, containsURL := inlineMessagesFilter(filter)
		response, err := deps.Rooms.Messages(r.Context(), roomserver.MessagesRequest{
			RoomID:          roomID,
			From:            query.Get("from"),
			To:              query.Get("to"),
			Direction:       query.Get("dir"),
			Limit:           limit,
			Filter:          filterKind(filter),
			LazyLoadMembers: lazyLoadRequested(filter),
			ContainsURL:     containsURL,
			Reader:          dReader(deps, r, roomID, session),
		})
		if err != nil {
			if strings.Contains(err.Error(), "unparsable stream token") {
				httpx.RespondError(w, r, matrix.NewInvalidParam("Invalid pagination token"))
				return
			}
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}

func handleMembers(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		query := r.URL.Query()

		notMembership := []string{}
		if raw := query.Get("not_membership"); raw != "" {
			notMembership = append(notMembership, strings.Split(raw, ",")...)
		}
		members, err := deps.Rooms.Members(r.Context(), roomserver.MembersRequest{
			RoomID:        roomID,
			NotMembership: notMembership,
			Membership:    query.Get("membership"),
			Terse:         query.Get("terse_memberships") == "true" || query.Get("terse_memberships") == "1",
			Reader:        dReader(deps, r, roomID, session),
		})
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"chunk": members})
	}
}

func handleJoinedMembers(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		// Existence is not membership: a caller who left still has a row, and answering this
		// endpoint from it would keep showing the guest list of a room they are no longer in.
		membership, err := dStore(deps).Memberships.Get(r.Context(), roomID, session.Localpart)
		if err != nil || membership.Membership != matrix.MembershipJoin {
			httpx.RespondError(w, r, matrix.NewForbidden("You are not joined to this room"))
			return
		}
		entries, err := dStore(deps).Memberships.List(r.Context(), roomID, []string{matrix.MembershipJoin}, nil)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		chunk := map[string]any{}
		for _, member := range entries {
			// The membership row's local part is the whole user ID for a remote member, so
			// rebuilding an ID from it would produce "@@bob:other.org:hs1".
			userID := member.UserID
			if userID == "" {
				userID = matrix.UserID(member.Localpart, deps.Config.ServerName)
			}
			// Both profile keys are always present, null when unset. A key that appears only
			// once a value exists cannot be told apart from a server that did not answer the
			// question, and clients have to branch on the difference.
			entry := map[string]any{"display_name": nilIfEmpty(member.DisplayName)}
			entry["avatar_url"] = nilIfEmpty(member.AvatarURL)
			chunk[userID] = entry
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"joined": chunk})
	}
}

func handleJoinedRooms(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		rooms, err := dStore(deps).Memberships.RoomsForUser(r.Context(), session.Localpart, matrix.MembershipJoin)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"joined_rooms": rooms})
	}
}

// membershipActionRequest is the body of the join/leave/invite/kick/ban endpoints.
type membershipActionRequest struct {
	UserID string `json:"user_id"`
	RoomID string `json:"room_id"`
	Reason string `json:"reason"`

	// extra holds the body's keys this request type does not define, kept raw so they can be
	// written into the member event unchanged.
	extra map[string]json.RawMessage
}

// extraContentKeys returns every key of a decoded body except the named ones. A membership
// body is a member event's content rather than a fixed set of fields: the specification lets a
// client attach whatever it likes, and a server that keeps only the reason throws away data the
// sender put there on purpose.
func extraContentKeys(document map[string]json.RawMessage, named ...string) map[string]json.RawMessage {
	extra := map[string]json.RawMessage{}
	for key, value := range document {
		taken := false
		for _, name := range named {
			if key == name {
				taken = true
				break
			}
		}
		if !taken {
			extra[key] = value
		}
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

func handleJoinRoom(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		// The specification spells joining two ways: POST /rooms/{roomID}/join and
		// POST /join/{roomIDOrAlias}. Clients use the second for aliases and the first for
		// a room they already hold an ID for, so both are served by this one handler.
		roomID := r.PathValue("roomID")
		if roomID == "" {
			roomID = r.PathValue("roomIDOrAlias")
		}

		// A join may name an alias instead of a room ID; clients do this constantly and the
		// spec allows either.
		if strings.HasPrefix(roomID, "#") {
			resolved, err := deps.Rooms.ResolveAlias(r.Context(), roomID)
			if err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			roomID = resolved
		}

		// The body is optional on this endpoint: an empty one means "join with no reason",
		// and a malformed one is worth reporting because the client meant something by it.
		//
		// It is decoded as a raw document rather than a struct because a join body *is* the
		// content of the member event it produces: a key this server does not know is still
		// something the client asked to have written into the room, and a struct would drop it
		// silently. Only the reason is read here; everything else travels along untouched.
		var raw map[string]json.RawMessage
		if err := httpx.DecodeJSONIfPresent(r, &raw); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		body := membershipActionRequest{extra: extraContentKeys(raw, "reason", "user_id", "room_id", "vi")}
		if encoded, ok := raw["reason"]; ok {
			if err := json.Unmarshal(encoded, &body.Reason); err != nil {
				httpx.RespondError(w, r, matrix.NewBadJSON("reason must be a string"))
				return
			}
		}

		// The servers to join through. `server_name` is this endpoint's spelling and `via` the one
		// an invite or knock carries; a client that names none is asking this server to work out
		// where the room is, which for a room it does not hold means the room ID's own home server.
		var servers []string
		servers = append(servers, r.URL.Query()["server_name"]...)
		servers = append(servers, r.URL.Query()["via"]...)

		eventID, err := deps.Rooms.JoinRoomVia(r.Context(), senderOf(session), roomID, body.Reason,
			body.extra, servers)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"room_id": roomID, "event_id": eventID})
	}
}

func handleLeaveRoom(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body membershipActionRequest
		if err := httpx.DecodeJSONIfPresent(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		eventID, err := deps.Rooms.LeaveRoom(r.Context(), senderOf(session),
			r.PathValue("roomID"), session.Localpart, body.Reason)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"room_id": eventID})
	}
}

func handleForgetRoom(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if err := deps.Rooms.Forget(r.Context(), session.Localpart, r.PathValue("roomID")); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

func handleInvite(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body membershipActionRequest
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if body.UserID == "" {
			httpx.RespondError(w, r, matrix.NewMissingParam("user_id"))
			return
		}
		if _, _, err := matrix.ParseUserID(body.UserID, deps.Config.ServerName); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		eventID, err := deps.Rooms.InviteToRoom(r.Context(), senderOf(session),
			r.PathValue("roomID"), body.UserID, body.Reason)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"event_id": eventID})
	}
}

func handleKick(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body membershipActionRequest
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if body.UserID == "" {
			httpx.RespondError(w, r, matrix.NewMissingParam("user_id"))
			return
		}
		if _, _, err := matrix.ParseUserID(body.UserID, deps.Config.ServerName); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		eventID, err := deps.Rooms.KickFromRoom(r.Context(), senderOf(session),
			r.PathValue("roomID"), body.UserID, body.Reason)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"event_id": eventID})
	}
}

func handleBan(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body membershipActionRequest
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if body.UserID == "" {
			httpx.RespondError(w, r, matrix.NewMissingParam("user_id"))
			return
		}
		if _, _, err := matrix.ParseUserID(body.UserID, deps.Config.ServerName); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		eventID, err := deps.Rooms.BanUser(r.Context(), senderOf(session),
			r.PathValue("roomID"), body.UserID, body.Reason)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"event_id": eventID})
	}
}

func handleUnban(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body membershipActionRequest
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if body.UserID == "" {
			httpx.RespondError(w, r, matrix.NewMissingParam("user_id"))
			return
		}
		eventID, err := deps.Rooms.UnbanUser(r.Context(), senderOf(session),
			r.PathValue("roomID"), body.UserID)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"event_id": eventID})
	}
}

func handleRedact(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var body struct {
			Reason string `json:"reason"`
		}
		// The redaction body may be absent entirely, in which case there is no reason.
		if err := httpx.DecodeJSONIfPresent(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		eventID, err := deps.Rooms.RedactEvent(r.Context(), senderOf(session),
			r.PathValue("roomID"), r.PathValue("eventID"), r.PathValue("txnID"), body.Reason)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"event_id": eventID})
	}
}

func handleRoomAliases(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The list of aliases is a room read: it names the addresses a room answers to, which
		// a private room does not publish to strangers.
		session := httpx.MustSession(r)
		if membership, err := dStore(deps).Memberships.Get(r.Context(), r.PathValue("roomID"), session.Localpart); err != nil ||
			membership.Membership != matrix.MembershipJoin {
			httpx.RespondError(w, r, matrix.NewForbidden("You are not a member of this room"))
			return
		}

		aliases, err := deps.Rooms.AliasesOfRoom(r.Context(), r.PathValue("roomID"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"aliases": aliases})
	}
}

// stateTupleOf splits the `state/{tail...}` path back into an event type and a state key.
//
// The empty state key has several spellings on the wire — absent, or a trailing slash — and
// all of them mean the same state tuple. Splitting here rather than registering one pattern
// per spelling is also what keeps a state key containing a slash working, which m.space.child
// and m.room.pinned_events clients do produce.
func stateTupleOf(r *http.Request) (eventType, stateKey string) {
	tail := r.PathValue("tail")
	if index := strings.Index(tail, "/"); index >= 0 {
		return tail[:index], strings.TrimPrefix(tail[index+1:], "/")
	}
	return tail, ""
}

func handleResolveAlias(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		alias := r.PathValue("alias")
		roomID, err := deps.Rooms.ResolveAlias(r.Context(), alias)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"room_id": roomID, "servers": []string{deps.Config.ServerName}})
	}
}

func handleCreateAlias(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		alias := r.PathValue("alias")
		var body struct {
			RoomID string `json:"room_id"`
		}
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if body.RoomID == "" {
			httpx.RespondError(w, r, matrix.NewMissingParam("room_id"))
			return
		}
		if err := deps.Rooms.CreateAlias(r.Context(), alias, body.RoomID, session.Localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

func handleDeleteAlias(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if err := deps.Rooms.DeleteAlias(r.Context(), r.PathValue("alias"), session.Localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

func handlePublicRooms(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.Method == http.MethodPost {
			// The POST form carries the same parameters in a body, plus a filter object
			// whose only field this server can honour is generic_search_term.
			var body struct {
				Limit             int    `json:"limit"`
				Since             string `json:"since"`
				GenericSearchTerm string `json:"generic_search_term"`
				Filter            struct {
					GenericSearchTerm string `json:"generic_search_term"`
				} `json:"filter"`
			}
			if err := httpx.DecodeJSONIfPresent(r, &body); err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			if term := body.Filter.GenericSearchTerm; term != "" {
				query.Set("filter", term)
			} else if body.GenericSearchTerm != "" {
				query.Set("filter", body.GenericSearchTerm)
			}
			if body.Limit > 0 {
				query.Set("limit", strconv.Itoa(body.Limit))
			}
			if body.Since != "" {
				query.Set("since", body.Since)
			}
		}
		limit := 100
		if raw := query.Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				httpx.RespondError(w, r, matrix.NewInvalidParam("limit must be an integer"))
				return
			}
			limit = parsed
		}
		page, err := deps.Rooms.PublicRooms(r.Context(), roomserver.PublicRoomsRequest{
			Limit:       limit,
			Since:       query.Get("since"),
			Filter:      query.Get("filter"),
			GuestAccess: query.Get("third_party_instance_id"),
		})
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, page)
	}
}

func handleRoomDirectoryVisibility(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("roomID")
		switch r.Method {
		case http.MethodGet:
			visibility, err := deps.Rooms.RoomDirectoryVisibility(r.Context(), roomID)
			if err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			httpx.RespondJSON(w, http.StatusOK, map[string]string{"visibility": visibility})
		case http.MethodPut:
			session := httpx.MustSession(r)
			var body struct {
				Visibility string `json:"visibility"`
			}
			if err := httpx.DecodeJSON(r, &body); err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			if err := deps.Rooms.SetRoomDirectoryVisibility(r.Context(), roomID, body.Visibility, session.Localpart); err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			httpx.RespondJSON(w, http.StatusOK, map[string]any{})
		}
	}
}

// dReader is [Deps.readerOf] with the receiver bound, so the handlers above read as one
// action rather than a method-call chain.
func dReader(deps Deps, r *http.Request, roomID string, session *httpx.Session) roomserver.RenderOptions {
	return deps.readerOf(r, roomID, session)
}

// dStore is a shorthand the read-only handlers use; it exists so that a handler reading a
// repository directly is visibly different from one going through a service.
func dStore(deps Deps) *store.Store { return deps.Store }

// nilIfEmpty renders an absent string as JSON null, which is how the client-server API spells
// "this account has no value for that profile field".
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// filterKind reads the `/messages` filter parameter's two spellings: the literal `recent`, and an
// inline JSON filter document (whose limit this endpoint does not honour, since `limit` is a query
// parameter of its own).
func filterKind(filter string) string {
	if strings.HasPrefix(strings.TrimSpace(filter), "{") {
		return ""
	}
	return filter
}

// inlineMessagesFilter reads the options a `/messages` request carries as a JSON document rather
// than as a saved filter ID.
//
// A filter that cannot be parsed is treated as no filter at all. The parameter is optional, and a
// server that refuses a pagination request over a malformed optional parameter withholds messages the
// caller is entitled to — the worst possible reading of "your filter made no sense".
func inlineMessagesFilter(filter string) (lazyLoad bool, containsURL *bool) {
	if !strings.HasPrefix(strings.TrimSpace(filter), "{") {
		return false, nil
	}
	// The flags have to be read out of a raw document rather than a struct, because "absent" and
	// "false" are different questions for contains_url: `false` asks for the events that have no
	// URL, and omitting it asks for every event. A plain bool cannot tell those apart.
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(filter), &document); err != nil {
		return false, nil
	}
	if raw, ok := document["lazy_load_members"]; ok {
		var wanted bool
		if err := json.Unmarshal(raw, &wanted); err == nil {
			lazyLoad = wanted
		}
	}
	if raw, ok := document["contains_url"]; ok {
		var wanted bool
		if err := json.Unmarshal(raw, &wanted); err == nil {
			containsURL = &wanted
		}
	}
	return lazyLoad, containsURL
}

// lazyLoadRequested reports whether a `/messages` filter asks for MSC1337 lazy loading.
func lazyLoadRequested(filter string) bool {
	lazyLoad, _ := inlineMessagesFilter(filter)
	return lazyLoad
}

// handleRelations answers GET /rooms/{roomID}/relations/{eventID}[/{relType}[/{eventType}]].
//
// The two optional path segments are the whole filtering interface the endpoint offers, which is why
// they are path segments and not query parameters: `/relations` is one endpoint with three shapes,
// and a client that spells the third gets a narrower answer rather than a different endpoint. The
// event type may be `*`, the spec's wildcard, because a relation type is often what people mean
// ("show me the reactions") and the event carrying them varies by server.
func handleRelations(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		query := r.URL.Query()

		limit := 5
		if raw := query.Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				httpx.RespondError(w, r, matrix.NewInvalidParam("limit must be an integer"))
				return
			}
			limit = parsed
		}

		response, err := deps.Rooms.Relations(r.Context(), roomserver.RelationsRequest{
			RoomID:    roomID,
			EventID:   r.PathValue("eventID"),
			RelType:   r.PathValue("relType"),
			EventType: r.PathValue("eventType"),
			From:      query.Get("from"),
			To:        query.Get("to"),
			Direction: query.Get("dir"),
			Limit:     limit,
			Reader:    dReader(deps, r, roomID, session),
		})
		if err != nil {
			if strings.Contains(err.Error(), "unparsable stream token") {
				httpx.RespondError(w, r, matrix.NewInvalidParam("Invalid pagination token"))
				return
			}
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}

// handleRoomSummary answers GET /_matrix/client/v1/room_summary/{roomIDOrAlias}.
//
// A client asks this before it asks to join, so the fields that matter are the ones that decide what
// joining would cost: the join rule, the rooms whose membership would satisfy a restricted join,
// whether guests may enter, and how many people are already in there. Returning a summary of a room
// the caller cannot see would leak all of that about a private room, so the read permission check is
// the same one every other read of the room makes.
func handleRoomSummary(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		// Resolve first, then decide what the caller may see: permission is a fact about the room, and
		// an alias is only a way of naming one. Asking whether the caller may read "#office:example"
		// is not a question with an answer.
		roomID := r.PathValue("roomIDOrAlias")
		if strings.HasPrefix(roomID, "#") {
			resolved, err := deps.Rooms.ResolveAlias(r.Context(), roomID)
			if err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			roomID = resolved
		}
		summary, err := deps.Rooms.RoomSummaryV1(r.Context(), roomID,
			dReader(deps, r, roomID, session))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, summary)
	}
}

// handleRoomHierarchy answers GET /_matrix/client/v1/rooms/{roomID}/hierarchy.
//
// The query parameters are the caller's, not the room's: `limit` is how many rooms this page
// should carry, `max_depth` how far down the tree to go, `suggested_only` whether to follow
// links the space's administrator did not recommend, and `from` a position in a walk already
// begun. They are parsed here and enforced there — see roomserver.Hierarchy for what each one
// is allowed to change.
func handleRoomHierarchy(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		roomID := r.PathValue("roomID")
		query := r.URL.Query()

		limit := 0
		if raw := query.Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 {
				httpx.RespondError(w, r, matrix.NewInvalidParam("limit must be a positive integer"))
				return
			}
			limit = parsed
		}
		// An absent max_depth means "as far as there is", which the service reads as -1;
		// the specification allows a non-negative integer and treats no-limit as a value
		// the caller has to ask for by leaving it out.
		maxDepth := -1
		if raw := query.Get("max_depth"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				httpx.RespondError(w, r, matrix.NewInvalidParam("max_depth must be a non-negative integer"))
				return
			}
			maxDepth = parsed
		}
		suggestedOnly := false
		if raw := query.Get("suggested_only"); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				httpx.RespondError(w, r, matrix.NewInvalidParam("suggested_only must be true or false"))
				return
			}
			suggestedOnly = parsed
		}

		page, err := deps.Rooms.Hierarchy(r.Context(), roomID, dReader(deps, r, roomID, session),
			limit, maxDepth, suggestedOnly, query.Get("from"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, page)
	}
}
