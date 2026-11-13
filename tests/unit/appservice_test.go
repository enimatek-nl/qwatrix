package unit_test

// An application service is a program the homeserver has been told to trust: it registers a user ID
// namespace, and the server lets it act as those users.
//
// The trust is the whole design, so the parsing that establishes it is worth testing on its own: a
// service that is read with the wrong token is a back door, and a namespace that matches by substring
// is a namespace that matches more users than its operator agreed to.
//
// The file format is Synapse's, because that is the format every existing application service
// registration is written in and the format complement hands to the container.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/appservice"
)

const bridgeRegistration = `id: my_as_id
hs_token: hs-secret-token
as_token: as-secret-token
url: 'http://localhost:9000'
sender_localpart: the-bridge-user
rate_limited: false
namespaces:
  users:
    - exclusive: false
      regex: .*
  rooms: []
  aliases: []
`

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestAnApplicationServiceRegistrationIsRead(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "my_as_id.yaml"), bridgeRegistration)

	registry, err := appservice.Load([]string{path}, nil)
	if err != nil {
		t.Fatalf("loading the registration: %v", err)
	}
	service, ok := registry.LookupByToken("as-secret-token")
	if !ok {
		t.Fatalf("the as_token did not identify the service (services: %d)", registry.Len())
	}
	if got := service.UserID("test.local"); got != "@the-bridge-user:test.local" {
		t.Errorf("the service's sender user is %q, want @the-bridge-user:test.local", got)
	}
	if !service.AllowsUser("@the-bridge-user:test.local") {
		t.Error("a service may always act as its own sender user")
	}
	if service.URL != "http://localhost:9000" {
		t.Errorf("url = %q, want the address complement's mock service listens on", service.URL)
	}
	// The homeserver token is the other half of the pair: it is what the service presents when
	// this server calls it, so reading it wrong would break the callback rather than the API.
	if service.HSToken != "hs-secret-token" {
		t.Errorf("hs_token = %q, want hs-secret-token", service.HSToken)
	}
}

func TestAConfiguredNamespaceDoesNotMatchBySubstring(t *testing.T) {
	dir := t.TempDir()
	// A namespace naming one user is a statement about that user. Patterns are matched against the
	// whole user ID, anchored, because an unanchored match is a namespace that also covers alice-evil,
	// alice_admin and anybody else whose ID happens to contain the string — a wider grant than anybody
	// agreed to.
	//
	// The pattern is quoted, and not for style: "@" is a reserved indicator character in YAML, so a
	// user-ID regex simply cannot be written as a plain scalar. A loader that only coped with bare
	// words would reject the most common namespace there is.
	path := writeFile(t, filepath.Join(dir, "narrow.yaml"), `id: narrow
as_token: narrow-token
sender_localpart: bridge
namespaces:
  users:
    - exclusive: true
      regex: '@alice:test\.local'
`)
	registry, err := appservice.Load([]string{path}, nil)
	if err != nil {
		t.Fatalf("loading the registration: %v", err)
	}
	service, ok := registry.LookupByToken("narrow-token")
	if !ok {
		t.Fatal("the service was not found by its token")
	}
	if !service.AllowsUser("@alice:test.local") {
		t.Error("the named user is not allowed by its own namespace")
	}
	for _, userID := range []string{"@alice-evil:test.local", "@alice_admin:test.local", "@mallory:test.local", "@alice:other.local"} {
		if service.AllowsUser(userID) {
			t.Errorf("the namespace also covers %s, which nobody granted", userID)
		}
	}
}

func TestARegistrationThatIsNotAnAccountForThisServerIsRejected(t *testing.T) {
	dir := t.TempDir()
	// Without a sender local part there is no user ID to attribute the service's own events to, and
	// without a token there is nothing to authenticate. Both are refused at start-up rather than
	// discovered at the first request.
	if _, err := appservice.Load([]string{writeFile(t, filepath.Join(dir, "anonymous.yaml"),
		"id: anonymous\nas_token: tok\nnamespaces:\n  users: []\n")}, nil); err == nil {
		t.Error("a registration with no sender_localpart was accepted")
	}
	if _, err := appservice.Load([]string{writeFile(t, filepath.Join(dir, "tokenless.yaml"),
		"id: tokenless\nsender_localpart: bridge\n")}, nil); err == nil {
		t.Error("a registration with no as_token was accepted")
	}
}

func TestEveryConfiguredPatternIsReadAndAGoodFileIsNotLost(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "one.yaml"), strings.Replace(bridgeRegistration, "my_as_id", "one", 1))
	writeFile(t, filepath.Join(dir, "two.yaml"), strings.Replace(
		strings.Replace(bridgeRegistration, "my_as_id", "two", 1), "as-secret-token", "second-token", 1))
	writeFile(t, filepath.Join(dir, "notes.txt"), "not a registration\n")

	registry, err := appservice.Load([]string{filepath.Join(dir, "*.yaml")}, nil)
	if err != nil {
		t.Fatalf("loading a glob: %v", err)
	}
	if registry.Len() != 2 {
		t.Errorf("%d services loaded from the glob, want 2", registry.Len())
	}
	for _, token := range []string{"as-secret-token", "second-token"} {
		if _, ok := registry.LookupByToken(token); !ok {
			t.Errorf("the service with token %s is missing", token)
		}
	}
}

func TestABrokenRegistrationIsReportedRatherThanSkipped(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, filepath.Join(dir, "one.yaml"), bridgeRegistration)
	bad := writeFile(t, filepath.Join(dir, "broken.yaml"), "id: broken\nas_token: [this is not a string\n")

	_, err := appservice.Load([]string{good, bad}, nil)
	if err == nil {
		t.Fatal("a registration that cannot be parsed was accepted")
	}
	// The message has to name the file: an operator with twelve of them is not served by "bad yaml".
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

func TestAnUnknownTokenIsNotAnApplicationService(t *testing.T) {
	dir := t.TempDir()
	registry, err := appservice.Load([]string{writeFile(t, filepath.Join(dir, "one.yaml"), bridgeRegistration)}, nil)
	if err != nil {
		t.Fatalf("loading the registration: %v", err)
	}
	if _, ok := registry.LookupByToken("some-user's-access-token"); ok {
		t.Error("an arbitrary token was recognised as an application service token")
	}
	// An empty registry has to behave, because that is the configuration of every server that has
	// never been told about a bridge.
	empty, err := appservice.Load(nil, nil)
	if err != nil {
		t.Fatalf("loading with no files configured: %v", err)
	}
	if _, ok := empty.LookupByToken("as-secret-token"); ok {
		t.Error("an empty registry recognised a token")
	}
}
