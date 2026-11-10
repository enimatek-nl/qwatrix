package unit_test

import (
	"strings"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
)

// TestValidateEventContent covers the number rule of the event content contract: an integer an
// IEEE-754 double cannot hold exactly is refused, because a signature commits to bytes and
// another server would not be able to reproduce them.
func TestValidateEventContent(t *testing.T) {
	t.Parallel()

	accepted := []string{
		`{}`,
		`{"body":"hello"}`,
		`{"body":"a\nb","flag":true,"nothing":null}`,
		`{"n":0,"neg":-1,"big":9007199254740991,"small":-9007199254740991}`,
		`{"nested":{"deeper":[{"even":[{"deep":1}]}]}}`,
		`{"array":[1,2,3,{"in":["array"]}],"and":"strings"}`,
		`{"key with spaces":1,"unicode":"✅","empty-arr":[]}`,
	}
	for _, content := range accepted {
		if err := matrix.ValidateEventContent([]byte(content)); err != nil {
			t.Errorf("valid content refused: %s: %v", content, err)
		}
	}

	rejected := map[string]string{
		"a number a client cannot represent": `{"body": 9007199254740992}`,
		"its negative twin":                  `{"body": -9007199254740992}`,
		"a fraction":                         `{"body": 1.1}`,
		"a fraction in disguise":             `{"progress": 0.5000000000000001}`,
		"an exponent far out":                `{"huge": 1e400}`,
		"a number nested deep":               `{"a":{"b":[{"c": 1.5}]}}`,
		"an array":                           `[1,2,3]`,
		"a bare number":                      `42`,
		"a bare string":                      `"content"`,
		"nothing":                            ``,
		"truncated":                          `{"body":`,
		"a trailing comma":                   `{"a":1,}`,
	}
	for what, content := range rejected {
		err := matrix.ValidateEventContent([]byte(content))
		if err == nil {
			t.Errorf("%s was accepted: %s", what, content)
			continue
		}
		apiErr, ok := err.(*matrix.APIError)
		if !ok {
			t.Errorf("%s produced %T, want *matrix.APIError", what, err)
			continue
		}
		if apiErr.Code != matrix.ErrBadJSON {
			t.Errorf("%s answered %q, want M_BAD_JSON", what, apiErr.Code)
		}
		if strings.TrimSpace(apiErr.Message) == "" {
			t.Errorf("%s answered without explaining itself", what)
		}
	}
}
