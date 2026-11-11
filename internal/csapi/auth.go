package csapi

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/accounts"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/enimatek-nl/qwatrix/internal/uia"
)

// registerAuthRoutes installs the account, session and device endpoints.
func registerAuthRoutes(mux *httpx.RouteTable, deps Deps) {
	// Registration, standard client-server flow.
	for _, path := range versionedPath("register") {
		mux.HandleFunc("POST "+path, handleRegister(deps))
	}
	for _, path := range versionedPath("register/available") {
		mux.HandleFunc("GET "+path, handleRegisterAvailable(deps))
	}

	// Synapse-compatible shared-secret registration. Complement registers nearly every
	// test user through this endpoint, so it is a first-class citizen rather than an
	// admin curiosity.
	mux.HandleFunc("GET /_synapse/admin/v1/register", handleSharedSecretRegisterNonce(deps))
	mux.HandleFunc("POST /_synapse/admin/v1/register", handleSharedSecretRegister(deps))

	// Login and logout.
	for _, path := range versionedPath("login") {
		mux.HandleFunc("GET "+path, handleLoginFlows(deps))
		mux.HandleFunc("POST "+path, handleLogin(deps))
	}
	for _, path := range versionedPath("logout") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleLogout(deps)))
	}
	for _, path := range versionedPath("logout/all") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleLogoutAll(deps)))
	}
	for _, path := range versionedPath("account/whoami") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleWhoami(deps)))
	}

	// Password change and account deactivation are user-interactive-authenticated.
	for _, path := range versionedPath("account/password") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleChangePassword(deps)))
	}
	for _, path := range versionedPath("account/deactivate") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleDeactivate(deps)))
	}

	// Device management.
	for _, path := range versionedPath("devices") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleListDevices(deps)))
	}
	for _, path := range versionedPath("delete_devices") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleDeleteDevices(deps)))
	}
	for _, path := range versionedPath("devices/{deviceID}") {
		// GET is its own endpoint, not a projection of GET /devices: a client that knows a
		// device ID (from a QR login, or its own session) asks about that one device.
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetDevice(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handleUpdateDevice(deps)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeleteDevice(deps)))
	}

	// Profile.
	for _, path := range versionedPath("profile/{userID}/displayname") {
		// Reading a profile is public since spec v1.1: a client must be able to show who
		// wrote a message to somebody who is not logged in to this server.
		mux.HandleFunc("GET "+path, deps.optionalAuth(handleGetDisplayName(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutDisplayName(deps)))
	}
	for _, path := range versionedPath("profile/{userID}/avatar_url") {
		mux.HandleFunc("GET "+path, deps.optionalAuth(handleGetAvatarURL(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutAvatarURL(deps)))
	}
}

// registerRequest is the body of POST /register.
type registerRequest struct {
	InhibitLogin             bool          `json:"inhibit_login"`
	RefreshToken             bool          `json:"refresh_token"`
	InitialDeviceDisplayName string        `json:"initial_device_display_name"`
	InitialDeviceID          string        `json:"initial_device_id"`
	DeviceID                 string        `json:"device_id"`
	Password                 string        `json:"password"`
	Username                 string        `json:"username"`
	Auth                     *uia.AuthBody `json:"auth"`
}

// registerResponse is the result of a successful registration.
type registerResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	ExpiresInMS int64  `json:"expires_in_ms,omitempty"`
}

// registrationStages is the flow this server offers for open registration.
//
// `m.login.dummy` is the placeholder stage the specification defines for exactly this
// situation: registration is open, so there is nothing to prove, but the exchange still has
// to look like an authentication exchange.
var registrationStages = []string{uia.StageDummy}

// handleRegister implements POST /register, including the 401/200 user-interactive dance.
func handleRegister(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if !deps.Config.EnableRegistration {
			httpx.RespondError(w, r, matrix.NewForbidden("Registration is not enabled on this homeserver"))
			return
		}

		// The request body is validated before the user-interactive exchange even starts.
		// The order matters: a client that sent a broken username wants to hear about the
		// username, not to be told to authenticate, and a client driving a UIA session
		// should not have to complete a stage to learn the name is taken.
		localpart := strings.ToLower(strings.TrimSpace(req.Username))
		if localpart != "" {
			if err := deps.Accounts.CheckLocalpart(localpart); err != nil {
				httpx.RespondError(w, r, err)
				return
			}
			if taken, err := deps.Accounts.LocalpartTaken(r.Context(), localpart); err != nil {
				httpx.RespondError(w, r, err)
				return
			} else if taken {
				httpx.RespondError(w, r, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrUserInUse,
					"That user ID is already registered"))
				return
			}
		}

		// An `auth` dict that names a stage but carries no usable session is refused rather
		// than treated as a fresh start: the session is what ties a completed stage to the
		// request that asked for it, and ignoring it would let a client reuse somebody
		// else's completed dummy stage by leaving the field out.
		var session *uia.Session
		if req.Auth != nil && req.Auth.Type != "" {
			switch req.Auth.Session {
			case "":
				// The client is presenting a stage it already knows it intends to satisfy, and
				// `m.login.dummy` asks for no credential: this request has said everything the flow
				// needs. Answering it with a challenge would turn a registration that could finish
				// into a 401 — which is why complement's blueprint builder, and gomatrixserverlib's
				// own client, could not bring a homeserver up at all against this server. A session is
				// still opened, so the stage is recorded against this request and cannot be borrowed
				// from another one.
				session = deps.UIA.Start(registrationStages, "")
			default:
				found, ok := deps.UIA.Get(req.Auth.Session)
				if !ok {
					// A named session that does not exist is a different thing from no session: the
					// caller is holding a stale or forged handle, and starting a fresh conversation
					// would let one registration's completed stage satisfy another.
					challenge := uia.ChallengeFor(deps.UIA.Start(registrationStages, ""), nil)
					challenge.ErrCode = matrix.ErrSessionInvalid
					challenge.Error = "Session is invalid or has expired"
					httpx.RespondJSON(w, http.StatusUnauthorized, challenge)
					return
				}
				session = found
			}
		}

		// The client has not authenticated yet: answer with the flows and a session, which
		// is what every Matrix client's registration form is driven by.
		if session == nil {
			httpx.RespondJSON(w, http.StatusUnauthorized,
				uia.ChallengeFor(deps.UIA.Start(registrationStages, ""), nil))
			return
		}
		stage, ok := req.Auth.MatchesStage(session)
		if !ok {
			deps.UIA.Drop(session.ID)
			httpx.RespondError(w, r, matrix.NewForbidden("Unsupported auth type %q", req.Auth.Type))
			return
		}
		if _, err := deps.UIA.Complete(session.ID, stage); err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		account, err := deps.Accounts.Register(r.Context(), accounts.RegisterParams{
			Localpart:   localpart,
			Password:    req.Password,
			DeviceID:    firstNonEmpty(req.DeviceID, req.InitialDeviceID),
			DisplayName: req.InitialDeviceDisplayName,
		})
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		deps.UIA.Drop(session.ID)

		response := registerResponse{UserID: account.UserID}
		if !req.InhibitLogin {
			response.AccessToken = account.AccessToken
			response.DeviceID = account.DeviceID
		}
		httpx.RespondJSON(w, http.StatusOK, response)
	}
}

// handleRegisterAvailable answers GET /register/available.
//
// An unusable name is an error, not a `200 {"available": false}`: the specification answers
// 400 with the reason in the error body (M_INVALID_USERNAME or M_USER_IN_USE), which is also
// what lets a client show the reason without parsing a second field.
func handleRegisterAvailable(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := strings.ToLower(r.URL.Query().Get("username"))
		if username == "" {
			httpx.RespondError(w, r, matrix.NewMissingParam("username"))
			return
		}
		if err := deps.Accounts.CheckLocalpart(username); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		taken, err := deps.Accounts.LocalpartTaken(r.Context(), username)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if taken {
			httpx.RespondError(w, r, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrUserInUse,
				"That user ID is already registered"))
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"available": true})
	}
}

// sharedSecretNonceResponse is the GET half of the Synapse admin registration endpoint.
type sharedSecretNonceResponse struct {
	Success        bool   `json:"success"`
	Nonce          string `json:"nonce"`
	Enabled        bool   `json:"enabled"`
	SessionID      string `json:"session_id"`
	IdentityServer string `json:"identity_server"`
}

// handleSharedSecretRegisterNonce answers GET /_synapse/admin/v1/register.
//
// A server without a shared secret must 404 rather than 403: that is the signal Complement
// uses to skip the tests that need shared-secret registration instead of failing them.
func handleSharedSecretRegisterNonce(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Config.RegistrationSecret == "" {
			httpx.RespondError(w, r, matrix.NewUnrecognized("Shared-secret registration is not enabled"))
			return
		}
		nonce, err := deps.SharedSecrets.Issue()
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, sharedSecretNonceResponse{
			Success:        true,
			Nonce:          nonce,
			Enabled:        true,
			SessionID:      nonce,
			IdentityServer: "",
		})
	}
}

// sharedSecretRegisterRequest is the POST body of the admin registration endpoint.
type sharedSecretRegisterRequest struct {
	Nonce    string `json:"nonce"`
	Username string `json:"username"`
	Password string `json:"password"`
	MAC      string `json:"mac"`
	Admin    bool   `json:"admin"`
}

// handleSharedSecretRegister completes the Synapse-compatible registration handshake.
//
// The MAC recipe is Synapse's: HMAC-SHA1 keyed with the shared secret over
// nonce NUL username NUL password NUL ("admin"|"notadmin"). It is reproduced rather than
// negotiated because it is the contract Complement (and every operator's registration
// script) speaks.
func handleSharedSecretRegister(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Config.RegistrationSecret == "" {
			httpx.RespondError(w, r, matrix.NewUnrecognized("Shared-secret registration is not enabled"))
			return
		}
		var req sharedSecretRegisterRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if err := deps.SharedSecrets.Consume(req.Nonce); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if strings.TrimSpace(req.Username) == "" {
			// Unlike POST /register, where an omitted username means "the server picks
			// one", this endpoint exists to create a named account: an admin script that
			// mistypes the username must not quietly mint something else.
			httpx.RespondError(w, r, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidUsername,
				"A username is required"))
			return
		}
		if !verifySharedSecretMAC(deps.Config.RegistrationSecret, req) {
			httpx.RespondError(w, r, matrix.NewAPIError(http.StatusUnauthorized, matrix.ErrUnauthorized,
				"MAC does not match the shared secret"))
			return
		}

		// Synapse's shared-secret protocol hashes the username the client sent, but stores
		// it downcased, and clients rely on that: an admin script that writes
		// `--user=Alice` expects @alice, not an error. The MAC is verified against the
		// string as sent, above.
		account, err := deps.Accounts.Register(r.Context(), accounts.RegisterParams{
			Localpart: strings.ToLower(req.Username),
			Password:  req.Password,
			Admin:     req.Admin,
		})
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"user_id":      account.UserID,
			"access_token": account.AccessToken,
			"device_id":    account.DeviceID,
		})
	}
}

// verifySharedSecretMAC recomputes the Synapse registration MAC in constant time.
func verifySharedSecretMAC(secret string, req sharedSecretRegisterRequest) bool {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(req.Nonce))
	mac.Write([]byte{0})
	mac.Write([]byte(req.Username))
	mac.Write([]byte{0})
	mac.Write([]byte(req.Password))
	mac.Write([]byte{0})
	if req.Admin {
		mac.Write([]byte("admin"))
	} else {
		mac.Write([]byte("notadmin"))
	}
	expected := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(req.MAC))) == 1
}

// loginFlowsResponse is GET /login.
type loginFlowsResponse struct {
	Flows []loginFlow `json:"flows"`
}

type loginFlow struct {
	Type string `json:"type"`
}

// handleLoginFlows advertises the login mechanisms. `m.login.password` is the one qwatrix
// authenticates; `m.login.token` is deliberately absent until login tokens exist, because
// advertising a flow means promising it works.
func handleLoginFlows(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flows := []loginFlow{{Type: uia.StagePassword}}
		if deps.Config.EnableRegistration {
			// Registration-as-login: a client that has no account yet can be pointed at
			// the dummy flow, which is what Synapse advertises when registration is open.
			flows = append(flows, loginFlow{Type: uia.StageDummy})
		}
		httpx.RespondJSON(w, http.StatusOK, loginFlowsResponse{Flows: flows})
	}
}

// loginRequest is POST /login. Both the modern `identifier` object and the deprecated
// top-level `user` field are accepted, because both are in the wild in current clients.
type loginRequest struct {
	Type                     string          `json:"type"`
	Identifier               *uia.Identifier `json:"identifier"`
	User                     string          `json:"user"`
	Address                  string          `json:"address"`
	Medium                   string          `json:"medium"`
	Password                 string          `json:"password"`
	Token                    string          `json:"token"`
	DeviceID                 string          `json:"device_id"`
	InitialDeviceDisplayName string          `json:"initial_device_display_name"`
	RefreshToken             bool            `json:"refresh_token"`
}

// loginResponse is the result of a successful login.
type loginResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
	ExpiresInMS int64  `json:"expires_in_ms,omitempty"`
	WellKnown   any    `json:"well_known,omitempty"`
}

// handleLogin implements POST /login.
func handleLogin(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if req.Type != "" && req.Type != uia.StagePassword {
			httpx.RespondError(w, r, matrix.NewInvalidParam("Unsupported login type %q", req.Type))
			return
		}
		identifier := req.User
		if req.Identifier != nil && req.Identifier.User != "" {
			identifier = req.Identifier.User
		}
		// Accounts are stored in lower case and clients send what a human typed, so the
		// identifier is folded before lookup: a user ID's local part was created lowercase by
		// this server, and its domain is a DNS name, which is case-insensitive by definition.
		// Registration stays strict — this endpoint is a login attempt, where a wrong case is
		// a typo, not a request to create an account.
		identifier = strings.ToLower(identifier)
		if identifier == "" {
			httpx.RespondError(w, r, matrix.NewMissingParam("identifier"))
			return
		}
		localpart, err := deps.Accounts.LocalpartOfUserOrLocal(identifier)
		if err != nil {
			// A malformed identifier is answered like a wrong password: the login
			// endpoint does not get to tell the caller which user IDs exist.
			httpx.RespondError(w, r, matrix.NewForbidden("Invalid login details"))
			return
		}

		account, err := deps.Accounts.Login(r.Context(), accounts.LoginParams{
			Localpart:   localpart,
			Password:    req.Password,
			DeviceID:    req.DeviceID,
			DisplayName: req.InitialDeviceDisplayName,
			IP:          clientIP(r),
			UserAgent:   r.UserAgent(),
		})
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, loginResponse{
			UserID:      account.UserID,
			AccessToken: account.AccessToken,
			DeviceID:    account.DeviceID,
		})
	}
}

// handleLogout implements POST /logout.
func handleLogout(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if err := deps.Accounts.Logout(r.Context(), session.Localpart, session.DeviceID); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleLogoutAll implements POST /logout/all.
func handleLogoutAll(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if err := deps.Accounts.LogoutAll(r.Context(), session.Localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// whoamiResponse is GET /account/whoami.
type whoamiResponse struct {
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id,omitempty"`
	IsGuest  bool   `json:"is_guest,omitempty"`
}

// handleWhoami implements GET /account/whoami.
func handleWhoami(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		httpx.RespondJSON(w, http.StatusOK, whoamiResponse{
			UserID:   session.UserID,
			DeviceID: session.DeviceID,
		})
	}
}

// handleChangePassword implements POST /account/password behind m.login.password.
func handleChangePassword(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Auth          *uia.AuthBody `json:"auth"`
			NewPassword   string        `json:"new_password"`
			LogoutDevices *bool         `json:"logout_devices"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		session := httpx.MustSession(r)
		if done, err := deps.authenticatePassword(w, r, session.UserID, req.Auth, session.Localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		} else if !done {
			return // the 401 challenge has been written
		}
		if req.NewPassword == "" {
			httpx.RespondError(w, r, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrWeakPassword,
				"The new password must not be empty"))
			return
		}

		// Absent means true, the specification's default. The acting session survives either
		// way — the spec promises *other* devices are logged out, and the caller just proved
		// they know the new password.
		logoutOthers := true
		if req.LogoutDevices != nil {
			logoutOthers = *req.LogoutDevices
		}
		if err := deps.Accounts.SetPassword(r.Context(), session.Localpart, req.NewPassword,
			session.DeviceID, logoutOthers); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleDeactivate implements POST /account/deactivate.
func handleDeactivate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Auth  *uia.AuthBody `json:"auth"`
			Erase bool          `json:"erase"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			// An empty body is allowed: it just has to carry the auth object, so a body
			// that is not JSON at all is the only failure here.
			httpx.RespondError(w, r, err)
			return
		}
		session := httpx.MustSession(r)
		if done, err := deps.authenticatePassword(w, r, session.UserID, req.Auth, session.Localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		} else if !done {
			return
		}
		if err := deps.Accounts.Deactivate(r.Context(), session.Localpart, req.Erase); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"id_server_account_exists": false,
		})
	}
}

// device is one entry of GET /devices.
type device struct {
	DeviceID    string `json:"device_id"`
	DisplayName string `json:"display_name,omitempty"`
	LastSeenTS  int64  `json:"last_seen_ts,omitempty"`
	LastSeenIP  string `json:"last_seen_ip,omitempty"`
}

// handleListDevices implements GET /devices.
func handleListDevices(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		devices, err := deps.Store.Devices.List(r.Context(), session.Localpart)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		out := make([]device, 0, len(devices))
		for _, d := range devices {
			out = append(out, device{
				DeviceID:    d.ID,
				DisplayName: d.DisplayName,
				LastSeenTS:  d.LastSeenMS,
				LastSeenIP:  d.ClientIP,
			})
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"devices": out})
	}
}

// handleGetDevice implements GET /devices/{deviceID}.
func handleGetDevice(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		deviceID := r.PathValue("deviceID")

		found, err := deps.Store.Devices.Get(r.Context(), deviceID)
		if err != nil {
			httpx.RespondError(w, r, matrix.NewNotFound("Device %q does not exist", deviceID))
			return
		}
		// A device belongs to one account; another account's device is indistinguishable
		// from one that does not exist, because this endpoint is not a way to enumerate
		// other people's sessions.
		if found.Localpart != session.Localpart {
			httpx.RespondError(w, r, matrix.NewNotFound("Device %q does not exist", deviceID))
			return
		}
		httpx.RespondJSON(w, http.StatusOK, device{
			DeviceID:    found.ID,
			DisplayName: found.DisplayName,
			LastSeenTS:  found.LastSeenMS,
			LastSeenIP:  found.ClientIP,
		})
	}
}

// handleUpdateDevice implements PUT /devices/{deviceID}.
func handleUpdateDevice(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var req struct {
			DisplayName *string `json:"display_name"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if req.DisplayName == nil {
			httpx.RespondJSON(w, http.StatusOK, map[string]any{})
			return
		}
		if err := deps.Store.Devices.Update(r.Context(), session.Localpart, r.PathValue("deviceID"), *req.DisplayName); err != nil {
			httpx.RespondError(w, r, notFoundAsAPIError(err, "device"))
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleDeleteDevice implements DELETE /devices/{deviceID}.
func handleDeleteDevice(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var req struct {
			Auth     *uia.AuthBody `json:"auth"`
			Password string        `json:"password"`
		}
		// The body is optional on this endpoint, so a non-JSON body is treated as absent.
		_ = httpx.DecodeJSON(r, &req)

		if done, err := deps.authenticatePassword(w, r, session.UserID, req.Auth, session.Localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		} else if !done {
			return
		}

		deviceID := r.PathValue("deviceID")
		if err := deps.deleteDevices(r, session.Localpart, []string{deviceID}); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleDeleteDevices implements POST /delete_devices.
func handleDeleteDevices(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		var req struct {
			Auth    *uia.AuthBody `json:"auth"`
			Devices []string      `json:"devices"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if len(req.Devices) == 0 {
			httpx.RespondError(w, r, matrix.NewMissingParam("devices"))
			return
		}
		if done, err := deps.authenticatePassword(w, r, session.UserID, req.Auth, session.Localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		} else if !done {
			return
		}
		if err := deps.deleteDevices(r, session.Localpart, req.Devices); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleGetDisplayName implements GET /profile/{userID}/displayname.
func handleGetDisplayName(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := deps.localOfPathUser(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		user, err := deps.Accounts.User(r.Context(), localpart)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"displayname": user.DisplayName})
	}
}

// handlePutDisplayName implements PUT /profile/{userID}/displayname.
func handlePutDisplayName(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := deps.localOfPathUser(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if err := deps.ensureSelfOrAdmin(r, localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		var req struct {
			DisplayName *string `json:"displayname"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		name := ""
		if req.DisplayName != nil {
			name = *req.DisplayName
		}
		if err := deps.updateProfile(r, localpart, &name, nil); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleGetAvatarURL implements GET /profile/{userID}/avatar_url.
func handleGetAvatarURL(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := deps.localOfPathUser(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		user, err := deps.Accounts.User(r.Context(), localpart)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"avatar_url": user.AvatarURL})
	}
}

// handlePutAvatarURL implements PUT /profile/{userID}/avatar_url.
func handlePutAvatarURL(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := deps.localOfPathUser(r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if err := deps.ensureSelfOrAdmin(r, localpart); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		var req struct {
			AvatarURL *string `json:"avatar_url"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		avatar := ""
		if req.AvatarURL != nil {
			avatar = *req.AvatarURL
		}
		if err := deps.updateProfile(r, localpart, nil, &avatar); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{})
	}
}

// authenticatePassword runs the m.login.password stage of a user-interactive exchange.
//
// It returns (true, nil) when the caller is authenticated, (false, nil) when the 401
// challenge has been written to w, and (false, err) when the request should be answered
// with that error. The three-way shape exists because the specification's answer to "wrong
// password" is not an error but a fresh challenge naming the stages still outstanding.
func (deps Deps) authenticatePassword(w http.ResponseWriter, r *http.Request, userID string, auth *uia.AuthBody, localpart string) (bool, error) {
	if auth == nil || auth.Type == "" {
		session := deps.UIA.Start([]string{uia.StagePassword}, userID)
		httpx.RespondJSON(w, http.StatusUnauthorized, uia.ChallengeFor(session, nil))
		return false, nil
	}
	if auth.Type != uia.StagePassword {
		return false, matrix.NewForbidden("Unsupported auth type %q", auth.Type)
	}

	claimed, ok := auth.ClaimedUser()
	if !ok {
		return false, matrix.NewMissingParam("identifier.user")
	}
	claimedLocal, err := deps.Accounts.LocalpartOfUserOrLocal(claimed)
	if err != nil {
		return false, matrix.NewForbidden("Auth '%s' failed", uia.StagePassword)
	}
	// The credential must belong to the caller: an authenticated user may not use
	// somebody else's password to satisfy their own session.
	if claimedLocal != localpart {
		return false, matrix.NewForbidden("Auth '%s' failed", uia.StagePassword)
	}

	if _, err := deps.Accounts.VerifyPassword(r.Context(), claimedLocal, auth.Password); err != nil {
		if auth.Session != "" {
			deps.UIA.Fail(auth.Session)
		}
		session := deps.UIA.Start([]string{uia.StagePassword}, userID)
		challenge := uia.ChallengeFor(session, nil).WithFailure(matrix.ErrForbidden, "Invalid password")
		httpx.RespondJSON(w, http.StatusUnauthorized, challenge)
		return false, nil
	}
	if auth.Session != "" {
		deps.UIA.Drop(auth.Session)
	}
	return true, nil
}

// deleteDevices removes devices and their tokens and transactions, in one place so that
// deleting a device cannot leave a token behind.
func (deps Deps) deleteDevices(r *http.Request, localpart string, deviceIDs []string) error {
	for _, deviceID := range deviceIDs {
		if err := deps.Accounts.DeleteDevice(r.Context(), localpart, deviceID); err != nil {
			return err
		}
	}
	return nil
}

// localOfPathUser resolves the {userID} path parameter of a profile endpoint to a local
// part on this server.
func (deps Deps) localOfPathUser(r *http.Request) (string, error) {
	raw := r.PathValue("userID")
	if raw == "" {
		return "", matrix.NewInvalidParam("Missing user ID")
	}
	// A bare localpart is accepted too: clients and operators type it, and the spec's
	// grammar makes the two forms unambiguous.
	if !strings.HasPrefix(raw, "@") {
		if strings.Contains(raw, ":") {
			return "", matrix.NewInvalidParam("User ID %q is not valid", raw)
		}
		return raw, nil
	}
	return deps.Accounts.LocalpartOfUser(raw)
}

// ensureSelfOrAdmin enforces the rule that profile writes are for yourself, unless you are
// an administrator acting through the admin API.
func (deps Deps) ensureSelfOrAdmin(r *http.Request, localpart string) error {
	session := httpx.SessionFrom(r)
	if session == nil {
		return matrix.NewMissingAccessToken()
	}
	if session.Localpart == localpart {
		return nil
	}
	if session.Admin {
		return nil
	}
	return matrix.NewForbidden("You do not have permission to change this user's profile")
}

// isNotFound reports a store miss, wherever it came from.
func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// notFoundAsAPIError turns a store miss into the specification's 404.
func notFoundAsAPIError(err error, what string) error {
	if isNotFound(err) {
		return matrix.NewNotFound("%s does not exist", what)
	}
	return err
}

// firstNonEmpty returns the first non-empty string, for the several aliases a field has.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// clientIP is re-exported here so handlers do not reach into the accounts package for it.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		return strings.TrimSpace(first)
	}
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

// updateProfile changes an account's profile and pushes the change into the rooms they are in.
//
// The previous values have to be read before the write, because they are what distinguishes a
// room-specific display name from a copy of the old global one — and that difference is the whole
// reason a user can be "Alice" in one room and "Work Alice" in another.
func (deps Deps) updateProfile(r *http.Request, localpart string, displayName, avatarURL *string) error {
	previous := struct{ displayName, avatarURL string }{}
	if user, err := deps.Store.Users.Get(r.Context(), localpart); err == nil {
		previous.displayName = user.DisplayName
		previous.avatarURL = user.AvatarURL
	}
	if err := deps.Accounts.SetProfile(r.Context(), localpart, derefOrEmpty(displayName), avatarURL); err != nil {
		return err
	}
	if deps.Rooms == nil {
		return nil
	}
	// Propagating to rooms is deliberately not allowed to fail the request: the profile has
	// already changed, and a room the caller can no longer write to must not turn a successful
	// rename into a 500 that a client would retry forever.
	if err := deps.Rooms.RefreshMemberProfile(r.Context(), localpart,
		previous.displayName, previous.avatarURL); err != nil {
		slog.Warn("csapi: could not propagate a profile change to rooms",
			"localpart", localpart, "error", err)
	}
	return nil
}

// derefOrEmpty reads an optional string as "" when absent, which is how the accounts service is
// told "no opinion about the display name".
func derefOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
