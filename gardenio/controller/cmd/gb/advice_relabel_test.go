package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// relabelledLabels is the pure core of `gb advice relabel` (bug ka-0000003820).
// These tests lock the SEMANTICS of the label move — what the resulting set is,
// and what the reported diff says about it — without a daemon.

// TestRelabelledLabels_WorkOrderCase is the headline regression: the exact
// invocation the work order specifies must leave EXACTLY scope:on-demand, with
// no residue of either label being moved away from. This is the state a
// non-atomic `kd label add` + `kd label remove` pair leaves half-done when the
// second call is dropped.
func TestRelabelledLabels_WorkOrderCase(t *testing.T) {
	current := []string{"scope:project-broadcast", "project:gardenio"}
	next, res := relabelledLabels(current,
		[]string{"scope:on-demand"},
		[]string{"scope:project-broadcast", "project:gardenio"}, "")

	if got := strings.Join(next, ","); got != "scope:on-demand" {
		t.Fatalf("labels = %q; want exactly \"scope:on-demand\" (no residue)", got)
	}
	if len(res.added) != 1 || res.added[0] != "scope:on-demand" {
		t.Errorf("added = %v; want [scope:on-demand]", res.added)
	}
	if len(res.removed) != 2 {
		t.Errorf("removed = %v; want both old labels reported", res.removed)
	}
	if len(res.absent) != 0 {
		t.Errorf("absent = %v; want none (both removals matched)", res.absent)
	}
	if !res.changed() {
		t.Error("changed() = false; want true")
	}
}

// TestRelabelledLabels_ScopeSwap covers the --scope sugar: adding scope:X must
// sweep any OTHER scope: label without the caller enumerating the old value,
// while leaving non-scope labels untouched. That sweep is the whole reason
// --scope exists — the caller cannot forget the removal half.
func TestRelabelledLabels_ScopeSwap(t *testing.T) {
	current := []string{"project:gardenio", "scope:project-broadcast", "role:crew"}
	next, res := relabelledLabels(current, []string{"scope:on-demand"}, nil, "on-demand")

	want := "project:gardenio,role:crew,scope:on-demand"
	if got := strings.Join(next, ","); got != want {
		t.Fatalf("labels = %q; want %q", got, want)
	}
	if len(res.removed) != 1 || res.removed[0] != "scope:project-broadcast" {
		t.Errorf("removed = %v; want only the old scope: label", res.removed)
	}
	// The implicit sweep must NOT be reported as an absent removal — the caller
	// never named it, so warning about it would be noise.
	if len(res.absent) != 0 {
		t.Errorf("absent = %v; want none for an implicit scope sweep", res.absent)
	}
}

// TestRelabelledLabels_ScopeSwapStripsGroupPrefix locks the sweep comparing on
// the STRIPPED label form, so a grouped g0:scope:x is swept too. The delivery
// predicates read scope through advice.StripGroupPrefix, so a sweep that missed
// the grouped form would leave a bead double-scoped as far as delivery is
// concerned while looking clean in the label list.
func TestRelabelledLabels_ScopeSwapStripsGroupPrefix(t *testing.T) {
	current := []string{"g0:scope:project-broadcast", "g0:project:gardenio"}
	next, _ := relabelledLabels(current, []string{"scope:on-demand"}, nil, "on-demand")

	for _, l := range next {
		if strings.Contains(l, "scope:project-broadcast") {
			t.Fatalf("labels = %v; grouped scope label survived the swap", next)
		}
	}
	if !slices.Contains(next, "scope:on-demand") {
		t.Errorf("labels = %v; want the new scope label present", next)
	}
	if !slices.Contains(next, "g0:project:gardenio") {
		t.Errorf("labels = %v; want the unrelated grouped label untouched", next)
	}
}

// TestRelabelledLabels_AddWinsOverRemove pins the documented precedence: a label
// named in BOTH the additions and --remove survives. Without a deterministic
// rule this would depend on map iteration order, making the command
// non-deterministic on contradictory input.
func TestRelabelledLabels_AddWinsOverRemove(t *testing.T) {
	next, res := relabelledLabels([]string{"role:crew"},
		[]string{"role:crew"}, []string{"role:crew"}, "")

	if !slices.Contains(next, "role:crew") {
		t.Fatalf("labels = %v; want role:crew to survive (explicit add wins)", next)
	}
	if res.changed() {
		t.Errorf("changed() = true (added=%v removed=%v); want a no-op", res.added, res.removed)
	}
	if len(res.absent) != 0 {
		t.Errorf("absent = %v; want none — the label IS present", res.absent)
	}
}

// TestRelabelledLabels_NoOpIsReported ensures "already in the target state" is
// distinguishable from "moved". A verb whose success line is identical either way
// is unfalsifiable — the same defect class as the old bare "Removed label(s)"
// (ka-0000000424).
func TestRelabelledLabels_NoOpIsReported(t *testing.T) {
	current := []string{"scope:on-demand"}
	next, res := relabelledLabels(current, []string{"scope:on-demand"}, nil, "on-demand")

	if res.changed() {
		t.Errorf("changed() = true; want false for an already-correct bead")
	}
	if got := strings.Join(next, ","); got != "scope:on-demand" {
		t.Errorf("labels = %q; want the set preserved unchanged", got)
	}
}

// TestRelabelledLabels_AbsentRemovalReported covers the typo path: a --remove of
// a label the bead never carried must be surfaced, so a caller cannot believe it
// dropped a label it misspelled. The add half still lands (the end state is
// correct), which is why this is a warning rather than a hard error.
func TestRelabelledLabels_AbsentRemovalReported(t *testing.T) {
	next, res := relabelledLabels([]string{"role:crew"},
		[]string{"scope:on-demand"}, []string{"not-a-real-label"}, "")

	if len(res.absent) != 1 || res.absent[0] != "not-a-real-label" {
		t.Fatalf("absent = %v; want the misspelled label named", res.absent)
	}
	if !slices.Contains(next, "scope:on-demand") {
		t.Errorf("labels = %v; want the add half to still land", next)
	}
	if !slices.Contains(next, "role:crew") {
		t.Errorf("labels = %v; want the untouched label to survive", next)
	}
}

// TestRelabelledLabels_DuplicateAddsCollapse guards against a doubled arg
// producing a duplicate label row or a double-counted `added` report.
func TestRelabelledLabels_DuplicateAddsCollapse(t *testing.T) {
	next, res := relabelledLabels(nil,
		[]string{"scope:on-demand", "scope:on-demand"}, nil, "")

	if len(next) != 1 {
		t.Fatalf("labels = %v; want a single deduplicated label", next)
	}
	if len(res.added) != 1 {
		t.Errorf("added = %v; want the label reported once", res.added)
	}
}

// TestRelabelledLabels_ReaddingCurrentScopeIsNoDuplicate pins the interaction
// between the --scope sweep and an addition that is ALREADY on the bead: the
// sweep must skip it (it is in addSet) and the add loop must not re-append it.
// Getting this wrong yields a duplicate label row plus a phantom "added" line
// claiming a change that did not happen.
func TestRelabelledLabels_ReaddingCurrentScopeIsNoDuplicate(t *testing.T) {
	current := []string{"project:gardenio", "scope:on-demand"}
	next, res := relabelledLabels(current, []string{"scope:on-demand"}, nil, "on-demand")

	if got := strings.Join(next, ","); got != "project:gardenio,scope:on-demand" {
		t.Fatalf("labels = %q; want the set unchanged with no duplicate", got)
	}
	if len(res.added) != 0 || len(res.removed) != 0 {
		t.Errorf("added=%v removed=%v; want both empty — nothing actually changed", res.added, res.removed)
	}
}

// TestRelabelledLabels_FlagMisparseIsSurfaced covers the `--remove a b` trap.
// Cobra binds ONE value per --remove occurrence, so the second label becomes a
// POSITIONAL add and is silently kept — a caller who meant "remove both" would
// otherwise read "Relabelled" and believe it went away. The already-present
// positional must be recorded so the command can warn.
func TestRelabelledLabels_FlagMisparseIsSurfaced(t *testing.T) {
	current := []string{"project:gardenio", "role:crew"}
	// What `--scope on-demand --remove role:crew project:gardenio` actually parses
	// to: project:gardenio lands in `add`, NOT in `remove`.
	next, res := relabelledLabels(current,
		[]string{"project:gardenio", "scope:on-demand"},
		[]string{"role:crew"}, "on-demand")

	if !slices.Contains(next, "project:gardenio") {
		t.Fatalf("labels = %v; the mis-parsed label is an ADD, so it must survive", next)
	}
	if len(res.noopAdds) != 1 || res.noopAdds[0] != "project:gardenio" {
		t.Fatalf("noopAdds = %v; want the already-present positional recorded for the warning", res.noopAdds)
	}
	// The --scope value must NOT be reported: re-asserting a scope is legitimate.
	if got := positionalNoopAdds(res.noopAdds, "on-demand"); len(got) != 1 || got[0] != "project:gardenio" {
		t.Errorf("positionalNoopAdds = %v; want only the mis-parsed positional", got)
	}
}

// TestPositionalNoopAdds_ExcludesScopeValue ensures an idempotent
// `--scope X` on a bead already carrying scope:X does not produce a spurious
// "nothing added" warning — that call is a legitimate no-op, not a mis-parse.
func TestPositionalNoopAdds_ExcludesScopeValue(t *testing.T) {
	if got := positionalNoopAdds([]string{"scope:on-demand"}, "on-demand"); len(got) != 0 {
		t.Errorf("positionalNoopAdds = %v; want empty for a re-asserted scope", got)
	}
	// A redundant "scope:" prefix on the flag value must be tolerated too.
	if got := positionalNoopAdds([]string{"scope:on-demand"}, "scope:on-demand"); len(got) != 0 {
		t.Errorf("positionalNoopAdds = %v; want empty when --scope carried the prefix", got)
	}
	// With no --scope at all, every already-present positional is reportable.
	if got := positionalNoopAdds([]string{"role:crew"}, ""); len(got) != 1 {
		t.Errorf("positionalNoopAdds = %v; want the positional reported", got)
	}
}

// TestRelabelledLabels_PreservesOrder documents that surviving labels keep their
// original relative order with additions appended, so the printed before/after
// diff reads as a move rather than a reshuffle.
func TestRelabelledLabels_PreservesOrder(t *testing.T) {
	current := []string{"a", "b", "c"}
	next, _ := relabelledLabels(current, []string{"z"}, []string{"b"}, "")

	if got := strings.Join(next, ","); got != "a,c,z" {
		t.Errorf("labels = %q; want \"a,c,z\" (order preserved, addition appended)", got)
	}
}

// ── beadsapi.SetLabels wire contract ─────────────────────────────────────

// TestSetLabels_SendsSinglePatch is the ATOMICITY proof at the client boundary:
// the whole move must leave as ONE PATCH carrying the complete desired label
// set. If this ever regresses into an add-call plus a remove-call, the
// double-scoped intermediate state ka-0000003820 is about becomes reachable
// again.
func TestSetLabels_SendsSinglePatch(t *testing.T) {
	var calls int
	var gotMethod, gotPath string
	var gotBody map[string][]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotMethod, gotPath = r.Method, r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	if err := c.SetLabels(t.Context(), "gd-bead1", []string{"scope:on-demand"}); err != nil {
		t.Fatalf("SetLabels: %v", err)
	}

	if calls != 1 {
		t.Errorf("made %d HTTP calls; want exactly 1 (the move must be atomic)", calls)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s; want PATCH", gotMethod)
	}
	if gotPath != "/v1/beads/gd-bead1" {
		t.Errorf("path = %s; want /v1/beads/gd-bead1", gotPath)
	}
	if len(gotBody["labels"]) != 1 || gotBody["labels"][0] != "scope:on-demand" {
		t.Errorf("body labels = %v; want the complete desired set", gotBody["labels"])
	}
}

// TestSetLabels_RefusesEmptySet locks the client-side guard. The daemon infers
// set-vs-noop from a NON-EMPTY array, so sending [] is ambiguous between "wipe
// every label" and "change nothing" — refuse locally instead of letting the
// caller guess which one happened.
func TestSetLabels_RefusesEmptySet(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	if err := c.SetLabels(t.Context(), "gd-bead1", nil); err == nil {
		t.Fatal("SetLabels(nil) = nil; want an error rather than an ambiguous wire call")
	}
	if calls != 0 {
		t.Errorf("made %d HTTP calls; want 0 — the guard must not reach the wire", calls)
	}
}

// TestSetLabels_ServerErrorNamesTarget checks a failed move reports the bead and
// the intended set, so a caller reading only stderr can tell WHICH relabel failed
// and what it was trying to become.
func TestSetLabels_ServerErrorNamesTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid advice labels"}`))
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	err = c.SetLabels(t.Context(), "gd-bead1", []string{"scope:on-demand"})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if !strings.Contains(err.Error(), "gd-bead1") || !strings.Contains(err.Error(), "scope:on-demand") {
		t.Errorf("error = %q; want it to name the bead and the intended label set", err.Error())
	}
}

// TestRelabelRemoveFlagHelpTeachesOnePerFlag pins the --remove usage string so
// the one-label-per-flag guidance — the fix for the `--remove a b` mis-parse
// (ka-0000006934) — cannot silently regress out of the help text.
func TestRelabelRemoveFlagHelpTeachesOnePerFlag(t *testing.T) {
	f := adviceRelabelCmd.Flags().Lookup("remove")
	if f == nil {
		t.Fatal("--remove flag not registered on adviceRelabelCmd")
	}
	if !strings.Contains(f.Usage, "ONE per flag") {
		t.Errorf("--remove usage does not teach one-per-flag: %q", f.Usage)
	}
}
