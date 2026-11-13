package appservice

// The authentication half: turning an application service's token into a session.
//
// An application service authenticates with a secret from its registration file, not with a session
// this server issued. That inverts the usual question. A normal token answers "which device is
// asking?"; this one answers "which program is asking, and whom does it want to speak for?" — and the
// answer to the second question is a user ID the service names itself, in a query parameter, on every
// request.
//
// So the checks here are the entire boundary of that inversion, and they are all cheap: the token must
// be one this server was told about; the user it names must be a local user of this server; that user
// must be inside a namespace the registration granted; and the account is then created if it does not
// exist, because bridge users are virtual by design.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/accounts"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// Accounts is the part of the account service this authenticator needs: an account for the user the
// service is acting as, created when it does not exist yet.
type Accounts interface {
	EnsureVirtualUser(ctx context.Context, localpart, deviceID string) (*accounts.Account, error)
}

// Authenticator resolves application-service tokens into sessions. It satisfies the account service's
// external-token seam, so it is consulted only for tokens this server never issued.
type Authenticator struct {
	registry   *Registry
	accounts   Accounts
	serverName string
	// log is where a refusal is recorded. A service that cannot act as a user it believes it owns
	// has a configuration mistake somewhere, and the operator needs to see which user the
	// namespaces did not cover.
	log *slog.Logger
}

// NewAuthenticator wires the authenticator to its registry and account service.
func NewAuthenticator(registry *Registry, accounts Accounts, serverName string, log *slog.Logger) *Authenticator {
	if log == nil {
		log = slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	return &Authenticator{registry: registry, accounts: accounts, serverName: serverName, log: log}
}

// discardWriter keeps a nil logger from writing anywhere; the interface wants a writer and a
// homeserver that has not been given one should not be printing to stderr on a request path.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// deviceIDFor is the device an application service's events are attributed to, one per service rather
// than one per request: a bridge is a program, and a program does not sign out.
func deviceIDFor(registration *Registration) string {
	return "as-" + registration.ID
}

// ResolveExternal answers the external-token question: nil session and nil error means "this token is
// not mine", which leaves the standard M_UNKNOWN_TOKEN answer standing for everybody else.
func (a *Authenticator) ResolveExternal(r *http.Request, token string) (*httpx.Session, error) {
	registration, ok := a.registry.LookupByToken(token)
	if !ok {
		return nil, nil
	}
	// The service speaks for its own sender user unless it names another one. Its own sender needs no
	// namespace entry: the registration names that user explicitly, and a service that could not be
	// itself would be unable to send anything at all.
	localpart := registration.SenderLocalpart
	if requested := strings.TrimSpace(r.URL.Query().Get("user_id")); requested != "" {
		parsed, domain, err := matrix.ParseUserID(requested, a.serverName)
		if err != nil {
			return nil, matrix.NewInvalidParam("user_id %q is not a user of this homeserver", requested)
		}
		if !strings.EqualFold(domain, a.serverName) {
			// A user of another server is not this server's to speak for: it would sign an event for an
			// identity it cannot vouch for, and no peer would believe the result.
			//
			// The domain is checked rather than discarded. Rebuilding the ID from the local part alone
			// would turn "@someone:evil.example" into "@someone:this.server", which is a different person
			// and one this service may well have been granted — an identity swap dressed as a typo.
			return nil, matrix.NewInvalidParam("user_id %q belongs to %s, not to this homeserver", requested, domain)
		}
		localpart = parsed
		userID := matrix.UserID(localpart, a.serverName)
		if !registration.AllowsUser(userID) {
			a.log.Warn("application service asked for a user outside its namespaces",
				"as_id", registration.ID, "user_id", userID)
			return nil, matrix.NewForbidden("this application service is not authorised to act as %s", userID)
		}
	}

	account, err := a.accounts.EnsureVirtualUser(r.Context(), localpart, deviceIDFor(registration))
	if err != nil {
		return nil, err
	}
	return &httpx.Session{
		Localpart:    account.Localpart,
		UserID:       account.UserID,
		DeviceID:     account.DeviceID,
		AccessToken:  token,
		AppService:   true,
		AppServiceID: registration.ID,
	}, nil
}
