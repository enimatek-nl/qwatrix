package fedapi

// Taking delivery of a message addressed to one of this server's devices.
//
// This is the receiving half of the path an end-to-end key share takes between homeservers. The
// sending half queues the message and retries it; this half decides what arrives.
//
// Two asymmetries are worth naming, because both invite the wrong implementation. The peer is
// believed about *who sent* the message and disbelieved about *who it is for*: a server may deliver
// messages for its own users and for nobody else's, or any peer could write into any device's inbox
// on this server, and a device's inbox is precisely where its owner's client trusts what it finds. And
// because ephemeral events are unacknowledged, this server cannot tell the peer that delivery failed —
// the log line is the only record there is, which is why a refusal is written as a warning naming the
// sender rather than as a debug line nobody reads at 3am.

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// toDeviceMessage is the content of an `m.direct_to_device` ephemeral event.
type toDeviceMessage struct {
	// MessageID is the sending server's idempotence identifier: the same key share arriving twice is
	// one message, and this is how the destination knows.
	MessageID string `json:"message_id"`
	// Type is the event type the recipient sees, e.g. m.room_key.
	Type string `json:"type"`
	// Sender is who the message claims to come from. Stored, not trusted: the recipient's client
	// decides what a claimed identity is worth, and the payload it carries is signed by that device.
	Sender string `json:"sender"`
	// Messages is `user ID -> device ID -> content`, where a device ID of "*" means every device that
	// user has — a question only the server that holds them can answer.
	Messages map[string]map[string]json.RawMessage `json:"messages"`
}

// applyToDevice delivers one device message.
func (h *Handler) applyToDevice(ctx context.Context, origin spec.ServerName, content spec.RawJSON) {
	if len(content) == 0 {
		return
	}
	if h.opts.ToDevice == nil {
		// This server has nowhere to put the message. Saying so in the log is the whole of the
		// evidence: an ephemeral event gets no answer, so the peer will believe it sent one.
		slog.Warn("fedapi: a device message arrived for a homeserver with no device queue",
			"origin", origin)
		return
	}
	var message toDeviceMessage
	if err := json.Unmarshal(content, &message); err != nil {
		slog.Warn("fedapi: a device message could not be read", "origin", origin, "error", err)
		return
	}
	if len(message.Messages) == 0 {
		slog.Debug("fedapi: a device message named no recipients", "origin", origin)
		return
	}
	if message.Sender != "" {
		// The sender is not required to be a user of the sending server — a key share can be relayed —
		// but it does have to be a user ID, or the recipient is being told a story about nobody.
		if _, _, err := matrix.ParseUserID(message.Sender, string(origin)); err != nil {
			slog.Warn("fedapi: a device message named an unreadable sender",
				"origin", origin, "sender", message.Sender)
			return
		}
	}
	queued, err := h.opts.ToDevice.IngestFromPeer(ctx, string(origin), message.MessageID,
		message.Type, message.Sender, message.Messages)
	if err != nil {
		slog.Warn("fedapi: could not take delivery of a device message",
			"origin", origin, "sender", message.Sender, "error", err)
		return
	}
	if queued == 0 {
		// Not a failure — a retry, a message for a device that has since been deleted, or one for a
		// user this server does not host. All of them are worth one line, because "the keys never
		// arrived" is the question this is the answer to.
		slog.Debug("fedapi: a device message was delivered and dropped",
			"origin", origin, "message_id", message.MessageID)
	}
}
