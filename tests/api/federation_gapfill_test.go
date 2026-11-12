package api_test

// Gap filling: a peer sends an event whose history this server has never been told about.
//
// Refusing such an event is right on its own terms and terminal in practice. The peer has already sent
// what it has; the two servers then sit at different places in the same room, and every later event
// widens the difference. So the refusal names what is missing, and the transport offers that
// question back to the peer that reported the event — once per missing event, up to a ceiling, because
// a peer can always claim to be missing history and can also make this server fetch forever.
//
// What these tests pin is that the fetch happens, that it is bounded, and that a failure to fill the
// gap ends in the original refusal rather than in the event being stored anyway. Storing an event that
// does not attach to known history is how a room ends up with sections its own members cannot read.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// peerMessages is a remote user sending two messages in a room, the second following the first.
type peerMessages struct {
	hidden gomatrixserverlib.PDU
	gap    gomatrixserverlib.PDU
}

func twoMessagesBackToBack(t *testing.T, server *harness.Server, alice *harness.Account, peer *fakePeer, roomID string, join gomatrixserverlib.PDU, version string) peerMessages {
	t.Helper()

	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)
	auth := []string{
		state["m.room.create\x00"],
		state["m.room.power_levels\x00"],
		join.EventID(),
	}
	hidden := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.message",
		Depth:      join.Depth() + 1,
		PrevEvents: []string{join.EventID()},
		AuthEvents: auth,
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "the event that was not sent"})),
	})
	gap := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.message",
		Depth:      join.Depth() + 2,
		PrevEvents: []string{hidden.EventID()},
		AuthEvents: auth,
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "the event that was sent"})),
	})
	return peerMessages{hidden: hidden, gap: gap}
}

func TestAWindowOnThePeerIsFilledFromThePeer(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	messages := twoMessagesBackToBack(t, server, alice, peer, roomID, join, version)

	// The peer holds both messages and sends only the second. That is not a hypothetical: a peer that
	// reconnects after a while away often has a longer run of history than this server has seen.
	peer.serves = func(w http.ResponseWriter, r *http.Request) bool {
		const prefix = "/_matrix/federation/v1/event/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			return false
		}
		want, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, prefix))
		if err != nil || want != messages.hidden.EventID() {
			return false
		}
		raw, err := json.Marshal(map[string]any{
			"origin": string(peer.serverNo),
			"pdus":   []json.RawMessage{json.RawMessage(messages.hidden.JSON())},
		})
		if err != nil {
			t.Fatalf("encoding the fetched event: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return true
	}

	results, raw := sendTransaction(t, server, peer,
		[]json.RawMessage{json.RawMessage(messages.gap.JSON())}, harness.Nonce("gap"))
	if result, ok := results[messages.gap.EventID()]; !ok {
		t.Fatalf("the answer says nothing about the event it was sent (%s)", raw)
	} else if result.Error != "" {
		t.Fatalf("an event was refused for missing history that the peer could supply: %s", result.Error)
	}

	// Both halves have to end up in the timeline. Accepting the second while the first stays on the
	// peer would leave this server with a message whose predecessor it still does not have — the gap
	// moved rather than filled, and a client paginating back would fall through it.
	alice.MustTimelineHas(t, server, roomID, "m.room.message", "the event that was sent")
	alice.MustTimelineHas(t, server, roomID, "m.room.message", "the event that was not sent")

	// And exactly one request: the room server named one missing event, so one event was asked for.
	// A loop that re-asks for what it already has is how gap filling becomes a way to hammer a peer.
	if hits := peer.otherHits.Load(); hits != 1 {
		t.Errorf("filling one gap took %d requests to the peer, want 1", hits)
	}
}

func TestAGapNobodyCanFillEndsInARefusal(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")
	messages := twoMessagesBackToBack(t, server, alice, peer, roomID, join, version)

	// The peer cannot supply the missing event. The room must not gain the event anyway.
	var asked atomic.Int64
	peer.serves = func(_ http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/event/") {
			asked.Add(1)
		}
		return false
	}

	results, raw := sendTransaction(t, server, peer,
		[]json.RawMessage{json.RawMessage(messages.gap.JSON())}, harness.Nonce("unfillable"))
	result, ok := results[messages.gap.EventID()]
	if !ok {
		t.Fatalf("the answer says nothing about the event it was sent (%s)", raw)
	}
	if result.Error == "" {
		t.Fatal("an event was accepted while the history it follows is nowhere to be found")
	}
	if !strings.Contains(result.Error, messages.hidden.EventID()) {
		t.Errorf("the refusal should name what is missing so a peer can act on it; said %q", result.Error)
	}
	if asked.Load() == 0 {
		t.Error("the server never asked the peer for the event it was missing")
	}
	if asked.Load() > 4 {
		t.Errorf("the server made %d requests for an event it could not get; it should stop after one",
			asked.Load())
	}
}

// TestAGapOfSeveralEventsIsFilledInOneRequest is the case a one-event-at-a-time fetch cannot close.
//
// A gap is usually a run: a peer reconnects after a while away and sends its newest event, whose
// history this server has never seen. Filling that by asking for one event per round means a request
// per event and a bound that turns a long gap into a permanent refusal, so the bulk endpoint is tried
// first — and this test serves the missing events in reverse order on purpose, because a batch is only
// admissible in dependency order and a server that stores them shallowest-first needs no second trip.
func TestAGapOfSeveralEventsIsFilledInOneRequest(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")

	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)
	auth := []string{
		state["m.room.create\x00"],
		state["m.room.power_levels\x00"],
		join.EventID(),
	}

	// Four events the peer holds and does not send, then the one it does send.
	var hidden []gomatrixserverlib.PDU
	previous := join.EventID()
	depth := join.Depth()
	for _, body := range []string{"first missing", "second missing", "third missing", "fourth missing"} {
		depth++
		event := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
			RoomID:     roomID,
			SenderID:   remote,
			Type:       "m.room.message",
			Depth:      depth,
			PrevEvents: []string{previous},
			AuthEvents: auth,
			Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": body})),
		})
		hidden = append(hidden, event)
		previous = event.EventID()
	}
	last := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.message",
		Depth:      depth + 1,
		PrevEvents: []string{previous},
		AuthEvents: auth,
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "the event that was sent"})),
	})

	var asked atomic.Int64
	earliest, latest := "", ""
	peer.serves = func(w http.ResponseWriter, r *http.Request) bool {
		const prefix = "/_matrix/federation/v1/get_missing_events/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			return false
		}
		asked.Add(1)
		var request struct {
			EarliestEvents []string `json:"earliest_events"`
			LatestEvents   []string `json:"latest_events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("the gap-fill request cannot be read: %v", err)
		}
		if len(request.EarliestEvents) > 0 {
			earliest = request.EarliestEvents[0]
		}
		if len(request.LatestEvents) > 0 {
			latest = request.LatestEvents[0]
		}
		// Newest first: the answer arrives in the order a naive server would store it.
		raw, err := json.Marshal(map[string]any{
			"events": reversedJSON(hidden),
		})
		if err != nil {
			t.Fatalf("encoding the missing events: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return true
	}

	results, raw := sendTransaction(t, server, peer,
		[]json.RawMessage{json.RawMessage(last.JSON())}, harness.Nonce("chain"))
	if result, ok := results[last.EventID()]; !ok {
		t.Fatalf("the answer says nothing about the event it was sent (%s)", raw)
	} else if result.Error != "" {
		t.Fatalf("an event was refused while the peer was willing to supply its history: %s", result.Error)
	}

	// The question asked was the useful one: from somewhere this server holds, to the event the peer
	// sent. A gap-fill request with no anchor is a request for the whole room.
	if earliest == "" || earliest != join.EventID() {
		t.Errorf("the gap was asked about from %q, want the join this server holds (%s)", earliest, join.EventID())
	}
	if latest != last.EventID() {
		t.Errorf("the gap was asked about up to %q, want %q", latest, last.EventID())
	}

	// The whole run has to be here, not just the event that was sent. A room whose newest event knows
	// it exists but whose members cannot read what came before it is a room with holes in it.
	for _, body := range []string{"first missing", "fourth missing", "the event that was sent"} {
		alice.MustTimelineHas(t, server, roomID, "m.room.message", body)
	}
	if hits := asked.Load(); hits != 1 {
		t.Errorf("filling a four-event gap took %d bulk requests, want 1", hits)
	}
}

// reversedJSON returns events newest-first, which is the order that makes a server prove it can put
// them back in the right order itself.
func reversedJSON(events []gomatrixserverlib.PDU) []json.RawMessage {
	raw := make([]json.RawMessage, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		raw = append(raw, json.RawMessage(events[i].JSON()))
	}
	return raw
}

// TestAGapThePeerCanOnlyPartlyBridgeIsStillRefused states the boundary of gap filling, because the
// alternative is believing this server can close gaps it cannot.
//
// A peer may honestly hold the newest half of a run and have forgotten the older half — retention is a
// legitimate choice. The events it can supply then still do not attach to anything this server holds, so
// storing them would put a detached fragment in the room: an event whose predecessors are unreachable is
// a branch nothing can walk back to, and clients paginating from it fall into a hole.
//
// The real answer to a gap nobody can bridge is not a different gap-filling policy. It is asking the
// peer for the *state* at the event (`/state_ids`, `/state`) and persisting the event with that state
// despite the broken graph — outlier handling, which this server does not implement. Until it does, the
// refusal stands, and this test is the marker that the refusal is a decision with a named successor
// rather than an oversight.
func TestAGapThePeerCanOnlyPartlyBridgeIsStillRefused(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")

	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)
	auth := []string{
		state["m.room.create\x00"],
		state["m.room.power_levels\x00"],
		join.EventID(),
	}

	// A chain of four, of which the peer will only admit to the last two.
	var chain []gomatrixserverlib.PDU
	previous := join.EventID()
	depth := join.Depth()
	for _, body := range []string{"forgotten", "also forgotten", "remembered", "also remembered"} {
		depth++
		event := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
			RoomID:     roomID,
			SenderID:   remote,
			Type:       "m.room.message",
			Depth:      depth,
			PrevEvents: []string{previous},
			AuthEvents: auth,
			Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": body})),
		})
		chain = append(chain, event)
		previous = event.EventID()
	}
	last := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.message",
		Depth:      depth + 1,
		PrevEvents: []string{previous},
		AuthEvents: auth,
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "the event that was sent"})),
	})

	peer.serves = func(w http.ResponseWriter, r *http.Request) bool {
		const prefix = "/_matrix/federation/v1/get_missing_events/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			return false
		}
		// The two events it remembers, which hang from an event it no longer has.
		raw, err := json.Marshal(map[string]any{
			"events": []json.RawMessage{
				json.RawMessage(chain[2].JSON()),
				json.RawMessage(chain[3].JSON()),
			},
		})
		if err != nil {
			t.Fatalf("encoding the partial answer: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return true
	}

	results, raw := sendTransaction(t, server, peer,
		[]json.RawMessage{json.RawMessage(last.JSON())}, harness.Nonce("half-bridge"))
	result, ok := results[last.EventID()]
	if !ok {
		t.Fatalf("the answer says nothing about the event it was sent (%s)", raw)
	}
	if result.Error == "" {
		t.Fatal("an event was stored on top of a fragment of history that reaches nowhere")
	}

	// And the fragment must not have been kept either. Half a bridge is not half a room: the stored
	// events would be visible to local users with a hole underneath them.
	for _, event := range alice.MustSync(t, server, nil).Rooms.Join[roomID].Timeline.Events {
		var candidate struct {
			Content struct {
				Body string `json:"body"`
			} `json:"content"`
		}
		if err := json.Unmarshal(event, &candidate); err != nil {
			t.Fatalf("a timeline event cannot be read: %v", err)
		}
		switch candidate.Content.Body {
		case "remembered", "also remembered", "the event that was sent":
			t.Errorf("an event hung from unreachable history reached the timeline: %q", candidate.Content.Body)
		}
	}
}

// TestAGapWiderThanOneAnswerIsFilledFromThePeer is the case a peer that truncates its own answers
// creates, and the reason gap filling is a loop with a budget rather than a fixed number of tries.
//
// A peer asked for a gap answers with what it can carry — complement's own federation mock answers
// with the last ten events of the run it was asked about, which is a reasonable thing for a homeserver
// to do too. The gap is twenty-five events deep, so one round cannot close it, and neither can a fixed
// five: what matters is that every round that stores something keeps going, and that a round which
// stores nothing is where the loop stops. Bounding the *number* of rounds instead of the work done is
// how a server ends up refusing a gap it could have filled with two more requests.
//
// The `/event` fallback carries this scenario: after the bulk window is exhausted the room server names
// one missing event, the transport asks for exactly that one, and the walk continues.
func TestAGapWiderThanOneAnswerIsFilledFromThePeer(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithFederationInsecureTLS())
	alice := server.Register(t, "host", "pw-host", false)
	peer := newFakePeer(t)
	roomID, join, version := joinedRoom(t, server, alice, peer, "public_chat")

	remote := "@charlie:" + string(peer.serverNo)
	state := remoteStateEventIDs(t, server, alice, roomID)
	auth := []string{
		state["m.room.create\x00"],
		state["m.room.power_levels\x00"],
		join.EventID(),
	}

	const hiddenCount = 25
	var hidden []gomatrixserverlib.PDU
	previous := join.EventID()
	depth := join.Depth()
	for i := 0; i < hiddenCount; i++ {
		depth++
		event := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
			RoomID:     roomID,
			SenderID:   remote,
			Type:       "m.room.message",
			Depth:      depth,
			PrevEvents: []string{previous},
			AuthEvents: auth,
			Content: spec.RawJSON(mustMarshal(t, map[string]any{
				"msgtype": "m.text", "body": harness.Nonce("hidden"),
			})),
		})
		hidden = append(hidden, event)
		previous = event.EventID()
	}
	// The event that is actually sent: it follows the last hidden one, so this server cannot place it
	// until the whole run in front of it has been fetched.
	last := buildRemoteEvent(t, peer, version, gomatrixserverlib.ProtoEvent{
		RoomID:     roomID,
		SenderID:   remote,
		Type:       "m.room.message",
		Depth:      depth + 1,
		PrevEvents: []string{previous},
		AuthEvents: auth,
		Content:    spec.RawJSON(mustMarshal(t, map[string]any{"msgtype": "m.text", "body": "the event that was sent"})),
	})

	var bulkAsked, singleAsked atomic.Int64
	peer.serves = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/get_missing_events/"):
			bulkAsked.Add(1)
			// The last ten, whatever was asked for — a window, not an answer.
			window := hidden
			if len(window) > 10 {
				window = window[len(window)-10:]
			}
			raw, err := json.Marshal(map[string]any{"events": reversedJSON(window)})
			if err != nil {
				t.Fatalf("encoding the window: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
			return true
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/event/"):
			singleAsked.Add(1)
			want, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/_matrix/federation/v1/event/"))
			if err != nil {
				return false
			}
			for _, event := range hidden {
				if event.EventID() != want {
					continue
				}
				raw, err := json.Marshal(map[string]any{
					"origin": string(peer.serverNo),
					"pdus":   []json.RawMessage{json.RawMessage(event.JSON())},
				})
				if err != nil {
					t.Fatalf("encoding the fetched event: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
				return true
			}
			return false
		}
		return false
	}

	results, raw := sendTransaction(t, server, peer,
		[]json.RawMessage{json.RawMessage(last.JSON())}, harness.Nonce("long-gap"))
	if result, ok := results[last.EventID()]; !ok {
		t.Fatalf("the answer says nothing about the event it was sent (%s)", raw)
	} else if result.Error != "" {
		t.Fatalf("a %d-event gap was refused that the peer could bridge a little at a time: %s",
			hiddenCount, result.Error)
	}

	// Every hidden event has to be readable, or the gap has only been leapt over rather than filled:
	// a client paginating back from the newest event would still fall through the hole.
	held := pagedEventIDs(t, server, alice, roomID)
	for i, event := range hidden {
		if !held[event.EventID()] {
			t.Fatalf("hidden event %d of %d (%s) is not readable in the room's history",
				i+1, hiddenCount, event.EventID())
		}
	}
	if !held[last.EventID()] {
		t.Fatalf("the event that was actually sent is not readable in the room's history")
	}

	// Bounded work: fetching every event one at a time is legitimate, fetching forever is not. The
	// ceiling is loose on purpose — it is here to catch a loop that lost its stop condition, not to
	// tune a policy.
	if asked := bulkAsked.Load() + singleAsked.Load(); asked > 60 {
		t.Fatalf("filling a %d-event gap took %d requests to the peer, which is not bounded work",
			hiddenCount, asked)
	}
}

// pagedEventIDs walks a room's whole history the way a client does — `/messages` from the start of the
// room, forward, page by page — and returns the event IDs it found. Reading history rather than the
// latest sync batch is what makes this a test of the stored graph: a gap that exists in the room would
// show up as events nobody can reach.
func pagedEventIDs(t *testing.T, server *harness.Server, alice *harness.Account, roomID string) map[string]bool {
	t.Helper()
	escaped := harness.PathEscape(roomID)
	found := map[string]bool{}
	// Walking backwards from the tip is what a client does with no token at all: an absent `from`
	// means "from where the room is now", and each page's `end` is the next question.
	from := ""
	for page := 0; page < 20; page++ {
		var batch struct {
			Chunk []struct {
				EventID string `json:"event_id"`
			} `json:"chunk"`
			End string `json:"end"`
		}
		path := "/_matrix/client/v3/rooms/" + escaped + "/messages?dir=b&limit=50"
		if from != "" {
			path += "&from=" + from
		} else {
			path += "&from=-1"
		}
		response := alice.Do(t, server, harness.Request{Method: http.MethodGet, Path: path})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("reading history from %s: %d %s", from, response.StatusCode, response.Body)
		}
		if err := json.Unmarshal([]byte(response.Body), &batch); err != nil {
			t.Fatalf("reading history: %v (%s)", err, response.Body)
		}
		for _, event := range batch.Chunk {
			found[event.EventID] = true
		}
		if batch.End == "" || batch.End == from || len(batch.Chunk) == 0 {
			return found
		}
		from = batch.End
	}
	return found
}
