package csapi

import (
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/media"
)

// registerMediaRoutes registers the media endpoints.
//
// Both media APIs are served. The legacy `/_matrix/media/v3/*` paths are what existing clients and
// Complement use, and the newer `/_matrix/client/v1/media/*` paths are the same operations under the
// authenticated client prefix — the difference is not cosmetic, it is the spec's answer to the fact
// that an unauthenticated media read lets any third party who obtains a media URI read it forever.
// Serving only one of the two leaves either old clients or the security fix unimplemented, and the
// two are not interchangeable: one answers 401 to an anonymous request and the other does not.
func registerMediaRoutes(mux *httpx.RouteTable, deps Deps) {
	// Upload requires a token: an anonymous upload endpoint turns a homeserver into free file
	// hosting for anybody with a URL, which is the opposite of what a chat server is for.
	mux.HandleFunc("POST /_matrix/media/v3/upload", deps.requireAuth(handleUpload(deps)))
	mux.HandleFunc("POST /_matrix/media/v1/upload", deps.requireAuth(handleUpload(deps)))

	// Legacy downloads are served with optional authentication, and the authenticated variants
	// demand it — which is the distinction the two prefixes exist to make.
	mux.HandleFunc("GET /_matrix/media/v3/download/{serverName}/{mediaID}",
		deps.optionalAuth(handleDownload(deps)))
	mux.HandleFunc("HEAD /_matrix/media/v3/download/{serverName}/{mediaID}",
		deps.optionalAuth(handleDownload(deps)))
	mux.HandleFunc("GET /_matrix/media/v1/download/{serverName}/{mediaID}",
		deps.optionalAuth(handleDownload(deps)))
	// The patterns are spelled out rather than built from a prefix: the documentation check reads
	// the routes out of this file's literals, and a computed pattern is a route it cannot see and
	// a reader cannot find.
	mux.HandleFunc("GET /_matrix/client/v1/media/download/{serverName}/{mediaID}", deps.requireAuth(handleDownload(deps)))
	// The filename-bearing form takes the name from the *request*, not from what was uploaded: it is
	// the name the user saw when they saved the file, and a client that put "invoice.pdf" in the URL
	// should get a response that agrees with it. The media ID remains the only lookup key, so a wrong
	// name cannot fetch a different file.
	mux.HandleFunc("GET /_matrix/media/v3/download/{serverName}/{mediaID}/{fileName}",
		deps.optionalAuth(handleDownload(deps)))
	mux.HandleFunc("HEAD /_matrix/media/v3/download/{serverName}/{mediaID}/{fileName}",
		deps.optionalAuth(handleDownload(deps)))
	mux.HandleFunc("GET /_matrix/client/v1/media/download/{serverName}/{mediaID}/{fileName}", deps.requireAuth(handleDownload(deps)))
	mux.HandleFunc("GET /_matrix/client/v1/media/config", deps.requireAuth(handleMediaConfig(deps)))
	mux.HandleFunc("GET /_matrix/media/v3/config", deps.optionalAuth(handleMediaConfig(deps)))
	mux.HandleFunc("GET /_matrix/media/v1/config", deps.optionalAuth(handleMediaConfig(deps)))
}

// handleUpload answers POST /_matrix/media/v3/upload.
//
// The body *is* the file, and the content type and filename are the query string — which is why this
// handler never touches a JSON decoder: the request has no body-shaped structure, and the size limit
// has to be enforced while the bytes stream past rather than after they have all been read.
func handleUpload(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		uri, err := deps.Media.Upload(r.Context(), session.Localpart,
			r.URL.Query().Get("filename"),
			r.Header.Get("Content-Type"),
			r.Body)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"content_uri": uri})
	}
}

// handleDownload answers the download endpoints.
func handleDownload(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		row, body, err := deps.Media.Download(r.Context(), r.PathValue("serverName"),
			r.PathValue("mediaID"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if requested := r.PathValue("fileName"); requested != "" {
			// The row is copied rather than edited: it is the metadata of a file other requests are
			// answering from, and this request's chosen name is not a fact about the stored file.
			renamed := *row
			renamed.FileName = requested
			row = &renamed
		}
		// The file handle outlives this function, so it is closed here rather than by the
		// caller: the response has been fully written by the time the handler returns.
		defer func() { _ = body.Close() }()
		if r.Method == http.MethodHead {
			media.Respond(w, row, http.NoBody)
			return
		}
		media.Respond(w, row, body)
	}
}

// handleMediaConfig answers the config endpoints, which exist so a client can refuse an upload the
// server would reject rather than discovering the limit after sending a hundred megabytes.
func handleMediaConfig(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Media == nil {
			httpx.RespondError(w, r, matrix.NewUnknown("The media repository is not enabled"))
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{
			"m.upload.size": deps.Media.MaxUploadSize(),
		})
	}
}
