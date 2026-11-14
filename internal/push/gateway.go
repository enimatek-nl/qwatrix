package push

// The wire format: what this server says to a push gateway, and what it listens back for.
//
// The shape is the one the specification documents — one request per event, with a `devices` array
// naming the devices that should be told about it — rather than the batched `event_id_subset` shape
// that some servers and gateways speak as well. Both are in the world; the specified one is the one
// this server can be right about without knowing what a particular gateway happens to implement,
// and a homeserver that guesses at a gateway's dialect is a homeserver whose notifications depend
// on something it cannot see.
//
// Two choices here are visible from a phone, so they are worth naming:
//
//   - A pusher whose data says `event_id_only` is sent an event ID and nothing else. That is what
//     the flag means: the app has the keys and the token to fetch the event itself, so message text
//     does not pass through a gateway that could log it. It is also what the mobile apps in widest
//     use ask for.
//   - A gateway's answer can reject a push key, and a rejected key is not retried. An upstream
//     provider rejects a key when the app was uninstalled or the token expired; carrying on means
//     this server keeps posting a stranger's push key to a gateway forever, and that key is
//     information about a device its owner no longer has.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/pushrules"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// gatewayRequest is the body of POST <gateway URL>.
type gatewayRequest struct {
	Notification gatewayNotification `json:"notification"`
}

// gatewayNotification is one event, carrying as much of it as these pushers are allowed to see.
type gatewayNotification struct {
	// EventID is what a gateway uses to suppress a duplicate alert when this request is retried.
	EventID string `json:"event_id,omitempty"`
	RoomID  string `json:"room_id,omitempty"`
	// Type, Sender and Content are omitted for an `event_id_only` pusher, which asked not to
	// receive them.
	Type    string          `json:"type,omitempty"`
	Sender  string          `json:"sender,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// Counts is the recipient's unread situation. It is advisory — a badge a gateway may show —
	// and is omitted when this server cannot state it exactly.
	Counts *gatewayCounts `json:"counts,omitempty"`
	// UserIsTarget is true when the event is *about* the recipient: the member event that is an
	// invite, say. It is what lets a gateway read out "Invitation to #room" instead of "New
	// message".
	UserIsTarget bool `json:"user_is_target,omitempty"`
	// Devices is required. A notification with no device to notify is a message to nobody.
	Devices []gatewayDevice `json:"devices"`
}

// gatewayCounts holds the recipient's unacknowledged totals.
type gatewayCounts struct {
	Unread int64 `json:"unread"`
}

// gatewayDevice is one pusher, as the gateway knows it.
type gatewayDevice struct {
	AppID   string `json:"app_id"`
	PushKey string `json:"pushkey"`
	// PushKeyTS is when the pusher was last registered, in seconds. A gateway uses it to tell a
	// refreshed key from a stale one.
	PushKeyTS int64 `json:"pushkey_ts,omitempty"`
	// Tweaks is how the recipient's own push rule said to present this: a sound, a highlight.
	// They travel per device rather than per notification because two pushers of one account can
	// be governed by different rules — the same person, a work phone and a personal one.
	Tweaks            map[string]any `json:"tweaks,omitempty"`
	AppDisplayName    string         `json:"app_display_name,omitempty"`
	DeviceDisplayName string         `json:"device_display_name,omitempty"`
	Lang              string         `json:"lang,omitempty"`
	ProfileTag        string         `json:"profile_tag,omitempty"`
	// Data is the pusher's own data, less the URL this very request is addressed to.
	Data map[string]any `json:"data"`
}

// gatewayAnswer is what a gateway replies.
type gatewayAnswer struct {
	// Rejected names the push keys this gateway will not accept. It is not necessarily about the
	// notification just sent: the specification says a gateway may report the failure of an
	// earlier request, which is why a rejection removes the pusher rather than merely failing the
	// delivery in front of it.
	Rejected []string `json:"rejected"`
}

// delivery is one request: one event, one account, one gateway, one format.
type delivery struct {
	target string
	// rows are the pending deliveries this request answers — usually one per device.
	rows  []store.PendingPush
	event *gatewayNotification
	// tweaks belongs beside the event rather than inside it: it is derived from the rule that
	// decided, and travels to each device of this group, but it is not a field of the
	// notification the specification describes.
	tweaks map[string]any
}

// DeliverOnce attempts the deliveries that are due. It reports how many events a gateway accepted.
func (s *Service) DeliverOnce(ctx context.Context) (int, error) {
	if s.deliveryOff {
		return 0, nil
	}
	// A gateway that never answers must not accumulate debt forever.
	if dropped, err := s.store.Push.DropOldPushes(ctx, maxDeliveryAttempts); err != nil {
		return 0, err
	} else if dropped > 0 {
		slog.Warn("push: gave up on deliveries whose gateway never answered",
			"deliveries", dropped, "attempts", maxDeliveryAttempts)
	}
	now := time.Now()
	due, err := s.store.Push.DuePushes(ctx, now.UnixMilli(), deliverBatch)
	if err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}
	byPusher, err := s.pusherIndex(ctx)
	if err != nil {
		return 0, err
	}
	groups, err := s.group(ctx, due, byPusher, newUnreadCounter(s))
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, group := range groups {
		answer, err := s.send(ctx, group)
		if err != nil {
			s.scheduleRetry(ctx, group.rows, now, err)
			continue
		}
		if len(answer.Rejected) > 0 {
			s.dropRejected(ctx, group.rows, answer.Rejected)
		}
		// Rows whose push key was rejected left with the pusher. Whatever is left of the group
		// the gateway has taken, and the debt is paid.
		if err := s.store.Push.DeletePushes(ctx, idsOf(group.rows)); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// group builds the requests this batch adds up to, keyed by (account, event, gateway URL, format).
//
// Grouping is what lets several devices of one account behind one gateway share a request, which is
// what the specification's `devices` array is for. Splitting on the format is what keeps an
// `event_id_only` pusher from being sent message text because its sibling pusher did not mind.
func (s *Service) group(ctx context.Context, due []store.PendingPush,
	byPusher map[string]store.Pusher, counts *unreadCounter) ([]delivery, error) {
	var order []string
	groups := map[string]*delivery{}
	for _, row := range due {
		pusher, ok := byPusher[pusherKey(row.Localpart, row.AppID, row.PushKey)]
		if !ok {
			// The pusher died between queueing and delivering — a signed-out session, a
			// deactivated account. Nothing is owed for a device that no longer exists.
			if err := s.store.Push.DeletePushes(ctx, []int64{row.ID}); err != nil {
				return nil, err
			}
			continue
		}
		target, eventIDOnly, err := gatewayOf(pusher)
		if err != nil {
			// A pusher whose URL this server will not post to can never deliver, and retrying
			// it would be a loop with extra steps. Say why, then stop carrying it.
			slog.Warn("push: dropping a delivery for a pusher with an unusable gateway",
				"localpart", pusher.Localpart, "app_id", pusher.AppID, "error", err)
			if _, err := s.store.Push.DeletePusher(ctx, pusher.Localpart, pusher.AppID,
				pusher.PushKey); err != nil {
				return nil, err
			}
			if err := s.store.Push.DeletePushesOfPusher(ctx, pusher.Localpart, pusher.AppID,
				pusher.PushKey); err != nil {
				return nil, err
			}
			continue
		}
		groupKey := strings.Join([]string{row.Localpart, row.EventID, target,
			strconv.FormatBool(eventIDOnly)}, "\x00")
		group, known := groups[groupKey]
		if !known {
			notification, tweaks, err := s.notificationFor(ctx, row, eventIDOnly, counts)
			if err != nil {
				return nil, err
			}
			group = &delivery{target: target, rows: []store.PendingPush{row}, event: notification,
				tweaks: tweaks}
			groups[groupKey] = group
			order = append(order, groupKey)
			s.appendDevice(group, pusher)
			continue
		}
		group.rows = append(group.rows, row)
		s.appendDevice(group, pusher)
	}
	out := make([]delivery, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	return out, nil
}

// appendDevice adds one pusher to a group's request.
func (s *Service) appendDevice(group *delivery, pusher store.Pusher) {
	group.event.Devices = append(group.event.Devices, deviceOf(pusher, group.tweaks))
}

// notificationFor assembles the event part of a gateway request.
func (s *Service) notificationFor(ctx context.Context, row store.PendingPush, eventIDOnly bool,
	counts *unreadCounter) (*gatewayNotification, map[string]any, error) {
	notification := &gatewayNotification{EventID: row.EventID, RoomID: row.RoomID}
	if count, ok := counts.unread(ctx, row.Localpart); ok {
		notification.Counts = &gatewayCounts{Unread: count}
	}
	// The rule that decided this is worth a notification also said how to present it. It is read
	// from the record rather than re-evaluated, so that a rule edited after the event arrived
	// does not rewrite what the user was told to expect.
	var tweaks map[string]any
	recorded, err := s.store.Push.GetNotification(ctx, row.Localpart, row.EventID)
	if err != nil && !store.IsNotFound(err) {
		return nil, nil, err
	}
	if recorded != nil {
		tweaks = pushrules.TweaksOf(decodeActions(recorded.Actions))
	}
	if eventIDOnly {
		// The pusher asked not to be told what the message says. Omit it — sender, type and
		// content alike — rather than including them because they were cheap to add.
		return notification, tweaks, nil
	}
	stored, err := s.store.Events.ByEventID(ctx, row.EventID)
	if err != nil {
		if store.IsNotFound(err) {
			// The event is gone: purged, or redacted to nothing. A notification about an
			// event this server no longer has is still worth its event ID — the app can ask,
			// find nothing, and consider itself caught up.
			return notification, tweaks, nil
		}
		return nil, nil, err
	}
	notification.Type = stored.Type
	notification.Sender = stored.Sender
	if content := contentOf(stored.JSON); len(content) > 0 {
		if raw, err := json.Marshal(content); err == nil {
			notification.Content = raw
		}
	}
	if stored.StateKey != nil {
		notification.UserIsTarget = *stored.StateKey == "@"+row.Localpart+":"+s.serverName
	}
	return notification, tweaks, nil
}

// deviceOf renders one pusher as the gateway knows it.
func deviceOf(pusher store.Pusher, tweaks map[string]any) gatewayDevice {
	device := gatewayDevice{
		AppID:             pusher.AppID,
		PushKey:           pusher.PushKey,
		PushKeyTS:         pusher.UpdatedMS / 1000,
		Tweaks:            tweaks,
		AppDisplayName:    pusher.AppDisplayName,
		DeviceDisplayName: pusher.DeviceDisplayName,
		Lang:              pusher.Lang,
		ProfileTag:        pusher.ProfileTag,
		// Never nil: the specification's `data` object is an object, and an app that reads a
		// key out of it should not have to survive it being JSON null first.
		Data: map[string]any{},
	}
	if pusher.Data == "" {
		return device
	}
	var supplied map[string]any
	if err := json.Unmarshal([]byte(pusher.Data), &supplied); err != nil {
		return device
	}
	for key, value := range supplied {
		// The gateway is being addressed at that URL right now; the specification asks for the
		// data dictionary "minus the url key".
		if key == "url" {
			continue
		}
		device.Data[key] = value
	}
	return device
}

// send posts one notification to one gateway.
func (s *Service) send(ctx context.Context, group delivery) (*gatewayAnswer, error) {
	body, err := json.Marshal(gatewayRequest{Notification: *group.event})
	if err != nil {
		return nil, fmt.Errorf("encoding the notification for %s: %w", group.event.EventID, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, notifyGatewayTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, group.target,
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("preparing a request to a push gateway: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", s.userAgent)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("reaching the push gateway: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, gatewayResponseLimit))
		_ = response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("the push gateway answered %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, gatewayResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("reading the push gateway's answer: %w", err)
	}
	answer := &gatewayAnswer{}
	if len(bytes.TrimSpace(payload)) == 0 {
		// A gateway that answers nothing has accepted everything. The specification requires a
		// `rejected` list, but a proxy in front of a gateway is known to strip bodies, and
		// retrying a delivery a gateway already took means telling the user twice.
		return answer, nil
	}
	if err := json.Unmarshal(payload, answer); err != nil {
		slog.Warn("push: a push gateway answered something that is not JSON; accepting the delivery",
			"body", truncate(string(payload), 200))
	}
	return answer, nil
}

// scheduleRetry records that a delivery did not happen, and when to try again.
//
// It deliberately survives its caller's context going away: a delivery whose retry was never
// recorded because the request was cancelled is a delivery that is due again immediately, which is
// how a down gateway turns into a busy loop.
func (s *Service) scheduleRetry(ctx context.Context, rows []store.PendingPush, now time.Time,
	reason error) {
	detached := context.WithoutCancel(ctx)
	for _, row := range rows {
		delay := retryDelay(row.Attempts)
		if row.Attempts+1 >= maxDeliveryAttempts {
			slog.Warn("push: giving up on a delivery", "event_id", row.EventID,
				"app_id", row.AppID, "attempts", row.Attempts+1, "error", reason)
		}
		if err := s.store.Push.RetryPush(detached, row.ID, now.Add(delay).UnixMilli(),
			reason.Error()); err != nil {
			slog.Warn("push: could not record a failed delivery", "error", err)
		}
	}
}

// retryDelay is exponential backoff with a ceiling: two seconds, four, eight … up to five minutes.
//
// The specification asks for exponential backoff and no more precision than that, and the shape that
// matters is "the second retry is soon, the fortieth is never". An app reinstall can leave a gateway
// unreachable for hours, and a server that retried every few seconds would spend its capacity on a
// notification the user has already seen by opening the app.
func retryDelay(attempts int64) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 8 {
		attempts = 8
	}
	delay := time.Duration(2<<attempts) * time.Second
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}

// dropRejected removes the pushers a gateway said it will not accept, and their queued deliveries.
func (s *Service) dropRejected(ctx context.Context, rows []store.PendingPush, rejected []string) {
	want := make(map[string]bool, len(rejected))
	for _, key := range rejected {
		want[key] = true
	}
	done := map[string]bool{}
	for _, row := range rows {
		key := pusherKey(row.Localpart, row.AppID, row.PushKey)
		if !want[row.PushKey] || done[key] {
			continue
		}
		done[key] = true
		detached := context.WithoutCancel(ctx)
		if err := s.store.Push.DeletePushesOfPusher(detached, row.Localpart, row.AppID,
			row.PushKey); err != nil {
			slog.Warn("push: could not drop the deliveries of a rejected push key", "error", err)
		}
		removed, err := s.store.Push.DeletePusher(detached, row.Localpart, row.AppID, row.PushKey)
		if err != nil {
			slog.Warn("push: could not remove a rejected push key", "error", err)
			continue
		}
		if removed {
			slog.Info("push: removed a push key the gateway rejected",
				"localpart", row.Localpart, "app_id", row.AppID)
		}
	}
}

// pusherIndex maps (account, app, push key) to the live pusher.
func (s *Service) pusherIndex(ctx context.Context) (map[string]store.Pusher, error) {
	all, err := s.store.Push.ListAllPushers(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[string]store.Pusher, len(all))
	for _, pusher := range all {
		index[pusherKey(pusher.Localpart, pusher.AppID, pusher.PushKey)] = pusher
	}
	return index, nil
}

// gatewayOf reads where a pusher wants to be told, and how much it wants to be told.
//
// The URL is checked rather than trusted because it arrived from a client: this server is about to
// make a request to it, and a server that fetches whatever a user typed is a server-side request
// forgery with the added feature that the attacker chose the scheme. So: `http` or `https` only, a
// host that is there, and no credentials smuggled into the URL — those are the two ways a URL like
// that gets used, and refusing them costs nothing legitimate.
//
// Loopback and private addresses are allowed, and that is deliberate: a self-hosted push gateway or
// a UnifiedPush distributor on the same network as the homeserver is an ordinary deployment, and
// both are commonly plain http because they are on a LAN. A deployment that wants a stricter policy
// enforces it where that belongs — on the network.
func gatewayOf(pusher store.Pusher) (target string, eventIDOnly bool, err error) {
	if pusher.Data == "" {
		return "", false, errNoGateway
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(pusher.Data), &data); err != nil {
		return "", false, fmt.Errorf("the pusher's data does not decode: %w", err)
	}
	raw, _ := data["url"].(string)
	if strings.TrimSpace(raw) == "" {
		return "", false, errNoGateway
	}
	if err := validateGatewayURL(raw); err != nil {
		return "", false, err
	}
	if flag, ok := data["event_id_only"].(bool); ok && flag {
		eventIDOnly = true
	}
	if format, _ := data["format"].(string); format == "event_id_only" {
		eventIDOnly = true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("the pusher's gateway URL does not parse: %w", err)
	}
	return parsed.String(), eventIDOnly, nil
}

// validateGatewayURL decides whether this server will make a request to a URL a client supplied.
//
// It is called when the pusher is registered, not only when a delivery is attempted, so that the
// client that made the mistake hears about it in the response to the request that made it. It is
// also the reason an unusable scheme cannot be smuggled in through `append`, which is checked
// against the merged result.
func validateGatewayURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("a push gateway URL must parse: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("a push gateway URL must be http or https, not %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("a push gateway URL must name a host")
	}
	if parsed.User != nil {
		return errors.New("a push gateway URL must not carry credentials")
	}
	return nil
}

// pusherKey is the identity of a pusher, as a map key.
func pusherKey(localpart, appID, pushKey string) string {
	return strings.Join([]string{localpart, appID, pushKey}, "\x00")
}

// decodeActions reads a stored notification's action list.
func decodeActions(raw string) []any {
	if raw == "" {
		return nil
	}
	var actions []any
	if err := json.Unmarshal([]byte(raw), &actions); err != nil {
		return nil
	}
	return actions
}

// truncate shortens a gateway's body for a log line.
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
