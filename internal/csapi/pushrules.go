package csapi

import (
	"encoding/json"
	"net/http"

	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/pushrules"
)

// registerPushRuleRoutes registers the push-rule endpoints.
//
// The trailing-slash spelling is registered because that is how clients ask for the whole set: the
// specification's path for "all rules" is `/pushrules/`, and the path-joining rule that produces it
// is a known wart (matrix-spec#457) that clients work around by adding the slash. A server that
// answers 404 there has not declined a malformed request, it has refused the only URL a client has.
func registerPushRuleRoutes(mux *http.ServeMux, deps Deps) {
	for _, path := range versionedPath("pushrules") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetAllPushRules(deps)))
	}
	for _, path := range versionedPath("pushrules/") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetAllPushRules(deps)))
	}
	// The `/actions` and `/enabled` sub-resources are single fields of a rule with their own
	// endpoints, which is why they are separate routes rather than a PATCH of the rule.
	for _, path := range versionedPath("pushrules/{scope}/{kind}/{ruleID}/actions") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetPushRulePart(deps, partActions)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutPushRuleActions(deps)))
	}
	for _, path := range versionedPath("pushrules/{scope}/{kind}/{ruleID}/enabled") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetPushRulePart(deps, partEnabled)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutPushRuleEnabled(deps)))
	}
	for _, path := range versionedPath("pushrules/{scope}/{kind}/{ruleID}/pattern") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetPushRulePart(deps, partPattern)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutPushRulePattern(deps)))
	}
	for _, path := range versionedPath("pushrules/{scope}/{kind}/{ruleID}") {
		mux.HandleFunc("GET "+path, deps.requireAuth(handleGetPushRule(deps)))
		mux.HandleFunc("PUT "+path, deps.requireAuth(handlePutPushRule(deps)))
		mux.HandleFunc("DELETE "+path, deps.requireAuth(handleDeletePushRule(deps)))
	}
}

// RulePart names the single field of a rule that has its own endpoint.
type RulePart string

const (
	partActions RulePart = "actions"
	partEnabled RulePart = "enabled"
	partPattern RulePart = "pattern"
)

// pushRuleScope checks the scope, which the specification fixes to `global`.
//
// Answering a request for a scope this server does not have with an empty rule set would be the
// worst option available: a client asking for device-local rules and getting `{}` concludes that it
// has no rules, and may then "helpfully" create the defaults it expected to find. Refusing tells the
// client that the feature is missing, which is the truth.
func pushRuleScope(r *http.Request) (string, error) {
	scope := r.PathValue("scope")
	if scope != pushrules.Scope {
		return "", matrix.NewInvalidParam("unsupported push rule scope %q, only %q exists", scope, pushrules.Scope)
	}
	return scope, nil
}

// handleGetAllPushRules answers GET /pushrules/.
func handleGetAllPushRules(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		grouped, err := deps.PushRules.Rules(r.Context(), session.Localpart)
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, map[string]any{"global": grouped})
	}
}

// handleGetPushRule answers GET /pushrules/{scope}/{kind}/{ruleID}.
func handleGetPushRule(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if _, err := pushRuleScope(r); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		rule, err := deps.PushRules.Rule(r.Context(), session.Localpart, r.PathValue("kind"),
			r.PathValue("ruleID"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, rule)
	}
}

// handleGetPushRulePart answers the single-field reads.
func handleGetPushRulePart(deps Deps, part RulePart) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if _, err := pushRuleScope(r); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		rule, err := deps.PushRules.Rule(r.Context(), session.Localpart, r.PathValue("kind"),
			r.PathValue("ruleID"))
		if err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		switch part {
		case partActions:
			httpx.RespondJSON(w, http.StatusOK, map[string]any{"actions": rule.Actions})
		case partEnabled:
			httpx.RespondJSON(w, http.StatusOK, map[string]any{"enabled": rule.Enabled})
		case partPattern:
			httpx.RespondJSON(w, http.StatusOK, map[string]any{"pattern": rule.Pattern})
		}
	}
}

// handlePutPushRule answers PUT /pushrules/{scope}/{kind}/{ruleID}, which is how a rule comes into
// existence at all: the body carries the actions and, for a content rule, the pattern.
func handlePutPushRule(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if _, err := pushRuleScope(r); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		var body struct {
			Actions    *[]any  `json:"actions"`
			Pattern    *string `json:"pattern"`
			Conditions *[]any  `json:"conditions"`
			Enabled    *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}
		if body.Actions == nil {
			httpx.RespondError(w, r, matrix.NewMissingParam("actions"))
			return
		}
		// Conditions are accepted and echoed, not evaluated. Deciding whether a rule matches is
		// the push gateway's job at delivery time; the homeserver only has to remember what the
		// client asked for and hand the rules to whatever does the matching.
		kind, ruleID := r.PathValue("kind"), r.PathValue("ruleID")
		if err := deps.PushRules.SetActions(r.Context(), session.Localpart, kind, ruleID, *body.Actions); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if body.Pattern != nil {
			if err := deps.PushRules.SetPattern(r.Context(), session.Localpart, kind, ruleID, *body.Pattern); err != nil {
				httpx.RespondError(w, r, err)
				return
			}
		}
		if body.Enabled != nil {
			if err := deps.PushRules.SetEnabled(r.Context(), session.Localpart, kind, ruleID, *body.Enabled); err != nil {
				httpx.RespondError(w, r, err)
				return
			}
		}
		if before, after := r.URL.Query().Get("before"), r.URL.Query().Get("after"); before != "" || after != "" {
			if err := deps.PushRules.Move(r.Context(), session.Localpart, kind, ruleID, before, after); err != nil {
				httpx.RespondError(w, r, err)
				return
			}
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}

// handlePutPushRuleActions answers PUT /pushrules/{scope}/{kind}/{ruleID}/actions.
func handlePutPushRuleActions(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if _, err := pushRuleScope(r); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		var body struct {
			Actions *[]any `json:"actions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}
		if body.Actions == nil {
			httpx.RespondError(w, r, matrix.NewMissingParam("actions"))
			return
		}
		if err := deps.PushRules.SetActions(r.Context(), session.Localpart, r.PathValue("kind"),
			r.PathValue("ruleID"), *body.Actions); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}

// handlePutPushRuleEnabled answers PUT /pushrules/{scope}/{kind}/{ruleID}/enabled.
func handlePutPushRuleEnabled(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if _, err := pushRuleScope(r); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}
		if body.Enabled == nil {
			httpx.RespondError(w, r, matrix.NewMissingParam("enabled"))
			return
		}
		if err := deps.PushRules.SetEnabled(r.Context(), session.Localpart, r.PathValue("kind"),
			r.PathValue("ruleID"), *body.Enabled); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}

// handlePutPushRulePattern answers PUT /pushrules/{scope}/{kind}/{ruleID}/pattern.
func handlePutPushRulePattern(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if _, err := pushRuleScope(r); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		var body struct {
			Pattern *string `json:"pattern"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.RespondError(w, r, matrix.NewBadJSON(err.Error()))
			return
		}
		if body.Pattern == nil {
			httpx.RespondError(w, r, matrix.NewMissingParam("pattern"))
			return
		}
		if err := deps.PushRules.SetPattern(r.Context(), session.Localpart, r.PathValue("kind"),
			r.PathValue("ruleID"), *body.Pattern); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}

// handleDeletePushRule answers DELETE /pushrules/{scope}/{kind}/{ruleID}.
func handleDeletePushRule(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := httpx.MustSession(r)
		if _, err := pushRuleScope(r); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		if err := deps.PushRules.Delete(r.Context(), session.Localpart, r.PathValue("kind"),
			r.PathValue("ruleID")); err != nil {
			httpx.RespondError(w, r, err)
			return
		}
		httpx.RespondJSON(w, http.StatusOK, struct{}{})
	}
}
