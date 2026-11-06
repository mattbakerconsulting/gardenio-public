package bridge

import (
	"fmt"
	"regexp"
	"strings"
)

// mr_trickle_gate.go implements the trickle-eligibility bar for a monorepo-dark
// MR (bundle ka-0000005053 §3 "The Trickle-Eligibility Bar" + §5 "Enforcement";
// task ka-0000004967, master epic ka-0000005560). It is the top-level AGGREGATOR
// that `gb mr trickle-gate <bead-id-or-mr-url> [--json] [--enforce]` wraps —
// modeled 1:1 on `gb mr relevance-gate`.
//
// Like EvaluateTicketResolvedCloseGate / EvaluateDispatchGate / EvaluateSecurityGate
// in this package, EvaluateTrickleEligibility is a PURE function with no
// GitLab/daemon I/O, so the whole decision surface is table-testable in
// mr_trickle_gate_test.go. The CLI gathers the per-affordance facts (from
// GitLab, the daemon, and repo files), hands them in as a fully-resolved
// TrickleEligibilityInput, and this function applies the bar's decision rules.
//
// # The bar (bundle §3)
//
// An ordered checklist. HARD items block eligibility; ADVISORY items surface in
// the checklist without affecting the verdict. The stable per-item slug IDs and
// the HARD/ADVISORY split match the already-shipped beads-viewer renderer
// (cmd/beads-viewer/trickle_checklist.go trickleBarSpec), so this gate's --json
// output maps onto that view 1:1 — the gate FILLS the grid the viewer draws.
//
//	1  provenance     HARD  mr_url + linked issue bead + Operator: trailer + dark-MR edge
//	2  small-diff     HARD  ≤400 net LOC, ≤10 files, single concern (or exception trailer)
//	3  rationale      HARD  trust-dossier sections present & non-trivial (Why ≠ Summary)
//	4  coverage       HARD  patch-cov .pass && no .bypass; e2e summary clean
//	5  code-review    HARD  /code-review ran; ≥80-confidence findings resolved; artifact attached
//	6  security       HARD  EvaluateSecurityGate green (gitleaks/govulncheck/… + triage)
//	7  perf           HARD (glob-triggered) call-count guard holds; N/A when no perf path touched
//	—  perf-benchmark ADVISORY  bench.txt compiles (no benchstat threshold yet)
//	—  rationale-depth ADVISORY qualitative prose quality — human-adjudicated
//	—  test-quality   ADVISORY  assert-density + mutation testing — DEFERRED, pilot-gated
//
// Ordering note: the checklist below is ordered cheap/structural checks first
// (provenance, small-diff, rationale) so a human reading the reason sees the
// cheapest blocker first, mirroring §3's "cheap/structural checks short-circuit
// before expensive ones". Eligibility itself is order-independent (every HARD
// item must pass), so the order is purely presentational.

// Trickle checklist item kinds — HARD blocks eligibility, ADVISORY surfaces only.
// Values match the beads-viewer renderer's trickleHard / trickleAdvisory.
const (
	TrickleHard     = "HARD"
	TrickleAdvisory = "ADVISORY"
)

// Trickle checklist item IDs — the stable slugs the gate keys evidence by and
// the beads-viewer renderer matches against (trickle_checklist.go trickleBarSpec).
// Keep these in sync with that spec and deploy-values/gitlab-dark/trickle/README.md.
const (
	TrickleItemProvenance     = "provenance"
	TrickleItemSmallDiff      = "small-diff"
	TrickleItemRationale      = "rationale"
	TrickleItemCoverage       = "coverage"
	TrickleItemCodeReview     = "code-review"
	TrickleItemSecurity       = "security"
	TrickleItemPerf           = "perf"
	TrickleItemBenchmark      = "perf-benchmark"
	TrickleItemRationaleDepth = "rationale-depth"
	TrickleItemTestQuality    = "test-quality"
)

// Default small-diff bounds (bundle §2.5, all overridable via a justified
// Trickle-Size-Exception: trailer). A breach of either bound blocks the
// small-diff HARD item unless the exception trailer is present.
const (
	TrickleMaxNetLOC = 400
	TrickleMaxFiles  = 10
)

// GateItem is one row of the trickle bar's evaluated checklist. It serializes to
// the exact JSON the beads-viewer trickle_checklist field consumer parses
// (trickleGateJSONItem): {id, kind, pass, na, evidence}, with pass a tri-state
// pointer so a genuine fail (false) is distinct from "not evaluated" (nil).
type GateItem struct {
	// ID is the stable slug (TrickleItem* consts).
	ID string `json:"id"`
	// Kind is HARD | ADVISORY (TrickleHard / TrickleAdvisory).
	Kind string `json:"kind"`
	// Pass is a tri-state: nil = not evaluated (pending — no evidence yet),
	// &true = the affordance's gate is green, &false = a real fail. Mirrors the
	// viewer's *bool contract so the omitted-vs-false distinction survives JSON.
	Pass *bool `json:"pass"`
	// NA is true when the affordance does not apply to this MR (e.g. the perf
	// gate is glob-triggered and no perf-sensitive path was touched). N/A does
	// NOT block eligibility.
	NA bool `json:"na"`
	// Evidence is the human-readable links/output backing the status.
	Evidence string `json:"evidence"`
}

// blocks reports whether this item blocks trickle-eligibility: a HARD item that
// is neither passing nor N/A (i.e. an explicit fail OR still-pending) blocks.
// ADVISORY items never block; N/A never blocks.
func (it GateItem) blocks() bool {
	if it.Kind != TrickleHard || it.NA {
		return false
	}
	return it.Pass == nil || !*it.Pass
}

// passing reports whether a HARD item counts toward the "N of M HARD passing"
// tally — an explicit pass or an N/A (which does not block).
func (it GateItem) passing() bool {
	return it.NA || (it.Pass != nil && *it.Pass)
}

// TrickleEligibilityDecision is the outcome of the trickle-eligibility bar.
// It serializes to the beads-viewer trickle_checklist field shape (trickleGateJSON):
// {eligible, reason, checklist}.
type TrickleEligibilityDecision struct {
	// Eligible is true IFF every HARD checklist item passes (or is N/A).
	Eligible bool `json:"eligible"`
	// Reason is a one-line, human-readable explanation — the eligible message,
	// or the list of blocking HARD items when not eligible.
	Reason string `json:"reason"`
	// Checklist is the ordered per-item bar. HARD rows precede ADVISORY rows.
	Checklist []GateItem `json:"checklist"`
}

// HardPass / HardTotal count the HARD items for a "N of M HARD checks passing"
// summary (the CLI text output uses these; the viewer derives its own).
func (d TrickleEligibilityDecision) HardPass() int {
	n := 0
	for _, it := range d.Checklist {
		if it.Kind == TrickleHard && it.passing() {
			n++
		}
	}
	return n
}

func (d TrickleEligibilityDecision) HardTotal() int {
	n := 0
	for _, it := range d.Checklist {
		if it.Kind == TrickleHard {
			n++
		}
	}
	return n
}

// ── Per-affordance inputs ────────────────────────────────────────────────────
//
// Each sub-struct carries the RESOLVED facts for one affordance; the DECISION
// RULE lives in EvaluateTrickleEligibility below (not pre-computed by the CLI),
// so the rules — coverage-bypass-blocks, the small-diff bounds + exception
// trailer, the dossier Why≠Summary non-triviality, provenance completeness — are
// all table-testable here.

// TrickleProvenanceInput is the resolved provenance facts (§2.7). Completeness =
// mr_url set on the linked bead + a linked issue/feature bead + an Operator:
// trailer/operated_by + a typed-mention edge to the dark-fork MR(s).
type TrickleProvenanceInput struct {
	Evaluated          bool // false => provenance check is pending (no evidence)
	MRURLSet           bool
	LinkedIssueBeadSet bool
	OperatorTrailerSet bool
	DarkMRAncestrySet  bool
	Evidence           string
}

// TrickleSmallDiffInput is the resolved diff-size + single-concern facts (§2.5).
// NetLOC/FilesChanged are computed EXCLUDING generated/lock/vendored files by
// the caller. SingleConcern is the caller's concern-count verdict. The two
// escape-hatch trailers (Trickle-Size-Exception:/Multi-Concern-Ack:) are the
// justified overrides, mirroring the Coverage-Bypass: organic-trailer path.
type TrickleSmallDiffInput struct {
	Evaluated              bool // false => small-diff check is pending
	NetLOC                 int
	FilesChanged           int
	SingleConcern          bool
	SizeExceptionTrailer   string // non-empty reason overrides the size bounds
	MultiConcernAckTrailer string // non-empty reason overrides single-concern
	// StandaloneBuildGreen is a tri-state: nil = not yet known (the dark CI
	// pipeline that would prove standalone build+test on upstream HEAD does not
	// exist yet — §6.16), so its absence does NOT by itself fail the item; a
	// present false is a hard fail. §3 item 6.
	StandaloneBuildGreen *bool
	Evidence             string
}

// TrickleDossierInput is the resolved trust-dossier facts (§2.6/§4). The
// presence + non-triviality half is HARD (Why present and NOT byte-equal to
// Summary); qualitative depth is the separate ADVISORY rationale-depth item.
type TrickleDossierInput struct {
	Evaluated       bool     // false => rationale check is pending
	MissingSections []string // required §4 sections absent from the description
	Summary         string
	Why             string
	Evidence        string
}

// TrickleCoverageInput is the resolved patch-coverage facts (§2.2). Pass IFF the
// patch-cov result is present, .pass==true AND .bypass is absent (a bypass
// blocks trickle even if the MR merged into dark), and — when known — the e2e
// summary had zero failures.
type TrickleCoverageInput struct {
	Evaluated     bool // false => coverage check is pending
	ResultPresent bool
	Pass          bool
	Bypass        bool
	// E2EGreen is a tri-state: nil = the e2e run is not recorded yet (the dark
	// e2e pipeline does not exist — §6.16), so absence does not fail the item; a
	// present false is a hard fail.
	E2EGreen *bool
	Evidence string
}

// TrickleCodeReviewInput is the resolved /code-review facts (§2.1). Pass IFF the
// review ran, a review-provenance artifact is attached, and every ≥80-confidence
// finding was resolved or dismissed (UnresolvedFindings==0).
type TrickleCodeReviewInput struct {
	Evaluated          bool // false => code-review check is pending
	Ran                bool
	ArtifactAttached   bool
	UnresolvedFindings int
	Evidence           string
}

// TricklePerfInput is the resolved perf facts (§2.4). Perf is GLOB-TRIGGERED: it
// is N/A (does not block) unless a perf-sensitive path was touched. When
// triggered, Pass IFF the deterministic call-count guard holds.
type TricklePerfInput struct {
	Evaluated                 bool // false => perf check is pending (and triggered)
	PerfSensitivePathsTouched bool
	// CallCountGuardPass is a tri-state: nil = the guard verdict is not known yet
	// (it runs in CI / Test, which the CLI cannot re-run), so a triggered-but-
	// unverdicted perf item stays PENDING rather than a false fail; &true/&false
	// is the resolved guard result.
	CallCountGuardPass *bool
	Evidence           string
}

// TrickleAdvisoryInput carries the ADVISORY-item facts (§2.4/§2.6/§2.2). These
// never block eligibility; they surface in the checklist. A nil Pass leaves the
// item pending.
type TrickleAdvisoryInput struct {
	// Benchmark: bench.txt regenerated + compiles (perf-benchmark).
	BenchmarkPass     *bool
	BenchmarkEvidence string
	// RationaleDepth: qualitative prose quality (rationale-depth) — human word.
	RationaleDepthPass     *bool
	RationaleDepthEvidence string
	// TestQuality: assert-density + mutation testing (test-quality) — DEFERRED.
	TestQualityPass     *bool
	TestQualityEvidence string
}

// TrickleEligibilityInput is the fully-resolved, I/O-free input to the bar. The
// CLI gathers each affordance's facts; this struct is the seam between the
// GitLab/daemon side and the pure decision.
type TrickleEligibilityInput struct {
	Provenance TrickleProvenanceInput
	SmallDiff  TrickleSmallDiffInput
	Dossier    TrickleDossierInput
	Coverage   TrickleCoverageInput
	CodeReview TrickleCodeReviewInput
	// Security composes the existing pure security gate. When SecurityEvaluated
	// is true, EvaluateSecurityGate(Security) supplies the security verdict;
	// otherwise the security item is pending.
	Security          SecurityGateInput
	SecurityEvaluated bool
	Perf              TricklePerfInput
	Advisory          TrickleAdvisoryInput
}

// boolPtr is a tiny helper for the tri-state Pass fields.
func boolPtr(b bool) *bool { return &b }

// EvaluateTrickleEligibility applies the trickle-eligibility bar (bundle §3) to
// a fully-resolved input and returns the ordered checklist + eligibility
// verdict. It is pure: no network, no daemon, no clock.
//
// Eligibility rule: Eligible == every HARD item passes (explicit pass) or is
// N/A. A HARD item that is pending (no evidence) or an explicit fail blocks —
// pending blocks because "we could not prove it" is not "it passed" (fail-closed
// on the trickle candidate; the peel stays not-eligible until proven). ADVISORY
// items never affect Eligible.
func EvaluateTrickleEligibility(in TrickleEligibilityInput) TrickleEligibilityDecision {
	var hard []GateItem
	hard = append(hard, evalProvenance(in.Provenance))
	hard = append(hard, evalSmallDiff(in.SmallDiff))
	hard = append(hard, evalRationale(in.Dossier))
	hard = append(hard, evalCoverage(in.Coverage))
	hard = append(hard, evalCodeReview(in.CodeReview))
	hard = append(hard, evalSecurity(in.Security, in.SecurityEvaluated))
	hard = append(hard, evalPerf(in.Perf))

	advisory := evalAdvisory(in.Advisory)

	checklist := append(hard, advisory...)

	// Collect blocking HARD items in checklist order for a legible reason.
	var blocking []string
	for _, it := range hard {
		if it.blocks() {
			blocking = append(blocking, it.ID)
		}
	}

	dec := TrickleEligibilityDecision{Checklist: checklist}
	if len(blocking) == 0 {
		dec.Eligible = true
		dec.Reason = fmt.Sprintf("eligible — all %d HARD checks pass", len(hard))
	} else {
		dec.Eligible = false
		dec.Reason = fmt.Sprintf("blocked — %d/%d HARD checks not passing: %s",
			len(blocking), len(hard), strings.Join(blocking, ", "))
	}
	return dec
}

// ── Per-item evaluators ──────────────────────────────────────────────────────

func evalProvenance(p TrickleProvenanceInput) GateItem {
	it := GateItem{ID: TrickleItemProvenance, Kind: TrickleHard, Evidence: p.Evidence}
	if !p.Evaluated {
		return it // pending
	}
	var missing []string
	if !p.MRURLSet {
		missing = append(missing, "mr_url")
	}
	if !p.LinkedIssueBeadSet {
		missing = append(missing, "linked issue bead")
	}
	if !p.OperatorTrailerSet {
		missing = append(missing, "Operator: trailer/operated_by")
	}
	if !p.DarkMRAncestrySet {
		missing = append(missing, "dark-MR ancestry edge")
	}
	pass := len(missing) == 0
	it.Pass = boolPtr(pass)
	if !pass {
		it.Evidence = joinEvidence(p.Evidence, "missing: "+strings.Join(missing, ", "))
	}
	return it
}

func evalSmallDiff(s TrickleSmallDiffInput) GateItem {
	it := GateItem{ID: TrickleItemSmallDiff, Kind: TrickleHard, Evidence: s.Evidence}
	if !s.Evaluated {
		return it
	}
	var fails []string

	sizeBreached := s.NetLOC > TrickleMaxNetLOC || s.FilesChanged > TrickleMaxFiles
	if sizeBreached && strings.TrimSpace(s.SizeExceptionTrailer) == "" {
		fails = append(fails, fmt.Sprintf("size %d LOC / %d files exceeds %d/%d (no Trickle-Size-Exception:)",
			s.NetLOC, s.FilesChanged, TrickleMaxNetLOC, TrickleMaxFiles))
	}
	if !s.SingleConcern && strings.TrimSpace(s.MultiConcernAckTrailer) == "" {
		fails = append(fails, "multiple concerns (no Multi-Concern-Ack:)")
	}
	// StandaloneBuildGreen: a present false is a hard fail; nil (dark CI does not
	// exist yet, §6.16) does not fail the item on its own.
	if s.StandaloneBuildGreen != nil && !*s.StandaloneBuildGreen {
		fails = append(fails, "standalone build/test on upstream HEAD is red")
	}

	pass := len(fails) == 0
	it.Pass = boolPtr(pass)
	if !pass {
		it.Evidence = joinEvidence(s.Evidence, strings.Join(fails, "; "))
	}
	return it
}

func evalRationale(d TrickleDossierInput) GateItem {
	it := GateItem{ID: TrickleItemRationale, Kind: TrickleHard, Evidence: d.Evidence}
	if !d.Evaluated {
		return it
	}
	var fails []string
	if len(d.MissingSections) > 0 {
		fails = append(fails, "missing dossier sections: "+strings.Join(d.MissingSections, ", "))
	}
	why := strings.TrimSpace(d.Why)
	if why == "" {
		fails = append(fails, "Why section is empty")
	} else if why == strings.TrimSpace(d.Summary) {
		// The core non-triviality rule (§2.6/§4): a Why that restates Summary
		// demonstrates no understanding.
		fails = append(fails, "Why is byte-equal to Summary (trivial rationale)")
	}
	pass := len(fails) == 0
	it.Pass = boolPtr(pass)
	if !pass {
		it.Evidence = joinEvidence(d.Evidence, strings.Join(fails, "; "))
	}
	return it
}

func evalCoverage(c TrickleCoverageInput) GateItem {
	it := GateItem{ID: TrickleItemCoverage, Kind: TrickleHard, Evidence: c.Evidence}
	if !c.Evaluated {
		return it
	}
	var fails []string
	if !c.ResultPresent {
		fails = append(fails, "no patch-cov-result.json recorded")
	} else {
		if !c.Pass {
			fails = append(fails, "patch coverage below floor (.pass=false)")
		}
		// A bypass blocks trickle eligibility even though the MR may merge into
		// dark to unblock work (§2.2 "kill the escape hatch in dark").
		if c.Bypass {
			fails = append(fails, "coverage bypass present (blocks trickle even if merged into dark)")
		}
	}
	if c.E2EGreen != nil && !*c.E2EGreen {
		fails = append(fails, "e2e summary has failures")
	}
	pass := len(fails) == 0
	it.Pass = boolPtr(pass)
	if !pass {
		it.Evidence = joinEvidence(c.Evidence, strings.Join(fails, "; "))
	}
	return it
}

func evalCodeReview(r TrickleCodeReviewInput) GateItem {
	it := GateItem{ID: TrickleItemCodeReview, Kind: TrickleHard, Evidence: r.Evidence}
	if !r.Evaluated {
		return it
	}
	var fails []string
	if !r.Ran {
		fails = append(fails, "/code-review did not run")
	}
	if !r.ArtifactAttached {
		fails = append(fails, "no review-provenance artifact attached")
	}
	if r.UnresolvedFindings > 0 {
		fails = append(fails, fmt.Sprintf("%d ≥80-confidence finding(s) unresolved", r.UnresolvedFindings))
	}
	pass := len(fails) == 0
	it.Pass = boolPtr(pass)
	if !pass {
		it.Evidence = joinEvidence(r.Evidence, strings.Join(fails, "; "))
	}
	return it
}

// evalSecurity composes the existing pure EvaluateSecurityGate (§2.3). When the
// caller did not resolve the security inputs (evaluated=false), the item stays
// pending.
func evalSecurity(sin SecurityGateInput, evaluated bool) GateItem {
	it := GateItem{ID: TrickleItemSecurity, Kind: TrickleHard}
	if !evaluated {
		return it
	}
	sdec := EvaluateSecurityGate(sin)
	it.Pass = boolPtr(sdec.Pass)
	if sdec.Pass {
		it.Evidence = "security gate green"
	} else {
		it.Evidence = strings.Join(sdec.Reasons, "; ")
	}
	return it
}

func evalPerf(p TricklePerfInput) GateItem {
	it := GateItem{ID: TrickleItemPerf, Kind: TrickleHard, Evidence: p.Evidence}
	// Glob-triggered: N/A (does not block) when no perf-sensitive path is touched.
	if p.Evaluated && !p.PerfSensitivePathsTouched {
		it.NA = true
		if it.Evidence == "" {
			it.Evidence = "no perf-sensitive path touched — N/A"
		}
		return it
	}
	if !p.Evaluated {
		return it // pending
	}
	// Triggered. The call-count guard runs in CI / Test — when its verdict is not
	// supplied (nil), the item stays PENDING (blocks under --enforce until the CI
	// leg feeds it) rather than a false fail. A supplied verdict resolves it.
	if p.CallCountGuardPass == nil {
		it.Evidence = joinEvidence(p.Evidence, "call-count guard verdict not supplied (runs in CI / Test)")
		return it
	}
	it.Pass = boolPtr(*p.CallCountGuardPass)
	if !*p.CallCountGuardPass {
		it.Evidence = joinEvidence(p.Evidence, "deterministic call-count guard failed")
	}
	return it
}

// evalAdvisory builds the three ADVISORY rows. None block; a nil Pass leaves the
// row pending.
func evalAdvisory(a TrickleAdvisoryInput) []GateItem {
	return []GateItem{
		{ID: TrickleItemBenchmark, Kind: TrickleAdvisory, Pass: a.BenchmarkPass, Evidence: a.BenchmarkEvidence},
		{ID: TrickleItemRationaleDepth, Kind: TrickleAdvisory, Pass: a.RationaleDepthPass, Evidence: a.RationaleDepthEvidence},
		{ID: TrickleItemTestQuality, Kind: TrickleAdvisory, Pass: a.TestQualityPass, Evidence: a.TestQualityEvidence},
	}
}

// joinEvidence concatenates a base evidence string with an appended detail,
// keeping either half when the other is empty.
func joinEvidence(base, detail string) string {
	base = strings.TrimSpace(base)
	detail = strings.TrimSpace(detail)
	switch {
	case base == "":
		return detail
	case detail == "":
		return base
	default:
		return base + " — " + detail
	}
}

// ── Pure CLI-support helpers (perf glob, diff sizing, dossier, trailers) ──────
//
// These I/O-free helpers let the CLI wrapper stay thin: the GitLab side supplies
// raw changed-file lists and the MR description, and these functions apply the
// bar's structural rules — all table-testable here alongside the decision.

// TricklePerfSensitiveGlobs are the path patterns that flip the perf HARD item
// from N/A to required (§2.4): the fan-out handlers whose deterministic
// daemon-call-count guard (gardener_owner_nplus1_test.go) must hold, and the
// kbeads Postgres store where a new unbounded multi-id query would regress the
// lib/pq 65535-param invariant (advice ka-0000000229). Matching is prefix/glob on
// the repo-relative path.
var TricklePerfSensitiveGlobs = []string{
	"gardenio/controller/cmd/beads-viewer/",
	"kbeads/internal/store/postgres/",
}

// PerfSensitivePathTouched reports whether any changed file lands under a
// perf-sensitive glob — the trigger that makes the perf item required rather
// than N/A. Returns the matching files (sorted, deduped) for evidence.
func PerfSensitivePathTouched(changedFiles []string) (bool, []string) {
	seen := map[string]bool{}
	var hits []string
	for _, f := range changedFiles {
		lf := strings.ToLower(strings.TrimSpace(f))
		if lf == "" {
			continue
		}
		for _, g := range TricklePerfSensitiveGlobs {
			if strings.HasPrefix(lf, strings.ToLower(g)) && !seen[lf] {
				seen[lf] = true
				hits = append(hits, f)
				break
			}
		}
	}
	return len(hits) > 0, hits
}

// trickleNonCountedPathRe matches files EXCLUDED from the small-diff LOC/file
// budget (§2.5): generated code, lockfiles, vendored trees. A mechanical
// regen/lock churn must not count against the human-authored-slice budget.
var trickleNonCountedPathRe = regexp.MustCompile(`(?i)(` +
	`(^|/)vendor/|` +
	`(^|/)gen/|` +
	`\.pb\.go$|_pb2\.py$|\.gen\.go$|generated|` +
	`(^|/)(go\.sum|go\.work\.sum|cargo\.lock|bun\.lock|bun\.lockb|package-lock\.json|yarn\.lock|pnpm-lock\.yaml)$|` +
	`\.lock$` +
	`)`)

// IsTrickleCountedPath reports whether a path counts toward the small-diff
// budget (i.e. it is NOT generated/lock/vendored).
func IsTrickleCountedPath(p string) bool {
	return !trickleNonCountedPathRe.MatchString(strings.TrimSpace(p))
}

// CountTrickleDiffFiles returns the number of changed files that count toward
// the small-diff file budget (excluding generated/lock/vendored).
func CountTrickleDiffFiles(changedFiles []string) int {
	n := 0
	for _, f := range changedFiles {
		if strings.TrimSpace(f) != "" && IsTrickleCountedPath(f) {
			n++
		}
	}
	return n
}

// TrickleDossierSections is the required §4 trust-dossier section set, in
// canonical order. Kept in sync with .gitlab/merge_request_templates/trust-dossier.md
// and scripts/lint-trust-dossier.sh REQUIRED_SECTIONS.
var TrickleDossierSections = []string{
	"Summary", "Why", "Alternatives considered", "Risk assessment", "Evidence", "Provenance",
}

// dossierHeadingRe matches a `## <Section>` markdown heading, capturing the
// heading text. Case-insensitive, tolerant of leading whitespace.
var dossierHeadingRe = regexp.MustCompile(`(?im)^[ \t]*##[ \t]+(.+?)[ \t]*$`)

// ParseTrickleDossier extracts the trust-dossier fields from an MR description
// for the rationale HARD item: which required §4 sections are present, and the
// Summary / Why section bodies (for the Why≠Summary non-triviality check). It
// reads the whole description (not just between DOSSIER markers) so it works on
// any MR carrying the section headings; a section body is the text from its
// heading to the next `##` heading (placeholder/HTML-comment lines stripped).
func ParseTrickleDossier(description string) TrickleDossierInput {
	in := TrickleDossierInput{Evaluated: true}
	bodies := dossierSectionBodies(description)

	present := map[string]bool{}
	for name := range bodies {
		present[strings.ToLower(name)] = true
	}
	for _, want := range TrickleDossierSections {
		if !present[strings.ToLower(want)] {
			in.MissingSections = append(in.MissingSections, want)
		}
	}
	in.Summary = bodies["summary"]
	in.Why = bodies["why"]
	return in
}

// dossierSectionBodies splits a description into lowercased-section-name →
// cleaned body. The body excludes the heading line, HTML comments, and
// placeholder italic lines (a leading `_…_` template stub), so a body that is
// only the template placeholder reads as empty.
func dossierSectionBodies(description string) map[string]string {
	locs := dossierHeadingRe.FindAllStringSubmatchIndex(description, -1)
	bodies := map[string]string{}
	for i, loc := range locs {
		name := strings.ToLower(strings.TrimSpace(description[loc[2]:loc[3]]))
		bodyStart := loc[1]
		bodyEnd := len(description)
		if i+1 < len(locs) {
			bodyEnd = locs[i+1][0]
		}
		bodies[name] = cleanDossierBody(description[bodyStart:bodyEnd])
	}
	return bodies
}

// htmlCommentRe strips `<!-- ... -->` blocks (template guidance comments).
var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// cleanDossierBody normalizes a section body: drops HTML comments and template
// placeholder lines (`_italic stub_`), collapsing to the substantive prose. A
// body that is only a placeholder returns "".
func cleanDossierBody(body string) string {
	body = htmlCommentRe.ReplaceAllString(body, "")
	var keep []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// Template placeholder stub: a line that is entirely italic `_..._`.
		if strings.HasPrefix(t, "_") && strings.HasSuffix(t, "_") && len(t) > 1 {
			continue
		}
		keep = append(keep, t)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// trickleTrailerRe builds a case-insensitive line matcher for a `Key: value`
// MR-description trailer, capturing the value.
func trickleTrailerRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?im)^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*:[ \t]*(.+?)[ \t]*$`)
}

var (
	trickleSizeExceptionRe = trickleTrailerRe("Trickle-Size-Exception")
	trickleMultiConcernRe  = trickleTrailerRe("Multi-Concern-Ack")
	trickleOperatorRe      = trickleTrailerRe("Operator")
)

// ExtractTrickleTrailer returns the first non-empty value of the named trailer
// in an MR description, or "".
func extractTrailer(re *regexp.Regexp, description string) string {
	if m := re.FindStringSubmatch(description); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// TrickleSizeExceptionTrailer / TrickleMultiConcernAckTrailer / TrickleOperatorTrailer
// extract the corresponding §4/§2.5 trailers from an MR description.
func TrickleSizeExceptionTrailer(desc string) string {
	return extractTrailer(trickleSizeExceptionRe, desc)
}
func TrickleMultiConcernAckTrailer(desc string) string {
	return extractTrailer(trickleMultiConcernRe, desc)
}
func TrickleOperatorTrailer(desc string) string { return extractTrailer(trickleOperatorRe, desc) }

// TrickleDarkMRReferenced reports whether an MR description carries a dark-MR
// ancestry anchor (a Dark-MR:/Dark-Bead:/Peeled-From:/Peel-Of: trailer) — the
// description-side half of the provenance dark-ancestry check (§2.7). The
// bead-side half (a typed-mention edge) is resolved separately by the CLI.
var trickleDarkAnchorRe = regexp.MustCompile(`(?im)^[ \t]*(?:Dark-MR|Dark-Bead|Peeled-From|Peel-Of)[ \t]*:[ \t]*\S`)

func TrickleDarkMRReferenced(description string) bool {
	return trickleDarkAnchorRe.MatchString(description)
}
