// Package todevice queues the messages that go to a device rather than to a room.
//
// To-device messages exist for one reason: end-to-end encryption. A session key has to reach one
// person's laptop and nobody else in the room, including the server. So the message never becomes an
// event, is never visible in a timeline, and is delivered only to the device it names.
//
// Delivery here is one-way: the server remembers how far each device has been served and does not
// send the same message twice. The specification instead lets the client's token ask for a
// redelivery, which this server cannot express without turning its pagination tokens into something
// /messages would have to learn to parse; see the note on [store.Device.ToDevicePos]. The property
// that matters for correctness — a device never receives the same key twice, and never receives a
// message out of order — holds either way.
package todevice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// MaxQueuedPerDevice bounds what one sync will hand to a device.
const MaxQueuedPerDevice = 100

// maxMessageIDLength bounds the idempotence identifier this server is willing to remember. The
// specification caps it at 32 codepoints; the column is wider so that a peer exceeding the spec is
// visible in the log rather than silently reinterpreted.
const maxMessageIDLength = 64

// Forwarder is how a message addressed to somebody else's device leaves this server.
//
// It is a queue write, not a send, and that distinction is the whole point: a Megolm session key that
// is dropped because a peer was briefly unreachable is a message that can never be read, so the
// intent has to outlive the attempt.
type Forwarder interface {
	// ForwardToDevice queues one destination server's share of a to-device message, carrying
	// `messageID` as its idempotence identifier.
	ForwardToDevice(ctx context.Context, destination, messageID, eventType, sender string,
		messages map[string]map[string]json.RawMessage) error
}

// Service is the to-device queue.
type Service struct {
	store      *store.Store
	notifier   *notifier.Notifier
	serverName string
	// forward is how remote recipients are reached. Nil in a deployment that does not federate,
	// where a message for another server's device has nowhere to go.
	forward Forwarder
	// nowMS is the clock the idempotence memory is written with.
	nowMS func() int64
}

// Options configures a [Service].
type Options struct {
	// ServerName decides which recipients this server can reach at all.
	ServerName string
}

// New builds the to-device queue.
func New(st *store.Store, n *notifier.Notifier, opts Options) *Service {
	return &Service{store: st, notifier: n, serverName: opts.ServerName, nowMS: time.Now().UnixMilli}
}

// WithForwarder installs the transport that reaches another server's devices, and returns the service
// for chaining.
func (s *Service) WithForwarder(forward Forwarder) *Service {
	s.forward = forward
	return s
}

// Send queues messages for a set of devices.
//
// The body is `user ID -> device ID -> content`. A device ID of `*` means "every device this user
// has", which is what a well-behaved sender uses when it has not kept a device list; the server is
// the only party that knows which devices still exist, so expanding the star here is the sender's
// mistake turned into the server's work rather than lost keys.
//
// A recipient this server does not host is skipped rather than failing the whole call: a key share
// addressed to somebody on another homeserver is normal (that is how a room with visitors works), and
// refusing to encrypt for them because one of their devices is remote would break the sender's room.
func (s *Service) Send(ctx context.Context, sender string, txnID string, eventType string,
	messages map[string]map[string]json.RawMessage) error {
	if strings.TrimSpace(eventType) == "" {
		return matrix.NewMissingParam("eventType")
	}
	queued := 0
	remote := map[string]map[string]map[string]json.RawMessage{}
	for userID, byDevice := range messages {
		localpart, domain, err := matrix.ParseUserID(userID, s.serverName)
		if err != nil {
			// A user ID this server cannot read is not an address, and inventing a destination for it
			// would be sending a key share to wherever a malformed string points.
			slog.Warn("to-device: dropping a message addressed to an unreadable user", "user_id", userID)
			continue
		}
		if string(domain) != s.serverName {
			// Somebody else's device. The message is not this server's to read and not its own to keep:
			// it goes to the server that owns that user ID, and only there.
			if s.forward == nil {
				slog.Warn("to-device: a message for a remote device has nowhere to go on this homeserver",
					"user_id", userID, "type", eventType)
				continue
			}
			if remote[string(domain)] == nil {
				remote[string(domain)] = map[string]map[string]json.RawMessage{}
			}
			remote[string(domain)][userID] = byDevice
			continue
		}
		for deviceID, content := range byDevice {
			targets := []string{deviceID}
			if deviceID == "*" {
				// Expanding the star is what makes this work for a user who added a device
				// after the sender last looked: the sender could not have known its ID.
				devices, err := s.store.Devices.List(ctx, localpart)
				if err != nil {
					return err
				}
				targets = targets[:0]
				for _, device := range devices {
					targets = append(targets, device.ID)
				}
			}
			for _, target := range targets {
				message := store.ToDeviceMessage{
					Type: eventType, Sender: sender, Recipient: localpart,
					RecipientDevice: target, Content: string(content),
				}
				position, err := s.store.ToDevice.Append(ctx, &message)
				if err != nil {
					return err
				}
				queued++
				_ = position
			}
		}
	}
	if queued > 0 && s.notifier != nil {
		// A waiting /sync has to wake for these: a key share that arrives in the queue but not
		// in the client's inbox is a message nobody can decrypt until the client next thinks to
		// look, which may be never.
		s.notifier.Publish()
		slog.Debug("to-device: queued messages", "sender", sender, "type", eventType, "count", queued)
	}
	for destination, byUser := range remote {
		if err := s.forward.ForwardToDevice(ctx, destination,
			messageID(sender, txnID, destination, eventType), eventType, sender, byUser); err != nil {
			// Said out loud rather than returned: the local devices in this same batch have already been
			// queued, and failing the whole call would leave the sender's own devices without the key as
			// well. A client whose key share did not cross the border shares it again; a server that
			// quietly loses one is the failure nobody can recover from.
			slog.Error("to-device: could not queue a message for another server",
				"destination", destination, "sender", sender, "type", eventType, "error", err)
		}
	}
	return nil
}

// messageID is the idempotence identifier for one destination's share of a client's send.
//
// Derived rather than invented: a client that retries the transaction has to produce the same
// identifier, because that is the only way the far end can tell a retry from a second key share. A
// random identifier would make this server the author of exactly the duplicate the identifier exists
// to prevent. The event type is part of it because two different kinds of message are two different
// messages even when a client reused the transaction ID across them.
func messageID(sender, txnID, destination, eventType string) string {
	sum := sha256.Sum256([]byte(sender + "\x00" + txnID + "\x00" + destination + "\x00" + eventType))
	return hex.EncodeToString(sum[:])[:28]
}

// IngestFromPeer takes a to-device message a peer delivered for a device this server hosts.
//
// Three things are checked, and each is a different mistake without it:
//
//   - the message must not have been taken before, keyed on the sending server and the identifier it
//     chose. A duplicate key share lets a client replay a message key it has already used.
//   - every recipient must live here. A peer may deliver messages for this server's users and for
//     nobody else's: accepting others would let any peer write into any device's inbox, and a device
//     trusts what it finds there.
//   - a device that does not exist is dropped rather than queued. The queue is drained per device, so
//     a row addressed to a device that never existed is a message that will outlive every reader and
//     nothing else about the room.
func (s *Service) IngestFromPeer(ctx context.Context, origin, messageIDValue, eventType, sender string,
	messages map[string]map[string]json.RawMessage) (int, error) {
	if strings.TrimSpace(messageIDValue) == "" {
		return 0, matrix.NewInvalidParam("a device message must carry a message_id")
	}
	if len(messageIDValue) > maxMessageIDLength {
		// The specification caps this at 32 codepoints. Refusing is kinder than truncating: a peer
		// whose identifiers collide once truncated would start losing messages for reasons nobody could
		// reconstruct from the log.
		return 0, matrix.NewInvalidParam("message_id must be at most %d characters", maxMessageIDLength)
	}
	if strings.TrimSpace(eventType) == "" {
		return 0, matrix.NewInvalidParam("a device message must carry a type")
	}
	fresh, err := s.store.ToDevice.Remember(ctx, origin, messageIDValue, s.now())
	if err != nil {
		return 0, err
	}
	if !fresh {
		// A retry of a message already delivered. Dropping it is the whole point of the identifier.
		slog.Debug("to-device: a peer repeated a message this server already took",
			"origin", origin, "message_id", messageIDValue)
		return 0, nil
	}

	queued := 0
	for userID, byDevice := range messages {
		localpart, domain, err := matrix.ParseUserID(userID, s.serverName)
		if err != nil {
			slog.Warn("to-device: a peer delivered a message for an unreadable user",
				"origin", origin, "user_id", userID)
			continue
		}
		if string(domain) != s.serverName {
			// Not ours, and never will be: this server has no authority to deliver to it and no way to
			// know whether it exists.
			slog.Warn("to-device: a peer delivered a message for a user this homeserver does not host",
				"origin", origin, "user_id", userID)
			continue
		}
		devices, err := s.store.Devices.List(ctx, localpart)
		if err != nil {
			return queued, err
		}
		known := make(map[string]bool, len(devices))
		for _, device := range devices {
			known[device.ID] = true
		}
		for deviceID, content := range byDevice {
			targets := []string{deviceID}
			if deviceID == "*" {
				// The sender is asking for every device this user has, and only this server knows what
				// that list is — including the laptop that was added after the sender last looked.
				targets = targets[:0]
				for _, device := range devices {
					targets = append(targets, device.ID)
				}
			}
			for _, target := range targets {
				if !known[target] {
					slog.Debug("to-device: dropping a message for a device this user does not have",
						"user_id", userID, "device_id", target)
					continue
				}
				if _, err := s.store.ToDevice.Append(ctx, &store.ToDeviceMessage{
					Type: eventType, Sender: sender, Recipient: localpart,
					RecipientDevice: target, Content: string(content),
				}); err != nil {
					return queued, err
				}
				queued++
			}
		}
	}
	if queued > 0 && s.notifier != nil {
		s.notifier.Publish()
		slog.Debug("to-device: queued messages delivered by a peer",
			"origin", origin, "type", eventType, "count", queued)
	}
	return queued, nil
}

// now is the clock the idempotence memory is stamped with.
func (s *Service) now() int64 {
	if s.nowMS == nil {
		return time.Now().UnixMilli()
	}
	return s.nowMS()
}

// Take renders a device's queued messages and advances its cursor.
//
// The cursor moves as part of reading, which is the point: to-device messages carry keys, and
// handing the same key to a device twice makes a client replay a message key it has already used.
func (s *Service) Take(ctx context.Context, localpart, deviceID string) ([]json.RawMessage, int64, error) {
	if deviceID == "" {
		// A token with no device cannot be given a device-addressed queue: there is no cursor to
		// advance, so every sync would re-deliver everything.
		return nil, 0, nil
	}
	cursor, err := s.store.Devices.Cursor(ctx, deviceID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.store.ToDevice.List(ctx, localpart, deviceID, cursor, MaxQueuedPerDevice)
	if err != nil {
		return nil, 0, err
	}
	events := make([]json.RawMessage, 0, len(rows))
	latest := cursor
	for _, row := range rows {
		event, err := json.Marshal(map[string]any{
			"type":    row.Type,
			"sender":  row.Sender,
			"content": json.RawMessage(row.Content),
		})
		if err != nil {
			slog.Warn("to-device: dropping an unreadable queued message", "id", row.ID, "error", err)
			continue
		}
		events = append(events, event)
		if row.StreamPos > latest {
			latest = row.StreamPos
		}
	}
	if latest > cursor {
		if err := s.store.Devices.SetCursor(ctx, deviceID, latest); err != nil {
			return events, latest, err
		}
	}
	return events, latest, nil
}

// Prune drops queued messages older than a position, which is what keeps the queue from holding
// every key share the server has ever relayed.
func (s *Service) Prune(ctx context.Context, below int64) (int64, error) {
	if below <= 0 {
		return 0, nil
	}
	removed, err := s.store.ToDevice.Prune(ctx, below)
	if err != nil {
		return 0, fmt.Errorf("pruning the to-device queue: %w", err)
	}
	return removed, nil
}
