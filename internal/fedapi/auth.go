package fedapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
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
	//
	// The reason is kept rather than replaced. "the request could not be authenticated" tells a peer
	// nothing it can act on — its key may be unreachable, its signature stale, its server name
	// misconfigured, its certificate untrusted — and every one of those is the peer's problem to fix
	// and this server's only clue when two operators are comparing logs about a federation link that
	// does not work. It is the peer's own signature under discussion, so nothing of this server's is
	// given away by saying what went wrong.
	reason := verificationReason(response.JSON)
	message := "the request could not be authenticated"
	if reason != "" {
		message = message + ": " + reason
	}
	body, ok := response.JSON.(map[string]any)
	if !ok || body["errcode"] == nil {
		body = map[string]any{"errcode": matrix.ErrForbidden, "error": message}
	}
	status := response.Code
	if status == 0 {
		status = http.StatusForbidden
	}
	// The URI is logged as this server understood it, not as the peer wrote it, because that is the
	// only way to see the pair disagree: the signature covers the method and the URI, so a peer that
	// signed "/rooms/%21a%3Ahs1" and sent "/rooms/!a:hs1" (or the reverse) produces exactly this
	// refusal, and nothing else in the log distinguishes it from a bad key.
	header := r.Header.Get("Authorization")
	slog.Warn("fedapi: refused a federation request that could not be authenticated",
		"method", r.Method, "uri", r.URL.RequestURI(), "status", status,
		"origin", authorizationOrigin(header),
		"authorization", redactedAuthorization(header), "reason", reason)
	// The signature is not logged at any level, including debug. `sig=` alongside the method, URI,
	// body hash, timestamp and nonce is a complete, replayable authorisation for that one request,
	// and "we only write credentials when someone turns debugging on" is the same policy as writing
	// them, because the level is not the thing that gets shipped to an aggregator or pasted into a
	// channel — the file is. Whoever has to see the bytes a peer sent captures the wire, not the log.
	httpx.RespondJSON(w, status, body)
	return nil
}

// redactedAuthorization removes the signature from an `X-Matrix` Authorization header, leaving the
// parts that make a refusal diagnosable: the claiming origin, the key it says it signed with, and the
// nonce. Those identify the peer and the moment; the signature only authorises it.
func redactedAuthorization(header string) string {
	if header == "" {
		return ""
	}
	const marker = `sig="`
	start := strings.Index(header, marker)
	if start < 0 {
		// Nothing that looks like a signature: pass it through, because inventing a redaction for an
		// unrecognised shape would hide the very malformation being investigated.
		return header
	}
	end := strings.Index(header[start+len(marker):], `"`)
	if end < 0 {
		return header[:start] + `sig="<redacted>"`
	}
	return header[:start+len(marker)] + "<redacted>" + header[start+len(marker)+end:]
}

// authorizationOrigin pulls the claimed origin server name out of an Authorization header
//
//	Origin origin="hs1:8448", ...
//
// on a request whose signature never verified, which is the only time the claim is worth recording:
// after that the request is anonymous, and "somebody who claimed to be hs1" is the most that can be
// said about it. It is reported as a claim and never believed.
// verificationReason extracts the sentence a signature check gave up with.
//
// The library answers failures with a handful of shapes rather than one: a bare string, a struct with a
// "message" field (which is what util.MessageResponse builds, an anonymous struct, so a type assertion
// for a map or a string misses it), or an object already in Matrix error form. Marshalling and reading
// it back covers all three without guessing which one a given code path returns, and the sentence is
// worth the round trip: without it this server knows only that a peer is not who it says, not whether
// the peer's key was unreachable, its certificate untrusted or its server name misconfigured — and it is
// the peer that has to fix whichever of those it was.
func verificationReason(body any) string {
	if body == nil {
		return ""
	}
	if text, ok := body.(string); ok {
		return text
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	var detail struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return ""
	}
	if detail.Message != "" {
		return detail.Message
	}
	return detail.Error
}

func authorizationOrigin(header string) string {
	const key = `origin="`
	start := strings.Index(header, key)
	if start < 0 {
		return "(unnamed)"
	}
	rest := header[start+len(key):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "(unnamed)"
	}
	return rest[:end]
}
