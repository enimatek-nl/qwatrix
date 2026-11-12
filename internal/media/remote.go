package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// remoteMediaLocalpart marks files this server fetched rather than received from a user.
//
// It is not a local part, and that is the point: a query for "the files of alice" must not answer with
// files alice never uploaded, because an administrative delete-by-user would then be a way to remove
// other people's cached content.
const remoteMediaLocalpart = "\x00federation"

// RemoteMedia is a file this server downloaded from the server that originates it.
type RemoteMedia struct {
	// ContentType is the origin's own, served through unchanged. This server did not receive the
	// upload, so it cannot know what the bytes were better than the server that did.
	ContentType string
	// FileName is the uploader's suggested name, when the origin sends one.
	FileName string
	// Body is the content. The caller closes it.
	Body io.ReadCloser
	// Size is the origin's declared length when it declares one, and -1 otherwise. A declared length
	// over the local limit is refused before any bytes are read, so a hostile origin cannot make this
	// server write a file it would then refuse to serve.
	Size int64
}

// RemoteFetcher asks the server a mxc URI names for the file behind it.
//
// It is a seam rather than a dependency on internal/fedapi: the media repository should know no more
// about how a peer is signed than it knows about how a file is stored.
type RemoteFetcher interface {
	FetchMedia(ctx context.Context, origin, path string) (*RemoteMedia, error)
}

// RemoteFetcherFunc adapts a function to [RemoteFetcher].
type RemoteFetcherFunc func(ctx context.Context, origin, path string) (*RemoteMedia, error)

// FetchMedia implements [RemoteFetcher].
func (f RemoteFetcherFunc) FetchMedia(ctx context.Context, origin, path string) (*RemoteMedia, error) {
	return f(ctx, origin, path)
}

// WithRemoteFetcher installs the way this server obtains files that live elsewhere.
//
// Without it, a request for another server's media is a 404 — the right answer for a server that is
// not federating, and a silent broken image for one that is.
func (s *Service) WithRemoteFetcher(fetcher RemoteFetcher) *Service {
	s.remote = fetcher
	return s
}

// remoteMediaPaths are the resources an origin might serve this file from, in the order they should be
// asked for.
//
// Three spellings exist because the endpoint has been redrawn twice in the open. The current
// specification's is the authenticated `/_matrix/federation/v1/download/{serverName}/{mediaId}` and it
// goes first: it is the one that says who is asking, and it is the only one a new implementation is
// obliged to speak. Behind it comes the older unauthenticated `/_matrix/media/v3/download/...`, which
// deployments and reference implementations still rely on for federation because it predates the
// redraw. Last is the draft `/_matrix/federation/v1/media/download/{mediaId}` — the shape this server
// grew first, and the one another of this server's own deployments will still be serving.
//
// Asking in order rather than guessing once is the difference between a media ID that works across the
// federation and a broken image in a room on whichever side happened to be newer.
func remoteMediaPaths(origin, mediaID string) []string {
	escapedServer := url.PathEscape(origin)
	escapedMedia := url.PathEscape(mediaID)
	return []string{
		"/_matrix/federation/v1/download/" + escapedServer + "/" + escapedMedia,
		"/_matrix/media/v3/download/" + escapedServer + "/" + escapedMedia,
		"/_matrix/federation/v1/media/download/" + escapedMedia,
	}
}

// fetchRemote asks the origin for a file and keeps a copy of it.
//
// Why this is not the open proxy it resembles at first glance: the caller names a media ID and an
// origin *server*, and what leaves this process is a signed GET to one fixed federation path on that
// server — the same shape every other outbound federation call has. There is no way to make this
// server fetch a URL. What a caller can do is ask their own homeserver for media whose origin they
// name, which is the behaviour this exists to provide.
//
// Why the copy is kept: without it, every re-render of an image asks the origin again, and the day the
// origin deletes the file or leaves the network a room loses an image it has been displaying for
// months. The specification asks servers to cache for exactly this reason.
//
// What bounds it: the declared length and the bytes actually read are both capped by this server's own
// upload limit, so this server never stores a file it would refuse to serve — and the content type is
// served with the same headers an upload gets, so a fetched HTML file is no more able to execute on
// this origin than an uploaded one is.
func (s *Service) fetchRemote(ctx context.Context, origin, mediaID string) (*store.MediaFile, *os.File, error) {
	if s.remote == nil {
		return nil, nil, matrix.NewNotFound("Media %q:%q was not found on this homeserver", origin, mediaID)
	}
	paths := remoteMediaPaths(origin, mediaID)
	var fetched *RemoteMedia
	var err error
	for i, path := range paths {
		fetched, err = s.remote.FetchMedia(ctx, origin, path)
		if err == nil && fetched != nil && fetched.Body != nil {
			break
		}
		if err == nil {
			err = io.ErrNoProgress
		}
		// A peer that has never heard of a route says the same thing as a peer that has never
		// heard of this file, so every spelling gets its turn before this server concludes the
		// origin does not have it.
		slog.Debug("media: this path did not yield the file from its origin",
			"origin", origin, "media_id", loggableID(mediaID), "path", path,
			"attempt", i+1, "of", len(paths), "error", err)
	}
	if err != nil {
		// Logged with the origin, because "that server refused" and "that server does not exist"
		// look identical to the user and completely different to an operator.
		slog.Warn("media: could not fetch a file from its origin server",
			"origin", origin, "media_id", loggableID(mediaID), "error", err)
		return nil, nil, matrix.NewNotFound("Media %q:%q could not be fetched from its homeserver", origin, mediaID)
	}
	if fetched == nil || fetched.Body == nil {
		return nil, nil, matrix.NewNotFound("Media %q:%q could not be fetched from its homeserver", origin, mediaID)
	}
	defer func() { _ = fetched.Body.Close() }()
	if fetched.Size > s.maxBytes {
		return nil, nil, matrix.NewNotFound("Media %q:%q is larger than this homeserver will store", origin, mediaID)
	}

	// The copy is stored under a fresh local ID. Reusing the origin's ID would leave this server's
	// table holding rows whose IDs mean different things depending on where they came from, and a
	// download for (this server, that ID) would then have to be refused by reading the origin column
	// very carefully — a rule that survives exactly until somebody forgets to read it.
	localID := newMediaID()
	path := s.pathFor(localID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, nil, fmt.Errorf("creating the media directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), localID+"-*")
	if err != nil {
		return nil, nil, fmt.Errorf("opening a temporary media file: %w", err)
	}
	tempName := file.Name()
	defer func() { _ = os.Remove(tempName) }()

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(fetched.Body, s.maxBytes+1))
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("reading %s's answer: %w", origin, err)
	}
	if err := file.Close(); err != nil {
		return nil, nil, fmt.Errorf("closing the fetched media: %w", err)
	}
	if written > s.maxBytes {
		return nil, nil, matrix.NewNotFound("Media %q:%q is larger than this homeserver will store", origin, mediaID)
	}
	if err := os.Rename(tempName, path); err != nil {
		return nil, nil, fmt.Errorf("storing the fetched file: %w", err)
	}

	contentType := strings.TrimSpace(fetched.ContentType)
	if contentType == "" {
		// The origin said nothing. Guessing is what turns a file into a script, so the type that no
		// browser will render is the honest one.
		contentType = "application/octet-stream"
	}
	row := &store.MediaFile{
		ID: localID, Origin: origin, UpstreamID: mediaID, Localpart: remoteMediaLocalpart,
		ContentType: contentType, FileName: clampFileName(fetched.FileName),
		SHA256: hex.EncodeToString(hash.Sum(nil)), Size: written,
		CreatedMS: time.Now().UnixMilli(),
	}
	if err := s.store.Media.Put(ctx, row); err != nil {
		_ = os.Remove(path)
		return nil, nil, err
	}
	slog.Info("media: cached a file fetched from its origin server",
		"media_id", loggableID(mediaID), "origin", origin, "size", written, "content_type", contentType)

	// The row handed to the caller describes the media that was *asked for*, not this server's private
	// copy of it: the local ID is this server's business, and returning it would let a cache entry be
	// mistaken for an upload of ours.
	served := *row
	served.ID = mediaID
	handle, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the fetched file: %w", err)
	}
	return &served, handle, nil
}

// clampFileName keeps a peer's suggested name inside the length this server will put in a header.
func clampFileName(name string) string {
	if len(name) <= MaxFileNameLength {
		return name
	}
	return name[:MaxFileNameLength]
}
