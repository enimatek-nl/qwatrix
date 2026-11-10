// Package media stores and serves the files a homeserver holds: images, files, and the thumbnails
// derived from them.
//
// Media is where a homeserver stops being a metadata service and starts being a file host, and that
// changes what it has to be careful about. An upload is unbounded input from an authenticated user; a
// download is a URL that anybody can hand to a victim's client; a stored file's content type is
// attacker-chosen text that a browser will obey. So the three decisions in this package are about
// size, identity, and content type:
//
//   - uploads are capped, and the cap is checked while streaming rather than after buffering, so a
//     single request cannot exhaust the disk or the process's memory;
//   - media IDs are random, so that holding one is the capability and guessing another user's file
//     is not possible;
//   - the content type is what the uploader declared, but served with `Content-Security-Policy:
//     default-src 'none'` and `X-Content-Type-Options: nosniff` so that opening a media URL in a
//     browser cannot run the uploaded file as a script on the server's own origin.
package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// MaxFileNameLength bounds the echoed upload name. Long names are not a feature; they are a way to
// stuff a Content-Disposition header full of text a client may render.
const MaxFileNameLength = 1024

// Service is the media repository.
type Service struct {
	store      *store.Store
	dir        string
	serverName string
	maxBytes   int64
}

// Options configures a [Service].
type Options struct {
	// Dir is where file contents live, normally <data-dir>/media.
	Dir string
	// ServerName is the origin this server serves media for.
	ServerName string
	// MaxBytes caps one upload.
	MaxBytes int64
}

// New builds the media repository.
func New(st *store.Store, opts Options) (*Service, error) {
	if opts.Dir == "" {
		return nil, errors.New("media: a storage directory is required")
	}
	// Files live in a two-level tree so that a directory with a million entries stays a
	// theoretical problem rather than a real one: the ID is opaque, so any prefix will do.
	if err := os.MkdirAll(filepath.Join(opts.Dir, "media"), 0o750); err != nil {
		return nil, fmt.Errorf("creating the media directory: %w", err)
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 100 << 20
	}
	return &Service{store: st, dir: opts.Dir, serverName: opts.ServerName, maxBytes: opts.MaxBytes}, nil
}

// MaxUploadSize returns the configured cap, which is what /config advertises.
func (s *Service) MaxUploadSize() int64 { return s.maxBytes }

// Upload stores one file and returns its mxc:// URI.
//
// The bytes are written before the metadata row, and removed again if the row cannot be written: a
// row without bytes would be a download that 500s forever, and an orphan without a row is a file
// nobody can name and can be swept by size. The hash is computed on the way through rather than in
// memory, because the point of a limit is to bound memory as well as disk.
func (s *Service) Upload(ctx context.Context, localpart, fileName, contentType string, body io.Reader) (string, error) {
	if contentType == "" {
		// An absent type is not "text/plain" and not "application/octet-stream" by
		// accident: the caller's silence is recorded, and a download of a file the server
		// was never told about is served as a stream, which no renderer will execute.
		contentType = "application/octet-stream"
	}
	if len(fileName) > MaxFileNameLength {
		return "", matrix.NewInvalidParam("filename is longer than %d bytes", MaxFileNameLength)
	}
	id := newMediaID()
	path := s.pathFor(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("creating the media directory: %w", err)
	}

	file, err := os.CreateTemp(filepath.Dir(path), id+"-*")
	if err != nil {
		return "", fmt.Errorf("opening a temporary media file: %w", err)
	}
	tempName := file.Name()
	defer func() {
		// A failed upload leaves nothing behind. The name may still exist if the write
		// got partway, and an orphaned fragment is invisible to every query.
		_ = os.Remove(tempName)
	}()

	hash := sha256.New()
	// LimitReader of max+1 so that an oversized body is detected rather than silently
	// truncated: the extra byte is what distinguishes "exactly at the limit" from "over it".
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, s.maxBytes+1))
	if err != nil {
		_ = file.Close()
		return "", fmt.Errorf("reading the upload: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing the upload: %w", err)
	}
	if written > s.maxBytes {
		return "", matrix.NewAPIError(http.StatusRequestEntityTooLarge, matrix.ErrTooLarge,
			"File is too large: %d bytes uploaded, the limit is %d", written, s.maxBytes)
	}
	// An empty file is accepted. It is what a client writes when a user attaches a zero-length
	// file, and refusing it would turn an uninteresting upload into an error the user cannot
	// understand; the size limit above is the check that actually protects anything.
	if err := os.Rename(tempName, path); err != nil {
		return "", fmt.Errorf("storing the uploaded file: %w", err)
	}

	row := &store.MediaFile{
		ID: id, Origin: s.serverName, Localpart: localpart,
		ContentType: contentType, FileName: fileName,
		SHA256: hex.EncodeToString(hash.Sum(nil)), Size: written,
		CreatedMS: time.Now().UnixMilli(),
	}
	if err := s.store.Media.Put(ctx, row); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	slog.Info("media: stored a file", "media_id", id, "size", written, "content_type", contentType)
	return "mxc://" + s.serverName + "/" + id, nil
}

// Download looks a file up and returns its metadata.
//
// A file from another server is not fetched here: without federation this server has no way to
// verify what it would receive, and a media cache that will fetch and serve arbitrary URLs on demand
// is a way to make this server an open proxy for whoever can name an origin. The answer is therefore
// "not found", which is also what the specification allows for media this server does not hold.
func (s *Service) Download(ctx context.Context, origin, mediaID string) (*store.MediaFile, *os.File, error) {
	if origin != "" && origin != s.serverName {
		return nil, nil, matrix.NewNotFound("Media %q:%q was not found on this homeserver", origin, mediaID)
	}
	row, err := s.store.Media.Get(ctx, mediaID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil, matrix.NewNotFound("Media %q was not found on this homeserver", mediaID)
		}
		return nil, nil, err
	}
	handle, err := os.Open(s.pathFor(row.ID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The row outlived the bytes. That is a bug worth a log and a 404: the
			// alternative is a download that hangs or errors in a way that looks like the
			// client's fault.
			slog.Error("media: metadata has no file on disk", "media_id", row.ID, "error", err)
			return nil, nil, matrix.NewNotFound("Media %q was not found on this homeserver", mediaID)
		}
		return nil, nil, fmt.Errorf("opening media %q: %w", mediaID, err)
	}
	return row, handle, nil
}

// Respond writes a file to the response with the headers that make it safe to view.
//
// The content type is the uploader's, which is what makes an image render — and what would make an
// uploaded HTML page execute on this server's origin if it were fetched from a browser. The three
// security headers are what lets the server keep the first half without the second.
func Respond(w http.ResponseWriter, row *store.MediaFile, body io.Reader) {
	w.Header().Set("Content-Type", row.ContentType)
	if row.FileName != "" {
		// The name is quoted and its quotes and backslashes are escaped: it is attacker text
		// appearing inside a header, and an unescaped quote there ends the parameter and lets
		// the rest of the string be parsed as another header parameter.
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, escapeFileName(row.FileName)))
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", row.Size))
	w.Header().Set("Cache-Control", "private, no-cache, no-store, no-transform, must-revalidate")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; navigate-to 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, body); err != nil {
		slog.Warn("media: writing a file to a client failed", "media_id", row.ID, "error", err)
	}
}

// pathFor maps a media ID onto its file. The ID is opaque, so the path is derived rather than
// trusted; keeping the derivation in one place is what makes traversal impossible to reintroduce.
func (s *Service) pathFor(id string) string {
	safe := sanitizeID(id)
	if len(safe) < 4 {
		return filepath.Join(s.dir, "media", safe)
	}
	return filepath.Join(s.dir, "media", safe[:2], safe[2:4], safe)
}

// sanitizeID strips anything a path could take an interest in. Uploaded IDs never contain these
// characters, so this is a guard rather than a transformation.
func sanitizeID(id string) string {
	id = strings.ReplaceAll(id, "/", "")
	id = strings.ReplaceAll(id, "\\", "")
	id = strings.ReplaceAll(id, "..", "")
	return id
}

// newMediaID mints an opaque media ID.
//
// Random rather than sequential because a media URI is the only thing standing between a file and
// anybody who wants it: a client has no way to authorise a media read, so the ID is the capability,
// and an ID that can be enumerated is a file share with the whole world.
func newMediaID() string {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// A failure here means the system has no entropy, which is a problem far bigger
		// than the naming scheme, but a media ID still has to be unique — so fall back to
		// a timestamp, which is at least unlikely to collide, and let the caller's next
		// upload hit the same wall.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// escapeFileName makes a user-supplied name safe to put inside a quoted header parameter.
func escapeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "\\\\")
	name = strings.ReplaceAll(name, `"`, `\"`)
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	return name
}
