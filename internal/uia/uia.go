// Package uia implements User-Interactive Authentication: the state machine behind every
// endpoint that answers "prove who you are first" with a 401 and a list of flows.
//
// Sessions live in memory and expire. They hold only what a flow needs to finish: the
// stages still outstanding, what has been satisfied, and the parameters the client is
// accumulating (which for `m.login.email.identity` is the whole point of a session).
// Nothing a client sends inside `auth` is trusted until the flow that requires it has run.
package uia

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// Stage names, spelled once.
const (
	StagePassword      = "m.login.password"
	StageToken         = "m.login.token"
	StageDummy         = "m.login.dummy"
	StageRegistration  = "m.login.registration_shared_secret"
	StageEmailIdentity = "m.login.email.identity"
	StageMSISDN        = "m.login.msisdn"
)

// SessionValidity is how long a client may take to complete a flow. Synapse uses 20
// minutes; the same number keeps a half-finished registration from lingering while still
// allowing a user to go and read an email.
const SessionValidity = 20 * time.Minute

// Session is one in-flight authentication exchange.
type Session struct {
	// ID is the opaque identifier the client echoes back in `auth.session`.
	ID string
	// Stages are the stages this flow still needs, in any order.
	Stages []string
	// Completed holds the stages that have been satisfied.
	Completed map[string]struct{}
	// Params accumulates client-supplied parameters across attempts.
	Params map[string]any
	// Owner is the authenticated user ID for flows that started from an authenticated
	// endpoint (password-protected operations), empty for registration.
	Owner string
	// Created and Expires bound the session's lifetime.
	Created time.Time
	Expires time.Time
	// Attempt is incremented per failed attempt so a client cannot loop on one session
	// forever without re-fetching the flows.
	Attempt int
}

// Missing returns the stages the flow still needs.
func (s *Session) Missing() []string {
	missing := make([]string, 0, len(s.Stages))
	for _, stage := range s.Stages {
		if _, done := s.Completed[stage]; !done {
			missing = append(missing, stage)
		}
	}
	return missing
}

// Complete reports whether every stage of the flow is satisfied.
func (s *Session) Complete() bool { return len(s.Missing()) == 0 }

// Manager owns the live sessions.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	now      func() time.Time
	// validity is a field so tests can shorten the expiry rather than sleep.
	validity time.Duration
}

// NewManager returns a manager with the default session validity.
func NewManager() *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		now:      time.Now,
		validity: SessionValidity,
	}
}

// Start begins a flow for the given stages. A flow with no stages is not a flow: callers
// use that for endpoints that need no authentication at all.
func (m *Manager) Start(stages []string, owner string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.collectLocked()
	session := &Session{
		ID:        matrix.Nonce(),
		Stages:    append([]string(nil), stages...),
		Completed: map[string]struct{}{},
		Params:    map[string]any{},
		Owner:     owner,
		Created:   m.now(),
		Expires:   m.now().Add(m.validity),
	}
	m.sessions[session.ID] = session
	return session
}

// Get returns a live session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.collectLocked()
	session, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	return session, true
}

// Complete marks one stage of a session as satisfied and returns the session.
func (m *Manager) Complete(sessionID, stage string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, matrix.NewAPIError(http.StatusUnauthorized, matrix.ErrSessionNotFound,
			"Unknown auth session")
	}
	session.Completed[stage] = struct{}{}
	return session, nil
}

// Fail records a failed attempt, so that a client spinning on wrong credentials can be
// told to start over once it has tried too often.
func (m *Manager) Fail(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[sessionID]; ok {
		session.Attempt++
	}
}

// Drop removes a session, used when a flow finishes or is abandoned.
func (m *Manager) Drop(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}

// collectLocked removes expired sessions. The caller holds the lock.
func (m *Manager) collectLocked() {
	now := m.now()
	for id, session := range m.sessions {
		if now.After(session.Expires) {
			delete(m.sessions, id)
		}
	}
}

// Challenge is the 401 body that tells the client which flows are available. It is the
// same shape for every endpoint that uses UIA.
type Challenge struct {
	Flows []Flow `json:"flows"`
	// Params carries per-stage hints (for m.login.password, the user name to retype). It is
	// always an object, even when there is nothing to say: a client that has to handle both
	// "params is absent" and "params is empty" is a client with a crash waiting in the case it
	// never tested.
	Params         map[string]any `json:"params"`
	Session        string         `json:"session"`
	Completed      []string       `json:"completed,omitempty"`
	UnavailableReq []string       `json:"unavailable_stages,omitempty"`
	// ErrCode and Error are filled in when the client *tried* a stage and failed, and left
	// empty for the first challenge, where nothing has been attempted yet. Both belong in
	// the same object as the flows because a client must be able to tell "you have not
	// authenticated" apart from "you failed to authenticate" from one response.
	ErrCode matrix.ErrCode `json:"errcode,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// Flow is one alternative set of stages.
type Flow struct {
	Stages []string `json:"stages"`
}

// ChallengeFor builds the challenge for a session, sorted so that the same flows produce
// the same bytes (which keeps client-side caching and test expectations stable).
func ChallengeFor(session *Session, unavailable []string) Challenge {
	flows := []Flow{{Stages: append([]string(nil), session.Stages...)}}
	sort.Strings(flows[0].Stages)
	completed := make([]string, 0, len(session.Completed))
	for stage := range session.Completed {
		completed = append(completed, stage)
	}
	sort.Strings(completed)
	// A nil map would encode as null; an empty object is what the schema describes.
	params := session.Params
	if params == nil {
		params = map[string]any{}
	}
	return Challenge{
		Flows:          flows,
		Params:         params,
		Session:        session.ID,
		Completed:      completed,
		UnavailableReq: unavailable,
	}
}

// WithFailure returns a copy of the challenge annotated with why the attempt failed.
func (c Challenge) WithFailure(code matrix.ErrCode, format string, args ...any) Challenge {
	failed := c
	failed.ErrCode = code
	failed.Error = format
	if len(args) > 0 {
		failed.Error = fmt.Sprintf(format, args...)
	}
	return failed
}

// ErrIncompleteAuth is returned when a client has not yet satisfied every stage; the
// handler turns it into a 401 carrying the challenge.
var ErrIncompleteAuth = errors.New("uia: authentication incomplete")

// AuthBody is the `auth` object every UIA request carries.
type AuthBody struct {
	Type       string      `json:"type"`
	Session    string      `json:"session,omitempty"`
	Password   string      `json:"password,omitempty"`
	Identifier *Identifier `json:"identifier,omitempty"`
	User       string      `json:"user,omitempty"`
	Medium     string      `json:"medium,omitempty"`
	Token      string      `json:"token,omitempty"`
	Address    string      `json:"address,omitempty"`
	// Extra preserves the parts of the body this package does not model, so a service can
	// read what it needs without re-decoding.
	Extra map[string]any `json:"-"`

	// Issuer and Subject belong to the MSC3861/OIDC-shaped password identifier; they are
	// accepted and ignored so that a modern client's extra fields do not fail the flow.
	Issuer  string `json:"issuer,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// Identifier is the `identifier` object of m.login.password.
type Identifier struct {
	Type   string `json:"type"`
	User   string `json:"user"`
	Issuer string `json:"issuer,omitempty"`
	// Medium and Address belong to third-party identifiers, which qwatrix does not
	// authenticate; the fields exist so the JSON decodes without losing data.
	Medium  string `json:"medium,omitempty"`
	Address string `json:"address,omitempty"`
}

// ClaimedUser returns the user the credentials claim to be, either from
// `identifier.user` or the deprecated top-level `user`, as a local part or a full user ID.
func (a *AuthBody) ClaimedUser() (string, bool) {
	if a.Identifier != nil && a.Identifier.User != "" {
		return a.Identifier.User, true
	}
	if a.User != "" {
		return a.User, true
	}
	return "", false
}

// MatchesStage reports whether the credential type is the one the session still needs.
func (a *AuthBody) MatchesStage(session *Session) (stage string, ok bool) {
	for _, missing := range session.Missing() {
		if subtle.ConstantTimeCompare([]byte(missing), []byte(a.Type)) == 1 {
			return missing, true
		}
	}
	return "", false
}
