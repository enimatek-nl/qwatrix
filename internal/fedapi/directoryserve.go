package fedapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
)

// DirectoryServer answers two questions a peer asks about this server's front door: which rooms it
// publishes, and what one of its aliases points at.
type DirectoryServer interface {
	// PublicRooms lists the rooms this server publishes. The request must name no server: this is
	// the local list, and a peer asking about a third server is a peer that should ask it directly.
	PublicRooms(ctx context.Context, req roomserver.PublicRoomsRequest) (*roomserver.PublicRoomsPage, error)
	// LocalRoomIDForAlias resolves an alias this server holds, without asking anybody else.
	LocalRoomIDForAlias(ctx context.Context, alias string) (string, error)
}

// handlePublicRooms serves `GET`/`POST /_matrix/federation/v1/publicRooms`.
//
// This is the endpoint that puts a room on another server's explore pane, and therefore the one that
// decides whether a public room is discoverable at all: a room this server publishes but does not
// offer here exists, and is invisible. The answer is deliberately not restricted to peers who hold
// anything in the room — the room server has already decided what is publishable, and a room marked
// public is public to the federation, which is what a room directory is for.
//
// Both spellings are served from one handler: the GET carries its parameters in the query, and the
// newer POST carries them in a body so a filter object fits.
func (h *Handler) handlePublicRooms(w http.ResponseWriter, r *http.Request) {
	if h.authenticatePeer(w, r) == nil {
		return
	}
	if h.opts.Directory == nil {
		httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not serve its room directory"))
		return
	}
	query := r.URL.Query()
	if r.Method == http.MethodPost {
		var body struct {
			Limit  int    `json:"limit"`
			Since  string `json:"since"`
			Filter struct {
				GenericSearchTerm string `json:"generic_search_term"`
			} `json:"filter"`
		}
		if err := httpx.DecodeJSONIfPresent(r, &body); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if body.Limit > 0 {
			query.Set("limit", strconv.Itoa(body.Limit))
		}
		if body.Since != "" {
			query.Set("since", body.Since)
		}
		if body.Filter.GenericSearchTerm != "" {
			query.Set("filter", body.Filter.GenericSearchTerm)
		}
	}
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			httpx.RespondError(w, r, matrix.NewInvalidParam("limit must be an integer"))
			return
		}
		limit = parsed
	}
	page, err := h.opts.Directory.PublicRooms(r.Context(), roomserver.PublicRoomsRequest{
		Limit:  limit,
		Since:  query.Get("since"),
		Filter: query.Get("filter"),
	})
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	answer := map[string]any{"chunk": page.Chunk}
	if page.NextBatch != "" {
		answer["next_batch"] = page.NextBatch
	}
	if page.PrevBatch != "" {
		answer["prev_batch"] = page.PrevBatch
	}
	if page.TotalRooms > 0 {
		// The federation spelling of the count has always been an estimate under a longer name,
		// and a peer reading `total_rooms` here finds nothing it expected.
		answer["total_room_count_estimate"] = page.TotalRooms
	}
	httpx.RespondJSON(w, http.StatusOK, answer)
}

// handleResolveAlias serves `GET /_matrix/federation/v1/resolve/{roomIDOrAlias}` — the endpoint the
// current specification defines for turning an alias into a room ID.
//
// This server answers for aliases that carry its own name and for nothing else. Forwarding a lookup
// for `#room:somewhere.else` would mean answering for what other servers say, and doing it whenever
// an anonymous requester asked would make this server a proxy in front of them.
func (h *Handler) handleResolveAlias(w http.ResponseWriter, r *http.Request) {
	if h.authenticatePeer(w, r) == nil {
		return
	}
	if h.opts.Directory == nil {
		httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not serve alias resolution"))
		return
	}
	nameOrAlias := r.PathValue("roomIDOrAlias")
	_, host, err := matrix.ParseAlias(nameOrAlias)
	if err != nil {
		// A room ID asked of this endpoint asks a question this endpoint cannot answer, and a room
		// ID is already the thing a resolution produces.
		httpx.RespondError(w, r, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"%q is not a room alias", nameOrAlias))
		return
	}
	if !strings.EqualFold(host, string(h.opts.ServerName)) {
		httpx.RespondError(w, r, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"Alias %q does not belong to this homeserver", nameOrAlias))
		return
	}
	roomID, err := h.opts.Directory.LocalRoomIDForAlias(r.Context(), nameOrAlias)
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]any{
		"origin": string(h.opts.ServerName),
		"rooms":  []string{roomID},
	})
}

// handleQueryDirectory serves the older `GET /_matrix/federation/v1/query/directory?room_alias=…`.
//
// It is the spelling gomatrixserverlib speaks, and therefore what a good part of the federation
// actually sends when it wants to know what an alias means. It stays because a server that answers
// only the modern endpoint is a server whose aliases half the federation cannot resolve, and the
// person who notices is the one trying to join.
func (h *Handler) handleQueryDirectory(w http.ResponseWriter, r *http.Request) {
	if h.authenticatePeer(w, r) == nil {
		return
	}
	if h.opts.Directory == nil {
		httpx.RespondError(w, r, matrix.NewUnknown("this homeserver does not serve alias resolution"))
		return
	}
	alias := r.URL.Query().Get("room_alias")
	_, host, err := matrix.ParseAlias(alias)
	if err != nil {
		httpx.RespondError(w, r, matrix.NewInvalidParam("room_alias must be a room alias"))
		return
	}
	if !strings.EqualFold(host, string(h.opts.ServerName)) {
		httpx.RespondError(w, r, matrix.NewAPIError(http.StatusNotFound, matrix.ErrNotFound,
			"Alias %q does not belong to this homeserver", alias))
		return
	}
	roomID, err := h.opts.Directory.LocalRoomIDForAlias(r.Context(), alias)
	if err != nil {
		httpx.RespondError(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]any{
		"room_id": roomID,
		"servers": []string{string(h.opts.ServerName)},
	})
}
