package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestStripPrimaryAccountFromDesc exercises the pure description-rewrite core:
// primary_account is dropped while every other key (including unknown/future
// observability keys) is preserved verbatim; malformed/absent inputs are no-ops.
func TestStripPrimaryAccountFromDesc(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantChanged bool
		wantPrimary string
		// wantKeys asserts the resulting JSON object's key set (order-agnostic).
		wantKeys []string
		// wantAccount asserts the preserved account value when present.
		wantAccount string
	}{
		{
			name:        "self-pin primary_account dropped, account preserved",
			in:          `{"account":"acct-a","primary_account":"acct-a"}`,
			wantChanged: true,
			wantPrimary: "acct-a",
			wantKeys:    []string{"account"},
			wantAccount: "acct-a",
		},
		{
			name:        "stale primary_account != account dropped, account preserved",
			in:          `{"account":"acct-b","primary_account":"acct-a","failover_reason":"throttle"}`,
			wantChanged: true,
			wantPrimary: "acct-a",
			wantKeys:    []string{"account", "failover_reason"},
			wantAccount: "acct-b",
		},
		{
			name:        "unknown/future keys preserved verbatim",
			in:          `{"account":"acct-a","primary_account":"acct-a","future_hint":"x","nested":{"k":1}}`,
			wantChanged: true,
			wantPrimary: "acct-a",
			wantKeys:    []string{"account", "future_hint", "nested"},
			wantAccount: "acct-a",
		},
		{
			name:        "already stripped is a no-op (idempotent)",
			in:          `{"account":"acct-a","failover_reason":"throttle"}`,
			wantChanged: false,
		},
		{
			name:        "empty description is a no-op",
			in:          ``,
			wantChanged: false,
		},
		{
			name:        "malformed JSON is a no-op",
			in:          `{not json`,
			wantChanged: false,
		},
		{
			name:        "empty primary_account value still dropped",
			in:          `{"account":"acct-a","primary_account":""}`,
			wantChanged: true,
			wantPrimary: "",
			wantKeys:    []string{"account"},
			wantAccount: "acct-a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newDesc, prevPrimary, changed := stripPrimaryAccountFromDesc(tc.in)
			if changed != tc.wantChanged {
				t.Fatalf("changed: got %v, want %v", changed, tc.wantChanged)
			}
			if !tc.wantChanged {
				if newDesc != tc.in {
					t.Errorf("no-op should return input unchanged: got %q, want %q", newDesc, tc.in)
				}
				return
			}
			if prevPrimary != tc.wantPrimary {
				t.Errorf("prevPrimary: got %q, want %q", prevPrimary, tc.wantPrimary)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal([]byte(newDesc), &got); err != nil {
				t.Fatalf("result not valid JSON: %v (%q)", err, newDesc)
			}
			if _, stillThere := got["primary_account"]; stillThere {
				t.Errorf("primary_account not removed: %q", newDesc)
			}
			if len(got) != len(tc.wantKeys) {
				t.Errorf("key count: got %d (%v), want %d (%v)", len(got), keysOf(got), len(tc.wantKeys), tc.wantKeys)
			}
			for _, k := range tc.wantKeys {
				if _, ok := got[k]; !ok {
					t.Errorf("expected key %q preserved, missing from %q", k, newDesc)
				}
			}
			if tc.wantAccount != "" {
				var acct string
				_ = json.Unmarshal(got["account"], &acct)
				if acct != tc.wantAccount {
					t.Errorf("account value: got %q, want %q", acct, tc.wantAccount)
				}
			}
		})
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPlanStripPrimary asserts the plan only includes beads that actually carry
// the legacy data, is sorted by id, and captures the restore payloads.
func TestPlanStripPrimary(t *testing.T) {
	accounts := []*beadsapi.BeadDetail{
		{ID: "a3", Type: "account", Title: "gamma", Fields: map[string]string{"primary": "true"}},
		{ID: "a1", Type: "account", Title: "alpha", Fields: map[string]string{"primary": "true", "status": "active"}},
		{ID: "a2", Type: "account", Title: "beta"},                                            // no primary field → skipped
		{ID: "a4", Type: "account", Title: "delta", Fields: map[string]string{"primary": ""}}, // empty → skipped
		nil, // nil-safe
	}
	configs := []*beadsapi.BeadDetail{
		{ID: "c2", Type: "config", Title: "auth", Description: `{"account":"acct-b","primary_account":"acct-a"}`},
		{ID: "c1", Type: "config", Title: "auth", Description: `{"account":"acct-a","primary_account":"acct-a"}`},
		{ID: "c3", Type: "config", Title: "auth", Description: `{"account":"acct-c"}`},     // already clean → skipped
		{ID: "c4", Type: "config", Title: "other", Description: `{"primary_account":"x"}`}, // not title=auth → skipped
		{ID: "c5", Type: "task", Title: "auth", Description: `{"primary_account":"x"}`},    // not type=config → skipped
	}

	plan := planStripPrimary(accounts, configs)

	// Accounts: only a1, a3 carry a real primary marker; sorted by id.
	if len(plan.Accounts) != 2 {
		t.Fatalf("accounts: got %d, want 2: %+v", len(plan.Accounts), plan.Accounts)
	}
	if plan.Accounts[0].BeadID != "a1" || plan.Accounts[1].BeadID != "a3" {
		t.Errorf("account order: got [%s %s], want [a1 a3]", plan.Accounts[0].BeadID, plan.Accounts[1].BeadID)
	}
	if plan.Accounts[0].PrevValue != "true" {
		t.Errorf("prev value: got %q, want true", plan.Accounts[0].PrevValue)
	}

	// Configs: only c1, c2 carry primary_account; sorted by id.
	if len(plan.Configs) != 2 {
		t.Fatalf("configs: got %d, want 2: %+v", len(plan.Configs), plan.Configs)
	}
	if plan.Configs[0].BeadID != "c1" || plan.Configs[1].BeadID != "c2" {
		t.Errorf("config order: got [%s %s], want [c1 c2]", plan.Configs[0].BeadID, plan.Configs[1].BeadID)
	}
	// Restore payload preserved.
	if plan.Configs[0].PrevDescription != `{"account":"acct-a","primary_account":"acct-a"}` {
		t.Errorf("prev description not captured: %q", plan.Configs[0].PrevDescription)
	}
	if !strings.Contains(plan.Configs[0].NewDescription, `"account":"acct-a"`) ||
		strings.Contains(plan.Configs[0].NewDescription, "primary_account") {
		t.Errorf("new description wrong: %q", plan.Configs[0].NewDescription)
	}
}

// fakeStripClient is an in-memory stripPrimaryClient so we can assert Execute
// applies exactly the planned mutations, honors dry-run, and is idempotent.
type fakeStripClient struct {
	accounts    []*beadsapi.BeadDetail
	configs     []*beadsapi.BeadDetail
	unsetCalls  []struct{ id, field string }
	descCalls   []struct{ id, desc string }
	failUnsetID string
	failDescID  string
}

func (f *fakeStripClient) ListAccountBeadsRaw(context.Context) ([]*beadsapi.BeadDetail, error) {
	return f.accounts, nil
}

func (f *fakeStripClient) ListAuthConfigBeadsRaw(context.Context) ([]*beadsapi.BeadDetail, error) {
	return f.configs, nil
}

func (f *fakeStripClient) UnsetField(_ context.Context, beadID, fieldKey string) error {
	if beadID == f.failUnsetID {
		return fmt.Errorf("synthetic UnsetField failure")
	}
	f.unsetCalls = append(f.unsetCalls, struct{ id, field string }{beadID, fieldKey})
	for _, a := range f.accounts {
		if a.ID == beadID {
			delete(a.Fields, fieldKey)
		}
	}
	return nil
}

func (f *fakeStripClient) UpdateBeadDescription(_ context.Context, beadID, description string) error {
	if beadID == f.failDescID {
		return fmt.Errorf("synthetic UpdateBeadDescription failure")
	}
	f.descCalls = append(f.descCalls, struct{ id, desc string }{beadID, description})
	for _, c := range f.configs {
		if c.ID == beadID {
			c.Description = description
		}
	}
	return nil
}

func newFakeStripClient() *fakeStripClient {
	return &fakeStripClient{
		accounts: []*beadsapi.BeadDetail{
			{ID: "a1", Type: "account", Title: "alpha", Fields: map[string]string{"primary": "true"}},
			{ID: "a2", Type: "account", Title: "beta"},
		},
		configs: []*beadsapi.BeadDetail{
			{ID: "c1", Type: "config", Title: "auth", Description: `{"account":"acct-a","primary_account":"acct-a"}`},
			{ID: "c2", Type: "config", Title: "auth", Description: `{"account":"acct-b"}`},
		},
	}
}

func TestExecuteStripPrimary_AppliesThenIdempotent(t *testing.T) {
	f := newFakeStripClient()
	var out bytes.Buffer
	if err := executeStripPrimary(context.Background(), f, &out, stripPrimaryOpts{noSnapshot: true}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	if len(f.unsetCalls) != 1 || f.unsetCalls[0].id != "a1" || f.unsetCalls[0].field != "primary" {
		t.Errorf("unset calls: got %+v, want [{a1 primary}]", f.unsetCalls)
	}
	if len(f.descCalls) != 1 || f.descCalls[0].id != "c1" {
		t.Errorf("desc calls: got %+v, want one for c1", f.descCalls)
	}
	if strings.Contains(f.descCalls[0].desc, "primary_account") {
		t.Errorf("rewritten desc still has primary_account: %q", f.descCalls[0].desc)
	}
	if !strings.Contains(out.String(), "accounts_stripped=1 configs_stripped=1 errors=0") {
		t.Errorf("summary line missing/incorrect: %q", out.String())
	}

	// Idempotent re-run: state is already clean → 0 mutations.
	f.unsetCalls = nil
	f.descCalls = nil
	out.Reset()
	if err := executeStripPrimary(context.Background(), f, &out, stripPrimaryOpts{noSnapshot: true}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(f.unsetCalls) != 0 || len(f.descCalls) != 0 {
		t.Errorf("idempotent re-run mutated: unset=%+v desc=%+v", f.unsetCalls, f.descCalls)
	}
	if !strings.Contains(out.String(), "accounts_stripped=0 configs_stripped=0 errors=0") {
		t.Errorf("re-run summary should be all-zero: %q", out.String())
	}
}

func TestExecuteStripPrimary_DryRunMakesNoChanges(t *testing.T) {
	f := newFakeStripClient()
	var out bytes.Buffer
	if err := executeStripPrimary(context.Background(), f, &out, stripPrimaryOpts{dryRun: true, noSnapshot: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(f.unsetCalls) != 0 || len(f.descCalls) != 0 {
		t.Errorf("dry run mutated: unset=%+v desc=%+v", f.unsetCalls, f.descCalls)
	}
	s := out.String()
	if !strings.Contains(s, "DRY RUN") {
		t.Errorf("missing DRY RUN banner: %q", s)
	}
	if !strings.Contains(s, "remove primary=true") {
		t.Errorf("missing planned account strip: %q", s)
	}
	if !strings.Contains(s, "drop primary_account") {
		t.Errorf("missing planned config strip: %q", s)
	}
}

func TestExecuteStripPrimary_SnapshotEmittedBeforeMutation(t *testing.T) {
	f := newFakeStripClient()
	var out bytes.Buffer
	if err := executeStripPrimary(context.Background(), f, &out, stripPrimaryOpts{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "SNAPSHOT") {
		t.Errorf("snapshot header missing: %q", s)
	}
	// Snapshot must contain the restore payloads for both the account and config.
	if !strings.Contains(s, `"bead_id": "a1"`) || !strings.Contains(s, `"prev_primary": "true"`) {
		t.Errorf("account snapshot payload missing: %q", s)
	}
	if !strings.Contains(s, `"prev_description": "{\"account\":\"acct-a\",\"primary_account\":\"acct-a\"}"`) {
		t.Errorf("config snapshot restore payload missing: %q", s)
	}
	// Documented restore steps present.
	if !strings.Contains(s, "kd update <bead_id> -f primary=") {
		t.Errorf("restore instructions missing: %q", s)
	}
}

func TestExecuteStripPrimary_SnapshotToFile(t *testing.T) {
	f := newFakeStripClient()
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	var out bytes.Buffer
	if err := executeStripPrimary(context.Background(), f, &out, stripPrimaryOpts{dryRun: true, snapshotOut: path}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "Snapshot written to "+path) {
		t.Errorf("stdout should note snapshot file: %q", out.String())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading snapshot file: %v", err)
	}
	if !strings.Contains(string(body), `"bead_id": "a1"`) {
		t.Errorf("snapshot file missing account payload: %q", string(body))
	}
	if !strings.Contains(string(body), "Restore an account marker") {
		t.Errorf("snapshot file missing restore header: %q", string(body))
	}
}

func TestExecuteStripPrimary_ContinuesAndErrorsOnFailure(t *testing.T) {
	f := newFakeStripClient()
	// Add a second strippable account + config so we can prove the loop
	// continues past a failure and still reports the error.
	f.accounts = append(f.accounts, &beadsapi.BeadDetail{
		ID: "a3", Type: "account", Title: "gamma", Fields: map[string]string{"primary": "true"},
	})
	f.failUnsetID = "a1"

	var out bytes.Buffer
	err := executeStripPrimary(context.Background(), f, &out, stripPrimaryOpts{noSnapshot: true})
	if err == nil {
		t.Fatal("expected error from failed unset, got nil")
	}
	// a1 failed but a3 must still have been attempted.
	foundA3 := false
	for _, c := range f.unsetCalls {
		if c.id == "a3" {
			foundA3 = true
		}
	}
	if !foundA3 {
		t.Errorf("loop should continue past a1 failure to a3: %+v", f.unsetCalls)
	}
	if !strings.Contains(out.String(), "errors=1") {
		t.Errorf("summary should report errors=1: %q", out.String())
	}
}
