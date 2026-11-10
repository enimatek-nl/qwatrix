package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// The tests here cover the registration path Complement uses for nearly every test it
// runs: Synapse-compatible shared-secret registration. Getting the HMAC recipe wrong does
// not fail loudly — it silently turns hundreds of tests into skips, which is precisely the
// kind of green that means nothing.

const registrationSecret = "complement"

// TestSharedSecretRegistration walks the whole exchange: nonce, valid MAC, and every way a
// client can get it wrong.
func TestSharedSecretRegistration(t *testing.T) {
	t.Parallel()

	srv := harness.New(t, harness.WithRegistrationSecret(registrationSecret))

	t.Run("valid mac creates an account and returns a token", func(t *testing.T) {
		nonce := srv.SharedSecretNonce(t)
		resp := srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
			"nonce":    nonce,
			"username": "alice",
			"password": "wonderland",
			"mac":      harness.SharedSecretMAC(nonce, "alice", "wonderland", false),
			"admin":    false,
		})

		var body struct {
			UserID      string `json:"user_id"`
			AccessToken string `json:"access_token"`
			DeviceID    string `json:"device_id"`
		}
		resp.MustStatus(t, http.StatusOK).JSON(t, &body)

		if want := "@alice:" + srv.ServerName; body.UserID != want {
			t.Errorf("user_id = %q, want %q", body.UserID, want)
		}
		if body.AccessToken == "" {
			t.Error("access_token is empty; the caller cannot authenticate with this account")
		}
		if body.DeviceID == "" {
			t.Error("device_id is empty; every Matrix account has a device")
		}
	})

	t.Run("the account can log in with the same password", func(t *testing.T) {
		account := srv.Register(t, "bobs", "fish", false)
		if account.UserID != "@bobs:"+srv.ServerName {
			t.Errorf("user_id = %q", account.UserID)
		}
		srv.Get(t, "/_matrix/client/v3/account/whoami", account.AccessToken).
			MustStatus(t, http.StatusOK)
	})

	t.Run("an administrator account is created", func(t *testing.T) {
		nonce := srv.SharedSecretNonce(t)
		resp := srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
			"nonce":    nonce,
			"username": "root",
			"password": "toor",
			"mac":      harness.SharedSecretMAC(nonce, "root", "toor", true),
			"admin":    true,
		})
		// The admin flag is not visible on the client API; what this asserts is that
		// asking for an administrator does not break the exchange, and that the MAC of an
		// admin registration (which hashes the literal "admin") is accepted.
		resp.MustStatus(t, http.StatusOK)
	})

	t.Run("a taken localpart is refused", func(t *testing.T) {
		srv.Register(t, "dupe", "first", false)

		nonce := srv.SharedSecretNonce(t)
		resp := srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
			"nonce":    nonce,
			"username": "dupe",
			"password": "second",
			"mac":      harness.SharedSecretMAC(nonce, "dupe", "second", false),
		})
		resp.MustError(t, http.StatusBadRequest, "M_USER_IN_USE")
	})

	// The shared-secret protocol hashes the username as sent but stores it downcased, and
	// clients depend on that: an admin script that types --user=Alice means @alice. This is
	// the opposite of POST /register, which rejects an uppercase request outright, and the
	// asymmetry is deliberate — one endpoint takes what a human typed, the other is a
	// protocol exchange where a surprise local part is worse than an error.
	t.Run("an uppercase username is downcased", func(t *testing.T) {
		const username = "MIXED-Case-SHARED-SECRET"
		nonce := srv.SharedSecretNonce(t)
		resp := srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
			"nonce":    nonce,
			"username": username,
			"password": "pw",
			"mac":      harness.SharedSecretMAC(nonce, username, "pw", false),
		})
		resp.MustStatus(t, http.StatusOK)
		var body struct {
			UserID string `json:"user_id"`
		}
		resp.JSON(t, &body)
		if want := "@" + strings.ToLower(username) + ":" + harness.ServerName; body.UserID != want {
			t.Errorf("user_id = %q, want %q", body.UserID, want)
		}
		if account := srv.Login(t, strings.ToLower(username), "pw"); account.UserID != "@"+strings.ToLower(username)+":"+harness.ServerName {
			t.Errorf("the downcased account cannot log in, got %#v", account)
		}
	})

	t.Run("an invalid localpart is refused", func(t *testing.T) {
		for _, tc := range []struct{ name, username string }{
			{name: "empty", username: ""},
			{name: "contains a colon", username: "alice:evil"},
			{name: "contains an at sign", username: "alice@evil"},
			{name: "illegal punctuation", username: "alice!"},
			{name: "non-ascii", username: "ättila"},
			{name: "longer than the specification allows", username: strings.Repeat("a", 300)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				nonce := srv.SharedSecretNonce(t)
				resp := srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
					"nonce":    nonce,
					"username": tc.username,
					"password": "pw",
					"mac":      harness.SharedSecretMAC(nonce, tc.username, "pw", false),
				})
				resp.MustError(t, http.StatusBadRequest, "M_INVALID_USERNAME")
			})
		}
	})

	t.Run("a forged mac is refused", func(t *testing.T) {
		nonce := srv.SharedSecretNonce(t)
		resp := srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
			"nonce":    nonce,
			"username": "mallory",
			"password": "letmein",
			"mac":      strings.Repeat("00", 20),
		})
		resp.MustError(t, http.StatusUnauthorized, "M_UNAUTHORIZED")

		// And the account really was not created.
		srv.PostJSON(t, "/_matrix/client/v3/login", "", map[string]any{
			"type":       "m.login.password",
			"identifier": map[string]any{"type": "m.id.user", "user": "mallory"},
			"password":   "letmein",
		}).MustError(t, http.StatusForbidden, "M_FORBIDDEN")
	})

	t.Run("a mac computed with the wrong secret is refused", func(t *testing.T) {
		nonce := srv.SharedSecretNonce(t)
		resp := srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
			"nonce":    nonce,
			"username": "eve",
			"password": "pw",
			// Correct recipe, wrong key.
			"mac": wrongSecretMAC(nonce, "eve", "pw"),
		})
		resp.MustError(t, http.StatusUnauthorized, "M_UNAUTHORIZED")
	})

	t.Run("a consumed nonce cannot be replayed", func(t *testing.T) {
		nonce := srv.SharedSecretNonce(t)
		payload := map[string]any{
			"nonce":    nonce,
			"username": "replay",
			"password": "pw",
			"mac":      harness.SharedSecretMAC(nonce, "replay", "pw", false),
		}
		srv.PostJSON(t, "/_synapse/admin/v1/register", "", payload).MustStatus(t, http.StatusOK)

		// Same nonce, same MAC: if this were accepted, a captured registration request
		// would be replayable for the nonce's whole lifetime.
		srv.PostJSON(t, "/_synapse/admin/v1/register", "", payload).
			MustError(t, http.StatusUnauthorized, "M_UNAUTHORIZED")
	})

	t.Run("an unknown nonce is refused", func(t *testing.T) {
		foreign := "not-a-nonce-we-issued"
		resp := srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
			"nonce":    foreign,
			"username": "ghost",
			"password": "pw",
			"mac":      harness.SharedSecretMAC(foreign, "ghost", "pw", false),
		})
		resp.MustError(t, http.StatusUnauthorized, "M_UNAUTHORIZED")
	})
}

// TestSharedSecretRegistrationDisabledIsNotFound covers the other half of the contract:
// with no secret configured the endpoint must 404, because that is the answer Complement
// treats as "skip these tests" rather than "fail them".
func TestSharedSecretRegistrationDisabledIsNotFound(t *testing.T) {
	t.Parallel()

	srv := harness.New(t)

	srv.Get(t, "/_synapse/admin/v1/register", "").MustError(t, http.StatusNotFound, "M_UNRECOGNIZED")
	srv.PostJSON(t, "/_synapse/admin/v1/register", "", map[string]any{
		"nonce": "x", "username": "a", "password": "b", "mac": "c",
	}).MustError(t, http.StatusNotFound, "M_UNRECOGNIZED")
}

// wrongSecretMAC computes the shared-secret registration MAC with a deliberately wrong
// key, for the test that must distinguish "right recipe, wrong secret" from "wrong
// recipe".
func wrongSecretMAC(nonce, user, password string) string {
	return harness.SharedSecretMACWithSecret("not-the-configured-secret", nonce, user, password, false)
}
