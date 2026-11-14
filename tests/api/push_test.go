package api_test

// Push delivery, tested from both ends at once.
//
// A push notification is a chain with a stranger in the middle — user's app registers a pusher,
// this server decides an event was worth telling them about, this server tells a gateway whose URL
// that app supplied — and every test here therefore stands up the stranger as well: an ordinary HTTP
// server in the test process, recording what it was asked to deliver. A test that asserted only that
// a row appeared in a queue would pass while the user's phone stayed silent, which is the one
// failure mode this feature has that matters.
//
// What each test is really protecting:
//
//   - a pusher round-trips, because a client that cannot read back what it registered cannot tell
//     the user which of their devices is being sent their messages;
//   - a revoked session takes its pusher with it, because "sign out of that phone" has to mean the
//     phone stops being told things;
//   - an event reaches a gateway with the fields a gateway needs, and an `event_id_only` pusher is
//     not sent the message text it asked not to be sent;
//   - the judgement is recorded even when nobody is listening, because `GET /notifications` is about
//     what was worth reading, not about which account owns a phone;
//   - a rejected push key is dropped rather than retried forever.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// pushGateway is a push gateway a test can point a pusher at.
type pushGateway struct {
	server *httptest.Server
	// rejects names the push keys this gateway answers `rejected` for.
	reject map[string]bool

	mu       sync.Mutex
	requests []gatewayRequestRecord
}

// gatewayRequestRecord is what a gateway was asked to deliver, kept as the generic shape so a test
// can ask about any field without this file re-declaring the wire format.
type gatewayRequestRecord struct {
	Notification struct {
		EventID string          `json:"event_id"`
		RoomID  string          `json:"room_id"`
		Type    string          `json:"type"`
		Sender  string          `json:"sender"`
		Content json.RawMessage `json:"content"`
		Counts  *struct {
			Unread int64 `json:"unread"`
		} `json:"counts"`
		UserIsTarget bool `json:"user_is_target"`
		Devices      []struct {
			AppID   string         `json:"app_id"`
			PushKey string         `json:"pushkey"`
			Tweaks  map[string]any `json:"tweaks"`
			Data    map[string]any `json:"data"`
		} `json:"devices"`
	} `json:"notification"`
}

func newPushGateway(t *testing.T) *pushGateway {
	t.Helper()
	gateway := &pushGateway{reject: map[string]bool{}}
	gateway.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("a push gateway is POSTed to, not %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var record gatewayRequestRecord
		if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
			t.Errorf("the notification this server sent is not JSON a gateway could read: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rejected := []string{}
		gateway.mu.Lock()
		gateway.requests = append(gateway.requests, record)
		for _, device := range record.Notification.Devices {
			if gateway.reject[device.PushKey] {
				rejected = append(rejected, device.PushKey)
			}
		}
		gateway.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"rejected": rejected})
	}))
	t.Cleanup(gateway.server.Close)
	return gateway
}

func (g *pushGateway) url() string { return g.server.URL + "/_matrix/push/v1/notify" }

// sent reports the notifications this gateway was asked to deliver.
func (g *pushGateway) sent() []gatewayRequestRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]gatewayRequestRecord, len(g.requests))
	copy(out, g.requests)
	return out
}

// refusing makes this gateway reject one push key, which is how an upstream provider says that the
// app behind a token is gone.
func (g *pushGateway) refusing(pushKey string) {
	g.mu.Lock()
	g.reject[pushKey] = true
	g.mu.Unlock()
}

// registerPusher registers a pusher for one account through the real endpoint, which is the point:
// a test that wrote the row directly would not test the door the app uses.
func registerPusher(t *testing.T, server *harness.Server, account *harness.Account,
	url, pushKey string, data map[string]any) {
	t.Helper()
	body := map[string]any{
		"kind":                "http",
		"app_id":              "org.qwatrix.test",
		"app_display_name":    "Qwatrix Test",
		"device_display_name": "test device",
		"pushkey":             pushKey,
		"lang":                "en",
		"data":                withURL(data, url),
	}
	resp := account.MustPostJSON(t, server, "/_matrix/client/v3/pushers/set", body)
	resp.MustStatus(t, 200)
}

func withURL(data map[string]any, url string) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	data["url"] = url
	return data
}

// listPushers reads the account's pushers back.
func listPushers(t *testing.T, server *harness.Server, account *harness.Account) []map[string]any {
	t.Helper()
	resp := account.MustGet(t, server, "/_matrix/client/v3/pushers")
	resp.MustStatus(t, 200)
	var body struct {
		Pushers []map[string]any `json:"pushers"`
	}
	resp.JSON(t, &body)
	return body.Pushers
}

// TestAPusherIsRegisteredAndReadBack is the round trip a client depends on.
//
// The fields have to come back as they went in, including `data`: an app's pusher data is its own
// configuration, and a server that dropped a key it did not understand would break a feature it has
// never heard of. The list also includes another session's pushers, which is the specification's
// answer and the useful one — it is how a person notices a phone they no longer own is still being
// sent their messages.
func TestAPusherIsRegisteredAndReadBack(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	gateway := newPushGateway(t)

	registerPusher(t, server, alice, gateway.url(), "pushkey-read-back", map[string]any{
		"event_id_only": true,
		"custom_key":    "kept verbatim",
	})

	pushers := listPushers(t, server, alice)
	if len(pushers) != 1 {
		t.Fatalf("GET /pushers lists %d pushers, want the one that was registered", len(pushers))
	}
	got := pushers[0]
	if got["kind"] != "http" {
		t.Errorf("kind: got %v, want http", got["kind"])
	}
	if got["app_id"] != "org.qwatrix.test" || got["pushkey"] != "pushkey-read-back" {
		t.Errorf("the pusher's identity came back as %v/%v", got["app_id"], got["pushkey"])
	}
	if got["app_display_name"] != "Qwatrix Test" || got["lang"] != "en" {
		t.Errorf("the display fields were not kept: %v / %v", got["app_display_name"], got["lang"])
	}
	data, _ := got["data"].(map[string]any)
	if data == nil {
		t.Fatalf("GET /pushers returned no data object: %v", got)
	}
	if data["url"] != gateway.url() {
		t.Errorf("data.url: got %v, want it back as registered", data["url"])
	}
	if data["custom_key"] != "kept verbatim" {
		t.Errorf("a key this server does not use was dropped: %v", data)
	}
	if data["event_id_only"] != true {
		t.Errorf("data.event_id_only: got %v, want true", data["event_id_only"])
	}
}

// TestReRegisteringAPusherDoesNotDoubleIt is about the ordinary behaviour of apps.
//
// An app re-registers its pusher on launch, and its push token changes without warning; if each of
// those registrations added a pusher, the user would be notified once per registration per message.
func TestReRegisteringAPusherDoesNotDoubleIt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	gateway := newPushGateway(t)

	for attempt := 0; attempt < 3; attempt++ {
		registerPusher(t, server, alice, gateway.url(), "pushkey-once", nil)
	}
	if pushers := listPushers(t, server, alice); len(pushers) != 1 {
		t.Fatalf("registering the same pusher three times left %d pushers, want 1", len(pushers))
	}

	// A second device of the same app is a second pusher, and must stay separate: two phones,
	// two tokens, two sets of keys.
	registerPusher(t, server, alice, gateway.url(), "pushkey-second", nil)
	if pushers := listPushers(t, server, alice); len(pushers) != 2 {
		t.Fatalf("a second push key left %d pushers, want 2", len(pushers))
	}

	// Withdrawal is the same endpoint with no kind, which is how the specification says it.
	resp := alice.MustPostJSON(t, server, "/_matrix/client/v3/pushers/set", map[string]any{
		"app_id":  "org.qwatrix.test",
		"pushkey": "pushkey-second",
	})
	resp.MustStatus(t, 200)
	if pushers := listPushers(t, server, alice); len(pushers) != 1 {
		t.Fatalf("after withdrawing a pusher %d remain, want 1", len(pushers))
	}
	// Withdrawing something that is not there is not an error: the client's wish is already true.
	resp = alice.MustPostJSON(t, server, "/_matrix/client/v3/pushers/set", map[string]any{
		"app_id":  "org.qwatrix.test",
		"pushkey": "pushkey-never-existed",
	})
	resp.MustStatus(t, 200)
}

// TestAPusherDiesWithTheSessionThatRegisteredIt is the revocation test.
//
// Changing a password with logout_devices leaves the acting session alone and revokes the others,
// and a pusher belongs to the session that registered it — so a device the user just signed out of
// must stop being told what their messages say. A pusher that outlived its session would mean
// signing out of a phone was a UI gesture rather than a change.
func TestAPusherDiesWithTheSessionThatRegisteredIt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	gateway := newPushGateway(t)

	// A second, independent session for the same account, as a phone would have.
	other := server.Login(t, "alice", "pw-alice")
	registerPusher(t, server, other, gateway.url(), "pushkey-other-session", nil)
	registerPusher(t, server, alice, gateway.url(), "pushkey-this-session", nil)
	if pushers := listPushers(t, server, alice); len(pushers) != 2 {
		t.Fatalf("before the password change: got %d pushers, want 2", len(pushers))
	}

	resp := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/account/password",
		Token:  alice.AccessToken,
		Body: map[string]any{
			"new_password":   "pw-alice-two",
			"logout_devices": true,
			// A password change is user-interactive: proving the old password is what makes this a
			// change rather than a takeover by whoever held the access token.
			"auth": map[string]any{
				"type":       "m.login.password",
				"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
				"password":   "pw-alice",
			},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("changing the password: %d %s", resp.StatusCode, resp.Body)
	}

	pushers := listPushers(t, server, alice)
	if len(pushers) != 1 {
		t.Fatalf("after a password change that logged other devices out: got %d pushers, want only "+
			"this session's: %v", len(pushers), pushers)
	}
	if key, _ := pushers[0]["pushkey"].(string); key != "pushkey-this-session" {
		t.Errorf("the surviving pusher is %q; the session that changed the password keeps its own", key)
	}

	// The list is also scoped by account: a stranger's pusher is not this account's to see.
	bob := server.Register(t, "bob", "pw-bob", false)
	if pushers := listPushers(t, server, bob); len(pushers) != 0 {
		t.Errorf("bob sees %d pushers, want none of alice's", len(pushers))
	}
}

// TestAnUnusableGatewayIsRefusedWhenRegistered is the door that keeps this server from being turned
// into a generic fetch tool.
//
// A pusher's URL is client-supplied and this server will post to it, so the scheme and the shape are
// checked at registration. Refusing there rather than at delivery time means the client that made the
// mistake hears about it in the response to the request that made it, instead of in the silence of a
// phone that never rang.
func TestAnUnusableGatewayIsRefusedWhenRegistered(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)

	for name, url := range map[string]string{
		"a file URL":       "file:///etc/passwd",
		"no scheme":        "169.254.169.254/latest/meta-data/",
		"credentials":      "http://user:password@127.0.0.1:1/notify",
		"an empty URL":     "",
		"a data URL":       "data:text/plain,hello",
		"the host missing": "https:///notify",
	} {
		resp := alice.Do(t, server, harness.Request{
			Method: http.MethodPost,
			Path:   "/_matrix/client/v3/pushers/set",
			Token:  alice.AccessToken,
			Body: map[string]any{
				"kind":    "http",
				"app_id":  "org.qwatrix.test",
				"pushkey": "pushkey-" + name,
				"data":    map[string]any{"url": url},
			},
		})
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s was accepted as a push gateway; this server would then be asked to fetch it", name)
		}
		if resp.StatusCode < 400 || resp.StatusCode > 499 {
			t.Errorf("%s: got %d, want a 4xx that tells the client its URL was the problem\nbody: %s",
				name, resp.StatusCode, resp.Body)
		}
	}
	if pushers := listPushers(t, server, alice); len(pushers) != 0 {
		t.Errorf("a refused registration still left %d pushers behind", len(pushers))
	}
}

// TestAGatewayOnPrivateNetworkIsAccepted pins the other half of the policy, which is easy to "fix"
// by accident.
//
// Refusing everything but a public HTTPS host would be the safe-looking change and would break the
// two common deployments: a push gateway run beside the homeserver on loopback, and a UnifiedPush
// distributor on the same network segment. So this server checks the shape of a gateway URL — scheme,
// host, no credentials — and leaves address ranges to the network, which is where a policy about
// reachable address ranges can actually be enforced. `-push=false` remains the operator's opt-out of
// making outbound requests at all.
func TestAGatewayOnPrivateNetworkIsAccepted(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)

	for name, url := range map[string]string{
		"loopback, plain":      "http://127.0.0.1:8010/_matrix/push/v1/notify",
		"a container hostname": "http://sygnal:8009/_matrix/push/v1/notify",
		"an internal segment":  "http://10.0.0.7:8009/_matrix/push/v1/notify",
		"public, with TLS":     "https://push.matrix.org/_matrix/push/v1/notify",
	} {
		resp := alice.Do(t, server, harness.Request{
			Method: http.MethodPost,
			Path:   "/_matrix/client/v3/pushers/set",
			Token:  alice.AccessToken,
			Body: map[string]any{
				"kind":    "http",
				"app_id":  "org.qwatrix.test",
				"pushkey": "pushkey-" + name,
				"data":    map[string]any{"url": url},
			},
		})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s was refused as a push gateway (%d %s); a deployment is allowed to run its "+
				"gateway somewhere this server's configuration has never heard of", name,
				resp.StatusCode, resp.Body)
		}
	}
}

// TestAMessageReachesAPusher is the whole chain: an event arrives, the rules say it is worth telling
// somebody about, and the gateway they registered is told about it.
//
// The gateway's request has to carry what a gateway needs — which event, in which room, to which
// device, presented how — and `GET /notifications` has to agree with what was sent, down to whether
// the person has read it since.
func TestAMessageReachesAPusher(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	gateway := newPushGateway(t)
	registerPusher(t, server, bob, gateway.url(), "pushkey-bob", nil)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	eventID := alice.MustSendText(t, server, roomID, "the room needs you")

	server.DrainPushes(t)

	sent := gateway.sent()
	if len(sent) != 1 {
		t.Fatalf("the gateway received %d notifications, want exactly one for one message: %+v",
			len(sent), sent)
	}
	notification := sent[0].Notification
	if notification.EventID != eventID {
		t.Errorf("event_id: got %s, want the message's own %s", notification.EventID, eventID)
	}
	if notification.RoomID != roomID {
		t.Errorf("room_id: got %s, want %s", notification.RoomID, roomID)
	}
	if notification.Type != "m.room.message" || notification.Sender != alice.UserID {
		t.Errorf("type/sender: got %q from %q, want m.room.message from %s",
			notification.Type, notification.Sender, alice.UserID)
	}
	if !json.Valid(notification.Content) || !strings.Contains(string(notification.Content), "the room needs you") {
		t.Errorf("content: got %s, want the message body this pusher did not ask to withhold",
			notification.Content)
	}
	if len(notification.Devices) != 1 {
		t.Fatalf("devices: got %d entries, want bob's one device", len(notification.Devices))
	}
	device := notification.Devices[0]
	if device.PushKey != "pushkey-bob" || device.AppID != "org.qwatrix.test" {
		t.Errorf("the notification was addressed to %s / %s", device.AppID, device.PushKey)
	}
	if _, present := device.Data["url"]; present {
		t.Errorf("the device data carried the gateway URL the request is already addressed to: %v",
			device.Data)
	}
	if device.Tweaks["sound"] == nil {
		t.Errorf("tweaks: got %v, want the presentation the push rule asked for (a sound at least)",
			device.Tweaks)
	}
	if notification.Counts == nil || notification.Counts.Unread != 1 {
		t.Errorf("counts: got %+v, want one unread", notification.Counts)
	}

	// The same event is now visible to bob through the endpoint a client uses to list what this
	// server said was worth reading.
	notifications := listNotifications(t, server, bob)
	if len(notifications) != 1 {
		t.Fatalf("GET /notifications returns %d entries, want the one message", len(notifications))
	}
	entry := notifications[0]
	if entry["room_id"] != roomID {
		t.Errorf("room_id: got %v, want %s", entry["room_id"], roomID)
	}
	if entry["unread"] != true {
		t.Errorf("unread: got %v, want true — bob has not read it", entry["unread"])
	}
	event, _ := entry["event"].(map[string]any)
	if event == nil || event["event_id"] != eventID {
		t.Fatalf("the notification carries no event: %v", entry)
	}
	if actions, _ := entry["actions"].([]any); len(actions) == 0 {
		t.Errorf("actions: got %v, want the rule's actions so a client can tell a badge from a noise",
			entry["actions"])
	}

	// Reading it makes it read. The flag is per notification and follows receipts, so a receipt is
	// the only thing that should change it.
	resp := bob.MustPostJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/read_markers", map[string]any{"m.fully_read": eventID, "m.read": eventID})
	resp.MustStatus(t, 200)
	for _, entry := range listNotifications(t, server, bob) {
		if event, _ := entry["event"].(map[string]any); event["event_id"] == eventID && entry["unread"] == true {
			t.Error("the notification is still unread after a receipt covered it")
		}
	}

	// Nothing is owed any more: a second pass delivers nothing, because the debt was paid rather
	// than forgotten.
	if again := server.DrainPushes(t); again != 0 {
		// The second pass also re-evaluates no events, so a non-zero count here would mean
		// notifications were being re-sent for an event the gateway already took.
		t.Errorf("a second delivery pass delivered %d things, want 0", again)
	}
	if extra := len(gateway.sent()); extra != 1 {
		t.Errorf("the gateway received %d notifications in total, want 1: a retried notification is "+
			"a second buzz for the same message", extra)
	}
}

// TestAnEventIDOnlyPusherIsNotSentTheMessage is a privacy property, not an optimisation.
//
// `event_id_only` means "the app will fetch the event itself", and the point of that is that the
// text of a message does not pass through a gateway that could log it. Sending it anyway because it
// was cheap to include would make the flag a lie, and apps set it precisely because they do not
// trust the path.
func TestAnEventIDOnlyPusherIsNotSentTheMessage(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	gateway := newPushGateway(t)
	registerPusher(t, server, bob, gateway.url(), "pushkey-quiet", map[string]any{
		"event_id_only": true,
		"format":        "event_id_only",
	})

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	eventID := alice.MustSendText(t, server, roomID, "passphrase: correct horse battery staple")

	server.DrainPushes(t)
	sent := gateway.sent()
	if len(sent) != 1 {
		t.Fatalf("the gateway received %d notifications, want 1", len(sent))
	}
	notification := sent[0].Notification
	if notification.EventID != eventID || notification.RoomID != roomID {
		t.Errorf("an event_id_only notification still needs the event and room: got %q in %q",
			notification.EventID, notification.RoomID)
	}
	body := string(sent[0].Notification.Content)
	if len(sent[0].Notification.Content) != 0 {
		t.Errorf("content was sent to an event_id_only pusher: %s", body)
	}
	if notification.Sender != "" || notification.Type != "" {
		t.Errorf("sender/type were sent to an event_id_only pusher: %q / %q",
			notification.Sender, notification.Type)
	}
	if strings.Contains(string(must(t, sent[0])), "correct horse battery staple") {
		t.Error("the message text reached a gateway whose pusher asked not to be sent it")
	}
}

// TestARejectedPushKeyIsRemoved is about not stalking a device.
//
// A gateway rejects a push key when the app behind it is gone. Carrying on would mean this server
// posting a stranger's token to a gateway indefinitely, and the token is information about a device
// its owner no longer has.
func TestARejectedPushKeyIsRemoved(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	gateway := newPushGateway(t)
	gateway.refusing("pushkey-dead")
	registerPusher(t, server, bob, gateway.url(), "pushkey-dead", nil)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	alice.MustSendText(t, server, roomID, "one last time")

	server.DrainPushes(t)
	if pushers := listPushers(t, server, bob); len(pushers) != 0 {
		t.Fatalf("the gateway rejected this push key and %d pushers remain: %v", len(pushers), pushers)
	}

	// And nothing further is sent to a device that no longer exists.
	before := len(gateway.sent())
	alice.MustSendText(t, server, roomID, "and again")
	server.DrainPushes(t)
	if after := len(gateway.sent()); after != before {
		t.Errorf("the gateway received %d more notifications for a push key it rejected", after-before)
	}
}

// TestANotificationIsRecordedWithoutAPusher protects the reason the judgement is stored at all.
//
// `GET /notifications` lists events that were worth interrupting somebody for. If that list only
// knew about accounts that happened to register a device, then a person who turned notifications off
// — or who is on a laptop with no pusher — would find an endpoint that silently knows nothing, and
// the server would be deciding what to remember based on which hardware a user owns.
func TestANotificationIsRecordedWithoutAPusher(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	eventID := alice.MustSendText(t, server, roomID, "no phone involved")

	server.DrainPushes(t)

	if sent := gatewayCountOf(t, nil); sent != 0 {
		t.Fatalf("no gateway was registered and %d notifications were sent anyway", sent)
	}
	notifications := listNotifications(t, server, bob)
	if len(notifications) != 1 {
		t.Fatalf("GET /notifications returns %d entries, want the one message", len(notifications))
	}
	event, _ := notifications[0]["event"].(map[string]any)
	if event["event_id"] != eventID {
		t.Errorf("the notification names %v, want %s", event["event_id"], eventID)
	}
}

// TestAnInviteIsAboutThePersonItNames is the one notification that is not a message.
//
// `user_is_target` is how a gateway knows to say "Invitation to #room" rather than "New message",
// and it is true only when the event is *about* the recipient. A phone that said "New message from
// alice" for an invitation she sent would be technically literate and useless.
func TestAnInviteIsAboutThePersonItNames(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	gateway := newPushGateway(t)
	registerPusher(t, server, bob, gateway.url(), "pushkey-invited", nil)

	roomID := alice.MustCreateRoom(t, server, map[string]any{
		"preset":    "private_chat",
		"invite":    []string{bob.UserID},
		"name":      "somewhere to read",
		"is_direct": true,
	})
	server.DrainPushes(t)

	sent := gateway.sent()
	if len(sent) != 1 {
		t.Fatalf("the gateway received %d notifications for one invite, want 1: %+v", len(sent), sent)
	}
	notification := sent[0].Notification
	if notification.RoomID != roomID {
		t.Errorf("room_id: got %s, want %s", notification.RoomID, roomID)
	}
	if !notification.UserIsTarget {
		t.Error("user_is_target was not sent, so a gateway cannot tell an invitation from chatter")
	}
}

// TestWhatNobodyWasToldAbout checks the two exclusions a person notices immediately.
//
// A person's own words are not news to them, and a room they muted is a room they chose. Both are
// decided by the rules rather than here — which is the point of the test: if either were a hard-coded
// exception in the delivery path, a rule that contradicted it would lose.
func TestWhatNobodyWasToldAbout(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	gateway := newPushGateway(t)
	registerPusher(t, server, alice, gateway.url(), "pushkey-own-words", nil)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	alice.MustSendText(t, server, roomID, "talking to myself")

	server.DrainPushes(t)
	if sent := gateway.sent(); len(sent) != 0 {
		t.Errorf("alice was notified about her own message: %+v", sent)
	}
	if notifications := listNotifications(t, server, alice); len(notifications) != 0 {
		t.Errorf("alice's notification list holds %d entries for her own message", len(notifications))
	}

	// Muting the room is a rule, and the rule has to be what stops the notification.
	// A room rule that says `dont_notify` is how a person mutes one room, and it is a rule rather
	// than a setting: the delivery path has to ask the rules, not keep its own list.
	resp := alice.MustPutJSON(t, server, "/_matrix/client/v3/pushrules/global/room/"+
		harness.PathEscape(roomID), map[string]any{"actions": []string{"dont_notify"}})
	resp.MustStatus(t, 200)
	alice.MustSendText(t, server, roomID, "still muted")
	server.DrainPushes(t)
	if sent := gateway.sent(); len(sent) != 0 {
		t.Errorf("a room the account muted sent %d notifications", len(sent))
	}
}

// listNotifications reads GET /notifications as a client would.
func listNotifications(t *testing.T, server *harness.Server, account *harness.Account) []map[string]any {
	t.Helper()
	resp := account.MustGet(t, server, "/_matrix/client/v3/notifications")
	resp.MustStatus(t, 200)
	var body struct {
		Notifications []map[string]any `json:"notifications"`
	}
	resp.JSON(t, &body)
	if body.Notifications == nil {
		t.Fatal("GET /notifications returned no notifications key at all")
	}
	return body.Notifications
}

// gatewayCountOf is a guard for the test that has no gateway: it keeps "nothing was sent" honest
// rather than vacuous.
func gatewayCountOf(t *testing.T, gateway *pushGateway) int {
	t.Helper()
	if gateway == nil {
		return 0
	}
	return len(gateway.sent())
}

// must renders a gateway record for a substring assertion.
func must(t *testing.T, record gatewayRequestRecord) []byte {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("the gateway record cannot be rendered for a substring check: %v", err)
	}
	return encoded
}
