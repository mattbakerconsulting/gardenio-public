package main

import (
	"encoding/json"
	"testing"
)

func TestCheckBeadCloseSpec(t *testing.T) {
	spec := func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		opType, spec string
		ok           bool
	}{
		{"kbeads_bead_close", `{"bead_ids":["gd-a","gd-b"]}`, true},
		{"bead_close", `{"bead_ids":["gd-a"]}`, true},
		{"kbeads_bead_close", `{}`, false},
		{"bead_close", `{"bead_ids":[]}`, false},
		{"bead_close", `{"bead_ids":[" "]}`, false},
		{"kbeads_bead_close", `{"bead_ids":"gd-a"}`, false},
		{"kbeads_bead_close", `{"bead_ids":[1]}`, false},
		{"KBEADS_BEAD_CLOSE", `{}`, false},
		// Other operation types are not checked.
		{"gitlab_mr_merge", `{}`, true},
		{"aws_iam", `{"bead_ids":"whatever"}`, true},
	} {
		err := checkBeadCloseSpec(tc.opType, spec(tc.spec))
		if (err == nil) != tc.ok {
			t.Errorf("checkBeadCloseSpec(%q, %s) = %v; want ok=%v", tc.opType, tc.spec, err, tc.ok)
		}
	}
}
