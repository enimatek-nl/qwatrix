package csapi

// The client-server surface of application-service support: the two things an application service may
// do that a device may not.
//
// Both come from the same fact. A device is one person's session, so its events happen when it sends
// them and are sent by the user it logged in as. A service is a program the server delegated a
// namespace to, and a program that imports history — a bridge carrying years of another network's chat
// into a room — needs to say when a message happened, and needs to say it about the users it bridges
// rather than about itself.
//
// Everything else about a service session is deliberately narrower than a device's, not wider: it has
// no /sync, because /sync is a queue for one person's devices and a service is told what it asked about
// over its own transaction endpoint.

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// importedTimestamp reads the `?ts=` override an application service may attach to an event it sends.
//
// For anybody else the parameter means nothing and is ignored rather than refused. MSC3316 puts it
// plainly: a server must ignore `ts` from a non-application-service sender. A mistaken client that
// always sends the parameter is not an attacker, and its events happening when they happen is exactly
// what it should get — the alternative, a refusal, would break a client for doing something the spec
// says is harmless.
//
// A service that sends something unparseable *is* told, because silently stamping the event "now"
// would give it a history that is wrong in a way nothing downstream can detect.
func importedTimestamp(session *httpx.Session, r *http.Request) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("ts"))
	if raw == "" {
		return time.Time{}, nil
	}
	if !session.AppService {
		slog.Debug("ignoring a timestamp override from a sender that may not choose one",
			"user_id", session.UserID, "ts", raw)
		return time.Time{}, nil
	}
	milliseconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, matrix.NewInvalidParam("ts must be an integer number of milliseconds since the Unix epoch")
	}
	return time.UnixMilli(milliseconds).UTC(), nil
}

// refuseApplicationServiceSync is the read an application service is not given.
//
// /sync is a per-device queue of what one user's clients have not seen yet. A service does not have
// devices in that sense, and handing it the whole room traffic it happens to be joined to would be a
// firehose with no acknowledgement, no retry and no subscription — which is what its transaction
// endpoint provides, and the reason the specification routes a service's events to a URL instead.
func refuseApplicationServiceSync(session *httpx.Session) error {
	if !session.AppService {
		return nil
	}
	return matrix.NewForbidden(
		"an application service may not use /sync; events it is interested in are pushed to its configured URL")
}
