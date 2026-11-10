//go:build complement

// Package complement_test is qwatrix's own Complement suite: the out-of-repo pattern the
// Complement documentation describes, where a homeserver implementation keeps its own
// tests against the framework instead of only running upstream's.
//
// These are the tests that assert the *container contract* — the rules Complement imposes
// on an image, which no in-process test can check. They are the first thing to run after
// changing the image, the entrypoint, or the port/registration assumptions.
//
// Run them with:
//
//	make complement-smoke
//
// which is `go test -tags complement` with COMPLEMENT_BASE_IMAGE pointing at the built
// image. Without the build tag they are invisible to `go test ./...`, so the ordinary test
// suite never needs a container runtime.
package complement_test

import (
	"testing"

	"github.com/matrix-org/complement"
)

// TestMain bootstraps Complement for this package. The namespace must be unique among
// Complement packages that may run in parallel; "qwatrix" is ours.
func TestMain(m *testing.M) {
	complement.TestMain(m, "qwatrix")
}
