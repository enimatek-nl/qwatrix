package api_test

import (
	"net/http"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// TestAKnownEndpointRefusesTheWrongMethod covers a distinction that looks pedantic until it is load
// bearing: "nothing is here" and "that is here, but not that way" are different facts, and a client that
// hears the first one stops trying in a way the second one would not have stopped it.
//
// It is also the difference between a server that has a `/login` and one that does not. `404` for a
// mistyped method is the answer a proxy, a reverse proxy with a route typo, or a client sending a PUT
// instead of a POST all receive, and none of them can tell the difference from this side.
func TestAKnownEndpointRefusesTheWrongMethod(t *testing.T) {
	server := harness.New(t)

	for _, wrong := range []struct {
		method, path string
		allow        string
	}{
		{http.MethodPut, "/_matrix/client/v3/login", "GET,HEAD,POST"},
		{http.MethodPatch, "/_matrix/media/v3/upload", "POST"},
		{http.MethodDelete, "/_matrix/client/v3/sync", "GET,HEAD"},
		// The version string is public information about this server, so it is answered on this
		// listener too — and a wrong method against it is a wrong method, not a missing endpoint.
		{http.MethodPut, "/_matrix/federation/v1/version", "GET,HEAD"},
		// The key query endpoint is a server-server endpoint served on this listener as well, and it
		// is a POST: a wrong method against it is the same distinction as anywhere else.
		{http.MethodPut, "/_matrix/key/v2/query", "POST"},
	} {
		response := server.Do(t, harness.Request{Method: wrong.method, Path: wrong.path})
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 405 (the path is known, the method is not)\nbody: %s",
				wrong.method, wrong.path, response.StatusCode, response.Body)
			continue
		}
		var body struct {
			Errcode string `json:"errcode"`
		}
		response.JSON(t, &body)
		if body.Errcode != "M_UNRECOGNIZED" {
			t.Errorf("%s %s: errcode %q, want M_UNRECOGNIZED: a Matrix client reads the errcode, not the status",
				wrong.method, wrong.path, body.Errcode)
		}
		if allowed := response.Header.Get("Allow"); allowed != wrong.allow {
			t.Errorf("%s %s: Allow = %q, want %q — the header exists so a client can correct itself",
				wrong.method, wrong.path, allowed, wrong.allow)
		}
	}
}

// TestAnUnknownEndpointIsStillUnrecognized is the other half: a path this server does not have stays a
// 404. Turning everything into 405 would be as misleading as the reverse, and would advertise endpoints
// that do not exist.
func TestAnUnknownEndpointIsStillUnrecognized(t *testing.T) {
	server := harness.New(t)

	for _, path := range []string{
		"/_matrix/unknown",
		"/_matrix/client/v3/unknown",
		"/_matrix/client/v1/unknown/thing",
		"/_matrix/media/v3/unknown",
		"/_matrix/federation/v1/unknown",
	} {
		response := server.Get(t, path, "")
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, response.StatusCode)
		}
		var body struct {
			Errcode string `json:"errcode"`
		}
		response.JSON(t, &body)
		if body.Errcode != "M_UNRECOGNIZED" {
			t.Errorf("GET %s: errcode %q, want M_UNRECOGNIZED", path, body.Errcode)
		}
	}
}
