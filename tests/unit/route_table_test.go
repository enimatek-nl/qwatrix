package unit_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
)

// TestRouteTableSeparatesAWrongMethodFromAnUnknownPath pins the routing rule behind 405: a path this
// server serves, reached by a method it does not accept, must be a 405 naming the methods it does; a
// path it does not serve must stay a 404.
//
// This is unit-tested rather than only through the API because the rule is pure — patterns in, status
// out — and because the interesting cases (a route with placeholders, a route served on only one
// listener) are cheaper to enumerate here than through ten endpoints.
func TestRouteTableSeparatesAWrongMethodFromAnUnknownPath(t *testing.T) {
	mux := http.NewServeMux()
	routes := httpx.NewRouteTable(mux)
	notFound := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}
	// A catch-all like the one a Matrix server needs, which is precisely what defeats the standard
	// library's own 405 handling.
	mux.Handle("/_matrix/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes.ServeUnmatched(w, r, notFound)
	}))
	for _, pattern := range []string{
		"GET /_matrix/federation/v1/version",
		"POST /_matrix/client/v3/login",
		"GET /_matrix/client/v3/rooms/{roomID}/event/{eventID}",
		"PUT /_matrix/client/v3/rooms/{roomID}/send/{eventType}/{txnID}",
	} {
		routes.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	}

	for _, tc := range []struct {
		method, path string
		wantStatus   int
		wantAllow    string
	}{
		{"PUT", "/_matrix/federation/v1/version", http.StatusMethodNotAllowed, "GET,HEAD"},
		{"PATCH", "/_matrix/client/v3/login", http.StatusMethodNotAllowed, "POST"},
		{"DELETE", "/_matrix/client/v3/rooms/!room:server/event/$eventID", http.StatusMethodNotAllowed, "GET,HEAD"},
		{"GET", "/_matrix/client/v3/rooms/!room:server/send/m.room.message/txn1", http.StatusMethodNotAllowed, "PUT"},
		{"GET", "/_matrix/client/v3/nope", http.StatusNotFound, ""},
		{"GET", "/_matrix/client/v3/rooms/!room:server/event", http.StatusNotFound, ""},
	} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != tc.wantStatus {
			t.Errorf("%s %s: status %d, want %d (Allow %q)",
				tc.method, tc.path, recorder.Code, tc.wantStatus, recorder.Header().Get("Allow"))
		}
		if allow := recorder.Header().Get("Allow"); allow != tc.wantAllow {
			t.Errorf("%s %s: Allow = %q, want %q", tc.method, tc.path, allow, tc.wantAllow)
		}
	}
}
