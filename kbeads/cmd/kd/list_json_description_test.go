package main

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestListJSONAlwaysCarriesDescription pins the `kd list --json` contract that
// every bead object carries a `description` key, even when the description is
// empty (gd-X2zes76pYA).
//
// Before the fix, model.Bead.Description used `json:",omitempty"`, so the key
// was ABSENT for empty-description beads. That silently broke the documented
// content-grep idiom `kd list --json | jq '.[] | select(.description |
// test("x"))'`: the moment jq touched an empty-description bead it hard-errored
// with `null (null) cannot be matched, as it is not a string` (exit 5), turning
// a content search into a hard failure rather than a clean per-bead match. With
// the key always present as "" the same jq filter runs to completion.
func TestListJSONAlwaysCarriesDescription(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-1", Title: "empty-description bead"},
		{ID: "gd-2", Title: "has-description bead", Description: "the term json lives only here"},
	}

	out := captureStdout(t, func() {
		printBeadListJSON(beads)
	})

	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("list JSON is not a valid array: %v\n%s", err, out)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 beads, got %d\n%s", len(arr), out)
	}
	for i, b := range arr {
		v, ok := b["description"]
		if !ok {
			t.Errorf("bead[%d] (%v) is missing the description key — a jq content-grep would hard-error on it", i, b["id"])
			continue
		}
		if _, isStr := v.(string); !isStr {
			t.Errorf("bead[%d] (%v) description is %T, want string (a non-string value breaks jq's test())", i, b["id"], v)
		}
	}

	// The content-grep the acceptance criterion cares about: a term that lives
	// only in a description must be findable, and the empty-description sibling
	// must not derail the scan.
	if arr[0]["description"] != "" {
		t.Errorf("empty-description bead should carry \"\", got %q", arr[0]["description"])
	}
	if arr[1]["description"] != "the term json lives only here" {
		t.Errorf("description body not round-tripped, got %q", arr[1]["description"])
	}
}
