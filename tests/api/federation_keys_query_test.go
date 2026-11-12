package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
	"github.com/tidwall/gjson"
)

// A homeserver is also its own notary: `POST /_matrix/key/v2/query` is the batched form of asking
// "whose key is this?", and the answer is the key document with the responder's signature added, so a
// peer can believe a key without having reached the server that published it.
//
// This server is a notary for exactly one server — itself — because that is the only key document it
// holds. The specification allows nothing worse than silence for the rest: a server whose keys are not
// known "will not be included in the result". Omitting them is therefore conformant; inventing them, or
// refusing the whole batch because one name is unknown, would not be.
func TestTheKeyQueryEndpointAnswersForItsOwnServer(t *testing.T) {
	server := harness.New(t)

	response := server.Do(t, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/key/v2/query",
		Body: map[string]any{
			"server_keys": map[string]any{string(server.ServerName): map[string]any{}},
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST /_matrix/key/v2/query: status %d, want 200\nbody: %s",
			response.StatusCode, response.Body)
	}
	var answer struct {
		ServerKeys []json.RawMessage `json:"server_keys"`
	}
	response.JSON(t, &answer)
	if len(answer.ServerKeys) != 1 {
		t.Fatalf("returned %d key documents, want this server's one\nbody: %s",
			len(answer.ServerKeys), response.Body)
	}
	document := string(answer.ServerKeys[0])
	if got := gjson.Get(document, "server_name").String(); got != string(server.ServerName) {
		t.Errorf("returned a key document for %q, want %q", got, server.ServerName)
	}
	if gjson.Get(document, "verify_keys").Get("ed25519").Raw == "" &&
		!gjson.Get(document, "verify_keys").Exists() {
		t.Errorf("the key document carries no verify keys: %s", document)
	}
	// The signature is the whole point of asking a notary rather than the origin.
	// The server name is a gjson key too, and it contains dots and a port: unescaped, the lookup
	// reads them as path separators and a signed document looks unsigned.
	if sig := gjson.Get(document, "signatures."+gjson.Escape(string(server.ServerName))); !sig.Exists() {
		t.Errorf("the key document is not signed by %s: %s", server.ServerName, document)
	}
	// And what a query returns has to be the document the origin publishes, or the two endpoints
	// disagree about who this server is.
	published := string(server.Get(t, "/_matrix/key/v2/server", "").Body)
	if key, want := gjson.Get(document, "verify_keys").String(), gjson.Get(published, "verify_keys").String(); key != want {
		t.Errorf("the queried keys are %s, but this server publishes %s", key, want)
	}
}

// A name this server has never met is left out, not refused: a batch that asks for three servers and
// gets two back is a normal answer, and a peer is expected to go and ask someone else about the third.
func TestTheKeyQueryEndpointSaysNothingAboutKeysItDoesNotHold(t *testing.T) {
	server := harness.New(t)

	response := server.Do(t, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/key/v2/query",
		Body: map[string]any{
			"server_keys": map[string]any{
				"someone.else.example": map[string]any{},
			},
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d for a server this one knows nothing about, want 200 with nothing in it\nbody: %s",
			response.StatusCode, response.Body)
	}
	var answer struct {
		ServerKeys []json.RawMessage `json:"server_keys"`
	}
	response.JSON(t, &answer)
	if len(answer.ServerKeys) != 0 {
		t.Errorf("returned %d documents for a server this server is not a notary for: %s",
			len(answer.ServerKeys), response.Body)
	}
}

// An empty query is a well-formed question with an empty answer. It is worth pinning because "no
// servers were asked" and "the request was broken" pull in opposite directions for a client deciding
// whether to retry.
func TestTheKeyQueryEndpointAnswersAnEmptyQueryWithAnEmptyList(t *testing.T) {
	server := harness.New(t)

	response := server.Do(t, harness.Request{
		Method: http.MethodPost,
		Path:   "/_matrix/key/v2/query",
		Body:   map[string]any{"server_keys": map[string]any{}},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody: %s", response.StatusCode, response.Body)
	}
	if keys := gjson.GetBytes(response.Body, "server_keys"); !keys.IsArray() || len(keys.Array()) != 0 {
		t.Errorf("server_keys is %s, want an empty array", keys.Raw)
	}
}
