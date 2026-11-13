package api_test

// Application services: programs this homeserver has been told to trust.
//
// The tests here are written against the shape of the trust rather than against any particular bridge.
// A service authenticates with a secret from a file, sends as users it never registered, and may say
// when an event happened. Each of those is a capability a normal device does not have, so each needs a
// test that it works *and* a test that it stops at the file's edge: a service that can act as any user
// in the deployment is not a bridge but a way to read anybody's room, and a client that could choose
// its own timestamps could retroactively claim it was in a room before anyone joined it.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// bridgeRegistration is written the way an operator writes one, including the quoting: the format is
// Synapse's because that is what every existing registration uses and what complement copies into the
// container.
const bridgeRegistration = `id: qmatrix_bridge
hs_token: hs-side-of-the-pair
as_token: as-side-of-the-pair
url: 'http://localhost:9000'
sender_localpart: the-bridge-user
rate_limited: false
namespaces:
  users:
    - exclusive: false
      regex: .*
  rooms: []
  aliases: []
`

// narrowRegistration is the same service given one user to speak for. The pattern is quoted because "@"
// begins a reserved character in YAML, so a user-ID namespace cannot be written bare.
const narrowRegistration = `id: narrow_bridge
as_token: narrow-token
sender_localpart: narrow-bridge
namespaces:
  users:
    - exclusive: true
      regex: '@imported_.*'
`

// applicationService is the account shape a bridge presents: an access token from a file, and a user
// ID this server created on its behalf.
func applicationService(server *harness.Server, localpart, token string) *harness.Account {
	return &harness.Account{
		Localpart:   localpart,
		UserID:      "@" + localpart + ":" + server.ServerName,
		AccessToken: token,
	}
}

// sendTextWithQuery sends a message with extra query parameters on the request, which is the only way
// to ask for an event to be stamped at a moment other than now.
func sendTextWithQuery(t *testing.T, server *harness.Server, account *harness.Account,
	roomID, body, query string) string {
	t.Helper()
	if query != "" && !strings.HasPrefix(query, "?") {
		// The query belongs to the request, not to the transaction ID — the two are separated by a
		// "?" and a helper that gets that wrong sends to an event that cannot exist.
		query = "?" + query
	}
	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/send/m.room.message/" +
		harness.Nonce("as") + query
	response := account.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   path,
		Body:   map[string]any{"msgtype": "m.text", "body": body},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sending %q: got %d, want 200\nbody: %s", body, response.StatusCode, response.Body)
	}
	var sent struct {
		EventID string `json:"event_id"`
	}
	response.JSON(t, &sent)
	return sent.EventID
}

// originServerTS reads back when the server says an event happened.
func originServerTS(t *testing.T, server *harness.Server, account *harness.Account, roomID, eventID string) int64 {
	t.Helper()
	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/event/" + harness.PathEscape(eventID)
	response := account.MustGet(t, server, path)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reading the event back: got %d\nbody: %s", response.StatusCode, response.Body)
	}
	var event struct {
		OriginServerTS int64  `json:"origin_server_ts"`
		Sender         string `json:"sender"`
	}
	if err := json.Unmarshal([]byte(response.Body), &event); err != nil {
		t.Fatalf("the event cannot be read: %v (%s)", err, response.Body)
	}
	return event.OriginServerTS
}

func TestAnApplicationServiceSpeaksAsItsOwnBridgeUser(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithApplicationService(bridgeRegistration))
	alice := server.Register(t, "member", "pw-member", false)
	bridge := applicationService(server, "the-bridge-user", "as-side-of-the-pair")

	// The token from the file is the credential; there is no password, no login, and no device the
	// bridge ever created.
	whoami := bridge.MustGet(t, server, "/_matrix/client/v3/account/whoami")
	if whoami.StatusCode != http.StatusOK {
		t.Fatalf("asking who the service is: got %d, want 200\nbody: %s", whoami.StatusCode, whoami.Body)
	}
	if want := "@the-bridge-user:" + server.ServerName; !json.Valid([]byte(whoami.Body)) ||
		!strings.Contains(string(whoami.Body), want) {
		t.Errorf("whoami answered %s, expected the service's own user %s", whoami.Body, want)
	}

	// The bridge user did not exist until it acted, and joins like anybody else: the account is
	// created on demand, because a bridge usually has more users than the deployment has accounts.
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bridge.MustJoinRoom(t, server, roomID)
	bridge.MustSendText(t, server, roomID, "carried across by the bridge")

	// The room sees a member and a message, not a special kind of thing.
	alice.MustTimelineHas(t, server, roomID, "m.room.message", "carried across by the bridge")
	members := alice.MustGet(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+"/joined_members")
	if !strings.Contains(string(members.Body), "@the-bridge-user:"+server.ServerName) {
		t.Errorf("the bridge user is not a member of the room it joined: %s", members.Body)
	}
}

func TestAnApplicationServiceMayOnlyBeTheUsersItWasGiven(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithApplicationService(narrowRegistration))
	alice := server.Register(t, "member", "pw-member", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bridge := applicationService(server, "narrow-bridge", "narrow-token")

	// Inside its namespace: a user nobody registered, who exists from the moment the service speaks
	// for them.
	imported := "@imported_one:" + server.ServerName
	join := bridge.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/join?user_id=" + harness.PathEscape(imported),
		Body:   map[string]any{},
	})
	if join.StatusCode != http.StatusOK {
		t.Fatalf("the service joined as a user it was given: got %d, want 200\nbody: %s", join.StatusCode, join.Body)
	}
	sent := sendTextWithQuery(t, server, bridge, roomID, "from an imported user", "&user_id="+harness.PathEscape(imported))
	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/event/" + harness.PathEscape(sent)
	event := alice.MustGet(t, server, path)
	if !strings.Contains(string(event.Body), imported) {
		t.Errorf("the message was sent by somebody else: %s", event.Body)
	}

	// Outside it: alice is a real user of this server, and the ability to send as her would be the
	// ability to read and forge her rooms. The registration did not grant that, so the request fails
	// on the first call rather than halfway through it.
	refused := bridge.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/join?user_id=" +
			harness.PathEscape("@member:"+server.ServerName),
		Body: map[string]any{},
	})
	refused.MustError(t, http.StatusForbidden, "M_FORBIDDEN")

	// And a user on another server, which would be this server signing events for an identity it does
	// not hold.
	bridge.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/join?user_id=" + harness.PathEscape("@someone:else.example"),
		Body:   map[string]any{},
	}).MustError(t, http.StatusBadRequest, "M_INVALID_PARAM")
}

func TestOnlyAnApplicationServiceMayNameTheUserItSpeaksFor(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithApplicationService(bridgeRegistration))
	alice := server.Register(t, "member", "pw-member", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	// A device session that names a different user is refused outright. Ignoring the parameter would
	// leave the client believing it had sent as that user while the server recorded somebody else,
	// and the two of them would disagree about who wrote the message forever.
	alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/send/m.room.message/" +
			harness.Nonce("claim") + "?user_id=" + harness.PathEscape("@the-bridge-user:"+server.ServerName),
		Body: map[string]any{"msgtype": "m.text", "body": "claimed identity"},
	}).MustError(t, http.StatusForbidden, "M_FORBIDDEN")

	// Naming itself is a no-op a client may reasonably do, and costs nothing to allow.
	alice.Do(t, server, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/account/whoami?user_id=" + harness.PathEscape(alice.UserID),
	}).MustStatus(t, http.StatusOK)
}

func TestATimestampOverrideBelongsToTheApplicationService(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithApplicationService(bridgeRegistration))
	alice := server.Register(t, "member", "pw-member", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bridge := applicationService(server, "the-bridge-user", "as-side-of-the-pair")
	bridge.MustJoinRoom(t, server, roomID)

	// A bridge importing years of another network's chat has to say when a message happened, and the
	// timestamp it names is the one the room gets.
	const importedAt = int64(1_000_000_000_000)
	imported := sendTextWithQuery(t, server, bridge, roomID, "a message from the past",
		"?ts="+strconv.FormatInt(importedAt, 10))
	if got := originServerTS(t, server, alice, roomID, imported); got != importedAt {
		t.Errorf("the imported event carries origin_server_ts %d, want %d", got, importedAt)
	}

	// A normal client may not choose when its message happened. The parameter is ignored rather than
	// refused — the specification says so, and a client that always sends it is confused rather than
	// attacking — so the event is stamped when it actually arrived.
	ordinary := sendTextWithQuery(t, server, alice, roomID, "a message from now", "?ts=1")
	got := originServerTS(t, server, alice, roomID, ordinary)
	if got == 1 {
		t.Fatal("a client chose when its own message happened")
	}
	if floor := time.Now().Add(-time.Minute).UnixMilli(); got < floor {
		t.Errorf("the ordinary message is stamped %d, which is not when it was sent", got)
	}

	// A service that asks for something that is not a moment is told, because accepting the request and
	// quietly stamping the event "now" would leave it holding a history that is wrong in a way nothing
	// downstream could ever notice.
	bridge.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/send/m.room.message/" +
			harness.Nonce("bad-ts") + "?ts=not-a-moment",
		Body: map[string]any{"msgtype": "m.text", "body": "nonsense timestamp"},
	}).MustError(t, http.StatusBadRequest, "M_INVALID_PARAM")
}

func TestEventsImportedInTheSameMomentAreStillOrderedByArrival(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithApplicationService(bridgeRegistration))
	alice := server.Register(t, "member", "pw-member", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bridge := applicationService(server, "the-bridge-user", "as-side-of-the-pair")
	bridge.MustJoinRoom(t, server, roomID)

	// The case an importer produces by construction: a whole batch of messages that all happened at one
	// instant of the imported clock. Looking backwards must find the last one written and looking
	// forwards the first, which a store that orders only by timestamp cannot tell the difference between.
	same := strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10)
	first := sendTextWithQuery(t, server, bridge, roomID, "same instant, first", "?ts="+same)
	second := sendTextWithQuery(t, server, bridge, roomID, "same instant, second", "?ts="+same)

	lookup := func(direction string) string {
		t.Helper()
		path := "/_matrix/client/v1/rooms/" + harness.PathEscape(roomID) + "/timestamp_to_event?ts=" + same +
			"&dir=" + direction
		var answer struct {
			EventID string `json:"event_id"`
		}
		alice.MustGet(t, server, path).JSON(t, &answer)
		return answer.EventID
	}
	if got := lookup("b"); got != second {
		t.Errorf("looking backwards from the shared timestamp found %s, want the later event %s", got, second)
	}
	if got := lookup("f"); got != first {
		t.Errorf("looking forwards from the shared timestamp found %s, want the earlier event %s", got, first)
	}
}

func TestAnApplicationServiceDoesNotGetTheUserQueue(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithApplicationService(bridgeRegistration))
	bridge := applicationService(server, "the-bridge-user", "as-side-of-the-pair")

	// /sync is one person's devices catching up on what they missed. A service is told what it asked
	// about over its own URL instead, and receiving the whole room traffic with no acknowledgement, no
	// retry and no subscription would be a firehose, not a feature.
	bridge.Do(t, server, harness.Request{Method: http.MethodGet, Path: "/_matrix/client/v3/sync"}).
		MustError(t, http.StatusForbidden, "M_FORBIDDEN")
}

func TestATokenFromAFileNobodyReadIsJustAnUnknownToken(t *testing.T) {
	// The same secret, with no registration configured: this is the deployment that has never been
	// told about a bridge, and nothing may authenticate as anything.
	server := harness.NewWithRegistration(t)
	unknown := applicationService(server, "the-bridge-user", "as-side-of-the-pair")

	for _, path := range []string{"/_matrix/client/v3/sync", "/_matrix/client/v3/account/whoami",
		"/_matrix/client/v3/joined_rooms"} {
		unknown.Do(t, server, harness.Request{Method: http.MethodGet, Path: path}).
			MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	}
}

func TestAnApplicationServiceMayNotWakeADeactivatedUser(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithApplicationService(bridgeRegistration))
	alice := server.Register(t, "member", "pw-member", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bridge := applicationService(server, "the-bridge-user", "as-side-of-the-pair")

	// The service may speak for alice — the registration says so — and does, once.
	sendAs := func(body string) *harness.Response {
		return bridge.Do(t, server, harness.Request{
			Method: http.MethodPut,
			Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/send/m.room.message/" +
				harness.Nonce("as") + "?user_id=" + harness.PathEscape(alice.UserID),
			Body: map[string]any{"msgtype": "m.text", "body": body},
		})
	}
	if response := sendAs("before deactivation"); response.StatusCode != http.StatusOK {
		t.Fatalf("the service sent as a user it was given: got %d\nbody: %s", response.StatusCode, response.Body)
	}

	// Then alice switches her account off. Deactivation is this server's decision about an account,
	// and a service that still believes it owns the name must not undo it by sending one message —
	// that is the difference between an account that is closed and one that merely looks closed.
	alice.MustPostJSON(t, server, "/_matrix/client/v3/account/deactivate", map[string]any{
		"auth": map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]any{"type": "m.id.user", "user": "member"},
			"password":   "pw-member",
		},
	}).MustStatus(t, http.StatusOK)

	sent := sendAs("after deactivation")
	if sent.StatusCode == http.StatusOK {
		t.Fatalf("a deactivated account sent a message again; response: %s", sent.Body)
	}
	if !strings.Contains(string(sent.Body), "M_USER_DISABLED") && !strings.Contains(string(sent.Body), "M_FORBIDDEN") {
		t.Errorf("the refusal is %s, want the account to be said to be gone", sent.Body)
	}
}
