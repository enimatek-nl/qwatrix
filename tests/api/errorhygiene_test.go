package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// CN-7 says a homeserver must not tell a caller how it is implemented, and AC-7 says it must not
// write secrets where they can be read later. Both are properties of the failure paths, so both are
// properties nobody notices until an upgrade turns them into an incident: a message that names a Go
// struct field tells an attacker which parser is in use, and a password in a log file is a password
// in a backup, a support ticket, and a terminal somewhere.

// internalsAreNotForClients are strings that mean a Go fact reached a protocol field. They are
// chosen to be things only an implementation says: no homeserver spec contains the word "unmarshal",
// and no Matrix error is expected to name a source file.
var internalsAreNotForClients = []string{
	"json:", "unmarshal", "struct field", "Go struct", "cannot unmarshal",
	".go:", "goroutine", "runtime/debug", "runtime.Goexit",
	"sql:", "gorm", "sqlite", "no such table", "database is locked",
	"nil pointer dereference", "index out of range", "slice bounds",
	"invalid memory address", "interface conversion", "hash of unhashable",
	"call to a push of", "close of closed",
}

// errorCase is a request that a homeserver is expected to refuse, together with the refusal it
// should give. A case with no wantCode only asserts the shape of the refusal.
type errorCase struct {
	name        string
	method      string
	path        string
	body        any
	rawBody     string
	wantStatus  int
	wantErrcode string // errcode, when it matters
}

// errorCases are chosen for the paths most likely to hand a client something internal: the JSON
// decoder, the validator behind it, and anything that parses a token or an identifier.
func errorCases() []errorCase {
	return []errorCase{
		{name: "create room, malformed JSON", method: http.MethodPost,
			path: "/_matrix/client/v3/createRoom", rawBody: `{"preset":`, wantStatus: http.StatusBadRequest,
			wantErrcode: "M_BAD_JSON"},
		{name: "create room, name is not a string", method: http.MethodPost,
			path: "/_matrix/client/v3/createRoom", rawBody: `{"name": 42}`, wantStatus: http.StatusBadRequest},
		{name: "create room, visibility is an object", method: http.MethodPost,
			path: "/_matrix/client/v3/createRoom", rawBody: `{"visibility": {"a": 1}}`,
			wantStatus: http.StatusBadRequest},
		{name: "login, malformed JSON", method: http.MethodPost,
			path: "/_matrix/client/v3/login", rawBody: `[1,2,3]`, wantStatus: http.StatusBadRequest},
		{name: "login, password is a list", method: http.MethodPost,
			path: "/_matrix/client/v3/login", rawBody: `{"type":"m.login.password","password":["x"]}`,
			wantStatus: http.StatusBadRequest},
		{name: "register, password is a number", method: http.MethodPost,
			path: "/_matrix/client/v3/register", rawBody: `{"password": 1234}`, wantStatus: http.StatusBadRequest},
		{name: "send event, malformed JSON", method: http.MethodPut,
			path: "/_matrix/client/v3/rooms/" + url.PathEscape("!room:test.local") +
				"/send/m.room.message/txn1", rawBody: `{"msgtype":`, wantStatus: http.StatusBadRequest,
			wantErrcode: "M_BAD_JSON"},
		{name: "put state, content is a string", method: http.MethodPut,
			path: "/_matrix/client/v3/rooms/" + url.PathEscape("!room:test.local") +
				"/state/io.qwatrix.test/", rawBody: `"just a string"`, wantStatus: http.StatusBadRequest},
		{name: "sync, filter is not a filter", method: http.MethodGet,
			path: "/_matrix/client/v3/sync?filter=%7B%C2%A0", wantStatus: http.StatusBadRequest},
		// Reading the history of a room this session is not in is refused before anything about the
		// request's pagination parameters is looked at, which is the right order: the answer must not
		// depend on whether the caller could have read it.
		{name: "messages, room is not mine", method: http.MethodGet,
			path: "/_matrix/client/v3/rooms/" + url.PathEscape("!room:test.local") +
				"/messages?from=not-a-stream-token&dir=b", wantStatus: http.StatusForbidden},
		// A string that is not a room ID names a room this server has never heard of, and asking
		// strangers about it cannot succeed either, so "no such room" is the truthful answer.
		{name: "join, not a room ID", method: http.MethodPost,
			path: "/_matrix/client/v3/join/not-a-room-id", wantStatus: http.StatusNotFound},
		{name: "filter, ID is not a number", method: http.MethodGet,
			path:       "/_matrix/client/v3/user/@someone:test.local/filter/not-a-number",
			wantStatus: http.StatusBadRequest},
		{name: "filter, definition is a string", method: http.MethodPost,
			path: "/_matrix/client/v3/user/@someone:test.local/filter", rawBody: `"nope"`,
			wantStatus: http.StatusBadRequest},
		{name: "push rules, malformed JSON", method: http.MethodPut,
			path: "/_matrix/client/v3/pushrules/global/override/a.rule", rawBody: `{"actions":`,
			wantStatus: http.StatusBadRequest, wantErrcode: "M_BAD_JSON"},
		{name: "keys upload, malformed JSON", method: http.MethodPost,
			path: "/_matrix/client/v3/keys/upload", rawBody: `{"device_keys":`,
			wantStatus: http.StatusBadRequest},
		{name: "receipt, unknown event", method: http.MethodPost,
			path: "/_matrix/client/v3/rooms/" + url.PathEscape("!room:test.local") +
				"/receipt/m.read/$nope", wantStatus: http.StatusNotFound},
		{name: "unknown endpoint", method: http.MethodGet,
			path: "/_matrix/client/v3/does/not/exist", wantStatus: http.StatusNotFound},
		{name: "media, unknown MXC URI", method: http.MethodGet,
			path: "/_matrix/media/v3/download/test.local/does-not-exist", wantStatus: http.StatusNotFound},
	}
}

// TestRefusalsAreMatrixErrors checks that every one of these failures is answered the way the
// specification says a failure is answered: a JSON object with an errcode and a message, at a status
// that matches the kind of mistake.
//
// It matters because the alternative is what a proxy, a client, or a human on call reads: an empty
// 500, a plain-text panic, or an HTML page.
func TestRefusalsAreMatrixErrors(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice-not-a-secret", false)

	for _, tc := range errorCases() {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRawWithError(t, server, alice.AccessToken, tc)
			if resp.StatusCode < 400 {
				t.Fatalf("%s %s: got %d, want a refusal\nbody: %s", tc.method, tc.path,
					resp.StatusCode, resp.Body)
			}
			var body struct {
				ErrCode string `json:"errcode"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(resp.Body, &body); err != nil {
				t.Fatalf("refusal is not a JSON matrix error: %v\nbody: %s", err, resp.Body)
			}
			if body.ErrCode == "" || body.Error == "" {
				t.Fatalf("refusal has an empty errcode or message: %s", resp.Body)
			}
			if tc.wantStatus != 0 && resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, resp.Body)
			}
			if tc.wantErrcode != "" && body.ErrCode != tc.wantErrcode {
				t.Fatalf("errcode = %q, want %q (status %d): %s", body.ErrCode, tc.wantErrcode,
					resp.StatusCode, resp.Body)
			}
		})
	}
}

// TestRefusalsDoNotQuoteGoInternals is the CN-7 assertion: the text a homeserver sends when
// something went wrong may describe the caller's mistake and must not describe the implementation.
func TestRefusalsDoNotQuoteGoInternals(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice-not-a-secret", false)

	for _, tc := range errorCases() {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRawWithError(t, server, alice.AccessToken, tc)
			body := string(resp.Body)
			for _, marker := range internalsAreNotForClients {
				if strings.Contains(body, marker) {
					t.Fatalf("%s %s (%d) revealed %q to the client:\n%s",
						tc.method, tc.path, resp.StatusCode, marker, body)
				}
			}
		})
	}
}

// TestARejectedRequestBodyDoesNotDescribeTheDecoder covers the specific way this leaks: Go's JSON
// errors name struct fields and types, and several handlers hand the decoder's reason straight into
// the message they answer with.
func TestARejectedRequestBodyDoesNotDescribeTheDecoder(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice-not-a-secret", false)

	// A field of the wrong type is the case whose Go error is the most descriptive: it names the
	// struct, the field, and the type.
	cases := []string{
		`{"name": {"not":"a string"}}`,
		`{"topic": [1,2,3]}`,
		`{"room_version": 7}`,
		`{"is_direct": "yes"}`,
		`{"power_level_content_override": "no"}`,
		`{"initial_state": 5}`,
	}
	for _, raw := range cases {
		resp := server.PutJSON(t, "/_matrix/client/v3/createRoom", alice.AccessToken, json.RawMessage(raw))
		if resp.StatusCode < 400 {
			// Not every field has to be rejected — but none of them may be described.
			continue
		}
		for _, marker := range internalsAreNotForClients {
			if strings.Contains(string(resp.Body), marker) {
				t.Fatalf("a createRoom body of %s revealed %q to the client (%d):\n%s",
					raw, marker, resp.StatusCode, resp.Body)
			}
		}
	}
}

// TestSecretsStayOutOfTheLog performs the operations that put the most sensitive values on the wire
// and then reads the server's own log. A log is copied into backups, pasted into issues, and left on
// a terminal; anything in it has to be safe to read.
func TestSecretsStayOutOfTheLog(t *testing.T) {
	const password = "a-password-nobody-should-see-in-a-log"
	// Debug, deliberately. The audit has to be run from the vantage point of an operator who has
	// turned everything on, because that is the configuration in which a secret escapes: a server
	// started at Error would hide a leak made at Info, and the leak, not the level, is the bug.
	// (This was not a hypothetical — the first version of this test passed while a password was
	// being logged at Info, and only failed once the level was raised.)
	server := harness.NewWithRegistration(t, harness.WithLogLevel(slog.LevelDebug))

	alice := server.Register(t, "secretkeeper", password, false)
	token := alice.AccessToken
	if token == "" {
		t.Fatalf("no access token to look for")
	}

	// Room traffic, so a request that touches a lot of state and would be a natural thing to log.
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	alice.MustSendText(t, server, roomID, "hello")
	alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})

	// A login, which is the operation whose request body is a password.
	reloged := server.Login(t, "secretkeeper", password)

	// Shared-secret registration, whose whole mechanism is a MAC derived from the password.
	nonce := server.SharedSecretNonce(t)
	mac := harness.SharedSecretMAC(nonce, "another", password, false)
	server.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
		"nonce": nonce, "username": "another", "password": password, "mac": mac,
	}).MustStatus(t, 200)

	// Media, because a media URI is a capability: anyone holding it can fetch the file.
	uri := uploadFile(t, alice, server, "secret.txt", "text/plain", []byte("capability"))
	_, mediaID := splitMXC(t, uri)

	logs := server.Logs()
	for _, secret := range []struct{ name, value string }{
		{"the password", password},
		{"an access token", token},
		{"a refreshed access token", reloged.AccessToken},
		{"a registration nonce", nonce},
		{"a registration MAC", mac},
		{"a media identifier", mediaID},
	} {
		if secret.value == "" {
			t.Fatalf("the fixture produced no %s, so the test would have passed anyway", secret.name)
		}
		if strings.Contains(logs, secret.value) {
			t.Errorf("the log contains %s (%d bytes of it): %s", secret.name, len(secret.value),
				lineContaining(logs, secret.value))
		}
	}
}

// TestASyncTokenIsNotLogged is separated from the test above because sync tokens are the secret a
// server is most tempted to log: they arrive in a query string, where access logs like to put them.
func TestASyncTokenIsNotLogged(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithLogLevel(slog.LevelDebug))
	alice := server.Register(t, "tokkeeper", "pw-tokkeeper", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	alice.MustSendText(t, server, roomID, "synced")

	first := alice.MustSync(t, server, url.Values{"timeout": []string{"0"}})
	if first.NextBatch == "" {
		t.Fatalf("sync returned no position to look for")
	}
	// Use it, so the server has to parse it and would have to mention it if it were going to.
	alice.MustSync(t, server, url.Values{"timeout": []string{"0"}, "since": []string{first.NextBatch}})
	server.Get(t, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
		"/messages?from="+url.QueryEscape(first.NextBatch)+"&dir=b&limit=5", alice.AccessToken).
		MustStatus(t, 200)

	if logs := server.Logs(); strings.Contains(logs, first.NextBatch) {
		t.Errorf("the log contains a sync token: %s", lineContaining(logs, first.NextBatch))
	}
}

// doRawWithError sends an errorCase, authenticated when the case needs no particular session.
func doRawWithError(t *testing.T, server *harness.Server, token string, tc errorCase) *harness.Response {
	t.Helper()
	req := harness.Request{Method: tc.method, Path: tc.path, Token: token}
	switch {
	case tc.rawBody != "":
		req.RawBody = []byte(tc.rawBody)
		req.ContentType = "application/json"
	case tc.body != nil:
		req.Body = tc.body
	}
	return server.Do(t, req)
}

// lineContaining returns the log lines that mention needle, so a failure names the line to read
// rather than dumping a whole homeserver's output.
func lineContaining(logs, needle string) string {
	keep := []string{}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, needle) {
			keep = append(keep, line)
		}
	}
	if len(keep) > 6 {
		keep = keep[:6]
	}
	return strings.Join(keep, "\n")
}

// TestAStreamTokenIsRejectedWithoutDescribingTheParser covers the pagination case the table above
// cannot: the caller has to be allowed to read the room before a bad `from` tells them anything, and
// what it must never tell them is which token format is in use.
func TestAStreamTokenIsRejectedWithoutDescribingTheParser(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice-not-a-secret", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	alice.MustSendText(t, server, roomID, "hello")

	// Tokens that cannot name a position at all. A negative is deliberately absent: pagination
	// positions are an ordering, and "before everything" is a coherent reading of it that answers
	// with the oldest events — which is what a client asking for the start of a room wants anyway.
	for _, token := range []string{"not-a-token", "9999999999999999999999", "1%202", "snotanumber"} {
		resp := server.Get(t, "/_matrix/client/v3/rooms/"+harness.PathEscape(roomID)+
			"/messages?from="+url.QueryEscape(token)+"&dir=b&limit=5", alice.AccessToken)
		if resp.StatusCode < 400 {
			t.Fatalf("a pagination token of %q was accepted: %d %s", token, resp.StatusCode, resp.Body)
		}
		for _, marker := range internalsAreNotForClients {
			if strings.Contains(string(resp.Body), marker) {
				t.Fatalf("a pagination token of %q revealed %q to the client (%d):\n%s",
					token, marker, resp.StatusCode, resp.Body)
			}
		}
	}
}

// TestAFederationRefusalDoesNotLogTheSignature covers the one place this server writes an
// Authorization header down: a refused peer request, which is exactly the line an operator pastes
// into a channel. The claimed origin and key ID are what make the refusal diagnosable; the signature
// is what authorises the request, and alongside the method, URI, body hash, timestamp and nonce it is
// a complete credential for that request.
func TestAFederationRefusalDoesNotLogTheSignature(t *testing.T) {
	const signature = "aWNhbm5vdGJlY29waWVkZnJvbWFsb2dncmFwaA"
	// A name that resolves, so building the federation listener does not spend five seconds on a
	// DNS lookup that is going to fail.
	server := harness.NewWithRegistration(t, harness.WithLogLevel(slog.LevelDebug),
		harness.WithServerName("127.0.0.1:"+strconv.Itoa(reserveFreePort(t))))
	server.Register(t, "alice", "pw-alice-not-a-secret", false)

	listener := server.FederationServer(t)
	// PUT /send/{txnID} is a signed route; anything else would be refused by the router before any
	// signature was looked at, and the test would be measuring a different refusal.
	request, err := http.NewRequest(http.MethodPut,
		listener.URL+"/_matrix/federation/v1/send/txn-hygiene-probe",
		strings.NewReader(`{"origin":"evil.example","edus":[],"pdus":[]}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization",
		`X-Matrix origin="evil.example",key="evil.example:key1",sig="`+signature+`",mac="0000"`)
	response, err := server.FederationClient(t).Do(request)
	if err != nil {
		t.Fatalf("the peer request never reached the server: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 400 {
		t.Fatalf("an invented X-Matrix signature was accepted with %d", response.StatusCode)
	}

	// The server is running at debug, so this is the claim it has to survive: the signature is absent
	// from every level, not merely from the default one.
	logs := server.Logs()
	if strings.Contains(logs, signature) {
		t.Errorf("the refusal logged the signature itself: %s", lineContaining(logs, signature))
	}
	if !strings.Contains(logs, "<redacted>") {
		t.Errorf("the refusal did not log a redacted signature, so it cannot be diagnosed:\n%s",
			lineContaining(logs, "could not be authenticated"))
	}
	// And the parts that make it diagnosable are still there.
	for _, want := range []string{"evil.example", "send/txn-hygiene-probe"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the refusal lost %q, which is the reason the header is logged at all", want)
		}
	}
}

// TestAQueryStringEncodingTokenIsNotLogged covers the one way an access token reaches a log through
// middleware rather than a handler: the specification still accepts `?access_token=`, and the request
// log is one field away from writing it down — `r.URL.RequestURI()` instead of `r.URL.Path` is a
// one-character change that turns every debug log into a list of live sessions.
func TestAQueryStringEncodingTokenIsNotLogged(t *testing.T) {
	server := harness.NewWithRegistration(t, harness.WithLogLevel(slog.LevelDebug))
	alice := server.Register(t, "tokquery", "pw-tokquery", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "private_chat"})
	alice.MustSendText(t, server, roomID, "sent with a query token")

	// The request itself has to succeed: refusing query-string tokens would be a different feature
	// change, and this test is about what the log says, not about what is accepted.
	raw := server.Do(t, harness.Request{
		Method: http.MethodGet,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/state?access_token=" +
			url.QueryEscape(alice.AccessToken),
	})
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("a query-string access token was refused (%d): %s", raw.StatusCode, raw.Body)
	}

	if logs := server.Logs(); strings.Contains(logs, alice.AccessToken) {
		t.Errorf("the log contains the access token the caller put in a query string: %s",
			lineContaining(logs, alice.AccessToken))
	}
	// The request still has to be diagnosable, which is why the path is logged without it.
	if !strings.Contains(server.Logs(), "/_matrix/client/v3/rooms/") {
		t.Errorf("the request log does not name the path, so there is nothing left to diagnose with")
	}
}
