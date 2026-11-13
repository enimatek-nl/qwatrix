package api_test

// Carrying to-device messages between servers.
//
// To-device messages are how an end-to-end-encrypted conversation starts: a Megolm session key is
// handed to one person's laptop and to nobody else in the room — including the server. So the message
// never becomes an event, and the only way it crosses from one homeserver to another is an ephemeral
// unit pushed between the two servers.
//
// Until this worked, a user on this server could sit in a room with somebody on another server and
// watch the room fill with placeholders: the room events arrived, the keys that unlock them did not.
// Two properties are worth pinning here because both are easy to break and both are expensive to
// notice. A key delivered twice lets a client replay a message key it has already used, so the wire
// carries an idempotence ID and a retry has to be a no-op. And a server must accept these messages
// only for the users it hosts: otherwise any peer could write into any device's inbox, and the inbox
// is where a server's users trust what they find.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// keyShare is a stand-in for an m.room_key event: opaque bytes as far as the server is concerned, and
// the whole point of it is that the server cannot read it.
func keyShare(sessionID string) map[string]any {
	return map[string]any{
		"algorithm":   "m.megolm.v1.aes-sha2",
		"room_id":     "!somewhere:" + "example.org",
		"session_id":  sessionID,
		"session_key": "AgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAEhV6uM9Z1TnGmHn0mF4w",
	}
}

// sendToDevice shares a key with the named users and devices, exactly as a client does.
func sendToDevice(t *testing.T, from *harness.Account, server *harness.Server, txnID string,
	messages map[string]map[string]any) *harness.Response {
	t.Helper()
	return from.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   "/_matrix/client/v3/sendToDevice/m.room_key/" + txnID,
		Body:   map[string]any{"messages": messages},
	})
}

// deviceInbox lists the session IDs one device was handed since a token. A device's inbox is drained
// as it is read, so a test that wants to see a message has to look from before it was sent.
func deviceInbox(t *testing.T, account *harness.Account, server *harness.Server, since string) []string {
	t.Helper()
	synced := account.MustSync(t, server, url.Values{"since": {since}, "timeout": {"0"}})
	keys := []string{}
	for _, raw := range synced.ToDevice.Events {
		var message struct {
			Type    string `json:"type"`
			Sender  string `json:"sender"`
			Content struct {
				SessionID string `json:"session_id"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatalf("a to-device message cannot be read: %v\n%s", err, raw)
		}
		if message.Type != "m.room.message" {
			keys = append(keys, message.Content.SessionID)
		}
	}
	return keys
}

// atTheStartOf reads the token a device's inbox is empty at, before a key is shared with it.
func atTheStartOf(t *testing.T, account *harness.Account, server *harness.Server) string {
	t.Helper()
	return account.MustSync(t, server, nil).NextBatch
}

func TestADeviceMessageReachesADeviceOnAnotherServer(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)

	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	charlie := hs2.Register(t, "charlie", "pw-charlie", false)
	before := atTheStartOf(t, charlie, hs2)

	response := sendToDevice(t, alice, hs1, "share-1",
		map[string]map[string]any{charlie.UserID: {"*": keyShare("session-one")}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sharing a key with a device on another server: got %d, want 200\nbody: %s",
			response.StatusCode, response.Body)
	}
	hs1.DrainFederation(t)

	keys := deviceInbox(t, charlie, hs2, before)
	if len(keys) != 1 || keys[0] != "session-one" {
		t.Fatalf("charlie's device was handed %v, want the one session this room needs\nHS1 LOG:\n%s\nHS2 LOG:\n%s",
			keys, hs1.Logs(), hs2.Logs())
	}
}

func TestARetriedKeyShareArrivesOnce(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)

	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	charlie := hs2.Register(t, "charlie", "pw-charlie", false)
	before := atTheStartOf(t, charlie, hs2)

	// The same transaction, sent twice: a client that timed out and retried, or a server that lost the
	// response. The recipient cannot tell the difference from a second key, which is why the wire
	// carries an ID for exactly this.
	for attempt := 0; attempt < 2; attempt++ {
		response := sendToDevice(t, alice, hs1, "share-once",
			map[string]map[string]any{charlie.UserID: {"*": keyShare("session-one")}})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: got %d, want 200\nbody: %s", attempt, response.StatusCode, response.Body)
		}
	}
	hs1.DrainFederation(t)

	keys := deviceInbox(t, charlie, hs2, before)
	if len(keys) != 1 {
		t.Fatalf("a retried key share was delivered %d times, want once: %v\nHS1 LOG:\n%s\nHS2 LOG:\n%s",
			len(keys), keys, hs1.Logs(), hs2.Logs())
	}
}

func TestEveryDeviceOfARemoteUserTakesTheKey(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)

	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	first := hs2.Register(t, "charlie", "pw-charlie", false)
	second := hs2.Login(t, "charlie", "pw-charlie")
	if first.DeviceID == second.DeviceID {
		t.Fatalf("the two logins share a device (%s), so this would prove nothing", first.DeviceID)
	}
	beforeFirst := atTheStartOf(t, first, hs2)
	beforeSecond := atTheStartOf(t, second, hs2)

	// The sender names `*` because it does not know — and is not supposed to know — which devices
	// somebody else's server holds. Only this expansion can be trusted to include a device added after
	// the sender last looked at this user.
	response := sendToDevice(t, alice, hs1, "share-all",
		map[string]map[string]any{first.UserID: {"*": keyShare("session-one")}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sharing a key with every device of a remote user: got %d\nbody: %s",
			response.StatusCode, response.Body)
	}
	hs1.DrainFederation(t)

	for _, probe := range []struct {
		account *harness.Account
		since   string
	}{{first, beforeFirst}, {second, beforeSecond}} {
		if keys := deviceInbox(t, probe.account, hs2, probe.since); len(keys) != 1 {
			t.Errorf("device %s was handed %v, want the session key\nHS1 LOG:\n%s\nHS2 LOG:\n%s",
				probe.account.DeviceID, keys, hs1.Logs(), hs2.Logs())
		}
	}
}

func TestAKeyForADeviceThatDoesNotExistIsNotHeldForLater(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	alice := hs1.Register(t, "alice", "pw-alice", false)

	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)
	charlie := hs2.Register(t, "charlie", "pw-charlie", false)
	before := atTheStartOf(t, charlie, hs2)

	// One message for a device that exists and one for a device that does not. The first has to
	// arrive; the second has to be dropped rather than queued against a device that may never exist,
	// because a queue that fills with keys for ghosts is a queue that never gets read.
	response := sendToDevice(t, alice, hs1, "share-mixed", map[string]map[string]any{
		charlie.UserID: {
			charlie.DeviceID: keyShare("session-real"),
			"NOSUCHDEVICE":   keyShare("session-ghost"),
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a mixed batch: got %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	hs1.DrainFederation(t)

	keys := deviceInbox(t, charlie, hs2, before)
	if len(keys) != 1 || keys[0] != "session-real" {
		t.Fatalf("charlie's device was handed %v, want only the key addressed to it\nHS1 LOG:\n%s\nHS2 LOG:\n%s",
			keys, hs1.Logs(), hs2.Logs())
	}
}
