package httpx

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// RouteTable registers routes and remembers what was registered, so a request that matches a known
// path with an unknown method can be answered with 405 instead of 404.
//
// Why this is not simply the standard library's behaviour: Go's mux does answer 405 for a method
// mismatch, but only when nothing else matches the path at all — and a Matrix server has to keep a
// catch-all under /_matrix/ to answer unknown endpoints with M_UNRECOGNIZED, which is exactly the
// pattern that swallows the mismatch. So the knowledge of which paths exist has to be kept somewhere,
// and this is that place.
//
// The lookup mux holds one pattern per distinct *path*, with wildcard names normalised to {p}: two
// routes like /rooms/{roomID}/foo and /rooms/{roomID}/bar are one path as far as a method mismatch is
// concerned, and registering both would make Go's mux panic on conflicting wildcards.
type RouteTable struct {
	mux  *http.ServeMux
	path *http.ServeMux

	// methods maps a normalised path to the methods it answers.
	methods map[string]map[string]bool
	// registered guards against registering the same normalised path twice, which the lookup mux
	// would reject.
	registered map[string]bool
}

// NewRouteTable builds a table that registers on mux.
func NewRouteTable(mux *http.ServeMux) *RouteTable {
	return &RouteTable{
		mux:        mux,
		path:       http.NewServeMux(),
		methods:    map[string]map[string]bool{},
		registered: map[string]bool{},
	}
}

// HandleFunc registers a route. The pattern is Go's syntax: an optional method followed by the path,
// as in "GET /_matrix/client/v3/sync" or "/_matrix/".
func (t *RouteTable) HandleFunc(pattern string, handler http.HandlerFunc) {
	method, path := splitRoutePattern(pattern)
	t.mux.HandleFunc(pattern, handler)
	t.record(method, path)
}

// Handle registers a route with an http.Handler.
func (t *RouteTable) Handle(pattern string, handler http.Handler) {
	method, path := splitRoutePattern(pattern)
	t.mux.Handle(pattern, handler)
	t.record(method, path)
}

// record notes that path answers to method. A pattern with no method is a catch-all rather than an
// endpoint, and knowing about it would turn every 404 into a 405.
func (t *RouteTable) record(method, path string) {
	if method == "" {
		return
	}
	normalised := normaliseRoutePath(path)
	if t.registered[normalised] {
		t.methods[normalised][method] = true
		return
	}
	t.registered[normalised] = true
	t.methods[normalised] = map[string]bool{method: true}
	t.path.HandleFunc(normalised, func(http.ResponseWriter, *http.Request) {})
}

// ServeUnmatched answers a request that no route took.
//
// A path this server does have, reached by a method it does not accept, is a 405 naming the methods it
// does: those are different facts, and a client can only correct itself about one of them. Anything
// else falls through to notFound, which answers M_UNRECOGNIZED — because turning an unknown path into a
// 405 would advertise an endpoint that is not there.
func (t *RouteTable) ServeUnmatched(w http.ResponseWriter, r *http.Request, notFound http.HandlerFunc) {
	methods := t.registeredFor(r.URL.Path)
	if methods == nil {
		notFound(w, r)
		return
	}
	allowed := t.allowedOf(methods)
	w.Header().Set("Allow", strings.Join(allowed, ","))
	RespondError(w, r, matrix.NewMethodNotAllowed("This endpoint does not accept %s; it accepts %s",
		r.Method, strings.Join(allowed, ", ")))
}

// registeredFor returns the methods this server serves for a path, matching against the patterns that
// were registered rather than against the standard library's idea of what matched.
//
// The standard library could answer this — `ServeMux.Handler` reports a match — but it also reports
// matches that are not endpoints, such as a trailing-slash redirect, and a method table built from
// those answers turns a 404 into a 405 for a resource that does not exist. A Matrix server's routing
// is simple enough to match exactly: literal segments, `{wildcard}` segments, and a trailing `/{path}`
// tail, which is what Go's pattern syntax offers.
func (t *RouteTable) registeredFor(path string) map[string]bool {
	// The common case is a literal path, which is a map lookup.
	if methods, ok := t.methods[path]; ok {
		return methods
	}
	segments := splitPath(path)
	for pattern, methods := range t.methods {
		if matchPattern(pattern, segments) {
			return methods
		}
	}
	return nil
}

// splitPath cuts a URL path into its non-empty segments.
func splitPath(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// matchPattern reports whether a registered pattern names these path segments.
func matchPattern(pattern string, segments []string) bool {
	patternSegments := splitPath(pattern)
	// A pattern ending in a wildcard tail ("/{path}") matches every remaining segment.
	if n := len(patternSegments); n > 0 && strings.HasPrefix(patternSegments[n-1], "{path}") {
		if len(segments) < n-1 {
			return false
		}
		for i := 0; i < n-1; i++ {
			if !segmentMatches(patternSegments[i], segments[i]) {
				return false
			}
		}
		return true
	}
	if len(patternSegments) != len(segments) {
		return false
	}
	for i, part := range patternSegments {
		if !segmentMatches(part, segments[i]) {
			return false
		}
	}
	return true
}

// segmentMatches compares one segment, treating a brace-delimited one as a wildcard.
func segmentMatches(pattern, segment string) bool {
	if strings.HasPrefix(pattern, "{") {
		return segment != ""
	}
	return pattern == segment
}

// allowed lists the methods a known path answers, in a stable order. HEAD is included when GET is
// accepted, because Go's mux serves a HEAD from a GET route; nothing else is claimed, so an
// Allow header never advertises a method this server would refuse.
func (t *RouteTable) allowedOf(methods map[string]bool) []string {
	out := make([]string, 0, len(methods)+1)
	for method := range methods {
		out = append(out, method)
		if method == http.MethodGet && !methods[http.MethodHead] {
			out = append(out, http.MethodHead)
		}
	}
	sort.Strings(out)
	return out
}

// splitRoutePattern separates Go's "METHOD /path" pattern into its two halves.
func splitRoutePattern(pattern string) (method, path string) {
	// A pattern may begin with a host ("example.com/path"), which this server never registers; the
	// forms it does use are "/path" and "GET /path".
	if before, after, found := strings.Cut(pattern, " "); found && !strings.Contains(before, "/") {
		return before, after
	}
	return "", pattern
}

// normaliseRoutePath rewrites every wildcard to a common name so that paths which differ only in what
// their placeholders are called occupy one entry.
func normaliseRoutePath(path string) string {
	if !strings.Contains(path, "{") {
		return path
	}
	// Each placeholder is renamed in sequence: {p0}, {p1}. Go's mux rejects a pattern that repeats a
	// placeholder name, and a real route like /rooms/{roomID}/send/{eventType}/{txnID} has three.
	var out strings.Builder
	for index := 0; ; index++ {
		open := strings.Index(path, "{")
		if open < 0 {
			out.WriteString(path)
			return out.String()
		}
		end := strings.Index(path[open:], "}")
		if end < 0 {
			out.WriteString(path)
			return out.String()
		}
		out.WriteString(path[:open])
		fmt.Fprintf(&out, "{p%d}", index)
		path = path[open+end+1:]
	}
}
