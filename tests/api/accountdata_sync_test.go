package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/tidwall/gjson"
)

// A client that writes its own settings has to be told about the write, by the sync it already
// has open, with the token it already holds.
//
// This is not a nicety. Element Web's "reset my encryption" screen writes four account-data keys
// in a row and waits for each write to come back down `/sync` before making the next request. A
// server that reports the write late leaves the client sitting on a spinner; a server that never
// reports it leaves the client sitting there forever, with the user reading it as a homeserver
// that has hung. The initial sync hides this completely — it answers with everything — so the
// property is only tested by a sync that carries a `since`.
func TestAnAccountDataWriteIsToldToTheClientThatWroteIt(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "settings", "pw-settings", false)

	path := "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/account_data/com.example.theme"
	first := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	token := first.NextBatch

	alice.MustPutJSON(t, server, path, map[string]any{"theme": "dark"}).MustStatus(t, http.StatusOK)

	// The write has to be visible to the token the client already holds: that is the only
	// request a client makes after a write, because it is the one it is already sitting on.
	synced := alice.MustGet(t, server, "/_matrix/client/v3/sync?since="+token+"&timeout=0")
	if !carriesAccountData(synced.Body, "com.example.theme") {
		t.Errorf("the account data written after token %s was not reported by the next sync\n%s",
			token, synced.Body)
	}
	if next := gjson.GetBytes(synced.Body, "next_batch").String(); next == token {
		t.Errorf("a write happened and the sync still answered with the token it was given (%s); "+
			"a client can never catch up, and every later sync repeats this one\n%s", token, synced.Body)
	}
}

// The same write has to arrive while a sync is waiting, not after it times out.
//
// Measured rather than asserted in passing: the request is started, the write happens a moment
// later, and the response has to have arrived well inside the long-poll timeout. A server that
// does not consider account data worth waking a client for still passes an eventual-consistency
// test like the one above — it just takes the whole timeout, which is 30 seconds of a human
// watching a spinner, and the client that chains four writes is waiting two minutes.
func TestAnAccountDataWriteWakesASyncThatIsWaiting(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "waiter", "pw-waiter", false)

	path := "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/account_data/com.example.waitlist"
	token := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch

	waking := make(chan gjson.Result, 1)
	go func() {
		response := alice.Do(t, server, harness.Request{
			Method: http.MethodGet,
			Path:   "/_matrix/client/v3/sync?since=" + token + "&timeout=20000",
		})
		waking <- gjson.ParseBytes(response.Body)
	}()

	started := time.Now()
	alice.MustPutJSON(t, server, path, map[string]any{"written": "while you were waiting"}).
		MustStatus(t, http.StatusOK)

	select {
	case body := <-waking:
		if took := time.Since(started); took > 5*time.Second {
			t.Errorf("the waiting sync took %s to report the write; a client waiting on that "+
				"echo is a client with a spinner on screen", took)
		}
		if !carriesAccountData([]byte(body.Raw), "com.example.waitlist") {
			t.Errorf("the sync that woke did not carry the write: %s", body.Raw)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("a waiting sync was not woken by an account-data write; the long-poll sat there")
	}
}

// One write is one piece of news.
//
// A token the server has already reported a write through must not report it again: a client that
// receives the same account data twice per retry is a client that reprocesses it, and a server
// that answers the same token forever never lets a client move forward at all.
func TestAccountDataIsToldOnceAndNotAgain(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "repeat", "pw-repeat", false)

	path := "/_matrix/client/v3/user/" + harness.PathEscape(alice.UserID) + "/account_data/com.example.once"
	first := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}}).NextBatch
	alice.MustPutJSON(t, server, path, map[string]any{"only": "once"}).MustStatus(t, http.StatusOK)

	synced := alice.MustGet(t, server, "/_matrix/client/v3/sync?since="+first+"&timeout=0").
		MustStatus(t, http.StatusOK)
	if !carriesAccountData(synced.Body, "com.example.once") {
		t.Fatalf("the write was never reported, so this test cannot say anything about reporting it twice\n%s",
			synced.Body)
	}
	second := gjson.GetBytes(synced.Body, "next_batch").String()

	again := alice.MustGet(t, server, "/_matrix/client/v3/sync?since="+second+"&timeout=0").
		MustStatus(t, http.StatusOK)
	if carriesAccountData(again.Body, "com.example.once") {
		t.Errorf("account data written once was reported by both %s and %s; the position the server "+
			"put on the write did not outrank the token it handed out\n%s", first, second, again.Body)
	}
}

// carriesAccountData reports whether a sync body carries an account-data event of a type.
func carriesAccountData(body []byte, dataType string) bool {
	var reply struct {
		AccountData struct {
			Events []struct {
				Type string `json:"type"`
			} `json:"events"`
		} `json:"account_data"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return false
	}
	for _, event := range reply.AccountData.Events {
		if event.Type == dataType {
			return true
		}
	}
	return false
}
