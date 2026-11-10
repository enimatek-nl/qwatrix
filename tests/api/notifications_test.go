package api_test

import (
	"net/url"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// The unread badge is the one part of a sync response a person sees without reading a message, so
// these tests are written against what the badge has to say rather than against how it is computed.
//
// The three rules that make it a real badge and not a message counter:
//
//   - it follows read receipts, not sync tokens: the badge says what the *person* has not seen, and
//     a second device syncing does not make it shorter;
//   - it counts notifications, not events, so a rename, a reaction, or the reader's own words are
//     not things to come back to;
//   - a mention is worth more than a message, which is why the highlight count is reported apart.

// sendContent writes one m.room.message with arbitrary content and returns its event ID.
func sendContent(t *testing.T, server *harness.Server, who *harness.Account, roomID string, content map[string]any) string {
	t.Helper()

	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/send/m.room.message/" +
		harness.Nonce("notif")
	// The caller's content is sent as written, so that a test asking for a thread reply or a
	// mention gets exactly the event it described rather than a text message with extras.
	resp := who.MustPutJSON(t, server, path, content)
	resp.MustStatus(t, 200)
	var body struct {
		EventID string `json:"event_id"`
	}
	resp.JSON(t, &body)
	if body.EventID == "" {
		t.Fatalf("sending a message returned no event_id: %v", content)
	}
	return body.EventID
}

// markRead moves the reader's read receipt to an event, optionally within one thread scope.
func markRead(t *testing.T, server *harness.Server, who *harness.Account, roomID, eventID, threadID string) {
	t.Helper()

	path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/receipt/m.read/" +
		harness.PathEscape(eventID)
	body := map[string]any{}
	if threadID != "" {
		body["thread_id"] = threadID
	}
	who.MustPostJSON(t, server, path, body).MustStatus(t, 200)
}

// syncWith pulls one sync, optionally with an inline filter, and returns it.
func syncWith(t *testing.T, server *harness.Server, who *harness.Account, since string, filter string) harness.SyncResponse {
	t.Helper()

	params := url.Values{"timeout": {"0"}}
	if since != "" {
		params.Set("since", since)
	}
	if filter != "" {
		params.Set("filter", filter)
	}
	return who.MustSync(t, server, params)
}

// roomWithMessages sets up a room where alice speaks after bob has joined, and returns the room ID
// together with a sync token from before any of it happened.
func roomWithMessages(t *testing.T, contents ...map[string]any) (string, *harness.Server, *harness.Account, *harness.Account, []string, string) {
	t.Helper()

	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)

	// A token taken once, after the join, so every later sync is incremental over the same
	// messages: the badge has to be the same on the first sync and the fifth.
	before := syncWith(t, server, bob, "", "")
	since := before.NextBatch

	ids := make([]string, 0, len(contents))
	for _, content := range contents {
		ids = append(ids, sendContent(t, server, alice, roomID, content))
	}
	return roomID, server, alice, bob, ids, since
}

func message(body string) map[string]any {
	return map[string]any{"msgtype": "m.text", "body": body}
}

// TestSyncCarriesAnUnreadBadge is the basic contract: three messages the reader has not read are a
// badge of three.
//
// The field must be *present* even at zero. A client that cannot tell "nothing unread" from "the
// server does not do this" falls back to counting the timeline itself, and gets it wrong on every
// room it has partially read.
func TestSyncCarriesAnUnreadBadge(t *testing.T) {
	roomID, server, _, bob, _, since := roomWithMessages(
		t, message("one"), message("two"), message("three"))

	resp := syncWith(t, server, bob, since, "")
	room, ok := resp.Rooms.Join[roomID]
	if !ok {
		t.Fatalf("the room is not in the sync: %v", resp.Rooms.Join)
	}
	if room.UnreadNotifications == nil {
		t.Fatalf("the joined room carries no unread_notifications at all, so a client cannot tell " +
			"nothing unread from no answer")
	}
	if room.UnreadNotifications.NotificationCount != 3 {
		t.Errorf("notification_count: got %d, want 3 unread messages", room.UnreadNotifications.NotificationCount)
	}
	if room.UnreadNotifications.HighlightCount != 0 {
		t.Errorf("highlight_count: got %d, want 0: nobody was mentioned", room.UnreadNotifications.HighlightCount)
	}

	// The badge follows receipts, not sync tokens: syncing again must not make it shorter.
	again := syncWith(t, server, bob, since, "")
	if got := again.Rooms.Join[roomID].UnreadNotifications; got == nil || got.NotificationCount != 3 {
		t.Errorf("a second sync changed the badge to %v; it counts what the person has read, not what the device has seen", got)
	}
}

// TestTheReaderOwnWordsAreNotAnErrandToReturnTo checks the one exclusion a person notices
// immediately: their own messages are never waiting for them.
func TestTheReaderOwnWordsAreNotAnErrandToReturnTo(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	since := syncWith(t, server, bob, "", "").NextBatch

	sendContent(t, server, alice, roomID, message("from alice"))
	sendContent(t, server, bob, roomID, message("from bob"))
	sendContent(t, server, alice, roomID, message("from alice again"))

	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil || room.UnreadNotifications.NotificationCount != 2 {
		t.Errorf("notification_count: got %v, want 2 — bob's own message must not wait for him",
			room.UnreadNotifications)
	}
}

// TestAMentionIsWorthMoreThanAMessage is the difference between a badge and a dot.
//
// Being mentioned is a highlight; the mention arrives as `m.mentions.user_ids` (the modern form) and,
// for older clients, as a bare user ID in the body.
func TestAMentionIsWorthMoreThanAMessage(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	since := syncWith(t, server, bob, "", "").NextBatch

	sendContent(t, server, alice, roomID, message("just talking"))
	sendContent(t, server, alice, roomID, map[string]any{
		"msgtype":    "m.text",
		"body":       "over here",
		"m.mentions": map[string]any{"user_ids": []string{bob.UserID}},
	})

	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 2 {
		t.Errorf("notification_count: got %d, want 2", room.UnreadNotifications.NotificationCount)
	}
	if room.UnreadNotifications.HighlightCount != 1 {
		t.Errorf("highlight_count: got %d, want 1 — the mention is the one worth coming back for",
			room.UnreadNotifications.HighlightCount)
	}
}

// TestAReceiptClearsWhatItCovers checks the receipt is a position, not a switch.
func TestAReceiptClearsWhatItCovers(t *testing.T) {
	roomID, server, _, bob, ids, since := roomWithMessages(
		t, message("one"), message("two"), message("three"))

	markRead(t, server, bob, roomID, ids[1], "")
	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 1 {
		t.Errorf("notification_count: got %d, want 1 — the receipt was put on the second of three",
			room.UnreadNotifications.NotificationCount)
	}
	if room.UnreadNotifications.HighlightCount != 0 {
		t.Errorf("highlight_count: got %d, want 0", room.UnreadNotifications.HighlightCount)
	}
}

// TestWhatIsNotWaitingForYou checks the categories that must not raise a badge.
//
// A rename describes the room rather than asking anything of the reader; a reaction is a acknowledgement
// of a message they already saw; a notice is a bot talking, which every client deliberately dims.
func TestWhatIsNotWaitingForYou(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	since := syncWith(t, server, bob, "", "").NextBatch

	messageID := sendContent(t, server, alice, roomID, message("a real message"))
	// A reaction to it.
	alice.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/send/m.reaction/"+harness.Nonce("react"), map[string]any{
		"m.relates_to": map[string]any{
			"rel_type": "m.annotation", "event_id": messageID, "key": "👍",
		},
	}).MustStatus(t, 200)
	// A notice: the shape bots use so that clients may dim them.
	sendContent(t, server, alice, roomID, map[string]any{"msgtype": "m.notice", "body": "build finished"})
	// And a state change: the room was renamed.
	alice.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/state/m.room.name", map[string]any{"name": "Renamed"}).MustStatus(t, 200)

	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 1 {
		t.Errorf("notification_count: got %d, want 1 — only the real message is an errand; the room was "+
			"renamed, reacted to, and notified (%+v)", room.UnreadNotifications.NotificationCount, room.UnreadNotifications)
	}
}

// TestThreadsAreCountedInUntilYouAskOtherwise is the split MSC3771 asks for, and the part servers
// usually get backwards.
//
// Without the filter flag the room's badge is the whole room — threads included — because a client
// that does not know about threads must still show a correct total. With the flag, the room badge
// becomes the main timeline alone and the threads arrive apart.
func TestThreadsAreCountedInUntilYouAskOtherwise(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	since := syncWith(t, server, bob, "", "").NextBatch

	root := sendContent(t, server, alice, roomID, message("root of the thread"))
	sendContent(t, server, alice, roomID, map[string]any{
		"msgtype":      "m.text",
		"body":         "first reply",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root},
	})
	sendContent(t, server, alice, roomID, map[string]any{
		"msgtype":      "m.text",
		"body":         "reply mentioning " + bob.UserID,
		"m.mentions":   map[string]any{"user_ids": []string{bob.UserID}},
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root},
	})
	sendContent(t, server, alice, roomID, map[string]any{
		"msgtype":    "m.text",
		"body":       "unthreaded mention " + bob.UserID,
		"m.mentions": map[string]any{"user_ids": []string{bob.UserID}},
	})

	// Unfiltered: everything in one number, and no thread section to misread.
	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 4 || room.UnreadNotifications.HighlightCount != 2 {
		t.Errorf("unfiltered badge: got %+v, want 4 notifications and 2 highlights (the whole room)",
			room.UnreadNotifications)
	}
	if room.UnreadThreadNotifications != nil {
		t.Errorf("a request that never asked for threads got %v", room.UnreadThreadNotifications)
	}

	// Filtered: the main timeline apart, and each thread apart.
	filtered := syncWith(t, server, bob, since, `{"room":{"timeline":{"unread_thread_notifications":true}}}`)
	room = filtered.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications on the filtered sync")
	}
	if room.UnreadNotifications.NotificationCount != 2 || room.UnreadNotifications.HighlightCount != 1 {
		t.Errorf("main-timeline badge: got %+v, want 2 notifications and 1 highlight",
			room.UnreadNotifications)
	}
	thread, ok := room.UnreadThreadNotifications[root]
	if !ok {
		t.Fatalf("the thread started at %s is not in %v", root, room.UnreadThreadNotifications)
	}
	if thread.NotificationCount != 2 || thread.HighlightCount != 1 {
		t.Errorf("thread badge: got %+v, want 2 notifications and 1 highlight", thread)
	}
}

// TestAThreadedReceiptReadsOnlyItsOwnThread is the difference between a receipt that means "this
// thread is finished" and one that silently marks a whole room read.
func TestAThreadedReceiptReadsOnlyItsOwnThread(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)
	bob := server.Register(t, "bob", "pw-bob", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})
	bob.MustJoinRoom(t, server, roomID)
	since := syncWith(t, server, bob, "", "").NextBatch

	root := sendContent(t, server, alice, roomID, message("root"))
	firstInThread := sendContent(t, server, alice, roomID, map[string]any{
		"msgtype":      "m.text",
		"body":         "reply",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root},
	})
	unthreaded := sendContent(t, server, alice, roomID, message("still waiting"))

	// Reading the thread must leave the unthreaded message unread.
	markRead(t, server, bob, roomID, firstInThread, root)
	resp := syncWith(t, server, bob, since, `{"room":{"timeline":{"unread_thread_notifications":true}}}`)
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 2 {
		t.Errorf("main-timeline badge: got %+v, want 2 — a thread receipt must not mark the room read",
			room.UnreadNotifications)
	}
	if thread := room.UnreadThreadNotifications[root]; thread.NotificationCount != 0 {
		t.Errorf("thread badge: got %+v, want 0 — that thread was just read", thread)
	}

	// And the unthreaded receipt reads everything it precedes, in every scope.
	markRead(t, server, bob, roomID, unthreaded, "")
	resp = syncWith(t, server, bob, since, `{"room":{"timeline":{"unread_thread_notifications":true}}}`)
	room = resp.Rooms.Join[roomID]
	if room.UnreadNotifications.NotificationCount != 0 {
		t.Errorf("main-timeline badge: got %+v, want 0", room.UnreadNotifications)
	}
	for id, thread := range room.UnreadThreadNotifications {
		if thread.NotificationCount != 0 {
			t.Errorf("thread %s: got %+v, want 0 — the unthreaded receipt covers it too", id, thread)
		}
	}
}

// TestAnEditIsNotANewThingToRead checks that fixing a typo does not raise a second badge.
func TestAnEditIsNotANewThingToRead(t *testing.T) {
	roomID, server, alice, bob, ids, since := roomWithMessages(t, message("typo"))

	alice.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/send/m.room.message/"+harness.Nonce("edit"), map[string]any{
		"msgtype": "m.text",
		"body":    "* typo, corrected",
		"m.relates_to": map[string]any{
			"rel_type": "m.replace", "event_id": ids[0],
		},
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "typo, corrected"},
	}).MustStatus(t, 200)

	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 1 {
		t.Errorf("notification_count: got %d, want 1 — an edit to a message is that same message",
			room.UnreadNotifications.NotificationCount)
	}
}

// TestTheBadgeSurvivesARestartAtAFreshPosition is deliberately about a *later* sync: a badge that is
// only right the first time is a badge that is wrong.
func TestTheBadgeSurvivesARestartAtAFreshPosition(t *testing.T) {
	roomID, server, alice, bob, _, since := roomWithMessages(t, message("one"), message("two"))

	// A later sync, then a new message, then a sync from the original token again.
	syncWith(t, server, bob, since, "")
	sendContent(t, server, alice, roomID, message("three"))

	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil || room.UnreadNotifications.NotificationCount != 3 {
		t.Errorf("notification_count: got %v, want 3 — the badge is a position, not a one-shot",
			room.UnreadNotifications)
	}
}

// TestPushRuleChangesChangeTheBadge is the point of push rules being data: turning notifications off
// for a room or a sender has to be visible in the badge, or the rules are decoration.
func TestPushRuleChangesChangeTheBadge(t *testing.T) {
	roomID, server, _, bob, _, since := roomWithMessages(t, message("one"), message("two"))

	// Silence this room, then ask again.
	// A room rule saying "nothing in this room is worth telling me", which is the one kind of
	// rule a person writes on purpose.
	bob.MustPutJSON(t, server, "/_matrix/client/v3/pushrules/global/room/"+
		harness.PathEscape(roomID)+"/actions", map[string]any{"actions": []any{"dont_notify"}}).
		MustStatus(t, 200)

	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 0 {
		t.Errorf("notification_count: got %d, want 0 — this room was silenced, and the badge did not "+
			"notice (%+v)", room.UnreadNotifications.NotificationCount, room.UnreadNotifications)
	}
}

// TestARoomWithSomethingToReadIsNotReportedAsFinished is the bug Complement caught and no local test
// had thought of.
//
// A client that polls again from a token past everything asks "anything new?", and the honest answer
// about the *timeline* is no. But a room still holding unread messages is not finished with, and a
// server that answers "nothing" teaches the client that the badge it is showing has no source. The
// unread count is a fact about the room, and it has to keep travelling with it.
func TestARoomWithSomethingToReadIsNotReportedAsFinished(t *testing.T) {
	roomID, server, _, bob, _, _ := roomWithMessages(t, message("one"), message("two"))

	// Catch up completely, so the next request is from a token with nothing new behind it.
	caught := syncWith(t, server, bob, "", "")
	afterEverything := caught.NextBatch

	// Sanity check: the catch-up sync did report the unread messages this room still has.
	if room := caught.Rooms.Join[roomID]; room.UnreadNotifications == nil ||
		room.UnreadNotifications.NotificationCount != 2 {
		t.Fatalf("the initial sync did not report the unread messages: %+v", room.UnreadNotifications)
	}

	later := syncWith(t, server, bob, afterEverything, "")
	room, present := later.Rooms.Join[roomID]
	if !present {
		t.Fatalf("a quiet room with unread messages dropped out of the sync entirely, so the client "+
			"has nothing left to show its badge against (join: %v)", later.Rooms.Join)
	}
	if room.UnreadNotifications == nil {
		t.Fatal("the room came back without unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 2 {
		t.Errorf("notification_count: got %d, want 2 — the messages are still unread",
			room.UnreadNotifications.NotificationCount)
	}
}

// TestARoomNobodyIsWaitingInCanBeLeftOut is the other half: keeping a room present because it is
// read-and-quiet would make every idle room ride along on every sync forever.
func TestARoomNobodyIsWaitingInCanBeLeftOut(t *testing.T) {
	// The event ID is the one the sender was told about, not whatever the reader's timeline
	// happens to lead with: an initial timeline opens with the room's own state events, and a
	// receipt put on one of those says nothing about the message.
	roomID, server, _, bob, ids, _ := roomWithMessages(t, message("one"))

	caught := syncWith(t, server, bob, "", "")
	markRead(t, server, bob, roomID, ids[0], "")

	later := syncWith(t, server, bob, caught.NextBatch, "")
	if room, present := later.Rooms.Join[roomID]; present && room.UnreadNotifications != nil &&
		room.UnreadNotifications.NotificationCount != 0 {
		t.Errorf("the badge survived the receipt: %+v", room.UnreadNotifications)
	}
}

// TestAThreadedReactionIsNotSomethingToRead covers the newer reaction event, `m.room.reaction`, which
// carries its relation the same way a thread reply does.
//
// Both spellings of a reaction mean "I saw that", which is a statement about a message the other
// person has already had; neither is news. Matching only the older type would make the badge wrong
// for exactly the client that switched.
func TestAThreadedReactionIsNotSomethingToRead(t *testing.T) {
	roomID, server, alice, bob, ids, since := roomWithMessages(t, message("a real message"))

	for _, reaction := range []struct {
		path    string
		content map[string]any
	}{
		{"m.reaction", map[string]any{"m.relates_to": map[string]any{
			"rel_type": "m.annotation", "event_id": ids[0], "key": "👍"}}},
		{"m.room.reaction", map[string]any{"m.relates_to": map[string]any{
			"rel_type": "m.annotation", "event_id": ids[0], "key": "🎉"}}},
	} {
		alice.MustPutJSON(t, server, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
			"/send/"+reaction.path+"/"+harness.Nonce("react"), reaction.content).MustStatus(t, 200)
	}

	resp := syncWith(t, server, bob, since, "")
	room := resp.Rooms.Join[roomID]
	if room.UnreadNotifications == nil {
		t.Fatal("no unread_notifications")
	}
	if room.UnreadNotifications.NotificationCount != 1 {
		t.Errorf("notification_count: got %d, want 1 — two reactions were added to one message and "+
			"neither is something to read (%+v)", room.UnreadNotifications.NotificationCount,
			room.UnreadNotifications)
	}
}
