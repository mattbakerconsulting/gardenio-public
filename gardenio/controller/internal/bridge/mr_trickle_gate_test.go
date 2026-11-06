package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// fullyPassingInput returns a TrickleEligibilityInput in which every HARD item
// passes and no perf-sensitive path is touched (so perf is N/A). Individual
// tests mutate one field to exercise a single blocking condition.
func fullyPassingInput() TrickleEligibilityInput {
	return TrickleEligibilityInput{
		Provenance: TrickleProvenanceInput{
			Evaluated:          true,
			MRURLSet:           true,
			LinkedIssueBeadSet: true,
			OperatorTrailerSet: true,
			DarkMRAncestrySet:  true,
		},
		SmallDiff: TrickleSmallDiffInput{
			Evaluated:            true,
			NetLOC:               120,
			FilesChanged:         4,
			SingleConcern:        true,
			StandaloneBuildGreen: boolPtr(true),
		},
		Dossier: TrickleDossierInput{
			Evaluated: true,
			Summary:   "Add the trickle gate.",
			Why:       "The bar had no enforcing decision function; this adds it.",
		},
		Coverage: TrickleCoverageInput{
			Evaluated:     true,
			ResultPresent: true,
			Pass:          true,
			Bypass:        false,
			E2EGreen:      boolPtr(true),
		},
		CodeReview: TrickleCodeReviewInput{
			Evaluated:          true,
			Ran:                true,
			ArtifactAttached:   true,
			UnresolvedFindings: 0,
		},
		Security: SecurityGateInput{
			StatusChecks: map[string]string{
				"CI / Gitleaks":    "success",
				"CI / Go-Vuln":     "success",
				"CI / Cargo-Audit": "success",
				"CI / Cargo-Deny":  "success",
				"CI / OSV-Scan":    "success",
			},
			ScanBlock: SecurityScanBlock{
				Present: true,
				Entries: map[string]SecurityScanEntry{
					"gitleaks": {Scanner: "gitleaks", Version: "8.21.2", Result: "clean"},
				},
			},
			LockPins: map[string]string{"gitleaks": "8.21.2"},
			Tier:     SecurityReviewTier{Effort: "low"},
		},
		SecurityEvaluated: true,
		Perf: TricklePerfInput{
			Evaluated:                 true,
			PerfSensitivePathsTouched: false,
		},
	}
}

// itemByID finds a checklist row by its stable slug.
func itemByID(d TrickleEligibilityDecision, id string) (GateItem, bool) {
	for _, it := range d.Checklist {
		if it.ID == id {
			return it, true
		}
	}
	return GateItem{}, false
}

func TestEvaluateTrickleEligibility_AllPass(t *testing.T) {
	d := EvaluateTrickleEligibility(fullyPassingInput())
	if !d.Eligible {
		t.Fatalf("expected eligible, got blocked: %s", d.Reason)
	}
	if d.HardTotal() != 7 {
		t.Errorf("HardTotal = %d, want 7", d.HardTotal())
	}
	if d.HardPass() != 7 {
		t.Errorf("HardPass = %d, want 7 (perf N/A counts as passing)", d.HardPass())
	}
	// perf must be N/A (glob-triggered, no perf path touched), not a fail.
	perf, ok := itemByID(d, TrickleItemPerf)
	if !ok || !perf.NA {
		t.Errorf("perf item = %+v, want NA=true", perf)
	}
	// Exactly 3 advisory rows, none blocking.
	adv := 0
	for _, it := range d.Checklist {
		if it.Kind == TrickleAdvisory {
			adv++
		}
	}
	if adv != 3 {
		t.Errorf("advisory count = %d, want 3", adv)
	}
}

func TestEvaluateTrickleEligibility_ChecklistOrderAndIDs(t *testing.T) {
	d := EvaluateTrickleEligibility(fullyPassingInput())
	wantHard := []string{
		TrickleItemProvenance, TrickleItemSmallDiff, TrickleItemRationale,
		TrickleItemCoverage, TrickleItemCodeReview, TrickleItemSecurity, TrickleItemPerf,
	}
	wantAdvisory := []string{
		TrickleItemBenchmark, TrickleItemRationaleDepth, TrickleItemTestQuality,
	}
	var gotHard, gotAdvisory []string
	for _, it := range d.Checklist {
		if it.Kind == TrickleHard {
			gotHard = append(gotHard, it.ID)
		} else {
			gotAdvisory = append(gotAdvisory, it.ID)
		}
	}
	if strings.Join(gotHard, ",") != strings.Join(wantHard, ",") {
		t.Errorf("HARD ids/order = %v, want %v", gotHard, wantHard)
	}
	if strings.Join(gotAdvisory, ",") != strings.Join(wantAdvisory, ",") {
		t.Errorf("ADVISORY ids/order = %v, want %v", gotAdvisory, wantAdvisory)
	}
	// HARD rows must precede ADVISORY rows in the flat checklist.
	firstAdvisory := -1
	for i, it := range d.Checklist {
		if it.Kind == TrickleAdvisory && firstAdvisory == -1 {
			firstAdvisory = i
		}
		if it.Kind == TrickleHard && firstAdvisory != -1 {
			t.Fatalf("HARD row %q appears after an ADVISORY row (index %d > %d)", it.ID, i, firstAdvisory)
		}
	}
}

func TestEvaluateTrickleEligibility_PendingBlocks(t *testing.T) {
	// A HARD item with no evidence (not evaluated) must block — "could not prove"
	// is not "passed".
	in := fullyPassingInput()
	in.CodeReview.Evaluated = false
	d := EvaluateTrickleEligibility(in)
	if d.Eligible {
		t.Fatalf("expected blocked on pending code-review, got eligible")
	}
	cr, _ := itemByID(d, TrickleItemCodeReview)
	if cr.Pass != nil {
		t.Errorf("pending code-review Pass = %v, want nil", cr.Pass)
	}
	if !strings.Contains(d.Reason, TrickleItemCodeReview) {
		t.Errorf("reason %q should name the blocking item", d.Reason)
	}
}

func TestEvaluateTrickleEligibility_ProvenanceMissing(t *testing.T) {
	in := fullyPassingInput()
	in.Provenance.DarkMRAncestrySet = false
	d := EvaluateTrickleEligibility(in)
	if d.Eligible {
		t.Fatal("expected blocked on incomplete provenance")
	}
	it, _ := itemByID(d, TrickleItemProvenance)
	if it.Pass == nil || *it.Pass {
		t.Errorf("provenance Pass = %v, want &false", it.Pass)
	}
	if !strings.Contains(it.Evidence, "dark-MR ancestry") {
		t.Errorf("evidence %q should name the missing piece", it.Evidence)
	}
}

func TestEvaluateTrickleEligibility_SmallDiff(t *testing.T) {
	t.Run("size breach without exception blocks", func(t *testing.T) {
		in := fullyPassingInput()
		in.SmallDiff.NetLOC = 900
		d := EvaluateTrickleEligibility(in)
		if d.Eligible {
			t.Fatal("expected blocked on oversized diff")
		}
		it, _ := itemByID(d, TrickleItemSmallDiff)
		if it.Pass == nil || *it.Pass {
			t.Errorf("small-diff Pass = %v, want &false", it.Pass)
		}
	})

	t.Run("size breach WITH Trickle-Size-Exception trailer passes", func(t *testing.T) {
		in := fullyPassingInput()
		in.SmallDiff.NetLOC = 900
		in.SmallDiff.FilesChanged = 25
		in.SmallDiff.SizeExceptionTrailer = "generated proto regen — mechanical"
		d := EvaluateTrickleEligibility(in)
		it, _ := itemByID(d, TrickleItemSmallDiff)
		if it.Pass == nil || !*it.Pass {
			t.Errorf("small-diff Pass = %v, want &true (exception trailer overrides)", it.Pass)
		}
		if !d.Eligible {
			t.Errorf("expected eligible with size-exception trailer, got: %s", d.Reason)
		}
	})

	t.Run("multi-concern without ack blocks; ack overrides", func(t *testing.T) {
		in := fullyPassingInput()
		in.SmallDiff.SingleConcern = false
		d := EvaluateTrickleEligibility(in)
		if d.Eligible {
			t.Fatal("expected blocked on multi-concern")
		}
		in.SmallDiff.MultiConcernAckTrailer = "coupled: schema field + its only reader"
		d = EvaluateTrickleEligibility(in)
		if !d.Eligible {
			t.Errorf("expected eligible with Multi-Concern-Ack, got: %s", d.Reason)
		}
	})

	t.Run("present standalone-build red blocks; nil does not", func(t *testing.T) {
		in := fullyPassingInput()
		in.SmallDiff.StandaloneBuildGreen = boolPtr(false)
		if EvaluateTrickleEligibility(in).Eligible {
			t.Fatal("expected blocked on red standalone build")
		}
		in.SmallDiff.StandaloneBuildGreen = nil // dark CI does not exist yet (§6.16)
		it, _ := itemByID(EvaluateTrickleEligibility(in), TrickleItemSmallDiff)
		if it.Pass == nil || !*it.Pass {
			t.Errorf("nil standalone-build should not fail small-diff; Pass=%v", it.Pass)
		}
	})
}

func TestEvaluateTrickleEligibility_RationaleNonTriviality(t *testing.T) {
	t.Run("Why byte-equal to Summary is trivial → block", func(t *testing.T) {
		in := fullyPassingInput()
		in.Dossier.Summary = "Add the trickle gate."
		in.Dossier.Why = "  Add the trickle gate.  " // trimmed-equal
		d := EvaluateTrickleEligibility(in)
		if d.Eligible {
			t.Fatal("expected blocked: Why == Summary")
		}
		it, _ := itemByID(d, TrickleItemRationale)
		if !strings.Contains(it.Evidence, "byte-equal") {
			t.Errorf("evidence %q should call out the Why==Summary triviality", it.Evidence)
		}
	})

	t.Run("empty Why blocks", func(t *testing.T) {
		in := fullyPassingInput()
		in.Dossier.Why = "   "
		if EvaluateTrickleEligibility(in).Eligible {
			t.Fatal("expected blocked on empty Why")
		}
	})

	t.Run("missing sections block", func(t *testing.T) {
		in := fullyPassingInput()
		in.Dossier.MissingSections = []string{"Risk assessment", "Provenance"}
		d := EvaluateTrickleEligibility(in)
		if d.Eligible {
			t.Fatal("expected blocked on missing dossier sections")
		}
		it, _ := itemByID(d, TrickleItemRationale)
		if !strings.Contains(it.Evidence, "Risk assessment") {
			t.Errorf("evidence %q should list the missing sections", it.Evidence)
		}
	})
}

func TestEvaluateTrickleEligibility_CoverageBypassBlocks(t *testing.T) {
	// The escape hatch that lets an MR merge into dark must still block trickle.
	in := fullyPassingInput()
	in.Coverage.Pass = true
	in.Coverage.Bypass = true
	d := EvaluateTrickleEligibility(in)
	if d.Eligible {
		t.Fatal("expected blocked: coverage bypass blocks trickle even when .pass=true")
	}
	it, _ := itemByID(d, TrickleItemCoverage)
	if !strings.Contains(it.Evidence, "bypass") {
		t.Errorf("evidence %q should name the bypass", it.Evidence)
	}
}

func TestEvaluateTrickleEligibility_CoverageBelowFloor(t *testing.T) {
	in := fullyPassingInput()
	in.Coverage.Pass = false
	if EvaluateTrickleEligibility(in).Eligible {
		t.Fatal("expected blocked on patch coverage below floor")
	}
}

func TestEvaluateTrickleEligibility_CodeReviewUnresolved(t *testing.T) {
	in := fullyPassingInput()
	in.CodeReview.UnresolvedFindings = 2
	d := EvaluateTrickleEligibility(in)
	if d.Eligible {
		t.Fatal("expected blocked on unresolved ≥80-confidence findings")
	}
	it, _ := itemByID(d, TrickleItemCodeReview)
	if !strings.Contains(it.Evidence, "unresolved") {
		t.Errorf("evidence %q should mention unresolved findings", it.Evidence)
	}
}

func TestEvaluateTrickleEligibility_SecurityComposesGate(t *testing.T) {
	// A failing named status check must fail the security item via the composed
	// EvaluateSecurityGate.
	in := fullyPassingInput()
	in.Security.StatusChecks["CI / Gitleaks"] = "failed"
	d := EvaluateTrickleEligibility(in)
	if d.Eligible {
		t.Fatal("expected blocked on failing security status check")
	}
	it, _ := itemByID(d, TrickleItemSecurity)
	if it.Pass == nil || *it.Pass {
		t.Errorf("security Pass = %v, want &false", it.Pass)
	}
	if !strings.Contains(it.Evidence, "Gitleaks") {
		t.Errorf("evidence %q should carry the security gate's reason", it.Evidence)
	}
}

func TestEvaluateTrickleEligibility_PerfGlobTriggered(t *testing.T) {
	t.Run("perf path touched + guard passes → pass, not NA", func(t *testing.T) {
		in := fullyPassingInput()
		in.Perf.PerfSensitivePathsTouched = true
		in.Perf.CallCountGuardPass = boolPtr(true)
		d := EvaluateTrickleEligibility(in)
		it, _ := itemByID(d, TrickleItemPerf)
		if it.NA {
			t.Fatal("perf should not be NA when a perf path is touched")
		}
		if it.Pass == nil || !*it.Pass {
			t.Errorf("perf Pass = %v, want &true", it.Pass)
		}
		if !d.Eligible {
			t.Errorf("expected eligible, got: %s", d.Reason)
		}
	})

	t.Run("perf path touched + guard fails → block", func(t *testing.T) {
		in := fullyPassingInput()
		in.Perf.PerfSensitivePathsTouched = true
		in.Perf.CallCountGuardPass = boolPtr(false)
		d := EvaluateTrickleEligibility(in)
		if d.Eligible {
			t.Fatal("expected blocked on failing call-count guard")
		}
	})

	t.Run("perf path touched + guard verdict unsupplied → pending (blocks, not false fail)", func(t *testing.T) {
		in := fullyPassingInput()
		in.Perf.PerfSensitivePathsTouched = true
		in.Perf.CallCountGuardPass = nil // runs in CI / Test; not resolvable here
		d := EvaluateTrickleEligibility(in)
		it, _ := itemByID(d, TrickleItemPerf)
		if it.NA {
			t.Fatal("triggered perf must not be NA")
		}
		if it.Pass != nil {
			t.Errorf("unverdicted perf Pass = %v, want nil (pending)", it.Pass)
		}
		if d.Eligible {
			t.Fatal("pending perf must block eligibility")
		}
	})
}

func TestEvaluateTrickleEligibility_AdvisoryNeverBlocks(t *testing.T) {
	in := fullyPassingInput()
	// Explicitly fail every advisory item.
	in.Advisory.BenchmarkPass = boolPtr(false)
	in.Advisory.RationaleDepthPass = boolPtr(false)
	in.Advisory.TestQualityPass = boolPtr(false)
	d := EvaluateTrickleEligibility(in)
	if !d.Eligible {
		t.Fatalf("advisory fails must NOT block eligibility; got: %s", d.Reason)
	}
	bench, _ := itemByID(d, TrickleItemBenchmark)
	if bench.Pass == nil || *bench.Pass {
		t.Errorf("benchmark Pass = %v, want &false (surfaced but non-blocking)", bench.Pass)
	}
}

func TestEvaluateTrickleEligibility_MultipleBlockersInReason(t *testing.T) {
	in := fullyPassingInput()
	in.Provenance.MRURLSet = false
	in.Coverage.Bypass = true
	d := EvaluateTrickleEligibility(in)
	if d.Eligible {
		t.Fatal("expected blocked")
	}
	for _, id := range []string{TrickleItemProvenance, TrickleItemCoverage} {
		if !strings.Contains(d.Reason, id) {
			t.Errorf("reason %q should name blocking item %q", d.Reason, id)
		}
	}
}

func TestPerfSensitivePathTouched(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"viewer handler", []string{"gardenio/controller/cmd/beads-viewer/server.go"}, true},
		{"postgres store", []string{"kbeads/internal/store/postgres/operators.go"}, true},
		{"unrelated go", []string{"gardenio/controller/internal/bridge/gate.go"}, false},
		{"docs only", []string{"docs/foo.md"}, false},
		{"mixed", []string{"README.md", "kbeads/internal/store/postgres/batch_queries.go"}, true},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, hits := PerfSensitivePathTouched(c.files)
			if got != c.want {
				t.Errorf("PerfSensitivePathTouched(%v) = %v, want %v (hits=%v)", c.files, got, c.want, hits)
			}
		})
	}
}

func TestCountTrickleDiffFiles(t *testing.T) {
	files := []string{
		"gardenio/controller/internal/bridge/mr_trickle_gate.go", // counted
		"gardenio/controller/go.sum",                             // lock — excluded
		"kbeads/gen/beads/v1/beads.pb.go",                        // generated — excluded
		"coop/Cargo.lock",                                        // lock — excluded
		"vendor/foo/bar.go",                                      // vendored — excluded
		"docs/design.md",                                         // counted
	}
	if got := CountTrickleDiffFiles(files); got != 2 {
		t.Errorf("CountTrickleDiffFiles = %d, want 2 (only the .go source + the doc)", got)
	}
	if IsTrickleCountedPath("go.sum") {
		t.Error("go.sum should not be a counted path")
	}
	if !IsTrickleCountedPath("main.go") {
		t.Error("main.go should be a counted path")
	}
}

func TestParseTrickleDossier(t *testing.T) {
	t.Run("complete dossier with distinct Why passes", func(t *testing.T) {
		desc := `## Summary
Adds the trickle gate.

## Why
The bar had no enforcing function; downstream tooling needs one.

## Alternatives considered
Considered a shell script; rejected — not unit-testable.

## Risk assessment
Read-only gate; no blast radius.

## Evidence
- tests: green

## Provenance
- Dark bead: gd-abc123

Operator: Matt Baker (slack:U0OPERATOR1)`
		d := ParseTrickleDossier(desc)
		if len(d.MissingSections) != 0 {
			t.Errorf("MissingSections = %v, want none", d.MissingSections)
		}
		if !strings.Contains(d.Why, "enforcing function") {
			t.Errorf("Why not parsed: %q", d.Why)
		}
		if !strings.Contains(d.Summary, "trickle gate") {
			t.Errorf("Summary not parsed: %q", d.Summary)
		}
		// Feed into the gate — should pass the rationale item.
		it := evalRationale(d)
		if it.Pass == nil || !*it.Pass {
			t.Errorf("rationale should pass; Pass=%v evidence=%q", it.Pass, it.Evidence)
		}
	})

	t.Run("template placeholders read as empty → missing/trivial", func(t *testing.T) {
		desc := `## Summary
_One-to-three-line summary of what this slice delivers._

## Why
<!-- guidance -->
_The problem this solves and why it was requested — distinct from Summary._`
		d := ParseTrickleDossier(desc)
		// Alternatives/Risk/Evidence/Provenance absent → missing.
		if len(d.MissingSections) < 4 {
			t.Errorf("expected several missing sections, got %v", d.MissingSections)
		}
		if d.Summary != "" || d.Why != "" {
			t.Errorf("placeholder bodies should be empty; Summary=%q Why=%q", d.Summary, d.Why)
		}
	})
}

func TestTrickleTrailerExtractors(t *testing.T) {
	desc := `Some prose.

Operator: Matt Baker (slack:U0OPERATOR1)
Trickle-Size-Exception: generated proto regen
Dark-MR: https://gitlab.example/dark/repo/-/merge_requests/42`
	if got := TrickleOperatorTrailer(desc); !strings.Contains(got, "Matt Baker") {
		t.Errorf("Operator trailer = %q", got)
	}
	if got := TrickleSizeExceptionTrailer(desc); got != "generated proto regen" {
		t.Errorf("size-exception trailer = %q", got)
	}
	if TrickleMultiConcernAckTrailer(desc) != "" {
		t.Error("no Multi-Concern-Ack should be found")
	}
	if !TrickleDarkMRReferenced(desc) {
		t.Error("Dark-MR anchor should be detected")
	}
	if TrickleDarkMRReferenced("no anchors here") {
		t.Error("bare prose should not match a dark anchor")
	}
}

// TestTrickleDecisionJSONMatchesViewerContract asserts the decision serializes to
// the exact shape the already-shipped beads-viewer trickle_checklist consumer
// parses (cmd/beads-viewer/trickle_checklist.go trickleGateJSON / trickleGateJSONItem):
// {eligible, reason, checklist:[{id, kind, pass, na, evidence}]}, with pass a
// tri-state (null when pending). This is the cross-package contract the whole
// design hinges on, so it is pinned here.
func TestTrickleDecisionJSONMatchesViewerContract(t *testing.T) {
	in := fullyPassingInput()
	in.CodeReview.Evaluated = false // leave one item pending → pass must be null
	d := EvaluateTrickleEligibility(in)

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Decode into the viewer's exact field shape (mirrored here) to prove the
	// keys/types line up.
	var viewer struct {
		Eligible  bool   `json:"eligible"`
		Reason    string `json:"reason"`
		Checklist []struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			Pass     *bool  `json:"pass"`
			NA       bool   `json:"na"`
			Evidence string `json:"evidence"`
		} `json:"checklist"`
	}
	if err := json.Unmarshal(raw, &viewer); err != nil {
		t.Fatalf("unmarshal into viewer contract: %v", err)
	}
	if len(viewer.Checklist) != 10 {
		t.Fatalf("checklist len = %d, want 10", len(viewer.Checklist))
	}

	// The pending code-review row must serialize pass as JSON null (tri-state).
	if !strings.Contains(string(raw), `"id":"code-review","kind":"HARD","pass":null`) {
		t.Errorf("pending code-review row should serialize pass:null; got %s", raw)
	}

	// Sanity: a HARD pass serializes pass:true, an N/A serializes na:true.
	var sawPassTrue, sawNA bool
	for _, it := range viewer.Checklist {
		if it.ID == TrickleItemProvenance && it.Pass != nil && *it.Pass {
			sawPassTrue = true
		}
		if it.ID == TrickleItemPerf && it.NA {
			sawNA = true
		}
	}
	if !sawPassTrue {
		t.Error("provenance should serialize pass:true")
	}
	if !sawNA {
		t.Error("perf should serialize na:true")
	}
}
