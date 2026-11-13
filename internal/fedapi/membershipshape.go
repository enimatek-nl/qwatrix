package fedapi

// The shape a membership endpoint requires, checked before anything is trusted about the event.
//
// `send_join` and `send_leave` are the two doors at which a peer writes a membership into a room this
// server hosts, and each door takes exactly one membership: a join at the join door, a leave at the leave
// door. This file says what makes an event unqualified for a door, in terms that need no trust in the
// event — its type, its state key, who that state key names, and what its content says the membership is.
//
// The status code is the substance. A defect in the event itself is answered 400 M_INVALID_PARAM, which
// tells a peer "rebuild it and send it again"; a question of standing is answered 403 M_FORBIDDEN, which
// tells a peer that no version of this event will do. Conflating them is not a cosmetic error: a server
// that receives 403 for a malformed event concludes the other server has a policy against it, and keeps
// asking — while a server that receives 400 for an unauthorised user learns nothing it can act on.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/tidwall/gjson"
)

// membershipShapeProblem returns why this event body could never be accepted at `endpoint`, which is
// named the way the specification names it so the refusal points at the door rather than at the caller's
// internals, or "" when the body is shaped for that door.
func membershipShapeProblem(content json.RawMessage, endpoint, want string) string {
	if len(content) == 0 {
		return endpoint + " requires the membership event in the request body"
	}
	eventType := gjson.GetBytes(content, "type").String()
	if eventType != string(matrix.EventTypeMember) {
		return fmt.Sprintf("%s takes a membership event, not %q", endpoint, eventType)
	}
	stateKey := gjson.GetBytes(content, "state_key")
	if !stateKey.Exists() || stateKey.Type != gjson.String || stateKey.String() == "" {
		return fmt.Sprintf("%s takes a membership about a user, and this event names none", endpoint)
	}
	// Two user IDs are comparable. The oldest room versions carry an opaque sender ID instead, where
	// this comparison would be nonsense — and nonsense is what a 400 would claim.
	if sender := gjson.GetBytes(content, "sender").String(); strings.HasPrefix(sender, "@") &&
		strings.HasPrefix(stateKey.String(), "@") && sender != stateKey.String() {
		return fmt.Sprintf("the membership is about %q but is sent from %q, and a membership event may only "+
			"speak for the user it is from", stateKey.String(), sender)
	}
	membership := gjson.GetBytes(content, "content.membership").String()
	if membership != want {
		return fmt.Sprintf("%s takes a %s, not a membership of %q", endpoint, want, membership)
	}
	return ""
}
