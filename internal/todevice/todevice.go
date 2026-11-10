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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// MaxQueuedPerDevice bounds what one sync will hand to a device.
const MaxQueuedPerDevice = 100

// Service is the to-device queue.
type Service struct {
	store      *store.Store
	notifier   *notifier.Notifier
	serverName string
}

// Options configures a [Service].
type Options struct {
	// ServerName decides which recipients this server can reach at all.
	ServerName string
}

// New builds the to-device queue.
func New(st *store.Store, n *notifier.Notifier, opts Options) *Service {
	return &Service{store: st, notifier: n, serverName: opts.ServerName}
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
func (s *Service) Send(ctx context.Context, sender string, eventType string, messages map[string]map[string]json.RawMessage) error {
	if strings.TrimSpace(eventType) == "" {
		return matrix.NewMissingParam("eventType")
	}
	queued := 0
	for userID, byDevice := range messages {
		localpart, domain, err := matrix.ParseUserID(userID, s.serverName)
		if err != nil || string(domain) != s.serverName {
			// Another server's user. Dropping the message is honest in a way that pretending to
			// deliver it is not: federation would carry it, and until it does, the sender's
			// client should not believe the key reached that device.
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
	return nil
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
