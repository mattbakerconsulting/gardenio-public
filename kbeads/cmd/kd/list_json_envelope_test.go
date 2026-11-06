package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/model"
)

// resetListEnvelopeFlags clears the flags these tests mutate so one subtest's
// setting never leaks into the next RunE call within this process. Mirrors
// resetListOperationalFlags but covers the pagination + envelope surface.
func resetListEnvelopeFlags(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Name {
		case "type", "kind", "status", "label":
			f.Changed = false
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(nil)
			}
		case "all-types", "all-projects", "project", "parent", "include-operational",
			"json-envelope", "limit", "offset":
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	})
	listProjectFlag = ""
	listAllProjectsFlag = false
}

// runListEnvelope drives listCmd.RunE with the given envelope/limit/offset and a
// mock returning `page` beads out of `total`, returning captured stdout.
func runListEnvelope(t *testing.T, page []*model.Bead, total, limit, offset int) string {
	t.Helper()
	mc := newMockClient()
	mc.ListBeadsResult = page
	mc.ListBeadsTotalOverride = total
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListEnvelopeFlags(t)
	if err := listCmd.Flags().Set("json-envelope", "true"); err != nil {
		t.Fatalf("setting --json-envelope: %v", err)
	}
	if err := listCmd.Flags().Set("limit", itoaEnv(limit)); err != nil {
		t.Fatalf("setting --limit: %v", err)
	}
	if err := listCmd.Flags().Set("offset", itoaEnv(offset)); err != nil {
		t.Fatalf("setting --offset: %v", err)
	}

	out := captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	return out
}

// itoaEnv is a tiny local int→string so tests need not import strconv just for
// flag values.
func itoaEnv(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestList_JSONEnvelope_TruncatedPageSignalsHasMore is the core acceptance
// check: a --limit-capped page (2 of 40) emits has_more=true and the true
// total, so a scripted caller can distinguish it from a complete set — the
// exact gap the bare array leaves (ka-0000001033).
func TestList_JSONEnvelope_TruncatedPageSignalsHasMore(t *testing.T) {
	page := []*model.Bead{
		{ID: "gd-1", Title: "First"},
		{ID: "gd-2", Title: "Second"},
	}
	out := runListEnvelope(t, page, 40, 2, 0)

	var env struct {
		Beads   []map[string]any `json:"beads"`
		Count   int              `json:"count"`
		Total   int              `json:"total"`
		HasMore bool             `json:"has_more"`
		Offset  int              `json:"offset"`
		Limit   int              `json:"limit"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, out)
	}
	if len(env.Beads) != 2 || env.Count != 2 {
		t.Errorf("beads/count = %d/%d, want 2/2", len(env.Beads), env.Count)
	}
	if env.Total != 40 {
		t.Errorf("total = %d, want 40", env.Total)
	}
	if !env.HasMore {
		t.Errorf("has_more = false, want true (2 of 40 is a capped page)")
	}
	if env.Offset != 0 || env.Limit != 2 {
		t.Errorf("offset/limit = %d/%d, want 0/2", env.Offset, env.Limit)
	}
}

// TestList_JSONEnvelope_CompletePageHasMoreFalse asserts a page that IS the
// whole set (total == count) reports has_more=false, the other half of the
// distinguish-a-page-from-the-whole-set contract.
func TestList_JSONEnvelope_CompletePageHasMoreFalse(t *testing.T) {
	page := []*model.Bead{
		{ID: "gd-1", Title: "First"},
		{ID: "gd-2", Title: "Second"},
	}
	out := runListEnvelope(t, page, 2, 20, 0)

	var env struct {
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
		Count   int  `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, out)
	}
	if env.Total != 2 || env.Count != 2 {
		t.Errorf("total/count = %d/%d, want 2/2", env.Total, env.Count)
	}
	if env.HasMore {
		t.Errorf("has_more = true, want false (page is the whole set)")
	}
}

// TestList_JSONEnvelope_HasMoreAccountsForOffset asserts has_more folds in the
// offset: the last page of a paged walk (offset 38, 2 of 40) is complete even
// though total > count, because offset+count == total.
func TestList_JSONEnvelope_HasMoreAccountsForOffset(t *testing.T) {
	page := []*model.Bead{
		{ID: "gd-39", Title: "39"},
		{ID: "gd-40", Title: "40"},
	}
	out := runListEnvelope(t, page, 40, 20, 38)

	var env struct {
		Offset  int  `json:"offset"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, out)
	}
	if env.Offset != 38 {
		t.Errorf("offset = %d, want 38", env.Offset)
	}
	if env.HasMore {
		t.Errorf("has_more = true, want false (offset 38 + 2 == total 40, no more pages)")
	}
}

// TestList_JSONEnvelope_EmptyResultIsEmptyArrayNotNull asserts an empty match
// still marshals beads as [] (never null), so a jq consumer's `.beads[]` and
// `.beads | length` behave on a zero-row result.
func TestList_JSONEnvelope_EmptyResultIsEmptyArrayNotNull(t *testing.T) {
	out := runListEnvelope(t, []*model.Bead{}, 0, 20, 0)
	if !strings.Contains(out, "\"beads\": []") {
		t.Errorf("empty result must render beads as [], got:\n%s", out)
	}
	var env struct {
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
		Count   int  `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, out)
	}
	if env.Total != 0 || env.Count != 0 || env.HasMore {
		t.Errorf("empty result env = {total:%d count:%d has_more:%v}, want {0 0 false}", env.Total, env.Count, env.HasMore)
	}
}

// TestList_JSONEnvelope_ImpliesJSON asserts --json-envelope alone (no explicit
// --json) still produces the enveloped object — the flag implies --json,
// mirroring how --jq forces JSON mode.
func TestList_JSONEnvelope_ImpliesJSON(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-1", Title: "First"}}
	mc.ListBeadsTotalOverride = 5
	withMockClient(t, mc) // leaves jsonOutput=false
	withStdoutTTY(t, false)

	resetListEnvelopeFlags(t)
	if err := listCmd.Flags().Set("json-envelope", "true"); err != nil {
		t.Fatalf("setting --json-envelope: %v", err)
	}

	out := captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	var env struct {
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("--json-envelope did not imply --json; stdout not a JSON object: %v\n%s", err, out)
	}
	if env.Total != 5 || !env.HasMore {
		t.Errorf("env = {total:%d has_more:%v}, want {5 true}", env.Total, env.HasMore)
	}
}

// TestList_DefaultJSON_StaysBareArray is the regression guard for the rejected
// top-level envelope (ka-0000007254): WITHOUT --json-envelope, `kd list --json`
// must remain a bare array so every `jq '.[]'` consumer keeps working.
func TestList_DefaultJSON_StaysBareArray(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-1", Title: "First"},
		{ID: "gd-2", Title: "Second"},
	}
	mc.ListBeadsTotalOverride = 40 // capped page — the bare array must STILL not grow an envelope
	withMockClient(t, mc)
	withStdoutTTY(t, false)
	jsonOutput = true

	resetListEnvelopeFlags(t)

	out := captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("default --json must be a bare array, got parse error %v:\n%s", err, out)
	}
	if len(arr) != 2 {
		t.Errorf("bare array len = %d, want 2", len(arr))
	}
	if strings.Contains(out, "has_more") || strings.Contains(out, "\"total\"") {
		t.Errorf("default --json leaked envelope keys, got:\n%s", out)
	}
}
