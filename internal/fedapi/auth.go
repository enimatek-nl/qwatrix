package fedapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	fclient "github.com/matrix-org/gomatrixserverlib/fclient"
)

// authenticatePeer verifies a peer's X-Matrix signature and answers the request when it fails.
//
// Federation has no sessions and no tokens. A peer proves who it is by signing the method, the URI,
// its own server name, this server's name and the request body with a key it publishes at a well-known
// path — and the key used is fetched from *that peer*, which is what makes the proof a proof: an
// imposter would have to have the sender's signing key.
//
// Two consequences worth stating because they shape every handler here:
//
// The signature covers the URI, so the request cannot be replayed against another endpoint, and it
// covers the body, so the body must be read from the parsed request — the verifier consumes it.
//
// A nil key ring means this deployment has no way to attribute a request to anybody. Handlers refuse
// rather than proceed: an event admitted without knowing who sent it is an event nobody can account
// for later.
//
// It returns nil after writing the refusal, so callers answer once and only once.
func (h *Handler) authenticatePeer(w http.ResponseWriter, r *http.Request) *fclient.FederationRequest {
	if h.keyRing == nil {
		httpx.RespondError(w, r, matrix.NewUnknown("this server cannot verify federation signatures"))
		return nil
	}
	fedRequest, response := fclient.VerifyHTTPRequest(r, time.Now(), h.opts.ServerName, h.isLocalServerName, h.keyRing)
	if response.Is2xx() {
		return fedRequest
	}

	// The verifier answers with its own response type. The body is forwarded when it already looks
	// like a Matrix error and wrapped when it does not, so a peer always gets the shape it expects
	// from this server.
	body, ok := response.JSON.(map[string]any)
	if !ok || body["errcode"] == nil {
		message := "the request could not be authenticated"
		if raw, isString := response.JSON.(string); isString && raw != "" {
			message = raw
		}
		body = map[string]any{"errcode": matrix.ErrForbidden, "error": message}
	}
	status := response.Code
	if status == 0 {
		status = http.StatusForbidden
	}
	slog.Debug("fedapi: refused an unauthenticated federation request",
		"method", r.Method, "path", r.URL.Path, "status", status)
	httpx.RespondJSON(w, status, body)
	return nil
}
