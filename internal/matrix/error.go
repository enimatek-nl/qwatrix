// Package matrix holds the value types of the Matrix specification that every layer of
// the homeserver needs: the standard error body, identifiers, and the protocol version
// list. It performs no I/O and knows nothing about HTTP or storage.
package matrix

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrCode is a Matrix standard error code. The spec spells some of them in an odd mix of
// cases (M_INCOMPATIBLE_ROOM_VERSION is M_IncompatibleRoomVersion), so they are values
// rather than a prefix-and-upper-case rule.
type ErrCode string

// Standard error codes from the client-server specification, §Application Service API and
// the room version machinery. Only codes qwatrix actually returns are listed.
const (
	ErrUnknownToken ErrCode = "M_UNKNOWN_TOKEN"
	ErrMissingToken ErrCode = "M_MISSING_TOKEN"
	ErrBadJSON      ErrCode = "M_BAD_JSON"
	ErrNotJSON      ErrCode = "M_NOT_JSON"
	ErrNotFound     ErrCode = "M_NOT_FOUND"
	ErrForbidden    ErrCode = "M_FORBIDDEN"
	ErrUnknown      ErrCode = "M_UNKNOWN"
	ErrUnrecognized ErrCode = "M_UNRECOGNIZED"
	// ErrIncompatibleRoomVersion is the federation answer when a peer asks for something in a
	// room version it has not agreed to speak. The response names the version the room actually
	// is, because that is the only thing the peer can act on.
	ErrIncompatibleRoomVersion ErrCode = "M_INCOMPATIBLE_ROOM_VERSION"
	ErrLimitExceeded           ErrCode = "M_LIMIT_EXCEEDED"
	ErrUserInUse               ErrCode = "M_USER_IN_USE"
	ErrInvalidUsername         ErrCode = "M_INVALID_USERNAME"
	ErrUserLocked              ErrCode = "M_USER_LOCKED"
	ErrUserDisabled            ErrCode = "M_USER_DISABLED"
	ErrWeakPassword            ErrCode = "M_WEAK_PASSWORD"
	ErrInvalidParam            ErrCode = "M_INVALID_PARAM"
	ErrExclusive               ErrCode = "M_EXCLUSIVE"
	ErrMissingParam            ErrCode = "M_MISSING_PARAM"
	ErrTooLarge                ErrCode = "M_TOO_LARGE"
	ErrTooManyRequests         ErrCode = "M_TOO_MANY_REQUESTS"
	ErrSessionNotFound         ErrCode = "M_SESSION_NOT_FOUND"
	ErrSessionInvalid          ErrCode = "M_SESSION_INVALID"
	ErrSessionModified         ErrCode = "M_SESSION_MODIFIED"
	ErrUnauthorized            ErrCode = "M_UNAUTHORIZED"
	ErrTransactionFailed       ErrCode = "M_TRANSACTION_FAILED"
	ErrUnsupportedVersion      ErrCode = "M_UNSUPPORTED_ROOM_VERSION"
	ErrIncompatibleVersion     ErrCode = "M_IncompatibleRoomVersion"
	ErrResourceLimitExceeded   ErrCode = "M_RESOURCE_LIMIT_EXCEEDED"
	ErrCannotLeaveServer       ErrCode = "M_CANNOT_LEAVE_SERVER"
	ErrRoomInUse               ErrCode = "M_ROOM_IN_USE"
	ErrBadStatus               ErrCode = "M_BAD_STATUS"
	ErrUnknownEndpointErr      ErrCode = "M_UNKNOWN_ENDPOINT"
	// ErrBadAlias is the 400 for an alias that does not resolve to the room claiming it. It is
	// distinct from M_INVALID_PARAM because the caller named a well-formed alias — the mistake
	// is about the room, not about the syntax, and a client correcting the syntax would retry
	// the same wrong claim.
	ErrBadAlias ErrCode = "M_BAD_ALIAS"
)

// APIError is a Matrix error response: an HTTP status, an error code and a human-readable
// message. Services return it; the HTTP layer serialises it. It is the only error shape
// that may reach a client, which is what keeps internal failures internal.
//
// The zero value is not useful; construct with NewAPIError or one of the helpers below.
type APIError struct {
	// Status is the HTTP status code to answer with.
	Status int
	// Code is the Matrix error code.
	Code ErrCode
	// Message is the human-readable explanation. It must not contain internal detail
	// (file paths, SQL, stack traces); see the repository's error-path rules.
	Message string
	// RetryAfterMS is set for M_LIMIT_EXCEEDED so clients know when to come back.
	RetryAfterMS int64
	// SoftLogout marks M_UNKNOWN_TOKEN errors whose session can be resumed by
	// re-authenticating, per the spec's soft-logout rule.
	SoftLogout bool
	// Links carries the optional `org.matrix.msc3861.v1` style links used by OAuth
	// errors; unused today but part of the wire shape.
	Links []map[string]string
	// RoomVersion is the room version a peer has to move to, carried by
	// M_INCOMPATIBLE_ROOM_VERSION responses as the specification requires.
	RoomVersion string
}

// Error implements error.
func (e *APIError) Error() string {
	if e == nil {
		return "<nil APIError>"
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

// NewAPIError builds an APIError with the given status, code and message.
func NewAPIError(status int, code ErrCode, format string, args ...any) *APIError {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &APIError{Status: status, Code: code, Message: msg}
}

// WithRetryAfter returns a copy carrying a retry_after_ms hint.
func (e *APIError) WithRetryAfter(d time.Duration) *APIError {
	copied := *e
	copied.RetryAfterMS = d.Milliseconds()
	return &copied
}

// WithSoftLogout returns a copy marked as a soft logout.
func (e *APIError) WithSoftLogout() *APIError {
	copied := *e
	copied.SoftLogout = true
	return &copied
}

// The helpers below exist so that call sites cannot drift in status or code: the pair is
// written once, here, and every endpoint reaches for the same value.

// NewMissingAccessToken is the 401 for a request that carried no credentials at all.
func NewMissingAccessToken() *APIError {
	return NewAPIError(http.StatusUnauthorized, ErrMissingToken, "Access token was not provided")
}

// NewUnknownAccessToken is the 401 for a token this server does not know: revoked, or
// never issued.
func NewUnknownAccessToken() *APIError {
	return NewAPIError(http.StatusUnauthorized, ErrUnknownToken, "Unrecognized access token")
}

// NewForbidden is the 403 for "you may not do that".
func NewForbidden(format string, args ...any) *APIError {
	return NewAPIError(http.StatusForbidden, ErrForbidden, format, args...)
}

// NewNotFound is the 404 for a room, event, user or media ID that is not here.
// NewIncompatibleRoomVersion answers the federation's "you did not say you could speak this room
// version" refusal. The version is in the body rather than only in the prose because the peer
// decides what to do next mechanically: retry with that version, or give up.
func NewIncompatibleRoomVersion(version string) *APIError {
	return &APIError{
		Status:      http.StatusBadRequest,
		Code:        ErrIncompatibleRoomVersion,
		Message:     "This room is not using a room version that this server can speak",
		RoomVersion: version,
	}
}

func NewNotFound(format string, args ...any) *APIError {
	return NewAPIError(http.StatusNotFound, ErrNotFound, format, args...)
}

// NewInvalidParam is the 400 for a parameter that is present but wrong.
func NewInvalidParam(format string, args ...any) *APIError {
	return NewAPIError(http.StatusBadRequest, ErrInvalidParam, format, args...)
}

// NewBadJSON is the 400 for a body that is JSON but not the shape that was asked for.
// DecodeFailure turns an error from decoding a request body into a refusal that describes the
// caller's mistake without describing this server.
//
// Go's own decoder errors are written for a programmer reading a stack trace and name the program:
// "json: cannot unmarshal array into Go struct field loginRequest.password of type string". Sent to
// a client, that says which types this homeserver is built from and which field of which struct
// parses a given endpoint — said, moreover, on the login and registration endpoints, which are the
// most publicly reachable and the least authenticated things a homeserver does. So the caller is
// told which of *their* fields was wrong, which is the part they can act on, and nothing else.
func DecodeFailure(subject string, err error) *APIError {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if typeErr.Field != "" {
			return NewAPIError(http.StatusBadRequest, ErrBadJSON,
				"%s field %q does not hold a value of the type this endpoint expects",
				subject, typeErr.Field)
		}
		return NewAPIError(http.StatusBadRequest, ErrBadJSON,
			"%s does not hold a value of the type this endpoint expects", subject)
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return NewAPIError(http.StatusBadRequest, ErrBadJSON, "%s is not valid JSON", subject)
	}
	// Anything else the decoder can say — an unsupported type, a destination that cannot be written
	// to, an internal mismatch — is a fact about this program rather than about the caller's bytes,
	// so it is reported as a malformed body and the detail is left where it belongs.
	//
	// Invalid UTF-8 is deliberately not handled here: `json.InvalidUTF8Error` is deprecated and the
	// decoder no longer produces it, so the check for it lives where the bytes are read.
	return NewBadJSON(subject + " could not be decoded as JSON")
}

func NewBadJSON(reason string) *APIError {
	return NewAPIError(http.StatusBadRequest, ErrBadJSON, "%s", reason)
}

// NewMissingParam is the 400 for a parameter the endpoint requires and the request lacks.
func NewMissingParam(name string) *APIError {
	return NewAPIError(http.StatusBadRequest, ErrMissingParam, "Missing parameters: %s", name)
}

// NewNotJSON is the 400 for a body that is not JSON at all.
func NewNotJSON(reason string) *APIError {
	return NewAPIError(http.StatusBadRequest, ErrNotJSON, "%s", reason)
}

// NewUnrecognized is the 404 for a route that does not exist, which is a distinguishable
// failure in Matrix and not a plain 404.
func NewUnrecognized(format string, args ...any) *APIError {
	return NewAPIError(http.StatusNotFound, ErrUnrecognized, format, args...)
}

// NewUnknown is a 500 whose message is safe to show: callers pass the real error to the
// logger, not to the client.
func NewUnknown(message string) *APIError {
	if message == "" {
		message = "Internal server error"
	}
	return NewAPIError(http.StatusInternalServerError, ErrUnknown, "%s", message)
}

// AsAPIError reports whether err (or anything it wraps) is an *APIError.
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}
