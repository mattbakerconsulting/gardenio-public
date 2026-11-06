package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// mr_trickle.go wires the trickle-eligibility bar (bundle ka-0000005053 §3 + §5;
// task ka-0000004967, master epic ka-0000005560) into the `gb mr` command group:
//
//	gb mr trickle-gate <bead-id-or-mr-url> [--json] [--enforce]
//
// It is the top-level AGGREGATOR modeled 1:1 on `gb mr relevance-gate` /
// `gb mr security-gate`: it resolves the MR + its linked bead, gathers each
// affordance's facts (from GitLab, the daemon, the MR description, and the
// composed security gate), runs the PURE bridge.EvaluateTrickleEligibility, and
// reports the ordered HARD/ADVISORY checklist + eligibility verdict.
//
// Honest framing (bundle §5/§6, echoed by the beads-viewer renderer): the dark
// CI pipeline that would prove the coverage / code-review / e2e / standalone-build
// legs against the dark fork's code DOES NOT EXIST YET. So the CLI resolves the
// legs it CAN today — provenance, small-diff, single-concern, the trust-dossier,
// the perf glob trigger, and the composed security gate — directly, and accepts
// evidence for the CI-supplied legs via flags (so a gardener or the gate's own
// e2e can feed them). A leg with neither a resolver nor supplied evidence stays
// PENDING, which (fail-closed on the trickle candidate) blocks eligibility until
// proven — exactly the beads-viewer "all-pending until the gate lands" posture.
//
// Exit codes mirror the sibling gates:
//
//	0 = eligible (all HARD checks pass), OR blocked-but-advisory (default rollout)
//	3 = blocked AND --enforce set (a HARD check is failing/pending)
//	1 = a real error (bad target, GitLab/daemon failure)
//
// The advisory-first default (§5 "Land advisory-first … then flip to enforcing")
// means the gate reports without failing CI until an operator opts into --enforce.

var (
	mrTrickleEnforce  bool
	mrTrickleLockPath string
	mrTrickleWrite    bool

	// Evidence flags for the CI-supplied legs the dark pipeline does not yet
	// produce. Each is a tri-state: unset => that leg stays pending; set =>
	// supplies the verdict. Modeled after how the security gate reads its
	// Security-Scan block, but as explicit flags since there is no artifact yet.
	mrTrickleCoveragePass   string // "", "true", "false"
	mrTrickleCoverageBypass bool
	mrTrickleE2E            string // "", "true", "false"
	mrTrickleCodeReview     string // "", "true", "false" — /code-review clean
	mrTrickleUnresolved     int
	mrTrickleStandalone     string // "", "true", "false"
	mrTrickleSingleConcern  string // "", "true", "false" — override concern verdict
	mrTricklePerfGuard      string // "", "true", "false" — call-count guard verdict
)

var mrTrickleGateCmd = &cobra.Command{
	Use:   "trickle-gate <bead-id-or-mr-url>",
	Short: "Evaluate the trickle-eligibility bar for a monorepo-dark MR (bundle ka-0000005053 §3)",
	Long: `Evaluate the trickle-eligibility bar — the machine-checkable quality bar a
monorepo-dark MR must clear before it is eligible to trickle upstream (bundle
ka-0000005053 §3/§5, master epic ka-0000005560). Modeled 1:1 on
` + "`gb mr relevance-gate`" + ` / ` + "`gb mr security-gate`" + `.

The bar is an ordered checklist. HARD items block eligibility; ADVISORY items
surface without blocking:

  provenance     HARD  mr_url + linked issue bead + Operator: trailer + dark-MR anchor
  small-diff     HARD  <=400 net LOC, <=10 files, single concern (or exception trailer)
  rationale      HARD  trust-dossier sections present & non-trivial (Why != Summary)
  coverage       HARD  patch-cov .pass && no .bypass; e2e summary clean
  code-review    HARD  /code-review ran; >=80-confidence findings resolved
  security       HARD  the security gate is green (composes gb mr security-gate)
  perf           HARD  call-count guard holds — glob-triggered; N/A off perf paths
  perf-benchmark ADVISORY   bench.txt compiles (no benchstat threshold yet)
  rationale-depth ADVISORY  qualitative prose quality — human-adjudicated
  test-quality   ADVISORY   assert-density + mutation testing — DEFERRED

HONEST FRAMING (§5/§6): the dark CI pipeline that would prove the coverage /
code-review / e2e / standalone-build legs against the dark fork's code does not
exist yet. This command resolves what it can today (provenance, small-diff,
single-concern, dossier, perf-glob, and the composed security gate) and accepts
evidence for the CI-supplied legs via flags. A leg with neither a resolver nor
supplied evidence stays PENDING, which blocks eligibility (fail-closed on the
trickle candidate) until proven.

Exit 0 = eligible (or blocked-but-advisory, the default rollout); exit 3 =
blocked AND --enforce; exit 1 = a real error. --json for machine output. With
--write, the structured checklist is stamped onto the MR bead's trickle_checklist
field so the beads-viewer renders it.`,
	Args: cobra.ExactArgs(1),
	RunE: runMRTrickleGate,
}

func init() {
	mrTrickleGateCmd.Flags().BoolVar(&mrTrickleEnforce, "enforce", false,
		"Exit 3 when the MR is not trickle-eligible (default: advisory — report but exit 0)")
	mrTrickleGateCmd.Flags().StringVar(&mrTrickleLockPath, "lock", "gardenio/locks/security-tools.lock",
		"Path to security-tools.lock for the composed security gate")
	mrTrickleGateCmd.Flags().BoolVar(&mrTrickleWrite, "write", false,
		"Stamp the structured checklist onto the MR bead's trickle_checklist field (beads-viewer render)")

	mrTrickleGateCmd.Flags().StringVar(&mrTrickleCoveragePass, "coverage-pass", "",
		"Supply the patch-coverage verdict (true|false); unset leaves coverage pending")
	mrTrickleGateCmd.Flags().BoolVar(&mrTrickleCoverageBypass, "coverage-bypass", false,
		"Record a coverage bypass (blocks trickle even if merged into dark)")
	mrTrickleGateCmd.Flags().StringVar(&mrTrickleE2E, "e2e", "",
		"Supply the e2e-summary verdict (true|false); unset leaves it unknown (does not block on its own)")
	mrTrickleGateCmd.Flags().StringVar(&mrTrickleCodeReview, "code-review", "",
		"Supply the /code-review clean verdict (true|false); unset leaves code-review pending")
	mrTrickleGateCmd.Flags().IntVar(&mrTrickleUnresolved, "unresolved-findings", 0,
		"Number of >=80-confidence /code-review findings still unresolved")
	mrTrickleGateCmd.Flags().StringVar(&mrTrickleStandalone, "standalone-build", "",
		"Supply the standalone-build-on-upstream-HEAD verdict (true|false); unset does not block (dark CI absent)")
	mrTrickleGateCmd.Flags().StringVar(&mrTrickleSingleConcern, "single-concern", "",
		"Override the single-concern verdict (true|false); unset infers it from the changed-file top-level components")
	mrTrickleGateCmd.Flags().StringVar(&mrTricklePerfGuard, "perf-guard", "",
		"Supply the deterministic call-count-guard verdict (true|false) for perf-sensitive MRs; unset leaves a triggered perf item pending (runs in CI / Test)")

	mrCmd.AddCommand(mrTrickleGateCmd)
}

// parseTristate maps a flag string to a *bool: "" => nil (unset), "true"/"1"/"yes"
// => &true, anything else => &false.
func parseTristate(s string) *bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return nil
	case "true", "1", "yes":
		b := true
		return &b
	default:
		b := false
		return &b
	}
}

func runMRTrickleGate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	// Resolve the MR URL + ref, and (best-effort) the linked bead — provenance
	// and the write-back both key on the bead.
	mrURL, err := resolveMRURL(cmd, args[0])
	if err != nil {
		return err
	}
	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return fmt.Errorf("could not parse MR URL: %s", mrURL)
	}
	client, err := newMRGitLabClient(baseURLFromGitLabURL(mrURL))
	if err != nil {
		return err
	}

	mr, err := client.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		return fmt.Errorf("fetching MR !%d: %w", ref.IID, err)
	}

	// The changed-files + diff-stats fetches feed the small-diff and perf legs.
	// They are best-effort: a per-leg GitLab failure (the /changes endpoint 500s
	// on some large MRs) leaves those legs pending rather than aborting the whole
	// aggregate gate — the sibling legs (provenance, dossier, security) still
	// evaluate. A pending HARD leg blocks under --enforce, which is the correct
	// fail-closed posture.
	files, filesErr := client.ListMergeRequestChangedFiles(ctx, ref.ProjectPath, ref.IID)
	if filesErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "trickle-gate: could not fetch changed files (small-diff/perf pending): %v\n", filesErr)
	}
	stats, statsErr := client.GetMergeRequestDiffStats(ctx, ref.ProjectPath, ref.IID)
	if statsErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "trickle-gate: could not fetch diff stats (small-diff LOC pending): %v\n", statsErr)
	}

	linkedBead, linkedBeadID := resolveTrickleLinkedBead(cmd, args[0], mrURL)

	// small-diff needs a reliable size read; if either changed-files or diff-stats
	// failed we cannot size the diff, so leave the leg pending.
	diffResolvable := filesErr == nil && statsErr == nil

	in := bridge.TrickleEligibilityInput{
		Provenance: resolveTrickleProvenance(ctx, mr.Description, linkedBead, linkedBeadID),
		SmallDiff:  resolveTrickleSmallDiff(mr.Description, files, stats, diffResolvable),
		Dossier:    bridge.ParseTrickleDossier(mr.Description),
		Coverage:   resolveTrickleCoverage(),
		CodeReview: resolveTrickleCodeReview(),
		Perf:       resolveTricklePerf(files, filesErr == nil),
	}
	in.Security, in.SecurityEvaluated = resolveTrickleSecurity(ctx, client, ref, mr, files)

	decision := bridge.EvaluateTrickleEligibility(in)

	if mrTrickleWrite && linkedBeadID != "" {
		if err := writeTrickleChecklist(ctx, linkedBeadID, decision); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "trickle-gate: failed to stamp trickle_checklist on %s: %v\n", linkedBeadID, err)
		}
	}

	if jsonOutput {
		printJSON(map[string]any{
			"mr_iid":     mr.IID,
			"bead_id":    linkedBeadID,
			"eligible":   decision.Eligible,
			"reason":     decision.Reason,
			"hard_pass":  decision.HardPass(),
			"hard_total": decision.HardTotal(),
			"checklist":  decision.Checklist,
		})
	} else {
		printTrickleDecision(cmd, mr.IID, decision)
	}

	if !decision.Eligible && mrTrickleEnforce {
		// Distinct non-zero exit (3) so a caller's shell branches on "blocked"
		// vs. a real error (1), matching gb mr relevance-gate / security-gate.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		os.Exit(3)
	}
	return nil
}

// printTrickleDecision renders the human-readable checklist.
func printTrickleDecision(cmd *cobra.Command, iid int, d bridge.TrickleEligibilityDecision) {
	if d.Eligible {
		cmd.Printf("ELIGIBLE — !%d clears the trickle bar (%d/%d HARD checks pass)\n",
			iid, d.HardPass(), d.HardTotal())
	} else {
		cmd.Printf("NOT ELIGIBLE — !%d (%d/%d HARD checks pass): %s\n",
			iid, d.HardPass(), d.HardTotal(), d.Reason)
	}
	for _, it := range d.Checklist {
		cmd.Printf("  %s %-8s %-16s %s\n", trickleGlyph(it), it.Kind, it.ID, it.Evidence)
	}
}

// trickleGlyph renders a per-item status glyph for the text output, matching the
// beads-viewer renderer's glyphs (✓ pass, ✗ fail, – n/a, ☐ pending).
func trickleGlyph(it bridge.GateItem) string {
	switch {
	case it.NA:
		return "–"
	case it.Pass == nil:
		return "☐"
	case *it.Pass:
		return "✓"
	default:
		return "✗"
	}
}

// resolveTrickleLinkedBead resolves the issue bead the MR is linked to (the one
// carrying mr_url). Best-effort: returns (nil, "") when no linked bead resolves
// — provenance then reports the linked-bead leg as missing.
func resolveTrickleLinkedBead(cmd *cobra.Command, arg, mrURL string) (*beadsapi.BeadDetail, string) {
	// If the arg was itself a gd- bead, prefer it directly.
	if strings.HasPrefix(arg, "gd-") || strings.HasPrefix(arg, "ka-") {
		if b, err := daemon.GetBead(cmd.Context(), arg); err == nil {
			return b, b.ID
		}
	}
	// Otherwise find the merge-request bead by URL, then its producing issue bead.
	mrBeadID, err := findMRBeadByURL(cmd, mrURL)
	if err != nil {
		return nil, ""
	}
	if task, err := producingTaskForMRBead(cmd.Context(), mrBeadID); err == nil && task != nil {
		return task, task.ID
	}
	// Fall back to the MR bead itself (it carries mr_url + provenance fields).
	if b, err := daemon.GetBead(cmd.Context(), mrBeadID); err == nil {
		return b, b.ID
	}
	return nil, ""
}

// resolveTrickleProvenance builds the provenance leg (§2.7): mr_url set on the
// linked bead, a linked issue bead, an Operator: trailer/operated_by, and a
// dark-MR ancestry anchor (description trailer OR a supersedes/related typed
// edge to a dark MR bead).
func resolveTrickleProvenance(ctx context.Context, description string, bead *beadsapi.BeadDetail, beadID string) bridge.TrickleProvenanceInput {
	p := bridge.TrickleProvenanceInput{Evaluated: true}
	p.LinkedIssueBeadSet = beadID != ""

	operatorTrailer := bridge.TrickleOperatorTrailer(description) != ""
	if bead != nil {
		if bead.Fields["mr_url"] != "" {
			p.MRURLSet = true
		}
		if bead.Fields["operated_by"] != "" {
			operatorTrailer = true
		}
	}
	p.OperatorTrailerSet = operatorTrailer

	// Dark-MR ancestry: a description anchor (Dark-MR:/Peel-Of:) OR a typed edge
	// (supersedes/related) to another bead — the edge kinds §3 item 2 specifies
	// for "links this MR to its dark-fork MR(s)". `produces` is deliberately
	// EXCLUDED: it is the ordinary task→merge-request-bead link that EVERY linked
	// MR carries, so counting it would make the dark-ancestry check always pass.
	p.DarkMRAncestrySet = bridge.TrickleDarkMRReferenced(description)
	if !p.DarkMRAncestrySet && beadID != "" {
		if deps, err := daemon.GetDependencies(ctx, beadID); err == nil {
			for _, d := range deps {
				switch d.Type {
				case "supersedes", "related":
					if d.DependsOnID != "" {
						p.DarkMRAncestrySet = true
					}
				}
			}
		}
	}

	var have []string
	if p.MRURLSet {
		have = append(have, "mr_url")
	}
	if p.LinkedIssueBeadSet {
		have = append(have, "linked bead "+beadID)
	}
	if p.OperatorTrailerSet {
		have = append(have, "operator")
	}
	if p.DarkMRAncestrySet {
		have = append(have, "dark-MR anchor")
	}
	p.Evidence = "provenance: " + strings.Join(have, " + ")
	return p
}

// resolveTrickleSmallDiff builds the small-diff leg (§2.5) from the changed files
// + diff stats, applying the counted-path exclusions and reading the escape-hatch
// trailers. Single-concern is inferred from the distinct top-level components the
// counted files touch, unless overridden by --single-concern.
func resolveTrickleSmallDiff(description string, files []string, stats *bridge.GitLabMRDiffStats, resolvable bool) bridge.TrickleSmallDiffInput {
	if !resolvable {
		// Could not read the diff — leave the leg pending (blocks under --enforce).
		return bridge.TrickleSmallDiffInput{Evaluated: false}
	}
	s := bridge.TrickleSmallDiffInput{Evaluated: true}
	s.FilesChanged = bridge.CountTrickleDiffFiles(files)
	if stats != nil {
		s.NetLOC = stats.LinesChanged()
	}
	s.SizeExceptionTrailer = bridge.TrickleSizeExceptionTrailer(description)
	s.MultiConcernAckTrailer = bridge.TrickleMultiConcernAckTrailer(description)

	if override := parseTristate(mrTrickleSingleConcern); override != nil {
		s.SingleConcern = *override
	} else {
		s.SingleConcern = inferSingleConcern(files)
	}
	if b := parseTristate(mrTrickleStandalone); b != nil {
		s.StandaloneBuildGreen = b
	}
	s.Evidence = fmt.Sprintf("diff: %d net LOC, %d counted file(s)", s.NetLOC, s.FilesChanged)
	return s
}

// inferSingleConcern reports whether all counted changed files share one
// top-level component (the first path segment, e.g. gardenio/kbeads/coop/docs).
// A heuristic proxy for "one logical concern" — an MR spanning multiple
// top-level trees is flagged multi-concern unless a Multi-Concern-Ack: trailer
// is present.
func inferSingleConcern(files []string) bool {
	seen := map[string]bool{}
	for _, f := range files {
		if strings.TrimSpace(f) == "" || !bridge.IsTrickleCountedPath(f) {
			continue
		}
		top := f
		if i := strings.IndexByte(f, '/'); i > 0 {
			top = f[:i]
		}
		seen[top] = true
	}
	return len(seen) <= 1
}

// resolveTrickleCoverage builds the coverage leg from the supplied flags. The
// dark coverage pipeline does not exist yet, so absent flags leave it pending.
func resolveTrickleCoverage() bridge.TrickleCoverageInput {
	pass := parseTristate(mrTrickleCoveragePass)
	if pass == nil && !mrTrickleCoverageBypass {
		return bridge.TrickleCoverageInput{Evaluated: false}
	}
	c := bridge.TrickleCoverageInput{
		Evaluated:     true,
		ResultPresent: true,
		Pass:          pass != nil && *pass,
		Bypass:        mrTrickleCoverageBypass,
		E2EGreen:      parseTristate(mrTrickleE2E),
	}
	return c
}

// resolveTrickleCodeReview builds the code-review leg from the supplied flags.
func resolveTrickleCodeReview() bridge.TrickleCodeReviewInput {
	cr := parseTristate(mrTrickleCodeReview)
	if cr == nil {
		return bridge.TrickleCodeReviewInput{Evaluated: false}
	}
	return bridge.TrickleCodeReviewInput{
		Evaluated:          true,
		Ran:                true,
		ArtifactAttached:   *cr,
		UnresolvedFindings: mrTrickleUnresolved,
	}
}

// resolveTricklePerf builds the perf leg (§2.4): glob-triggered on the viewer/
// store paths. When triggered, the call-count guard verdict is not resolvable
// from here (it runs in CI / Test), so the item is left triggered-but-pending
// unless the perf glob did not match (N/A).
func resolveTricklePerf(files []string, resolvable bool) bridge.TricklePerfInput {
	if !resolvable {
		// Could not read the changed files — cannot decide the glob trigger, so
		// leave the leg pending (blocks under --enforce) rather than falsely N/A.
		return bridge.TricklePerfInput{Evaluated: false}
	}
	touched, hits := bridge.PerfSensitivePathTouched(files)
	p := bridge.TricklePerfInput{Evaluated: true, PerfSensitivePathsTouched: touched}
	if !touched {
		return p
	}
	// Triggered: the deterministic call-count guard runs in CI / Test. We cannot
	// re-run it here, so the guard verdict is supplied via --perf-guard; unset
	// leaves it pending (blocks under --enforce until the CI leg feeds it) rather
	// than a false fail. Record the triggering files either way.
	p.CallCountGuardPass = parseTristate(mrTricklePerfGuard)
	p.Evidence = "perf-sensitive paths touched: " + strings.Join(hits, ", ")
	return p
}

// resolveTrickleSecurity composes the security gate (§2.3) by gathering the same
// inputs gb mr security-gate does: named status checks, the Security-Scan block,
// the lock pins, and the two-tier classification. Best-effort — a lock-read or
// status-fetch failure leaves the security leg pending rather than erroring the
// whole gate.
// changedFiles is the already-fetched changed-file list from the caller, reused
// for the two-tier classification so the gate does not re-hit GitLab's
// /changes endpoint (which the caller already paid for and which 500s on some
// large MRs).
func resolveTrickleSecurity(ctx context.Context, client *bridge.GitLabClient, ref *bridge.MRRef, mr *bridge.GitLabMR, changedFiles []string) (bridge.SecurityGateInput, bool) {
	content, err := os.ReadFile(mrTrickleLockPath) //nolint:gosec // operator-supplied lock path
	if err != nil {
		return bridge.SecurityGateInput{}, false
	}
	lockPins := bridge.ParseSecurityToolsLock(string(content))
	if len(lockPins) == 0 {
		return bridge.SecurityGateInput{}, false
	}

	statusChecks := map[string]string{}
	if mr.SHA != "" {
		if statuses, sErr := client.ListCommitStatusesByPath(ctx, ref.ProjectPath, mr.SHA); sErr == nil {
			for _, s := range statuses {
				statusChecks[s.Name] = s.Status
			}
		}
	}
	return bridge.SecurityGateInput{
		StatusChecks: statusChecks,
		ScanBlock:    bridge.ParseSecurityScanBlock(mr.Description),
		LockPins:     lockPins,
		Tier:         bridge.ClassifySecurityReviewEffort(changedFiles),
	}, true
}

// writeTrickleChecklist stamps the structured decision onto the MR bead's
// trickle_checklist field (the exact shape the beads-viewer renderer parses).
func writeTrickleChecklist(ctx context.Context, beadID string, d bridge.TrickleEligibilityDecision) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return daemon.UpdateBeadFields(ctx, beadID, map[string]string{"trickle_checklist": string(raw)})
}
