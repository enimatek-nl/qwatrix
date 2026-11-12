package fedapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/media"
	"github.com/enimatek-nl/qwatrix/internal/store"
	fclient "github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// MediaSource serves the files this server originates to the peers that ask for them.
//
// It is deliberately one method with the origin check inside it: a federation handler that could ask
// for "any file this server holds, including copies it borrowed from somebody else" would eventually
// be given one, and that is exactly how a server ends up publishing another server's content under its
// own name.
type MediaSource interface {
	// OriginFile returns a file this server received from one of its own users, or a Matrix
	// not-found error for anything else — including this server's cached copies of foreign files.
	OriginFile(ctx context.Context, mediaID string) (*store.MediaFile, *os.File, error)
}

// handleFederationMedia answers GET /_matrix/federation/v1/media/download/{mediaID} for a peer.
//
// Signed, like every endpoint that says something about this server's users or rooms. The bytes go out
// through the same writer an authenticated client download uses, so the three headers that make an
// uploaded file safe to view are not a rule this endpoint can step around.
func (h *Handler) handleFederationMedia(w http.ResponseWriter, r *http.Request) {
	h.serveOriginFile(w, r, r.PathValue("mediaID"), "")
}

// handleFederationMediaByServerName answers the specified form of the same question,
// GET /_matrix/federation/v1/download/{serverName}/{mediaID}, which names the origin in the path.
//
// A peer asks this server for `this.example`'s file, so a request naming anybody else is a question
// this server cannot answer honestly: it does not have those bytes, and its cached copies of other
// servers' files are deliberately not served here (see [Handler.serveOriginFile) — serving them would
// turn every media ID in the federation into a way to read them from anybody who once saw them.
func (h *Handler) handleFederationMediaByServerName(w http.ResponseWriter, r *http.Request) {
	h.serveOriginFile(w, r, r.PathValue("mediaID"), r.PathValue("serverName"))
}

// serveOriginFile is the one implementation behind both spellings of the federation download.
func (h *Handler) serveOriginFile(w http.ResponseWriter, r *http.Request, mediaID, serverName string) {
	origin := h.authenticatePeer(w, r)
	if origin == nil {
		return
	}
	if serverName != "" && !strings.EqualFold(serverName, string(h.opts.ServerName)) {
		httpx.RespondError(w, r, matrix.NewNotFound("This homeserver does not serve media for %q", serverName))
		return
	}
	if h.opts.Media == nil {
		httpx.RespondError(w, r, matrix.NewNotFound("This homeserver does not serve media over federation"))
		return
	}
	row, body, err := h.opts.Media.OriginFile(r.Context(), mediaID)
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()
	slog.Debug("fedapi: served a file to a peer",
		"origin", string(origin.Origin()), "media_id", shortMediaID(mediaID))
	media.Respond(w, row, body)
}

// shortMediaID keeps a media identifier out of a log line in a form that could be downloaded. See
// the same reasoning in internal/media.
func shortMediaID(id string) string {
	if len(id) <= 8 {
		return "[short]"
	}
	return id[:8] + "…"
}

// RemoteFile is a file this server downloaded from the server that originates it.
type RemoteFile struct {
	// ContentType and FileName are the origin's own.
	ContentType string
	FileName    string
	// Body is the content; the caller closes it.
	Body io.ReadCloser
	// Size is the origin's declared length, or -1 when it declared none.
	Size int64
}

// GetSignedFile asks a peer for a file it originates, over a signed federation GET.
//
// The body is streamed rather than buffered: a media file is measured in megabytes and is handed to the
// caller as a stream, so reading a hundred megabytes into memory in order to pass it on would make this
// server the bottleneck it is trying not to be.
func (p *Peer) GetSignedFile(ctx context.Context, server spec.ServerName, path string) (*RemoteFile, error) {
	identity, err := signingIdentity(p.opts)
	if err != nil {
		return nil, fmt.Errorf("federation: %s", err)
	}
	request := fclient.NewFederationRequest(http.MethodGet, p.opts.ServerName, server, path)
	// Signed before the HTTP request is built, because the signature covers the method and the URI.
	if err := request.Sign(identity.ServerName, identity.KeyID, identity.PrivateKey); err != nil {
		return nil, fmt.Errorf("federation: signing the file request for %s: %w", server, err)
	}
	httpRequest, err := request.HTTPRequest()
	if err != nil {
		return nil, fmt.Errorf("federation: building the file request for %s: %w", server, err)
	}
	httpRequest = httpRequest.WithContext(ctx)

	response, err := p.transport.RoundTrip(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("federation: asking %s for %s: %w", server, path, err)
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
		return nil, fmt.Errorf("federation: %s answered %s with %s", server, path, response.Status)
	}
	return &RemoteFile{
		ContentType: response.Header.Get("Content-Type"),
		FileName:    contentDispositionFileName(response.Header.Get("Content-Disposition")),
		Body:        response.Body,
		// -1 when the peer declared no length, which the caller treats as "find out while reading".
		Size: response.ContentLength,
	}, nil
}

// contentDispositionFileName pulls the suggested name out of a Content-Disposition header.
//
// A malformed header yields no name rather than an error: the name is a courtesy, and refusing a file
// because its origin spelled the name badly would be a strange thing to do to a user looking at a photo.
func contentDispositionFileName(header string) string {
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return params["filename"]
}
