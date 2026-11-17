package media

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/store"
)

// TestConcurrentViewersOfOneRemoteFileAskItsOriginOnce is the room-list case.
//
// A /sync that names three members holding the same avatar issues three downloads, and a client that
// retries a picture it could not render issues more. Each of them used to walk to the origin, which on a
// healthy peer is wasted load and on a broken one — the wrong certificate, the closed port — is a fresh
// TLS handshake every few milliseconds, visible in the log as the same media ID refused four times inside
// sixty. One caller asks; the rest read what it cached.
func TestConcurrentViewersOfOneRemoteFileAskItsOriginOnce(t *testing.T) {
	ctx := context.Background()
	const viewers = 6

	service, asked := newFightingService(t, func() (*RemoteMedia, error) {
		// Long enough that every viewer is definitely inside the fetch when the first one starts.
		time.Sleep(50 * time.Millisecond)
		return &RemoteMedia{
			ContentType: "image/png",
			Body:        io.NopCloser(strings.NewReader("the same avatar, once")),
			Size:        -1,
		}, nil
	})

	body := make(chan string, viewers)
	var waiting sync.WaitGroup
	for range viewers {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			_, file, err := service.Download(ctx, "elsewhere.example", "sharedAvatar")
			if err != nil {
				body <- "error: " + err.Error()
				return
			}
			defer func() { _ = file.Close() }()
			raw, _ := io.ReadAll(file)
			body <- string(raw)
		}()
	}
	waiting.Wait()
	close(body)

	if got := atomic.LoadInt32(asked); got != 1 {
		t.Errorf("the origin was asked for the file %d times, want 1: %d viewers of one file are one "+
			"request and %d cache reads", got, viewers, viewers-1)
	}
	for answer := range body {
		if answer != "the same avatar, once" {
			t.Errorf("a viewer was handed %q instead of the file", answer)
		}
	}
}

// TestARefusedRemoteFileIsNotAskedForAgainImmediately is the same saving on the other side of a refusal.
//
// An origin that cannot serve a file says so in one request's worth of work — this server tries each of
// the three endpoints a file can live behind — and remembering that answer for a moment is what stops a
// client redrawing a room list from paying it again and again. A moment, not forever: an origin that was
// restarting becomes fetchable again, which is why the pause expires rather than the file being written
// off.
func TestARefusedRemoteFileIsNotAskedForAgainImmediately(t *testing.T) {
	ctx := context.Background()
	service, asked := newFightingService(t, func() (*RemoteMedia, error) {
		return nil, io.ErrUnexpectedEOF
	})

	for _, attempt := range []string{"first", "second"} {
		if _, _, err := service.Download(ctx, "elsewhere.example", "gone"); err == nil {
			t.Fatalf("the %s download of a file the origin refuses reported success", attempt)
		}
	}
	// One turn down the ladder of three endpoints a file can live behind.
	const ladder = 3
	if got := atomic.LoadInt32(asked); got != ladder {
		t.Errorf("the origin was asked %d times for two requests, want %d: the refusal was not remembered",
			got, ladder)
	}

	// Let the pause run out, and the same request is willing to find out whether it works now.
	service.flights.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, _, err := service.Download(ctx, "elsewhere.example", "gone"); err == nil {
		t.Fatal("a file the origin refuses reported success after the pause expired")
	}
	if got := atomic.LoadInt32(asked); got != 2*ladder {
		t.Errorf("after the pause the origin had been asked %d times, want %d: a remembered refusal has "+
			"to expire", got, 2*ladder)
	}
}

// newFightingService builds a media service whose origin counts how often it is asked, and can be made to
// answer slowly or badly.
func newFightingService(t *testing.T, answer func() (*RemoteMedia, error)) (*Service, *int32) {
	t.Helper()

	st, err := store.Open(t.TempDir() + "/media.db")
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	service, err := New(st, Options{Dir: t.TempDir(), ServerName: "home.example", MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("building the media service: %v", err)
	}
	var asked int32
	service.WithRemoteFetcher(RemoteFetcherFunc(func(context.Context, string, string) (*RemoteMedia, error) {
		atomic.AddInt32(&asked, 1)
		return answer()
	}))
	return service, &asked
}

// TestFetchedMediaIsStoredUnderThisServersOwnName pins what the cache write is for: the bytes are kept,
// and they are kept as a copy rather than as a reference to the origin's media ID.
func TestFetchedMediaIsStoredUnderThisServersOwnName(t *testing.T) {
	ctx := context.Background()

	service, _ := newFightingService(t, func() (*RemoteMedia, error) {
		return &RemoteMedia{ContentType: "text/plain", FileName: "notes.txt",
			Body: io.NopCloser(bytes.NewBufferString("fetched")), Size: 7}, nil
	})
	row, file, err := service.Download(ctx, "elsewhere.example", "theirs")
	if err != nil {
		t.Fatalf("downloading the remote file: %v", err)
	}
	defer func() { _ = file.Close() }()
	if row.ID != "theirs" {
		t.Errorf("the caller was told the file is %q; the origin's ID is what they asked for", row.ID)
	}
	cached, err := service.store.Media.GetUpstream(ctx, "elsewhere.example", "theirs")
	if err != nil {
		t.Fatalf("the fetched file was not cached: %v", err)
	}
	if cached.ID == "theirs" {
		t.Error("the cached copy reuses the origin's media ID, so a download for this server's own name " +
			"and that ID has to be refused by remembering to read the origin column")
	}
}
