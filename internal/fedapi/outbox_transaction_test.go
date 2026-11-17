package fedapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
)

// TestAnEphemeralOnlyTransactionStillNamesItsEventLists is about a word on the wire, not a behaviour.
//
// The specification types a transaction's `pdus` as an array, and gomatrixserverlib marshals a nil slice
// as JSON `null` — so every transaction carrying only ephemeral events announced `"pdus": null`, which is
// not an empty list but a value of the wrong type. Synapse reads it as nothing to deliver and moves on; a
// server that deserialises what it was handed answers 400 M_BAD_JSON — `invalid type: null, expected a
// sequence` — and the device-list update, or the end-to-end key share, never arrives. The sender's log is
// the only place that says it was ever attempted, and the recipient's is the only place that says why.
//
// It survived every test this repository has because every one of them federates with this same
// codebase, which is as lenient about its own mistake as Synapse is.
func TestAnEphemeralOnlyTransactionStillNamesItsEventLists(t *testing.T) {
	var received map[string]json.RawMessage
	asked := 0
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/send/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		asked++
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Decode the way a strict peer does: a field that must be an array is read as one, and a
		// `null` there is an error rather than an empty list.
		received = map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &received); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, field := range []string{"pdus", "edus"} {
			if value, present := received[field]; present && !json.Valid(value) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		if string(received["pdus"]) == "null" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errcode":"M_BAD_JSON","error":"invalid type: null, expected a sequence"}`))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pdus":{}}`))
	}))
	defer peerServer.Close()
	destination := strings.TrimPrefix(peerServer.URL, "https://")

	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the signing key: %v", err)
	}
	peer, err := NewPeer(PeerOptions{
		ServerName:            "ours.test",
		KeyID:                 "ed25519:ours",
		PrivateKey:            private,
		InsecureSkipTLSVerify: true,
	})
	if err != nil {
		t.Fatalf("building the peer client: %v", err)
	}

	outbox := NewOutbox(peer, nil, "ours.test")
	if err := outbox.SendDeviceListUpdate(context.Background(), destination,
		json.RawMessage(`{"user_id":"@alice:ours.test","device_id":"DEVICE"}`)); err != nil {
		t.Fatalf("a peer that accepts the body refused it: %v", err)
	}
	if asked != 1 {
		t.Fatalf("the peer was asked %d times, want 1", asked)
	}

	if value, present := received["pdus"]; !present {
		t.Error("the transaction omits `pdus`, which the specification requires it to name")
	} else if string(value) != "[]" {
		t.Errorf("`pdus` is %s; an ephemeral-only transaction has no room events, and empty is what it has", value)
	}
	var edus []gomatrixserverlib.EDU
	if err := json.Unmarshal(received["edus"], &edus); err != nil {
		t.Fatalf("`edus` is not the array it promises (%s): %v", received["edus"], err)
	}
	if len(edus) != 1 || edus[0].Type != deviceListUpdateEDU {
		t.Errorf("the device-list update is not in the transaction: %+v", edus)
	}
}

// TestATransactionOfEventsOnlyLeavesTheEphemeralListAlone is the other half: `edus` is optional, and
// optional means absent or an array — never the wrong type either.
func TestATransactionOfEventsOnlyLeavesTheEphemeralListAlone(t *testing.T) {
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the signing key: %v", err)
	}
	peer, err := NewPeer(PeerOptions{ServerName: "ours.test", KeyID: "ed25519:ours", PrivateKey: private})
	if err != nil {
		t.Fatalf("building the peer client: %v", err)
	}
	outbox := NewOutbox(peer, nil, "ours.test")

	for _, check := range []struct {
		name     string
		pdus     []json.RawMessage
		edus     []gomatrixserverlib.EDU
		wantPDUs string
		wantEDUs bool
	}{
		{"neither list", nil, nil, "[]", false},
		{"events only", []json.RawMessage{json.RawMessage(`{}`)}, nil, "[{}]", false},
		{"ephemeral only", nil, []gomatrixserverlib.EDU{{Type: "m.any"}}, "[]", true},
	} {
		built := outbox.newTransaction("peer.test", check.pdus, check.edus)
		raw, err := json.Marshal(built)
		if err != nil {
			t.Fatalf("%s: marshalling the transaction: %v", check.name, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("%s: reading the transaction back: %v", check.name, err)
		}
		if got := string(fields["pdus"]); got != check.wantPDUs {
			t.Errorf("%s: `pdus` is %s, want %s", check.name, got, check.wantPDUs)
		}
		value, present := fields["edus"]
		if present != check.wantEDUs {
			t.Errorf("%s: `edus` present = %v (%s), want %v", check.name, present, value, check.wantEDUs)
		}
		if present && strings.HasPrefix(string(value), "null") {
			t.Errorf("%s: `edus` is null, and null is a type error to a peer that reads it strictly", check.name)
		}
	}
}
