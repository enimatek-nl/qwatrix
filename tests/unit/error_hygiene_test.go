package unit_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// CN-7 says a homeserver must not reveal internal details in error responses. The two choke points
// are where an unexpected Go error becomes an HTTP answer ([httpx.RespondError]) and where a panic
// becomes one ([httpx.Recoverer]); these tests pin both, because everything else on those paths is
// ordinary code that a handler could get wrong tomorrow.
//
// The client-facing tests in tests/api cover the endpoints; these cover the machinery underneath,
// where the strings that leak are actually produced.

// panicValue is written to look like a real internal failure: a driver message naming a database
// path. If any of it reaches a client, the test's assertions on it mean something.
const panicValue = "dial postgres: /var/lib/qwatrix/qwatrix.db: connection refused: pq: sorry"

func TestAPanicBecomesAGenericError(t *testing.T) {
	handler := httpx.Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(panicValue)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/sync", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("a panicking handler answered %d, want 500", recorder.Code)
	}
	var body struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("a panic was answered with something that is not a matrix error: %v\n%s",
			err, recorder.Body)
	}
	if body.ErrCode != string(matrix.ErrUnknown) {
		t.Errorf("errcode = %q, want %q", body.ErrCode, matrix.ErrUnknown)
	}
	for _, forbidden := range []string{panicValue, "goroutine", ".go:", "runtime", "panic"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Errorf("the panic response reveals %q: %s", forbidden, recorder.Body)
		}
	}
}

// TestAPanicDoesNotEndTheResponse asserts the other half: the panic is contained, so the connection
// is not left half-written. A handler that panics after writing a status line cannot be saved, but a
// panic before any output has to produce a complete, well-formed answer.
func TestAPanicDoesNotEndTheResponse(t *testing.T) {
	handler := httpx.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Panic") == "yes" {
			panic("boom")
		}
		w.WriteHeader(http.StatusOK)
	}))

	for _, panics := range []bool{true, false} {
		request := httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/versions", nil)
		if panics {
			request.Header.Set("X-Panic", "yes")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if got, want := recorder.Code, map[bool]int{true: 500, false: 200}[panics]; got != want {
			t.Fatalf("panicking=%v answered %d, want %d", panics, got, want)
		}
	}
}

func TestAnUnexpectedErrorIsNotDescribedToTheClient(t *testing.T) {
	// Errors a homeserver can realistically produce without any handler meaning any harm by them.
	for _, err := range []error{
		errors.New("sql: database is locked"),
		errors.New("runtime error: index out of range [3] with length 2"),
		errors.New("close of closed channel"),
		errors.New("sqlite3: no such column: events.txn_id at /home/enimatek/internal/app/app.go:118"),
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/sync", nil)
		httpx.RespondError(recorder, request, err)

		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("an unexpected error answered %d, want 500: %s", recorder.Code, recorder.Body)
		}
		var body struct {
			ErrCode string `json:"errcode"`
			Error   string `json:"error"`
		}
		if decodeErr := json.Unmarshal(recorder.Body.Bytes(), &body); decodeErr != nil {
			t.Fatalf("refusal is not a matrix error: %v\n%s", decodeErr, recorder.Body)
		}
		if body.ErrCode != string(matrix.ErrUnknown) {
			t.Errorf("errcode = %q, want %q", body.ErrCode, matrix.ErrUnknown)
		}
		if body.Error != "Internal server error" {
			t.Errorf("the client was told %q for the internal error %q; an unexpected error has to "+
				"be described as nothing more than that it was internal", body.Error, err)
		}
	}
}

// TestAMatrixErrorIsStillDescribed checks that sanitising is not the same as silencing: a refusal the
// specification defines has to keep the text the server chose, or clients lose the ability to tell a
// banned user apart from a room that has moved.
func TestAMatrixErrorIsStillDescribed(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/join/%21room%3Aexample.test", nil)
	httpx.RespondError(recorder, request, matrix.NewNotFound("Room %q does not exist on this homeserver",
		"!room:example.test"))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	var body struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal is not a matrix error: %v\n%s", err, recorder.Body)
	}
	if body.ErrCode != "M_NOT_FOUND" || !strings.Contains(body.Error, "!room:example.test") {
		t.Fatalf("a matrix error was rewritten: %s", recorder.Body)
	}
}

// TestDecodeFailureNamesTheCallersFieldNotTheProgrammers covers the leak this whole file is about:
// Go's decoder explains a type mismatch by naming the struct, the field, and the Go type.
func TestDecodeFailureNamesTheCallersFieldNotTheProgrammers(t *testing.T) {
	var destination struct {
		Password string         `json:"password"`
		Rooms    map[string]any `json:"rooms"`
	}

	for _, body := range []string{`{"password": ["x"]}`, `{"rooms": "nope"}`, `[1,2,3]`} {
		err := json.Unmarshal([]byte(body), &destination)
		if err == nil {
			t.Fatalf("%s decoded into the request type, so the test proved nothing", body)
		}
		failure := matrix.DecodeFailure("the request body", err)
		if failure.Status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, failure.Status)
		}
		if failure.Code != matrix.ErrBadJSON {
			t.Errorf("%s: errcode = %q, want %q", body, failure.Code, matrix.ErrBadJSON)
		}
		for _, forbidden := range []string{"json:", "struct field", "Go value", "unmarshal", "csapi",
			"string", "map["} {
			if strings.Contains(failure.Message, forbidden) {
				t.Errorf("%s: the refusal revealed %q: %q", body, forbidden, failure.Message)
			}
		}
	}

	// And the caller still learns which of their own fields to fix, which is the point of the
	// message and the reason it is not simply "bad JSON".
	err := json.Unmarshal([]byte(`{"password": 42}`), &destination)
	if failure := matrix.DecodeFailure("the request body", err); !strings.Contains(failure.Message, "password") {
		t.Errorf("the refusal %q does not name the field the caller sent", failure.Message)
	}
}
