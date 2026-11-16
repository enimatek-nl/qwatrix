package api_test

// Cross-signing is what turns a pile of devices into an identity: the user's master key vouches
// for their own devices, and their user-signing key vouches for other people. None of it can be
// seen by a client until the homeserver has been told about it, through the two endpoints a real
// client posts its bundle to — `/keys/device_signing/upload` for the keys themselves and
// `/keys/signatures/upload` for the signatures they make.
//
// The documents these tests upload are signed for real (see harness/crosssigning.go), because a
// server that accepted unsigned cross-signing keys would be a server where anybody with a stolen
// access token could hand out an identity, and a server that *rejected* correctly signed ones is
// how a perfectly healthy client ends up stuck on "setting up encryption".

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// identity is a user's three cross-signing keys, generated for a test that is about something
// other than the keys themselves.
type identity struct {
	Master, SelfSigning, UserSigning, Device *harness.SigningKey
}

func newIdentity(t *testing.T) identity {
	t.Helper()
	return identity{
		Master:      harness.NewSigningKey(t),
		SelfSigning: harness.NewSigningKey(t),
		UserSigning: harness.NewSigningKey(t),
		Device:      harness.NewSigningKey(t),
	}
}

// bundle is the request body a client posts when it publishes an identity.
func (i identity) bundle(t *testing.T, userID string) map[string]any {
	t.Helper()
	return map[string]any{
		"master_key":       json.RawMessage(harness.MasterKey(t, userID, i.Master, i.Device)),
		"self_signing_key": json.RawMessage(harness.SelfSigningKey(t, userID, i.Master, i.SelfSigning)),
		"user_signing_key": json.RawMessage(harness.UserSigningKey(t, userID, i.Master, i.UserSigning)),
	}
}

// uploadCrossSigningKeys posts a bundle the way the Rust crypto stack does: one POST, no `auth`,
// and the three keys the bootstrap produced.
func uploadCrossSigningKeys(t *testing.T, account *harness.Account, server *harness.Server, body map[string]any) *harness.Response {
	t.Helper()
	return account.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/device_signing/upload",
		Body:   body,
	})
}

// mustUploadCrossSigningKeys publishes an identity and fails the test if the server did not take it.
func mustUploadCrossSigningKeys(t *testing.T, account *harness.Account, server *harness.Server, who identity) {
	t.Helper()
	uploadCrossSigningKeys(t, account, server, who.bundle(t, account.UserID)).MustStatus(t, http.StatusOK)
}

// uploadSignatures posts what cross-signing keys signed, which is the other half of publishing an
// identity: the keys say who the user is, the signatures say which devices they answer for.
func uploadSignatures(t *testing.T, account *harness.Account, server *harness.Server, body map[string]any) *harness.Response {
	t.Helper()
	return account.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/signatures/upload",
		Body:   body,
	})
}

// keyQuery asks the server what it knows about some users' keys.
func keyQuery(t *testing.T, account *harness.Account, server *harness.Server, users ...string) map[string]any {
	t.Helper()
	wanted := map[string][]string{}
	for _, userID := range users {
		wanted[userID] = nil
	}
	response := account.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/keys/query",
		Body:   map[string]any{"device_keys": wanted},
	})
	response.MustStatus(t, http.StatusOK)
	var out map[string]any
	response.JSON(t, &out)
	return out
}

// asMap reads a nested object out of a decoded JSON response, and reports whether it was there.
func asMap(response map[string]any, field string) (map[string]any, bool) {
	raw, found := response[field]
	if !found {
		return nil, false
	}
	object, ok := raw.(map[string]any)
	return object, ok
}

// TestSettingUpCrossSigningForTheFirstTimeAsksForNoPassword covers the request Element Web makes
// the moment it has finished creating a user's identity. It is the request that, unanswered, left
// the "digital identity" screen spinning: the keys were uploaded to the device, and nowhere a
// client could find them.
//
// No authentication is asked for because there is nothing to protect yet (MSC3967): the client has
// just authenticated to log in, and a password prompt here would be a second one for the same act.
func TestSettingUpCrossSigningForTheFirstTimeAsksForNoPassword(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "identity", "pw-identity", false)

	master, self, user, device := harness.NewSigningKey(t), harness.NewSigningKey(t), harness.NewSigningKey(t), harness.NewSigningKey(t)

	response := uploadCrossSigningKeys(t, alice, server, map[string]any{
		"master_key":       json.RawMessage(harness.MasterKey(t, alice.UserID, master, device)),
		"self_signing_key": json.RawMessage(harness.SelfSigningKey(t, alice.UserID, master, self)),
		"user_signing_key": json.RawMessage(harness.UserSigningKey(t, alice.UserID, master, user)),
	})
	response.MustStatus(t, http.StatusOK)
	if body := string(response.Body); body != "{}" && body != "{}\n" {
		t.Errorf("POST /keys/device_signing/upload answered %s; the specification says an empty object", body)
	}
}

// TestAKeyQueryAnswersWithTheIdentityAndNotOnlyTheDevices is the other half of the same story: keys
// a client cannot read back are keys it was never told about.
//
// The user-signing key is the one field with a rule about who may see it. It is the key that says
// "I vouch for this other person", and a homeserver that handed it to everybody would be publishing
// a friendship graph that nobody agreed to publish.
func TestAKeyQueryAnswersWithTheIdentityAndNotOnlyTheDevices(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "verified", "pw-verified", false)
	bob := server.Register(t, "checking", "pw-checking", false)

	mustUploadCrossSigningKeys(t, alice, server, newIdentity(t))

	own := keyQuery(t, alice, server, alice.UserID)
	for _, field := range []string{"master_keys", "self_signing_keys", "user_signing_keys"} {
		keys, found := asMap(own, field)
		if !found {
			t.Fatalf("POST /keys/query for herself answered no %q; her own devices cannot rebuild her identity without it", field)
		}
		if _, hasAlice := keys[alice.UserID]; !hasAlice {
			t.Errorf("POST /keys/query: %q holds no key for %s", field, alice.UserID)
		}
	}

	someoneElse := keyQuery(t, bob, server, alice.UserID)
	for _, field := range []string{"master_keys", "self_signing_keys"} {
		keys, found := asMap(someoneElse, field)
		if !found {
			t.Fatalf("POST /keys/query for %q answered no %q; without it %s cannot check whether alice's devices are hers", bob.UserID, field, bob.UserID)
		}
		if _, hasAlice := keys[alice.UserID]; !hasAlice {
			t.Errorf("POST /keys/query: %q holds no key for %s", field, alice.UserID)
		}
	}
	if keys, found := asMap(someoneElse, "user_signing_keys"); found {
		if _, hasAlice := keys[alice.UserID]; hasAlice {
			t.Errorf("POST /keys/query showed %s alice's user-signing key; who she vouches for is hers to publish, not everybody's to read", bob.UserID)
		}
	}
}

// TestAKeyTheMasterKeyDidNotSignIsRefused is the difference between storing an identity and storing
// an assertion about one.
//
// The self-signing key is the key a user's devices are vouched for by, so a server that accepted one
// signed by anything other than that user's master key would let a person with a stolen access token
// sign any device they liked — including one they control — and have it believed by every client that
// later queried the account.
func TestAKeyTheMasterKeyDidNotSignIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "forged", "pw-forged", false)

	master := harness.NewSigningKey(t)
	stranger := harness.NewSigningKey(t)
	response := uploadCrossSigningKeys(t, alice, server, map[string]any{
		"master_key":       json.RawMessage(harness.MasterKey(t, alice.UserID, master, nil)),
		"self_signing_key": json.RawMessage(harness.SignedByKey(t, alice.UserID, stranger, "self_signing", harness.NewSigningKey(t))),
	})
	response.MustError(t, http.StatusBadRequest, string(matrix.ErrInvalidSignature))
}

// TestAMasterKeyNobodySignedIsRefused covers the same rule from the other end: the master key is the
// root of the whole chain, and an unsigned one is a root nobody has any reason to believe.
func TestAMasterKeyNobodySignedIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "unsigned", "pw-unsigned", false)

	response := uploadCrossSigningKeys(t, alice, server, map[string]any{
		"master_key": json.RawMessage(harness.UnsignedDocument(t, alice.UserID, harness.NewSigningKey(t))),
	})
	response.MustError(t, http.StatusBadRequest, string(matrix.ErrInvalidSignature))
}

// TestReplacingAnIdentityAsksFirstAndThenTakesIt is the second half of the MSC3967 rule: the first
// identity an account publishes is a thing the account owner just did, and the second is a thing
// somebody is claiming.
//
// Both halves matter to a client. A setup flow that demanded a password for a brand-new account asks
// for credentials nobody has just typed; a replacement that asked for none would let a stolen token
// take over an identity other people are already trusting.
func TestReplacingAnIdentityAsksFirstAndThenTakesIt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "rotator", "pw-rotator", false)

	first := newIdentity(t)
	mustUploadCrossSigningKeys(t, alice, server, first)

	// The same bundle again, which is what a client that never saw the answer does. No challenge,
	// because nothing is being changed.
	uploadCrossSigningKeys(t, alice, server, first.bundle(t, alice.UserID)).MustStatus(t, http.StatusOK)

	second := newIdentity(t)
	unauthenticated := uploadCrossSigningKeys(t, alice, server, second.bundle(t, alice.UserID))
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replacing an identity without proving who you are: got %d (%s), want 401 with a challenge",
			unauthenticated.StatusCode, unauthenticated.Body)
	}
	var challenge struct {
		Flows []struct {
			Stages []string `json:"stages"`
		} `json:"flows"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(unauthenticated.Body, &challenge); err != nil {
		t.Fatalf("decoding the auth challenge: %v\nbody: %s", err, unauthenticated.Body)
	}
	if challenge.Session == "" || len(challenge.Flows) == 0 {
		t.Fatalf("the auth challenge names no flows or session: %s", unauthenticated.Body)
	}
	found := false
	for _, flow := range challenge.Flows {
		for _, stage := range flow.Stages {
			if stage == "m.login.password" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the auth challenge offers no password stage: %s", unauthenticated.Body)
	}

	authenticated := uploadCrossSigningKeys(t, alice, server, map[string]any{
		"master_key":       json.RawMessage(harness.MasterKey(t, alice.UserID, second.Master, second.Device)),
		"self_signing_key": json.RawMessage(harness.SelfSigningKey(t, alice.UserID, second.Master, second.SelfSigning)),
		"auth": map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]string{"type": "m.id.user", "user": "rotator"},
			"password":   "pw-rotator",
		},
	})
	authenticated.MustStatus(t, http.StatusOK)

	// And the replaced key is the one on file: an identity the user retired has to be gone, or the
	// old key keeps being handed out to everyone who asks.
	keys := keyQuery(t, alice, server, alice.UserID)
	masters, _ := asMap(keys, "master_keys")
	document, _ := json.Marshal(masters[alice.UserID])
	if !strings.Contains(string(document), second.Master.PublicKey) {
		t.Errorf("after a replacement the server still answers with the old master key: %s", document)
	}
}

// TestACrossSigningKeyNamedAfterADeviceIsRefused protects the one thing a client has to tell apart:
// the devices of a user, and the keys that vouch for them. Both are listed in the same `keys` map of
// a key query answer, and they are told apart by their names — so a cross-signing key whose name is
// also one of the user's device IDs is not a naming accident, it is a device pretending to be an
// identity, or the other way round.
func TestACrossSigningKeyNamedAfterADeviceIsRefused(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "collider", "pw-collider", false)

	response := uploadCrossSigningKeys(t, alice, server, map[string]any{
		"master_key": json.RawMessage(harness.KeyDocumentWithID(t, alice.UserID, "ed25519:"+alice.DeviceID)),
	})
	response.MustError(t, http.StatusForbidden, string(matrix.ErrForbidden))
}

// TestASelfSignedDeviceIsAcceptedAndReported is the request that turns a device from "a thing that
// claims to be alice's" into "a thing alice's identity says is hers".
//
// It is the third call of the bootstrap a client makes, and without it a freshly created device is
// shown to every other user as unverified forever, no matter how carefully its owner set their
// identity up.
func TestASelfSignedDeviceIsAcceptedAndReported(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "signer", "pw-signer", false)
	bob := server.Register(t, "witness", "pw-witness", false)

	upload(t, alice, server, map[string]any{
		"device_keys":   deviceKeysFor(alice.UserID, alice.DeviceID),
		"one_time_keys": oneTimeKeysSet(1),
	})
	who := newIdentity(t)
	mustUploadCrossSigningKeys(t, alice, server, who)

	response := uploadSignatures(t, alice, server, map[string]any{
		alice.UserID: map[string]any{alice.DeviceID: harness.DeviceKeyDocument(t, alice.UserID, alice.DeviceID, who.SelfSigning)},
	})
	response.MustStatus(t, http.StatusOK)
	var answered struct {
		Failures map[string]map[string]any `json:"failures"`
	}
	response.JSON(t, &answered)
	if len(answered.Failures) > 0 {
		t.Fatalf("POST /keys/signatures/upload refused a signature alice made with her own self-signing key: %s", response.Body)
	}

	// The signature has to come back out again, or storing it achieved nothing: it is how a client
	// that has never met this device decides it belongs to the person it is talking about.
	keys := keyQuery(t, bob, server, alice.UserID)
	devices, _ := asMap(keys, "device_keys")
	mine, _ := devices[alice.UserID].(map[string]any)
	document, _ := json.Marshal(mine[alice.DeviceID])
	if !strings.Contains(string(document), "ed25519:"+who.SelfSigning.PublicKey) {
		t.Errorf("POST /keys/query answers with a device key carrying no self-signing signature: %s", document)
	}
}

// TestSigningAnotherPersonTakesAUserSigningKeyAndNothingElse is what a completed user verification
// leaves behind on the server: the target's master key, carrying a signature made by the verifier's
// user-signing key.
//
// Two rules make this worth having. The signature must come from the *verifier's* user-signing key,
// because that is the key that says whom they checked; and it must cover the master key actually on
// file, because a server that stored signatures over whatever document it was handed would let
// anybody invent a person's identity and have it endorsed in the same request.
func TestSigningAnotherPersonTakesAUserSigningKeyAndNothingElse(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "endorser", "pw-endorser", false)
	bob := server.Register(t, "endorsed", "pw-endorsed", false)

	aliceIdentity, bobIdentity := newIdentity(t), newIdentity(t)
	mustUploadCrossSigningKeys(t, alice, server, aliceIdentity)
	mustUploadCrossSigningKeys(t, bob, server, bobIdentity)

	endorsement := harness.SignAnotherUsersMasterKey(t, bob.UserID, bobIdentity.Master, alice.UserID, aliceIdentity.UserSigning)
	response := uploadSignatures(t, alice, server, map[string]any{
		bob.UserID: map[string]any{bobIdentity.Master.ID: endorsement},
	})
	response.MustStatus(t, http.StatusOK)
	if body := string(response.Body); strings.Contains(body, "M_INVALID_SIGNATURE") {
		t.Fatalf("POST /keys/signatures/upload refused a valid endorsement: %s", response.Body)
	}

	seen := keyQuery(t, bob, server, bob.UserID)
	masters, _ := asMap(seen, "master_keys")
	document, _ := json.Marshal(masters[bob.UserID])
	if !strings.Contains(string(document), "ed25519:"+aliceIdentity.UserSigning.PublicKey) {
		t.Errorf("bob's master key carries no endorsement from alice: %s", document)
	}
}

// TestYouCannotSignSomebodyElsesDevice closes the door on the obvious abuse of the same endpoint: a
// device that anybody could sign is a device that means nothing.
//
// The refusal is per key rather than per request, because a client posts a batch of signatures and
// handles the ones that did not land on their own.
func TestYouCannotSignSomebodyElsesDevice(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "willsigner", "pw-willsigner", false)
	bob := server.Register(t, "neighbour", "pw-neighbour", false)

	upload(t, bob, server, map[string]any{
		"device_keys":   deviceKeysFor(bob.UserID, bob.DeviceID),
		"one_time_keys": oneTimeKeysSet(1),
	})
	aliceIdentity := newIdentity(t)
	mustUploadCrossSigningKeys(t, alice, server, aliceIdentity)

	response := uploadSignatures(t, alice, server, map[string]any{
		bob.UserID: map[string]any{bob.DeviceID: harness.DeviceKeyDocument(t, bob.UserID, bob.DeviceID, aliceIdentity.SelfSigning)},
	})
	response.MustStatus(t, http.StatusOK)
	var answered struct {
		Failures map[string]map[string]struct {
			ErrCode string `json:"errcode"`
		} `json:"failures"`
	}
	response.JSON(t, &answered)
	if _, reported := answered.Failures[bob.UserID][bob.DeviceID]; !reported {
		t.Fatalf("POST /keys/signatures/upload accepted alice's signature for bob's device: %s", response.Body)
	}

	// And bob's device key is still exactly what bob published: a refused signature is not stored
	// while it is being refused.
	keys := keyQuery(t, alice, server, bob.UserID)
	devices, _ := asMap(keys, "device_keys")
	mine, _ := devices[bob.UserID].(map[string]any)
	document, _ := json.Marshal(mine[bob.DeviceID])
	if strings.Contains(string(document), "ed25519:"+aliceIdentity.SelfSigning.PublicKey) {
		t.Errorf("a refused signature still landed on bob's device key: %s", document)
	}
}

// TestAPublishedIdentityIsAnnounced is the difference between an identity that exists and one that
// anybody hears about.
//
// A client does not re-query a person's keys because it feels like it; it does so because a /sync
// told it that the device list moved. Without that announcement a user who resets their identity is
// invisible to the people who need to notice, and their clients keep encrypting to keys that were
// retired an hour ago — which reads to everybody involved as a broken conversation.
func TestAPublishedIdentityIsAnnounced(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "announcer", "pw-announcer", false)
	bob := server.Register(t, "notified", "pw-notified", false)

	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "trusted_private_chat", "invite": []string{bob.UserID}})
	bob.MustJoinRoom(t, server, roomID)
	bob.MustSync(t, server, url.Values{})

	ownSince := ""
	bobsSince := ""
	mustUploadCrossSigningKeys(t, alice, server, newIdentity(t))

	// Bob first: the person whose view of alice just changed.
	syncDeviceLists(t, bob, server, &bobsSince, "changed", alice.UserID)
	// And alice's own other devices, which are the ones that have to start trusting the new identity
	// before they can sign anything with it.
	syncDeviceLists(t, alice, server, &ownSince, "changed", alice.UserID)
}
