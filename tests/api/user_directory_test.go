package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// searchDirectory runs a user-directory search and returns the results as user ID -> display name.
func searchDirectory(t *testing.T, account *harness.Account, server *harness.Server, term string) map[string]string {
	t.Helper()
	resp := account.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/user_directory/search",
		Body:   map[string]any{"search_term": term},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /user_directory/search: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}
	var page struct {
		Results []struct {
			UserID      string `json:"user_id"`
			DisplayName string `json:"display_name"`
		} `json:"results"`
		Limited bool `json:"limited"`
	}
	resp.JSON(t, &page)
	out := map[string]string{}
	for _, result := range page.Results {
		out[result.UserID] = result.DisplayName
	}
	return out
}

// TestUserDirectoryKeepsRoomNamesPrivate checks that the user directory reveals a name only to
// people who were told it.
//
// A display name is a per-room fact: a person may be "Alice Cooper" to the world and "Freddy" to
// one colleague. A directory that indexed the second name alongside the first would hand out a
// private disclosure to anybody who happened to type it, which is the opposite of what the person
// in the private room chose. So each row records the room it came from, and a search is filtered
// by what the searcher could already have seen.
func TestUserDirectoryKeepsRoomNamesPrivate(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "directoryalice", "pw-directoryalice", false)
	bob := server.Register(t, "directorybob", "pw-directorybob", false)
	eve := server.Register(t, "directoryeve", "pw-directoryeve", false)

	alice.MustPutJSON(t, server, "/_matrix/client/v3/profile/"+
		harness.PathEscape(alice.UserID)+"/displayname",
		map[string]any{"displayname": "Alice Cooper"}).MustStatus(t, http.StatusOK)
	// A public room is what makes a profile name searchable at all: the public part of a
	// server's user list is the part the user put somewhere public.
	alice.MustCreateRoom(t, server, map[string]any{"visibility": "public"})

	// A private room Alice is invited into, where she goes by something else.
	privateRoom := bob.MustCreateRoom(t, server, map[string]any{
		"visibility": "private", "invite": []string{alice.UserID},
	})
	alice.MustJoinRoom(t, server, privateRoom)
	alice.MustSetState(t, server, privateRoom, "m.room.member", alice.UserID, map[string]any{
		"membership": "join", "displayname": "Freddy",
	})

	// Eve shares nothing with Alice but the public room, so the public name is all she can find.
	if got := searchDirectory(t, eve, server, "Alice Cooper")[alice.UserID]; got != "Alice Cooper" {
		t.Errorf("Eve searching the profile name found %q, want Alice Cooper", got)
	}
	if results := searchDirectory(t, eve, server, "Freddy"); len(results) != 0 {
		t.Errorf("Eve found a name said in a room she is not in: %v", results)
	}

	// Bob was told the private name, and may also find Alice by the profile name she published
	// publicly — and is shown the public one, because that is the name she published.
	if got := searchDirectory(t, bob, server, "Alice Cooper")[alice.UserID]; got != "Alice Cooper" {
		t.Errorf("Bob searching the profile name found %q, want Alice Cooper", got)
	}
	if got := searchDirectory(t, bob, server, "Freddy")[alice.UserID]; got == "" {
		t.Error("Bob could not find the name Alice chose in his room")
	}

	// Nobody searches themself out of a directory: a directory that returns the person using it
	// is noise.
	if results := searchDirectory(t, eve, server, "eve"); len(results[eve.UserID]) != 0 {
		t.Errorf("Eve found themself in the directory: %v", results)
	}

	// A search of the user ID finds the account, which is the one thing a directory must do even
	// for a user who never set a display name.
	found := searchDirectory(t, alice, server, bob.UserID)
	if _, ok := found[bob.UserID]; !ok {
		t.Errorf("searching Bob's own user ID did not find him: %v", found)
	}
}

// TestUserDirectoryIgnoresDepartedNames checks that leaving a room takes the room's name for you
// out of circulation.
func TestUserDirectoryIgnoresDepartedNames(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "leaver", "pw-leaver", false)
	bob := server.Register(t, "roommate", "pw-roommate", false)

	roomID := bob.MustCreateRoom(t, server, map[string]any{
		"visibility": "private", "invite": []string{alice.UserID},
	})
	alice.MustJoinRoom(t, server, roomID)
	alice.MustSetState(t, server, roomID, "m.room.member", alice.UserID, map[string]any{
		"membership": "join", "displayname": "Nickname",
	})
	if got := searchDirectory(t, bob, server, "Nickname")[alice.UserID]; got != "Nickname" {
		t.Fatalf("before the leave, Bob found %q, want Nickname", got)
	}

	alice.MustLeaveRoom(t, server, roomID)
	if got := searchDirectory(t, bob, server, "Nickname"); len(got) != 0 {
		t.Errorf("the room kept a name for Alice after she left: %v", got)
	}
}

// TestToDeviceMessagesReachTheirDevices checks the channel end-to-end encryption depends on.
//
// A key share is addressed to one device of one person. It is not a room event, so it appears in no
// timeline, and the only place a client learns of it is its own inbox. If the server drops it, or
// hands it to a sibling device, or hands it out twice, the symptom is a message that never decrypts
// and no error anywhere to explain it.
func TestToDeviceMessagesReachTheirDevices(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "sender", "pw-sender", false)
	bob := server.Register(t, "recipient", "pw-recipient", false)
	// A second login is a second device of the same person, which is the case the `*` spelling in
	// a send is for.
	bobOther := server.Login(t, "recipient", "pw-recipient")
	if bobOther.DeviceID == bob.DeviceID {
		t.Fatalf("two logins share device %q, so this test cannot tell devices apart", bob.DeviceID)
	}

	content := map[string]any{"my_key": "my_value"}
	send := func(account *harness.Account, body any) *harness.Response {
		return account.Do(t, server, harness.Request{
			Method: "PUT",
			Path:   "/_matrix/client/v3/sendToDevice/my.test.type/txn-1",
			Body:   body,
		})
	}
	resp := send(alice, map[string]any{"messages": map[string]any{
		bob.UserID: map[string]any{"*": content},
	}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /sendToDevice: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}

	// Every device of the recipient gets it, including the one the sender did not name: a sender
	// that has not kept a device list says `*`, and the server is the only party that knows which
	// devices exist.
	for _, account := range []*harness.Account{bob, bobOther} {
		since := ""
		var batch []json.RawMessage
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && len(batch) == 0; {
			sync := account.MustSync(t, server, url.Values{"timeout": []string{"0"}})
			since = sync.NextBatch
			batch = sync.ToDevice.Events
		}
		if len(batch) == 0 {
			t.Fatalf("device %s received no to-device message", account.DeviceID)
		}
		var event struct {
			Type    string         `json:"type"`
			Sender  string         `json:"sender"`
			Content map[string]any `json:"content"`
		}
		if err := json.Unmarshal(batch[0], &event); err != nil {
			t.Fatalf("to-device event is unreadable: %v", err)
		}
		if event.Type != "my.test.type" || event.Sender != alice.UserID {
			t.Errorf("to-device event is %q from %q, want my.test.type from %s",
				event.Type, event.Sender, alice.UserID)
		}
		if event.Content["my_key"] != "my_value" {
			t.Errorf("to-device content arrived as %v, want %v", event.Content, content)
		}

		// And not twice: the cursor advances with delivery. A second sync must be empty, because
		// a client handed the same key share again replays a message key it already used.
		followUp := account.MustSync(t, server, url.Values{"since": []string{since}})
		if len(followUp.ToDevice.Events) != 0 {
			t.Errorf("device %s was handed the same to-device message twice", account.DeviceID)
		}
	}

	// A recipient on another server is skipped rather than failing the send: a room with visitors
	// is normal, and refusing to share keys with anybody remote would break encryption for it.
	resp = send(alice, map[string]any{"messages": map[string]any{
		"@someone:elsewhere.example": map[string]any{"*": content},
	}})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("sending to a remote user: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}
}
