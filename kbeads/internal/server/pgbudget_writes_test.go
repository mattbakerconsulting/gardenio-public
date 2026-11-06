package server

import (
	"encoding/json"
	"os"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestBuiltinPgBudgetWritesValidate validates every bead the controller's
// Postgres budget report writes (internal/pgbudget: the filer's tasks and the
// #machines post) against the builtin schemas. type:task and type:message
// reject unknown fields, and the controller's fake store accepts anything, so
// an undeclared pg_budget_key or pg_report_day 400'd every filed task and every
// report post only on the live daemon (gd-ylgk1jBhLi). The golden is the
// controller's real writes, pinned by its TestRunOnce_WritesMatchKbeadsGolden.
func TestBuiltinPgBudgetWritesValidate(t *testing.T) {
	b, err := os.ReadFile("testdata/pgbudget_writes.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var writes []struct {
		Type   string          `json:"type"`
		Fields json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(b, &writes); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, w := range writes {
		seen[w.Type] = true
		tc := parseBuiltinSchema(t, w.Type)
		tc.TypeName = w.Type
		if err := model.ValidateTypeFields(w.Fields, tc); err != nil {
			t.Errorf("write %d (type:%s) does not validate: %v", i, w.Type, err)
		}
		if w.Type == "message" {
			if err := validateMessageCardFields(w.Fields); err != nil {
				t.Errorf("write %d (type:message) fails the card rules: %v", i, err)
			}
		}
	}
	if !seen["task"] || !seen["message"] {
		t.Errorf("golden covers %v, want a task and a message", seen)
	}
}
