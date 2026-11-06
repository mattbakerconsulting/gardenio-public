package main

// advice_audit.go implements `gb advice audit` — the repeatable form of the
// one-off advice-cost audit that grounded the context-crisis plan
// (ka-0000005777 / epic ka-0000003733). It simulates delivery for each
// (project, role) archetype through the REAL matching pipeline
// (advice.AuditCatalog reuses MatchesSubscriptions, supersession collapse,
// and the hippo-mirror drop) and reports matched bead count + description
// bytes + approximate tokens, so Part-A cut progress is measurable and
// regressions are visible.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/advice"

	"github.com/spf13/cobra"
)

var adviceAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Audit per-archetype matched advice cost (count + bytes + ~tokens)",
	Long: `Audit the advice catalog: for each (project, role) archetype, simulate
delivery through the real matching pipeline and report how many advice beads
match and how many description bytes / approximate tokens they carry.

This measures the MATCHED catalog — everything MatchesSubscriptions selects
before the render budget clips it — which is the number the context-crisis
plan budgets against (≤18k tokens per archetype, ka-0000003733 Part A). The
rendered size (what actually hits an agent's context) is reported separately
by gb prime --size.

By default the archetype grid is the real fleet: every (project, role) pair
carried by an agent bead spawned in the recent window. Narrow it with
--project/--role, or audit one exact archetype (even a not-yet-run one) with
both flags.

The BUDGET column is advisory by default (it always exits 0); add --strict to
turn it into a gate that exits non-zero when any archetype exceeds
--budget-tokens, so a scheduled check or make lint can enforce the ceiling at
runtime — the live-corpus analog of the frozen-snapshot real-catalog budget
test.

--by-bead re-projects the same audit into a per-bead, FLEET-AMPLIFIED cost
ranking: each matched bead once, weighted by REACH (how many archetypes match
it), sorted by fleet-tokens = bytes × reach ÷ 4. This answers "which bead do I
cut first for the biggest fleet-wide saving" — the lever the per-archetype grid
and --top N cannot show, because a global bead reaching every archetype looks
the same per-copy size as a role bead reaching one, yet costs N× more in
aggregate. Narrowing with --project/--role scopes REACH to that subset.

Examples:
  gb advice audit                            # full grid
  gb advice audit --project gardenio          # all roles for one project
  gb advice audit --project gardenio --role gardener --top 10
  gb advice audit --json                     # machine-readable, all items
  gb advice audit --project gardenio --strict # exit non-zero if any over budget
  gb advice audit --by-bead --top 20         # 20 heaviest beads fleet-wide, cut these first`,
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		role, _ := cmd.Flags().GetString("role")
		top, _ := cmd.Flags().GetInt("top")
		budgetTokens, _ := cmd.Flags().GetInt("budget-tokens")
		strict, _ := cmd.Flags().GetBool("strict")
		byBead, _ := cmd.Flags().GetBool("by-bead")

		beads, err := advice.ListOpenAdvice(cmd.Context(), daemon)
		if err != nil {
			return fmt.Errorf("listing advice: %w", err)
		}

		// Derive the archetype grid from the REAL fleet (agent beads), not the
		// advice-corpus cross-product that manufactured phantom archetypes and
		// skipped the highest-volume real pairs (ka-0000002153).
		archetypes, err := advice.ListFleetArchetypes(cmd.Context(), daemon, time.Now())
		if err != nil {
			return fmt.Errorf("deriving fleet archetypes: %w", err)
		}
		if project != "" || role != "" {
			filtered := archetypes[:0]
			for _, a := range archetypes {
				if project != "" && a.Project != project {
					continue
				}
				if role != "" && a.Role != role {
					continue
				}
				filtered = append(filtered, a)
			}
			archetypes = filtered
			// Both flags name an exact archetype: audit it even if no agent bead
			// of that (project, role) has run yet (the useful case when checking a
			// new role before any agent spawns for it).
			if len(archetypes) == 0 && project != "" && role != "" {
				archetypes = []advice.Archetype{{Project: project, Role: role}}
			}
		}
		if len(archetypes) == 0 {
			return fmt.Errorf("no archetypes to audit (no agent beads in the last %s; use --project and --role to name one)", advice.FleetArchetypeWindow)
		}

		results := advice.AuditCatalog(beads, archetypes)

		// Worst-first: the archetype closest to (or furthest past) budget is
		// the one the operator needs to see at the top.
		sort.SliceStable(results, func(i, j int) bool {
			return results[i].Bytes > results[j].Bytes
		})

		// --by-bead re-projects the SAME per-archetype audit into a per-bead,
		// fleet-amplified cost ranking (bytes × archetype-reach), so the operator
		// sees which single bead cut saves the most tokens across the audited
		// fleet — the lever the per-archetype table and --top N (which ranks
		// beads within each archetype independently, repeating global beads once
		// per archetype) structurally cannot surface (ka-0000003599).
		if byBead {
			beadCosts := advice.AggregateByBead(results)
			if jsonOutput {
				printJSON(beadCosts)
			} else {
				printAdviceAuditByBead(beadCosts, top, len(results))
			}
		} else if jsonOutput {
			printJSON(results)
		} else {
			printAdviceAudit(results, top, budgetTokens)
		}

		// --strict turns the BUDGET column into a gate: exit non-zero when any
		// audited archetype's matched pool exceeds the token budget, so a
		// scheduled check or `make lint` can enforce the ≤budget-tokens ceiling
		// (ka-0000003733 Part A) at runtime — the live-corpus analog of the
		// frozen-snapshot real_catalog_budget_test.go. Mirrors the --strict
		// convention of `gb advice lint --over-constraint` and `gb lint
		// trigger-prompts --over-constraint`. The table/JSON above IS the
		// diagnostic; the error only sets the exit code (ka-0000006559).
		if strict {
			over := countOverBudget(results, budgetTokens)
			if over > 0 {
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return fmt.Errorf("%d archetype(s) over the %dk-token matched budget", over, budgetTokens/1000)
			}
		}
		return nil
	},
}

// countOverBudget returns how many audited archetypes have a matched pool
// larger than budgetTokens. Shared by the --strict gate so it counts the same
// rows the BUDGET column flags OVER, whether output was table or JSON.
func countOverBudget(results []advice.ArchetypeAudit, budgetTokens int) int {
	over := 0
	for _, r := range results {
		if r.Tokens > budgetTokens {
			over++
		}
	}
	return over
}

// printAdviceAudit renders the audit summary table plus, when top > 0, a
// per-archetype list of the heaviest matched beads.
func printAdviceAudit(results []advice.ArchetypeAudit, top, budgetTokens int) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tROLE\tMATCHED\tBYTES\t~TOKENS\tGLOBAL\tPROJECT-WIDE\tROLE-SCOPED\tBUDGET")
	for _, r := range results {
		status := "ok"
		if r.Tokens > budgetTokens {
			status = fmt.Sprintf("OVER +%dk", (r.Tokens-budgetTokens)/1000)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%dk\t%s\t%s\t%s\t%s\n",
			r.Project, r.Role, r.Matched, r.Bytes, r.Tokens/1000,
			scopeCell(r.ByScope["global"]),
			scopeCell(r.ByScope["project"]),
			scopeCell(r.ByScope["role"]),
			status)
	}
	w.Flush()
	fmt.Printf("\n%d archetypes audited, %d over the %dk-token matched budget (ka-0000003733 Part A)\n",
		len(results), countOverBudget(results, budgetTokens), budgetTokens/1000)

	if top <= 0 {
		return
	}
	for _, r := range results {
		fmt.Printf("\n── %s/%s — top %d of %d matched beads by bytes ──\n", r.Project, r.Role, min(top, len(r.Items)), len(r.Items))
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for i, it := range r.Items {
			if i >= top {
				break
			}
			title := it.Title
			if len(title) > 60 {
				title = title[:57] + "..."
			}
			fmt.Fprintf(tw, "%s\t%s\t%d B\t%s\n", it.ID, it.Scope, it.Bytes, title)
		}
		tw.Flush()
	}
}

// printAdviceAuditByBead renders the fleet-amplified per-bead cost ranking:
// one row per matched advice bead, sorted by FleetTokens (bytes × reach)
// descending, so the heaviest fleet-wide cut target is first. archetypes is the
// audited population size, printed as the reach denominator so REACH reads as
// "N of M archetypes". When top > 0 the list is clipped to the top N rows.
func printAdviceAuditByBead(costs []advice.BeadCost, top, archetypes int) {
	shown := len(costs)
	if top > 0 && top < shown {
		shown = top
	}
	var totalFleetTokens int
	for _, c := range costs {
		totalFleetTokens += c.FleetTokens
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// AUTHORED-BY / AGE are provenance, not a score (task ka-0000007527). This
	// table is a cut list, and every row is a candidate for deletion ranked by
	// what it costs to deliver. Naming the origin and the age does not change
	// that ranking — it just means the operator deleting the top row can see
	// that it is a practice somebody wrote on a particular day and the fleet
	// has kept delivering ever since, rather than an anonymous byte count.
	// There is deliberately no per-author aggregate anywhere in this output.
	fmt.Fprintln(w, "BEAD\tSCOPE\tBYTES\tREACH\t~FLEET-TOKENS\tAUTHORED BY\tAGE\tTITLE")
	for i, c := range costs {
		if i >= shown {
			break
		}
		title := c.Title
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d/%d\t%s\t%s\t%s\t%s\n",
			c.ID, c.Scope, c.Bytes, c.Reach, archetypes, humanKTokens(c.FleetBytes),
			shortActor(c.CreatedBy), adviceAgeCell(c.CreatedAt, time.Now()), title)
	}
	w.Flush()
	fmt.Printf("\n%d matched beads across %d archetypes; ~%s fleet-tokens total (Σ bytes×reach ÷ 4). "+
		"Cut the top rows first — a global bead's fleet cost is its size × every archetype it reaches (ka-0000003733 Part A).\n",
		len(costs), archetypes, humanKTokens(totalFleetTokens*advice.TokenBytesRatio))
	fmt.Printf("REACH is a DELIVERY count, not an impact measure: nothing here observes whether an injected practice changed behavior.\n")
}

// shortActor renders an actor id compactly for a table cell. Agent names run
// long (project-task-suffix), and the useful part for provenance is the head of
// the name plus the fact that it is an agent rather than a human.
func shortActor(actor string) string {
	a := strings.TrimSpace(actor)
	if a == "" {
		return "-"
	}
	const maxActorCell = 28
	if len(a) > maxActorCell {
		return a[:maxActorCell-1] + "…"
	}
	return a
}

// adviceAgeCell renders how long a practice has been in the catalog, in whole
// days. Age is the second half of provenance: a rule written yesterday by one
// agent and one that has survived in the catalog for months are different
// objects, and neither the byte count nor the reach count distinguishes them.
func adviceAgeCell(created, now time.Time) string {
	if created.IsZero() {
		return "-"
	}
	d := now.Sub(created)
	if d < 24*time.Hour {
		return "today"
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// scopeCell renders one by-scope column as "N beads/Mk" or "-" when empty.
func scopeCell(s advice.ScopeStat) string {
	if s.Beads == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%s", s.Beads, humanKTokens(s.Bytes))
}

// humanKTokens renders a byte count as approximate k-tokens ("12k", "<1k").
func humanKTokens(bytes int) string {
	tok := bytes / advice.TokenBytesRatio
	if tok < 1000 {
		return "<1k"
	}
	return fmt.Sprintf("%dk", tok/1000)
}

func init() {
	adviceCmd.AddCommand(adviceAuditCmd)
	adviceAuditCmd.Flags().String("project", "", "audit only this project's archetypes")
	adviceAuditCmd.Flags().String("role", "", "audit only this role's archetypes")
	adviceAuditCmd.Flags().Int("top", 0, "also list the top-N heaviest matched beads per archetype (or, with --by-bead, clip the per-bead ranking to N rows)")
	adviceAuditCmd.Flags().Int("budget-tokens", 18000, "matched-token budget per archetype for the BUDGET column")
	adviceAuditCmd.Flags().Bool("strict", false, "exit non-zero when any archetype's matched pool exceeds --budget-tokens (turns the BUDGET column into a gate)")
	adviceAuditCmd.Flags().Bool("by-bead", false, "rank matched beads by fleet-amplified cost (bytes × archetype-reach) instead of the per-archetype grid — which bead to cut first for maximum fleet-wide savings")
}
