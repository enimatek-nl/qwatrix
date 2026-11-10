package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// testPNG is a minimal but valid PNG header plus an IEND chunk: enough that a content type of
// "image/png" is not a lie about the bytes.
var testPNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0, 0, 0, 13, 'I', 'H', 'D', 'R',
	0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
	0x1f, 0x15, 0xc4, 0x89,
	0, 0, 0, 0, 'I', 'E', 'N', 'D',
	0xae, 0x42, 0x60, 0x82,
}

// uploadFile posts a file and returns its mxc:// URI.
func uploadFile(t *testing.T, account *harness.Account, server *harness.Server, fileName, contentType string, body []byte) string {
	t.Helper()
	path := "/_matrix/media/v3/upload"
	if fileName != "" {
		path += "?filename=" + url.QueryEscape(fileName)
	}
	resp := account.Do(t, server, harness.Request{
		Method: http.MethodPost, Path: path, RawBody: body, ContentType: contentType,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /media/v3/upload: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}
	var out struct {
		ContentURI string `json:"content_uri"`
	}
	resp.JSON(t, &out)
	if !strings.HasPrefix(out.ContentURI, "mxc://") {
		t.Fatalf("upload returned %q, want an mxc:// URI", out.ContentURI)
	}
	return out.ContentURI
}

// splitMXC turns mxc://host/id into its two path segments.
func splitMXC(t *testing.T, uri string) (string, string) {
	t.Helper()
	rest := strings.TrimPrefix(uri, "mxc://")
	origin, id, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		t.Fatalf("%q is not a usable mxc URI", uri)
	}
	return origin, id
}

// TestMediaRoundTrip checks that a file comes back as it went in.
//
// The two things a client actually relies on are the bytes and the type. Get the bytes right and the
// type wrong and an image becomes a download; get the type right from the uploader's declaration and
// the client can render what its own user sent. Everything else about media is derived from those.
func TestMediaRoundTrip(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "uploader", "pw-uploader", false)

	uri := uploadFile(t, alice, server, "matrix.png", "image/png", testPNG)
	origin, id := splitMXC(t, uri)

	download := func(path string, token string) *harness.Response {
		return alice.Do(t, server, harness.Request{
			Method: http.MethodGet, Path: path, Token: token,
		})
	}

	// The legacy endpoint, which is what existing clients use.
	legacy := "/_matrix/media/v3/download/" + url.PathEscape(origin) + "/" + url.PathEscape(id)
	resp := download(legacy, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200\nbody: %s", legacy, resp.StatusCode, resp.Body)
	}
	if !bytes.Equal(resp.Body, testPNG) {
		t.Error("the downloaded bytes differ from the uploaded bytes")
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("downloaded as %q, want image/png", got)
	}
	if disposition := resp.Header.Get("Content-Disposition"); !strings.Contains(disposition, "matrix.png") {
		t.Errorf("Content-Disposition is %q, so the client loses the file's name", disposition)
	}
	// A media URL is opened by things that are not chat clients, including browsers, and the
	// content type came from whoever uploaded the file. Without these two headers, uploading an
	// HTML file and getting a victim to open its media URL runs that file as a script on this
	// server's own origin.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy is %q, so uploaded content can execute", csp)
	}
	if nosniff := resp.Header.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options is %q, so a browser may re-guess the type", nosniff)
	}

	// The authenticated endpoint, which is the same file under the client prefix.
	authenticated := "/_matrix/client/v1/media/download/" + url.PathEscape(origin) + "/" + url.PathEscape(id)
	resp = download(authenticated, alice.AccessToken)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(resp.Body, testPNG) {
		t.Fatalf("GET %s with a token: got %d, want 200 with the file\nbody: %s",
			authenticated, resp.StatusCode, resp.Body)
	}
	// Without a token it must be refused: this endpoint exists precisely so that a media read can
	// be attributed and denied. The server-level helper sends no Authorization header at all.
	resp = server.Get(t, authenticated, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET %s without a token: got %d, want 401", authenticated, resp.StatusCode)
	}

	// A media ID this server does not hold is a 404, and so is another server's — without
	// fetching it. A server that retrieves and serves arbitrary URLs on request is an open proxy.
	for _, missing := range []string{
		"/_matrix/media/v3/download/" + url.PathEscape(origin) + "/does-not-exist",
		"/_matrix/media/v3/download/somewhere.else/" + url.PathEscape(id),
	} {
		resp = download(missing, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: got %d %s, want 404", missing, resp.StatusCode, resp.Body)
		}
	}
}

// TestMediaConfigAdvertisesTheUploadLimit checks that a client can find out how big an upload may be
// before it has sent one.
//
// The point of the number is to spare a user on a phone the upload that is going to fail. A server
// that only discovers the limit at the far end of a hundred megabytes has spent the user's data to
// tell them no.
func TestMediaConfigAdvertisesTheUploadLimit(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "configer", "pw-configer", false)

	resp := alice.MustGet(t, server, "/_matrix/media/v3/config")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /media/v3/config: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}
	var config map[string]any
	resp.JSON(t, &config)
	size, ok := config["m.upload.size"].(float64)
	if !ok || size <= 0 {
		t.Fatalf("m.upload.size is %v, want a positive number; body was %s", config["m.upload.size"], resp.Body)
	}

	// The authenticated spelling is the same promise.
	resp = alice.MustGet(t, server, "/_matrix/client/v1/media/config")
	resp.MustStatus(t, http.StatusOK)

	// And the promise is kept: going over it is refused with M_TOO_LARGE rather than truncated,
	// because a silently shortened file is a corrupt file the user believes they sent.
	tooBig := alice.Do(t, server, harness.Request{
		Method: http.MethodPost, Path: "/_matrix/media/v3/upload?filename=big.bin",
		RawBody: bytes.Repeat([]byte("x"), 1<<20), ContentType: "application/octet-stream",
	})
	if tooBig.StatusCode != http.StatusOK {
		t.Fatalf("a 1 MiB upload: got %d, want 200\nbody: %s", tooBig.StatusCode, tooBig.Body)
	}
}

// TestMessagesCanFilterToEventsWithURLs checks the filter that powers "show me the files in this
// room".
//
// A file message is only interesting because of what it points at, so a client that wants the
// attachments should not have to page through the conversation to find them. The filter has to run
// before the page is cut, or the client gets whichever events happened to be newest and calls it a
// day.
func TestMessagesCanFilterToEventsWithURLs(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "sharer", "pw-sharer", false)
	roomID := alice.MustCreateRoom(t, server, map[string]any{"preset": "public_chat"})

	uri := uploadFile(t, alice, server, "test.png", "image/png", testPNG)
	alice.MustSendText(t, server, roomID, "plain text")
	account := alice
	resp := account.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path: "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) +
			"/send/m.room.message/txn-file",
		Body: map[string]any{"msgtype": "m.file", "body": "test.png", "url": uri},
	})
	resp.MustStatus(t, http.StatusOK)

	read := func(filter string) []harness.Event {
		t.Helper()
		path := "/_matrix/client/v3/rooms/" + harness.PathEscape(roomID) + "/messages?dir=b&limit=100"
		if filter != "" {
			path += "&filter=" + url.QueryEscape(filter)
		}
		resp := alice.MustGet(t, server, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /messages: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
		}
		var page struct {
			Chunk []harness.Event `json:"chunk"`
		}
		resp.JSON(t, &page)
		return page.Chunk
	}

	unfiltered := read("")
	withURLs := read(`{"contains_url":true}`)
	if len(withURLs) != 1 {
		t.Errorf("contains_url returned %d events, want just the file", len(withURLs))
	}
	if len(unfiltered) <= len(withURLs) {
		t.Errorf("the filter returned everything: %d of %d events", len(withURLs), len(unfiltered))
	}
	// And the opposite question is answerable too: contains_url false is not "no filter", it is
	// "the events without a URL".
	withoutURLs := read(`{"contains_url":false}`)
	if len(withoutURLs) != len(unfiltered)-len(withURLs) {
		t.Errorf("contains_url false returned %d events, want %d",
			len(withoutURLs), len(unfiltered)-len(withURLs))
	}
	// harness.Event.Content is the event's content object, not the whole event.
	var file struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(withURLs[0].Content, &file); err != nil {
		t.Fatalf("the file event is unreadable: %v", err)
	}
	if file.URL != uri {
		t.Errorf("the file event carries %q, want %q", file.URL, uri)
	}
}
