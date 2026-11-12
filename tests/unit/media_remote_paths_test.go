package unit_test

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/media"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Asking a peer for a file it holds has three spellings in the wild, and a server that picks one will
// meet the other two. There is the current specification's authenticated
// `GET /_matrix/federation/v1/download/{serverName}/{mediaId}`; the older unauthenticated
// `GET /_matrix/media/v3/download/{origin}/{mediaId}`, which complement's reference server serves and
// which real implementations still rely on for federation; and the draft
// `GET /_matrix/federation/v1/media/download/{mediaId}`, which this server grew first and still serves.
//
// A media ID is a pointer into somebody else's storage, so failing to find the right door is not a
// configuration detail — it is a broken image in a room, forever, on whichever side is unlucky.
func TestAMediaFetchTriesEveryPathAnOriginMightServe(t *testing.T) {
	for _, served := range []string{
		"/_matrix/federation/v1/download/origin.example/the-file",
		"/_matrix/media/v3/download/origin.example/the-file",
		"/_matrix/federation/v1/media/download/the-file",
	} {
		t.Run(strings.TrimPrefix(served, "/_matrix/"), func(t *testing.T) {
			var asked []string
			service := newMediaServiceWithFetch(t, func(origin, path string) (*media.RemoteMedia, error) {
				asked = append(asked, path)
				if path != served {
					// A peer that does not know this route says so; it is not refusing the file.
					return nil, errNotFoundLikePeer
				}
				body := io.NopCloser(strings.NewReader("hello from the origin"))
				return &media.RemoteMedia{Body: body, Size: 23, ContentType: "text/plain"}, nil
			})

			_, file, err := service.Download(context.Background(), "origin.example", "the-file")
			if err != nil {
				t.Fatalf("could not download media the origin serves at %s: %v (asked for %v)",
					served, err, asked)
			}
			defer func() { _ = file.Close() }()
			if len(asked) == 0 || asked[len(asked)-1] != served {
				t.Errorf("the origin was asked %v, and never settled on %s", asked, served)
			}
		})
	}
}

// The order is by what implementations answer, not by which endpoint is newest: asking a route a peer
// does not have is not free, because the peer sees the request — some of them treat an unknown request
// as an error and say so loudly.
func TestAMediaFetchAsksThePathImplementationsServeFirst(t *testing.T) {
	var asked []string
	service := newMediaServiceWithFetch(t, func(origin, path string) (*media.RemoteMedia, error) {
		asked = append(asked, path)
		return nil, errNotFoundLikePeer
	})

	if _, _, err := service.Download(context.Background(), "origin.example", "the-file"); err == nil {
		t.Fatal("an origin that serves nothing should not satisfy a download")
	}
	want := []string{
		"/_matrix/media/v3/download/origin.example/the-file",
		"/_matrix/federation/v1/download/origin.example/the-file",
		"/_matrix/federation/v1/media/download/the-file",
	}
	if strings.Join(asked, " ") != strings.Join(want, " ") {
		t.Errorf("asked in the order %v, want %v", asked, want)
	}
}

// A peer that answers with a real refusal for every path is a peer that does not have the file, and the
// caller has to be told. Quietly returning an empty successful-looking file would be worse than a 404.
func TestAMediaFetchGivesUpWithNothingToOffer(t *testing.T) {
	service := newMediaServiceWithFetch(t, func(origin, path string) (*media.RemoteMedia, error) {
		return nil, io.EOF
	})
	if _, _, err := service.Download(context.Background(), "origin.example", "the-file"); err == nil {
		t.Fatal("a fetch that never succeeded should not report success")
	}
}

// errNotFoundLikePeer is what a peer answers when the route is not there: a 404, not a refusal of the
// file. The distinction is why a second path is worth trying after the first.
var errNotFoundLikePeer = &peerError{}

type peerError struct{}

func (*peerError) Error() string { return "404 Not Found from the origin" }

func newMediaServiceWithFetch(t *testing.T, fetch func(origin, path string) (*media.RemoteMedia, error)) *media.Service {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatalf("opening a store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	service, err := media.New(st, media.Options{
		Dir:        t.TempDir(),
		ServerName: "this.example",
		MaxBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("building the media service: %v", err)
	}
	return service.WithRemoteFetcher(media.RemoteFetcherFunc(
		func(_ context.Context, origin, path string) (*media.RemoteMedia, error) {
			return fetch(origin, path)
		}))
}
