package roomserver

import (
	"context"
	"encoding/json"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// RefreshMemberProfile pushes a profile change into the rooms the user is in, as new member
// events.
//
// A display name lives in two places at once: the account's profile, and the `m.room.member`
// events that carry it into each room so members can be named without a lookup per event. Matrix
// has no "profile changed" event, so the only way a room learns that somebody's face or name is
// different is for the server to write the member event again. Without this, every room keeps
// showing the old name to everyone who already synced it — which is most people, most of the time.
//
// A member event that disagrees with the *old* profile is left alone: that is a name the user chose
// for that room, and overwriting it with the new global name would destroy a deliberate choice.
// This is what makes "use a different name in this room" survive renaming yourself everywhere else.
func (s *Service) RefreshMemberProfile(ctx context.Context, localpart, previousDisplayName, previousAvatarURL string) error {
	userID := matrix.UserID(localpart, s.cfg.ServerName)
	memberships, err := s.store.Memberships.ListByUser(ctx, localpart)
	if err != nil {
		return err
	}
	user, err := s.store.Users.Get(ctx, localpart)
	if err != nil {
		return err
	}

	for _, membership := range memberships {
		if membership.Membership != matrix.MembershipJoin {
			continue
		}
		current, err := s.memberContent(ctx, membership.RoomID, userID)
		if err != nil {
			continue // a room whose member event cannot be read is not a room to rewrite
		}
		if keepsOwnName(current["displayname"], previousDisplayName) {
			continue
		}
		if keepsOwnAvatar(current["avatar_url"], previousAvatarURL) {
			continue
		}

		content := map[string]any{"membership": matrix.MembershipJoin}
		if user.DisplayName != "" {
			content["displayname"] = user.DisplayName
		}
		if user.AvatarURL != "" {
			content["avatar_url"] = user.AvatarURL
		}
		// A room the caller can no longer write to (a power-level change, a ban elsewhere) is
		// not a reason the whole profile update should fail: the profile changed, and every
		// other room still needs telling.
		if _, err := s.SendStateEvent(ctx, Sender{
			Localpart: localpart,
			UserID:    userID,
		}, membership.RoomID, matrix.EventTypeMember, userID, mustJSON(content)); err != nil {
			continue
		}
	}
	return nil
}

// memberContent reads a member event's content as a loose object.
func (s *Service) memberContent(ctx context.Context, roomID, userID string) (map[string]any, error) {
	entry, err := s.store.State.Get(ctx, roomID, matrix.EventTypeMember, userID)
	if err != nil {
		return nil, err
	}
	row, err := s.store.Events.ByEventID(ctx, entry.EventID)
	if err != nil {
		return nil, err
	}
	var document struct {
		Content map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(row.JSON), &document); err != nil {
		return nil, err
	}
	if document.Content == nil {
		document.Content = map[string]any{}
	}
	return document.Content, nil
}

// keepsOwnName reports whether a member event carries a display name the user set for this room
// rather than a copy of the profile name they have just replaced.
func keepsOwnName(current any, previousProfileName string) bool {
	name, ok := current.(string)
	if !ok || name == "" {
		return false
	}
	return name != previousProfileName
}

// keepsOwnAvatar is the same rule for avatars.
func keepsOwnAvatar(current any, previousProfileAvatar string) bool {
	url, ok := current.(string)
	if !ok || url == "" {
		return false
	}
	return url != previousProfileAvatar
}

// mustJSON encodes a value that cannot fail to encode, because it is a map of strings.
func mustJSON(value map[string]any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"membership":"join"}`)
	}
	return encoded
}
