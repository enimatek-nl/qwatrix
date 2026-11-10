package api_test

import (
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// TestVersionsIsPublicAndWellFormed is the tracer bullet: the cheapest request a Matrix
// client (and Complement itself) makes, and the one Complement uses to decide a homeserver
// is up. It needs no account, so it proves routing, middleware and the JSON contract work
// end to end before any feature is layered on.
func TestVersionsIsPublicAndWellFormed(t *testing.T) {
	t.Parallel()

	srv := harness.New(t)

	resp := srv.Get(t, "/_matrix/client/versions", "").MustStatus(t, 200)
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want exactly application/json", got)
	}

	var body struct {
		Versions         []string        `json:"versions"`
		UnstableFeatures map[string]bool `json:"unstable_features"`
	}
	resp.JSON(t, &body)

	if len(body.Versions) == 0 {
		t.Fatal("versions is empty; the spec requires the list of supported versions")
	}
	// v1.11 is the newest client-server API version defined by specification release
	// v1.19; a client picks the newest entry it understands, so both ends of the list
	// matter.
	for _, want := range []string{"v1.0", "v1.11"} {
		if !contains(body.Versions, want) {
			t.Errorf("versions %v does not advertise %s", body.Versions, want)
		}
	}
	for _, version := range body.Versions {
		if len(version) < 4 || version[:2] != "v1" || version[3] < '0' {
			t.Errorf("version %q is not of the form v1.x", version)
		}
	}
}

// TestVersionsIsServedOnLegacyPrefixes covers the compatibility aliases: clients written
// against r0 still ask for it.
func TestVersionsIsServedOnLegacyPrefixes(t *testing.T) {
	t.Parallel()

	srv := harness.New(t)

	// /versions has no versioned prefix in the specification, but /versions does exist
	// under each alias in real deployments; the endpoint that matters is the single one.
	srv.Get(t, "/_matrix/client/versions", "").MustStatus(t, 200)
}

// TestUnknownEndpoints covers the specification's contract for "this server does not speak
// that": a Matrix error body with M_UNRECOGNIZED, not an HTML 404.
func TestUnknownEndpoints(t *testing.T) {
	t.Parallel()

	srv := harness.New(t)

	for _, tc := range []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{name: "unknown client endpoint", method: "GET", path: "/_matrix/client/v3/does_not_exist", wantStatus: 404, wantCode: "M_UNRECOGNIZED"},
		{name: "unknown matrix endpoint", method: "GET", path: "/_matrix/nope", wantStatus: 404, wantCode: "M_UNRECOGNIZED"},
		{name: "unknown synapse endpoint", method: "GET", path: "/_synapse/admin/v1/nope", wantStatus: 404, wantCode: "M_UNRECOGNIZED"},
		{name: "unknown admin endpoint", method: "POST", path: "/_synapse/admin/v2/nope", wantStatus: 404, wantCode: "M_UNRECOGNIZED"},
		{name: "non matrix path is a plain 404", method: "GET", path: "/nothing", wantStatus: 404, wantCode: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := srv.Do(t, harness.Request{Method: tc.method, Path: tc.path})
			if tc.wantCode == "" {
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.wantStatus, resp.Body)
				}
				// A request that never spoke Matrix gets no Matrix vocabulary back.
				if got := resp.Header.Get("Content-Type"); got == "application/json" {
					t.Errorf("non-Matrix path answered with a JSON Matrix error: %s", resp.Body)
				}
				return
			}
			resp.MustError(t, tc.wantStatus, tc.wantCode)
		})
	}
}

// contains is the one-line helper the version assertions need without importing slices for
// a single call.
func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
