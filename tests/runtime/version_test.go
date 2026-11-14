package runtime_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/config"
)

// TestAStampedBuildReportsTheStampItWasGiven proves that the release workflow's
// `-X main.version=…` lands somewhere.
//
// This is the failure the workflow's own tag check cannot see: `go build -X` for a symbol
// that does not exist is not an error. A rename in cmd/qwatrix, or a variable that quietly
// stops being read, and every published container reports the source tree's development
// default while being tagged something else — for release after release, with nothing
// failing. The tag check compares a tag to a file; only this compares a tag to the
// behaviour of the binary it built.
func TestAStampedBuildReportsTheStampItWasGiven(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("find the module root: %v", err)
	}
	const stamp = "9.9.9-stamped-for-a-test"
	binary := filepath.Join(t.TempDir(), "qwatrix")
	build := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-s -w -X main.version="+stamp, "-o", binary, "./cmd/qwatrix")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build a stamped binary: %v\n%s", err, out)
	}

	// The usage banner is the cheapest place the version surfaces: it needs no server
	// name, no data directory, no port, and no write lock on anything.
	out, err := exec.Command(binary, "-h").CombinedOutput()
	if err != nil && !strings.Contains(string(out), "Usage:") {
		t.Fatalf("run the stamped binary: %v\n%s", err, out)
	}
	usage := string(out)
	if !strings.Contains(usage, "qwatrix "+stamp) {
		t.Fatalf("a binary stamped %q does not report it:\n%s", stamp, usage)
	}
	if strings.Contains(usage, config.Default().Version) {
		t.Fatalf("the compiled-in default overrode the stamp %q:\n%s", stamp, usage)
	}
}

// TestTheCompiledInVersionIsOneATagCanName keeps the release workflow's guard from being
// the only thing standing between a bad tag and a published image.
//
// The workflow derives the publishable version from this string, so a version that is not
// `MAJOR.MINOR.PATCH` with an optional `-suffix` cannot produce a semver tag, and
// `docker/metadata-action` refuses a tag it cannot parse: the release fails in a way that
// looks like the publisher's fault. Catching it in the tree means it fails as a test.
func TestTheCompiledInVersionIsOneATagCanName(t *testing.T) {
	version := config.Default().Version
	// A leading "v" here would be the tag's business, not the version's: the workflow
	// strips it, and a version carrying one would be stripped twice.
	pattern := `^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$`
	if !regexp.MustCompile(pattern).MatchString(version) {
		t.Fatalf("config.Default().Version is %q, which is not a version a release tag can name (%s)",
			version, pattern)
	}
}

// TestTheImageTakesItsVersionFromTheSource is the other half of not stating the same fact
// twice.
//
// deploy/Dockerfile used to carry `ARG VERSION=0.1.0-dev` next to config.go's own
// `Version: "0.1.0-dev"`. Those two agreed exactly as long as somebody remembered to edit
// both, and an image whose reported version comes from a build argument nobody changed is
// the drift this repository's release workflow exists to prevent. So the default has to be
// read from the source, and the only way to hold that line is a test: a literal default
// looks like a tidy cleanup right up to the release where it is wrong.
func TestTheImageTakesItsVersionFromTheSource(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("find the module root: %v", err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "deploy", "Dockerfile"))
	if err != nil {
		t.Fatalf("read deploy/Dockerfile: %v", err)
	}
	text := string(dockerfile)
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "ARG VERSION") {
			continue
		}
		if strings.Contains(trimmed, "=") && !strings.HasSuffix(trimmed, "=") {
			t.Fatalf("deploy/Dockerfile gives VERSION a literal default (%q); the version lives in "+
				"internal/config/config.go and the image reads it from there", trimmed)
		}
	}
	if !strings.Contains(text, "internal/config/config.go") {
		t.Fatal("deploy/Dockerfile no longer reads the version out of internal/config/config.go, " +
			"so it is stating the version again somewhere else")
	}
}
