package main

// advice_lint_over_constraint.go wires the `--over-constraint` mode of
// `gb advice lint` — the QUALITATIVE advice-hygiene lint (task ka-0000004072,
// P4 of the Opus/Fable-5 context-audit epic ka-0000006604). Where the default
// `gb advice lint` mode (advice_lint.go) runs the TARGETING-hygiene checks
// (denied broadcasts, dead targeting, junk labels), --over-constraint runs the
// three over-constraint checks in internal/advice/overconstraint.go:
//
//   over-constraint:rules-over-judgment  (warn)  no-discretion absolutes that
//                                                replace judgment with a rigid rule
//   over-constraint:directive-density    (warn)  too many emphatic imperatives
//                                                (MUST/NEVER/DO NOT/…) per bead length
//   over-constraint:redundancy           (warn)  restates the injected workflow-
//                                                context every agent already reads
//
// The redundancy check needs the injected "system prompt" — the config beads
// EnsureConfigs seeds (claude-instructions + nudge-prompts). We fetch those from
// the same daemon the corpus comes from and concatenate their descriptions into
// the reference corpus, so the check compares each advice bead against the exact
// text already in every session. On a fetch failure we degrade to skipping the
// redundancy check (the other two still run), mirroring the fail-open posture of
// the dead-role-target check.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// refCorpusCategories are the config-bead categories whose injected text forms
// the "system prompt" the redundancy check compares advice against: the
// per-role/global claude-instructions (the workflow-context body) and the
// nudge-prompts. These are exactly what gb prime renders into every session.
var refCorpusCategories = []string{"claude-instructions", "nudge-prompts"}

// buildRefCorpus fetches the injected config beads and concatenates their
// descriptions into one reference-corpus string for the redundancy check. It
// pulls EVERY config bead of the target categories regardless of role scope
// (the union across archetypes) so an advice bead that duplicates any role's
// instructions is caught. Returns "" on a fetch error or when no config beads
// exist, which advice.LintOverConstraint treats as "skip the redundancy check".
//
// The fetch is PAGED (ListAllBeadsFiltered), not a single Limit:500: a truncated
// config page would silently drop a role's claude-instructions/nudge-prompts
// from the reference corpus and cause the redundancy check to MISS genuine
// duplicates (the config half of the F10 silent-truncation class, ka-0000005760).
func buildRefCorpus(ctx context.Context, lister interface {
	ListAllBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}) string {
	result, err := lister.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
	})
	if err != nil || result == nil {
		return "" // fail-open: skip the redundancy check
	}
	want := make(map[string]bool, len(refCorpusCategories))
	for _, c := range refCorpusCategories {
		want[c] = true
	}
	var b strings.Builder
	for _, bead := range result.Beads {
		if bead == nil || !want[bead.Title] {
			continue
		}
		b.WriteString(bead.Description)
		b.WriteByte('\n')
	}
	return b.String()
}

// runAdviceLintOverConstraint executes the over-constraint lint mode: fetch the
// corpus + reference config, run the three checks, print findings, and set a
// non-zero exit only when --strict is passed (all findings are WARN by design,
// so the default is advisory — it reports without gating).
func runAdviceLintOverConstraint(cmd *cobra.Command, strict bool) error {
	beads, err := advice.ListOpenAdvice(cmd.Context(), daemon)
	if err != nil {
		return fmt.Errorf("listing advice: %w", err)
	}

	refCorpus := buildRefCorpus(cmd.Context(), daemon)
	findings := advice.LintOverConstraint(beads, refCorpus, advice.OverConstraintConfig{})
	advice.SortOverConstraintFindings(findings)

	if jsonOutput {
		printJSON(findings)
	} else {
		printOverConstraintLint(findings, len(beads), refCorpus != "")
	}

	if strict && len(findings) > 0 {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return fmt.Errorf("%d over-constraint finding(s)", len(findings))
	}
	return nil
}

// printOverConstraintLint renders the over-constraint findings as a table.
func printOverConstraintLint(findings []advice.OverConstraintFinding, corpus int, refUsed bool) {
	redundancyNote := ""
	if !refUsed {
		redundancyNote = " (redundancy check skipped — no config beads reachable)"
	}
	if len(findings) == 0 {
		fmt.Printf("No over-constraint smells in %d open advice beads%s.\n", corpus, redundancyNote)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSEVERITY\tCHECK\tLINE\tRULE\tDETAIL\tSNIPPET")
	for _, f := range findings {
		detail := overConstraintDetail(f)
		snippet := f.Snippet
		if len(snippet) > 60 {
			snippet = snippet[:57] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			f.ID, f.Severity, strings.TrimPrefix(f.Check, "over-constraint:"),
			f.Line, f.Rule, detail, snippet)
	}
	w.Flush()
	fmt.Printf("\n%d over-constraint finding(s) in %d open advice beads%s\n",
		len(findings), corpus, redundancyNote)
	fmt.Println("(all WARN — smells to review, not delivery breakage; see the P4 context-audit epic ka-0000006604)")
}

// overConstraintDetail renders the per-check numeric detail column.
func overConstraintDetail(f advice.OverConstraintFinding) string {
	switch f.Check {
	case advice.OverConstraintDirectiveDensity:
		return fmt.Sprintf("%d dir / %.2f per-1k", f.DirectiveCount, f.DirectiveDensity)
	case advice.OverConstraintRedundancy:
		return fmt.Sprintf("%.0f%% overlap", f.DirectiveDensity)
	default:
		return ""
	}
}
