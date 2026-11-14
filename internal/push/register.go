package push

// The two doors a client uses: "tell this device about things", and "what have I been told is worth
// reading?".
//
// Both look like CRUD on a table and both hide a decision worth stating.
//
// The first is that a pusher is bound to the session that registered it. A pusher is a phone being
// told what its user's messages say, and the session is what the user can revoke: signing out of a
// device, deleting it, changing a password that logs other devices out, deactivating the account —
// each of those has to end the pusher, or the user's revocation is a UI gesture rather than a
// change. That is why every read of the table filters on the session still existing, and why
// registration records which session did it.
//
// The second is that `GET /notifications` answers from what was recorded when the event arrived, not
// from what the rules would say now. A person who mutes a room does not want its past notifications
// to have evaporated; a person who turns mentions back on does not want old messages to suddenly
// claim to be mentions. The judgement belongs to the moment, and this endpoint is a list of moments.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

const (
	// maxPushersPerAccount bounds how many pushers one account may register.
	//
	// It is not a performance knob. A pusher is this server agreeing to make an outbound request
	// every time somebody sends that user a message, at a URL the user chose; a single scripted
	// account with no limit could turn a busy room into a flood of requests to a host of its
	// choosing, and could do it under somebody else's name. A hundred is far above what a person
	// with phones, laptops, and a UnifiedPush topic will ever need.
	maxPushersPerAccount = 100

	// maxFieldLength bounds the free-text fields of a pusher. They are echoed back to gateways
	// and into logs, and an unbounded one is a memory leak with extra steps.
	maxFieldLength = 512
)

// Kind is the delivery mechanism of a pusher. `http` is the only one the specification defines, and
// accepting others would be accepting a pusher this server cannot deliver to.
const Kind = "http"

// Registration is one client's request about one pusher.
type Registration struct {
	// Localpart, AccessToken and DeviceID come from the authenticated session. The access token
	// is not decoration: it is what the pusher is bound to.
	Localpart   string
	AccessToken string
	DeviceID    string
	// Kind selects the operation. `http` registers or refreshes; empty means "withdraw the
	// pusher", which is how a client deletes one — a DELETE would have said the same thing, but
	// the endpoint is a POST to `/pushers/set` and a server argues with that at its clients'
	// expense.
	Kind     string
	AppID    string
	PushKey  string
	Data     map[string]any
	Append   bool
	AppendTo string
	// The display fields are what a notification is dressed in: the app's name, the device's
	// name, the user's language. A gateway needs them to write a sentence a person can read.
	AppDisplayName    string
	DeviceDisplayName string
	ProfileTag        string
	Lang              string
}

// ClientPusher is a pusher as its owner sees it.
type ClientPusher struct {
	Kind              string         `json:"kind"`
	AppID             string         `json:"app_id"`
	PushKey           string         `json:"pushkey"`
	Data              map[string]any `json:"data"`
	DeviceID          string         `json:"device_id,omitempty"`
	ProfileTag        string         `json:"profile_tag,omitempty"`
	Lang              string         `json:"lang,omitempty"`
	AppDisplayName    string         `json:"app_display_name,omitempty"`
	DeviceDisplayName string         `json:"device_display_name,omitempty"`
}

// ClientNotification is one entry of GET /notifications.
type ClientNotification struct {
	RoomID string          `json:"room_id"`
	Event  json.RawMessage `json:"event"`
	// Actions is what the rule said at the time, which is what a client needs to decide whether
	// this was a badge, a noise, or a highlight.
	Actions []any `json:"actions"`
	// Unread is whether a receipt covers the event yet. It is per notification rather than per
	// room because the list spans rooms, and because a person reading one message on a laptop
	// should not have their phone told the rest of the room was read.
	Unread bool `json:"unread"`
}

// Register registers, refreshes, or withdraws a pusher.
//
// Refreshing is the ordinary case and the reason this is an upsert rather than a create: an app that
// re-registers its pusher on every launch (which is what apps do, because a push token changes
// without warning) must not end up with one pusher per launch, and a server that allowed that would
// notify the user once per launch per message.
func (s *Service) Register(ctx context.Context, reg Registration) error {
	appID := strings.TrimSpace(reg.AppID)
	pushKey := strings.TrimSpace(reg.PushKey)
	if appID == "" {
		return matrix.NewMissingParam("app_id")
	}
	if pushKey == "" {
		return matrix.NewMissingParam("pushkey")
	}
	if err := printable("app_id", appID); err != nil {
		return err
	}
	if err := printable("pushkey", pushKey); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"app_display_name":    reg.AppDisplayName,
		"device_display_name": reg.DeviceDisplayName,
		"profile_tag":         reg.ProfileTag,
		"lang":                reg.Lang,
	} {
		if len(value) > maxFieldLength {
			return matrix.NewInvalidParam("%s must be at most %d bytes", name, maxFieldLength)
		}
		if err := printable(name, value); err != nil {
			return err
		}
	}
	if reg.Kind != "" && reg.Kind != Kind {
		return matrix.NewInvalidParam("pusher kind %q is not supported, only %q", reg.Kind, Kind)
	}
	if err := validateData(reg.Data); err != nil {
		return err
	}
	if reg.Kind == "" {
		return s.withdraw(ctx, reg.Localpart, appID, pushKey)
	}
	return s.enrol(ctx, reg, appID, pushKey)
}

// enrol records a pusher, or updates the one this request identifies.
func (s *Service) enrol(ctx context.Context, reg Registration, appID, pushKey string) error {
	existing, err := s.store.Push.GetPusher(ctx, reg.Localpart, appID, pushKey)
	if reg.Append {
		// Appending is how a client updates one field of the data it passed at registration
		// without knowing the rest of it. Refusing when the pusher is absent is the honest
		// answer: the client's assumption about its own state is wrong, and quietly creating a
		// pusher from a partial body would leave a device registered with only the field this
		// request carried — including, quite possibly, no gateway URL at all.
		if err != nil {
			if store.IsNotFound(err) {
				return matrix.NewNotFound("there is no pusher to append to")
			}
			return err
		}
		return s.put(ctx, reg, appID, pushKey, appendData(existing.Data, reg))
	}
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	if err == nil && existing.AccessToken == reg.AccessToken {
		// The same session refreshing the same pusher. The registration time moves, which is
		// what a gateway's `pushkey_ts` means, and nothing else changes.
		existing.UpdatedMS = nowMS()
		return s.store.Push.PutPusher(ctx, existing)
	}
	// A different session registering the same pusher takes it over. That is right rather than
	// surprising: the pusher belongs to the account, and the session that registered it can only
	// ever be one of them.
	data, err := json.Marshal(reg.Data)
	if err != nil {
		return fmt.Errorf("encoding pusher data: %w", err)
	}
	return s.put(ctx, reg, appID, pushKey, string(data))
}

// put writes the row, enforcing the per-account bound.
func (s *Service) put(ctx context.Context, reg Registration, appID, pushKey, data string) error {
	count, err := s.store.Push.CountPushers(ctx, reg.Localpart)
	if err != nil {
		return err
	}
	if existing, err := s.store.Push.GetPusher(ctx, reg.Localpart, appID, pushKey); err == nil {
		if existing != nil {
			// Replacing an existing pusher cannot take the account over its bound; counting
			// it as new would mean a user at the limit could never refresh a pusher.
			count--
		}
	} else if !store.IsNotFound(err) {
		return err
	}
	if count >= maxPushersPerAccount {
		return matrix.NewAPIError(400, matrix.ErrCode("pusher_pool_limit_reached"),
			"an account may register at most %d pushers", maxPushersPerAccount)
	}
	return s.store.Push.PutPusher(ctx, &store.Pusher{
		Localpart:         reg.Localpart,
		AppID:             appID,
		PushKey:           pushKey,
		Kind:              Kind,
		AccessToken:       reg.AccessToken,
		DeviceID:          reg.DeviceID,
		Data:              data,
		AppDisplayName:    reg.AppDisplayName,
		DeviceDisplayName: reg.DeviceDisplayName,
		ProfileTag:        reg.ProfileTag,
		Lang:              reg.Lang,
		UpdatedMS:         nowMS(),
	})
}

// withdraw removes a pusher and anything this server still owes it.
func (s *Service) withdraw(ctx context.Context, localpart, appID, pushKey string) error {
	removed, err := s.store.Push.DeletePusher(ctx, localpart, appID, pushKey)
	if err != nil {
		return err
	}
	if err := s.store.Push.DeletePushesOfPusher(ctx, localpart, appID, pushKey); err != nil {
		return err
	}
	// `removed` being false is not an error. The client is expressing a wish — that this pusher
	// not be told things — and a wish that is already true needs no message about it.
	_ = removed
	return nil
}

// Pushers lists an account's pushers, including the ones another of its sessions registered.
//
// The specification asks for that, and it is the right reading: this is the list a person uses to
// notice that a phone they no longer own is still being sent their messages. A list scoped to the
// session asking would hide exactly the thing worth seeing.
func (s *Service) Pushers(ctx context.Context, localpart string) ([]ClientPusher, error) {
	rows, err := s.store.Push.ListPushers(ctx, localpart)
	if err != nil {
		return nil, err
	}
	out := make([]ClientPusher, 0, len(rows))
	for _, row := range rows {
		out = append(out, ClientPusher{
			Kind:              row.Kind,
			AppID:             row.AppID,
			PushKey:           row.PushKey,
			Data:              decodeData(row.Data),
			DeviceID:          row.DeviceID,
			ProfileTag:        row.ProfileTag,
			Lang:              row.Lang,
			AppDisplayName:    row.AppDisplayName,
			DeviceDisplayName: row.DeviceDisplayName,
		})
	}
	return out, nil
}

// Notifications reads one page of what this server told the person was worth their attention.
//
// The page token is a stream position in disguise, which is what makes paging stable: an offset
// would shift under a write, so a person on page two would be shown a notification they had already
// seen and never shown the one that moved. `only` narrows the list to the notifications that used to
// have a badge of their own — mentions — because that is the question a client asks when it is
// building a "needs you" view.
func (s *Service) Notifications(ctx context.Context, localpart, page string, pageSize int,
	only string, recentN int) ([]ClientNotification, string, error) {
	switch only {
	case "", "all", "mentions", "errors":
	default:
		return nil, "", matrix.NewInvalidParam("unknown value %q for only", only)
	}
	if only == "errors" {
		// This server has no failure notifications to report: a delivery that could not be made
		// is retried, not logged as a notification about the failure. Answering with an empty
		// list is the truth; inventing an error would be a claim about something that did not
		// happen.
		return []ClientNotification{}, "", nil
	}
	before, err := decodePage(page)
	if err != nil {
		return nil, "", err
	}
	if pageSize <= 0 {
		pageSize = defaultNotificationPageSize
	}
	if pageSize > maxNotificationPageSize {
		return nil, "", matrix.NewInvalidParam("page_size must be at most %d", maxNotificationPageSize)
	}
	// `only=mentions` asks for highlights only, which the store answers from the actions it
	// recorded rather than by re-running rules: the judgement is the record.
	rows, err := s.store.Push.ListNotifications(ctx, localpart, before, pageSize, only == "mentions")
	if err != nil {
		return nil, "", err
	}
	if recentN > 0 && recentN < len(rows) {
		// `recent_n` caps the newest few, and the caller is paging from the top, so the cap
		// applies to the end of what was read: the oldest rows are the ones dropped.
		rows = rows[:recentN]
	}
	out := make([]ClientNotification, 0, len(rows))
	positions := map[string]readPositions{}
	for _, row := range rows {
		stored, err := s.store.Events.ByEventID(ctx, row.EventID)
		if err != nil {
			if store.IsNotFound(err) {
				// The event has gone. A notification whose event this server no longer
				// holds cannot be shown, and answering with an event this server invented
				// would be worse than answering with one fewer row.
				continue
			}
			return nil, "", err
		}
		unread, err := s.isUnread(ctx, positions, localpart, row)
		if err != nil {
			return nil, "", err
		}
		event, err := eventForClient(stored)
		if err != nil {
			return nil, "", err
		}
		out = append(out, ClientNotification{
			RoomID:  row.RoomID,
			Event:   event,
			Actions: decodeActions(row.Actions),
			Unread:  unread,
		})
	}
	next := ""
	if len(rows) > 0 {
		next = encodePage(rows[len(rows)-1].Position)
	}
	return out, next, nil
}

// eventForClient renders a stored event the way a client expects to receive it.
//
// The stored JSON is what this server persisted, and an event ID is not always a key inside it —
// the sync path injects it the same way, and so does this. Returning the raw column would hand a
// client an event it cannot name, and this endpoint is a list of events: an entry without an ID is
// not something a client can open, quote, or mark read.
func eventForClient(row *store.EventRow) (json.RawMessage, error) {
	var document map[string]any
	if err := json.Unmarshal([]byte(row.JSON), &document); err != nil {
		return nil, fmt.Errorf("the stored form of %s is not JSON: %w", row.EventID, err)
	}
	document["event_id"] = row.EventID
	if row.RoomID != "" {
		document["room_id"] = row.RoomID
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("rendering %s for a client: %w", row.EventID, err)
	}
	return encoded, nil
}

// isUnread answers whether a receipt covers this notification yet.
func (s *Service) isUnread(ctx context.Context, cache map[string]readPositions, localpart string,
	row store.Notification) (bool, error) {
	if s.reader == nil {
		// Without read positions nothing can be claimed either way. Reporting it as unread is
		// the answer that cannot mislead anybody into thinking they are up to date.
		return true, nil
	}
	where, err := positionsOf(ctx, s.reader, cache, row.RoomID, localpart)
	if err != nil {
		return false, err
	}
	if row.Position <= where.roomWide {
		return false, nil
	}
	if row.ThreadID != "" {
		if covered, known := where.threads[row.ThreadID]; known && row.Position <= covered {
			return false, nil
		}
	}
	return true, nil
}

// validateData checks the object a client wants stored with its pusher.
//
// The URL is the one key this server acts on, and it is checked here rather than at delivery time
// for the same reason a form validates its input: a pusher that can never deliver should not be
// allowed to exist, and refusing at registration tells the client about the mistake in the request
// that made it, rather than leaving the user to wonder why their phone is quiet.
func validateData(data map[string]any) error {
	if data == nil {
		return nil
	}
	if len(data) > 32 {
		return matrix.NewInvalidParam("a pusher's data may hold at most %d keys", 32)
	}
	raw, ok := data["url"]
	if !ok {
		return nil
	}
	text, _ := raw.(string)
	if strings.TrimSpace(text) == "" {
		return matrix.NewInvalidParam("a pusher's data.url must be a non-empty URL")
	}
	if err := validateGatewayURL(text); err != nil {
		return matrix.NewInvalidParam("%s", err.Error())
	}
	return nil
}

// appendData merges one request's data into a pusher that already exists.
//
// `append_to` names a dotted path inside the pusher's own data, and a leading `data.` is accepted
// and ignored — clients write that prefix because other parts of the API name these keys from
// outside the object, and a server that made them choose between two spellings of the same key
// would only be documenting its own parsing.
func appendData(existing string, reg Registration) string {
	current := map[string]any{}
	if existing != "" {
		var stored map[string]any
		if err := json.Unmarshal([]byte(existing), &stored); err == nil && stored != nil {
			current = stored
		}
	}
	path := strings.TrimPrefix(strings.TrimPrefix(reg.AppendTo, "data."), ".")
	if reg.AppendTo == "" || path == "" {
		for key, value := range reg.Data {
			current[key] = value
		}
	} else {
		setPath(current, strings.Split(path, "."), reg.Data)
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		// A pusher's data is whatever the client sent, and JSON values round-trip; a failure here
		// means the supplied data held something that cannot be encoded, which the caller's
		// validation should have caught. Keeping the previous data is the safe answer.
		return existing
	}
	return string(encoded)
}

// setPath writes `value` at a dotted path, creating the objects along the way.
func setPath(target map[string]any, path []string, value map[string]any) {
	for index, key := range path {
		if index == len(path)-1 {
			nested, _ := target[key].(map[string]any)
			if nested == nil {
				nested = map[string]any{}
			}
			for name, item := range value {
				nested[name] = item
			}
			target[key] = nested
			return
		}
		nested, _ := target[key].(map[string]any)
		if nested == nil {
			nested = map[string]any{}
			target[key] = nested
		}
		target = nested
	}
}

// decodeData reads a stored pusher's data back for a client.
func decodeData(raw string) map[string]any {
	data := map[string]any{}
	if raw == "" {
		return data
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return map[string]any{}
	}
	return data
}

// printable refuses control characters in the free-text fields of a pusher.
//
// These strings reach a gateway's request body and this server's logs. A newline in an app ID is not
// a creative name, it is a request split in two or a log line forged, and neither is worth
// accommodating.
func printable(name, value string) error {
	if strings.ContainsAny(value, "\r\n\t") {
		return matrix.NewInvalidParam("%s must not contain newlines or tabs", name)
	}
	for _, r := range value {
		if r < 0x20 {
			return matrix.NewInvalidParam("%s must not contain control characters", name)
		}
	}
	return nil
}

// The page-token codec. A stream position, marked so that a token from /sync — which is also a
// stream position, in a different namespace — fails here with a clear message rather than paging
// from a place the caller never asked for.
const pagePrefix = "np"

func encodePage(position int64) string { return pagePrefix + strconv.FormatInt(position, 10) }

func decodePage(page string) (int64, error) {
	if page == "" {
		return 0, nil
	}
	if !strings.HasPrefix(page, pagePrefix) {
		return 0, matrix.NewInvalidParam("unknown page token %q", page)
	}
	position, err := strconv.ParseInt(strings.TrimPrefix(page, pagePrefix), 10, 64)
	if err != nil || position < 0 {
		return 0, matrix.NewInvalidParam("unknown page token %q", page)
	}
	return position, nil
}

// errNoGateway is what a pusher with no usable URL is: a registration this server can never honour.
var errNoGateway = errors.New("the pusher has no gateway URL")
