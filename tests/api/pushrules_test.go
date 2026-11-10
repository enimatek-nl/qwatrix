package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/enimatek-nl/qwatrix/tests/harness"
)

// pushRules reads the whole rule set the way a client does — with the trailing slash, which is the
// only spelling the specification offers for "everything".
func pushRules(t *testing.T, account *harness.Account, server *harness.Server) map[string]map[string][]map[string]any {
	t.Helper()
	resp := account.MustGet(t, server, "/_matrix/client/v3/pushrules/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /pushrules/: got %d, want 200\nbody: %s", resp.StatusCode, resp.Body)
	}
	var body struct {
		Global map[string][]map[string]any `json:"global"`
	}
	resp.JSON(t, &body)
	return map[string]map[string][]map[string]any{"global": body.Global}
}

func findRule(rules []map[string]any, ruleID string) map[string]any {
	for _, rule := range rules {
		if id, _ := rule["rule_id"].(string); id == ruleID {
			return rule
		}
	}
	return nil
}

// TestPushRulesShipWorkableDefaults checks that a brand-new account is already configured to notify.
//
// The defaults are the difference between a chat app that buzzes for an invitation and one that
// stays silent until the user finds a settings screen they have never seen. An empty rule set is a
// plausible-looking response and a broken product.
func TestPushRulesShipWorkableDefaults(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "pushy", "pw-pushy", false)

	all := pushRules(t, alice, server)["global"]
	for _, kind := range []string{"override", "content", "room", "sender", "underride"} {
		if _, ok := all[kind]; !ok {
			t.Errorf("GET /pushrules/ has no %q list; a client cannot tell %q from %q",
				kind, "no rules of this kind", "no such key")
		}
	}
	master := findRule(all["override"], ".m.rule.master")
	if master == nil {
		t.Fatal("the default override rules are missing, so nothing is configured to notify")
	}
	if enabled, _ := master["enabled"].(bool); enabled {
		// The master rule is the kill switch, and it ships switched off. Shipped on, it matches
		// everything and stops evaluation, which silences the account it was supposed to configure.
		t.Error("the master rule ships enabled, which suppresses every rule below it")
	}
	if invite := findRule(all["override"], ".m.rule.invite_for_me"); invite == nil {
		t.Error("no invite_for_me rule, so invitations do not notify")
	} else if actions, _ := invite["actions"].([]any); len(actions) == 0 {
		t.Error("invite_for_me has no actions, so it notifies by doing nothing")
	}
	if named := findRule(all["content"], ".m.rule.contains_user_name"); named == nil {
		t.Error("no contains_user_name rule, so being mentioned does not notify")
	} else if pattern, _ := named["pattern"].(string); pattern != "pushy" {
		t.Errorf("contains_user_name matches %q, want the account's own local part", pattern)
	}
}

// TestPushRulesRoundTripAndReachSync checks the two halves of the contract: a rule the client sets
// comes back unchanged, and a client sitting in /sync is told about it.
//
// The second half is what makes push rules usable at all. A rule set that only exists on the server
// is a configuration the client cannot display, and a change that does not wake the sync leaves a
// phone buzzing according to rules its owner deleted an hour ago.
func TestPushRulesRoundTripAndReachSync(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "setter", "pw-setter", false)

	// A client's first sync carries the rules, because a client that receives none has to assume
	// the account is configured to be silent.
	since := ""
	first := alice.MustSync(t, server, nil)
	since = first.NextBatch
	if !json.Valid([]byte(first.NextBatch)) && since == "" {
		t.Fatal("sync returned no token")
	}
	if !containsType(t, first.AccountData.Events, "m.push_rules") {
		t.Errorf("an initial /sync carried no m.push_rules account data: %s", since)
	}

	// A rule is created by setting its actions, which is the only thing a rule cannot exist
	// without.
	if _, err := doPut(t, alice, server,
		"/_matrix/client/v3/pushrules/global/sender/"+harness.PathEscape(alice.UserID),
		map[string]any{"actions": []any{"dont_notify"}}); err != nil {
		t.Fatal(err)
	}

	all := pushRules(t, alice, server)["global"]
	sender := all["sender"]
	if len(sender) != 1 {
		t.Fatalf("global.sender holds %d rules, want the one just set", len(sender))
	}
	actions, _ := sender[0]["actions"].([]any)
	if len(actions) != 1 || actions[0] != "dont_notify" {
		t.Errorf("the rule's actions came back as %v, want [dont_notify]", actions)
	}
	if isDefault, _ := sender[0]["default"].(bool); isDefault {
		t.Error("a rule the client set is reported as a server default")
	}
	if enabled, _ := sender[0]["enabled"].(bool); !enabled {
		t.Error("a newly created rule is disabled, so setting it did nothing")
	}

	// Enabling and disabling has to be visible on the next read.
	if _, err := doPut(t, alice, server, "/_matrix/client/v3/pushrules/global/sender/"+
		harness.PathEscape(alice.UserID)+"/enabled", map[string]any{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	all = pushRules(t, alice, server)["global"]
	if enabled, _ := all["sender"][0]["enabled"].(bool); enabled {
		t.Error("disabling a rule did not stick")
	}

	// And the change wakes a sync that was already waiting.
	changed := alice.SyncUntil(t, server, &since, func(resp harness.SyncResponse) bool {
		return containsType(t, resp.AccountData.Events, "m.push_rules")
	})
	if changed.NextBatch == "" {
		t.Error("a push-rule change did not reach /sync")
	}
}

// TestPushRulesProtectTheRulesTheServerOwns checks what a client may not do.
//
// A default rule decides whether a person's phone buzzes while they are not looking. Letting a client
// rewrite its conditions means one bad settings screen can silence an account in a way its owner will
// never find, and cannot undo. Being able to switch the same rule off is the legitimate operation, and
// stays available.
func TestPushRulesProtectTheRulesTheServerOwns(t *testing.T) {
	server := harness.NewWithRegistration(t)
	alice := server.Register(t, "guardy", "pw-guardy", false)

	resp := alice.Do(t, server, harness.Request{
		Method: http.MethodPut,
		Path:   "/_matrix/client/v3/pushrules/global/override/.m.rule.master",
		Body:   map[string]any{"actions": []any{"notify"}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rewriting a default rule: got %d %s, want 400", resp.StatusCode, resp.Body)
	}

	// Disabling it is fine, and must survive being read again.
	if _, err := doPut(t, alice, server, "/_matrix/client/v3/pushrules/global/override/.m.rule.roomnotif/enabled",
		map[string]any{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	all := pushRules(t, alice, server)["global"]
	rule := findRule(all["override"], ".m.rule.roomnotif")
	if rule == nil {
		t.Fatal("disabling a default rule deleted it")
	}
	if enabled, _ := rule["enabled"].(bool); enabled {
		t.Error("disabling a default rule did not stick")
	}
	if isDefault, _ := rule["default"].(bool); !isDefault {
		t.Error("a default rule stopped being one")
	}

	// Deleting a default rule is remembered: the defaults must not come back underneath the user.
	if _, err := doDelete(t, alice, server, "/_matrix/client/v3/pushrules/global/override/.m.rule.roomnotif"); err != nil {
		t.Fatal(err)
	}
	all = pushRules(t, alice, server)["global"]
	if rule := findRule(all["override"], ".m.rule.roomnotif"); rule != nil {
		t.Errorf("a deleted default rule came back: %v", rule)
	}

	// A scope or kind this server does not have is refused, not answered with an empty set: an
	// empty answer reads as "you have no rules", and a client may then create the ones it expected
	// to find.
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodGet, Path: "/_matrix/client/v3/pushrules/device/sender/foo",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown scope: got %d, want 400", resp.StatusCode)
	}

	// A kind this server has never heard of has no rules, which is a fact about the rule rather
	// than a complaint about the request: clients probe for rules a particular server may not ship,
	// and treat "not found" as the ordinary negative.
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodGet, Path: "/_matrix/client/v3/pushrules/global/postcontent/.example.rule",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("reading a rule under an unknown kind: got %d, want 404\nbody: %s", resp.StatusCode, resp.Body)
	}
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodPut, Path: "/_matrix/client/v3/pushrules/global/postcontent/.example.rule/enabled",
		Body: map[string]any{"enabled": false},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("disabling a rule under an unknown kind: got %d, want 404", resp.StatusCode)
	}
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodPut, Path: "/_matrix/client/v3/pushrules/global/nonsense/foo",
		Body: map[string]any{"actions": []any{"notify"}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown kind: got %d, want 400", resp.StatusCode)
	}
	resp = alice.Do(t, server, harness.Request{
		Method: http.MethodPut, Path: "/_matrix/client/v3/pushrules/global/sender/foo",
		Body: map[string]any{"pattern": "x"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a rule with no actions: got %d, want 400\nbody: %s", resp.StatusCode, resp.Body)
	}
}

// doPut sends an authenticated PUT and reports a non-2xx answer as an error.
func doPut(t *testing.T, account *harness.Account, server *harness.Server, path string, body any) (string, error) {
	t.Helper()
	resp := account.Do(t, server, harness.Request{Method: http.MethodPut, Path: path, Body: body})
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("PUT %s: got %d, want 2xx\nbody: %s", path, resp.StatusCode, resp.Body)
	}
	var out struct {
		EventID string `json:"event_id"`
	}
	resp.JSON(t, &out)
	return out.EventID, nil
}

// doDelete sends an authenticated DELETE.
func doDelete(t *testing.T, account *harness.Account, server *harness.Server, path string) (string, error) {
	t.Helper()
	resp := account.Do(t, server, harness.Request{Method: http.MethodDelete, Path: path})
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("DELETE %s: got %d, want 2xx\nbody: %s", path, resp.StatusCode, resp.Body)
	}
	return "", nil
}

// containsType reports whether a batch of account-data events holds one of a type.
func containsType(t *testing.T, events []json.RawMessage, eventType string) bool {
	t.Helper()
	for _, raw := range events {
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}
		if event.Type == eventType {
			return true
		}
	}
	return false
}
