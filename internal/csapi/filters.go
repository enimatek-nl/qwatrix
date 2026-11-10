package csapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/filters"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// registerFilterRoutes registers the saved-filter endpoints.
func registerFilterRoutes(mux *http.ServeMux, deps Deps) {
	for _, path := range versionedPath("user/{userID}/filter") {
		mux.HandleFunc("POST "+path, deps.requireAuth(handleCreateFilter(deps)))
	}
	for _, path := range versionedPath("user/{userID}/filter/{filterID}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetFilter(deps)))
	}
}

// filterOwner resolves the {userID} of a filter endpoint and checks it is the caller.
//
// Filters are per-account, and the ID space is per-account too, so one account reading another's
// filter would not just list it — it could then use that ID on /sync and be served a stream shaped
// by somebody else's rules.
func filterOwner(deps Deps, r *http.Request) (string, error) {
	session := httpx.MustSession(r)
	localpart, err := deps.localOfPathUser(r)
	if err != nil {
		return "", err
	}
	if localpart != session.Localpart {
		return "", matrix.NewForbidden("You can only use your own filters")
	}
	return localpart, nil
}

// handleCreateFilter answers POST /user/{userID}/filter.
func handleCreateFilter(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := filterOwner(deps, r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}

		var definition json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&definition); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}
		// Anything but an object is refused: a filter is a set of rules, and rules that cannot
		// be parsed would silently mean "no filtering", which is the one thing a client cannot
		// tell apart from its filter having been ignored.
		if !filters.Valid(definition) {
			httpx.RespondError(w, r, matrix.NewBadJSON("a filter must be a JSON object"))
			return
		}
		// Shape is checked before storing. A filter that quietly matches nothing is worse than
		// one that is refused: the client sees an empty timeline and is told its filter worked.
		if err := filters.Validate(definition); err != nil {
			httpx.RespondError(w, r, matrix.NewInvalidParam("%s", err.Error()))
			return
		}
		if _, err := filters.Parse(definition); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}

		var created *store.Filter
		if err := deps.Store.Do(r.Context(), func(ctx context.Context, tx store.Tx) error {
			var err error
			created, err = deps.Store.Filters.Create(ctx, localpart, string(definition))
			return err
		}); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]string{"filter_id": created.ID})
	}
}

// handleGetFilter answers GET /user/{userID}/filter/{filterID}.
func handleGetFilter(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localpart, err := filterOwner(deps, r)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		stored, err := deps.Store.Filters.Get(r.Context(), localpart, r.PathValue("filterID"))
		if err != nil {
			if store.IsNotFound(err) {
				// The spec's answer for an unknown filter ID is a 400, not a 404: the ID came
				// from a query parameter on an endpoint that exists, so this is a bad request
				// rather than a missing resource.
				httpx.RespondError(w, r, matrix.NewAPIError(http.StatusBadRequest, matrix.ErrInvalidParam,
					"Unknown filter"))
				return
			}
			httpx.RespondError(w, r, err)
			return
		}
		// The definition goes back exactly as it was stored, so a client reads back the keys
		// this server does not act on rather than a lossy summary of them.
		httpx.RespondRawJSON(w, http.StatusOK, json.RawMessage(stored.Definition))
	}
}
