package matrix

import "slices"

// ClientVersions lists the client-server API versions qwatrix implements, oldest first.
//
// The values are the API version strings (the specification *release* version is
// different: release v1.19 defines client-server API version v1.11). A client that does not
// understand the list picks the newest entry it knows, so the order and the completeness
// both matter.
var ClientVersions = []string{
	"v1.0", "v1.1", "v1.2", "v1.3", "v1.4", "v1.5", "v1.6", "v1.7", "v1.8",
	"v1.9", "v1.10", "v1.11",
}

// SupportsClientVersion reports whether v is advertised by [ClientVersions].
func SupportsClientVersion(v string) bool {
	return slices.Contains(ClientVersions, v)
}

// UnstableFeatures returns the unstable feature flags advertised by GET /versions.
//
// Only flags that are actually implemented are advertised: advertising a flag is a promise
// that the corresponding unstable endpoint prefix works, and a homeserver that promises
// MSCs it does not speak confuses clients in ways that look like client bugs.
func UnstableFeatures() map[string]bool {
	return map[string]bool{}
}
