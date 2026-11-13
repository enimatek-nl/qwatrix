package fedapi

// The two federation queries a peer makes about this server's users' *encryption* keys, as opposed to
// the device list served by devices.go: the key documents themselves, and the one-time keys that start
// an Olm session.
//
// They are the reason an encrypted conversation between two homeservers can begin at all. A client
// that wants to send a Megolm key to a device on another server first needs that device's identity keys
// and then a one-time key to encrypt the first message to; without these two endpoints it has the first
// — fetched through `/user/devices/{userID}` — and never gets the second, so the session is never
// established and the message is never sent.
//
// Both are served under one rule, and it is the rule every other key path here runs on: this server
// answers for the users whose user IDs name it, and for nobody else's. A device's keys are believed
// because they arrived from the domain in its user ID, so answering for another domain would let any
// peer mint an identity for any user; and a one-time key is spent by being handed over, so serving one
// for somebody else's device would be spending a secret this server does not own.

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// handlePeerKeyQuery answers `POST /_matrix/federation/v1/user/keys/query`.
//
// The body names users and, optionally, the devices of theirs being asked about; an empty list means
// every device that user has.
func (h *Handler) handlePeerKeyQuery(w http.ResponseWriter, r *http.Request) {
	if h.opts.PeerKeys == nil {
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"device_keys": map[string]any{},
			"failures":    unreachablePeerFailure(string(h.opts.ServerName)),
		})
		return
	}
	fedRequest := h.authenticatePeer(w, r)
	if fedRequest == nil {
		return
	}
	var request struct {
		DeviceKeys map[string][]string `json:"device_keys"`
	}
	if err := json.Unmarshal(fedRequest.Content(), &request); err != nil {
		httpx.RespondError(w, r, matrix.NewNotJSON("a key query must carry a device_keys object"))
		return
	}
	documents, err := h.opts.PeerKeys.PeerDeviceKeys(r.Context(), request.DeviceKeys)
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]any{
		"device_keys": nonNilDocuments(documents),
		"failures":    map[string]any{},
	})
}

// handlePeerKeyClaim answers `POST /_matrix/federation/v1/user/keys/claim`, which is where an Olm
// session with a device on another server actually begins.
//
// Spending is not undone. A peer that claims a key and then vanishes has used it, which is what the
// protocol expects and why a device tops its stock up: the alternative — reserving a key in case the
// asker comes back — would let a peer freeze this server's devices' key pools by asking and not
// finishing.
func (h *Handler) handlePeerKeyClaim(w http.ResponseWriter, r *http.Request) {
	if h.opts.PeerKeys == nil {
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"one_time_keys": map[string]any{},
			"failures":      unreachablePeerFailure(string(h.opts.ServerName)),
		})
		return
	}
	fedRequest := h.authenticatePeer(w, r)
	if fedRequest == nil {
		return
	}
	var request struct {
		OneTimeKeys map[string]map[string]string `json:"one_time_keys"`
	}
	if err := json.Unmarshal(fedRequest.Content(), &request); err != nil {
		httpx.RespondError(w, r, matrix.NewNotJSON("a key claim must carry a one_time_keys object"))
		return
	}
	claimed, err := h.opts.PeerKeys.PeerClaimKeys(r.Context(), request.OneTimeKeys)
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	slog.Debug("fedapi: claimed one-time keys for a peer", "users", len(claimed))
	httpx.RespondJSON(w, http.StatusOK, map[string]any{
		"one_time_keys": nonNilDocuments(claimed),
		"failures":      map[string]any{},
	})
}

// nonNilDocuments keeps an empty answer an object rather than null, which is what every client reading
// these maps expects.
func nonNilDocuments(documents map[string]map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	if documents == nil {
		return map[string]map[string]json.RawMessage{}
	}
	return documents
}

// unreachablePeerFailure is the `failures` map for a question this server cannot answer at all. The
// specification keys failures by server name, and the name the asking peer expects to find is the one
// it asked — that is this server.
func unreachablePeerFailure(serverName string) map[string]any {
	return map[string]any{
		serverName: map[string]any{
			"error": "this homeserver does not serve device keys over federation",
		},
	}
}
