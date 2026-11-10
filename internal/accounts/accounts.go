// Package accounts owns identities: user records, passwords, devices and access tokens.
//
// It is also the implementation of [httpx.SessionResolver], which makes it the thing every
// authenticated request passes through. Nothing about HTTP reaches this package; handlers
// call these methods and translate the results.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Service is the accounts service.
type Service struct {
	store *store.Store
	cfg   *config.Config
	// nowMS is a seam for tests that need deterministic timestamps.
	nowMS func() int64
	// onProfile is where an account's public name is published when it is created or changed.
	// It is a seam rather than a dependency: the user directory is a derived index, and an
	// account service that could not mint an account without one would let an indexing failure
	// take registration down with it.
	onProfile ProfileObserver
	// observeDevice is where a *new* device is announced, so that the people who encrypt for
	// this user learn it exists. Like the profile observer it is a seam, and a failure to
	// announce must never turn into a failed login.
	observeDevice func(userID string)
}

// DeviceObserver is notified when an account gains a device.
type DeviceObserver func(userID string)

// WithDeviceObserver installs the device observer and returns the service for chaining.
func (s *Service) WithDeviceObserver(observer DeviceObserver) *Service {
	s.observeDevice = observer
	return s
}

// ProfileObserver is notified when an account's profile name changes.
type ProfileObserver func(ctx context.Context, localpart, displayName, avatarURL string)

// WithProfileObserver installs the observer and returns the service for chaining.
func (s *Service) WithProfileObserver(observer ProfileObserver) *Service {
	s.onProfile = observer
	return s
}

// publishProfile tells the observer about an account's current profile. Nil-safe, and called
// after the account row is committed so an observer can read the profile it is told about.
func (s *Service) publishProfile(ctx context.Context, localpart string) {
	if s.onProfile == nil {
		return
	}
	user, err := s.store.Users.Get(ctx, localpart)
	if err != nil {
		return
	}
	s.onProfile(ctx, localpart, user.DisplayName, user.AvatarURL)
}

// New builds the service.
func New(st *store.Store, cfg *config.Config) *Service {
	return &Service{store: st, cfg: cfg, nowMS: func() int64 { return time.Now().UnixMilli() }}
}

// ServerName is the identity accounts are minted against.
func (s *Service) ServerName() string { return s.cfg.ServerName }

// Account is the result of a successful registration or login: an authenticated device.
type Account struct {
	Localpart   string
	UserID      string
	DeviceID    string
	AccessToken string
	Admin       bool
	DisplayName string
	AvatarURL   string
	// NewlyCreated distinguishes "this registration made an account" from "this login
	// reused one", which is what a caller logging the event needs to know.
	NewlyCreated bool
}

// RegisterParams describes the account to create.
type RegisterParams struct {
	// Localpart is the requested user ID local part. Empty asks the server to choose.
	Localpart string
	// Password is optional; an account with no password cannot use m.login.password.
	Password string
	// DeviceID is the client's requested device identifier; empty asks the server.
	DeviceID string
	// Admin marks the account as an administrator (shared-secret registration only).
	Admin bool
	// DisplayName and AvatarURL seed the profile.
	DisplayName string
	AvatarURL   string
}

// ErrPasswordTooShort is returned for passwords below the configured minimum.
var ErrPasswordTooShort = errors.New("accounts: password too short")

// Register creates a user, a device and an access token in one transaction.
//
// The specification's errors come back as *matrix.APIError so a handler can pass them
// straight to the response writer: M_USER_IN_USE for a taken local part,
// M_INVALID_USERNAME for something the spec does not allow.
// CheckLocalpart reports whether a local part is one this server would create an account
// for, without touching the database. The registration endpoint needs to answer this before
// the user-interactive exchange runs, so that a client is told about a bad username instead
// of being asked to authenticate.
func (s *Service) CheckLocalpart(localpart string) error {
	if localpart == "" {
		return nil
	}
	if !matrix.ValidLocalpart(localpart) {
		return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidUsername,
			"Localpart may only contain lowercase letters, digits and the symbols ._=-/+, and must not be empty or start with '_'")
	}
	return nil
}

// LocalpartTaken reports whether an account already exists for a local part. A forgotten or
// deactivated account still counts: the user ID stays reserved, because other homeservers
// may hold history that attributes events to it.
func (s *Service) LocalpartTaken(ctx context.Context, localpart string) (bool, error) {
	if localpart == "" {
		return false, nil
	}
	_, err := s.store.Users.Get(ctx, localpart)
	if err == nil {
		return true, nil
	}
	if store.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *Service) Register(ctx context.Context, params RegisterParams) (*Account, error) {
	localpart := params.Localpart
	if localpart == "" {
		localpart = strings.ToLower(matrix.RandomID(8))
	}
	if !matrix.ValidLocalpart(localpart) {
		return nil, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidUsername,
			"Localpart may only contain lowercase letters, digits and the symbols ._=-/+, and must not be empty or start with '_'")
	}

	userID := matrix.UserID(localpart, s.cfg.ServerName)
	hash, err := hashPassword(params.Password)
	if err != nil {
		return nil, fmt.Errorf("hashing password for %q: %w", userID, err)
	}

	deviceID := params.DeviceID
	if deviceID == "" {
		deviceID = matrix.DeviceID()
	}
	// The device identifier is client-supplied, and a client may send one that is already
	// in use by another account. Asking for a fresh one is the least surprising answer:
	// the device ID is still opaque to the client, and no account loses its sessions.
	if existing, err := s.store.Devices.Get(ctx, deviceID); err == nil && existing.Localpart != localpart {
		deviceID = matrix.DeviceID()
	}

	token := matrix.AccessToken()
	now := s.nowMS()

	err = s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		user := &store.User{
			Localpart:    localpart,
			UserID:       userID,
			PasswordHash: hash,
			DisplayName:  params.DisplayName,
			AvatarURL:    params.AvatarURL,
			Admin:        params.Admin,
			CreatedMS:    now,
			UserVersion:  1,
		}
		if err := s.store.Users.Create(ctx, user); err != nil {
			if store.IsAlreadyExists(err) {
				return matrix.NewAPIError(http.StatusBadRequest, matrix.ErrUserInUse,
					"A user with this ID already exists")
			}
			return fmt.Errorf("creating user %q: %w", userID, err)
		}
		device := &store.Device{
			ID:         deviceID,
			Localpart:  localpart,
			CreatedMS:  now,
			LastSeenMS: now,
		}
		if err := s.store.Devices.Create(ctx, device); err != nil {
			return fmt.Errorf("creating device for %q: %w", userID, err)
		}
		accessToken := &store.AccessToken{
			Token:     token,
			DeviceID:  deviceID,
			Localpart: localpart,
			CreatedMS: now,
		}
		if err := s.store.Tokens.Create(ctx, accessToken); err != nil {
			return fmt.Errorf("issuing access token for %q: %w", userID, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// The directory learns about the account at creation, not at its first rename: a user who
	// never sets a display name is still somebody others may want to find by their ID, and an
	// index that only contains people who configured something is not a directory.
	s.publishProfile(ctx, localpart)
	slog.Info("registered account", "user_id", userID, "device_id", deviceID)
	return &Account{
		Localpart:    localpart,
		UserID:       userID,
		DeviceID:     deviceID,
		AccessToken:  token,
		Admin:        params.Admin,
		DisplayName:  params.DisplayName,
		AvatarURL:    params.AvatarURL,
		NewlyCreated: true,
	}, nil
}

// LoginParams describes a password login attempt.
type LoginParams struct {
	Localpart string
	Password  string
	DeviceID  string
	// DisplayName is the client's `initial_device_display_name`, applied only when this
	// login creates the device: a client renaming a session uses PUT /devices/{id}, and a
	// re-login should not overwrite a name the user chose in a settings page.
	DisplayName string
	ClientID    string
	IP          string
	UserAgent   string
}

// Login checks a password and issues a token for a new device.
//
// Every failure is deliberately the same 403 M_FORBIDDEN with the same message: whether a
// user ID exists is not something a login endpoint should tell the world.
func (s *Service) Login(ctx context.Context, params LoginParams) (*Account, error) {
	// newDevice records that this login created a session rather than reusing one, which is the
	// difference between telling a user's room-mates about a new device and telling them nothing
	// happened forty times a day.
	var newDevice bool
	localpart := params.Localpart
	if !strings.HasPrefix(localpart, "@") {
		if domain := params.Localpart; strings.Contains(domain, ":") {
			parsed, _, err := matrix.ParseUserID(localpart, s.cfg.ServerName)
			if err != nil {
				return nil, errForbiddenLogin()
			}
			localpart = parsed
		}
	} else {
		parsed, _, err := matrix.ParseUserID(localpart, s.cfg.ServerName)
		if err != nil {
			return nil, errForbiddenLogin()
		}
		localpart = parsed
	}

	user, err := s.store.Users.Get(ctx, localpart)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, errForbiddenLogin()
		}
		return nil, fmt.Errorf("reading account %q: %w", localpart, err)
	}
	if user.Deactivated {
		return nil, matrix.NewAPIError(http.StatusForbidden, matrix.ErrUserDisabled,
			"This account has been deactivated")
	}
	if !passwordMatches(params.Password, user.PasswordHash) {
		return nil, errForbiddenLogin()
	}

	token := matrix.AccessToken()
	now := s.nowMS()

	// A client may name its own device (POST /login's device_id). Naming one that already
	// belongs to this account means "this session again", so the device is reused rather than
	// duplicated: two devices with the same purpose and one token each is how a session list
	// fills up with entries the user cannot tell apart.
	//
	// Somebody else's device is not a conflict to report — that would confirm the ID exists —
	// so this login quietly takes a fresh one.
	deviceID := params.DeviceID
	reuse := false
	if deviceID != "" {
		if existing, err := s.store.Devices.Get(ctx, deviceID); err == nil {
			if existing.Localpart != localpart {
				deviceID = matrix.DeviceID()
			} else {
				reuse = true
			}
		}
	}
	if deviceID == "" {
		deviceID = matrix.DeviceID()
	}

	err = s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		device := &store.Device{
			ID:          deviceID,
			Localpart:   localpart,
			DisplayName: params.DisplayName,
			ClientID:    params.ClientID,
			ClientIP:    params.IP,
			LastSeenUA:  params.UserAgent,
			CreatedMS:   now,
			LastSeenMS:  now,
		}
		if reuse {
			if err := s.store.Devices.Touch(ctx, deviceID, params.IP, params.UserAgent, now); err != nil {
				return fmt.Errorf("touching device on login: %w", err)
			}
			// A display name on re-login renames the session only if the caller named it;
			// an unnamed re-login must not erase a name set from a settings page.
			if params.DisplayName != "" && params.DisplayName != device.DisplayName {
				if err := s.store.Devices.Update(ctx, localpart, deviceID, params.DisplayName); err != nil {
					return fmt.Errorf("renaming device on login: %w", err)
				}
			}
		} else if err := s.store.Devices.Create(ctx, device); err != nil {
			return fmt.Errorf("creating device on login: %w", err)
		} else {
			// A new device is a device-list change even before it has keys: a client that
			// is encrypting for this user has to learn the device exists, or a message
			// sent from now on will not reach it.
			newDevice = true
		}
		accessToken := &store.AccessToken{
			Token:     token,
			DeviceID:  deviceID,
			Localpart: localpart,
			CreatedMS: now,
		}
		if err := s.store.Tokens.Create(ctx, accessToken); err != nil {
			return fmt.Errorf("issuing access token on login: %w", err)
		}
		return nil
	})
	if err == nil && newDevice && s.observeDevice != nil {
		// Best-effort: the session is already issued, and an account must not fail to log in
		// because an index of who should be told about it is unhappy.
		s.observeDevice(matrix.UserID(localpart, s.cfg.ServerName))
	}
	if err != nil {
		return nil, err
	}

	return &Account{
		Localpart:    localpart,
		UserID:       user.UserID,
		DeviceID:     deviceID,
		AccessToken:  token,
		Admin:        user.Admin,
		DisplayName:  user.DisplayName,
		AvatarURL:    user.AvatarURL,
		NewlyCreated: false,
	}, nil
}

// ResolveSession implements [httpx.SessionResolver].
func (s *Service) ResolveSession(r *http.Request, token string) (*httpx.Session, error) {
	ctx := r.Context()
	row, err := s.store.Tokens.Get(ctx, token)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewUnknownAccessToken()
		}
		return nil, fmt.Errorf("resolving access token: %w", err)
	}
	if row.ExpiryMS > 0 && row.ExpiryMS < s.nowMS() {
		return nil, matrix.NewUnknownAccessToken().WithSoftLogout()
	}
	user, err := s.store.Users.Get(ctx, row.Localpart)
	if err != nil {
		if store.IsNotFound(err) {
			// A token outliving its account (deactivation deletes both, so this is a
			// race or a manual delete): treat it as unknown rather than as an error.
			return nil, matrix.NewUnknownAccessToken()
		}
		return nil, fmt.Errorf("resolving account of access token: %w", err)
	}
	if user.Deactivated {
		return nil, matrix.NewAPIError(http.StatusForbidden, matrix.ErrUserDisabled,
			"This account has been deactivated")
	}
	device, err := s.store.Devices.Get(ctx, row.DeviceID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewUnknownAccessToken()
		}
		return nil, fmt.Errorf("resolving device of access token: %w", err)
	}

	// Touching the device is bookkeeping, not a request requirement; failing a request
	// because last_seen could not be written would be worse than a stale timestamp.
	if err := s.store.Devices.Touch(ctx, device.ID, clientIP(r), r.UserAgent(), s.nowMS()); err != nil {
		slog.Debug("updating device last_seen failed", "device_id", device.ID, "error", err)
	}

	return &httpx.Session{
		Localpart:   user.Localpart,
		UserID:      user.UserID,
		DeviceID:    device.ID,
		AccessToken: token,
		Admin:       user.Admin,
	}, nil
}

// DeleteDevice removes a device together with its access tokens and the transaction IDs it
// stored.
//
// The three go together because a token outliving its device would be a session nobody can
// list, revoke by name, or see signed out: /devices is how a user finds an unexpected session,
// and an unlisted one is unfoundable.
func (s *Service) DeleteDevice(ctx context.Context, localpart, deviceID string) error {
	return s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		device, err := s.store.Devices.Get(ctx, deviceID)
		if err != nil {
			if store.IsNotFound(err) {
				return matrix.NewNotFound("Device %q does not exist", deviceID)
			}
			return err
		}
		// Somebody else's device is reported as missing rather than forbidden: this is a
		// device ID, and confirming it exists is information about another account.
		if device.Localpart != localpart {
			return matrix.NewNotFound("Device %q does not exist", deviceID)
		}
		if err := s.store.Tokens.DeleteByDevice(ctx, deviceID); err != nil {
			return fmt.Errorf("revoking tokens of device %q: %w", deviceID, err)
		}
		if err := s.store.Txns.DeleteByDevice(ctx, localpart, deviceID); err != nil {
			return fmt.Errorf("clearing transactions of device %q: %w", deviceID, err)
		}
		return s.store.Devices.Delete(ctx, localpart, []string{deviceID})
	})
}

// Logout ends one session: it revokes the calling access token *and* removes the device it
// belonged to.
func (s *Service) Logout(ctx context.Context, localpart, deviceID string) error {
	return s.DeleteDevice(ctx, localpart, deviceID)
}

// LogoutAll revokes every token of an account (POST /logout/all).
func (s *Service) LogoutAll(ctx context.Context, localpart string) error {
	if err := s.store.Tokens.DeleteByUser(ctx, localpart); err != nil {
		return fmt.Errorf("revoking all access tokens of %q: %w", localpart, err)
	}
	return nil
}

// VerifyPassword checks a password for interactive authentication. It returns the
// specification's M_FORBIDDEN on a mismatch and never distinguishes "no such user" from
// "wrong password".
func (s *Service) VerifyPassword(ctx context.Context, localpart, password string) (*store.User, error) {
	user, err := s.store.Users.Get(ctx, localpart)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewForbidden("Auth '%s' failed", "m.login.password")
		}
		return nil, fmt.Errorf("reading account %q: %w", localpart, err)
	}
	if user.Deactivated {
		return nil, matrix.NewAPIError(http.StatusForbidden, matrix.ErrUserDisabled,
			"This account has been deactivated")
	}
	if !passwordMatches(password, user.PasswordHash) {
		return nil, matrix.NewForbidden("Auth '%s' failed", "m.login.password")
	}
	return user, nil
}

// SetPassword replaces an account's password and, unless told otherwise, revokes every other
// device's tokens (a password change must not leave the old sessions alive).
// The acting device survives either way. The specification's promise is that *other* devices are
// logged out, and the request itself is proof that whoever holds this session knows the new
// password; signing the caller out mid-task would punish them for the change they just made.
//
// logout_devices=false has to mean "change the password and change nothing else". A client
// changing a password on the user's behalf — a password manager, a recovery flow — says so
// exactly because the user's phone should not silently stop working.
func (s *Service) SetPassword(ctx context.Context, localpart, newPassword, actingDeviceID string,
	logoutOtherDevices bool) error {
	hash, err := hashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hashing new password: %w", err)
	}
	return s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := s.store.Users.Update(ctx, localpart, map[string]any{"password_hash": hash}); err != nil {
			return fmt.Errorf("storing new password: %w", err)
		}
		if !logoutOtherDevices {
			return nil
		}
		if err := s.store.Tokens.DeleteByUserExcept(ctx, localpart, []string{actingDeviceID}); err != nil {
			return fmt.Errorf("revoking other sessions after password change: %w", err)
		}
		return nil
	})
}

// Deactivate closes an account: it can never log in again, every token is revoked, and the
// local part stays reserved (the spec makes re-use of a deactivated ID impossible).
func (s *Service) Deactivate(ctx context.Context, localpart string, erase bool) error {
	err := s.store.Do(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := s.store.Users.Update(ctx, localpart, map[string]any{
			"deactivated":   true,
			"password_hash": "",
			"display_name":  "",
			"avatar_url":    "",
		}); err != nil {
			return fmt.Errorf("deactivating account: %w", err)
		}
		if err := s.store.Tokens.DeleteByUser(ctx, localpart); err != nil {
			return fmt.Errorf("revoking tokens of deactivated account: %w", err)
		}
		if err := s.store.Devices.DeleteByUser(ctx, localpart); err != nil {
			return fmt.Errorf("removing devices of deactivated account: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if erase {
		slog.Info("account deactivated with erase request", "localpart", localpart,
			"note", "history retention is governed by the erasure policy; content is not deleted synchronously")
	}
	return nil
}

// User returns an account record.
func (s *Service) User(ctx context.Context, localpart string) (*store.User, error) {
	user, err := s.store.Users.Get(ctx, localpart)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, matrix.NewNotFound("User %q does not exist", matrix.UserID(localpart, s.cfg.ServerName))
		}
		return nil, err
	}
	return user, nil
}

// LocalpartOfUserOrLocal accepts either a bare local part ("alice") or a full user ID
// ("@alice:server"), which is what login identifiers and interactive-auth credentials look
// like in the wild: both forms are common and both are unambiguous.
func (s *Service) LocalpartOfUserOrLocal(value string) (string, error) {
	localpart, domain, err := matrix.ParseLocalpartOrUserID(value, s.cfg.ServerName)
	if err != nil {
		return "", matrix.NewInvalidParam("User %q is not valid", value)
	}
	if domain != s.cfg.ServerName {
		return "", matrix.NewInvalidParam("User %q is not owned by this homeserver", value)
	}
	if !matrix.ValidLocalpart(localpart) {
		return "", matrix.NewInvalidParam("User %q is not valid", value)
	}
	return localpart, nil
}

// SetProfile writes the display name and/or avatar URL of an account. A nil argument is
// left untouched, which is how PUT with no field behaves.
func (s *Service) SetProfile(ctx context.Context, localpart, displayName string, avatarURL *string) error {
	updates := map[string]any{}
	if displayName != "" || avatarURL == nil {
		updates["display_name"] = displayName
	}
	if avatarURL != nil {
		updates["avatar_url"] = *avatarURL
	}
	if len(updates) == 0 {
		return nil
	}
	// Bumping the version is what tells other users' device lists and sync loops that
	// something about this account changed.
	user, err := s.store.Users.Get(ctx, localpart)
	if err != nil {
		if store.IsNotFound(err) {
			return matrix.NewNotFound("User %q does not exist", matrix.UserID(localpart, s.cfg.ServerName))
		}
		return err
	}
	updates["user_version"] = user.UserVersion + 1
	if err := s.store.Users.Update(ctx, localpart, updates); err != nil {
		if store.IsNotFound(err) {
			return matrix.NewNotFound("User %q does not exist", matrix.UserID(localpart, s.cfg.ServerName))
		}
		return fmt.Errorf("updating profile of %q: %w", localpart, err)
	}

	// Membership rows cache this user's name and avatar so that /members and
	// /joined_members do not need a profile lookup per row. Refreshing them here is what
	// makes an avatar change visible in rooms the user is already in: without it, every
	// room keeps showing the old face until the user happens to write a member event,
	// which changing a profile never does.
	//
	// Peers whose /sync state already contains the old member event are not woken; they
	// re-read the profile through /profile or /joined_members, which are correct. A room
	// that wants the change pushed to it needs a new member event, and Matrix has no
	// profile-changed event to send.
	_, err = s.store.Memberships.RefreshProfile(ctx, localpart,
		stringOf(updates, "display_name", user.DisplayName),
		stringOf(updates, "avatar_url", user.AvatarURL))
	if err != nil {
		return err
	}
	s.publishProfile(ctx, localpart)
	return nil
}

// stringOf reads one entry of an updates map, falling back to the value already stored
// when the map leaves that column alone.
func stringOf(updates map[string]any, key, fallback string) string {
	if value, ok := updates[key]; ok {
		if text, isString := value.(string); isString {
			return text
		}
	}
	return fallback
}

// LocalpartOfUser validates a user ID and returns the local part, refusing remote user
// IDs with M_INVALID_PARAM (a client asking to change somebody else's display name is
// asking for a parameter error, not for permission).
func (s *Service) LocalpartOfUser(userID string) (string, error) {
	localpart, domain, err := matrix.ParseUserID(userID, s.cfg.ServerName)
	if err != nil {
		return "", matrix.NewInvalidParam("User ID %q is not valid", userID)
	}
	if domain != s.cfg.ServerName {
		return "", matrix.NewInvalidParam("User %q is not owned by this homeserver", userID)
	}
	return localpart, nil
}

// hashPassword returns a bcrypt hash, or "" for an empty password (an account with no
// password simply cannot authenticate with m.login.password).
func hashPassword(password string) (string, error) {
	if password == "" {
		return "", nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(hash), nil
}

// passwordMatches compares a candidate against a stored hash. An empty stored hash never
// matches, so an account without a password cannot be logged into with a password.
func passwordMatches(candidate, hash string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(candidate)) == nil
}

// errForbiddenLogin is the one answer a failed login gives.
func errForbiddenLogin() *matrix.APIError {
	return matrix.NewForbidden("Invalid login details")
}

// clientIP extracts the caller's address, preferring the forwarder header the deployment
// is trusted to set.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		first, _, _ := strings.Cut(fwd, ",")
		return strings.TrimSpace(first)
	}
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}
