package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var newsCmd = &cobra.Command{
	Use:   "news",
	Short: "Show in-progress work by others",
	Long: `Show what other agents are actively working on. Use this before starting
work to avoid conflicts and get situational awareness.

Shows:
- Operator-action block (ALWAYS FIRST, un-suppressible): open P0/P1 escalations
  and open decisions awaiting an operator — the pivotal facts every caller needs.
  It leads every output mode (incl. --json / --active-only / --stale-only) and no
  flag hides it; only the per-section --limit cap bounds it, and a truncation
  marker names how many rows are below the fold. --no-decisions is a deprecated
  no-op.
- Relevant to your work: when you hold a claimed bead, a caller-scoped digest
  follows the operator-action block — new comments on your bead, your blockers
  (including ones that just cleared), other work in your epic subtree, work
  sharing a topical label with your bead, and possible-duplicate work in other
  projects. This is the section most likely to change what you do next; read it.
  Disable with --no-relevance.
- In-progress beads by other agents (potential conflicts)
- Recently closed beads (context on recent progress)
- Recently resolved decisions (what was decided and by whom)
- Recent artifacts/reports (deliverables submitted for decisions)

Per-run gardener/cron beads ("Multi[cron]: ..." / "Schedule: ..." run instances)
are hidden by default — they are operational exhaust, not teammate work. Pass
--include-cron-runs to show them.

Issues by the current actor are excluded unless --all is specified.

--json emits a JSON OBJECT envelope, not a bare array, so the flat-array
"jq '.[]'" idiom (correct for kd list / gb ready / kd search) hard-errors here.
Iterate a named array instead: gb news --json | jq '.stale[]'. Keys: agents,
in_progress, stale, unassigned, closed, decisions, open_decisions,
open_escalations, artifacts, window, plus relevant (an object, present only when
you hold a claim). open_escalations carries the open P0/P1 escalations (id,
title, priority, operation_type) — always present, never gated by the filter
flags. Each
agents[] row carries role, status, pod_phase and last_heartbeat (previously
null); prewarmed-pool placeholders and your own row are excluded by default
(--all re-includes self). Each stale[] row carries stale_class (claimable |
hard-claimed | respawned-successor-active | escalation-parked | awaits-release)
and successor (the live holder), so a
hard-claimed or successor-occupied bead is never mistaken for free work. For
scripting prefer --ids-only (optionally with --stale-only/--active-only); it
emits "ID<TAB>title" per line, so cut -f1 recovers the bare ID.`,
	GroupID: "orchestration",
	RunE:    runNews,
}

func init() {
	newsCmd.Flags().Bool("all", false, "include your own activity")
	newsCmd.Flags().String("window", "2h", "lookback window for recently closed")
	newsCmd.Flags().IntP("limit", "n", 50, "maximum beads per section")
	newsCmd.Flags().String("project", defaultProject(), "filter by project label (default: $KD_PROJECT or $BOAT_PROJECT)")
	newsCmd.Flags().Bool("all-projects", false, "show activity from all projects (disables project filter)")
	newsCmd.Flags().Bool("stale-only", false, "only show stale in-progress beads (assignee agent gone); omits other sections")
	newsCmd.Flags().Bool("active-only", false, "only show live in-progress beads (assignee agent still running); omits other sections")
	newsCmd.Flags().Bool("ids-only", false, "emit one bead per line as \"ID<TAB>title\" (useful with --stale-only/--active-only for scripting; cut -f1 for the bare ID)")
	newsCmd.Flags().Bool("no-decisions", false, "DEPRECATED / no-op: the operator-action block (open P0/P1 escalations + open decisions) leads the digest un-suppressibly (ka-0000002114); this flag no longer hides it")
	newsCmd.Flags().Bool("include-cron-runs", false, "include per-run gardener/cron beads (source:multi, source:schedule; default: hidden)")
	newsCmd.Flags().Bool("no-relevance", false, "suppress the caller-scoped \"Relevant to your work\" section (default on when you hold a claim)")
}

// newsRunInstanceLabels are the trigger-engine source labels stamped on
// per-cycle run-instance task beads (one per gardener firing, titled
// "[1:1:<ts>] Multi[cron]: ..." or "[<ts>] Schedule: ..."). They are
// operational exhaust, not teammate work: at a 24h window they made up over
// half of the "Closed" section, drowning real progress (ka-0000000723;
// rubric class NWS-4 CLOSED-NOISE / RDY-1 CRON-EXHAUST in
// docs/news-ready-relevance-rubric.md). Excluded server-side by default so
// the section limit fills with real work. Must stay in sync with
// statusreporter.runInstanceSourceLabels.
var newsRunInstanceLabels = []string{"source:schedule", "source:multi", "source:schedule-mirror"}

// newsExcludeLabels returns the server-side label exclusions for the
// in-progress and closed queries. Empty when the caller opts back in via
// --include-cron-runs.
func newsExcludeLabels(includeCronRuns bool) []string {
	if includeCronRuns {
		return nil
	}
	return newsRunInstanceLabels
}

func runNews(cmd *cobra.Command, args []string) error {
	showAll, _ := cmd.Flags().GetBool("all")
	windowStr, _ := cmd.Flags().GetString("window")
	limit, _ := cmd.Flags().GetInt("limit")
	project, _ := cmd.Flags().GetString("project")
	allProjects, _ := cmd.Flags().GetBool("all-projects")
	staleOnly, _ := cmd.Flags().GetBool("stale-only")
	activeOnly, _ := cmd.Flags().GetBool("active-only")
	idsOnly, _ := cmd.Flags().GetBool("ids-only")
	includeCronRuns, _ := cmd.Flags().GetBool("include-cron-runs")
	noRelevance, _ := cmd.Flags().GetBool("no-relevance")

	if staleOnly && activeOnly {
		return fmt.Errorf("--stale-only and --active-only are mutually exclusive")
	}

	window, err := time.ParseDuration(windowStr)
	if err != nil {
		return fmt.Errorf("invalid --window duration %q: %w", windowStr, err)
	}

	ctx := cmd.Context()

	var projectLabels []string
	// exclusiveProject drives the daemon's server-side exclusive_project filter:
	// each section query below excludes any bead carrying a `project:<other>`
	// label, so the SQL LIMIT accounts for the project narrowing and no row it
	// would drop is lost past the limit. This replaces the client-side
	// filterExclusiveProject pass (and its over-fetch multiplier) the six
	// sections used to run after the daemon had already truncated to Limit — the
	// honest fix for ka-0000003411 (the `gb news` residual of ka-0000004559).
	// projectLabels still ANDs `project:<name>` so a bead with NO project label
	// is excluded exactly as before (the client pass never added it either).
	var exclusiveProject string
	if !allProjects && project != "" {
		projectLabels = []string{"project:" + project}
		exclusiveProject = project
	}

	// The six core section fetches, the agent roster, and the caller-scoped
	// relevance digest are all INDEPENDENT read-only daemon round-trips. They
	// historically ran strictly sequentially, so `gb news` wall-clock was the SUM
	// of ~8 round-trips — measured 10-17s in prod (ka-0000006233), the single
	// largest per-invocation latency sink in the agent inner loop, paid at every
	// spawn (SessionStart advises `gb news`) and before every irreversible action.
	// Fan them out with an errgroup keyed off a derived context: the beadsapi
	// client wraps a stdlib *http.Client (safe for concurrent use) and every query
	// here is read-only and independent, so wall-clock collapses to the SLOWEST
	// single round-trip instead of their sum. The six core fetches propagate their
	// error verbatim (any one failing fails `gb news`, exactly as before); the
	// roster and relevance goroutines stay best-effort — they swallow their own
	// error and leave a nil result, preserving the prior degrade-don't-fail
	// behavior (roster fell back to empty, relevance to nil).
	var (
		ipResult      *beadsapi.ListBeadsResult
		closedResult  *beadsapi.ListBeadsResult
		decResult     *beadsapi.ListBeadsResult
		reportResult  *beadsapi.ListBeadsResult
		openDecResult *beadsapi.ListBeadsResult
		openEscResult *beadsapi.ListBeadsResult
		agentResult   *beadsapi.ListBeadsResult
		relevance     *relevanceDigest
	)

	fetchGroup, fetchCtx := errgroup.WithContext(ctx)

	fetchGroup.Go(func() (err error) {
		ipResult, err = daemon.ListBeadsFiltered(fetchCtx, beadsapi.ListBeadsQuery{
			Statuses:         []string{"in_progress"},
			Kinds:            []string{"issue"},
			Labels:           projectLabels,
			ExclusiveProject: exclusiveProject,
			ExcludeLabels:    newsExcludeLabels(includeCronRuns),
			Limit:            limit,
			// Mark in-progress rows PARKED when an open operator decision names
			// them in its prose (ka-0000003797) — the text-mention complement to
			// the requesting_task-field parking classifyStale already surfaces.
			// One indexed correlated subquery per row; cost scales with the page.
			WithParkedOnDecision: true,
		})
		if err != nil {
			return fmt.Errorf("fetching in-progress: %w", err)
		}
		return nil
	})

	fetchGroup.Go(func() (err error) {
		closedResult, err = daemon.ListBeadsFiltered(fetchCtx, beadsapi.ListBeadsQuery{
			Statuses:         []string{"closed"},
			Kinds:            []string{"issue"},
			Labels:           projectLabels,
			ExclusiveProject: exclusiveProject,
			ExcludeLabels:    newsExcludeLabels(includeCronRuns),
			Sort:             "-updated_at",
			Limit:            limit,
		})
		if err != nil {
			return fmt.Errorf("fetching recently closed: %w", err)
		}
		return nil
	})

	fetchGroup.Go(func() (err error) {
		decResult, err = daemon.ListBeadsFiltered(fetchCtx, beadsapi.ListBeadsQuery{
			Types:            []string{"decision"},
			Statuses:         []string{"closed"},
			Labels:           projectLabels,
			ExclusiveProject: exclusiveProject,
			Sort:             "-updated_at",
			Limit:            limit,
		})
		if err != nil {
			return fmt.Errorf("fetching decisions: %w", err)
		}
		return nil
	})

	fetchGroup.Go(func() (err error) {
		reportResult, err = daemon.ListBeadsFiltered(fetchCtx, beadsapi.ListBeadsQuery{
			Types:            []string{"report"},
			Statuses:         []string{"closed"},
			Labels:           projectLabels,
			ExclusiveProject: exclusiveProject,
			Sort:             "-updated_at",
			Limit:            limit,
		})
		if err != nil {
			return fmt.Errorf("fetching reports: %w", err)
		}
		return nil
	})

	// OPEN decisions awaiting operator — sorted OLDEST-first (created_at ASC) so
	// the per-section cap drops the NEWEST rows, never the starved oldest ones
	// (ka-0000005518, ka-0000002114). It leads the digest un-suppressibly in every
	// output mode; the only bound is the per-section --limit cap.
	fetchGroup.Go(func() (err error) {
		openDecResult, err = daemon.ListBeadsFiltered(fetchCtx, beadsapi.ListBeadsQuery{
			Types:            []string{"decision"},
			Statuses:         []string{"open"},
			Labels:           projectLabels,
			ExclusiveProject: exclusiveProject,
			Sort:             "created_at",
			Limit:            limit,
		})
		if err != nil {
			return fmt.Errorf("fetching open decisions: %w", err)
		}
		return nil
	})

	// OPEN high-priority escalations (P0/P1) — priority ASC then created_at ASC so
	// the oldest most-urgent asks lead and survive the cap (ka-0000002114).
	fetchGroup.Go(func() (err error) {
		openEscResult, err = daemon.ListBeadsFiltered(fetchCtx, beadsapi.ListBeadsQuery{
			Types:            []string{"escalation"},
			Statuses:         []string{"open"},
			Labels:           projectLabels,
			ExclusiveProject: exclusiveProject,
			Sort:             "priority,created_at",
			Limit:            limit,
		})
		if err != nil {
			return fmt.Errorf("fetching open escalations: %w", err)
		}
		return nil
	})

	// Agent roster (liveness detection + JSON agents[] + successor detection).
	// Best-effort: a fetch error degrades to an empty roster rather than failing
	// `gb news`, so swallow the error and leave agentResult nil. Sorted
	// freshest-first so a capped page keeps the most-recently-heartbeated agents
	// (ka-0000007979).
	fetchGroup.Go(func() error {
		res, err := daemon.ListBeadsFiltered(fetchCtx, beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"open"},
			Sort:     "-updated_at",
			Limit:    200,
		})
		if err == nil {
			agentResult = res
		}
		return nil
	})

	// Caller-scoped relevance digest (audit fixes M2/M3/M6, ka-0000006077): the
	// epic subtree, shared labels, blockers incl. recently-cleared ones, comments
	// on the caller's own bead, and cross-project duplicate matches. It needs only
	// actor/project/window, so it joins the parallel wave. Best-effort and nil
	// when the caller holds no claim; skipped for the scripting/triage
	// short-circuits (--ids-only / --stale-only / --active-only), which are for
	// gardeners and machine consumers, not the crew this targets.
	if !noRelevance && !idsOnly && !staleOnly && !activeOnly {
		fetchGroup.Go(func() error {
			relevance = gatherRelevance(fetchCtx, actor, project, newsRelevanceWindow)
			return nil
		})
	}

	if err := fetchGroup.Wait(); err != nil {
		return err
	}

	openDecisions := openDecResult.Beads
	openDecTotal := openDecResult.Total
	openEscalations := filterHighPriorityEscalations(openEscResult.Beads)

	ipBeads := ipResult.Beads
	closedBeads := filterRecentlyClosed(closedResult.Beads, window)
	decisions := filterRecentlyClosed(decResult.Beads, window)
	reports := filterRecentlyClosed(reportResult.Beads, window)
	// Project narrowing is now applied SERVER-SIDE via ExclusiveProject on each
	// section query above, so the daemon's LIMIT already accounts for it — there
	// is no post-Limit client-side exclusive-project pass to backfill, and
	// result.Total is exact for the project-exclusive match (ka-0000003411).
	if !showAll && actor != "" && actor != "unknown" {
		ipBeads = filterOutAssignee(ipBeads, actor)
		closedBeads = filterOutAssignee(closedBeads, actor)
	}

	// Cap each section to `limit` as a defensive ceiling after the remaining
	// client-side passes (recently-closed window, filter-out-assignee). Project
	// narrowing and LIMIT are now both server-side (ExclusiveProject above), so
	// the daemon already returns at most `limit` project-exclusive rows per
	// section and these passes only ever remove rows — capBeads is effectively a
	// no-op here, retained so a section can never render MORE than `limit` even
	// if a future caller widens a fetch. It stays a no-op when limit <= 0
	// (unlimited stays unlimited). See ka-0000003411.
	// Capture each section's IN-SCOPE length immediately BEFORE capBeads so the
	// --json path can report a per-section <section>_total/<section>_truncated
	// (ka-0000005034). Measuring here — after the window/project/self filter
	// chain but before the cap — makes "total" mean "rows the cap dropped from
	// what you'd otherwise see", never the raw daemon match count: the windowed
	// closed/decisions/artifacts queries have no server-side time bound, so their
	// daemon Total counts every closed bead ever and would flag truncated on
	// every call. The in_progress trio (in_progress/stale/unassigned) shares this
	// one capped query and is split only later once the roster exists, so retain
	// the pre-cap ipBeads slice and split it with the same activeAgents map below.
	ipBeadsPreCap := ipBeads
	closedPreCap := len(closedBeads)
	decisionsPreCap := len(decisions)
	reportsPreCap := len(reports)
	// Escalations are priority-filtered client-side (no server-side time bound),
	// so the pre-cap in-scope length is the correct "total" for the truncation
	// marker — the same measure decisions uses via openDecTotal.
	openEscTotal := len(openEscalations)

	ipBeads = capBeads(ipBeads, limit)
	closedBeads = capBeads(closedBeads, limit)
	decisions = capBeads(decisions, limit)
	reports = capBeads(reports, limit)
	openDecisions = capBeads(openDecisions, limit)
	openEscalations = capBeads(openEscalations, limit)

	// Build the agent roster used for liveness detection, the JSON agents array,
	// and respawned-successor detection. A single fetch feeds all three so they
	// never disagree (M4, ka-0000000465). Prewarmed-pool placeholder agents and
	// the caller's own row are excluded so the roster reflects real teammates
	// rather than pool scaffolding + self (the "3/4 rows were junk" finding).
	var roster []newsAgentInfo
	activeAgents := make(map[string]bool)
	agentSpawner := make(map[string]string) // lowercase agent name → spawned_by
	if agentResult != nil {
		roster = buildNewsAgents(agentResult.Beads, time.Now(), newsAliveFreshnessWindowValue(), actor)
		activeAgents, agentSpawner = rosterDerived(roster)
	}
	// Compute open-decision dup groups before the early-exit so the JSON
	// path always has a populated openDecGrouped slice (stable shape).
	openDecGrouped := groupOpenDecisions(openDecisions)

	hasContent := len(ipBeads) > 0 || len(closedBeads) > 0 || len(decisions) > 0 || len(reports) > 0 || len(openDecGrouped) > 0 || len(openEscalations) > 0 || !relevance.empty()
	if !hasContent && !jsonOutput {
		fmt.Fprintf(os.Stdout, "\nNo recent activity (last %s)\n\n", windowStr)
		return nil
	}

	activeIP, staleIP, unassignedIP := splitActiveStale(ipBeads, activeAgents)

	// Defend against a TRUNCATED roster manufacturing false-stale rows
	// (ka-0000007979): the roster fetch above is capped at 200, so a live agent
	// beyond that page is absent from the active set and its in-progress bead is
	// misclassified stale/reapable. Targeted-fetch each stale assignee's own agent
	// bead, and if it is actually live, fold it into the roster and re-split so its
	// work is not advertised as free to grab. No-op (0 round-trips) when the roster
	// fetch already resolved every stale assignee.
	if extra := resolveStaleAssigneeAgents(ctx, staleIP, roster, time.Now(), newsAliveFreshnessWindowValue()); len(extra) > 0 {
		roster = append(roster, extra...)
		activeAgents, agentSpawner = rosterDerived(roster)
		activeIP, staleIP, unassignedIP = splitActiveStale(ipBeads, activeAgents)
	}

	// Classify each stale bead (assignee agent gone) into one of three
	// triage-critical buckets (M4, ka-0000000465):
	//   - claimable: the assignee is a genuinely-terminated agent with no live
	//     replacement — the zombie reaper will release it and `kd claim` works.
	//   - hard-claimed: another LIVE agent still holds the claim (a truncated-live
	//     agent, or one whose claim survived a reap) — `kd claim` will fail.
	//   - respawned-successor-active: a NEW live agent occupies the same seat as
	//     the gone assignee. This is the dangerous case that nearly misled a
	//     pre-prod-apply session: the row reads "agent gone" while the successor
	//     is actively working the same state. It is marked so callers never treat
	//     it as free to grab.
	//   - escalation-parked: an open operator handle (escalation/decision) names
	//     the bead in requesting_task, so it is blocked on an operator action, not
	//     free work. Advertising it as reapable restarts the documented
	//     reap->respawn->re-escalate loop (ka-0000007805), so it is split out and
	//     excluded from the claimable bucket.
	parkedByBead := gatherEscalationParked(ctx, staleIP, projectLabels)
	staleClass := classifyStale(staleIP, roster, parkedByBead)

	if jsonOutput {
		// Pre-cap in_progress split: the shared ipBeads cap runs before this
		// active/stale/unassigned partition, so split the retained pre-cap slice
		// with the SAME activeAgents map to recover each bucket's un-truncated
		// count. These feed the per-section <section>_total fields; nothing else
		// consumes them, so the post-cap activeIP/staleIP/unassignedIP the render
		// path uses are untouched.
		preActiveIP, preStaleIP, preUnassignedIP := splitActiveStale(ipBeadsPreCap, activeAgents)
		totals := newsSectionTotals{
			InProgress: len(preActiveIP),
			Stale:      len(preStaleIP),
			Unassigned: len(preUnassignedIP),
			Closed:     closedPreCap,
			Decisions:  decisionsPreCap,
			Artifacts:  reportsPreCap,
		}
		return emitNewsJSON(windowStr, roster, agentSpawner, activeIP, staleIP, staleClass, unassignedIP, closedBeads, decisions, reports, openDecGrouped, openDecTotal, openEscalations, openEscTotal, totals, relevance, staleOnly, activeOnly, showAll)
	}

	// Filtered-output modes (--stale-only / --active-only) short-circuit the
	// normal pretty output. These are designed for gardener triage: pipe bead
	// IDs into rescue dispatches, or count live work without zombie inflation.
	if staleOnly || activeOnly {
		var picked []*beadsapi.BeadDetail
		if staleOnly {
			picked = staleIP
		} else {
			picked = activeIP
		}
		if idsOnly {
			// --ids-only is a bare machine stream (one "ID<TAB>title" per line for
			// `cut -f1`); injecting the operator-action heading block would corrupt
			// it, so the un-suppressible block is skipped ONLY for this scripting
			// format — never for human-readable output.
			for _, b := range picked {
				printNewsIDLine(b)
			}
			return nil
		}
		// The operator-action block (open P0/P1 escalations + open decisions) leads
		// even the filtered triage modes un-suppressibly (ka-0000002114): these
		// modes are exactly the gardener-triage path where a starved P0 escalation
		// or merge-gate decision most needs to be seen, so --active-only /
		// --stale-only no longer drop it.
		printed := printOperatorActionBlock(os.Stdout, openEscalations, openEscTotal, openDecGrouped, openDecTotal, limit)
		if len(picked) == 0 {
			if !printed {
				label := "stale"
				if activeOnly {
					label = "active"
				}
				fmt.Fprintf(os.Stdout, "\nNo %s in-progress beads\n\n", label)
			} else {
				fmt.Fprintln(os.Stdout)
			}
			return nil
		}
		heading := fmt.Sprintf("Stale (agent gone, %d):", len(picked))
		if activeOnly {
			heading = fmt.Sprintf("In-progress by others (%d):", len(picked))
		}
		if printed {
			fmt.Fprintln(os.Stdout)
			fmt.Fprintf(os.Stdout, "%s\n\n", heading)
		} else {
			fmt.Fprintf(os.Stdout, "\n%s\n\n", heading)
		}
		for _, b := range picked {
			printNewsBeadWithSpawner(b, agentSpawner)
		}
		fmt.Fprintln(os.Stdout)
		return nil
	}

	if idsOnly {
		for _, b := range ipBeads {
			printNewsIDLine(b)
		}
		return nil
	}

	// OPERATOR-ACTION block leads the digest, un-suppressibly (ka-0000002114).
	// Open P0/P1 escalations and open decisions are the pivotal facts EVERY
	// caller needs and the 23-session audit (category M1, the #1 gap) found
	// agents paying a separate round-trip for each cycle. They print FIRST —
	// above the caller-scoped relevance digest — so an agent piping through
	// `head -40` still sees them, and in every output mode (no flag hides them).
	printed := printOperatorActionBlock(os.Stdout, openEscalations, openEscTotal, openDecGrouped, openDecTotal, limit)

	// Caller-scoped relevance digest (audit fix M2/M3/M6): the block most likely
	// to change what the reading crew agent does next, printed AFTER the
	// un-suppressible operator-action block above. Its renderer ends without a
	// trailing blank line and carries its own leading newline; when the operator
	// block already printed, add one blank line separator.
	if !relevance.empty() {
		printRelevance(relevance, os.Stdout)
		printed = true
	}

	if len(ipBeads) > 0 {
		if len(activeIP) > 0 {
			if printed {
				fmt.Fprintln(os.Stdout)
				fmt.Fprintf(os.Stdout, "In-progress by others (%d):\n\n", len(activeIP))
			} else {
				fmt.Fprintf(os.Stdout, "\nIn-progress by others (%d):\n\n", len(activeIP))
			}
			for _, b := range activeIP {
				printNewsBeadWithSpawner(b, agentSpawner)
			}
			printed = true
		}
		if len(staleIP) > 0 {
			if printed {
				fmt.Fprintln(os.Stdout)
			}
			printStaleSections(os.Stdout, staleClass, agentSpawner)
			printed = true
		}
		if len(unassignedIP) > 0 {
			if printed {
				fmt.Fprintln(os.Stdout)
			}
			fmt.Fprintf(os.Stdout, "Unassigned in_progress (%d):\n\n", len(unassignedIP))
			for _, b := range unassignedIP {
				printNewsBeadWithSpawner(b, agentSpawner)
			}
			printed = true
		}
	}

	if len(closedBeads) > 0 {
		if printed {
			fmt.Fprintln(os.Stdout)
		}
		fmt.Fprintf(os.Stdout, "Closed in last %s (%d):\n\n", windowStr, len(closedBeads))
		for _, b := range closedBeads {
			printNewsBead(b)
		}
		printed = true
	}

	if len(decisions) > 0 {
		if printed {
			fmt.Fprintln(os.Stdout)
		}
		fmt.Fprintf(os.Stdout, "Decisions resolved in last %s (%d):\n\n", windowStr, len(decisions))
		for _, b := range decisions {
			printNewsDecision(b)
		}
		printed = true
	}

	if len(reports) > 0 {
		if printed {
			fmt.Fprintln(os.Stdout)
		}
		fmt.Fprintf(os.Stdout, "Artifacts submitted in last %s (%d):\n\n", windowStr, len(reports))
		for _, b := range reports {
			printNewsReport(b)
		}
	}

	fmt.Fprintln(os.Stdout)
	return nil
}

func printNewsBead(b *beadsapi.BeadDetail) {
	printNewsBeadWithSpawner(b, nil)
}

// printNewsBeadWithSpawner prints a bead with optional spawner info.
// If agentSpawner is non-nil and the bead's assignee has a known spawner,
// it is appended to help gardeners identify their crew.
// truncTitle shortens a title to at most `max` display runes, appending "…"
// when it cuts. Two properties the old `s[:max-1] + "…"` byte-slice lacked
// (ka-0000007728): it counts RUNES so a multi-byte character is never split
// into invalid UTF-8, and it backs off to the last word boundary so the digest
// never truncates mid-word (~12 sessions in the relevance audit hit that). If a
// single word is longer than the budget it is cut hard rather than dropped
// whole, so a very long token still fits. max <= 1 yields at most the ellipsis.
func truncTitle(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	// Collect the byte offset of each rune so we can slice on a boundary. Budget
	// max-1 runes to leave room for the ellipsis.
	budget := max - 1
	var lastSpace, count int // lastSpace = byte offset just before a run of spaces
	cut := len(s)
	for i, r := range s {
		if count == budget {
			cut = i
			break
		}
		if unicode.IsSpace(r) {
			lastSpace = i
		}
		count++
	}
	// Prefer the last word boundary within the budget so we don't split a word.
	// Fall back to the hard rune cut when the first word alone overflows.
	if lastSpace > 0 {
		cut = lastSpace
	}
	return strings.TrimRight(s[:cut], " ") + "…"
}

// withArms puts the emoji run of a bead's arms (heraldry, the canonical form
// on BeadDetail.Heraldry) and a space before title, the terminal form of arms
// (heraldry spec §4.2); a bead with none keeps title as it is. Apply it after
// truncTitle so the clip never counts or cuts the run.
func withArms(heraldry, title string) string {
	if run := beadsapi.HeraldryRun(heraldry); run != "" {
		return run + " " + title
	}
	return title
}

// printNewsIDLine renders one bead for the --ids-only machine format as
// "ID<TAB>title". The bare-ID form (2026 audit ka-0000001955 rec 7c) had zero
// observed uptake because an ID alone forces a follow-up `kd show` to learn what
// each row is; appending a tab-delimited title makes the stream self-describing
// while keeping `cut -f1` / `awk '{print $1}'` scripting intact. The title is
// whitespace-collapsed so a stray newline can never break the one-row-per-line
// contract.
func printNewsIDLine(b *beadsapi.BeadDetail) {
	title := strings.TrimSpace(newsDecisionWhitespace.ReplaceAllString(b.Title, " "))
	fmt.Fprintf(os.Stdout, "%s\t%s\n", b.ID, title)
}

func printNewsBeadWithSpawner(b *beadsapi.BeadDetail, agentSpawner map[string]string) {
	assignee := "unassigned"
	if b.Assignee != "" {
		assignee = "@" + b.Assignee
	}

	typeStr := ""
	if b.Type != "" {
		typeStr = fmt.Sprintf("[%s] ", b.Type)
	}

	title := withArms(b.Heraldry, truncTitle(b.Title, 72))

	spawnerSuffix := ""
	if agentSpawner != nil && b.Assignee != "" {
		if spawner, ok := agentSpawner[strings.ToLower(b.Assignee)]; ok {
			spawnerSuffix = fmt.Sprintf(" (by %s)", formatSpawner(spawner))
		}
	}

	// PARKED badge (ka-0000003797): an open operator decision names this bead in
	// its prose, so it is awaiting an operator choice. This is the text-mention
	// complement to the requesting_task-field parking gatherEscalationParked
	// already surfaces for the STALE bucket — it catches the case where a
	// decision only references the bead in its prompt/context, which carries no
	// blocking edge. Suffix the row so a triaging reader never treats it as free.
	parkedSuffix := ""
	if b.ParkedOnDecision {
		parkedSuffix = "  " + readyParkedBadge(b.ParkedOnDecisionBeadID)
	}

	fmt.Fprintf(os.Stdout, "  %s %s— %s  %s%s%s\n", b.ID, typeStr, title, assignee, spawnerSuffix, parkedSuffix)
}

func printNewsDecision(b *beadsapi.BeadDetail) {
	prompt := b.Fields["prompt"]
	if prompt == "" {
		prompt = b.Title
	}
	prompt = truncTitle(prompt, 60)

	chosen := b.Fields["chosen"]
	// Try to resolve the chosen option ID to a human-readable short label.
	if chosen != "" {
		if label := resolveOptionLabel(b.Fields["options"], chosen); label != "" {
			chosen = label
		}
		if len(chosen) > 40 {
			chosen = chosen[:39] + "…"
		}
	}

	respondedBy := b.Fields["responded_by"]
	if respondedBy == "" {
		respondedBy = "unknown"
	}

	if chosen != "" {
		fmt.Fprintf(os.Stdout, "  %s — %s  -> %s  (by @%s)\n", b.ID, prompt, chosen, respondedBy)
	} else {
		fmt.Fprintf(os.Stdout, "  %s — %s  (resolved by @%s)\n", b.ID, prompt, respondedBy)
	}
}

func printNewsReport(b *beadsapi.BeadDetail) {
	reportType := b.Fields["report_type"]
	if reportType == "" {
		reportType = "report"
	}

	decisionID := b.Fields["decision_id"]

	title := withArms(b.Heraldry, truncTitle(b.Title, 60))

	createdBy := b.CreatedBy
	if createdBy == "" {
		createdBy = "unknown"
	}

	if decisionID != "" {
		fmt.Fprintf(os.Stdout, "  %s [%s] — %s  (for %s, by @%s)\n", b.ID, reportType, title, decisionID, createdBy)
	} else {
		fmt.Fprintf(os.Stdout, "  %s [%s] — %s  (by @%s)\n", b.ID, reportType, title, createdBy)
	}
}

// openDecisionGroup carries an open decision plus two distinct relationship
// signals derived from its sibling entries:
//   - Duplicate: another open decision with the IDENTICAL identifier-token set
//     (DupCount/DupIDs). These are true duplicates — resolving one usually
//     moots the rest.
//   - Same-subject: another open decision that SHARES a subject id token but has
//     a different token set (SharesSubject/RelatedIDs/SubjectToken). These are
//     NOT duplicates — they are distinct forks about the same target, and per
//     ka-0000000559 (no compare-and-swap on a kbeads text field) resolving two
//     that rewrite the same field in one respawn window silently reverts the
//     first, so the operator needs to see the whole cluster to batch it.
type openDecisionGroup struct {
	Bead      *beadsapi.BeadDetail
	BucketKey string
	DupCount  int      // total exact-set bucket size, including this bead (1 = no dup)
	DupIDs    []string // sibling bead ids in the SAME exact-set bucket (excluding this one)

	// SubjectToken is the rarest identifier token this decision shares with at
	// least one other open decision (empty when it shares none). It is the
	// display label for the same-subject cluster.
	SubjectToken string
	// RelatedIDs are open decisions that share a subject token with this one but
	// are NOT exact duplicates (disjoint from DupIDs). Non-empty ⇒ same-subject
	// cluster that may need batched resolution.
	RelatedIDs []string
}

// SharesSubject reports whether this decision belongs to a multi-member
// same-subject cluster (has at least one non-duplicate related decision).
func (g openDecisionGroup) SharesSubject() bool { return len(g.RelatedIDs) > 0 }

var (
	newsDecisionWhitespace = regexp.MustCompile(`\s+`)
	// MR identifier (GitLab/GitHub style: "!2172", "#1234"). Captures the
	// stable target across gardener rephrasings.
	newsDecisionMRID = regexp.MustCompile(`[!#]\d+`)
	// Bead identifier ("ka-XXXXXXXXXXXX"). Lowercased prompt is matched, so
	// `ka-Bd...` and `ka-bd...` collide — desirable for dedup; collisions with
	// genuinely different beads are astronomically unlikely.
	newsDecisionBeadID = regexp.MustCompile(`(?:gd|ka)-[0-9a-z]{6,}`)
)

// decisionBucketKey is the duplicate-detection key. The previous implementation
// keyed off the first ~60 chars of the normalized prompt, which is defeated by
// gardener phrasing variation: two gardeners filing about the same MR or bead
// produce different prefixes ("MR !2172 merge gate" vs "merge-gate on !2172
// ready") and dedup misses (ka-0000008159, 24 dups on 2026-05-06).
//
// Strategy: extract identifying tokens (MR ids, bead ids) from the prompt and
// key on the sorted token set. When tokens are present they are the stable
// target identity; the surrounding prose is a phrasing artifact and ignored.
// Two decisions about the same MR/bead therefore always collide regardless of
// how the gardener wrote the prompt.
//
// When no tokens are present, fall back to the prior prefix bucket so prompts
// that legitimately have no identifier (e.g. "investigate the gardenio
// fanout?") still get a deterministic, content-derived key.
//
// The same function is shared with the `gb decision create` dedup
// (ka-0000003825), so `gb news` and `gb decision create` always agree.
func decisionBucketKey(prompt string) string {
	s := strings.ToLower(prompt)

	tokens := extractDecisionTargetTokens(s)
	if len(tokens) > 0 {
		return "ids:" + strings.Join(tokens, ",")
	}

	s = newsDecisionWhitespace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".!?:;,")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// extractDecisionTargetTokens pulls all MR ids and bead ids out of the
// (already-lowercased) prompt and returns them sorted + deduplicated. Returns
// nil when no tokens are found.
func extractDecisionTargetTokens(lowerPrompt string) []string {
	seen := make(map[string]struct{})
	var tokens []string
	for _, m := range newsDecisionMRID.FindAllString(lowerPrompt, -1) {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			tokens = append(tokens, m)
		}
	}
	for _, m := range newsDecisionBeadID.FindAllString(lowerPrompt, -1) {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			tokens = append(tokens, m)
		}
	}
	if len(tokens) == 0 {
		return nil
	}
	sort.Strings(tokens)
	return tokens
}

// newsSubjectTokenMaxFreq bounds same-subject grouping: a token appearing in
// this many or more decisions is treated as a "hub" (a shared epic every
// decision cites) and is NOT used to relate decisions — otherwise one popular
// id would chain every unrelated decision into a single mega-cluster
// (AC2, ka-0000000551). Tuned to the observed live case: the real cluster on
// ka-0000001007 is 4 decisions, so 5 keeps that intact while suppressing a
// 20-way epic hub.
const newsSubjectTokenMaxFreq = 5

// groupOpenDecisions buckets open decisions by their normalized prompt
// prefix and emits one entry per decision (preserving the input order, which
// is "oldest first" from the daemon — open decisions are a FIFO work queue, so
// the oldest/starved rows sort to the top and survive the per-section cap; see
// ka-0000005518). Two orthogonal relationships are annotated per entry:
//
//   - DUPLICATES (DupIDs/DupCount): siblings with the IDENTICAL identifier-token
//     set (same decisionBucketKey). Resolving one typically moots the rest.
//
//   - SAME-SUBJECT (RelatedIDs/SubjectToken): siblings that share at least one
//     identifier token but are NOT exact duplicates. These are distinct forks
//     about the same target (e.g. four different fixes to one live trigger). The
//     exact-set key put each in its own singleton bucket, hiding the collision
//     cluster the operator must resolve as a batch (ka-0000000551). We surface
//     them without labelling them duplicates because they provably are not.
//
// Same-subject grouping is bounded by newsSubjectTokenMaxFreq so a hub id (an
// epic every decision cites) does not chain unrelated decisions together.
func groupOpenDecisions(decisions []*beadsapi.BeadDetail) []openDecisionGroup {
	if len(decisions) == 0 {
		return nil
	}
	bucket := make(map[string][]string, len(decisions))
	keyByID := make(map[string]string, len(decisions))
	// tokensByID: the identifier tokens extracted from each decision's prompt.
	// tokenFreq: how many decisions each token appears in (rarity ranking +
	// hub suppression).
	tokensByID := make(map[string][]string, len(decisions))
	tokenFreq := make(map[string]int)
	for _, b := range decisions {
		prompt := b.Fields["prompt"]
		if prompt == "" {
			prompt = b.Title
		}
		key := decisionBucketKey(prompt)
		keyByID[b.ID] = key
		bucket[key] = append(bucket[key], b.ID)

		toks := extractDecisionTargetTokens(strings.ToLower(prompt))
		tokensByID[b.ID] = toks
		for _, t := range toks {
			tokenFreq[t]++
		}
	}

	// tokenMembers: for every non-hub token, the set of decision ids carrying
	// it. Membership drives same-subject grouping.
	tokenMembers := make(map[string][]string)
	for _, b := range decisions {
		for _, t := range tokensByID[b.ID] {
			if tokenFreq[t] >= newsSubjectTokenMaxFreq {
				continue // hub token — do not relate on it (AC2)
			}
			tokenMembers[t] = append(tokenMembers[t], b.ID)
		}
	}

	out := make([]openDecisionGroup, 0, len(decisions))
	for _, b := range decisions {
		key := keyByID[b.ID]
		ids := bucket[key]
		dupSet := make(map[string]struct{}, len(ids))
		var siblings []string
		for _, id := range ids {
			dupSet[id] = struct{}{}
			if id != b.ID {
				siblings = append(siblings, id)
			}
		}

		// Same-subject: collect ids sharing any non-hub token with this bead,
		// excluding self and exact duplicates. Pick the RAREST shared token as
		// the display subject so the cluster reads by its most specific target.
		relatedSet := make(map[string]struct{})
		subjectToken := ""
		subjectFreq := 0
		for _, t := range tokensByID[b.ID] {
			members := tokenMembers[t]
			if len(members) < 2 {
				continue // token unique to this decision — not a shared subject
			}
			freq := tokenFreq[t]
			if subjectToken == "" || freq < subjectFreq || (freq == subjectFreq && t < subjectToken) {
				subjectToken = t
				subjectFreq = freq
			}
			for _, id := range members {
				if id == b.ID {
					continue
				}
				if _, isDup := dupSet[id]; isDup {
					continue // already surfaced as a duplicate
				}
				relatedSet[id] = struct{}{}
			}
		}
		var related []string
		for id := range relatedSet {
			related = append(related, id)
		}
		sort.Strings(related)
		if len(related) == 0 {
			subjectToken = "" // no genuine cluster — clear the display label
		}

		out = append(out, openDecisionGroup{
			Bead:         b,
			BucketKey:    key,
			DupCount:     len(ids),
			DupIDs:       siblings,
			SubjectToken: subjectToken,
			RelatedIDs:   related,
		})
	}
	return out
}

// shortAge renders a created_at timestamp as a compact "5h ago" / "30m ago"
// suffix. Anything older than 7d falls back to a date stamp; recent rows
// dominate the open-decisions view in practice.
func shortAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.UTC().Format("2006-01-02")
	}
}

func printNewsOpenDecisionTo(w io.Writer, od openDecisionGroup) {
	b := od.Bead
	prompt := b.Fields["prompt"]
	if prompt == "" {
		prompt = b.Title
	}
	prompt = newsDecisionWhitespace.ReplaceAllString(prompt, " ")
	prompt = truncTitle(prompt, 72)

	createdBy := b.CreatedBy
	if createdBy == "" {
		createdBy = "unknown"
	}

	fmt.Fprintf(w, "  %s — %s\n", b.ID, prompt)
	fmt.Fprintf(w, "      by @%s, filed %s\n", createdBy, shortAge(b.CreatedAt))
	if od.DupCount > 1 {
		fmt.Fprintf(w, "      ⚠ DUP×%d: %s\n", od.DupCount, strings.Join(od.DupIDs, ", "))
	}
	if od.SharesSubject() {
		fmt.Fprintf(w, "      ↔ SAME-SUBJECT %s ×%d (resolve as a batch — not duplicates): %s\n",
			od.SubjectToken, len(od.RelatedIDs)+1, strings.Join(od.RelatedIDs, ", "))
	}
}

// newsEscalationMaxPriority bounds which open escalations lead the digest as
// operator-action items (ka-0000002114). An escalation is a concrete "operator
// must DO this" ask that BLOCKS the requesting task until acted on, so the
// high-priority ones (P0/P1, i.e. priority <= 1) are at least as pivotal as an
// open decision and belong at the un-suppressible top of `gb news`. P2+
// escalations are real but not lead-the-digest urgent; they still surface via
// the normal in-progress/parked machinery and `gb escalation list`, so gating
// on P0/P1 keeps the top block signal-dense rather than a dumping ground.
const newsEscalationMaxPriority = 1

// filterHighPriorityEscalations keeps only the open escalations at or above the
// P0/P1 urgency bar (priority <= newsEscalationMaxPriority). The daemon stores
// P0 as priority 0 and higher numbers as lower urgency, so the ASC
// priority,created_at sort already delivers them most-urgent-first; this just
// drops the P2+ tail. Input order is preserved so the caller's cap keeps the
// most-urgent rows.
func filterHighPriorityEscalations(beads []*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
	out := make([]*beadsapi.BeadDetail, 0, len(beads))
	for _, b := range beads {
		if b == nil {
			continue
		}
		if b.Priority <= newsEscalationMaxPriority {
			out = append(out, b)
		}
	}
	return out
}

// printOperatorActionBlock renders the un-suppressible operator-action block —
// open P0/P1 escalations first, then open decisions — that leads every `gb
// news` output mode (ka-0000002114). It returns whether anything was printed so
// the caller can manage the one-blank-line separator to the section that
// follows. Both sub-sections carry a truncation marker when the per-section cap
// dropped rows, so a starved item always reads as "below the fold", never as
// "no such item".
func printOperatorActionBlock(
	w io.Writer,
	openEscalations []*beadsapi.BeadDetail,
	openEscTotal int,
	openDecGrouped []openDecisionGroup,
	openDecTotal int,
	limit int,
) bool {
	printed := false
	if len(openEscalations) > 0 {
		fmt.Fprintf(w, "\n⚠ Open escalations awaiting operator — P0/P1 (%d):\n\n", len(openEscalations))
		for _, b := range openEscalations {
			printNewsEscalation(w, b)
		}
		// Truncation marker mirrors the open-decisions one below: with the
		// priority,created_at sort the cap drops the LOWEST-priority/newest rows,
		// so the shown set is the most-urgent N. Tell the operator how many are
		// hidden so an un-surfaced escalation reads as "below the fold".
		if limit > 0 && len(openEscalations) == limit && openEscTotal > limit {
			fmt.Fprintf(w, "  … and %d more open P0/P1 escalation(s) not shown (most-urgent shown first; `gb news -n 0` for all)\n", openEscTotal-len(openEscalations))
		}
		printed = true
	}

	if len(openDecGrouped) > 0 {
		if printed {
			fmt.Fprintln(w)
			fmt.Fprintf(w, "Open decisions awaiting operator (%d):\n\n", len(openDecGrouped))
		} else {
			fmt.Fprintf(w, "\nOpen decisions awaiting operator (%d):\n\n", len(openDecGrouped))
		}
		for _, od := range openDecGrouped {
			printNewsOpenDecisionTo(w, od)
		}
		// Truncation marker: with the oldest-first sort above, capBeads drops the
		// NEWEST rows past `limit`, so the shown set is the oldest N. Tell the
		// operator how many are hidden so an un-surfaced decision reads as "below
		// the fold", never "no such decision" (ka-0000005518). Gate on actually
		// hitting the cap (shown == limit) so a client-side project filter dropping
		// dual-project rows can't manufacture a false marker while under the limit.
		if limit > 0 && len(openDecGrouped) == limit && openDecTotal > limit {
			fmt.Fprintf(w, "  … and %d more open decision(s) not shown (oldest shown first; `gb news -n 0` for all)\n", openDecTotal-len(openDecGrouped))
		}
		printed = true
	}
	return printed
}

// printNewsEscalation renders one open escalation as a two-line operator-action
// row: the priority-tagged prompt/title, then the requesting agent + filed age.
// The operation_type field (mint_secret, gitlab_mr_merge, …) is surfaced inline
// so the operator sees WHAT action is being asked for without a `kd show`.
func printNewsEscalation(w io.Writer, b *beadsapi.BeadDetail) {
	summary := b.Title
	if summary == "" {
		summary = b.Fields["operation_spec"]
	}
	summary = newsDecisionWhitespace.ReplaceAllString(summary, " ")
	summary = withArms(b.Heraldry, truncTitle(summary, 68))

	createdBy := b.CreatedBy
	if createdBy == "" {
		createdBy = "unknown"
	}

	opType := b.Fields["operation_type"]
	opSuffix := ""
	if opType != "" {
		opSuffix = fmt.Sprintf("  [%s]", opType)
	}

	fmt.Fprintf(w, "  %s P%d — %s%s\n", b.ID, b.Priority, summary, opSuffix)
	fmt.Fprintf(w, "      by @%s, filed %s\n", createdBy, shortAge(b.CreatedAt))
}

// resolveOptionLabel finds the short or label text for a chosen option ID.
func resolveOptionLabel(optionsJSON, chosenID string) string {
	if optionsJSON == "" || chosenID == "" {
		return ""
	}
	var opts []map[string]any
	if json.Unmarshal([]byte(optionsJSON), &opts) != nil {
		return ""
	}
	for _, opt := range opts {
		id, _ := opt["id"].(string)
		if id != chosenID {
			continue
		}
		if short, _ := opt["short"].(string); short != "" {
			return short
		}
		if label, _ := opt["label"].(string); label != "" {
			return label
		}
	}
	return ""
}

// splitActiveStale partitions in-progress beads into three buckets:
//   - liveIP: assignee is in the active-agent set (another agent is actually working on it).
//   - staleIP: assignee is set but the agent is gone (zombie).
//   - unassignedIP: no assignee at all AND not formula scaffolding. Phased
//     formula molecules sit on status=in_progress while waiting for a prior
//     phase gate to clear — they are "ready but not newsworthy" and are
//     dropped entirely (see ka-0000002078). Unassigned non-formula beads
//     still surface here so genuinely-orphaned work is visible.
func splitActiveStale(beads []*beadsapi.BeadDetail, active map[string]bool) (liveIP, staleIP, unassignedIP []*beadsapi.BeadDetail) {
	for _, b := range beads {
		switch {
		case b.Assignee == "":
			if isFormulaScaffolding(b) {
				continue
			}
			unassignedIP = append(unassignedIP, b)
		case !active[strings.ToLower(b.Assignee)]:
			staleIP = append(staleIP, b)
		default:
			liveIP = append(liveIP, b)
		}
	}
	return liveIP, staleIP, unassignedIP
}

// isFormulaScaffolding reports whether a bead was instantiated from a formula
// (kd formula apply / pour / wisp). These beads sit on in_progress while a
// phased gate clears and aren't independently actionable — they're not
// conflict-detection signal for `gb news`. We recognize them by type=molecule
// (canonical formula instantiation) or by a non-empty formula_id / source_formula
// field, which covers task/epic step beads spawned by a formula.
func isFormulaScaffolding(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if b.Type == "molecule" {
		return true
	}
	if b.Fields == nil {
		return false
	}
	if b.Fields["formula_id"] != "" || b.Fields["source_formula"] != "" {
		return true
	}
	return false
}

// newsAliveFreshnessWindow bounds how long an open agent bead may go without a
// controller status write before `gb news` stops reporting its agent as alive.
//
// WHY THIS EXISTS (ka-0000000637). `alive` is the liveness primitive gardener
// prompts gate dispatch on. Before this bound it was derived purely from the
// bead's agent_state field with NO pod-existence or freshness grounding:
//   - The pod_phase guard below is inert — nothing in the controller ever
//     writes pod_phase onto an agent bead (verified: 0 populated in prod; the
//     field has readers and a schema enum but no writer), so `alive` collapsed
//     to "agent_state ∉ {done,failed,rate_limited}".
//   - An agent whose pod despawns WITHOUT a clean in-pod `gb stop` (OOM, evict,
//     SIGKILL, entrypoint exit, force-delete) leaves its bead open at
//     agent_state=working. The reconciler's leaked-bead close only fires for
//     ALREADY-terminal states, so such a bead reads alive:true until the 2h
//     stale reaper — long enough to corrupt a 30-minute gardener cycle
//     (release_owner_live>0 gate, live_bug_crew budget term).
//
// The controller refreshes every genuinely-live pod's bead at least once per
// COOP_SYNC_INTERVAL (default 60s) via SyncAll's status/usage/backend writes,
// each of which bumps updated_at. Measured against the live fleet, every agent
// with a running pod had updated_at within ~60s. So updated_at IS a
// controller-maintained liveness heartbeat: an agent whose pod is gone stops
// getting bumps, and once its bead ages past this window it is no longer
// reported alive. 5m = 5× the default sync cadence — comfortably above cadence
// + jitter so a live-but-busy agent is never false-negatived, yet dramatically
// shorter than the 2h reaper tail, bounding the false-positive window to ≤5m.
// Mirrors the beads-viewer /agents staleness precedent (agentStaleThreshold,
// audit ka-0000007486) and the orphanReapAfter=5m bound in the stale reaper.
//
// Override with GB_NEWS_ALIVE_FRESHNESS_WINDOW (a Go duration, e.g. "3m"); a
// value <= 0 disables the bound (restores the prior agent_state-only behavior).
const newsAliveFreshnessWindow = 5 * time.Minute

// newsAliveFreshnessWindowValue resolves the effective freshness window,
// honoring the GB_NEWS_ALIVE_FRESHNESS_WINDOW override. An unparseable value
// falls back to the compiled default so a typo never silently disables the
// bound.
func newsAliveFreshnessWindowValue() time.Duration {
	if v := os.Getenv("GB_NEWS_ALIVE_FRESHNESS_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return newsAliveFreshnessWindow
}

// newsAgentInfo is the per-agent roster row `gb news` builds once from the open
// type=agent beads and reuses for liveness, the JSON agents array, and
// respawned-successor detection (M4, ka-0000000465). Before this the roster was a
// bare name→bool alive map plus a name→spawner map, and the JSON agents array
// carried NO status or role — the single field release gardeners query
// (`.agents[] | .role` / `.status`) was null on 100% of rows across three audit
// sessions. Carrying the source bead's fields on the row lets emitNewsJSON
// populate status/role/pod_phase/heartbeat directly.
type newsAgentInfo struct {
	Name      string // lowercased agent name (roster key)
	Alive     bool   // passes the liveness predicate below
	Role      string // role field (crew, gardener, thread, …)
	State     string // agent_state field (working, spawning, done, …)
	PodPhase  string // pod_phase field when populated (currently usually empty)
	SpawnedBy string // spawned_by field
	SeatKey   string // seat_key field — used to detect a respawned successor
	TaskID    string // task_id field — the bead this agent is working
	UpdatedAt time.Time
	// IsSelf marks the caller's own row. Self is kept in the roster (so its
	// liveness is known — otherwise the caller's own in_progress beads, visible
	// under --all, would be misclassified stale) but is hidden from the displayed
	// agents array/roster so it never inflates the teammate list (M4,
	// ka-0000000465).
	IsSelf bool
}

// newsAliveState reports whether an agent bead passes the liveness predicate,
// factored out of buildNewsAgents so classifyStale can re-check a candidate
// successor bead with the identical rule. An agent is alive only when its bead
// is (1) not in a terminal agent_state (done, failed, rate_limited), (2) not in
// a terminal pod_phase (Failed, Succeeded), (3) updated within freshWindow, and
// (4) not progress-stalled past ProgressStaleThreshold. `now`/`freshWindow` are
// injected for testability; a freshWindow <= 0 disables the freshness check.
func newsAliveState(fields map[string]string, updatedAt time.Time, now time.Time, freshWindow time.Duration) bool {
	switch fields["agent_state"] {
	case "done", "failed", "rate_limited":
		return false
	}
	switch fields["pod_phase"] {
	case "Failed", "Succeeded":
		return false
	}
	// Freshness bound (ka-0000000637): a live pod's bead is refreshed by the
	// controller at least once per sync cadence; a despawned agent's is not. A
	// zero updated_at (unparseable/absent) fails OPEN — treated as fresh — so a
	// daemon-side timestamp glitch never hides a genuinely-live agent.
	if freshWindow > 0 && !updatedAt.IsZero() && now.Sub(updatedAt) > freshWindow {
		return false
	}
	// Progress bound (ka-0000000286): updated_at STAYS FRESH through a livelock,
	// so last_progress_at is the only signal that catches a wedged crew whose pod
	// is alive but making zero progress. Fails open when absent/unparseable.
	if beadsapi.AgentProgressStalled(fields, now, beadsapi.ProgressStaleThreshold) {
		return false
	}
	return true
}

// isPrewarmedPoolAgent reports whether an agent bead is an unassigned
// prewarmed-pool placeholder — a warm pod idling for assignment, not a teammate
// doing work. These inflated the roster ("3/4 rows were junk" in the audit,
// ka-0000000465). Recognized by agent_state=prewarmed or the "prewarmed" label
// with no engagement marker (task/thread binding) — mirrors the bridge's
// isUnengagedPoolAgent gate. An engaged pool agent (it has a task_id) has left
// the pool and is a real teammate, so it is NOT excluded.
func isPrewarmedPoolAgent(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if b.Fields["agent_state"] == "prewarmed" {
		// A prewarmed-state bead that already carries a task assignment has been
		// handed out of the pool (the state write races the assignment); keep it.
		return b.Fields["task_id"] == ""
	}
	hasPrewarmedLabel := false
	for _, l := range b.Labels {
		if l == "prewarmed" {
			hasPrewarmedLabel = true
			break
		}
	}
	if !hasPrewarmedLabel {
		return false
	}
	// Any engagement marker means it left the pool — keep it.
	if b.Fields["task_id"] != "" {
		return false
	}
	if b.Fields["slack_thread_channel"] != "" && b.Fields["slack_thread_ts"] != "" {
		return false
	}
	if strings.HasPrefix(b.Fields["spawn_source"], "slack-thread") {
		return false
	}
	return true
}

// buildNewsAgents builds the roster from the open type=agent beads. It excludes
// prewarmed-pool placeholders (roster junk) entirely and TAGS the caller's own
// row IsSelf so downstream display can hide it — both were called out in the
// audit (ka-0000000465). Self is kept (not dropped) so its liveness is known:
// dropping it would make the caller's own in_progress beads (visible under
// --all) resolve to a missing agent and be misclassified as stale. selfActor is
// the caller's agent name (bare, e.g. "gardenio-foo-abc"); the leaked
// "gardenio/foo-abc" env form is normalized here. Every retained row carries
// role/state/pod_phase/heartbeat so downstream consumers stop seeing null
// status/role.
func buildNewsAgents(beads []*beadsapi.BeadDetail, now time.Time, freshWindow time.Duration, selfActor string) []newsAgentInfo {
	selfLower := normalizeSelfActor(selfActor)
	out := make([]newsAgentInfo, 0, len(beads))
	for _, ab := range beads {
		name := ab.Fields["agent"]
		if name == "" {
			continue
		}
		nameLower := strings.ToLower(name)
		isSelf := selfLower != "" && nameLower == selfLower
		// Drop unassigned prewarmed-pool placeholders — pool scaffolding, not work.
		// (Self is never a prewarmed placeholder, so this order is safe.)
		if isPrewarmedPoolAgent(ab) {
			continue
		}
		out = append(out, newsAgentInfo{
			Name:      nameLower,
			Alive:     newsAliveState(ab.Fields, ab.UpdatedAt, now, freshWindow),
			Role:      ab.Fields["role"],
			State:     ab.Fields["agent_state"],
			PodPhase:  ab.Fields["pod_phase"],
			SpawnedBy: ab.Fields["spawned_by"],
			SeatKey:   ab.Fields["seat_key"],
			TaskID:    ab.Fields["task_id"],
			UpdatedAt: ab.UpdatedAt,
			IsSelf:    isSelf,
		})
	}
	return out
}

// normalizeSelfActor reduces the caller's actor name to the bare agent-name form
// used in the roster. The gb actor is the git user.name (bare, e.g.
// "gardenio-foo-abc"), but BEADS_AGENT_NAME leaks a "project/agent" form
// ("gardenio/gardenio-foo-abc"); strip a leading "project/" so self-exclusion
// matches regardless of which form reached us. Returns lowercase.
func normalizeSelfActor(actor string) string {
	actor = strings.ToLower(strings.TrimSpace(actor))
	if actor == "" || actor == "unknown" {
		return ""
	}
	if i := strings.LastIndex(actor, "/"); i >= 0 {
		actor = actor[i+1:]
	}
	return actor
}

// rosterDerived projects the roster into the two legacy maps the rest of the
// news pipeline consumes: the alive-agent set (lowercased name → true) and the
// spawner map (lowercased name → spawned_by). Only ALIVE agents populate the
// active set, preserving the prior buildActiveAgents contract.
func rosterDerived(roster []newsAgentInfo) (active map[string]bool, spawners map[string]string) {
	active = make(map[string]bool)
	spawners = make(map[string]string)
	for _, a := range roster {
		if a.SpawnedBy != "" {
			spawners[a.Name] = a.SpawnedBy
		}
		if a.Alive {
			active[a.Name] = true
		}
	}
	return active, spawners
}

// buildActiveAgents is the legacy (active, spawners) view over the roster, kept
// as the thin composition of buildNewsAgents (no self-exclusion) + rosterDerived
// so existing callers/tests that only need the two maps stay unchanged. New code
// wanting role/state/heartbeat should use buildNewsAgents directly.
func buildActiveAgents(beads []*beadsapi.BeadDetail, now time.Time, freshWindow time.Duration) (active map[string]bool, spawners map[string]string) {
	return rosterDerived(buildNewsAgents(beads, now, freshWindow, ""))
}

// staleClass is one of the three triage buckets a stale in-progress bead falls
// into (M4, ka-0000000465). The distinction is load-bearing: two of the three
// must NOT be treated as free to claim, and the third burned a session by
// masking a live successor before a prod apply.
type staleClass int

const (
	// staleClaimable: the assignee is a genuinely-terminated agent with no live
	// replacement. The zombie reaper will release the claim (reopen the bead) and
	// `kd claim` succeeds today. Safe to pick up.
	staleClaimable staleClass = iota
	// staleHardClaimed: another LIVE agent still holds this exact bead's claim
	// (a page-truncated-but-live agent, or a claim that survived a reap). `kd
	// claim` FAILS with "already claimed". Do not attempt.
	staleHardClaimed
	// staleRespawnedSuccessorActive: a NEW live agent now occupies the same SEAT
	// as the gone assignee (same seat_key, or same task_id under a different live
	// agent). The row reads "agent gone" but the successor is actively working —
	// the dangerous case that nearly misled a pre-prod-apply session. Never grab.
	staleRespawnedSuccessorActive
	// staleEscalationParked: the bead is parked behind an open operator handle —
	// an open type=escalation / type=decision whose requesting_task names this
	// bead (the blocks-edge `--requesting-task` writes since 2026-07, advice
	// ka-0000001746). It is blocked on an operator action, not free work, and it
	// closes itself when that action executes (close-on-escalation-execute). The
	// prior classifier had no class for this, so every such bead fell through to
	// claimable and got advertised as reapable — inviting the documented
	// reap->respawn->re-escalate loop (ka-0000007805; ka-0000000909 measured 12+
	// wasted dispatches on one bead). Never grab.
	staleEscalationParked
	// staleAwaitsRelease: the bead carries awaits_release=true. Its PR merged and
	// the deployment of the first release that carries it closes it; the
	// stale-reaper leaves it in_progress (gd-aaGqLb2cUk). Never grab.
	staleAwaitsRelease
)

// staleEntry pairs a stale bead with its computed class and (when relevant) the
// live agent that makes it hard-claimed or a respawned successor, or the open
// operator handle that makes it escalation-parked.
type staleEntry struct {
	Bead      *beadsapi.BeadDetail
	Class     staleClass
	Successor string // the live successor/holder agent name, when Class is hard-claimed / respawned-successor
	Handle    string // the open escalation/decision bead id, when Class is escalation-parked
}

// resolveStaleAssigneeAgents defends the stale classification against a
// TRUNCATED agent roster (ka-0000007979). The roster fetch below is capped
// (Limit 200, no per-project scope); when the live fleet is larger than that
// cap, a genuinely-live agent's bead can fall outside the fetched page and be
// absent from the roster ENTIRELY — not merely resolved-and-not-alive.
// splitActiveStale then finds the assignee in neither the active set nor the
// roster, so its in-progress bead is reported STALE ("agent gone") and a gardener
// following the runbook's stale-work rule reaps a live agent's work.
//
// This was reproduced against a live victim (bug body): agent ka-0000004764 was
// agent_state=working with live_turns climbing 140→436, yet its bead surfaced
// under `gb news --stale-only` and was cleared three times. The cs gardener who
// filed the third sighting even checked liveness first and got a FALSE-NEGATIVE
// from paging — the same truncation, one layer up.
//
// The fix is the bounded, targeted JOIN on the agent bead the bug asks for
// (fix #1), performed ONLY for the small set of stale assignees the roster did
// not already resolve. Each unresolved assignee is looked up directly by its
// `agent` field and re-checked with the identical newsAliveState predicate
// (agent_state / last_progress_at / freshness). Live matches are returned as
// roster rows so the caller can re-derive the active set and re-split, moving the
// false-stale bead back into the active bucket where it belongs. Assignees
// already present in the roster (at any liveness) are skipped — they were fetched
// and classified from ground truth. Best-effort: a nil daemon or a per-assignee
// fetch error drops that assignee (it stays classified exactly as before) rather
// than failing `gb news`. The round-trips are bounded by the number of DISTINCT
// unresolved stale assignees, which is 0 whenever the roster fetch was complete.
func resolveStaleAssigneeAgents(ctx context.Context, staleIP []*beadsapi.BeadDetail, roster []newsAgentInfo, now time.Time, freshWindow time.Duration) []newsAgentInfo {
	if daemon == nil || len(staleIP) == 0 {
		return nil
	}
	known := make(map[string]bool, len(roster))
	for _, a := range roster {
		known[a.Name] = true
	}
	// Distinct stale assignees the roster did not resolve. Key by lowercased name
	// (roster/active keys are lowercased) but keep the original-case assignee
	// string for the field-equality lookup, since fields.agent is stored as-cased.
	unresolved := make(map[string]string) // lowercased name → raw assignee
	for _, b := range staleIP {
		if b == nil || b.Assignee == "" {
			continue
		}
		lower := strings.ToLower(b.Assignee)
		if known[lower] {
			continue
		}
		if _, seen := unresolved[lower]; !seen {
			unresolved[lower] = b.Assignee
		}
	}
	if len(unresolved) == 0 {
		return nil
	}
	var extra []newsAgentInfo
	for _, assignee := range unresolved {
		res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"open"},
			Fields:   map[string]string{"agent": assignee},
			Limit:    5,
		})
		if err != nil || res == nil {
			continue
		}
		// buildNewsAgents applies the identical liveness predicate and lowercases
		// the name; selfActor is "" because a stale assignee is never the caller.
		extra = append(extra, buildNewsAgents(res.Beads, now, freshWindow, "")...)
	}
	return extra
}

// classifyStale sorts each stale in-progress bead (its assignee agent is gone)
// into claimable / hard-claimed / respawned-successor-active / escalation-parked.
// The roster carries every LIVE agent's seat_key and task_id, so a successor is
// detected without any extra daemon round-trip:
//   - respawned-successor-active: a live agent shares the gone assignee's seat
//     (same seat_key) or is assigned the same task_id — a fresh pod took over the
//     seat/work while this row still names the dead predecessor.
//   - hard-claimed: no successor, but the assignee itself resolves to a live
//     roster row (it was excluded from the active set only by page truncation or
//     a self/prewarmed filter, yet is genuinely alive) — the claim still stands.
//   - escalation-parked: no live agent holds it, but an open operator handle
//     (escalation/decision) names it in requesting_task, so it is blocked on an
//     operator action rather than free — parkedByBead[bead.ID] gives the handle.
//   - awaits-release: awaits_release=true; the release that carries it closes it.
//   - claimable: none of the above; the assignee is gone and nothing live holds
//     the seat, so the reaper will release it and `kd claim` works.
//
// parkedByBead maps a stale bead's id to the open escalation/decision bead that
// parks it (see gatherEscalationParked). A stale bead with a live successor or
// live holder still classifies as that live case — the parked check is only the
// fallthrough before claimable, so a genuinely-live row is never masked.
func classifyStale(staleIP []*beadsapi.BeadDetail, roster []newsAgentInfo, parkedByBead map[string]string) []staleEntry {
	// Index the live roster by seat_key and by task_id for O(1) successor lookup.
	liveBySeat := make(map[string]string)
	liveByTask := make(map[string]string)
	liveByName := make(map[string]bool)
	for _, a := range roster {
		if !a.Alive {
			continue
		}
		liveByName[a.Name] = true
		if a.SeatKey != "" {
			liveBySeat[a.SeatKey] = a.Name
		}
		if a.TaskID != "" {
			liveByTask[a.TaskID] = a.Name
		}
	}
	// Index the FULL roster (any liveness) by name so we can find the gone
	// assignee's own seat_key even though it is not in the live set.
	seatByAgent := make(map[string]string)
	for _, a := range roster {
		if a.SeatKey != "" {
			seatByAgent[a.Name] = a.SeatKey
		}
	}

	out := make([]staleEntry, 0, len(staleIP))
	for _, b := range staleIP {
		assignee := strings.ToLower(b.Assignee)
		entry := staleEntry{Bead: b, Class: staleClaimable}

		// Respawned successor working the same TASK: a different live agent is
		// assigned this bead's own id. This is the most direct signal.
		if holder, ok := liveByTask[b.ID]; ok && holder != assignee {
			entry.Class = staleRespawnedSuccessorActive
			entry.Successor = holder
			out = append(out, entry)
			continue
		}
		// Respawned successor occupying the same SEAT: a live agent shares the
		// gone assignee's seat_key. A fresh pod took the seat while this row still
		// names the dead predecessor.
		if seat := seatByAgent[assignee]; seat != "" {
			if holder, ok := liveBySeat[seat]; ok && holder != assignee {
				entry.Class = staleRespawnedSuccessorActive
				entry.Successor = holder
				out = append(out, entry)
				continue
			}
		}
		// Hard-claimed: the assignee itself is actually still live (truncated out
		// of the active set, or filtered as self/prewarmed) — the claim stands and
		// `kd claim` would fail.
		if liveByName[assignee] {
			entry.Class = staleHardClaimed
			entry.Successor = assignee
			out = append(out, entry)
			continue
		}
		// Escalation-parked: nothing live holds it, but an open operator handle
		// (escalation/decision) names this bead in requesting_task — it is blocked
		// on an operator action, not free work, and closes itself when that action
		// executes. Reaping it here restarts the documented reap->respawn->
		// re-escalate loop (ka-0000007805).
		if handle := parkedByBead[b.ID]; handle != "" {
			entry.Class = staleEscalationParked
			entry.Handle = handle
			out = append(out, entry)
			continue
		}
		if beadsapi.AwaitsRelease(b.Fields) {
			entry.Class = staleAwaitsRelease
			out = append(out, entry)
			continue
		}
		out = append(out, entry)
	}
	return out
}

// gatherEscalationParked returns a map from stale-bead id to the not-closed
// escalation/decision bead that parks it — the load-bearing signal for the
// staleEscalationParked class (ka-0000007805). It reads the requesting_task
// field on every NOT-CLOSED type=escalation / type=decision bead (open, or
// in_progress while the merge desk executes it): since 2026-07 both
// `gb escalation create --requesting-task` and `gb decision create
// --requesting-task` stamp that field (and write the blocks-edge) so the parked
// work knows which handle it waits on (advice ka-0000001746).
//
// It matches a handle to a stale bead by TWO relations (ka-0000003000):
//
//	(a) direct: the handle's requesting_task IS the stale bead itself. This is
//	    the single-bead shape (an MR-merge task files its own merge escalation)
//	    and was the only relation the classifier understood historically.
//
//	(b) subtree: the handle's requesting_task is a bead in the stale bead's
//	    parent-child SUBTREE. A molecule/formula parks differently — the gate
//	    STEP files the handle, so `decision.requesting_task == <gate-step>`,
//	    never the molecule ROOT that appears in the stale section. Keying only
//	    on the exact id (relation (a)) mislabeled every such gate-parked
//	    molecule as `claimable (reapable)` — a data-loss path, since NWS-1
//	    drives the stale set toward 0 by closing/unassigning it seconds before
//	    the operator approves the gated deliverable.
//
// For the subtree relation we also treat an OPEN decision/escalation living IN
// the stale bead's subtree as parking the root directly, regardless of which
// bead its own requesting_task names — the inverse safety net: a stale
// container whose only open children are a gate + an un-actioned operator
// handle is never free work.
//
// It intentionally does NOT key on the close-on-escalation-execute label alone
// — that label is only a corroborating signal and can outlive a closed handle,
// so keying on the live open handle is what keeps the class precise (it clears
// the moment the operator acts and the handle closes). Only bead ids present in
// the stale set are returned so the map stays scoped to what the stale
// classifier actually needs.
//
// Best-effort: a daemon error yields an empty map (the caller degrades to the
// prior claimable-by-default behavior rather than failing `gb news`). The
// subtree fetch is scoped to stale molecule/formula rows only — the single-bead
// shape needs no walk — so the extra round-trips are bounded by the (small)
// number of stale multi-step containers.
func gatherEscalationParked(ctx context.Context, staleIP []*beadsapi.BeadDetail, projectLabels []string) map[string]string {
	parked := make(map[string]string)
	if daemon == nil || len(staleIP) == 0 {
		return parked
	}
	staleIDs := make(map[string]bool, len(staleIP))
	for _, b := range staleIP {
		staleIDs[b.ID] = true
	}
	// Every non-closed handle parks, not just open ones: the merge desk claims a
	// merge escalation (in_progress) while it lands the PR, and the task it names
	// is still parked until the handle closes (gd-Z2E1CJKZR7).
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation", "decision"},
		Statuses: beadsapi.ActiveStatuses(),
		Labels:   projectLabels,
		Limit:    500,
	})
	if err != nil || res == nil {
		return parked
	}

	// consider records a parking relation. Lex-first handle wins when several
	// open handles could park the same stale bead, so the reported handle is
	// stable across runs. Only stale beads are recorded.
	consider := func(bead, handle string) {
		if !staleIDs[bead] {
			return
		}
		if existing, ok := parked[bead]; !ok || handle < existing {
			parked[bead] = handle
		}
	}

	// Pass 1 — relation (a), and index every handle by the bead it names so the
	// subtree pass can look a descendant up without re-scanning.
	handleByReqTask := make(map[string]string, len(res.Beads))
	for _, h := range res.Beads {
		if h == nil || h.Fields == nil {
			continue
		}
		reqTask := h.Fields["requesting_task"]
		if reqTask == "" {
			continue
		}
		consider(reqTask, h.ID)
		if existing, ok := handleByReqTask[reqTask]; !ok || h.ID < existing {
			handleByReqTask[reqTask] = h.ID
		}
	}

	// Pass 2 — relation (b): for each stale multi-step container, resolve its
	// open parent-child subtree and park the ROOT when a handle names any
	// descendant (the gate step), or when an open decision/escalation lives in
	// the subtree (the inverse safety net).
	for _, b := range staleIP {
		if b == nil || (b.Type != "molecule" && b.Type != "formula") {
			continue
		}
		sub, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Parent:   b.ID,
			Statuses: []string{"open", "in_progress"},
			Limit:    500,
		})
		if err != nil || sub == nil {
			continue
		}
		for _, child := range sub.Beads {
			if child == nil || child.ID == b.ID {
				continue
			}
			if handle, ok := handleByReqTask[child.ID]; ok {
				consider(b.ID, handle)
			}
			if child.Type == "decision" || child.Type == "escalation" {
				consider(b.ID, child.ID)
			}
		}
	}
	return parked
}

// staleClassLabel is the compact JSON/enum token for a stale class.
func staleClassLabel(c staleClass) string {
	switch c {
	case staleHardClaimed:
		return "hard-claimed"
	case staleRespawnedSuccessorActive:
		return "respawned-successor-active"
	case staleEscalationParked:
		return "escalation-parked"
	case staleAwaitsRelease:
		return "awaits-release"
	default:
		return "claimable"
	}
}

// printStaleSections renders the four stale buckets as separate, clearly-labeled
// groups so a triaging gardener never treats a hard-claimed, successor-occupied,
// or escalation-parked bead as free to grab. Claimable is listed first (the
// actionable bucket); respawned-successor-active carries an explicit ⚠ danger
// marker; escalation-parked names the open operator handle it waits on.
func printStaleSections(w io.Writer, entries []staleEntry, spawners map[string]string) {
	var claimable, hardClaimed, respawned, parked []staleEntry
	for _, e := range entries {
		switch e.Class {
		case staleHardClaimed:
			hardClaimed = append(hardClaimed, e)
		case staleRespawnedSuccessorActive:
			respawned = append(respawned, e)
		case staleEscalationParked, staleAwaitsRelease:
			parked = append(parked, e)
		default:
			claimable = append(claimable, e)
		}
	}
	first := true
	section := func(heading string, es []staleEntry, danger bool) {
		if len(es) == 0 {
			return
		}
		if !first {
			fmt.Fprintln(w)
		}
		first = false
		fmt.Fprintf(w, "%s (%d):\n\n", heading, len(es))
		for _, e := range es {
			printNewsBeadWithSpawner(e.Bead, spawners)
			switch {
			case danger && e.Successor != "":
				fmt.Fprintf(w, "      ⚠ successor @%s is ACTIVE on this seat/task — do NOT claim; coordinate first\n", e.Successor)
			case e.Class == staleHardClaimed && e.Successor != "":
				fmt.Fprintf(w, "      ↳ still held by live @%s — kd claim will fail\n", e.Successor)
			case e.Class == staleEscalationParked && e.Handle != "":
				fmt.Fprintf(w, "      ↳ parked on operator handle %s (not closed) — do NOT reap; it closes when the handle is executed\n", e.Handle)
			case e.Class == staleAwaitsRelease:
				fmt.Fprintf(w, "      ↳ awaits its release (awaits_release=true) — do NOT reap; the deployment that carries it closes it\n")
			}
		}
	}
	section("Stale — claimable (agent gone, reapable)", claimable, false)
	section("Stale — parked (awaiting operator or release, do NOT reap)", parked, false)
	section("Stale — hard-claimed (live holder, cannot claim)", hardClaimed, false)
	section("Stale — respawned-successor-active (DANGER)", respawned, true)
}

// capBeads truncates beads to at most `limit` rows. A limit <= 0 means
// "unlimited" and returns the slice unchanged — matching the fetchLimit guard
// in runNews, which only over-fetches when limit > 0. This is the trim half of
// the news over-fetch: the daemon queries pull up to limit*overFetch rows so
// the client-side filter passes can backfill dropped slots, and capBeads then
// restores the per-section ceiling so a section never renders more than the
// caller asked for (ka-0000003808; the `gb ready` analogue is ka-0000004559).
func capBeads(beads []*beadsapi.BeadDetail, limit int) []*beadsapi.BeadDetail {
	if limit > 0 && len(beads) > limit {
		return beads[:limit]
	}
	return beads
}

func filterOutAssignee(beads []*beadsapi.BeadDetail, actorName string) []*beadsapi.BeadDetail {
	actorLower := strings.ToLower(actorName)
	var filtered []*beadsapi.BeadDetail
	for _, b := range beads {
		assigneeLower := strings.ToLower(b.Assignee)
		if assigneeLower == actorLower {
			continue
		}
		if strings.HasSuffix(assigneeLower, "/"+actorLower) {
			continue
		}
		filtered = append(filtered, b)
	}
	return filtered
}

// newsJSONOutput is the structured --json shape emitted by `gb news`.
// Downstream tooling (e.g. gardener liveness blocks) relies on this shape:
//
//	gb news --json | jq -r '.agents[] | select(.alive) | .name'
//
// alive SEMANTICS + BOUND (ka-0000000637). `alive` is a bead-derived liveness
// signal: an agent reads alive when its open type=agent bead is non-terminal
// (agent_state ∉ {done,failed,rate_limited}) AND was updated within
// newsAliveFreshnessWindow (default 5m). The freshness clause is what bounds
// staleness: a live pod's bead is refreshed by the controller every
// COOP_SYNC_INTERVAL (default 60s), so a despawned agent's `alive` self-heals to
// false within the window — NOT after the 2h stale reaper as it did before. The
// guarantee is therefore: `alive:true` may lag pod death by at most one
// freshness window (~5m). For gating that must be authoritative RIGHT NOW (never
// a 5m window), do not trust `alive` alone — cross-check ground truth: the
// agent's pod (`kubectl get pods -A -l gardenio.io/agent=<name>`, or the
// pod-authoritative `gb fleet`/`gb probe` paths) or the type=agent bead's
// agent_state/status. See buildActiveAgents.
//
// AGENT ROW FIELDS (M4, ka-0000000465). Each `agents[]` row now carries `role`,
// `status` (the agent_state field), `pod_phase`, and `last_heartbeat` (the bead
// updated_at, RFC3339) — previously null on 100% of rows, they are the fields
// release gardeners actually query. Prewarmed-pool placeholders and the caller's
// own row are excluded from the roster by default (the latter shown with --all).
// Each `stale[]` row carries `stale_class` (claimable | hard-claimed |
// respawned-successor-active) and, for the last two, `successor` — the live agent
// holding the seat/task, so a caller never treats a hard-claimed or
// successor-occupied bead as free to grab.
type newsJSONOutput struct {
	Window        string                 `json:"window"`
	Relevant      *newsJSONRelevance     `json:"relevant,omitempty"`
	Agents        []newsJSONAgent        `json:"agents"`
	InProgress    []newsJSONBead         `json:"in_progress"`
	Stale         []newsJSONBead         `json:"stale"`
	Unassigned    []newsJSONBead         `json:"unassigned"`
	Closed        []newsJSONBead         `json:"closed"`
	Decisions     []newsJSONDecision     `json:"decisions"`
	Artifacts     []newsJSONArtifact     `json:"artifacts"`
	OpenDecisions []newsJSONOpenDecision `json:"open_decisions"`
	// OpenEscalations are the open P0/P1 escalations that lead the digest
	// un-suppressibly (ka-0000002114). Always emitted (never gated by
	// --active-only/--stale-only) so a machine consumer scoring pending
	// operator actions gets the pivotal escalations in the same call as
	// open_decisions, not via a separate `gb escalation list` round-trip.
	OpenEscalations []newsJSONOpenEscalation `json:"open_escalations"`
	// OpenEscalationsTotal / OpenEscalationsTruncated mirror the open_decisions
	// pair: the pre-cap in-scope P0/P1 count and whether the cap dropped rows.
	OpenEscalationsTotal     int  `json:"open_escalations_total,omitempty"`
	OpenEscalationsTruncated bool `json:"open_escalations_truncated,omitempty"`
	// OpenDecisionsTotal is the daemon's total open-decision match count, and
	// OpenDecisionsTruncated reports whether capBeads dropped rows from
	// OpenDecisions. The rendered path already prints "… and N more open
	// decision(s) not shown" (ka-0000005518); without these the JSON path
	// cannot distinguish "50 exist" from "50 of 75 shown", so an agent scoring
	// the queue from --json silently under-counts and reads the cap as a drain
	// (ka-0000007375). Omitted when nothing was truncated.
	OpenDecisionsTotal     int  `json:"open_decisions_total,omitempty"`
	OpenDecisionsTruncated bool `json:"open_decisions_truncated,omitempty"`
	// Every capBeads-truncated section mirrors the open_decisions pair above:
	// a <section>_total + <section>_truncated so a --json consumer can tell "N
	// exist" from "N of M shown" (ka-0000005034). The <section>_total is the
	// section's IN-SCOPE row count immediately BEFORE capBeads — i.e. after the
	// recently-closed window, exclusive-project, and filter-out-self passes have
	// run — NOT the raw daemon match count. The daemon Total for the windowed
	// sections (closed/decisions/artifacts) counts every closed bead ever, so it
	// would flag truncated on every call and print a total of thousands against a
	// 12h window; the pre-cap length is the only count that means "rows the cap
	// dropped from what you'd otherwise see". truncated is set (and total emitted)
	// only when the cap actually dropped rows — total > the rendered row count —
	// so an un-truncated digest keeps its existing shape via omitempty. The three
	// in_progress sub-sections (in_progress/stale/unassigned) share one query
	// whose combined cap runs before the active/stale/unassigned split, so a full
	// bucket can starve a sibling: each carries its own pre-cap bucket count so
	// that starvation is visible per section, not just in aggregate.
	InProgressTotal     int  `json:"in_progress_total,omitempty"`
	InProgressTruncated bool `json:"in_progress_truncated,omitempty"`
	StaleTotal          int  `json:"stale_total,omitempty"`
	StaleTruncated      bool `json:"stale_truncated,omitempty"`
	UnassignedTotal     int  `json:"unassigned_total,omitempty"`
	UnassignedTruncated bool `json:"unassigned_truncated,omitempty"`
	ClosedTotal         int  `json:"closed_total,omitempty"`
	ClosedTruncated     bool `json:"closed_truncated,omitempty"`
	DecisionsTotal      int  `json:"decisions_total,omitempty"`
	DecisionsTruncated  bool `json:"decisions_truncated,omitempty"`
	ArtifactsTotal      int  `json:"artifacts_total,omitempty"`
	ArtifactsTruncated  bool `json:"artifacts_truncated,omitempty"`
}

// newsSectionTotals carries each capBeads-truncated section's IN-SCOPE row count
// measured immediately BEFORE the cap (after the window/project/self filters).
// emitNewsJSON compares each against the rendered array length to set the
// per-section <section>_total / <section>_truncated fields. The in_progress trio
// (InProgress/Stale/Unassigned) is the split of the PRE-cap ipBeads set, so each
// bucket's count reflects what it would show if the shared cap had not fired.
type newsSectionTotals struct {
	InProgress int
	Stale      int
	Unassigned int
	Closed     int
	Decisions  int
	Artifacts  int
}

type newsJSONOpenDecision struct {
	ID        string `json:"id"`
	Prompt    string `json:"prompt"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	DupGroup  string `json:"dup_group"`
	DupCount  int    `json:"dup_count"`
	// SubjectToken/RelatedIDs surface the same-subject cluster (distinct forks
	// about one target that are NOT exact duplicates — ka-0000000551). Empty
	// when the decision shares no subject with another open decision.
	SubjectToken string   `json:"subject_token,omitempty"`
	RelatedIDs   []string `json:"related_ids,omitempty"`
}

// newsJSONOpenEscalation is one open P0/P1 escalation in the operator-action
// block (ka-0000002114). Priority + operation_type let a machine consumer rank
// and route the ask without a follow-up `kd show`.
type newsJSONOpenEscalation struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Priority      int    `json:"priority"`
	OperationType string `json:"operation_type,omitempty"`
	CreatedBy     string `json:"created_by,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
}

type newsJSONAgent struct {
	Name      string `json:"name"`
	Alive     bool   `json:"alive"`
	Role      string `json:"role,omitempty"`
	Status    string `json:"status,omitempty"`
	PodPhase  string `json:"pod_phase,omitempty"`
	SpawnedBy string `json:"spawned_by,omitempty"`
	// LastHeartbeat is the agent bead's updated_at (RFC3339) — the controller
	// bumps it every sync cadence for a live pod, so it is the freshness signal
	// gardeners previously shelled to kubectl for (M4, ka-0000000465).
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
}

type newsJSONBead struct {
	ID        string `json:"id"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title"`
	Assignee  string `json:"assignee,omitempty"`
	Alive     bool   `json:"alive"`
	SpawnedBy string `json:"spawned_by,omitempty"`
	// StaleClass is populated only on rows in the `stale` array: one of
	// claimable / hard-claimed / respawned-successor-active / escalation-parked /
	// awaits-release (M4, ka-0000007805; gd-aaGqLb2cUk). Empty on live, closed, and unassigned rows.
	StaleClass string `json:"stale_class,omitempty"`
	// Successor is the live agent holding the seat/task, set on hard-claimed and
	// respawned-successor-active stale rows so a caller can coordinate instead of
	// colliding.
	Successor string `json:"successor,omitempty"`
	// ParkedOn is the open escalation/decision bead id that parks this row, set
	// only on escalation-parked stale rows so a machine consumer can subtract it
	// from a reap sweep and link straight to the operator handle (ka-0000007805).
	ParkedOn string `json:"parked_on,omitempty"`
}

type newsJSONDecision struct {
	ID          string `json:"id"`
	Prompt      string `json:"prompt"`
	Chosen      string `json:"chosen,omitempty"`
	RespondedBy string `json:"responded_by,omitempty"`
}

type newsJSONArtifact struct {
	ID         string `json:"id"`
	ReportType string `json:"report_type,omitempty"`
	Title      string `json:"title"`
	DecisionID string `json:"decision_id,omitempty"`
	CreatedBy  string `json:"created_by,omitempty"`
}

// emitNewsJSON renders structured news output to stdout. Filter flags
// (--stale-only, --active-only) shape the bead arrays but the agents list
// and other sections are emitted unchanged so callers always get a stable
// top-level shape to query.
func emitNewsJSON(
	windowStr string,
	roster []newsAgentInfo,
	agentSpawner map[string]string,
	activeIP, staleIP []*beadsapi.BeadDetail,
	staleClass []staleEntry,
	unassignedIP, closedBeads, decisions, reports []*beadsapi.BeadDetail,
	openDec []openDecisionGroup,
	openDecTotal int,
	openEscalations []*beadsapi.BeadDetail,
	openEscTotal int,
	totals newsSectionTotals,
	relevance *relevanceDigest,
	staleOnly, activeOnly, showAll bool,
) error {
	activeAgents := make(map[string]bool)
	for _, a := range roster {
		if a.Alive {
			activeAgents[a.Name] = true
		}
	}
	out := newsJSONOutput{
		Window:          windowStr,
		Relevant:        relevanceToJSON(relevance),
		Agents:          buildNewsJSONAgents(roster, staleClass, showAll),
		InProgress:      []newsJSONBead{},
		Stale:           []newsJSONBead{},
		Unassigned:      []newsJSONBead{},
		Closed:          []newsJSONBead{},
		Decisions:       []newsJSONDecision{},
		Artifacts:       []newsJSONArtifact{},
		OpenDecisions:   []newsJSONOpenDecision{},
		OpenEscalations: []newsJSONOpenEscalation{},
	}
	// Mirror the rendered truncation marker (see the printed "… and N more open
	// decision(s) not shown" above) onto the machine path. groupOpenDecisions is
	// 1:1 — it annotates dup siblings, it does not collapse rows — so
	// len(openDec) is the rendered row count and comparing it against the
	// daemon's total is the same signal the pretty path uses (ka-0000007375).
	if openDecTotal > len(openDec) {
		out.OpenDecisionsTotal = openDecTotal
		out.OpenDecisionsTruncated = true
	}
	// Open P0/P1 escalations — always emitted (un-suppressible, ka-0000002114),
	// mirroring the open_decisions truncation pair.
	for _, b := range openEscalations {
		var createdAt string
		if !b.CreatedAt.IsZero() {
			createdAt = b.CreatedAt.UTC().Format(time.RFC3339)
		}
		out.OpenEscalations = append(out.OpenEscalations, newsJSONOpenEscalation{
			ID:            b.ID,
			Title:         b.Title,
			Priority:      b.Priority,
			OperationType: b.Fields["operation_type"],
			CreatedBy:     b.CreatedBy,
			CreatedAt:     createdAt,
		})
	}
	if openEscTotal > len(openEscalations) {
		out.OpenEscalationsTotal = openEscTotal
		out.OpenEscalationsTruncated = true
	}
	// Index the stale classification by bead ID so the stale array can carry
	// stale_class + successor on each row.
	classByID := make(map[string]staleEntry, len(staleClass))
	for _, e := range staleClass {
		classByID[e.Bead.ID] = e
	}
	for _, od := range openDec {
		prompt := od.Bead.Fields["prompt"]
		if prompt == "" {
			prompt = od.Bead.Title
		}
		var createdAt string
		if !od.Bead.CreatedAt.IsZero() {
			createdAt = od.Bead.CreatedAt.UTC().Format(time.RFC3339)
		}
		out.OpenDecisions = append(out.OpenDecisions, newsJSONOpenDecision{
			ID:           od.Bead.ID,
			Prompt:       prompt,
			CreatedBy:    od.Bead.CreatedBy,
			CreatedAt:    createdAt,
			DupGroup:     od.BucketKey,
			DupCount:     od.DupCount,
			SubjectToken: od.SubjectToken,
			RelatedIDs:   od.RelatedIDs,
		})
	}

	if !staleOnly {
		for _, b := range activeIP {
			out.InProgress = append(out.InProgress, beadToNewsJSON(b, true, agentSpawner))
		}
	}
	if !activeOnly {
		for _, b := range staleIP {
			nb := beadToNewsJSON(b, false, agentSpawner)
			if e, ok := classByID[b.ID]; ok {
				nb.StaleClass = staleClassLabel(e.Class)
				nb.Successor = e.Successor
				nb.ParkedOn = e.Handle
			}
			out.Stale = append(out.Stale, nb)
		}
	}
	if !staleOnly && !activeOnly {
		for _, b := range unassignedIP {
			out.Unassigned = append(out.Unassigned, beadToNewsJSON(b, false, agentSpawner))
		}
		for _, b := range closedBeads {
			alive := b.Assignee == "" || activeAgents[strings.ToLower(b.Assignee)]
			out.Closed = append(out.Closed, beadToNewsJSON(b, alive, agentSpawner))
		}
		for _, b := range decisions {
			prompt := b.Fields["prompt"]
			if prompt == "" {
				prompt = b.Title
			}
			chosen := b.Fields["chosen"]
			if chosen != "" {
				if label := resolveOptionLabel(b.Fields["options"], chosen); label != "" {
					chosen = label
				}
			}
			out.Decisions = append(out.Decisions, newsJSONDecision{
				ID:          b.ID,
				Prompt:      prompt,
				Chosen:      chosen,
				RespondedBy: b.Fields["responded_by"],
			})
		}
		for _, b := range reports {
			out.Artifacts = append(out.Artifacts, newsJSONArtifact{
				ID:         b.ID,
				ReportType: b.Fields["report_type"],
				Title:      b.Title,
				DecisionID: b.Fields["decision_id"],
				CreatedBy:  b.CreatedBy,
			})
		}
	}

	// Mirror the open_decisions truncation pair onto every capBeads-truncated
	// section (ka-0000005034). Each total is the section's pre-cap in-scope count
	// (see newsSectionTotals); set the pair only when it exceeds the rendered row
	// count so the cap actually dropped rows — an un-truncated section keeps its
	// existing shape via omitempty. Gate each on the SAME filter flags that
	// populate its array, so a section the filter suppressed (empty by design)
	// never reads a phantom truncated:true from a stale pre-cap total.
	if !staleOnly && totals.InProgress > len(out.InProgress) {
		out.InProgressTotal = totals.InProgress
		out.InProgressTruncated = true
	}
	if !activeOnly && totals.Stale > len(out.Stale) {
		out.StaleTotal = totals.Stale
		out.StaleTruncated = true
	}
	if !staleOnly && !activeOnly {
		if totals.Unassigned > len(out.Unassigned) {
			out.UnassignedTotal = totals.Unassigned
			out.UnassignedTruncated = true
		}
		if totals.Closed > len(out.Closed) {
			out.ClosedTotal = totals.Closed
			out.ClosedTruncated = true
		}
		if totals.Decisions > len(out.Decisions) {
			out.DecisionsTotal = totals.Decisions
			out.DecisionsTruncated = true
		}
		if totals.Artifacts > len(out.Artifacts) {
			out.ArtifactsTotal = totals.Artifacts
			out.ArtifactsTruncated = true
		}
	}

	// Route through printJSON so a --jq filter projects the news envelope
	// in-process; without --jq this is byte-identical to the prior indented
	// encoder output (both MarshalIndent and the encoder HTML-escape and add a
	// trailing newline).
	printJSON(out)
	return nil
}

// buildNewsJSONAgents renders the roster as the JSON agents array, carrying
// role / status (agent_state) / pod_phase / last_heartbeat on every row so the
// field release gardeners query is never null again (M4, ka-0000000465). Gone
// assignees of stale beads that are NOT in the roster (their agent bead has
// already closed, so they never appear as an open type=agent bead) are still
// appended as alive=false rows so `.agents[] | select(.alive)` liveness lists and
// the prior "stale assignee shows up in agents" contract both hold. The
// caller-scoped self row and prewarmed placeholders are already excluded upstream
// in buildNewsAgents.
func buildNewsJSONAgents(roster []newsAgentInfo, staleClass []staleEntry, showSelf bool) []newsJSONAgent {
	seen := make(map[string]bool)
	out := []newsJSONAgent{}
	for _, a := range roster {
		// Hide the caller's own row from the displayed roster unless --all — self
		// is not a teammate to coordinate with (M4, ka-0000000465). Self stays in
		// the roster for classification; only display drops it.
		if a.IsSelf && !showSelf {
			continue
		}
		if seen[a.Name] {
			continue
		}
		seen[a.Name] = true
		ja := newsJSONAgent{
			Name:      a.Name,
			Alive:     a.Alive,
			Role:      a.Role,
			Status:    a.State,
			PodPhase:  a.PodPhase,
			SpawnedBy: a.SpawnedBy,
		}
		if !a.UpdatedAt.IsZero() {
			ja.LastHeartbeat = a.UpdatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, ja)
	}
	// A stale bead's assignee whose agent bead has closed is absent from the
	// roster; surface it as a dead row so callers that walk .agents for zombie
	// names still see it.
	for _, e := range staleClass {
		name := strings.ToLower(e.Bead.Assignee)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, newsJSONAgent{Name: name, Alive: false})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func beadToNewsJSON(b *beadsapi.BeadDetail, alive bool, spawners map[string]string) newsJSONBead {
	nb := newsJSONBead{
		ID:       b.ID,
		Type:     b.Type,
		Title:    b.Title,
		Assignee: b.Assignee,
		Alive:    alive,
	}
	if b.Assignee != "" && spawners != nil {
		nb.SpawnedBy = spawners[strings.ToLower(b.Assignee)]
	}
	return nb
}

func filterRecentlyClosed(beads []*beadsapi.BeadDetail, window time.Duration) []*beadsapi.BeadDetail {
	cutoff := time.Now().Add(-window)
	var filtered []*beadsapi.BeadDetail
	for _, b := range beads {
		if b.UpdatedAt.IsZero() || b.UpdatedAt.After(cutoff) {
			filtered = append(filtered, b)
		}
	}
	return filtered
}
