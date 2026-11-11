package fedapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// Device lists over federation: how a homeserver learns which devices a user it does not host has, and
// how it answers when somebody asks the same question of it.
//
// This is the load-bearing part of cross-server encryption. A client that cannot enumerate a remote
// user's devices cannot start an Olm session with them, so it cannot send a Megolm key, so a message
// it "sends" to a shared room is unreadable by design. The failure is silent by default — the message
// goes out, the timeline shows it, and it stays a lock with no key in it.

// DeviceListSource answers, from this server's own data, the question a peer asks about a user who
// lives here.
type DeviceListSource interface {
	// UserDevices returns the federation answer for a user ID, or a matrix error if the user is not
	// this server's to describe.
	UserDevices(ctx context.Context, userID string) (map[string]any, error)
}

// handleUserDevices answers `GET /_matrix/federation/v1/user/devices/{userID}`.
//
// The answer is a statement about who may encrypt to a user, so it is signed and so it is limited to
// this server's own users: a homeserver that would describe `@alice:other.example` is a homeserver
// where anybody can publish a device for anybody.
func (h *Handler) handleUserDevices(w http.ResponseWriter, r *http.Request) {
	if h.opts.Devices == nil {
		// A refusal, not an empty list: "this server publishes no device lists" and "this user has no
		// devices" are different facts, and the second one tells the peer to stop encrypting to them.
		httpx.RespondError(w, r, matrix.NewNotFound("This homeserver does not publish device lists"))
		return
	}
	if h.authenticatePeer(w, r) == nil {
		return
	}
	userID := r.PathValue("userID")
	answer, err := h.opts.Devices.UserDevices(r.Context(), userID)
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, answer)
}

// RemoteDevices asks other homeservers for the device lists of their own users. It satisfies
// keyserver.RemoteKeySource without importing it, which is how every outbound seam here is wired.
type RemoteDevices struct {
	peer *Peer
}

// NewRemoteDevices builds the source on an outbound federation client. A nil peer yields a source that
// fails every question, which the key server turns into "their devices are unknown to us".
func NewRemoteDevices(peer *Peer) *RemoteDevices { return &RemoteDevices{peer: peer} }

// deviceListResponse is the peer's answer, read loosely on purpose.
//
// Two shapes are in the wild. The specification calls the array `device_lists`; the Go libraries that
// predate it read `devices`. Each entry is accepted as raw bytes rather than parsed into a struct
// because a device key is a signed document — a struct that dropped `signatures` or re-ordered `keys`
// would produce a key that fails to verify on the client that finally receives it, and the failure
// would look like a stranger refusing to talk to a friend.
type deviceListResponse struct {
	UserID      string            `json:"user_id"`
	DeviceLists []json.RawMessage `json:"device_lists"`
	Devices     []json.RawMessage `json:"devices"`
}

// UserDevices asks `serverName` about `userID`.
//
// The server is named by the user ID's own domain, three times over: here, by the caller, and in the
// signature the peer computes over the request. That is what stops one homeserver from being a
// mouthpiece for another, and it is why a failed question is answered with an empty list rather than a
// guess.
func (d *RemoteDevices) UserDevices(ctx context.Context, serverName, userID string) ([]json.RawMessage, error) {
	if d == nil || d.peer == nil {
		return nil, matrix.NewUnknown("this homeserver is not federating")
	}
	if _, domain, err := matrix.ParseUserID(userID, serverName); err != nil {
		return nil, matrix.NewInvalidParam("%q is not a valid user ID", userID)
	} else if string(spec.ServerName(domain)) != serverName {
		// Asked to look a user up "on" a server they do not live on. Refusing is the only answer that
		// cannot be made into a lie: the peer is entitled to describe its own users and nobody else's,
		// so an answer from it would be worthless even if it were a good one.
		return nil, matrix.NewInvalidParam("user %q does not live on %q", userID, serverName)
	}

	var answer deviceListResponse
	path := "/_matrix/federation/v1/user/devices/" + url.PathEscape(userID)
	if err := d.peer.GetSignedJSON(ctx, spec.ServerName(serverName), path, &answer); err != nil {
		return nil, err
	}

	documents := answer.DeviceLists
	if len(documents) == 0 {
		documents = answer.Devices
	}
	out := make([]json.RawMessage, 0, len(documents))
	for _, document := range documents {
		if !json.Valid(document) {
			continue
		}
		out = append(out, document)
	}
	slog.Debug("fedapi: asked a peer for a user's devices",
		"server", serverName, "user_id", userID, "devices", len(out))
	return out, nil
}
