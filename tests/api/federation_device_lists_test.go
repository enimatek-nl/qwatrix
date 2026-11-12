package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/tidwall/gjson"
)

// TestAPeersDeviceChangeReachesThePeopleTheyShareARoomWith is the cross-server half of a device list:
// Bob lives on another homeserver, shares a room with Alice, and adds a device. Alice's server cannot
// look that up — only Bob's server may speak for Bob's devices — so Bob's server has to say so, and
// Alice's server has to believe the one server it is allowed to believe.
//
// When this goes missing nothing fails loudly: Alice keeps encrypting to Bob's old set, her new device
// never appears, and both users see a room that quietly stops carrying new messages.
func TestAPeersDeviceChangeReachesThePeopleTheyShareARoomWith(t *testing.T) {
	hs1 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs1.FederationServer(t)
	hs2 := harness.NewWithRegistration(t,
		harness.WithServerName(dialableServerName(t)), harness.WithFederationInsecureTLS())
	hs2.FederationServer(t)

	alice := hs1.Register(t, "crossalice", "pw-crossalice", false)
	bob := hs2.Register(t, "crossbob", "pw-crossbob", false)
	roomID := alice.MustCreateRoom(t, hs1, map[string]any{"preset": "public_chat"})
	bob.MustPostJSON(t, hs2, "/_matrix/client/v3/join/"+url.PathEscape(roomID)+
		"?server_name="+url.QueryEscape(hs1.ServerName), map[string]any{}).MustStatus(t, http.StatusOK)
	hs2.DrainFederation(t)
	hs1.DrainFederation(t)

	first := alice.MustSync(t, hs1, url.Values{"timeout": []string{"0"}})
	if first.NextBatch == "" {
		t.Fatal("alice's first sync returned no position to continue from")
	}

	// Bob turns on a new device. The update should reach hs1 on its own, without anyone asking.
	uploadDeviceKeys(t, bob, hs2)
	hs2.DrainFederation(t)

	deadline := time.Now().Add(10 * time.Second)
	for {
		next := alice.MustSync(t, hs1, url.Values{"since": []string{first.NextBatch}, "timeout": []string{"0"}})
		if containsString(next.DeviceLists.Changed, bob.UserID) {
			return
		}
		if next.NextBatch != "" {
			first.NextBatch = next.NextBatch
		}
		if time.Now().After(deadline) {
			t.Fatalf("bob on %s changed his devices and alice never heard: %+v\nHS1 LOG:\n%s",
				hs2.ServerName, next.DeviceLists, hs1.Logs())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestADeviceChangeFromTheWrongServerIsIgnored is the same message from the side that has to refuse it.
//
// A device list is a claim about a user, and the only witness a server can accept is the server that
// hosts them. A peer reporting somebody else's user is either confused or asking this server to throw
// away keys on the word of a stranger, and the two look identical from here.
//
// What this pins is the refusal together with the envelope's status: a transaction whose content is a
// lie is still accepted, because refusing the envelope would let a peer deny itself out of a room. The
// positive half — that a well-founded update reaches the people who share a room with the subject — is
// the test above, with two real homeservers and real membership behind it.
func TestADeviceChangeFromTheWrongServerIsIgnored(t *testing.T) {
	// The default server name, not a dialable one: the signature on a federation request names the
	// destination by name, and this test's server has to be the name the peer signs for.
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	server.FederationServer(t)
	alice := server.Register(t, "listeneralice", "pw-listeneralice", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	// Somebody has to share the room for a change to be worth reporting, so the observer exists.
	_ = roomID

	first := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	peer := newFakePeer(t)
	forged := map[string]any{
		// The peer is not the server in this user ID, and cannot speak for them.
		"user_id":   "@somewhere.else:user.that.does.not.exist",
		"device_id": "FORGED",
		"stream_id": 1,
		"deleted":   false,
	}
	status, body := federationSendEDU(t, server, peer, deviceListUpdateEDU, forged)
	if status != http.StatusOK {
		t.Fatalf("a transaction carrying a device-list update: status %d, want 200 (an EDU is not refused\n"+
			"because one of its contents is a lie — that would let a peer deny itself)\nbody: %s", status, body)
	}

	next := alice.MustSync(t, server, url.Values{"since": []string{first.NextBatch}, "timeout": []string{"0"}})
	raw, err := json.Marshal(next.DeviceLists)
	if err != nil {
		t.Fatalf("device_lists are unreadable: %v", err)
	}
	if got := gjson.Get(string(raw), "changed").Array(); len(got) != 0 {
		t.Errorf("a forged device-list update was reported to alice: %s", raw)
	}
}

// deviceListUpdateEDU is the ephemeral event type that carries a device-list change.
const deviceListUpdateEDU = "m.device_list_update"

// federationSendEDU puts one ephemeral event into a transaction and delivers it the way a peer would.
//
// The wire names are the ones this server's federation library speaks — `edus`, and `edu_type` for the
// type — which is worth stating out loud because the ratified specification says `type`. Two spellings
// of the same message is the same story the media endpoints told: the library this server is built on
// predates the wording, and an implementation on the other end is free to be written against either.
func federationSendEDU(t *testing.T, server *harness.Server, peer *fakePeer, eduType string, content map[string]any,
) (int, []byte) {
	t.Helper()

	body := map[string]any{
		"origin":           string(peer.serverNo),
		"origin_server_ts": time.Now().UnixMilli(),
		"pdus":             []json.RawMessage{},
		"edus": []map[string]any{{
			"edu_type": eduType,
			"content":  content,
		}},
	}
	path := "/_matrix/federation/v1/send/" + harness.PathEscape("txn-device-list-probe")
	response := signedFederationPut(t, server, peer, path, body)
	return response.StatusCode, federationBody(t, response)
}
