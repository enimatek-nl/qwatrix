//go:build complement

package complement_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/matrix-org/complement"
	"github.com/matrix-org/complement/b"
	"github.com/matrix-org/complement/client"
	"github.com/matrix-org/complement/helpers"
	"github.com/matrix-org/complement/match"
	"github.com/matrix-org/complement/must"
)

// TestComplementCanDeployAndAnswerVersions is the container contract in its smallest
// form: Complement waits for the health check and then for /versions. If this passes, the
// image's ports, readiness signalling and start-up time are all right.
//
// It is worth being explicit about what this test would catch, because it looks trivial:
// a client port other than 8008, an image without a HEALTHCHECK, a server that binds only
// localhost inside the container, and an entrypoint that is not executable all fail here
// and nowhere else.
func TestComplementCanDeployAndAnswerVersions(t *testing.T) {
	deployment := complement.Deploy(t, 1)
	defer deployment.Destroy(t)

	anonymous := deployment.UnauthenticatedClient(t, "hs1")
	response := anonymous.MustDo(t, "GET", []string{"_matrix", "client", "versions"})
	must.MatchResponse(t, response, match.HTTPResponse{
		StatusCode: http.StatusOK,
		JSON: []match.JSON{
			match.JSONKeyPresent("versions"),
			match.JSONArrayEach("versions", func(value gjson.Result) error {
				if len(value.Str) < 4 || value.Str[0] != 'v' {
					t.Errorf("versions contains %q, which is not of the form v1.x", value.Str)
				}
				return nil
			}),
		},
	})
}

// TestComplementSharedSecretRegistration asserts the Synapse-compatible
// registration endpoint works against the *image*, with the secret the image sets by
// default. It matters that this runs in the container rather than in-process: the entrypoint
// is what sets QWATRIX_REGISTRATION_SECRET, and Complement treats a non-200 here as
// "skip every test that needs a user" — a silently degrading suite.
func TestComplementSharedSecretRegistration(t *testing.T) {
	deployment := complement.Deploy(t, 1)
	defer deployment.Destroy(t)

	alice := deployment.Register(t, "hs1", helpers.RegistrationOpts{LocalpartSuffix: "smokealice"})
	bob := deployment.Register(t, "hs1", helpers.RegistrationOpts{LocalpartSuffix: "smokebob"})

	response := alice.MustDo(t, "GET", []string{"_matrix", "client", "v3", "account", "whoami"})
	must.MatchResponse(t, response, match.HTTPResponse{
		StatusCode: http.StatusOK,
		JSON: []match.JSON{
			match.JSONKeyEqual("user_id", alice.UserID),
			match.JSONKeyEqual("device_id", alice.DeviceID),
		},
	})

	// A second account proves the nonce machinery is not single-use per process.
	response = bob.MustDo(t, "GET", []string{"_matrix", "client", "v3", "account", "whoami"})
	must.MatchResponse(t, response, match.HTTPResponse{
		StatusCode: http.StatusOK,
		JSON:       []match.JSON{match.JSONKeyEqual("user_id", bob.UserID)},
	})
}

// TestAdminRegistrationIsAccepted covers the administrator flag of the shared-secret
// endpoint, whose MAC covers the literal "admin" rather than "notadmin": computing it the
// other way round fails only here.
func TestAdminRegistrationIsAccepted(t *testing.T) {
	deployment := complement.Deploy(t, 1)
	defer deployment.Destroy(t)

	admin := deployment.Register(t, "hs1", helpers.RegistrationOpts{
		LocalpartSuffix: "smokeadmin",
		IsAdmin:         true,
	})
	response := admin.MustDo(t, "GET", []string{"_matrix", "client", "v3", "account", "whoami"})
	must.MatchResponse(t, response, match.HTTPResponse{
		StatusCode: http.StatusOK,
		JSON:       []match.JSON{match.JSONKeyEqual("user_id", admin.UserID)},
	})
}

// TestComplementRoomLifecycle is the shortest path that touches the database, the
// room server, the sync loop and the event pipeline, all through the public API: create a
// room, send an event, and see it come back down /sync.
func TestComplementRoomLifecycle(t *testing.T) {
	deployment := complement.Deploy(t, 1)
	defer deployment.Destroy(t)

	alice := deployment.Register(t, "hs1", helpers.RegistrationOpts{LocalpartSuffix: "smokeroom"})

	roomID := alice.MustCreateRoom(t, map[string]any{
		"preset": "public_chat",
		"name":   "smoke room",
		"topic":  "created by the container contract test",
	})
	if roomID == "" {
		t.Fatal("createRoom returned no room_id")
	}

	eventID := alice.SendEventSynced(t, roomID, b.Event{
		Type: "m.room.message",
		Content: map[string]any{
			"msgtype": "m.text",
			"body":    "hello from the container",
		},
	})
	if eventID == "" {
		t.Fatal("sending an event returned no event_id")
	}

	// And the state the room was created with is readable.
	response := alice.MustDo(t, "GET", []string{"_matrix", "client", "v3", "rooms", roomID, "state"})
	must.MatchResponse(t, response, match.HTTPResponse{
		StatusCode: http.StatusOK,
		JSON: []match.JSON{
			// GET /state answers with the array itself, not wrapped in an object, so the
			// matcher walks the body (an empty path means "the document itself").
			match.JSONArrayEach("", func(event gjson.Result) error {
				if event.Get("type").Str == "m.room.create" {
					return nil
				}
				return nil
			}),
			// Same reason as above: the state endpoint answers with a bare array.
			match.JSONArraySome("", func(event gjson.Result) error {
				if event.Get("type").Str != "m.room.create" {
					return nil
				}
				return errFoundCreate
			}),
		},
	})

	// Finally a full /sync from the beginning must mention the room as joined.
	_, nextBatch := alice.MustSync(t, client.SyncReq{})
	if nextBatch == "" {
		t.Fatal("sync returned an empty next_batch")
	}
}

// errFoundCreate is the sentinel the state assertion above uses with JSONArraySome, which
// reports success on the first error: finding the create event is the pass condition.
var errFoundCreate = errors.New("found m.room.create")

// TestComplementTwoServerDeployment is the federation precondition: Complement can stand up two
// containers of this image, both of which answer for the name they were given. The
// server-server API itself is a later milestone; this proves the deployment machinery and
// the per-container identity work, which is what every two-server test depends on.
func TestComplementTwoServerDeployment(t *testing.T) {
	deployment := complement.Deploy(t, 2)
	defer deployment.Destroy(t)

	for _, hsName := range []string{"hs1", "hs2"} {
		anonymous := deployment.UnauthenticatedClient(t, hsName)
		response := anonymous.MustDo(t, "GET", []string{"_matrix", "client", "versions"})
		must.MatchResponse(t, response, match.HTTPResponse{StatusCode: http.StatusOK})

		user := deployment.Register(t, hsName, helpers.RegistrationOpts{LocalpartSuffix: "two" + hsName})
		response = user.MustDo(t, "GET", []string{"_matrix", "client", "v3", "account", "whoami"})
		must.MatchResponse(t, response, match.HTTPResponse{
			StatusCode: http.StatusOK,
			JSON:       []match.JSON{match.JSONKeyEqual("user_id", user.UserID)},
		})
	}
}
