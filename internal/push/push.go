// Package push delivers the fact that something happened to a device that is not asking.
//
// Push rules answer "is this worth interrupting this person for". This package is what does
// something about the answer: it walks the event stream, asks the rules for each person who might
// care, remembers the events that were worth telling them about, and pays the debt each registered
// pusher is owed by telling their gateway.
//
// Three decisions shape the whole package:
//
//   - It reads the event stream from a saved position rather than being called from the write path.
//     A notification is a consequence of an event, not part of it: making the write wait for an HTTP
//     request to a stranger's push gateway would let somebody else's outage slow down this server's
//     users sending messages to each other, and would mean an event whose delivery to a phone failed
//     had to roll back. It also means the position survives a restart, so events received while this
//     server was down are still notified for.
//   - The judgement is recorded even when there is nobody to tell. `GET /notifications` lists events
//     that were worth interrupting somebody for, and it would be a strange endpoint if it only knew
//     things about accounts that happened to register a device for it.
//   - A delivery is a debt, kept in the database until the gateway has confirmed it. The
//     alternative — best-effort at the moment the event arrives — loses notifications to a push
//     gateway being briefly down, and a notification is exactly the kind of message whose absence
//     the user cannot see, so it is the kind that must not be lost quietly.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/pushrules"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

const (
	// cursorName names this package's place in the event stream. It is a name rather than a
	// bare number so that a second consumer of the same stream — e-mail digests, say — starts
	// from its own position rather than inheriting this one's and quietly skipping everything
	// in between.
	cursorName = "push"

	// evaluateBatch is how many events one pass considers. It bounds how long a pass can hold
	// the reader, which matters because the pass runs in the same process as the requests that
	// answer users.
	evaluateBatch = 200

	// deliverBatch is how many deliveries one pass attempts. Low on purpose: a push gateway
	// that is down should cost this server a handful of timeouts per tick, not a thousand.
	deliverBatch = 40

	// maxDeliveryAttempts is when a delivery stops being owed. The specification asks for
	// exponential backoff and "a reasonable amount of time", and an attempt schedule that
	// doubles from two seconds reaches about an hour of trying at this count. Past that, a
	// notification whose gateway has not answered once is not going to, and the user's phone
	// will catch up from /sync when it is next open.
	maxDeliveryAttempts = 12

	// unreadScanLimit bounds what an unread count is summed over. The count is *omitted* when
	// the scan hits the bound rather than reporting the number it reached, because "you have no
	// unread messages" is worse than silence: the first tells the user to stop looking.
	unreadScanLimit = 1000

	// notifyGatewayTimeout bounds one request to one push gateway. A gateway is somebody
	// else's computer, and this server has no obligation to wait for it.
	notifyGatewayTimeout = 10 * time.Second

	// gatewayResponseLimit caps how much of a gateway's answer is read. The answer is a small
	// JSON object; anything more is either a misconfiguration or something to be suspicious of.
	gatewayResponseLimit = 64 << 10

	// defaultNotificationPageSize and maxNotificationPageSize are the bounds of
	// GET /notifications' paging. The maximum is a limit on response size rather than on
	// politeness: each entry carries an event, and an unbounded page is a way for one client to
	// ask this server to serialise a room's worth of history in one request.
	defaultNotificationPageSize = 50
	maxNotificationPageSize     = 1000
)

// nowMS is the clock, in the unit every timestamp in the store is kept.
func nowMS() int64 { return time.Now().UnixMilli() }

// Classifier decides what one event is worth to one person. It is satisfied by
// [*pushrules.Service], and injected so that this package does not own the rule language — only
// the consequence of what it says.
type Classifier interface {
	Evaluate(ctx context.Context, localpart string, ev pushrules.Event) (*pushrules.Evaluation, error)
}

// Reader answers how far a person has read in a room. It is satisfied by
// [*readstate.Service], and asked because whether a notification has been read is a question about
// receipts, and receipts belong to the service that holds them.
type Reader interface {
	ReadPositions(ctx context.Context, roomID, localpart string) (int64, map[string]int64, error)
}

// Options configures a [Service].
type Options struct {
	// ServerName is this server's name: it is the `org_id` a gateway uses to scope its dedupe,
	// and it is what turns a push key from another server's deployment into a push key from
	// another server's deployment.
	ServerName string
	// Client is the HTTP client used for gateway calls. Optional; a client with a timeout is
	// built when it is nil. Tests substitute one whose transport refuses to leave the process.
	Client *http.Client
	// Tick is the fallback interval between passes, used when there is no notifier to wake this
	// service. It is the retry interval for a gateway that was away.
	Tick time.Duration
	// UserAgent identifies this server to push gateways. Optional; it defaults to the server
	// name, because a gateway operator debugging a flood of requests wants to know who sent them.
	UserAgent string
}

// Service is the push notifier.
type Service struct {
	store      *store.Store
	rules      Classifier
	reader     Reader
	client     *http.Client
	changes    *notifier.Notifier
	serverName string
	userAgent  string
	tick       time.Duration
	// deliveryOff belongs to a server that has decided not to make outbound requests on
	// behalf of its clients. Notifications are still recorded — `GET /notifications` and the
	// badges keep working — and nothing queues up behind a worker that will never run.
	deliveryOff bool
}

// New builds the service.
func New(st *store.Store, opts Options) *Service {
	tick := opts.Tick
	if tick <= 0 {
		tick = 5 * time.Second
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: notifyGatewayTimeout}
	}
	agent := opts.UserAgent
	if strings.TrimSpace(agent) == "" {
		agent = "qwatrix"
		if opts.ServerName != "" {
			agent += " (" + opts.ServerName + ")"
		}
	}
	return &Service{
		store:      st,
		client:     client,
		serverName: opts.ServerName,
		userAgent:  agent,
		tick:       tick,
	}
}

// WithClassifier installs the push-rule evaluator and returns the service for chaining.
//
// Without one nothing notifies, which is the right failure: the alternative is delivering every
// event to every device, and a user who has carefully muted a room would hear it anyway.
func (s *Service) WithClassifier(rules Classifier) *Service {
	s.rules = rules
	return s
}

// WithReader installs the service that knows what a person has read, for the `unread` flag and the
// unread counts. Without them a notification is reported as unread forever, which is at least an
// honest failure of a count, rather than marking things read that are not.
func (s *Service) WithReader(reader Reader) *Service {
	s.reader = reader
	return s
}

// WithWakeup lets an event written a moment ago be notified for now rather than at the next tick.
//
// The tick stays: it is what retries a gateway that was away. But a tick is a latency floor, and
// "notification arrives after the app's own /sync does" is a floor a user can feel.
func (s *Service) WithWakeup(changes *notifier.Notifier) *Service {
	s.changes = changes
	return s
}

// WithoutDelivery stops this server making outbound requests to push gateways.
//
// A pusher's URL is supplied by whoever registered it, so push delivery is, by design, a server
// making requests to addresses its users choose. Most deployments want that. One that does not — an
// air-gapped installation, a network policy that forbids egress — should be able to say so and have
// everything except the outbound request keep working, which is what this is for.
func (s *Service) WithoutDelivery() *Service {
	s.deliveryOff = true
	return s
}

// Run passes until ctx is cancelled: evaluate, deliver, repeat when something was written or when
// the tick says a gateway may have recovered.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	var generation uint64
	if s.changes != nil {
		generation = s.changes.Generation()
	}
	for {
		if !s.wait(ctx, ticker, &generation) {
			return nil
		}
		if _, err := s.DrainOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			slog.Warn("push: the delivery pass failed", "error", err)
		}
	}
}

// wait parks the loop until there is a reason to run, and reports whether the loop should continue.
func (s *Service) wait(ctx context.Context, ticker *time.Ticker, generation *uint64) bool {
	if s.changes == nil {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			return true
		}
	}
	if s.changes.WaitForChange(ctx, *generation, s.tick) {
		*generation = s.changes.Generation()
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	// The wait timed out, which is the same reason a tick would carry. Do not run twice for it.
	select {
	case <-ticker.C:
	default:
	}
	return true
}

// DrainOnce evaluates new events and attempts the deliveries that are due.
//
// A production server does not call this — [Service.Run] does, on the tick and on wake-ups — but a
// test cannot assert against a goroutine it has no influence over, and "the notification arrived
// eventually" is not something anybody wants to debug.
func (s *Service) DrainOnce(ctx context.Context) (int, error) {
	evaluated, err := s.EvaluateOnce(ctx)
	if err != nil {
		return evaluated, err
	}
	delivered, err := s.DeliverOnce(ctx)
	return evaluated + delivered, err
}

// EvaluateOnce walks the event stream from where the last pass stopped, recording what was worth
// notifying people about and queueing the deliveries that follow from it. It reports how many
// events it considered.
func (s *Service) EvaluateOnce(ctx context.Context) (int, error) {
	if s.rules == nil {
		return 0, nil
	}
	// A pusher whose session was revoked is not a pusher; see the `livePushers` condition in the
	// store. Pruning here rather than never means the table does not accumulate a row for every
	// phone a user has ever owned and then signed out of.
	if pruned, err := s.store.Push.PrunePushers(ctx); err != nil {
		return 0, err
	} else if pruned > 0 {
		slog.Debug("push: removed pushers whose session no longer exists", "pushers", pruned)
	}
	position, err := s.store.Push.Cursor(ctx, cursorName)
	if err != nil {
		return 0, err
	}
	rows, err := s.store.Events.Range(ctx, store.EventQuery{After: position, Limit: evaluateBatch})
	if err != nil {
		return 0, fmt.Errorf("reading events to notify for: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	// Load the pushers once for the whole batch rather than per event. A server with a handful of
	// users has a handful of rows; the query is not cheaper per event, and it is certainly not
	// more correct.
	pushers, err := s.livePushersByAccount(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixMilli()
	considered := 0
	last := position
	for _, row := range rows {
		if err := s.consider(ctx, row, pushers, now); err != nil {
			// One event that cannot be notified for must not stop the stream, and must not
			// be skipped over either: the cursor does not advance past it, so the next pass
			// tries it again, and a transient database error heals.
			slog.Warn("push: could not evaluate an event", "event_id", row.EventID, "error", err)
			break
		}
		considered++
		last = row.ID
	}
	if err := s.store.Push.SetCursor(ctx, cursorName, last); err != nil {
		return considered, err
	}
	return considered, nil
}

// recipient is one person an event might be worth interrupting, and the point in the stream from
// which it is.
type recipient struct {
	localpart string
	// joinedAt is when this person became somebody the room talks to. It is the second half of
	// "new to them": an event this server received before a user joined is that room's history,
	// however new it is to this server, and a user who joined a room yesterday must not be woken
	// up by a phone buzzing about the room's creation.
	joinedAt int64
}

// consider decides what one event is worth to the people in its room.
func (s *Service) consider(ctx context.Context, row store.EventRow,
	pushers map[string][]store.Pusher, nowMS int64) error {
	recipients, err := s.recipientsOf(ctx, row)
	if err != nil {
		return err
	}
	if len(recipients) == 0 {
		return nil
	}
	view := pushrules.Event{
		RoomID:  row.RoomID,
		Type:    row.Type,
		Sender:  row.Sender,
		Content: contentOf(row.JSON),
	}
	if row.StateKey != nil {
		key := *row.StateKey
		view.StateKey = &key
	}
	var queue []store.PendingPush
	for _, person := range recipients {
		if row.ID <= person.joinedAt && !s.namesThem(row, person.localpart) {
			// History, not news. The exception is the event that is *about* them — the
			// membership event that invited them, or kicked them — which is the one piece of
			// the past they have a right to be told about, and it is what `m.rule.invite_for_me`
			// exists to match.
			continue
		}
		evaluation, err := s.rules.Evaluate(ctx, person.localpart, view)
		if err != nil {
			return err
		}
		if evaluation == nil || !evaluation.Notifies {
			// Either nothing matched, or what matched said `dont_notify`. Both mean the same
			// thing to a pusher: nothing to send.
			continue
		}
		actions, err := json.Marshal(evaluation.Actions)
		if err != nil {
			return fmt.Errorf("encoding the actions of %q: %w", row.EventID, err)
		}
		threadID := ""
		if row.RelType == "m.thread" {
			threadID = row.RelatesToEventID
		}
		if err := s.store.Push.AddNotification(ctx, &store.Notification{
			Localpart: person.localpart, EventID: row.EventID, RoomID: row.RoomID,
			Type: row.Type, Sender: row.Sender, ThreadID: threadID,
			Actions: string(actions), Position: row.ID, CreatedMS: nowMS,
		}); err != nil {
			return err
		}
		for _, pusher := range pushers[person.localpart] {
			// A delivery is queued per pusher rather than per account because a gateway
			// rejection names a push key: one dead phone must not stop the other one being
			// told.
			queue = append(queue, store.PendingPush{
				Localpart: pusher.Localpart, AppID: pusher.AppID, PushKey: pusher.PushKey,
				EventID: row.EventID, RoomID: row.RoomID, NextAttemptMS: nowMS, CreatedMS: nowMS,
			})
		}
	}
	if len(queue) == 0 {
		return nil
	}
	return s.store.Push.QueuePushes(ctx, queue)
}

// namesThem reports whether an event is the membership event about this person.
func (s *Service) namesThem(row store.EventRow, localpart string) bool {
	if row.Type != "m.room.member" || row.StateKey == nil {
		return false
	}
	return *row.StateKey == "@"+localpart+":"+s.serverName
}

// recipientsOf lists the local users an event might be worth interrupting, and when each of them
// stopped being able to be surprised by this room.
//
// Joined and invited people only. Somebody who left is not waiting to be told about a room they
// left, and somebody who was banned has been told, in the most direct way the room has, to stop
// coming. The sender is excluded by the rule evaluator — a person's own words are not news to them —
// rather than here, so that this function stays about membership and not about judgement.
func (s *Service) recipientsOf(ctx context.Context, row store.EventRow) ([]recipient, error) {
	members, err := s.store.Memberships.List(ctx, row.RoomID,
		[]string{store.MembershipJoin, store.MembershipInvite}, nil)
	if err != nil {
		return nil, fmt.Errorf("reading who to notify about %s: %w", row.RoomID, err)
	}
	people := make([]recipient, 0, len(members))
	for _, member := range members {
		// A membership row is keyed by local part for this server's own users and by the full
		// user ID for everybody else's — see [store.Membership]. Only this server's own users can
		// have a pusher here, and only their rules are worth evaluating.
		if strings.Contains(member.Localpart, ":") {
			continue
		}
		people = append(people, recipient{localpart: member.Localpart, joinedAt: member.StreamPos})
	}
	return people, nil
}

// livePushersByAccount groups every pusher by the account it belongs to.
func (s *Service) livePushersByAccount(ctx context.Context) (map[string][]store.Pusher, error) {
	all, err := s.store.Push.ListAllPushers(ctx)
	if err != nil {
		return nil, err
	}
	byAccount := map[string][]store.Pusher{}
	for _, pusher := range all {
		byAccount[pusher.Localpart] = append(byAccount[pusher.Localpart], pusher)
	}
	return byAccount, nil
}

// contentOf decodes an event's content for the rule language.
//
// A stored event whose content does not decode is not a notification: it is a database row nobody
// can interpret, and the rules have nothing to match against. Returning nothing is the honest
// answer; failing the pass would stop the stream behind it.
func contentOf(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	parsed := gjson.Get(raw, "content")
	if !parsed.Exists() {
		return nil
	}
	var content map[string]any
	if err := json.Unmarshal([]byte(parsed.Raw), &content); err != nil {
		return nil
	}
	return content
}
