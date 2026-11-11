package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/tidwall/gjson"
)

// TestOnlyTheRoomVersionsItAdvertisesCanBeCreated keeps a server's promise and its behaviour in the
// same file.
//
// `GET /capabilities` is what a client reads before it offers a version to pick, so an entry there is
// a promise: "ask for this and you will get a room". The set used to be whatever the room-version
// library called stable, which included version 12 — a version in which a room's ID carries no
// domain, which this server cannot issue because it names rooms `!id:its.own.domain`. The client was
// told yes, the create event was built, and the authorisation layer refused it with a 403 quoting an
// internal check.
//
// Refusing is not a regression: the specification has an error for exactly this
// (`M_INCOMPATIBLE_ROOM_VERSION`), and a refusal that arrives before the room is half-built is what a
// client can act on.
func TestOnlyTheRoomVersionsItAdvertisesCanBeCreated(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "alice", "pw-alice", false)

	response := alice.MustGet(t, server, "/_matrix/client/v3/capabilities")
	var envelope struct {
		Capabilities struct {
			RoomVersions struct {
				Default   string            `json:"default"`
				Available map[string]string `json:"available"`
			} `json:"m.room_versions"`
		} `json:"capabilities"`
	}
	response.JSON(t, &envelope)
	capabilities := envelope.Capabilities
	if len(capabilities.RoomVersions.Available) == 0 {
		t.Fatalf("the server advertises no room versions at all: %s", response.Body)
	}
	if _, ok := capabilities.RoomVersions.Available[capabilities.RoomVersions.Default]; !ok {
		t.Errorf("the default room version %q is not in the advertised set %v",
			capabilities.RoomVersions.Default, capabilities.RoomVersions.Available)
	}

	// Every advertised version has to actually produce a room. This is the whole test: a list a
	// client trusts and a server that cannot honour one entry of it is worse than a shorter list.
	for version := range capabilities.RoomVersions.Available {
		created := alice.Do(t, server, harness.Request{
			Method: http.MethodPost,
			Path:   "/_matrix/client/v3/createRoom",
			Body:   map[string]any{"preset": "private_chat", "room_version": version},
		})
		if created.StatusCode != http.StatusOK {
			t.Errorf("version %s was advertised as available but createRoom answered %d: %s",
				version, created.StatusCode, created.Body)
		}
	}

	// And a version this server cannot build is refused before anything is written, with the code
	// that means "not here" rather than a failure from inside the event graph.
	unsupported := alice.Do(t, server, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/client/v3/createRoom",
		Body:   map[string]any{"preset": "private_chat", "room_version": "12"},
	})
	if unsupported.StatusCode == http.StatusOK {
		t.Skip("this server can build version 12 rooms; the refusal is no longer owed")
	}
	if code := gjson.GetBytes(unsupported.Body, "errcode").Str; code != "M_INCOMPATIBLE_ROOM_VERSION" {
		t.Errorf("version 12 answered %d %q, want 400 M_INCOMPATIBLE_ROOM_VERSION: %s",
			unsupported.StatusCode, code, unsupported.Body)
	}
	if body := string(unsupported.Body); strings.Contains(body, "eventauth") {
		t.Errorf("the refusal leaks an internal check: %s", body)
	}
}
