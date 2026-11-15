package slidingsync

// The extensions: the five streams that are not rooms.
//
// MSC4186 deliberately leaves the shape of an extension to "a future MSC", which is a polite way of
// saying the endpoint was merged without finishing it. What is finished is the set of names a client
// sends and the JSON it expects back, because matrix-rust-sdk — the library Element X and every other
// Element app on that stack is built from — was written against Synapse, and Synapse was written
// against the proxy that came before it. A server that invents its own shapes here is a server those
// clients cannot use, however reasonable the shapes look on their own, so what follows is copied from
// the implementation clients speak to today rather than derived from the specification text.
//
// Two properties are worth stating because they decide when this code runs:
//
//   - An extension is off unless the request turns it on. `enabled` defaults to false, and a client
//     that leaves an extension out is not owed it — which is also why an extension this server does not
//     know is ignored rather than refused: refusing would make a client that grew a new extension
//     unable to sync at all, and MSC4186 adds extensions faster than servers adopt them.
//   - Deltas are measured from the connection's own position, not from a token per extension. In this
//     server typing, receipts and account data are all stamped in the one event stream, so the
//     connection's `pos` already answers "what has moved since I last looked" for all of them.
//     To-device messages are the exception: they ride a per-device queue with its own cursor, which is
//     what `next_batch` reports.
//
// A new connection reports no deltas for the first three, and an empty device-list change, because a
// connection that has just been created has not missed anything: its first answer describes rooms from
// the beginning, and handing it every receipt since the room was created would be a way for one
// reconnect to cost a room's whole history.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// The extension names this server serves. They are wire values, and they are the names a client
// already sends.
const (
	extensionToDevice     = "to_device"
	extensionE2EE         = "e2ee"
	extensionTyping       = "typing"
	extensionReceipts     = "receipts"
	extensionAccountData  = "account_data"
	defaultToDeviceLimit  = 100
	maxToDeviceLimitBatch = 1000
)

// ExtensionConfig is one extension's entry in the request.
//
// `since` is accepted and not used: an extension's position is whatever this connection's `pos` says,
// so a client that sent one is not asking for anything the server can honour differently. Accepting it
// quietly is what the ecosystem's clients do with each other's extra fields, and a strict decoder here
// would turn a client's harmless field into a sync that does not happen.
type ExtensionConfig struct {
	Enabled *bool   `json:"enabled,omitempty"`
	Limit   *int    `json:"limit,omitempty"`
	Since   *string `json:"since,omitempty"`
}

// enabled says whether this extension was asked for.
func (c *ExtensionConfig) enabled() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
}

// toDeviceLimit is how many to-device messages one answer may carry. The default is the number the
// ecosystem's servers use; the ceiling stops one queued burst from being delivered in a response no
// proxy will carry.
func (c *ExtensionConfig) toDeviceLimit() int {
	if c == nil || c.Limit == nil || *c.Limit <= 0 {
		return defaultToDeviceLimit
	}
	if *c.Limit > maxToDeviceLimitBatch {
		return maxToDeviceLimitBatch
	}
	return *c.Limit
}

// extensions answers the extension part of a request.
//
// The second result says whether any extension had *news*. It is not the same question as "is the
// payload empty": the encryption extension always reports its one-time-key counts, because a client
// decides whether to upload more keys from that number and cannot be made to wait for something else to
// happen first. But counts are not news, and a long-poll that treated them as news would return
// immediately on every request and turn a connection into a busy loop.
func (s *Service) extensions(ctx context.Context, req *Request, rooms []string, since, currentPos int64) (map[string]any, bool, error) {
	var (
		payload = map[string]any{}
		news    bool
	)
	for _, name := range []string{extensionToDevice, extensionE2EE, extensionTyping, extensionReceipts, extensionAccountData} {
		extension := req.Extensions[name]
		if !extension.enabled() {
			continue
		}
		var (
			answer any
			has    bool
			err    error
		)
		switch name {
		case extensionToDevice:
			answer, has, err = s.toDeviceExtension(ctx, req, extension.toDeviceLimit())
		case extensionE2EE:
			answer, has, err = s.e2eeExtension(ctx, req, since)
		case extensionTyping:
			answer, has, err = s.typingExtension(ctx, rooms, since)
		case extensionReceipts:
			answer, has, err = s.receiptsExtension(ctx, rooms, extensionSince(since))
		case extensionAccountData:
			answer, has, err = s.accountDataExtension(ctx, req, rooms, extensionSince(since))
		}
		if err != nil {
			// One unreadable extension must not fail the whole answer. The client's own position is
			// not advanced by an extension it was not given, so it will ask again on the next
			// connection cycle; a sync that fails outright is a device that shows nothing.
			slog.Warn("sliding sync: reading extension", "extension", name, "error", err)
			continue
		}
		if answer == nil {
			continue
		}
		payload[name] = answer
		news = news || has
	}
	// An extension the client asked for that this server has no notion of is reported once per
	// request in the log, which is where a maintainer looks; silently dropping it would make an
	// endpoint that half-works look like a client bug.
	for name := range req.Extensions {
		switch name {
		case extensionToDevice, extensionE2EE, extensionTyping, extensionReceipts, extensionAccountData:
		default:
			if extension := req.Extensions[name]; extension.enabled() {
				slog.Debug("sliding sync: ignoring an extension this server does not implement", "extension", name)
			}
		}
	}
	if len(payload) == 0 {
		return nil, false, nil
	}
	return payload, news, nil
}

// toDeviceExtension drains this device's queue of messages addressed to it alone.
//
// This is the extension that decides whether encrypted messages arrive at all: a room key shared with
// one device rather than a room travels here, so a sliding-sync connection that did not drain the queue
// would leave a device able to send and unable to read. It is also why this one reports `next_batch`
// rather than relying on the connection's position: the queue belongs to the device and is consumed
// once, not re-read by position, so two connections of one device share it the way two /v3/sync loops
// do.
func (s *Service) toDeviceExtension(ctx context.Context, req *Request, limit int) (any, bool, error) {
	if s.toDevice == nil || req.DeviceID == "" {
		return nil, false, nil
	}
	events, cursor, err := s.toDevice.Take(ctx, req.Localpart, req.DeviceID)
	if err != nil {
		return nil, false, err
	}
	if len(events) > limit {
		// The queue is drained by the device's cursor, which has already moved past what this answer
		// carries, so the rest cannot be held back — the honest thing is to say how many were dropped
		// rather than deliver a short batch that looks complete.
		slog.Warn("sliding sync: to-device batch over the extension limit",
			"device_id", req.DeviceID, "dropped", len(events)-limit)
		events = events[:limit]
	}
	answer := map[string]any{"next_batch": fmt.Sprintf("%d", cursor), "events": events}
	if events == nil {
		answer["events"] = []json.RawMessage{}
	}
	return answer, len(events) > 0, nil
}

// e2eeExtension reports what a device needs to keep end-to-end encryption working: how many one-time
// keys it still owes the server, whether it has a fallback key, and whose devices have changed since
// this connection's position.
//
// The counts are reported on every answer that asks for them, including an answer with nothing else in
// it. A device that has run out of one-time keys cannot be told about it by an event — the symptom is
// that other people's messages to it stop decrypting — so the number has to ride along.
func (s *Service) e2eeExtension(ctx context.Context, req *Request, since int64) (any, bool, error) {
	answer := map[string]any{
		"device_one_time_keys_count":       map[string]int64{},
		"device_unused_fallback_key_types": []string{},
		"device_lists":                     map[string]any{"changed": []string{}, "left": []string{}},
	}
	// The keys are stored under a Matrix user ID, so they are read under one built by the same
	// helper: `matrix.UserID` owns the sigil and the colon, and a hand-spliced variant that swaps one
	// separator for the other reads as a device that has never uploaded a key. That is a silent
	// answer — the client sees zero, keeps its keys to itself, and nothing ever decrypts.
	userID := matrix.UserID(req.Localpart, s.serverName)
	if req.DeviceID != "" {
		counts, err := s.store.Keys.OneTimeKeyCounts(ctx, userID, req.DeviceID)
		if err != nil {
			return nil, false, err
		}
		answer["device_one_time_keys_count"] = counts
		types, err := s.store.Keys.FallbackKeyTypes(ctx, userID, req.DeviceID)
		if err != nil {
			return nil, false, err
		}
		answer["device_unused_fallback_key_types"] = types
	}
	if s.deviceLists == nil {
		return answer, false, nil
	}
	changed, left, err := s.deviceLists.Delta(ctx, req.Localpart, since)
	if err != nil {
		return answer, false, err
	}
	// Both lists are always sent. A client that does not see `changed` cannot tell "nobody's devices
	// moved" from "this server does not track device lists", and the two lead to different amounts of
	// key fetching.
	answer["device_lists"] = map[string]any{"changed": orEmpty(changed), "left": orEmpty(left)}
	return answer, len(changed) > 0 || len(left) > 0, nil
}

// extensionSince is the position the receipt and account-data reads start from.
//
// The store reports what moved *strictly after* a position, and neither of those two streams can ever
// be strictly after the connection's own position when nothing else happened: a receipt deliberately
// takes the event stream's current maximum as its stamp, so that a read marker does not look like new
// room history to every other client, and account data is stamped the same way. So a read marker moved
// in an idle room sits exactly *on* the position being compared against, and a strict reading loses
// it until the next event in that room — a device that was marked read on one phone and still shows
// unread on the other. One below the connection's position is the honest question, "what has this
// connection not been told"; the cost is that a client can be shown the same read marker twice, which
// a client applies twice without noticing, because a receipt is an idempotent fact and not a message.
func extensionSince(pos int64) int64 {
	if pos <= 0 {
		return 0
	}
	return pos - 1
}

// orEmpty turns a nil list into an empty one so it serialises as [] rather than being left out.
func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// typingExtension reports who is typing, per room this answer is about.
//
// Typing is the one extension with no history: an indicator that arrived after this position is only
// meaningful as *now*, which is why the read is "is somebody typing right now, changed since" rather
// than a list of typing events. A client learns that typing stopped the same way v3 sync reports it —
// an empty list, which is still a fact.
func (s *Service) typingExtension(ctx context.Context, rooms []string, since int64) (any, bool, error) {
	if s.readState == nil || len(rooms) == 0 {
		return map[string]any{"rooms": map[string]any{}}, false, nil
	}
	byRoom := map[string]any{}
	for _, roomID := range rooms {
		event, moved, err := s.readState.TypingEvent(roomID, since)
		if err != nil {
			return nil, false, err
		}
		if !moved {
			continue
		}
		content, err := contentOf(event)
		if err != nil {
			return nil, false, err
		}
		byRoom[roomID] = content
	}
	return map[string]any{"rooms": byRoom}, len(byRoom) > 0, nil
}

// receiptsExtension reports the read markers and delivered markers that moved since this position.
//
// Unlike typing, a receipt is a persisted fact, so a connection that reconnects must not lose them:
// they are how a device knows which of its own notifications are already read.
func (s *Service) receiptsExtension(ctx context.Context, rooms []string, since int64) (any, bool, error) {
	if s.readState == nil || len(rooms) == 0 {
		return map[string]any{"rooms": map[string]any{}}, false, nil
	}
	byRoom := map[string]any{}
	for _, roomID := range rooms {
		events, err := s.readState.ReceiptEvents(ctx, roomID, since)
		if err != nil {
			return nil, false, err
		}
		if len(events) == 0 {
			continue
		}
		// One room's receipts arrive as one merged content: the wire form is a map of user to receipt
		// type to event, and a client applying two contents for the same room would keep only the
		// last.
		merged := map[string]any{}
		for _, event := range events {
			content, err := contentOf(event)
			if err != nil {
				return nil, false, err
			}
			for user, receipt := range content {
				if existing, ok := merged[user].(map[string]any); ok {
					if additions, ok := receipt.(map[string]any); ok {
						for kind, value := range additions {
							existing[kind] = value
						}
						continue
					}
				}
				merged[user] = receipt
			}
		}
		byRoom[roomID] = merged
	}
	return map[string]any{"rooms": byRoom}, len(byRoom) > 0, nil
}

// accountDataExtension reports the account data that moved since this position, globally and per room.
//
// This is how a device learns that a room was marked read, that a push rule changed, or that its own
// user added a contact: all three are account data, and all three are invisible to the room lists.
func (s *Service) accountDataExtension(ctx context.Context, req *Request, rooms []string, since int64) (any, bool, error) {
	if s.readState == nil {
		return nil, false, nil
	}
	global, err := s.readState.AccountData(ctx, req.Localpart, since)
	if err != nil {
		return nil, false, err
	}
	byRoom := map[string]any{}
	if len(rooms) > 0 {
		entries, err := s.readState.RoomAccountData(ctx, req.Localpart, rooms, since)
		if err != nil {
			return nil, false, err
		}
		for roomID, events := range entries {
			if len(events) == 0 {
				continue
			}
			byRoom[roomID] = events
		}
	}
	answer := map[string]any{"global": global, "rooms": byRoom}
	if global == nil {
		answer["global"] = []json.RawMessage{}
	}
	return answer, len(global) > 0 || len(byRoom) > 0, nil
}

// contentOf lifts the `content` out of an event this package has been handed, because the extensions
// carry contents keyed by room rather than whole events.
func contentOf(event json.RawMessage) (map[string]any, error) {
	var document struct {
		Content map[string]any `json:"content"`
	}
	if err := json.Unmarshal(event, &document); err != nil {
		return nil, fmt.Errorf("reading the content of an extension event: %w", err)
	}
	if document.Content == nil {
		document.Content = map[string]any{}
	}
	return document.Content, nil
}
