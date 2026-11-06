package beadsapi

import (
	"encoding/json"
	"testing"
)

// TestParseFieldsJSON_NullIsUnset pins the regression from the ka-0000001419
// follow-up: a role bead carrying `"model": null` alongside complex fields
// (secrets array → slow path) used to decode to the literal string "null",
// which resolveModelOverride treated as a real override — every pod under the
// role then requested a model named "null" and 401'd. JSON null must decode
// to "" on BOTH decode paths.
func TestParseFieldsJSON_NullIsUnset(t *testing.T) {
	t.Run("slow path (complex values present)", func(t *testing.T) {
		raw := json.RawMessage(`{
			"model": null,
			"account": "litellm-bedrock",
			"account_whitelist": ["litellm-bedrock"],
			"secrets": [{"env": "X", "secret": "s", "key": "k"}]
		}`)
		m := ParseFieldsJSON(raw)
		if m["model"] != "" {
			t.Errorf("null field decoded to %q, want \"\"", m["model"])
		}
		if m["account"] != "litellm-bedrock" {
			t.Errorf("account = %q, want litellm-bedrock", m["account"])
		}
		if m["account_whitelist"] != `["litellm-bedrock"]` {
			t.Errorf("array field = %q, want JSON re-marshal", m["account_whitelist"])
		}
	})

	t.Run("fast path (strings and null only)", func(t *testing.T) {
		m := ParseFieldsJSON(json.RawMessage(`{"model": null, "account": "gardenio2"}`))
		if m["model"] != "" {
			t.Errorf("null field decoded to %q, want \"\"", m["model"])
		}
		if m["account"] != "gardenio2" {
			t.Errorf("account = %q, want gardenio2", m["account"])
		}
	})
}
