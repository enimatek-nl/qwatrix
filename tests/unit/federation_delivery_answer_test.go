package unit_test

import (
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/fedapi"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/matrix-org/gomatrixserverlib/fclient"
)

// A peer answers `PUT /_matrix/federation/v1/send/{txnId}` with 200 whatever it did with the events
// inside, because the per-event answers live in the body: `{"pdus": {"$a": {}, "$b": {"error": "..."}}}`.
// Reading the status line as the verdict therefore reports success for an event the peer refused — and
// a sender that then clears its queue has destroyed the only copy of an event it is owed, with no retry
// and nothing in the log to connect to the room that is missing it.
//
// So the queue may only be cleared per event, on that event's own answer.
func TestATransactionAnswerClearsOnlyTheEventsItAccepted(t *testing.T) {
	rows := []store.FederationOutbox{
		{ID: 1, EventID: "$taken", Destination: "peer.example"},
		{ID: 2, EventID: "$refused", Destination: "peer.example"},
		{ID: 3, EventID: "$unmentioned", Destination: "peer.example"},
	}
	answer := fclient.RespSend{PDUs: map[string]fclient.PDUResult{
		"$taken":   {},
		"$refused": {Error: "this server is not in the room"},
		// "$unmentioned" is absent: a peer that did not answer for an event did not take it.
	}}

	delivered, refused, reason := fedapi.SplitDelivery(rows, answer)

	if len(delivered) != 1 || delivered[0] != 1 {
		t.Errorf("cleared %v for delivery, want only the one event the peer accepted", delivered)
	}
	if len(refused) != 2 {
		t.Fatalf("kept %v queued, want the refused event and the unanswered one: %v", refused, refused)
	}
	if reason == "" {
		t.Error("a refusal that reaches nobody's log is a room with a missing event and no explanation")
	}
}

// An older peer answers in the legacy unsigned form, and a peer that answers with no `pdus` object at
// all has not confirmed anything. Neither may be read as a delivery.
func TestAnEmptyTransactionAnswerClearsNothing(t *testing.T) {
	rows := []store.FederationOutbox{{ID: 7, EventID: "$one", Destination: "peer.example"}}

	delivered, refused, _ := fedapi.SplitDelivery(rows, fclient.RespSend{})

	if len(delivered) != 0 {
		t.Errorf("an answer naming no events cleared %v anyway", delivered)
	}
	if len(refused) != 1 || refused[0] != 7 {
		t.Errorf("kept %v, want the single queued event", refused)
	}
}
