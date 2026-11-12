package events

import (
	"encoding/json"
	"fmt"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/tidwall/gjson"
)

// CoSignEvent adds this server's signature to an event that the specification expects more than one
// server to sign, and returns the event with both signatures on it.
//
// Today that means one thing: an invite. The inviter's signature proves that the invitation was sent;
// this server's signature says that the user the invitation is about lives here and this server is
// willing to hand it over. The invited user acts on that event — joins from it, decrypts with it —
// and the only party between them and an event about them is the server they trust with their account.
// So a receiving server that finds an invite with only the inviter's signature refuses it, and a server
// that never signs cannot invite anybody who lives elsewhere.
//
// What gets signed is the same document a verifier will check: the room version's own reference
// redaction of the event. Signing the whole event instead would produce a signature over bytes nobody
// verifies — the redaction drops fields (`hashes`, `unsigned`, and in some room versions `depth`) that
// a signature is not meant to cover, and the result is an event that looks co-signed and is not.
//
// Adding a signature does not change the event's identity. Event IDs are computed over the redaction,
// which excludes signatures, so the inviter keeps the event ID it asked about and every server in the
// room agrees on it.
func (s *Service) CoSignEvent(roomVersion string, eventJSON string) (json.RawMessage, error) {
	impl, err := s.RoomVersion(roomVersion)
	if err != nil {
		return nil, err
	}
	reference, err := impl.RedactEventJSON([]byte(eventJSON))
	if err != nil {
		return nil, fmt.Errorf("co-signing: redacting the event for signing: %w", err)
	}
	signedReference, err := s.identity.SignJSON(reference)
	if err != nil {
		return nil, fmt.Errorf("co-signing: %w", err)
	}
	// `SignJSON` returns the signed document with the signatures it collected attached; the only part
	// belonging to the event is this server's own signature set, since the rest of the reference
	// document is a redaction of it rather than the event itself.
	ours := gjson.GetBytes(signedReference, "signatures."+gjson.Escape(string(s.identity.ServerName)))
	if !ours.Exists() {
		return nil, fmt.Errorf("co-signing: the signed document carries no signature for %q",
			s.identity.ServerName)
	}

	var event map[string]json.RawMessage
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
		return nil, fmt.Errorf("co-signing: the event is not a JSON object: %w", err)
	}
	signatures := map[string]json.RawMessage{}
	if existing, ok := event["signatures"]; ok {
		if err := json.Unmarshal(existing, &signatures); err != nil {
			return nil, fmt.Errorf("co-signing: the event's signatures are not a JSON object: %w", err)
		}
	}
	if already, ok := signatures[string(s.identity.ServerName)]; ok && len(already) > 0 {
		// This server has already signed it. Signing again would replace the key set with a
		// different key ID for no reason, and a verifier that had cached the first signature would
		// find the event changed under it.
		return json.RawMessage(eventJSON), nil
	}
	signatures[string(s.identity.ServerName)] = json.RawMessage(ours.Raw)
	event["signatures"], err = json.Marshal(signatures)
	if err != nil {
		return nil, fmt.Errorf("co-signing: encoding the signatures: %w", err)
	}
	coSigned, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("co-signing: encoding the event: %w", err)
	}
	if !json.Valid(coSigned) {
		return nil, matrix.NewUnknown("co-signing produced an event that is not valid JSON")
	}
	return json.RawMessage(coSigned), nil
}

// IsInvite reports whether an event JSON is an `m.room.member` event inviting a user.
//
// The membership is read out of the raw document rather than a parsed event because a caller on this
// path is deciding whether to sign, and must be able to do that about an event whose room version it
// has not settled yet.
func IsInvite(eventJSON []byte) bool {
	return gjson.GetBytes(eventJSON, "type").String() == matrix.EventTypeMember &&
		gjson.GetBytes(eventJSON, "content.membership").String() == string(matrix.MembershipInvite)
}
