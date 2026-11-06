package main

// lint_ready_targets.go implements `gb lint ready-targets` — a queue-hygiene lint
// that flags `gb ready` rows whose every referenced target bead is already
// closed. It is the detector for the residue class in bug ka-0000006198: a task
// whose stated ACTION names another bead ("close trigger gd-…", "reword advice
// gd-…", "finalize MR (gd-…)") stays status=open forever after that target
// closes, and keeps rendering as claimable work that no other gate catches.
//
// Why this exists (ka-0000006198):
//   `gb ready` today has no target-liveness check anywhere. A row that names
//   another bead in its title/description has no edge or lint asserting that bead
//   is still open, so when the target closes the row silently becomes moot but
//   individually plausible — legible title, sane priority, real parent — so
//   triage never rejects it and an agent only discovers the mootness after
//   claiming and investigating. Bug ka-0000003967 (MR !7601) fixed ONE instance
//   (10 `tune:` rows) inside the meta-gardener's scoring prompt; that fix is narrow
//   to that one lane. This lint covers the whole class across every lane by
//   cross-checking the frozen row text against the LIVE status of each bead it
//   names.
//
// The check is deliberately conservative and REPORT-ONLY:
//   - ENABLEMENT / CONTEXT refs are excluded up front (bug ka-0000001923). A ref
//     the row names as "follow-up to ka-X", "deferred from ka-X", "once ka-X
//     ships", "blocked on ka-X", or via a typed-mention prefix (`parent:`,
//     `related:`) is an enablement or backstory signal — that bead CLOSING is
//     exactly what makes the row actionable, or is mere context — so its closed
//     status is NOT read as mootness. This is the dominant false-positive class
//     the original heuristic inverted (measured 90% false-positive on a 10-row
//     sample), where a row's enablement signal was mistaken for its obsolescence
//     signal.
//   - After that exclusion, a row is flagged only when it still carries at least
//     one ACTION-target ref AND *every* such ref resolves to a real bead AND
//     *every* one of them is closed. A row that references a still-open action
//     target, one we cannot resolve (typo / deleted / out of reach), or whose
//     only refs were enablement/context, is NOT flagged.
//   - Even a flagged row is a WEAK signal, not a close instruction: the output is
//     a READING list, never a worklist. A closed action-target only makes the
//     row *plausibly* moot; its own acceptance text can still be unmet. It never
//     mutates or auto-closes anything — the finding is a hint for a human/gardener
//     to read and verify. The command exits 0 by default; pass --strict to make
//     findings gate CI.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// kdRefRe matches a bead id reference — `gd-` followed by exactly 10
// alphanumeric characters, the canonical bead-id shape. It is the same pattern
// the bug's reproduction uses to pull refs out of a ready row's title and
// description.
var kdRefRe = regexp.MustCompile(`(?:gd|ka)-[A-Za-z0-9]{10}`)

// enablementCtxRe matches the language that, when it sits just BEFORE a bead
// reference, means that reference is an ENABLEMENT or CONTEXT signal rather than
// an action target — i.e. the referenced bead CLOSING is exactly the event that
// makes the row actionable (or merely provides backstory), not evidence the row
// is moot. This is the dominant false-positive class measured in bug
// ka-0000001923: a 90% false-positive rate on a 10-row sample, where rows like
// "follow-up to ka-X", "deferred from ka-X", "once ka-X ships", "blocked on
// ka-X", and typed-mention prefixes (`parent: ka-X`, `related: ka-X`) were
// flagged as moot precisely because their target had closed.
//
// It intentionally covers three sub-classes the bug names:
//   - enablement: follow-up to / deferred from / once … ships / blocked on /
//     after / unblocked by / spawned by / depends on / when … lands
//   - incident/backstory context: revealed by / surfaced by / discovered in /
//     from incident / per
//   - structural typed mentions: a `<key>:` prefix (parent, related, blocks,
//     depends-on, follow-up-to, deferred-from, discovered-by, parent-child, …)
//
// A `superseded by ka-X` / `obsoleted by ka-X` ref is deliberately NOT here: a
// closed superseding bead IS a genuine mootness signal, so leaving it out keeps
// that (rare, correct) positive intact.
var enablementCtxRe = regexp.MustCompile(`(?i)\b(?:` +
	`follow[- ]?up(?:s)?(?: to)?|` +
	`defer(?:red|s)?(?: from| by)?|` +
	`once|after|when|until|pending|awaiting|` +
	`blocked (?:on|by)|block(?:ed|s)?[- ]by|` +
	`unblock(?:ed|s|ed by)?|enabled by|` +
	`depends?(?: on)?|depends-on|dependent on|` +
	`spawn(?:ed)?(?: by| from)|` +
	`reveal(?:ed)?(?: by| in)|surfac(?:ed|es)?(?: by| in)|` +
	`discover(?:ed)?(?: by| in)|` +
	`split(?: off)?(?: from)|carved(?: out)?(?: from)|extracted from|` +
	`child of|sub-?task of|part of|` +
	`per|from incident|incident|` +
	`related|parent|blocks|blocked-by|parent-child` +
	`)[:\s(]*$`)

// refIsEnablementOrContext reports whether the given bead reference appears in
// text as an enablement or context reference rather than an action target. It
// inspects the run of text immediately preceding each occurrence of ref and
// tests that window against enablementCtxRe. A single enablement/context
// occurrence is enough to return true — the whole point is that the closed
// status of such a ref is expected, so it must not be read as a mootness signal.
func refIsEnablementOrContext(text, ref string) bool {
	// A window of 48 chars before the ref is enough to catch "deferred from ",
	// "once ", "follow-up to ", and typed-mention prefixes while staying local
	// enough not to be rescued by unrelated prose elsewhere in the row.
	const window = 48
	for start := 0; ; {
		i := strings.Index(text[start:], ref)
		if i < 0 {
			break
		}
		pos := start + i
		lo := pos - window
		if lo < 0 {
			lo = 0
		}
		if enablementCtxRe.MatchString(text[lo:pos]) {
			return true
		}
		start = pos + len(ref)
	}
	return false
}

// ReadyTargetFinding is one flagged ready row: an open, claimable bead whose
// every referenced target bead is already closed, so the row's stated work is
// almost certainly moot.
type ReadyTargetFinding struct {
	ID            string   `json:"id"`             // the ready (open) bead id
	Title         string   `json:"title"`          // its title
	ClosedTargets []string `json:"closed_targets"` // referenced bead ids, all closed
	Message       string   `json:"message"`        // human-readable explanation
}

// extractKDRefs returns the unique bead-id references found in text, excluding
// selfID (a row referencing its own id is not a target). The result is sorted so
// findings are stable across runs.
func extractKDRefs(text, selfID string) []string {
	matches := kdRefRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var refs []string
	for _, m := range matches {
		if m == selfID || seen[m] {
			continue
		}
		seen[m] = true
		refs = append(refs, m)
	}
	sort.Strings(refs)
	return refs
}

// lintReadyTargets scans the given ready beads and returns a finding for every
// row whose every referenced bead is present in resolved AND closed. resolved
// maps a referenced bead id to its detail; an id absent from the map is treated
// as unresolvable (unknown state), which makes the whole row abstain rather than
// flag — the conservative posture bug ka-0000006198 requires for a report-only
// heuristic. Findings are sorted by ready-bead id for stable output.
func lintReadyTargets(beads []*beadsapi.BeadDetail, resolved map[string]*beadsapi.BeadDetail) []ReadyTargetFinding {
	var findings []ReadyTargetFinding
	for _, b := range beads {
		if b == nil {
			continue
		}
		text := b.Title + "\n" + b.Description
		refs := extractKDRefs(text, b.ID)
		if len(refs) == 0 {
			continue
		}
		// Drop enablement/context refs BEFORE the all-closed test. A ref that the
		// row names as "follow-up to ka-X" / "deferred from ka-X" / "once ka-X
		// ships" / "blocked on ka-X" / a `parent:`/`related:` typed mention is an
		// ENABLEMENT or backstory signal: that bead closing is exactly what makes
		// the row actionable (or is mere context), not evidence the row is moot.
		// Reading such a closed ref as mootness is the 90% false-positive class in
		// bug ka-0000001923 — so an enablement ref neither flags a row nor, by
		// itself, keeps a row alive: it is simply not a mootness-bearing target.
		var actionRefs []string
		for _, ref := range refs {
			if refIsEnablementOrContext(text, ref) {
				continue
			}
			actionRefs = append(actionRefs, ref)
		}
		// If every reference was an enablement/context ref, the row carries no
		// mootness-bearing target at all — abstain rather than flag.
		if len(actionRefs) == 0 {
			continue
		}
		allClosed := true
		for _, ref := range actionRefs {
			t, ok := resolved[ref]
			if !ok || !isClosedStatus(t.Status) {
				// A single open or unresolvable target is enough to abstain: the
				// row may legitimately reference a closed bead as context, so we
				// only flag when EVERY named action target is known-closed.
				allClosed = false
				break
			}
		}
		if !allClosed {
			continue
		}
		refs = actionRefs
		msg := fmt.Sprintf("every action-target ref is closed (%s) — WEAK mootness signal; read the row's own acceptance text before acting, do not bulk-close (enablement/context refs already excluded)", strings.Join(refs, ", "))
		findings = append(findings, ReadyTargetFinding{
			ID:            b.ID,
			Title:         b.Title,
			ClosedTargets: refs,
			Message:       msg,
		})
	}
	sort.SliceStable(findings, func(i, j int) bool {
		return findings[i].ID < findings[j].ID
	})
	return findings
}

// isClosedStatus reports whether a bead status counts as closed for the
// target-liveness check. Only the terminal `closed` status makes a referenced
// target moot; open / in_progress / deferred are all still live work.
func isClosedStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "closed")
}

// --- CLI wiring ---

var lintReadyTargetsCmd = &cobra.Command{
	Use:   "ready-targets",
	Short: "Flag gb ready rows whose every referenced bead is already closed (likely-moot claimable work)",
	Long: `Scan the ` + "`gb ready`" + ` set and flag every row whose title/description names
one or more other beads by id where ALL of the named beads are already closed.

Such a row is the residue queue-noise class from bug ka-0000006198: a task whose
stated action targets another bead — "close trigger gd-…", "reword advice gd-…",
"finalize MR gd-…" — stays status=open forever after that target closes, keeps
rendering as claimable work, and is caught by no other gate — cron-exhaust,
zombie, and orphan sweeps all pass it. It reads fine on page 1, so triage never
rejects it; an agent only discovers the mootness after claiming.

ENABLEMENT and CONTEXT refs are excluded first: a ref the row names as
"follow-up to ka-X", "deferred from ka-X", "once ka-X ships", "blocked on ka-X",
or via a typed-mention prefix (parent:/related:) is a signal that the target
CLOSING is what makes the row actionable — not evidence the row is moot. The
original heuristic inverted this and ran ~90% false-positive on a measured
sample (bug ka-0000001923).

After that exclusion, a row is flagged ONLY when it still carries at least one
ACTION-target ref AND every such ref resolves AND every one of them is closed. A
row that names a still-open action target, one that cannot be resolved (typo /
deleted / out of scope), or whose only refs were enablement/context, is not
flagged.

Even a flagged row is a WEAK signal: the output is a READING list, not a close
list. A closed action-target makes the row only *plausibly* moot — its own
acceptance text may still be unmet — so read each before acting and never
bulk-close.

By default the scan is scoped to the CLAIMABLE LEAF view a work-seeking agent
sees (category=intermediate, no epic/molecule containers) — the rows that cause
the actual harm, where an agent claims a moot leaf task and only then discovers
the target closed. Containers legitimately reference their own closed children,
so pass --all only when you want the full ready set (it is noisier).

This is a HEURISTIC and is report-only: it never closes or mutates anything.
Exits 0 by default; pass --strict to exit non-zero when any row is flagged so it
can gate CI.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		strict, _ := cmd.Flags().GetBool("strict")

		beads, err := listReadyTargetBeads(cmd)
		if err != nil {
			return err
		}

		// Collect every bead id referenced by any ready row, then resolve them all
		// in one batched round-trip rather than an N+1 kd show per row.
		refSet := map[string]bool{}
		for _, b := range beads {
			for _, ref := range extractKDRefs(b.Title+"\n"+b.Description, b.ID) {
				refSet[ref] = true
			}
		}
		resolved := map[string]*beadsapi.BeadDetail{}
		if len(refSet) > 0 {
			ids := make([]string, 0, len(refSet))
			for id := range refSet {
				ids = append(ids, id)
			}
			resolved, err = daemon.BatchGetBeads(cmd.Context(), ids)
			if err != nil {
				return fmt.Errorf("resolving referenced targets: %w", err)
			}
		}

		findings := lintReadyTargets(beads, resolved)

		if jsonOutput {
			printJSON(findings)
		} else {
			printReadyTargetsLint(findings, len(beads))
		}

		if strict && len(findings) > 0 {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			return fmt.Errorf("%d ready-target finding(s)", len(findings))
		}
		return nil
	},
}

// listReadyTargetBeads fetches the ready set the lint scans. By default it mirrors
// the CLAIMABLE LEAF view a work-seeking agent actually sees (category=intermediate,
// no epic/molecule containers) — the exact rows that cause the harm in bug
// ka-0000006198, where an agent claims a moot leaf task and only then discovers the
// target closed. Container rows (epics/molecules) legitimately reference their own
// closed children, so scanning them by default would drown the signal in
// structural noise (measured: 143 flagged rows under a full-set scan vs. the small
// claimable residue). Pass --all to scan the entire ready set including containers.
// It fails closed on a truncated page: a partial fetch could silently skip a moot
// row and report a false all-clear.
func listReadyTargetBeads(cmd *cobra.Command) ([]*beadsapi.BeadDetail, error) {
	project, _ := cmd.Flags().GetString("project")
	allProjects, _ := cmd.Flags().GetBool("all-projects")
	limit, _ := cmd.Flags().GetInt("limit")
	all, _ := cmd.Flags().GetBool("all")

	opts := readyOptions{
		Project:     project,
		AllProjects: allProjects,
		Limit:       limit,
		// Mirror the claimable leaf view an agent sees. AgentContext gates the
		// leaf default in buildReadyQuery; force it on here so the lint applies the
		// same category=intermediate + container-exclusion scope regardless of
		// whether it runs inside an agent pod or a bare CI shell. --all opts into
		// the full ready set (containers included).
		AgentContext: !all,
		All:          all,
	}
	q := buildReadyQuery(opts)
	res, err := daemon.ListBeadsFiltered(cmd.Context(), q)
	if err != nil {
		return nil, fmt.Errorf("listing ready beads: %w", err)
	}
	beads := res.Beads
	// The exclusive-project narrowing gb ready uses for the un-anchored project
	// view is now applied SERVER-SIDE by buildReadyQuery (ExclusiveProject), so a
	// multi-project row is never returned under a project it does not exclusively
	// belong to and res.Total is exact for the truncation guard below
	// (ka-0000003411).
	if res.Total > len(res.Beads) {
		return nil, fmt.Errorf("ready listing truncated (%d of %d returned); rerun with a larger --limit", len(res.Beads), res.Total)
	}
	return beads, nil
}

func printReadyTargetsLint(findings []ReadyTargetFinding, scanned int) {
	if len(findings) == 0 {
		fmt.Printf("No ready rows with an all-closed target set across %d ready bead(s).\n", scanned)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tCLOSED TARGETS")
	for _, f := range findings {
		title := f.Title
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.ID, title, strings.Join(f.ClosedTargets, ","))
	}
	w.Flush()
	// This headline is deliberately a READING list, not a close list (bug
	// ka-0000001923): a measured 10-row sample was 90% LIVE even after the
	// enablement/context refs are suppressed above, because a closed action-target
	// is only a WEAK mootness signal — the row's own acceptance text can still be
	// unmet. Never phrase the summary as an actionable worklist; a wrongly-closed
	// ready row is invisible afterward, a far worse outcome than a stale-looking
	// one that survives.
	fmt.Printf("\n%d ready row(s) whose action-target refs are all closed across %d ready bead(s)\n", len(findings), scanned)
	fmt.Println("(WEAK signal — a READING list, not a close list: the closed ref may be stale context and the row's own work may still be LIVE. Read each; never bulk-close.)")
}

func init() {
	lintCmd.AddCommand(lintReadyTargetsCmd)
	lintReadyTargetsCmd.Flags().String("project", defaultProject(), "filter by project label (default: $KD_PROJECT or $BOAT_PROJECT)")
	lintReadyTargetsCmd.Flags().Bool("all-projects", false, "scan ready beads from all projects (disables project filter)")
	lintReadyTargetsCmd.Flags().Bool("all", false, "scan the FULL ready set including epics/molecules (default: only the claimable leaf view an agent sees). Containers legitimately reference closed children, so --all is noisier.")
	lintReadyTargetsCmd.Flags().Int("limit", readyJSONFullLimit, "maximum ready beads to scan (default: the full ready set)")
	lintReadyTargetsCmd.Flags().Bool("strict", false, "exit non-zero when any ready row is flagged (for CI gating); default is report-only (exit 0)")
}
