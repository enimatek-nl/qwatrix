// Package harness stands a real homeserver up inside the test process and drives it over
// HTTP with a small typed client.
//
// There is exactly one way to start a server in this repository, and it is here: a
// temporary directory, a real SQLite file, the real handlers, the real middleware chain.
// Integration-style on purpose — the behaviours under test are the ones a client
// experiences, and a mock of the thing being tested proves nothing.
package harness

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	tlsconfig "crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/app"
	"github.com/enimatek-nl/qwatrix/internal/config"
)

// ServerName is the identity every harness server answers for. It is a name with a port
// because that is what real deployments use and because it proves nothing in the code
// assumes a bare DNS name.
const ServerName = "test.local:8448"

// Options tunes a harness server. Use [WithRegistrationSecret] and friends rather than
// mutating the struct.
type Options struct {
	// RegistrationSecret enables shared-secret registration; empty leaves the endpoint
	// absent, which is how a real deployment without the secret behaves.
	RegistrationSecret string
	// ServerName overrides [ServerName] when a test needs a specific identity.
	ServerName string
	// EnableRegistration toggles open POST /register (on by default).
	EnableRegistration bool
	// RateLimit enables rate limiting. It is off in tests: a compliance suite sends
	// bursts that would otherwise turn into 429s unrelated to the behaviour under test.
	RateLimit bool
	// DefaultRoomVersion overrides the room version created rooms use.
	DefaultRoomVersion string
	// LogLevel silences (or raises) the server's own logging.
	LogLevel slog.Level
	// FederationInsecureTLS lets this server's outbound federation client accept any peer
	// certificate, which is how a test peer (an httptest TLS server with its own certificate)
	// can stand in for a real homeserver. It mirrors the operator flag of the same name and is
	// off otherwise, so a test that forgets to ask for it does not silently weaken the rest.
	FederationInsecureTLS bool
}

// Option mutates [Options].
type Option func(*Options)

// WithRegistrationSecret enables shared-secret registration with the given secret.
func WithRegistrationSecret(secret string) Option {
	return func(o *Options) { o.RegistrationSecret = secret }
}

// WithServerName sets the homeserver identity.
func WithServerName(name string) Option {
	return func(o *Options) { o.ServerName = name }
}

// WithFederationInsecureTLS lets this server's federation client talk to a test peer whose
// certificate no CA could have issued. Test-only, by the same argument as the operator flag.
func WithFederationInsecureTLS() Option {
	return func(o *Options) { o.FederationInsecureTLS = true }
}

// WithClosedRegistration disables open registration.
func WithClosedRegistration() Option {
	return func(o *Options) { o.EnableRegistration = false }
}

// WithRateLimit turns rate limiting on for a test that is about rate limiting.
func WithRateLimit() Option {
	return func(o *Options) { o.RateLimit = true }
}

// WithRoomVersion sets the default room version.
func WithRoomVersion(version string) Option {
	return func(o *Options) { o.DefaultRoomVersion = version }
}

// WithLogLevel raises the server's log level, which is the difference between a debuggable
// failing test and a mysterious one.
func WithLogLevel(level slog.Level) Option {
	return func(o *Options) { o.LogLevel = level }
}

// Server is a running homeserver plus the HTTP client to talk to it with.
type Server struct {
	// URL is the base URL of the server, e.g. "http://127.0.0.1:53219".
	URL string
	// ServerName is the identity the server answers for.
	ServerName string
	// Dir is the data directory, removed when the test ends.
	Dir string

	httpSrv          *httptest.Server
	federationSrv    *httptest.Server
	federationClient *http.Client
	instance         *app.App
	options          Options
	// client is shared so many tests do not each open their own connection pool.
	client *http.Client
	// logCapture holds the server's log output for assertions.
	logCapture *syncBuffer
}

// New starts a homeserver for the duration of the test. The database is a real file in a
// temporary directory: in-memory databases hide the durability behaviour that matters to a
// homeserver.
func New(t *testing.T, opts ...Option) *Server {
	t.Helper()

	options := Options{
		EnableRegistration: true,
		ServerName:         ServerName,
		DefaultRoomVersion: "11",
		LogLevel:           slog.LevelError,
	}
	for _, opt := range opts {
		opt(&options)
	}

	dir := t.TempDir()
	cfg := config.Default()
	cfg.ServerName = options.ServerName
	cfg.ClientAddr = "127.0.0.1:0"
	cfg.FederationAddr = ""
	cfg.DataDir = dir
	cfg.DatabasePath = dir + "/qwatrix.db"
	cfg.MediaSizeLimitBytes = 5 << 20
	cfg.RegistrationSecret = options.RegistrationSecret
	cfg.EnableRegistration = options.EnableRegistration
	cfg.RateLimitEnabled = options.RateLimit
	cfg.DefaultRoomVersion = options.DefaultRoomVersion
	cfg.FederationInsecureSkipTLSVerify = options.FederationInsecureTLS
	cfg.Version = "test"

	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: options.LogLevel})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	instance, err := app.New(cfg)
	if err != nil {
		t.Fatalf("harness: building the homeserver: %v", err)
	}
	srv := httptest.NewServer(instance.Handler())

	server := &Server{
		URL:        srv.URL,
		ServerName: options.ServerName,
		Dir:        dir,
		httpSrv:    srv,
		instance:   instance,
		options:    options,
		client:     &http.Client{Timeout: 30 * time.Second},
		logCapture: logs,
	}

	t.Cleanup(func() {
		srv.Close()
		if err := instance.Close(); err != nil {
			t.Errorf("harness: closing the homeserver: %v", err)
		}
	})
	return server
}

// Logs returns everything the server has logged so far, for tests that assert that
// secrets and internals stay out of the log.
func (s *Server) Logs() string { return s.logCapture.String() }

// Client returns the HTTP client the server's helpers use, for tests that need to do
// something the helpers do not model.
func (s *Server) Client() *http.Client { return s.client }

// Request is a prepared HTTP request against this server.
type Request struct {
	Method string
	Path   string
	Token  string
	Body   any
	// RawBody is used instead of Body when a test needs bytes that are not JSON.
	RawBody     []byte
	ContentType string
	Headers     map[string]string
}

// Response wraps an http.Response with the body already readable once.
type Response struct {
	*http.Response
	// Body is the raw response body, fully read.
	Body []byte
}

// JSON decodes the response body into dst, failing the test on malformed JSON.
func (r *Response) JSON(t *testing.T, dst any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, dst); err != nil {
		t.Fatalf("response body is not valid JSON: %v\n%s", err, r.Body)
	}
}

// MustStatus fails the test unless the status matches, reporting the body because a status
// alone never says why.
func (r *Response) MustStatus(t *testing.T, want int) *Response {
	t.Helper()
	if r.StatusCode != want {
		t.Fatalf("status = %d, want %d\nbody: %s", r.StatusCode, want, r.Body)
	}
	return r
}

// MustError fails the test unless the response is a Matrix error with the given code.
func (r *Response) MustError(t *testing.T, wantStatus int, wantCode string) {
	t.Helper()
	if r.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d\nbody: %s", r.StatusCode, wantStatus, r.Body)
	}
	var body struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("error response is not JSON: %v\nbody: %s", err, r.Body)
	}
	if body.ErrCode != wantCode {
		t.Fatalf("errcode = %q, want %q\nbody: %s", body.ErrCode, wantCode, r.Body)
	}
	if body.Error == "" {
		t.Fatalf("error message is empty for %s\nbody: %s", wantCode, r.Body)
	}
}

// Do performs a request. It never fails the test on transport errors without reporting the
// request it tried to make, because "context deadline exceeded" alone is not actionable.
func (s *Server) Do(t *testing.T, req Request) *Response {
	t.Helper()

	var body io.Reader
	contentType := req.ContentType
	switch {
	case len(req.RawBody) > 0:
		body = bytes.NewReader(req.RawBody)
		if contentType == "" {
			contentType = "application/json"
		}
	case req.Body != nil:
		encoded, err := json.Marshal(req.Body)
		if err != nil {
			t.Fatalf("harness: encoding request body: %v", err)
		}
		body = bytes.NewReader(encoded)
		if contentType == "" {
			contentType = "application/json"
		}
	}

	target := s.URL + req.Path
	httpReq, err := http.NewRequestWithContext(t.Context(), req.Method, target, body)
	if err != nil {
		t.Fatalf("harness: building %s %s: %v", req.Method, target, err)
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	if req.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.Token)
	}
	for name, value := range req.Headers {
		httpReq.Header.Set(name, value)
	}

	resp, err := s.client.Do(httpReq)
	if err != nil {
		t.Fatalf("harness: %s %s: %v", req.Method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("harness: reading response of %s %s: %v", req.Method, target, err)
	}
	t.Logf("→ %s %s ⇒ %d (%d bytes)", req.Method, req.Path, resp.StatusCode, len(raw))
	return &Response{Response: resp, Body: raw}
}

// Get is shorthand for a GET with no body.
func (s *Server) Get(t *testing.T, path, token string) *Response {
	t.Helper()
	return s.Do(t, Request{Method: http.MethodGet, Path: path, Token: token})
}

// PostJSON is shorthand for a JSON POST.
func (s *Server) PostJSON(t *testing.T, path, token string, body any) *Response {
	t.Helper()
	return s.Do(t, Request{Method: http.MethodPost, Path: path, Token: token, Body: body})
}

// PutJSON is shorthand for a JSON PUT.
func (s *Server) PutJSON(t *testing.T, path, token string, body any) *Response {
	t.Helper()
	return s.Do(t, Request{Method: http.MethodPut, Path: path, Token: token, Body: body})
}

// Account is an authenticated client: the credential triple a Matrix client holds.
type Account struct {
	Localpart   string
	UserID      string
	DeviceID    string
	AccessToken string
	Admin       bool

	// txnCounter numbers the transaction IDs this account sends, so that two sends from the
	// same test are two events rather than one event and a replay.
	txnCounter atomic.Uint64
}

// RegistrationSecret is the secret the harness uses when it enables shared-secret
// registration, mirroring the value Complement hard-codes.
const RegistrationSecret = "complement"

// Register creates an account through shared-secret registration and returns a client for
// it. This is how nearly every test obtains its actors.
func (s *Server) Register(t *testing.T, localpart, password string, admin bool) *Account {
	t.Helper()

	nonce := s.SharedSecretNonce(t)
	payload := map[string]any{
		"nonce":    nonce,
		"username": localpart,
		"password": password,
		"mac":      SharedSecretMAC(nonce, localpart, password, admin),
		"admin":    admin,
	}
	resp := s.PostJSON(t, "/_synapse/admin/v1/register", "", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shared-secret registration of %q: status = %d, want 200\nbody: %s", localpart, resp.StatusCode, resp.Body)
	}
	var body struct {
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
		DeviceID    string `json:"device_id"`
	}
	resp.JSON(t, &body)
	return &Account{
		Localpart:   localpart,
		UserID:      body.UserID,
		DeviceID:    body.DeviceID,
		AccessToken: body.AccessToken,
		Admin:       admin,
	}
}

// Login authenticates with m.login.password and returns a client for a fresh device.
func (s *Server) Login(t *testing.T, user, password string) *Account {
	t.Helper()

	resp := s.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
		"type": "m.login.password",
		"identifier": map[string]any{
			"type": "m.id.user",
			"user": user,
		},
		"password": password,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login as %q: status = %d, want 200\nbody: %s", user, resp.StatusCode, resp.Body)
	}
	var body struct {
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
		DeviceID    string `json:"device_id"`
	}
	resp.JSON(t, &body)
	localpart := strings.TrimSuffix(strings.TrimPrefix(body.UserID, "@"), ":"+s.ServerName)
	return &Account{
		Localpart:   localpart,
		UserID:      body.UserID,
		DeviceID:    body.DeviceID,
		AccessToken: body.AccessToken,
	}
}

// SharedSecretNonce fetches a registration nonce, failing the test if the endpoint is not
// enabled (which would silently turn every dependent test into a skip).
func (s *Server) SharedSecretNonce(t *testing.T) string {
	t.Helper()

	resp := s.Get(t, "/_synapse/admin/v1/register", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /_synapse/admin/v1/register: status = %d, want 200 (is the registration secret configured?)\nbody: %s",
			resp.StatusCode, resp.Body)
	}
	var body struct {
		Nonce string `json:"nonce"`
	}
	resp.JSON(t, &body)
	if body.Nonce == "" {
		t.Fatalf("registration nonce response has no nonce: %s", resp.Body)
	}
	return body.Nonce
}

// SharedSecretMAC computes the Synapse shared-secret registration MAC: HMAC-SHA1 keyed
// with the shared secret over nonce NUL user NUL password NUL ("admin"|"notadmin"). It is
// the same recipe Complement implements, reimplemented here so that the in-process suite
// tests the server against the protocol rather than against the server's own idea of it.
func SharedSecretMAC(nonce, localpart, password string, admin bool) string {
	return SharedSecretMACWithSecret(RegistrationSecret, nonce, localpart, password, admin)
}

// SharedSecretMACWithSecret is [SharedSecretMAC] with an explicit secret, for the test that
// has to tell "right recipe, wrong key" apart from "wrong recipe".
func SharedSecretMACWithSecret(secret, nonce, localpart, password string, admin bool) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(nonce))
	mac.Write([]byte{0})
	mac.Write([]byte(localpart))
	mac.Write([]byte{0})
	mac.Write([]byte(password))
	mac.Write([]byte{0})
	if admin {
		mac.Write([]byte("admin"))
	} else {
		mac.Write([]byte("notadmin"))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// MustSync runs a /sync and decodes it, failing the test on any problem.
func (a *Account) MustSync(t *testing.T, s *Server, params url.Values) SyncResponse {
	t.Helper()

	// A test that asks for a sync wants an answer, not a long-poll, so timeout=0 is the
	// default and a caller has to ask for a long-poll on purpose. Without this, a test that
	// passes any query parameter silently waits out the server's long-poll cap.
	if params == nil {
		params = url.Values{}
	}
	if params.Get("timeout") == "" {
		params.Set("timeout", "0")
	}
	path := "/_matrix/client/v3/sync?" + params.Encode()
	resp := s.Get(t, path, a.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync: status = %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}
	var parsed SyncResponse
	resp.JSON(t, &parsed)
	return parsed
}

// SyncResponse is the part of a /sync response tests reach for most.
type SyncResponse struct {
	NextBatch   string        `json:"next_batch"`
	Rooms       SyncRooms     `json:"rooms"`
	AccountData SyncEventList `json:"account_data"`
	ToDevice    SyncEventList `json:"to_device"`
	DeviceLists struct {
		Changed []string `json:"changed"`
		Left    []string `json:"left"`
	} `json:"device_lists"`
	DeviceOneTimeKeysCount map[string]int `json:"device_one_time_keys_count"`
	DeviceFallbackKeys     map[string]int `json:"device_fallback_keys"`
	Presence               SyncEventList  `json:"presence"`
}

// SyncRooms is the `rooms` object.
type SyncRooms struct {
	Join   map[string]SyncJoinedRoom  `json:"join"`
	Invite map[string]SyncInvitedRoom `json:"invite"`
	Leave  map[string]SyncLeftRoom    `json:"leave"`
	Knock  map[string]SyncKnockedRoom `json:"knock"`
}

// SyncEventList is a list of events with a prev_batch, as used by timeline/state/ephemeral.
type SyncEventList struct {
	Events    []json.RawMessage `json:"events"`
	PrevBatch string            `json:"prev_batch"`
	Limited   bool              `json:"limited"`
}

// SyncJoinedRoom is one joined room in a sync response.
type SyncJoinedRoom struct {
	Timeline    SyncEventList `json:"timeline"`
	State       SyncEventList `json:"state"`
	Ephemeral   SyncEventList `json:"ephemeral"`
	AccountData SyncEventList `json:"account_data"`
	// UnreadNotifications is a pointer because a test has to be able to tell "the server
	// reported nothing unread" from "the server did not answer the question" — which is exactly
	// the distinction complement draws.
	UnreadNotifications *UnreadCounts `json:"unread_notifications"`
	// UnreadThreadNotifications is the per-thread split, present only when the request asked
	// for it with `{"room":{"timeline":{"unread_thread_notifications":true}}}`.
	UnreadThreadNotifications map[string]UnreadCounts `json:"unread_thread_notifications"`
	NotificationCounts        struct {
		NotificationCount int64 `json:"notification_count"`
		HighlightCount    int64 `json:"highlight_count"`
	} `json:"notification_counts"`
	Summary map[string]any `json:"summary"`
}

// SyncInvitedRoom is one invited room in a sync response.
type SyncInvitedRoom struct {
	InviteState SyncEventList `json:"invite_state"`
}

// SyncLeftRoom is one left room in a sync response.
type SyncLeftRoom struct {
	Timeline SyncEventList `json:"timeline"`
	State    SyncEventList `json:"state"`
}

// SyncKnockedRoom is one knocked room in a sync response.
type SyncKnockedRoom struct {
	KnockState struct {
		Events []json.RawMessage `json:"events"`
	} `json:"knock_state"`
}

// Event is the decoded shape of a timeline event, enough for assertions without pulling in
// a full event implementation.
type Event struct {
	Type           string          `json:"type"`
	StateKey       *string         `json:"state_key"`
	Sender         string          `json:"sender"`
	EventID        string          `json:"event_id"`
	OriginServerTS int64           `json:"origin_server_ts"`
	Content        json.RawMessage `json:"content"`
	Unsigned       struct {
		Age           *int64          `json:"age"`
		TransactionID *string         `json:"transaction_id"`
		PrevContent   json.RawMessage `json:"prev_content"`
		ReplacesState *string         `json:"replaces_state"`
	} `json:"unsigned"`
}

// DecodeEvents turns a list of raw events into [Event] values.
func DecodeEvents(t *testing.T, raw []json.RawMessage) []Event {
	t.Helper()
	out := make([]Event, 0, len(raw))
	for _, item := range raw {
		var event Event
		if err := json.Unmarshal(item, &event); err != nil {
			t.Fatalf("event is not valid JSON: %v\n%s", err, item)
		}
		out = append(out, event)
	}
	return out
}

// MustCreateRoom is a shorthand for the most common setup step.
func (a *Account) MustCreateRoom(t *testing.T, s *Server, body map[string]any) string {
	t.Helper()

	resp := s.PostJSON(t, "/_matrix/client/v3/createRoom", a.AccessToken, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("createRoom: status = %d, want 200\nbody: %s\nserver log:\n%s",
			resp.StatusCode, resp.Body, s.Logs())
	}
	var parsed struct {
		RoomID string `json:"room_id"`
	}
	resp.JSON(t, &parsed)
	if parsed.RoomID == "" {
		t.Fatalf("createRoom returned no room_id: %s", resp.Body)
	}
	return parsed.RoomID
}

// PathEscape is a shorthand for building room-scoped paths, where the room ID's "!" and
// ":" must be escaped.
func PathEscape(parts ...string) string {
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, url.PathEscape(part))
	}
	return strings.Join(escaped, "/")
}

// syncBuffer is a bytes.Buffer with a mutex, so the server's goroutines can write to it
// while the test reads from it.
type syncBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// MustGet is shorthand for an authenticated GET that must succeed.
func (a *Account) MustGet(t *testing.T, s *Server, path string) *Response {
	t.Helper()
	return s.Get(t, path, a.AccessToken).MustStatus(t, http.StatusOK)
}

// MustPostJSON is shorthand for an authenticated JSON POST that must succeed.
func (a *Account) MustPostJSON(t *testing.T, s *Server, path string, body any) *Response {
	t.Helper()
	return s.PostJSON(t, path, a.AccessToken, body).MustStatus(t, http.StatusOK)
}

// MustPutJSON is shorthand for an authenticated JSON PUT that must succeed.
func (a *Account) MustPutJSON(t *testing.T, s *Server, path string, body any) *Response {
	t.Helper()
	return s.PutJSON(t, path, a.AccessToken, body).MustStatus(t, http.StatusOK)
}

// MustSendText sends an m.text message and returns the event ID.
//
// The transaction ID is derived from the call sequence rather than taken from the caller,
// because a test that forgot to vary it would silently exercise the idempotency path and
// see its own second message disappear.
func (a *Account) MustSendText(t *testing.T, s *Server, roomID, body string) string {
	t.Helper()

	txnID := fmt.Sprintf("txn-%d", a.txnCounter.Add(1))
	resp := a.MustPutJSON(t, s,
		"/_matrix/client/v3/rooms/"+PathEscape(roomID, "send", "m.room.message", txnID),
		map[string]any{"msgtype": "m.text", "body": body})
	var parsed struct {
		EventID string `json:"event_id"`
	}
	resp.JSON(t, &parsed)
	if parsed.EventID == "" {
		t.Fatalf("send returned no event_id: %s", resp.Body)
	}
	return parsed.EventID
}

// MustSetState writes a state event and returns its event ID.
func (a *Account) MustSetState(t *testing.T, s *Server, roomID, eventType, stateKey string, content any) string {
	t.Helper()

	path := "/_matrix/client/v3/rooms/" + PathEscape(roomID, "state", eventType)
	if stateKey != "" {
		path += "/" + PathEscape(stateKey)
	}
	resp := a.MustPutJSON(t, s, path, map[string]any{"content": content})
	var parsed struct {
		EventID string `json:"event_id"`
	}
	resp.JSON(t, &parsed)
	if parsed.EventID == "" {
		t.Fatalf("setting %s state returned no event_id: %s", eventType, resp.Body)
	}
	return parsed.EventID
}

// MustJoinRoom joins a room by ID or alias.
func (a *Account) MustJoinRoom(t *testing.T, s *Server, roomID string) string {
	t.Helper()
	resp := a.MustPostJSON(t, s, "/_matrix/client/v3/rooms/"+PathEscape(roomID)+"/join", map[string]any{})
	var parsed struct {
		RoomID string `json:"room_id"`
	}
	resp.JSON(t, &parsed)
	return parsed.RoomID
}

// MustInvite invites a user by ID.
func (a *Account) MustInvite(t *testing.T, s *Server, roomID, userID string) {
	t.Helper()
	a.MustPostJSON(t, s, "/_matrix/client/v3/rooms/"+PathEscape(roomID)+"/invite",
		map[string]any{"user_id": userID})
}

// MustLeaveRoom leaves a room.
func (a *Account) MustLeaveRoom(t *testing.T, s *Server, roomID string) {
	t.Helper()
	a.MustPostJSON(t, s, "/_matrix/client/v3/rooms/"+PathEscape(roomID)+"/leave", map[string]any{})
}

// SyncUntil runs /sync until `match` accepts the response, or the deadline passes.
//
// A long-polling endpoint makes a test that reads once racy in the other direction: the
// event it wants may have been written after the request was built. Polling with a deadline
// is what Complement's own client does, so the harness matches it.
func (a *Account) SyncUntil(t *testing.T, s *Server, since *string, match func(SyncResponse) bool) SyncResponse {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		params := url.Values{}
		if since != nil && *since != "" {
			params.Set("since", *since)
		}
		params.Set("timeout", "0")
		response := a.MustSync(t, s, params)
		if match(response) {
			if since != nil {
				*since = response.NextBatch
			}
			return response
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync never matched the expected response (last next_batch %s)", response.NextBatch)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// MustTimelineHas fails unless the given room's timeline, in a sync the account performs
// now, contains an event of `eventType` whose body matches `body`.
func (a *Account) MustTimelineHas(t *testing.T, s *Server, roomID, eventType, body string) {
	t.Helper()

	var since string
	response := a.SyncUntil(t, s, &since, func(sync SyncResponse) bool {
		room, ok := sync.Rooms.Join[roomID]
		if !ok {
			return false
		}
		for _, raw := range room.Timeline.Events {
			var event Event
			if err := json.Unmarshal(raw, &event); err != nil {
				continue
			}
			if event.Type != eventType {
				continue
			}
			if body == "" {
				return true
			}
			var content struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(event.Content, &content); err == nil && content.Body == body {
				return true
			}
		}
		return false
	})
	_ = response
}

// ErrorBody is a Matrix error body as tests read it.
type ErrorBody struct {
	Errcode    string `json:"errcode"`
	Error      string `json:"error"`
	RetryAfter int64  `json:"retry_after_ms"`
	SoftLogout bool   `json:"soft_logout"`
}

// ErrorBody decodes the response as a Matrix error, failing the test if the body is not one.
//
// Tests assert on error codes rather than messages because the codes are protocol and the
// messages are prose; this helper makes the protocol assertion the easy one to write.
func (r *Response) ErrorBody(t *testing.T) ErrorBody {
	t.Helper()

	var body ErrorBody
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, r.Body)
	}
	if body.Errcode == "" {
		t.Fatalf("error body has no errcode: %s", r.Body)
	}
	return body
}

// Do runs a raw authenticated request as this account.
func (a *Account) Do(t *testing.T, s *Server, req Request) *Response {
	t.Helper()
	if req.Token == "" {
		req.Token = a.AccessToken
	}
	return s.Do(t, req)
}

// MustDelete issues a DELETE that must succeed.
func (a *Account) MustDelete(t *testing.T, s *Server, path string) *Response {
	t.Helper()
	return a.Do(t, s, Request{Method: http.MethodDelete, Path: path}).MustStatus(t, http.StatusOK)
}

// NewWithRegistration is [New] with shared-secret registration enabled.
//
// Suites that need accounts rather than testing registration use this, so that "the server
// cannot create users" fails as one clear error in a helper rather than as a confusing 404
// in fifteen places.
func NewWithRegistration(t *testing.T, opts ...Option) *Server {
	t.Helper()
	return New(t, append([]Option{WithRegistrationSecret(RegistrationSecret)}, opts...)...)
}

// FederationServer starts the server-server API on a TLS listener and returns it.
//
// Federation is HTTPS by specification, and a peer's certificate is verified against the
// server name, so a plain HTTP test server cannot stand in for it. Verification is skipped
// on the returned client for the same reason Complement skips it: this test is about the
// protocol, not about public certificate authorities.
func (s *Server) FederationServer(t *testing.T) *httptest.Server {
	t.Helper()

	if s.federationSrv != nil {
		return s.federationSrv
	}
	tls := httptest.NewTLSServer(s.instance.FederationHandler())
	s.federationSrv = tls
	s.federationClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tlsconfig.Config{InsecureSkipVerify: true}},
	}
	t.Cleanup(tls.Close)
	return tls
}

// FederationClient is an HTTP client that will talk to FederationServer.
func (s *Server) FederationClient(t *testing.T) *http.Client {
	t.Helper()

	s.FederationServer(t)
	return s.federationClient
}

// UnreadCounts is one badge: events worth a notification, and the subset worth a highlight.
type UnreadCounts struct {
	NotificationCount int64 `json:"notification_count"`
	HighlightCount    int64 `json:"highlight_count"`
}

// Nonce returns a unique string for a request path, used for transaction IDs by tests that send
// more than one event and must not have the second one read as a retry of the first.
func Nonce(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, nonceCounter.Add(1))
}

var nonceCounter atomic.Uint64
