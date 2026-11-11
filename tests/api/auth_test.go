package api_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// TestLoginFlowsAndSessions covers the login endpoint and the session lifecycle around it:
// what is advertised, what a good and a bad credential produce, and what logout does to a
// token.

func TestLoginFlowsAdvertisesPassword(t *testing.T) {
	t.Parallel()

	srv := harness.New(t)

	resp := srv.Get(t, "/_matrix/client/v3/login", "").MustStatus(t, 200)
	var body struct {
		Flows []struct {
			Type string `json:"type"`
		} `json:"flows"`
	}
	resp.JSON(t, &body)

	var found bool
	for _, flow := range body.Flows {
		if flow.Type == "m.login.password" {
			found = true
		}
	}
	if !found {
		t.Fatalf("flows %v does not include m.login.password", body.Flows)
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "alice", "correct horse", false)

	t.Run("correct credentials return a token and a device", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":                        "m.login.password",
			"identifier":                  map[string]any{"type": "m.id.user", "user": "alice"},
			"password":                    "correct horse",
			"initial_device_display_name": "laptop",
		})
		var body struct {
			UserID      string `json:"user_id"`
			AccessToken string `json:"access_token"`
			DeviceID    string `json:"device_id"`
		}
		resp.MustStatus(t, 200).JSON(t, &body)

		if body.UserID != alice.UserID {
			t.Errorf("user_id = %q, want %q", body.UserID, alice.UserID)
		}
		if body.AccessToken == "" || body.DeviceID == "" {
			t.Fatalf("login returned no credential: %s", resp.Body)
		}
		// The new token is a different session; the one registration handed out must stay
		// alive, or logging in anywhere would log you out everywhere.
		srv.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken).MustStatus(t, 200)
	})

	t.Run("a full user ID is accepted as the identifier", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]any{"type": "m.id.user", "user": alice.UserID},
			"password":   "correct horse",
		})
		resp.MustStatus(t, 200)
	})

	t.Run("the deprecated top-level user field is accepted", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":     "m.login.password",
			"user":     "alice",
			"password": "correct horse",
		})
		resp.MustStatus(t, 200)
	})

	t.Run("wrong password is M_FORBIDDEN and says nothing about the user", func(t *testing.T) {
		for _, tc := range []struct{ name, user, password string }{
			{name: "wrong password", user: "alice", password: "wrong horse"},
			{name: "unknown user", user: "nobody", password: "anything"},
			{name: "empty password", user: "alice", password: ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
					"type":       "m.login.password",
					"identifier": map[string]any{"type": "m.id.user", "user": tc.user},
					"password":   tc.password,
				})
				resp.MustError(t, http.StatusForbidden, "M_FORBIDDEN")
			})
		}
	})

	t.Run("a remote user ID is refused without revealing anything", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]any{"type": "m.id.user", "user": "@alice:evil.example"},
			"password":   "correct horse",
		})
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("a login for a remote user succeeded: %s", resp.Body)
		}
	})

	t.Run("an unsupported login type is a parameter error", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":     "m.login.token",
			"token":    "abc",
			"password": "x",
		})
		resp.MustError(t, http.StatusBadRequest, "M_INVALID_PARAM")
	})

	t.Run("a missing identifier is reported as such", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":     "m.login.password",
			"password": "correct horse",
		})
		resp.MustError(t, http.StatusBadRequest, "M_MISSING_PARAM")
	})
}

func TestWhoamiAndTokenHandling(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "alice", "pw", false)

	t.Run("whoami reports the user and device", func(t *testing.T) {
		resp := srv.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken).MustStatus(t, 200)
		var body struct {
			UserID   string `json:"user_id"`
			DeviceID string `json:"device_id"`
			IsGuest  *bool  `json:"is_guest"`
		}
		resp.JSON(t, &body)
		if body.UserID != alice.UserID {
			t.Errorf("user_id = %q, want %q", body.UserID, alice.UserID)
		}
		if body.DeviceID != alice.DeviceID {
			t.Errorf("device_id = %q, want %q", body.DeviceID, alice.DeviceID)
		}
		if body.IsGuest != nil && *body.IsGuest {
			t.Error("is_guest is true for a registered account")
		}
	})

	t.Run("no token is M_MISSING_TOKEN", func(t *testing.T) {
		srv.Get(t, "/_matrix/client/v3/account/whoami", "").
			MustError(t, http.StatusUnauthorized, "M_MISSING_TOKEN")
	})

	t.Run("an unknown token is M_UNKNOWN_TOKEN", func(t *testing.T) {
		srv.Get(t, "/_matrix/client/v3/account/whoami", "not-a-token").
			MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	})

	t.Run("the deprecated access_token query parameter is accepted", func(t *testing.T) {
		path := "/_matrix/client/v3/account/whoami?access_token=" + url.QueryEscape(alice.AccessToken)
		srv.Get(t, path, "").MustStatus(t, 200)
	})

	t.Run("a malformed JSON body is a bad-json error, not a stack trace", func(t *testing.T) {
		path := "/_matrix/client/v3/profile/" + url.PathEscape(alice.UserID) + "/displayname"
		resp := srv.Do(t, harness.Request{
			Method:  http.MethodPut,
			Path:    path,
			Token:   alice.AccessToken,
			RawBody: []byte("{not json"),
		})
		resp.MustError(t, http.StatusBadRequest, "M_BAD_JSON")
		for _, leak := range []string{"goroutine", "panic", "sqlite", "gorm", ".go:"} {
			if contains([]string{string(resp.Body)}, leak) {
				t.Errorf("error body leaks internals (%s): %s", leak, resp.Body)
			}
		}
		// Nothing was written by the failed request.
		srv.Get(t, path, alice.AccessToken).MustStatus(t, 200)
	})
}

func TestLogout(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "alice", "pw", false)
	second := srv.Login(t, "alice", "pw")

	t.Run("logout revokes exactly the calling device", func(t *testing.T) {
		srv.PostJSON(t, "/_matrix/client/v3/logout", alice.AccessToken, map[string]any{}).
			MustStatus(t, 200)

		srv.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken).
			MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
		srv.Get(t, "/_matrix/client/v3/account/whoami", second.AccessToken).
			MustStatus(t, 200)
	})

	t.Run("logging out again with a revoked token is unknown-token", func(t *testing.T) {
		srv.PostJSON(t, "/_matrix/client/v3/logout", alice.AccessToken, map[string]any{}).
			MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	})

	t.Run("logout/all revokes every device", func(t *testing.T) {
		srv.PostJSON(t, "/_matrix/client/v3/logout/all", second.AccessToken, map[string]any{}).
			MustStatus(t, 200)

		srv.Get(t, "/_matrix/client/v3/account/whoami", second.AccessToken).
			MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")

		// And the account itself still exists.
		srv.Login(t, "alice", "pw")
	})
}

func TestDeviceManagement(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "alice", "pw", false)
	extra := srv.Login(t, "alice", "pw")

	t.Run("the device list contains both devices", func(t *testing.T) {
		resp := srv.Get(t, "/_matrix/client/v3/devices", alice.AccessToken).MustStatus(t, 200)
		var body struct {
			Devices []struct {
				DeviceID    string `json:"device_id"`
				DisplayName string `json:"display_name"`
			} `json:"devices"`
		}
		resp.JSON(t, &body)

		seen := map[string]string{}
		for _, device := range body.Devices {
			seen[device.DeviceID] = device.DisplayName
		}
		for _, want := range []string{alice.DeviceID, extra.DeviceID} {
			if _, ok := seen[want]; !ok {
				t.Errorf("devices %v does not list %s", seen, want)
			}
		}
	})

	t.Run("a device can be renamed", func(t *testing.T) {
		path := "/_matrix/client/v3/devices/" + url.PathEscape(alice.DeviceID)
		srv.PutJSON(t, path, alice.AccessToken, map[string]any{"display_name": "kitchen tablet"}).
			MustStatus(t, 200)

		resp := srv.Get(t, "/_matrix/client/v3/devices", alice.AccessToken).MustStatus(t, 200)
		var body struct {
			Devices []struct {
				DeviceID    string `json:"device_id"`
				DisplayName string `json:"display_name"`
			} `json:"devices"`
		}
		resp.JSON(t, &body)
		var renamed bool
		for _, device := range body.Devices {
			if device.DeviceID == alice.DeviceID && device.DisplayName == "kitchen tablet" {
				renamed = true
			}
		}
		if !renamed {
			t.Errorf("device was not renamed: %s", resp.Body)
		}
	})

	t.Run("deleting a device requires a password", func(t *testing.T) {
		path := "/_matrix/client/v3/devices/" + url.PathEscape(extra.DeviceID)

		// No auth object: the server must answer with a 401 challenge naming the stages.
		resp := srv.Do(t, harness.Request{Method: http.MethodDelete, Path: path, Token: alice.AccessToken})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401\nbody: %s", resp.StatusCode, resp.Body)
		}
		var challenge struct {
			Flows []struct {
				Stages []string `json:"stages"`
			} `json:"flows"`
			Session string `json:"session"`
		}
		resp.JSON(t, &challenge)
		if challenge.Session == "" || len(challenge.Flows) == 0 {
			t.Fatalf("challenge carries no session or flows: %s", resp.Body)
		}
		var hasPassword bool
		for _, stage := range challenge.Flows[0].Stages {
			if stage == "m.login.password" {
				hasPassword = true
			}
		}
		if !hasPassword {
			t.Errorf("challenge does not offer m.login.password: %s", resp.Body)
		}

		// A wrong password must not delete anything either.
		failed := srv.Do(t, harness.Request{
			Method: http.MethodDelete,
			Path:   path,
			Token:  alice.AccessToken,
			Body: map[string]any{
				"auth": map[string]any{
					"type":       "m.login.password",
					"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
					"password":   "not the password",
				},
			},
		})
		// A failed stage is answered with the flows *and* the reason: one response has to
		// say both "you are not authenticated" and "that credential was wrong".
		failed.MustError(t, http.StatusUnauthorized, "M_FORBIDDEN")
		var body struct {
			Flows []struct {
				Stages []string `json:"stages"`
			} `json:"flows"`
		}
		failed.JSON(t, &body)
		if len(body.Flows) == 0 {
			t.Errorf("a failed stage answered without flows: %s", failed.Body)
		}
	})

	t.Run("deleting a device with a password revokes its token", func(t *testing.T) {
		path := "/_matrix/client/v3/devices/" + url.PathEscape(extra.DeviceID)
		srv.Do(t, harness.Request{
			Method: http.MethodDelete,
			Path:   path,
			Token:  alice.AccessToken,
			Body: map[string]any{
				"auth": map[string]any{
					"type":       "m.login.password",
					"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
					"password":   "pw",
				},
			},
		}).MustStatus(t, 200)

		srv.Get(t, "/_matrix/client/v3/account/whoami", extra.AccessToken).
			MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	})

	t.Run("a device of another user cannot be deleted", func(t *testing.T) {
		bob := srv.Register(t, "bob", "pw", false)
		path := "/_matrix/client/v3/devices/" + url.PathEscape(bob.DeviceID)
		resp := srv.Do(t, harness.Request{
			Method: http.MethodDelete,
			Path:   path,
			Token:  alice.AccessToken,
			Body: map[string]any{
				"auth": map[string]any{
					"type":       "m.login.password",
					"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
					"password":   "pw",
				},
			},
		})
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("alice deleted bob's device: %s", resp.Body)
		}
		srv.Get(t, "/_matrix/client/v3/account/whoami", bob.AccessToken).MustStatus(t, 200)
	})

	t.Run("POST /delete_devices removes several at once", func(t *testing.T) {
		first := srv.Login(t, "alice", "pw")
		second := srv.Login(t, "alice", "pw")

		srv.PostJSON(t, "/_matrix/client/v3/delete_devices", alice.AccessToken, map[string]any{
			"devices": []string{first.DeviceID, second.DeviceID},
			"auth": map[string]any{
				"type":       "m.login.password",
				"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
				"password":   "pw",
			},
		}).MustStatus(t, 200)

		for _, account := range []*harness.Account{first, second} {
			srv.Get(t, "/_matrix/client/v3/account/whoami", account.AccessToken).
				MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
		}
	})
}

func TestChangePassword(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "alice", "first", false)
	other := srv.Login(t, "alice", "first")

	auth := func(password string) map[string]any {
		return map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
			"password":   password,
		}
	}

	t.Run("changing the password needs the current one", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/account/password", alice.AccessToken, map[string]any{
			"new_password": "second",
			"auth":         auth("wrong"),
		})
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("a password change with the wrong current password succeeded: %s", resp.Body)
		}
	})

	t.Run("a successful change keeps the acting device and revokes the rest", func(t *testing.T) {
		srv.PostJSON(t, "/_matrix/client/v3/account/password", alice.AccessToken, map[string]any{
			"new_password": "second",
			"auth":         auth("first"),
		}).MustStatus(t, 200)

		srv.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken).MustStatus(t, 200)
		srv.Get(t, "/_matrix/client/v3/account/whoami", other.AccessToken).
			MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")

		// The old password no longer authenticates.
		srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
			"password":   "first",
		}).MustError(t, http.StatusForbidden, "M_FORBIDDEN")
		srv.Login(t, "alice", "second")
	})

	t.Run("logout_devices true still keeps the acting device", func(t *testing.T) {
		// An earlier version of this test asserted the opposite — that naming the flag
		// explicitly revokes the caller's own session as well. Nothing in the specification
		// says so: the promise is that *other* devices are logged out, and the request itself
		// proves the caller knows the new password. Signing the caller out mid-task would
		// punish them for the change they just made, and no client sends `true` expecting to
		// be disconnected.
		alice = srv.Login(t, "alice", "second")
		srv.PostJSON(t, "/_matrix/client/v3/account/password", alice.AccessToken, map[string]any{
			"new_password":   "third",
			"logout_devices": true,
			"auth":           auth("second"),
		}).MustStatus(t, 200)

		srv.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken).MustStatus(t, 200)
		srv.Login(t, "alice", "third")
	})

	t.Run("an empty new password is refused", func(t *testing.T) {
		alice = srv.Login(t, "alice", "third")
		resp := srv.PostJSON(t, "/_matrix/client/v3/account/password", alice.AccessToken, map[string]any{
			"new_password": "",
			"auth":         auth("third"),
		})
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("an empty password was accepted: %s", resp.Body)
		}
	})
}

func TestDeactivateAccount(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "alice", "pw", false)

	t.Run("deactivation requires a password", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/account/deactivate", alice.AccessToken, map[string]any{})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want a 401 challenge\nbody: %s", resp.StatusCode, resp.Body)
		}
		srv.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken).MustStatus(t, 200)
	})

	t.Run("a deactivated account cannot authenticate", func(t *testing.T) {
		srv.PostJSON(t, "/_matrix/client/v3/account/deactivate", alice.AccessToken, map[string]any{
			"auth": map[string]any{
				"type":       "m.login.password",
				"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
				"password":   "pw",
			},
		}).MustStatus(t, 200)

		srv.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken).
			MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
		resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]any{"type": "m.id.user", "user": "alice"},
			"password":   "pw",
		})
		resp.MustError(t, http.StatusForbidden, "M_USER_DISABLED")
	})

	t.Run("the local part stays reserved", func(t *testing.T) {
		nonce := srv.SharedSecretNonce(t)
		srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
			"nonce":    nonce,
			"username": "alice",
			"password": "pw",
			"mac":      harness.SharedSecretMAC(nonce, "alice", "pw", false),
		}).MustError(t, http.StatusBadRequest, "M_USER_IN_USE")
	})
}

func TestProfile(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "alice", "pw", false)
	bob := srv.Register(t, "bob", "pw", false)

	t.Run("a profile starts empty", func(t *testing.T) {
		resp := srv.Get(t, "/_matrix/client/v3/profile/"+url.PathEscape(alice.UserID)+"/displayname", bob.AccessToken).
			MustStatus(t, 200)
		var body struct {
			DisplayName string `json:"displayname"`
		}
		resp.JSON(t, &body)
		if body.DisplayName != "" {
			t.Errorf("displayname = %q, want empty", body.DisplayName)
		}
	})

	t.Run("a user can set and clear their display name", func(t *testing.T) {
		path := "/_matrix/client/v3/profile/" + url.PathEscape(alice.UserID) + "/displayname"
		srv.PutJSON(t, path, alice.AccessToken, map[string]any{"displayname": "Alice"}).
			MustStatus(t, 200)

		resp := srv.Get(t, path, bob.AccessToken).MustStatus(t, 200)
		var body struct {
			DisplayName string `json:"displayname"`
		}
		resp.JSON(t, &body)
		if body.DisplayName != "Alice" {
			t.Errorf("displayname = %q, want Alice", body.DisplayName)
		}

		// Clearing it is a write of the empty string, not a missing field.
		srv.PutJSON(t, path, alice.AccessToken, map[string]any{"displayname": ""}).MustStatus(t, 200)
		resp = srv.Get(t, path, bob.AccessToken).MustStatus(t, 200)
		resp.JSON(t, &body)
		if body.DisplayName != "" {
			t.Errorf("displayname = %q after clearing, want empty", body.DisplayName)
		}
	})

	t.Run("a user can set their avatar", func(t *testing.T) {
		path := "/_matrix/client/v3/profile/" + url.PathEscape(alice.UserID) + "/avatar_url"
		srv.PutJSON(t, path, alice.AccessToken, map[string]any{"avatar_url": "mxc://server/abc"}).
			MustStatus(t, 200)

		resp := srv.Get(t, path, bob.AccessToken).MustStatus(t, 200)
		var body struct {
			AvatarURL string `json:"avatar_url"`
		}
		resp.JSON(t, &body)
		if body.AvatarURL != "mxc://server/abc" {
			t.Errorf("avatar_url = %q", body.AvatarURL)
		}
	})

	t.Run("nobody else may write a profile", func(t *testing.T) {
		path := "/_matrix/client/v3/profile/" + url.PathEscape(alice.UserID) + "/displayname"
		resp := srv.PutJSON(t, path, bob.AccessToken, map[string]any{"displayname": "Mallory"})
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("bob rewrote alice's display name: %s", resp.Body)
		}
		resp.MustError(t, http.StatusForbidden, "M_FORBIDDEN")
	})

	t.Run("an unknown user is a 404", func(t *testing.T) {
		unknown := "@nobody:" + srv.ServerName
		srv.Get(t, "/_matrix/client/v3/profile/"+url.PathEscape(unknown)+"/displayname", alice.AccessToken).
			MustError(t, http.StatusNotFound, "M_NOT_FOUND")
	})
}

// registerSession drives the first half of the registration exchange and returns the session
// the server offered, which is what a real client echoes back with its chosen stage.
func registerSession(t *testing.T, srv *harness.Server, localpart, password string) string {
	t.Helper()
	resp := srv.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
		"username": localpart,
		"password": password,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first register returned %d, want the 401 challenge: %s", resp.StatusCode, resp.Body)
	}
	var challenge struct {
		Session string `json:"session"`
	}
	resp.JSON(t, &challenge)
	if challenge.Session == "" {
		t.Fatalf("challenge carried no session: %s", resp.Body)
	}
	return challenge.Session
}

// TestRegistrationViaClientAPI covers the standard flow Complement uses for non-admin
// users: a 401 challenge followed by m.login.dummy.
func TestRegistrationViaClientAPI(t *testing.T) {
	t.Parallel()

	srv := harness.New(t)

	t.Run("the first request is answered with a challenge", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
			"username": "challenge",
			"password": "pw",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401\nbody: %s", resp.StatusCode, resp.Body)
		}
		var challenge struct {
			Flows   []struct{ Stages []string } `json:"flows"`
			Session string                      `json:"session"`
		}
		resp.JSON(t, &challenge)
		if challenge.Session == "" {
			t.Error("challenge has no session")
		}
		if len(challenge.Flows) == 0 || len(challenge.Flows[0].Stages) == 0 {
			t.Fatalf("challenge has no flows: %s", resp.Body)
		}
	})

	t.Run("m.login.dummy completes the flow", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
			"username": "carol",
			"password": "pw",
			"auth":     map[string]any{"type": "m.login.dummy", "session": registerSession(t, srv, "carol", "pw")},
		})
		var body struct {
			UserID      string `json:"user_id"`
			AccessToken string `json:"access_token"`
			DeviceID    string `json:"device_id"`
		}
		resp.MustStatus(t, 200).JSON(t, &body)
		if body.UserID != "@carol:"+srv.ServerName || body.AccessToken == "" {
			t.Fatalf("unexpected registration result: %s", resp.Body)
		}
		srv.Get(t, "/_matrix/client/v3/account/whoami", body.AccessToken).MustStatus(t, 200)
	})

	// A client that already knows which stage it intends to satisfy does not need to be told which
	// stages exist. `m.login.dummy` asks for no credential at all, so a request carrying it has
	// already done everything this flow requires; making the client ask for a session first turns a
	// registration that could succeed into a 401. This is the shape complement's blueprint builder
	// and gomatrixserverlib's own client use, so the whole server-server suite could not stand its
	// homeservers up.
	t.Run("a session-less dummy stage completes the flow", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
			"username": "gina",
			"password": "pw",
			"auth":     map[string]any{"type": "m.login.dummy"},
		})
		var body struct {
			UserID      string `json:"user_id"`
			AccessToken string `json:"access_token"`
			DeviceID    string `json:"device_id"`
		}
		resp.MustStatus(t, 200).JSON(t, &body)
		if body.UserID != "@gina:"+srv.ServerName || body.AccessToken == "" || body.DeviceID == "" {
			t.Fatalf("unexpected registration result: %s", resp.Body)
		}
		srv.Get(t, "/_matrix/client/v3/account/whoami", body.AccessToken).MustStatus(t, 200)
	})

	// The same request with `inhibit_login` still creates the account and hands back no token.
	t.Run("a session-less dummy stage honours inhibit_login", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
			"username":      "henri",
			"password":      "pw",
			"auth":          map[string]any{"type": "m.login.dummy"},
			"inhibit_login": true,
		})
		var body struct {
			UserID      string `json:"user_id"`
			AccessToken string `json:"access_token"`
		}
		resp.MustStatus(t, 200).JSON(t, &body)
		if body.UserID != "@henri:"+srv.ServerName {
			t.Errorf("user_id = %q", body.UserID)
		}
		if body.AccessToken != "" {
			t.Errorf("inhibit_login returned an access token anyway: %s", resp.Body)
		}
	})

	// A session is what ties a completed stage to the request that asked for it, so an
	// `auth` dict naming a stage must name a live session too. Answering such a request by
	// starting over would let a client satisfy the stage of one registration with the
	// completed stage of another.
	t.Run("an unknown session is refused", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
			"username": "dave",
			"password": "pw",
			"auth":     map[string]any{"type": "m.login.dummy", "session": "not-a-session"},
		})
		body := resp.ErrorBody(t)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
		if body.Errcode != "M_SESSION_INVALID" {
			t.Errorf("errcode = %q, want M_SESSION_INVALID", body.Errcode)
		}
	})

	t.Run("a session with no stage is a challenge again", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
			"username": "dave",
			"password": "pw",
		})
		resp.MustStatus(t, http.StatusUnauthorized)
	})

	t.Run("inhibit_login returns the user ID and no token", func(t *testing.T) {
		resp := srv.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
			"username":      "erin",
			"password":      "pw",
			"inhibit_login": true,
			"auth":          map[string]any{"type": "m.login.dummy", "session": registerSession(t, srv, "erin", "pw")},
		})
		var body struct {
			UserID      string `json:"user_id"`
			AccessToken string `json:"access_token"`
		}
		resp.MustStatus(t, 200).JSON(t, &body)
		if body.UserID == "" {
			t.Fatalf("no user_id: %s", resp.Body)
		}
		if body.AccessToken != "" {
			t.Errorf("access_token = %q, want absent because inhibit_login was set", body.AccessToken)
		}
	})

	t.Run("registration can be closed", func(t *testing.T) {
		closed := harness.New(t, harness.WithClosedRegistration())
		resp := closed.PostJSON(t, "/_matrix/client/v3/register", "", map[string]any{
			"username": "nobody",
			"password": "pw",
		})
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("registration succeeded on a server with registration disabled: %s", resp.Body)
		}
	})
}

func TestRegisterAvailable(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "alice", "pw", false)

	for _, tc := range []struct {
		name        string
		username    string
		wantAvailbe bool
		wantCode    string
	}{
		{name: "free name", username: "bob", wantAvailbe: true},
		{name: "taken name", username: "alice", wantAvailbe: false, wantCode: "M_USER_IN_USE"},
		// Case folds, so an uppercase spelling of a taken name is still taken.
		{name: "taken name in another case", username: "ALICE", wantAvailbe: false, wantCode: "M_USER_IN_USE"},
		{name: "invalid name", username: "alice!", wantAvailbe: false, wantCode: "M_INVALID_USERNAME"},
		{name: "name with punctuation", username: "username,should_not_be_valid", wantAvailbe: false, wantCode: "M_INVALID_USERNAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The specification answers an unusable name with 400 and the reason in the
			// error body, so a client does not have to read a second field to explain itself.
			wantStatus := http.StatusOK
			if tc.wantCode != "" {
				wantStatus = http.StatusBadRequest
			}
			resp := srv.Get(t, "/_matrix/client/v3/register/available?username="+url.QueryEscape(tc.username), alice.AccessToken).
				MustStatus(t, wantStatus)
			var body struct {
				Available bool   `json:"available"`
				ErrCode   string `json:"errcode"`
			}
			resp.JSON(t, &body)
			if body.Available != tc.wantAvailbe {
				t.Errorf("available = %v, want %v", body.Available, tc.wantAvailbe)
			}
			if body.ErrCode != tc.wantCode {
				t.Errorf("errcode = %q, want %q", body.ErrCode, tc.wantCode)
			}
		})
	}

	t.Run("a missing username is a parameter error", func(t *testing.T) {
		srv.Get(t, "/_matrix/client/v3/register/available", alice.AccessToken).
			MustError(t, http.StatusBadRequest, "M_MISSING_PARAM")
	})
}

// TestLogoutDeletesTheDevice covers what /logout actually does to the device. Revoking the
// access token is only half of it: the session itself is gone, so a client listing the account's
// sessions must not see the one that was logged out — otherwise a user who signed out of a
// library computer still sees it in their own session list, with no way to tell it was logged out
// from one who simply never finished signing out.
func TestLogoutDeletesTheDevice(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "sleeper", "pw", false)
	elsewhere := srv.Login(t, "sleeper", "pw")

	resp := alice.MustGet(t, srv, "/_matrix/client/v3/devices")
	var listing struct {
		Devices []struct {
			DeviceID string `json:"device_id"`
		} `json:"devices"`
	}
	resp.JSON(t, &listing)
	if len(listing.Devices) != 2 {
		t.Fatalf("two sessions should be listed, got %d: %+v", len(listing.Devices), listing.Devices)
	}

	srv.PostJSON(t, "/_matrix/client/v3/logout", elsewhere.AccessToken, map[string]any{}).
		MustStatus(t, http.StatusOK)

	// The logged-out token is dead.
	srv.Get(t, "/_matrix/client/v3/account/whoami", elsewhere.AccessToken).
		MustError(t, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")

	// And the device is gone from the list the remaining session sees.
	resp = alice.MustGet(t, srv, "/_matrix/client/v3/devices")
	listing.Devices = nil
	resp.JSON(t, &listing)
	if len(listing.Devices) != 1 {
		t.Fatalf("the logged-out device is still listed: %+v", listing.Devices)
	}
	for _, device := range listing.Devices {
		if device.DeviceID == elsewhere.DeviceID {
			t.Fatalf("device %s survived its own logout: %+v", elsewhere.DeviceID, listing.Devices)
		}
	}

	// The device endpoint agrees, and asks for the logged-out device answer 404.
	alice.Do(t, srv, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/devices/" + harness.PathEscape(elsewhere.DeviceID),
	}).MustStatus(t, http.StatusNotFound)
}

// TestPasswordChangeKeepsSessionsWhenAsked covers the logout_devices flag. The default really is
// "log the others out", but a client changing the password on the user's behalf — from a
// password manager, or as part of a recovery flow — says false precisely because the user is
// about to be annoyed if their phone signs itself out.
func TestPasswordChangeKeepsSessionsWhenAsked(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "rotator", "first", false)
	phone := srv.Login(t, "rotator", "first")

	auth := func(password string) map[string]any {
		return map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]any{"type": "m.id.user", "user": "rotator"},
			"password":   password,
		}
	}

	srv.PostJSON(t, "/_matrix/client/v3/account/password", alice.AccessToken, map[string]any{
		"new_password":   "second",
		"logout_devices": false,
		"auth":           auth("first"),
	}).MustStatus(t, http.StatusOK)

	// Every session survives, including the one that did not make the request.
	srv.Get(t, "/_matrix/client/v3/account/whoami", alice.AccessToken).MustStatus(t, http.StatusOK)
	srv.Get(t, "/_matrix/client/v3/account/whoami", phone.AccessToken).MustStatus(t, http.StatusOK)

	// And the new password works everywhere.
	srv.Login(t, "rotator", "second")
}

// TestLoginWithRequestedDeviceID covers a client that names its own device. A client that has
// its device ID already — restoring a session on disk, or a login flow that wants a readable ID —
// asks for it, and expects the server to honour both the ID and the display name it supplied.
func TestLoginWithRequestedDeviceID(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "namer", "pw", false)

	resp := srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
		"type":                        "m.login.password",
		"identifier":                  map[string]any{"type": "m.id.user", "user": "namer"},
		"password":                    "pw",
		"device_id":                   "requested_device",
		"initial_device_display_name": "device display",
	})
	resp.MustStatus(t, http.StatusOK)
	var login struct {
		DeviceID string `json:"device_id"`
	}
	resp.JSON(t, &login)
	if login.DeviceID != "requested_device" {
		t.Fatalf("login ignored the requested device ID, answered %q", login.DeviceID)
	}

	// The device is readable by ID, with the display name the client asked for.
	device := alice.MustGet(t, srv, "/_matrix/client/v3/devices/requested_device")
	var body struct {
		DeviceID    string `json:"device_id"`
		DisplayName string `json:"display_name"`
	}
	device.JSON(t, &body)
	if body.DeviceID != "requested_device" || body.DisplayName != "device display" {
		t.Fatalf("GET /devices/requested_device = %+v", body)
	}

	// It is listed too.
	listing := alice.MustGet(t, srv, "/_matrix/client/v3/devices")
	listing.JSON(t, &struct {
		Devices []struct {
			DeviceID string `json:"device_id"`
		} `json:"devices"`
	}{})

	// A device ID nobody asked for is a 404, not an empty object.
	alice.Do(t, srv, harness.Request{Method: http.MethodGet, Path: "/_matrix/client/v3/devices/unknown_device"}).
		MustStatus(t, http.StatusNotFound)
}

// TestDeviceDeleteRequiresInteractiveAuth covers user-interactive authentication on
// DELETE /devices/{id}: what a client gets back when it has not authenticated yet.
//
// The distinction that matters is 401 versus 403. A 403 tells the client "you may not do this",
// which is true of every action the caller is not allowed to take; a 401 with a flow list tells
// it *how* to proceed. The caller here is perfectly allowed to delete their own session — they
// just have to prove they are who says they are, and the response is the instructions.
func TestDeviceDeleteRequiresInteractiveAuth(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(harness.RegistrationSecret))
	alice := srv.Register(t, "byedevice", "pw", false)
	// The target is a *different* session of the same account. Deleting the device that is
	// answering the request would revoke its own token, which is legal but makes the check
	// afterwards impossible.
	elsewhere := srv.Login(t, "byedevice", "pw")

	// A request with no body at all: the client has offered nothing, so it is told what to
	// offer. A body is not "missing" in a way that makes the request malformed.
	resp := alice.Do(t, srv, harness.Request{
		Method: http.MethodDelete,
		Path:   "/_matrix/client/v3/devices/" + harness.PathEscape(elsewhere.DeviceID),
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("DELETE /devices without a body = %d %s, want 401 with a UIA challenge",
			resp.StatusCode, resp.Body)
	}
	challenge := jsonBodyOf(t, resp)
	for _, key := range []string{"session", "flows", "params"} {
		if _, ok := challenge[key]; !ok {
			t.Errorf("the challenge is missing %q: %v", key, challenge)
		}
	}

	// A wrong password is also a challenge — the same session, now explaining why the stage
	// did not complete — rather than a refusal.
	resp = alice.Do(t, srv, harness.Request{
		Method: http.MethodDelete,
		Path:   "/_matrix/client/v3/devices/" + harness.PathEscape(elsewhere.DeviceID),
		Body: map[string]any{
			"auth": map[string]any{
				"type":       "m.login.password",
				"identifier": map[string]any{"type": "m.id.user", "user": alice.UserID},
				"password":   "not my password",
			},
		},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("DELETE /devices with a wrong password = %d %s, want 401", resp.StatusCode, resp.Body)
	}
	failed := jsonBodyOf(t, resp)
	for _, key := range []string{"flows", "params", "session", "error", "errcode"} {
		if _, ok := failed[key]; !ok {
			t.Errorf("the failed-stage challenge is missing %q: %v", key, failed)
		}
	}
	if failed["errcode"] != "M_FORBIDDEN" {
		t.Errorf("a failed password stage should answer M_FORBIDDEN, got %v", failed["errcode"])
	}

	// The device of a refused request is still there.
	alice.MustGet(t, srv, "/_matrix/client/v3/devices/"+harness.PathEscape(elsewhere.DeviceID)).
		MustStatus(t, http.StatusOK)

	// And the right password deletes it.
	alice.Do(t, srv, harness.Request{
		Method: http.MethodDelete,
		Path:   "/_matrix/client/v3/devices/" + harness.PathEscape(elsewhere.DeviceID),
		Body: map[string]any{
			"auth": map[string]any{
				"type":       "m.login.password",
				"identifier": map[string]any{"type": "m.id.user", "user": alice.UserID},
				"password":   "pw",
			},
		},
	}).MustStatus(t, http.StatusOK)
	alice.Do(t, srv, harness.Request{
		Method: http.MethodGet,
		Path:   "/_matrix/client/v3/devices/" + harness.PathEscape(elsewhere.DeviceID),
	}).MustStatus(t, http.StatusNotFound)
}
