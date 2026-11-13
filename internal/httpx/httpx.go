// Package httpx is the HTTP plumbing every handler shares: JSON and error responses in the
// shape the specification mandates, the middleware chain (recovery, request identity, CORS,
// cache headers, rate limiting), and access-token authentication.
//
// It deliberately depends on nothing above it in the dependency graph: authentication is
// expressed as a [SessionResolver] interface implemented by the accounts service, so the
// middleware can be tested without a database.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// MaxRequestBodyBytes is the default ceiling for JSON request bodies. The Matrix spec
// caps federation PDUs at 65536 bytes; client requests are allowed more headroom for
// uploads handled elsewhere, but a JSON body this large is always a mistake.
const MaxRequestBodyBytes = 512 << 10

// MaxEventBodyBytes is the largest request body that can turn into an event.
//
// The limit is not this server's preference: an event whose serialised form is larger than
// this cannot be relayed by any other homeserver, so accepting one would leave a room whose
// history stops at this building. It is checked against the request body rather than the
// finished event because the answer must not depend on how the server happens to encode the
// rest of the event, and because a client that sent 70 KB wants to hear 413 before the
// server parses anything.
const MaxEventBodyBytes = 65536

// ReadEventBody reads a request body that is meant to become an event.
func ReadEventBody(r *http.Request) ([]byte, error) {
	raw, err := ReadRawJSON(r)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxEventBodyBytes {
		return nil, matrix.NewAPIError(http.StatusRequestEntityTooLarge, matrix.ErrTooLarge,
			"Event is %d bytes, the limit is %d", len(raw), MaxEventBodyBytes)
	}
	return raw, nil
}

// Session is the authenticated caller injected into the request context.
type Session struct {
	// Localpart is the caller's user ID local part, without the sigil or domain.
	Localpart string
	// UserID is the fully qualified user ID.
	UserID string
	// DeviceID is the device the access token belongs to.
	DeviceID string
	// AccessToken is the token that authenticated this request; needed by logout and by
	// endpoints that revoke the caller's own session.
	AccessToken string
	// Admin is true when the user has administrator rights.
	Admin bool
	// AppService marks a session authenticated by an application service token rather than by a
	// device session this server issued. It is not decoration: it is the difference between a caller
	// that speaks for one device and one that speaks for a program this server delegated a slice of
	// its user namespace to, and the handful of capabilities only the latter holds are precisely the
	// ones that must not leak out of it.
	AppService bool
	// AppServiceID names which registered service the caller is, for logs and for attributing what
	// it did. Empty unless AppService is true.
	AppServiceID string
	// SoftLogout marks a session whose token is expired but whose identity is known: the
	// caller must re-authenticate rather than re-register.
	SoftLogout bool
}

// SessionResolver turns an access token into a [Session]. The accounts service implements
// it; tests can hand in a map.
type SessionResolver interface {
	// ResolveSession returns the session for token, or a *matrix.APIError with
	// M_MISSING_TOKEN / M_UNKNOWN_TOKEN when there is none.
	ResolveSession(r *http.Request, token string) (*Session, error)
}

// sessionKey is the context key for the authenticated session.
type sessionKey struct{}

// RespondJSON writes payload as JSON with the given status.
//
// The Content-Type is always exactly "application/json" (no charset): the specification
// asks for it and clients, including Complement's strict matchers, compare it.
// RespondRawJSON writes bytes that are already a JSON document.
//
// This exists for responses that hand back a stored event verbatim: re-encoding an event
// through Go's encoder would escape `<`, `>` and `&` inside its content, changing bytes the
// event's hash commits to.
func RespondRawJSON(w http.ResponseWriter, status int, document []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(document)))
	w.WriteHeader(status)
	if _, err := w.Write(document); err != nil {
		slog.Debug("writing a raw JSON response", "error", err)
	}
}

func RespondJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// A payload that cannot be marshalled is a programming error, and answering
		// with the partially written body would be worse than a bare 500.
		slog.Error("respond: marshalling response body failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		slog.Debug("respond: writing body failed", "error", err)
	}
}

// RespondError maps err to a Matrix error response. Anything that is not already a
// *matrix.APIError becomes M_UNKNOWN with the message withheld from the client and logged
// instead: internal failures must not leak through an error field.
func RespondError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr, ok := matrix.AsAPIError(err)
	if !ok {
		status := http.StatusInternalServerError
		if r.Context().Err() != nil {
			// The client hung up; there is nothing to answer and nothing to fix.
			return
		}
		slog.Error("request failed",
			"method", r.Method, "path", r.URL.Path, "error", err, "stack", string(debug.Stack()))
		RespondJSON(w, status, errorBody{ErrCode: matrix.ErrUnknown, Error: "Internal server error"})
		return
	}
	body := errorBody{ErrCode: apiErr.Code, Error: apiErr.Message}
	if apiErr.RetryAfterMS > 0 {
		body.RetryAfterMS = apiErr.RetryAfterMS
		w.Header().Set("Retry-After", fmt.Sprintf("%d", (apiErr.RetryAfterMS+999)/1000))
	}
	if apiErr.SoftLogout {
		body.SoftLogout = true
	}
	if len(apiErr.Links) > 0 {
		body.Links = apiErr.Links
	}
	if apiErr.RoomVersion != "" {
		body.RoomVersion = apiErr.RoomVersion
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="matrix"`)
	RespondJSON(w, apiErr.Status, body)
}

// errorBody is the wire shape of a Matrix error.
type errorBody struct {
	ErrCode      matrix.ErrCode      `json:"errcode"`
	Error        string              `json:"error"`
	RetryAfterMS int64               `json:"retry_after_ms,omitempty"`
	SoftLogout   bool                `json:"soft_logout,omitempty"`
	Links        []map[string]string `json:"links,omitempty"`
	// RoomVersion accompanies M_INCOMPATIBLE_ROOM_VERSION, naming the version to move to.
	RoomVersion string `json:"room_version,omitempty"`
}

// DecodeJSON reads a JSON request body into dst, translating every failure mode into the
// specification's error codes. A missing body for an endpoint that requires one is
// M_NOT_JSON (there was nothing to parse) rather than M_BAD_JSON.
func DecodeJSON(r *http.Request, dst any) error {
	raw, err := ReadRawJSON(r)
	if err != nil {
		return err
	}
	// UseNumber keeps an integer as the client wrote it: decoding into `any` would turn a
	// large depth or origin_server_ts into a float64, and a value that came back as 1e+18
	// would be written straight back into a room event.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return matrix.DecodeFailure("the request body", err)
	}
	return nil
}

// DecodeJSONIfPresent decodes a request body that the specification makes optional.
//
// The absence of a body has to be told apart from a broken one, and [http.Request]'s
// ContentLength cannot do it alone: a request with no Content-Length header at all reports -1,
// which is not zero, so a handler that only skips decoding on 0 tries to parse nothing and
// answers 400 M_NOT_JSON to a client that sent no body. That is how a plain
// POST /rooms/{id}/join — which has no body — can be refused by a server that means to accept
// it. A body that is present and malformed is still reported, because that is a mistake the
// client should hear about.
func DecodeJSONIfPresent(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading request body: %w", err)
	}
	defer func() { _ = r.Body.Close() }()
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if !json.Valid(body) {
		return matrix.NewBadJSON("request body is not valid JSON")
	}
	if !utf8.Valid(body) {
		return matrix.NewNotJSON("request body is not valid UTF-8 JSON")
	}
	if int64(len(body)) > MaxRequestBodyBytes {
		return matrix.NewAPIError(http.StatusRequestEntityTooLarge, matrix.ErrTooLarge,
			"Request body exceeds the %d byte limit", MaxRequestBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return matrix.DecodeFailure("the request body", err)
	}
	return nil
}

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies the middlewares in order, so that the first one listed is the outermost.
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// Recoverer turns a panic inside a handler into a 500 with the stack logged, so one bad
// request cannot take the process down.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic while serving request",
					"method", r.Method, "path", r.URL.Path, "panic", rec,
					"stack", string(debug.Stack()))
				RespondJSON(w, http.StatusInternalServerError, errorBody{
					ErrCode: matrix.ErrUnknown,
					Error:   "Internal server error",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CORS answers preflight requests and lets browser clients reach the API, as the
// specification requires for the client-server API.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		h.Set("Access-Control-Allow-Headers", "Origin, Safer, Progress-ID, Cache-Control, Content-Type, Content-Length, Authorization, X-Matrix-Trace")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Max-Age", "1728000")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NoStore stops caches from holding authenticated responses. A Matrix sync response is a
// function of the caller's token, which no HTTP cache may share.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-visit")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status so the logging middleware can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += n
	return n, err
}

// Flush passes through to the underlying writer when it supports flushing, which the
// streaming endpoints need.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// LogRequests logs one line per request after it completes.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		slog.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration", time.Since(start),
			"remote", r.RemoteAddr,
		)
	})
}

// AccessTokenFromRequest extracts the caller's access token, preferring the
// Authorization header and falling back to the (deprecated but still universally sent)
// access_token query parameter.
func AccessTokenFromRequest(r *http.Request) string {
	if header := r.Header.Get("Authorization"); header != "" {
		scheme, token, ok := strings.Cut(header, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
		return strings.TrimSpace(header)
	}
	return r.URL.Query().Get("access_token")
}

// RequireAuth returns a handler that resolves the access token before calling next. The
// resolved session is available through [SessionFrom].
func RequireAuth(resolver SessionResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := AccessTokenFromRequest(r)
		if token == "" {
			RespondError(w, r, matrix.NewMissingAccessToken())
			return
		}
		session, err := resolver.ResolveSession(r, token)
		if err != nil {
			RespondError(w, r, err)
			return
		}
		if err := checkImpersonation(r, session); err != nil {
			RespondError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withSession(r, session)))
	})
}

// checkImpersonation enforces the one rule that makes `?user_id=` safe: naming the user you are
// speaking for is an application service's privilege, because that is how a bridge sends as the
// people it bridges.
//
// A normal session that names *itself* is tolerated — it is a no-op, and refusing it would break a
// client that echoes the identity it already authenticated as. A normal session that names somebody
// else is refused rather than ignored: ignoring an identity claim leaves the client believing it sent
// as that user, and the two of them disagree about who wrote the message.
func checkImpersonation(r *http.Request, session *Session) error {
	if session.AppService {
		return nil
	}
	named := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if named == "" || named == session.UserID {
		return nil
	}
	return matrix.NewForbidden("only an application service may act as another user")
}

// OptionalAuth resolves the token when one is present but does not fail without it.
// Endpoints whose answer depends on who is asking (GET /versions, GET /publicRooms) use it.
func OptionalAuth(resolver SessionResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := AccessTokenFromRequest(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if session, err := resolver.ResolveSession(r, token); err == nil {
			next.ServeHTTP(w, r.WithContext(withSession(r, session)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withSession(r *http.Request, s *Session) context.Context {
	return context.WithValue(r.Context(), sessionKey{}, s)
}

// SessionFrom returns the authenticated session, or nil when the request is not
// authenticated.
func SessionFrom(r *http.Request) *Session {
	s, _ := r.Context().Value(sessionKey{}).(*Session)
	return s
}

// MustSession returns the authenticated session's localpart, panicking only when a
// authenticated-only route was reached without authentication, which is a routing bug.
func MustSession(r *http.Request) *Session {
	s := SessionFrom(r)
	if s == nil {
		panic("httpx.MustSession: route reached without a session; is RequireAuth missing?")
	}
	return s
}

// ReadRawJSON reads a request body as JSON without decoding it into a type.
//
// Endpoints whose request body *is* the data (a message's content, an account-data object)
// need the bytes: decoding into a Go map and re-encoding would reorder keys, lose numeric
// precision, and HTML-escape strings, all of which the client did not sign up for.
func ReadRawJSON(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, matrix.NewNotJSON("request has no body")
	}
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}
	if int64(len(body)) > MaxRequestBodyBytes {
		return nil, matrix.NewAPIError(http.StatusRequestEntityTooLarge, matrix.ErrTooLarge,
			"Request body exceeds the %d byte limit", MaxRequestBodyBytes)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, matrix.NewNotJSON("request has no JSON body")
	}
	if !json.Valid(body) {
		return nil, matrix.NewBadJSON("request body is not valid JSON")
	}
	// Go's decoder silently replaces invalid UTF-8 inside a string, which would let a
	// client store text that nobody can round-trip; the specification calls this M_NOT_JSON.
	if !utf8.Valid(body) {
		return nil, matrix.NewNotJSON("request body is not valid UTF-8 JSON")
	}
	return body, nil
}
