package api_test

// The endpoint documentation contract.
//
// A server that speaks a protocol should be able to say which parts of it it speaks. This
// test makes that claim checkable: it reads the routes out of the source that registers them
// and compares them against docs/ENDPOINTS.md in both directions, so a stale document fails
// the build exactly like a missing one does.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// routeFirstSegments are the top-level path segments that begin a Matrix client endpoint.
//
// They give the extractor a way to tell an endpoint suffix from the other string literals in
// a handler ("m.room.message", "application/json", a query parameter) without a hand-kept
// allowlist that would need editing whenever an endpoint is added.
var routeFirstSegments = map[string]bool{
	"account": true, "admin": true, "capabilities": true, "createRoom": true,
	"delete_devices": true, "devices": true, "directory": true, "directories": true,
	"events": true, "filters": true, "join": true, "joined_rooms": true, "keys": true,
	"login": true, "loginToken": true, "logout": true, "media": true,
	"presences": true, "preview_url": true, "profile": true, "publicRooms": true,
	"pushers": true, "pushrules": true, "register": true, "rooms": true,
	"sendtodevice": true, "stop": true, "sync": true, "tags": true, "user": true,
	"voip": true, "versions": true,
	// The server-server API is checked the same way. `key` is the Matrix key service that
	// publishes this server's signing key, and `federation` is the API namespace itself.
	"federation": true, "key": true,
}

const (
	csapiDirectory    = "../../internal/csapi"
	fedapiDirectory   = "../../internal/fedapi"
	endpointsDocument = "../../docs/ENDPOINTS.md"
)

// route is one endpoint the server registers: an HTTP method and the endpoint's own path,
// without the version prefixes the mux helper adds.
type route struct {
	Method string
	Suffix string
	File   string
	Line   int
}

// String is the identity used in the document and in failure messages.
func (r route) String() string { return r.Method + " " + r.Suffix }

func TestEveryEndpointIsDocumented(t *testing.T) {
	routes := collectRoutes(t)
	if len(routes) < 20 {
		t.Fatalf("only %d routes extracted; the extractor has fallen out of step with the code", len(routes))
	}

	documented := loadDocumentedRoutes(t)
	if len(documented) < 20 {
		t.Fatalf("%s yielded %d routes; its table format has changed", endpointsDocument, len(documented))
	}

	// Code → document: every route the server answers must be described.
	for _, found := range routes {
		if !documented[found.String()] {
			t.Errorf("%s (%s:%d) is not documented in docs/ENDPOINTS.md",
				found, filepath.Base(found.File), found.Line)
		}
	}

	// Document → code: a document that advertises an endpoint the server no longer serves
	// is worse than no document, because it is a false statement about a protocol promise.
	seen := map[string]bool{}
	for _, found := range routes {
		seen[found.String()] = true
	}
	var phantom []string
	for key := range documented {
		if !seen[key] {
			phantom = append(phantom, key)
		}
	}
	if len(phantom) > 0 {
		sort.Strings(phantom)
		t.Errorf("docs/ENDPOINTS.md lists %d endpoint(s) the server does not register: %v",
			len(phantom), phantom)
	}
}

// collectRoutes parses the client-server API package and returns the routes it registers.
//
// It reads the source rather than probing the running server because net/http.ServeMux
// cannot enumerate its patterns, and because a route that is registered is a promise whether
// or not this particular test process happened to reach it.
func collectRoutes(t *testing.T) []route {
	t.Helper()

	var routes []route
	// Both HTTP surfaces are held to the same documentation rule: the client API and the
	// server-server API drift apart from their docs in exactly the same way.
	for _, directory := range []string{csapiDirectory, fedapiDirectory} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatalf("reading %s: %v", directory, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			routes = append(routes, routesInFile(t, filepath.Join(directory, entry.Name()))...)
		}
	}
	return dedupeRoutes(expandStateRoutes(routes))
}

// expandStateRoutes accounts for the one route that serves two documented endpoints:
// `/rooms/{roomID}/state/{eventType}` and `.../{stateKey}` are the same handler behind a
// multi-segment pattern, so both rows of the document are backed by it.
func expandStateRoutes(routes []route) []route {
	out := make([]route, 0, len(routes)*2)
	for _, found := range routes {
		out = append(out, found)
		const suffix = "/state/{eventType}"
		if strings.HasSuffix(found.Suffix, suffix) {
			withKey := found
			withKey.Suffix = found.Suffix + "/{stateKey}"
			out = append(out, withKey)
		}
	}
	return out
}

// routesInFile extracts the routes one source file registers.
//
// Three shapes appear in the code and all three are real registrations:
//
//  1. `mux.HandleFunc("GET /_matrix/client/versions", …)` — a literal pattern.
//  2. `for _, path := range versionedPath("sync") { mux.HandleFunc("GET "+path, …) }` — the
//     common form, which serves one endpoint under four version prefixes.
//  3. a table of `{suffix, handler}` rows registered in a loop — how the seven membership
//     endpoints are declared without repeating the registration seven times.
func routesInFile(t *testing.T, path string) []route {
	t.Helper()

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	// Route tables are usually assigned to a variable and ranged over later, so the
	// literals have to be found by name. The map is per file: a name reused for a second
	// table in the same file would be read as the first, which is why the tables in this
	// package are named after what they register rather than called `routes`.
	tables := routeTables(file)

	var routes []route
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CallExpr:
			// Shape 1.
			if route, ok := literalRoute(typed, fileSet, path); ok {
				routes = append(routes, route)
			}
		case *ast.RangeStmt:
			// Shapes 2 and 3.
			routes = append(routes, routesInLoop(typed, tables, fileSet, path)...)
		}
		return true
	})
	return routes
}

// routeTables indexes the composite literals assigned to variables, by variable name.
func routeTables(file *ast.File) map[string]*ast.CompositeLit {
	tables := map[string]*ast.CompositeLit{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			for i, name := range typed.Lhs {
				ident, ok := name.(*ast.Ident)
				if !ok || i >= len(typed.Rhs) {
					continue
				}
				if literal, ok := typed.Rhs[i].(*ast.CompositeLit); ok {
					tables[ident.Name] = literal
				}
			}
		case *ast.ValueSpec:
			for i, name := range typed.Names {
				if i >= len(typed.Values) {
					continue
				}
				if literal, ok := typed.Values[i].(*ast.CompositeLit); ok {
					tables[name.Name] = literal
				}
			}
		}
		return true
	})
	return tables
}

// literalRoute recognises a registration whose pattern is written out in full.
func literalRoute(call *ast.CallExpr, fileSet *token.FileSet, file string) (route, bool) {
	callSite, ok := routeCallSite(call)
	if !ok {
		return route{}, false
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return route{}, false
	}
	pattern := strings.Trim(literal.Value, `"`)
	method, pathOnly, found := strings.Cut(pattern, " ")
	if !found {
		// A registration with no method prefix answers every method, which is what the
		// catch-all patterns do; those are not endpoints and are not documented as such.
		return route{}, false
	}
	suffix := normalizeSuffix(pathOnly)
	if suffix == "" || suffix == "/" {
		return route{}, false
	}
	return route{Method: method, Suffix: suffix, File: file, Line: fileSet.Position(callSite).Line}, true
}

// routeCallSite returns the position of a mux registration call.
func routeCallSite(call *ast.CallExpr) (token.Pos, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "HandleFunc" || len(call.Args) < 2 {
		return 0, false
	}
	return call.Pos(), true
}

// routesInLoop expands one registration loop into the routes it produces.
//
// The suffixes come from the loop's range expression (a versioned-path call, or a table of
// route rows) and the methods from the registrations in its body — both read from the
// enclosing loop, so a nested loop for a different endpoint cannot mix its methods in.
func routesInLoop(loop *ast.RangeStmt, tables map[string]*ast.CompositeLit, fileSet *token.FileSet, file string) []route {
	suffixes := suffixesOfRange(loop.X, tables)
	if len(suffixes) == 0 {
		return nil
	}

	var methods []string
	ast.Inspect(loop.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if _, ok := routeCallSite(call); !ok {
			return true
		}
		binary, ok := call.Args[0].(*ast.BinaryExpr)
		if !ok || binary.Op != token.ADD {
			return true
		}
		literal, ok := binary.X.(*ast.BasicLit)
		if !ok {
			return true
		}
		methods = append(methods, strings.TrimSpace(strings.Trim(literal.Value, `"`)))
		return true
	})
	if len(methods) == 0 {
		return nil
	}

	position := fileSet.Position(loop.Pos())
	out := make([]route, 0, len(suffixes)*len(methods))
	for _, suffix := range suffixes {
		for _, method := range methods {
			out = append(out, route{
				Method: method,
				Suffix: normalizeSuffix(suffix),
				File:   file,
				Line:   position.Line,
			})
		}
	}
	return out
}

// suffixesOfRange returns the endpoint paths a range expression iterates over.
func suffixesOfRange(expr ast.Expr, tables map[string]*ast.CompositeLit) []string {
	switch typed := expr.(type) {
	case *ast.Ident:
		if table, ok := tables[typed.Name]; ok {
			return suffixesOfRange(table, tables)
		}
		return nil
	case *ast.CallExpr:
		// versionedPath("sync") — a call whose argument names the endpoint. A call whose
		// argument is a variable belongs to a table-driven loop, whose suffixes come from
		// the table itself.
		function, ok := typed.Fun.(*ast.Ident)
		if !ok || function.Name != "versionedPath" || len(typed.Args) != 1 {
			return nil
		}
		literal, ok := typed.Args[0].(*ast.BasicLit)
		if !ok {
			return nil
		}
		return []string{strings.Trim(literal.Value, `"`)}
	case *ast.CompositeLit:
		// A route table: each row's first field is the endpoint suffix.
		var out []string
		for _, element := range typed.Elts {
			row, ok := element.(*ast.CompositeLit)
			if !ok || len(row.Elts) == 0 {
				continue
			}
			literal, ok := row.Elts[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			if suffix := strings.Trim(literal.Value, `"`); isEndpointSuffix(suffix) {
				out = append(out, suffix)
			}
		}
		return out
	}
	return nil
}

// isEndpointSuffix reports whether a path begins with a top-level Matrix endpoint name.
func isEndpointSuffix(suffix string) bool {
	first := suffix
	if index := strings.Index(suffix, "/"); index >= 0 {
		first = suffix[:index]
	}
	return routeFirstSegments[first]
}

// normalizeSuffix reduces a registered pattern to the document's form: the endpoint's own
// path, with the version prefixes removed, and non-client prefixes left whole.
func normalizeSuffix(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	// A multi-segment wildcard is how the state endpoints are served (see stateTupleOf),
	// because a state key can contain a slash and the empty state key has more than one
	// spelling. The document describes the endpoint the way the specification spells it, so
	// that internal shape is expanded back into the two documented tuples here rather than
	// leaking a routing detail into a protocol document.
	pattern = strings.ReplaceAll(pattern, "/state/{tail...}", "/state/{eventType}")
	for _, prefix := range []string{
		"/_matrix/client/v3/", "/_matrix/client/v1/", "/_matrix/client/r0.6.1/", "/_matrix/client/r0/",
	} {
		if strings.HasPrefix(pattern, prefix) {
			return strings.TrimPrefix(pattern, prefix)
		}
	}
	if strings.HasPrefix(pattern, "/_matrix/") || strings.HasPrefix(pattern, "/_synapse/") {
		return pattern
	}
	return strings.TrimPrefix(pattern, "/")
}

// loadDocumentedRoutes parses the markdown tables of the endpoint document, where a row's
// method cell may name several methods for one path (`GET, PUT`), the way the specification's
// own tables are written.
func loadDocumentedRoutes(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(endpointsDocument)
	if err != nil {
		t.Fatalf("reading %s: %v", endpointsDocument, err)
	}

	documented := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		columns := strings.Split(line, "|")
		if len(columns) < 3 {
			continue
		}
		methods := parseBackticks(columns[1])
		targets := parseBackticks(columns[2])
		if len(methods) == 0 || len(targets) == 0 {
			continue
		}
		for _, target := range targets {
			if !strings.HasPrefix(target, "/") {
				continue
			}
			for _, method := range methods {
				documented[method+" "+normalizeSuffix(target)] = true
			}
		}
	}
	return documented
}

// endpointStatuses are the only words the document's Status column may use, and they are AC-6's
// vocabulary: `implemented`, `partial`, `stub`.
var endpointStatuses = map[string]bool{
	"implemented": true,
	"partial":     true,
	"stub":        true,
}

// TestEveryDocumentedEndpointCarriesAStatus checks the second half of AC-6, which is the half a
// generated table does not give you for free.
//
// Listing an endpoint says the server answers it. Saying *how completely* it answers is the part a
// client plans around: an endpoint that accepts a request and quietly ignores half of it is worse
// than a 404, because the client has already committed to using it. So every row states its status
// in the criteria's own words, and a row that omits the column is a row whose completeness nobody
// has claimed — which is the state this catches, rather than letting the column decay into
// decoration.
func TestEveryDocumentedEndpointCarriesAStatus(t *testing.T) {
	raw, err := os.ReadFile(endpointsDocument)
	if err != nil {
		t.Fatalf("reading %s: %v", endpointsDocument, err)
	}
	missing := []string{}
	unexpected := []string{}
	rows := 0
	// Only the endpoint tables count. The document has other tables — an explanation of what an
	// application service may do, a list of what a refresh token is refused — whose first column is
	// also backticked, and demanding a status from a sentence would be a test that argues with the
	// wrong thing. A blank line ends a table.
	inEndpointTable := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			inEndpointTable = false
		case strings.HasPrefix(line, "| Method | Path | Spec | Status |"):
			inEndpointTable = true
		case !strings.HasPrefix(line, "| `"):
		default:
			if !inEndpointTable {
				continue
			}
		}
		if !inEndpointTable || !strings.HasPrefix(line, "| `") {
			continue
		}
		columns := strings.Split(line, "|")
		if len(columns) < 4 {
			continue
		}
		methods := parseBackticks(columns[1])
		targets := parseBackticks(columns[2])
		if len(methods) == 0 || len(targets) == 0 {
			continue
		}
		rows++
		status := strings.ToLower(strings.TrimSpace(columns[len(columns)-2]))
		switch {
		case status == "":
			missing = append(missing, strings.Join([]string{columns[1], columns[2]}, " "))
		case !endpointStatuses[status]:
			unexpected = append(unexpected, status+" ("+strings.TrimSpace(columns[2])+")")
		}
	}
	if rows < 100 {
		t.Fatalf("only %d endpoint rows were read, which is not the documented surface: is the "+
			"Status column missing a separator cell so the rows no longer parse?", rows)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%d endpoint rows state no status: %s", len(missing), strings.Join(missing[:min(len(missing), 6)], "; "))
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		t.Fatalf("the Status column uses words outside AC-6's vocabulary: %s", strings.Join(unexpected[:min(len(unexpected), 6)], "; "))
	}
}

// parseBackticks returns the backtick-quoted values of a markdown cell, in order.
func parseBackticks(cell string) []string {
	values := []string{}
	for {
		start := strings.Index(cell, "`")
		if start < 0 {
			return values
		}
		cell = cell[start+1:]
		end := strings.Index(cell, "`")
		if end < 0 {
			return values
		}
		values = append(values, cell[:end])
		cell = cell[end+1:]
	}
}

func dedupeRoutes(routes []route) []route {
	seen := map[string]bool{}
	out := make([]route, 0, len(routes))
	for _, found := range routes {
		if seen[found.String()] {
			continue
		}
		seen[found.String()] = true
		out = append(out, found)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
