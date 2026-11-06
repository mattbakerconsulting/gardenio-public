package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/runnerreg"

	"github.com/spf13/cobra"
)

// readyThreadLabel is the bare thread (wish) head label, mirrored from
// bridge.threadKindLabel — that constant is unexported in an internal package,
// and `gb` must not import the bridge just to read one string. Kept in sync by
// TestBuildReadyQueryExcludesThreadLabel + the bridge-side thread_bead tests.
const readyThreadLabel = "thread"

// readySentinelLabel marks a wake-keeper SENTINEL bead: a permanently-open
// kind=issue child whose only job is to satisfy a standing gardener's preflight
// gate (that gate counts >0 open ISSUE-kind children of the gardener's epic, so
// the sentinel cannot become a kind=data doc that leaves ready by construction —
// ka-0000000675). It is not claimable work: its own body forbids closing it, and
// the only "completion" would sever the gardener's wake path. Such beads must
// never render as a fake ready row, so `gb ready` drops this label by default
// exactly as it drops readyThreadLabel — same class of queue noise, same fix, and
// -l sentinel / --label-any sentinel still opts back in. This fixes the whole
// class fleet-wide, since every standing gardener uses the same open-issue-child
// trick to stay lit.
const readySentinelLabel = "sentinel"

// readyDocLabel marks a `kind:doc` STANDING PROCEDURE: a doc bead — a gardener
// cycle procedure, runbook, or other live reference text — that a perpetual
// gardener reads every cycle via `kd show`. Such a bead is deliberately kept
// status=in_progress (or open) forever so the gardener's wake path stays lit and
// the procedure reads as "the current one", NOT because there is a code feature
// to build against it. It is not claimable build/feature work, and a crew
// dispatched onto it can only stand down or (worse) silently mutate a running
// gardener's live procedure.
//
// This is the durable fix for ka-0000000054: the delivery/build gardener
// dispatches from `gb ready`, and ka-0000000327 (the live gardener-release-owner
// cycle procedure) is a `type=task`, `kind=issue`, `area:release`, `kind:doc`
// bead that looked dispatchable — two crew were misrouted onto it within ~13
// minutes on 2026-08-21. Dropping the `kind:doc` label by default removes the
// whole class from the ready queue exactly as `readyThreadLabel` and
// `readySentinelLabel` drop their classes — same queue noise, same mechanism,
// and -l kind:doc / --label-any kind:doc still opts back in for anyone who
// genuinely wants to browse standing procedures.
const readyDocLabel = "kind:doc"

// readyDefaultLimit is the browsing default for a human TTY `gb ready`: a screen-
// sized page, not the whole backlog. It is registered as the --limit default and
// re-derived in readyOptionsFromFlags so the "was --limit explicitly set?" test
// (cmd.Flags().Changed) can tell an inherited default from an operator-typed 20.
const readyDefaultLimit = 20

// readyJSONFullLimit is the limit `gb ready --json` uses when the caller did NOT
// pass an explicit --limit. It is the daemon's max page size (maxListBeadsLimit,
// kbeads http_beads.go), so a single round-trip returns the entire ready set for
// programmatic consumers.
//
// Rationale (ka-0000006944): the 20-row default is a *browsing* affordance for a
// human scanning a terminal. A script that does `gb ready --json | jq '.[].id'`
// inherits that same 20 silently — the JSON output is a bare array with no total
// and no truncation marker (advice ka-0000003286 forbids an envelope), so a
// gardener dispatch lane reasoning over the result cannot tell "20 = the whole
// queue" from "20 = the first 2.3% of 868". Measured live 2026-08-01: bare
// `gb ready --json` returned 20 while the true ready count was 868, and a gardener
// concluded "nothing dispatchable" while hundreds of ready beads sat unclaimed.
// Treating --json as programmatic (full set by default) removes the false-empty
// hazard at the exact command every dispatch lane runs first; a human TTY view
// keeps the 20-row page. An explicit --limit still wins in either mode.
const readyJSONFullLimit = 10000

var readyCmd = &cobra.Command{
	Use:     "ready",
	Short:   "Show beads ready to work on (open, not blocked)",
	GroupID: "session",
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := readyOptionsFromFlags(cmd)
		if err != nil {
			return err
		}

		q := buildReadyQuery(opts)
		// An agent on a local runner sees only the work placed on it (spec
		// §7), which a post-filter finds, so it reads the whole ready set and
		// cuts the page after filtering.
		me, onRunner := readyRunner(opts)
		if onRunner {
			q.Limit = readyJSONFullLimit
		}

		result, err := daemon.ListBeadsFiltered(cmd.Context(), q)
		if err != nil {
			return fmt.Errorf("listing ready beads: %w", err)
		}

		beads := result.Beads
		// truncated tracks whether the printed set is a strict PREFIX of the full
		// ready set, so --json can carry a machine-readable truncation marker on
		// stderr (ka-0000006944). stdout must stay a clean bare array with no total
		// and no envelope (advice ka-0000003286), so the "is this complete?" signal
		// a gardener script needs cannot live in the payload — it goes to stderr.
		// Project narrowing is now applied server-side (buildReadyQuery's
		// ExclusiveProject), so the daemon returning fewer rows than it matched
		// (len(result.Beads) < result.Total, e.g. an explicit --limit below the
		// match count, or a >maxListBeadsLimit ready set) is the only truncation
		// signal — there is no client-side pass that could shorten the page.
		truncated := len(result.Beads) < result.Total
		// displayTotal is the honest queue size the footer reports. With the
		// server-side exclusive_project filter (ka-0000003411) result.Total is the
		// exact count of the project-exclusive ready queue — no client-side pass
		// narrows the set afterward, so it no longer overstates the claimable queue
		// by the dual-project rows a post-Limit pass would have dropped
		// (ka-0000001533).
		displayTotal := result.Total
		// A task whose PR merged and that waits on its release is open but has
		// no work for a crew (gd-fHyi9y1k5B); --all keeps it.
		if !opts.All {
			kept := dropAwaitingRelease(beads)
			displayTotal -= len(beads) - len(kept)
			beads = kept
		}
		switch {
		case onRunner:
			beads = filterForRunner(beads, me, me.Owner)
			displayTotal = len(beads)
			if opts.Limit > 0 && len(beads) > opts.Limit {
				beads = beads[:opts.Limit]
			}
			truncated = len(beads) < displayTotal || len(result.Beads) < result.Total
		case opts.AgentContext:
			kept := dropLocallyPlaced(beads)
			displayTotal -= len(beads) - len(kept)
			beads = kept
		}
		var holds map[string]*overlapHold
		if opts.HoldOverlaps {
			prs, err := planOpenPRs(cmd.Context(), opts.Plan)
			if err != nil {
				return err
			}
			holds = overlapHolds(beads, prs, readyHoldNow())
			fmt.Fprintf(os.Stderr, "note: --hold-overlaps read %d open PRs of plan %s; %d ready steps held\n", len(prs), opts.Plan, len(holds))
		}
		if jsonOutput {
			// stdout is the bare array consumers pipe into jq; the truncation
			// signal (ka-0000006944) rides stderr so a script can `2>` capture it
			// or ignore it without corrupting the JSON on stdout.
			if truncated {
				fmt.Fprintf(os.Stderr, "note: gb ready returned %d of %d ready beads (truncated); pass a larger --limit for the full set\n", len(beads), displayTotal)
			}
			if opts.HoldOverlaps {
				printJSON(readyRows(beads, holds))
			} else {
				printJSON(beads)
			}
		} else if len(beads) == 0 {
			fmt.Println("No beads ready to work on")
		} else {
			// A ready bead is status=open, so a NON-EMPTY assignee is always
			// anomalous: nobody live is actively working an open bead. The three
			// shapes it takes are all NOT straightforwardly claimable — a dead
			// agent's stranded pre-claim (reapable), a human operator's genuine
			// ownership of operator-only work, or a load-bearing sentinel string.
			// `gb news` resolves assignee liveness (splitActiveStale) to build its
			// active/stale split; `gb ready` did not, so it rendered these rows as
			// silently claimable — the entire apparent claimable queue was one such
			// row (ka-0000005196). Resolve liveness the same way and annotate inline
			// so a reader sees WHY a row is not theirs; strip NOTHING, because two of
			// the three shapes must not be reaped. Resolved lazily: only when at
			// least one printed row carries an assignee, so the all-unassigned common
			// case pays no extra round-trip.
			live := liveAgentSetForReady(cmd.Context(), beads)
			for _, b := range beads {
				line := fmt.Sprintf("  %s  [%s]  %s  %s", b.ID, b.Type, withArms(b.Heraldry, b.Title), b.Assignee)
				if len(b.Labels) > 0 {
					line += "  {" + strings.Join(b.Labels, ",") + "}"
				}
				// PARKED badge: an open operator decision names this row in its
				// prose, so it is awaiting an operator choice — not free to claim
				// or mutate (ka-0000003797). Suffix the row so a scanner sees it
				// without opening the bead.
				if b.ParkedOnDecision {
					line += "  " + readyParkedBadge(b.ParkedOnDecisionBeadID)
				}
				fmt.Println(line)
				if note := readyAssigneeNote(b.Assignee, live); note != "" {
					fmt.Printf("      %s\n", note)
				}
				if b.ParkedOnDecision {
					fmt.Printf("      %s\n", readyParkedNote(b.ParkedOnDecisionBeadID))
				}
				if h := holds[b.ID]; h != nil {
					fmt.Printf("      %s\n", readyHoldNote(h, readyHoldNow()))
				}
			}
			fmt.Printf("\n%d beads (%d total)\n", len(beads), displayTotal)
			fmt.Println("\nRun `kd claim <id>` before starting work on any bead.")
		}

		// Satisfy the decision gate so the stop hook doesn't block after
		// the agent checks for more work. This prevents the stop-loop where
		// persistent agents cycle between "gb ready" and blocked stops.
		satisfyGateOnReady(cmd.Context())

		return nil
	},
}

// liveAgentSetForReady returns the set of live agent names (lowercased) used to
// classify a ready bead's assignee, or nil when no printed row carries an
// assignee. It reuses the exact roster + liveness rule `gb news` builds
// (buildActiveAgents over the open type=agent beads, same freshness window), so
// a bead reported stale by `gb news` and ready by `gb ready` agree on whether
// its assignee is a live agent. The type=agent fetch is the single extra
// round-trip the annotation costs, and it is skipped entirely in the
// all-unassigned common case: with no assignees to classify there is nothing to
// look up. Fails open (returns an empty set) on any daemon error — a lookup
// failure must never hide ready work, only drop the annotation.
func liveAgentSetForReady(ctx context.Context, beads []*beadsapi.BeadDetail) map[string]bool {
	anyAssigned := false
	for _, b := range beads {
		if b.Assignee != "" {
			anyAssigned = true
			break
		}
	}
	if !anyAssigned {
		return nil
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open"},
		Limit:    200,
	})
	if err != nil {
		return map[string]bool{} // fail open: no annotation, never hide work
	}
	active, _ := buildActiveAgents(result.Beads, time.Now(), newsAliveFreshnessWindowValue())
	return active
}

// readyAssigneeNote returns the inline annotation for a ready bead's assignee, or
// "" when the row is genuinely claimable (no assignee). A ready bead is
// status=open, so a non-empty assignee is never a live agent actively working it
// — it is one of two non-claimable shapes:
//   - a dead agent whose pre-claim was stranded (it exists in the live-agent set?
//     no) — reapable, so `kd claim` will work once the reaper releases it, but a
//     reader must know the current claim is a zombie, not a live teammate;
//   - a human operator or sentinel string that was never an agent — genuine
//     ownership / a load-bearing hold that an agent must NOT grab.
//
// The live set cannot by itself tell a dead agent from a never-an-agent name
// (both are simply absent), so the note is deliberately conservative: it flags
// the row as not-live-owned and tells the reader to check before claiming,
// rather than asserting a reap that two of the three real cases would make wrong
// (ka-0000005196).
func readyAssigneeNote(assignee string, live map[string]bool) string {
	if assignee == "" {
		return ""
	}
	if live[strings.ToLower(assignee)] {
		// A live agent holding an OPEN bead is unusual but not our concern to
		// reap — surface it plainly so the reader coordinates rather than grabs.
		return fmt.Sprintf("↳ assignee @%s is a live agent — coordinate before claiming", assignee)
	}
	return fmt.Sprintf("⚠ assignee %q is not a live agent (dead agent, human, or sentinel) — verify before claiming; do not assume it is free", assignee)
}

// readyParkedBadge is the compact inline suffix marking a ready row as PARKED
// behind an open operator decision (ka-0000003797). It names the decision id so
// a reader can jump straight to it. Kept short so it rides on the same line as
// the bead; the fuller explanation is readyParkedNote below.
func readyParkedBadge(decisionID string) string {
	if decisionID == "" {
		return "[parked: open decision]"
	}
	return fmt.Sprintf("[parked: %s]", decisionID)
}

// readyParkedNote is the indented second-line explanation for a parked ready
// row. It tells the reader the operator already owns a question about this bead,
// so claiming or mutating it now would preempt that choice — the exact failure
// bug ka-0000007469 recorded when 8 parked rows rendered as ordinary claimable
// work. Conservative wording: verify the decision before acting, do not assume
// the row is free.
func readyParkedNote(decisionID string) string {
	if decisionID == "" {
		return "⚠ PARKED behind an open operator decision — verify before claiming or mutating; the operator may resolve it as \"proceed\" or otherwise"
	}
	return fmt.Sprintf("⚠ PARKED behind open decision %s — an operator already owns a question naming this bead; check `kd show %s` before claiming or mutating", decisionID, decisionID)
}

// readyQuerySort orders ready beads by priority first (P0=0 sorts to the top,
// ascending priority number), then by newest-first within a priority band.
// Priority MUST be the primary key: ordering by -created_at alone sank high-
// priority work below the default Limit of 20 as soon as 20 newer beads landed,
// so a P0 task filed early was invisible to agents browsing the default ready
// set (ka-0000001517). The store's parseSortClause supports this composite sort
// and the auto-assign / index-rail ready paths already lead with priority.
const readyQuerySort = "priority,-created_at"

// resolvePlanParent collapses the --plan flag and its --parent alias into a
// single effective subtree-scope id. Both flags drive the identical parent-child
// subtree query (`kd list` exposes the same capability as --parent, hence the
// alias for flag-parity — ka-0000006275). If both are set to the same id (or one
// is empty) the non-empty value wins; if they are set to different ids we error
// rather than silently pick one.
func resolvePlanParent(plan, parent string) (string, error) {
	switch {
	case plan == "":
		return parent, nil
	case parent == "" || parent == plan:
		return plan, nil
	default:
		return "", fmt.Errorf("--plan (%s) and --parent (%s) are aliases and cannot be set to different ids", plan, parent)
	}
}

// readyOptions carries `gb ready`'s resolved flag values into buildReadyQuery.
// It replaced an 11-positional-parameter signature: the list had grown three
// adjacent bools (allProjects, includeScheduled, and now unassigned) plus four
// adjacent []string label slices, where a transposed pair of call-site arguments
// type-checks cleanly and silently changes which beads the ready queue returns.
// Named fields make every call site self-describing and additive.
type readyOptions struct {
	Types        []string
	ExcludeTypes []string
	Assignee     string
	// Unassigned restricts the queue to beads nobody has claimed. Mutually
	// exclusive with Assignee (validated at the flag layer).
	Unassigned       bool
	Project          string
	Limit            int
	AllProjects      bool
	IncludeScheduled bool
	Labels           []string
	LabelsAny        []string
	ExcludeLabels    []string
	// Plan is the already-resolved --plan/--parent subtree id (see
	// resolvePlanParent), not the raw flag pair.
	Plan string
	// All restores the exact pre-leaf-default behavior: no implicit category
	// scope, no epic/molecule exclusion. It is the escape hatch a script relying
	// on today's flat top-N depends on (--all). It wins over every implicit
	// default below but does NOT remove the explicit -t/-l filters.
	All bool
	// Categories, when non-empty, sets the daemon's materialized-category filter
	// explicitly (--category intake,intermediate,...). An explicit --category
	// disables the implicit leaf default — the caller has stated the scope.
	Categories []string
	// IncludeEpics keeps container types (epic/molecule) in the leaf default
	// view (--include-epics). No effect outside the leaf default.
	IncludeEpics bool
	// AgentContext is true when this `gb ready` runs inside an agent pod — i.e.
	// an agent identity is present (BOAT_ROLE or KD_AGENT_ID set). It is what
	// gates the leaf-work default: an agent work-seeker gets claimable leaf work
	// by default; a human/local caller (no identity) gets the full labeled view.
	// Set from env in readyOptionsFromFlags so buildReadyQuery stays a pure,
	// daemon-free unit (honoring decision ka-0000006048 via an identity-aware MVP
	// that needs no role-bead migration).
	AgentContext bool
	// HoldOverlaps (--hold-overlaps, needs --plan) marks steps held behind an
	// open PR of the same plan that changes a file they name (ready_overlaps.go).
	HoldOverlaps bool
}

// leafDefaultCategory is the materialized category the agent leaf default scopes
// to: `intermediate` is the daemon's classification for claimable leaf work
// (kbeads category.go rules 7/8 — tasks/bugs under an intermediate parent or with
// no parent), excluding `intake` (thread/source-labeled work bound to another
// agent), `config`/`action`, and `artifact` (published deliverables). See audit
// ka-0000001484.
const leafDefaultCategory = "intermediate"

// readyContainerTypes are the container bead types the agent leaf default drops:
// you claim leaf tasks, not these. `intermediate` alone is insufficient because
// an epic parented under another epic classifies `intermediate` too (category.go
// rule 4), so the leaf default must ALSO exclude these types.
var readyContainerTypes = []string{"epic", "molecule"}

// Published-deliverable types (bundle/dashboard/rich-report/mr/merge-request) are
// excluded from the agent leaf default by the `category=intermediate` scope alone:
// DeriveCategory rule 3 + migration 043 (MR !7580 / ka-0000007274) give these
// types their own `artifact` category, so `category=intermediate` drops them BY
// CONSTRUCTION. A `readyDeliverableTypes` ExcludeTypes list was previously KEPT as
// an order-independent backstop (ka-0000006718) to cover legacy rows still
// carrying a stale `intermediate` stamp before the `kd admin backfill-category`
// sweep ran. That sweep has since run to completion and 0 `intermediate`-stamped
// deliverable rows remain fleet-wide (ka-0000004790, verified 2026-08-20), so the
// category filter and the former type filter now AGREE and the type list has
// degraded to dead code — retired here per that task's step 4.

// readyOptionsFromFlags reads `gb ready`'s flags into a readyOptions and applies
// the two cross-flag validations. Split out of RunE so the flag-to-options
// mapping is unit-testable without a live daemon: inside the closure it could
// only be reached by a real command execution, which is also why a transposed
// or forgotten flag here would not be caught by buildReadyQuery's tests.
func readyOptionsFromFlags(cmd *cobra.Command) (readyOptions, error) {
	var opts readyOptions
	opts.Types, _ = cmd.Flags().GetStringSlice("type")
	opts.ExcludeTypes, _ = cmd.Flags().GetStringSlice("exclude-type")
	opts.Assignee, _ = cmd.Flags().GetString("assignee")
	opts.Unassigned, _ = cmd.Flags().GetBool("unassigned")
	opts.Limit, _ = cmd.Flags().GetInt("limit")
	// Treat --json as programmatic: when the caller did NOT type an explicit
	// --limit, JSON output returns the full ready set instead of the human
	// browsing default of 20. The bare-array JSON carries no truncation marker
	// (advice ka-0000003286), so a script inheriting the 20-row default cannot
	// tell a complete result from a capped one — the exact false-empty that
	// under-dispatched a gardener lane over 868 ready beads (ka-0000006944). An
	// explicit --limit (Changed) still wins; the human TTY view keeps the page.
	if jsonOutput && !cmd.Flags().Changed("limit") {
		opts.Limit = readyJSONFullLimit
	}
	opts.Project, _ = cmd.Flags().GetString("project")
	opts.AllProjects, _ = cmd.Flags().GetBool("all-projects")
	opts.IncludeScheduled, _ = cmd.Flags().GetBool("include-scheduled")
	opts.Labels, _ = cmd.Flags().GetStringSlice("label")
	opts.LabelsAny, _ = cmd.Flags().GetStringSlice("label-any")
	opts.ExcludeLabels, _ = cmd.Flags().GetStringSlice("exclude-label")
	opts.All, _ = cmd.Flags().GetBool("all")
	opts.Categories, _ = cmd.Flags().GetStringSlice("category")
	opts.IncludeEpics, _ = cmd.Flags().GetBool("include-epics")
	// AgentContext gates the leaf-work default. An agent pod always has an
	// identity in its env (BOAT_ROLE injected by podmanager for every pod,
	// KD_AGENT_ID by the controller); a human/local `gb ready` has neither and
	// must fail OPEN to the full labeled view — never hide work from a human by
	// accident. This is the identity-aware MVP of decision ka-0000006048: it
	// captures the per-role default's intent (work-seekers see leaf work) with no
	// role-bead `class` migration on the hot path.
	opts.AgentContext = os.Getenv("BOAT_ROLE") != "" || os.Getenv("KD_AGENT_ID") != ""
	plan, _ := cmd.Flags().GetString("plan")
	parent, _ := cmd.Flags().GetString("parent")
	opts.HoldOverlaps, _ = cmd.Flags().GetBool("hold-overlaps")

	// --unassigned and --assignee are contradictory (no bead is both claimed by
	// X and claimed by nobody), so the pair can only ever return 0. Fail loudly:
	// a silent empty ready queue is indistinguishable from "no work available" —
	// the false-zero hazard --unassigned exists to remove.
	if opts.Unassigned && opts.Assignee != "" {
		return opts, fmt.Errorf("--unassigned and --assignee are mutually exclusive (a bead cannot be both assigned to %q and unassigned); drop one", opts.Assignee)
	}

	// --parent is an alias of --plan (flag-parity with `kd list --parent`); both
	// drive the same transitive parent-child subtree query. Resolve to a single
	// effective value, erroring only if both are set to conflicting ids.
	var err error
	opts.Plan, err = resolvePlanParent(plan, parent)
	if err != nil {
		return opts, err
	}
	if opts.HoldOverlaps && opts.Plan == "" {
		return opts, fmt.Errorf("--hold-overlaps needs --plan: it holds a step only for open PRs of the same plan")
	}
	return opts, nil
}

// buildReadyQuery constructs the ListBeadsQuery for `gb ready`. Extracted so the
// ordering and filter semantics are unit-testable without a live daemon.
func buildReadyQuery(o readyOptions) beadsapi.ListBeadsQuery {
	beadType, excludeType := o.Types, o.ExcludeTypes
	assignee, project, plan := o.Assignee, o.Project, o.Plan
	limit := o.Limit
	allProjects, includeScheduled := o.AllProjects, o.IncludeScheduled
	labels, labelsAny, excludeLabels := o.Labels, o.LabelsAny, o.ExcludeLabels
	// Default: hide scheduled-task beads (auto-spawned by the cron
	// scheduler each tick) since they re-spawn before any human can
	// claim them and only add noise to ready views (ka-0000006652).
	// Pass --include-scheduled to opt back in.
	// ExcludeTypes (--exclude-type) is the negative type filter, forwarded to
	// the same server-side `type NOT IN (...)` clause `kd list --exclude-type`
	// already uses. Without it, dropping the container types (epic / molecule)
	// from the ready queue meant `gb ready --json | jq 'map(select(.type != …))'`
	// — a client-side post-filter over a full page pull, where one wrong jq
	// field name silently returns an empty set indistinguishable from "no work"
	// (ka-0000007209; the type analogue of --exclude-label, ka-0000007103).
	// Narrow to the caller's project SERVER-SIDE for the un-anchored project-wide
	// view (not --all-projects, not a --plan subtree): the daemon's
	// exclusive_project filter drops any bead carrying a `project:<other>` label,
	// so LIMIT accounts for the narrowing and no eligible row is lost past the
	// limit. This is the honest fix (ka-0000003411) that replaces the client-side
	// filterExclusiveProject pass and its 4x over-fetch: result.Total is now exact
	// for the project-exclusive queue by construction. A --plan subtree is
	// ID-anchored (its steps may carry a different project label), so it is left
	// unnarrowed — mirroring `kd list --parent`.
	var exclusiveProject string
	if !allProjects && project != "" && plan == "" {
		exclusiveProject = project
	}
	q := beadsapi.ListBeadsQuery{
		Statuses:         []string{"open"},
		Types:            beadType,
		ExcludeTypes:     excludeType,
		Kinds:            []string{"issue"},
		Limit:            limit,
		ExclusiveProject: exclusiveProject,
		Sort:             readyQuerySort,
		NoOpenDeps:       true,
		ExcludeScheduled: !includeScheduled,
		// Surface each row's parent-epic id (and title when in scope) so a
		// dispatcher reasoning over `gb ready --json` can tell a leaf task under
		// an epic from a floating one — and group by epic — without an N+1
		// `kd show` per row (ka-0000006083). The daemon answers it as ONE batched
		// deps query keyed on the returned page, so the cost scales with the page,
		// not the ready set. Orphans / old daemons read back an empty parent_id.
		WithParent: true,
		// Surface whether an open operator decision NAMES each row in its prose —
		// the "parked behind an open decision" signal (ka-0000003797). A row
		// parked this way carries no blocking dep edge (a `--requesting-task`
		// decision writes a `blocks` edge that already keeps its target out of the
		// ready set via NoOpenDeps; a parent-child decision is caught by
		// blocked_on_decision), so it renders as ordinary claimable work — the
		// exact invisibility that made the 2026-08-20 preemption near-inevitable
		// (bug ka-0000007469). The daemon resolves it as one indexed correlated
		// subquery per row (bead_references EXISTS), so the cost scales with the
		// page, not the ready set. Rendered inline below and carried in --json.
		WithParkedOnDecision: true,
	}
	// Thread (wish) head beads are Slack CONVERSATIONS, not claimable work: a
	// thread agent is already bound to each one, and every gardener prompt tells
	// its reader never to touch `thread`/`source:slack` beads. They are the same
	// class of queue noise as the cron firing artifacts above and are excluded
	// for the same reason (ka-0000003760; precedent ka-0000006652 /
	// ka-0000007651, where firing beads flooded gb ready at 194/200 rows).
	//
	// This mattered because thread heads were also minted at P0 (see
	// threadBeadPriority in bridge/thread_bead.go). Measured 2026-07-27: 83 open
	// thread beads, all P0, made up 82 of the 106 P0 beads in the ready set — and
	// since `gb ready` sorts `priority,-created_at`, they sat above every real
	// P1-P3 bead. The default --limit 20 view returned 20 P0 rows and ZERO bugs,
	// while all 19 open gardenio bugs sat unassigned; adding --exclude-label thread
	// surfaced real work immediately. gardener-delivery dispatches from a bare
	// `gb ready`, so it could not reach an open bug. Excluding them is the durable
	// half of the fix: it holds even if some path mints a high-priority thread
	// bead again.
	//
	// -l thread / --label-any thread still opts back in: an explicit request for
	// the label wins over the implicit default, so `gb ready -l thread` is not
	// silently self-contradictory (it would otherwise return nothing).
	if !slices.Contains(labels, readyThreadLabel) && !slices.Contains(labelsAny, readyThreadLabel) {
		q.ExcludeLabels = append(q.ExcludeLabels, readyThreadLabel)
	}
	// Wake-keeper sentinels are permanently-open kind=issue beads that exist only
	// to keep a standing gardener's preflight gate lit; they are never claimable
	// work and their close is explicitly forbidden (ka-0000000675). Drop them by
	// default for the same reason and via the same mechanism as thread heads — an
	// explicit -l sentinel / --label-any sentinel still opts back in, so the query
	// never becomes self-contradictory.
	if !slices.Contains(labels, readySentinelLabel) && !slices.Contains(labelsAny, readySentinelLabel) {
		q.ExcludeLabels = append(q.ExcludeLabels, readySentinelLabel)
	}
	// `kind:doc` standing procedures (gardener cycle procedures, runbooks, live
	// reference text) are held open/in_progress forever so a perpetual gardener's
	// wake path stays lit — they are NOT claimable build/feature work. The
	// delivery/build gardener dispatches from `gb ready`, so a doc bead that
	// happens to be type=task/kind=issue looked dispatchable and misrouted crew
	// onto a running gardener's live procedure (ka-0000000054). Drop it by default
	// for the same reason and via the same mechanism as thread heads and
	// sentinels; an explicit -l kind:doc / --label-any kind:doc still opts back
	// in, so the query never becomes self-contradictory.
	if !slices.Contains(labels, readyDocLabel) && !slices.Contains(labelsAny, readyDocLabel) {
		q.ExcludeLabels = append(q.ExcludeLabels, readyDocLabel)
	}
	if assignee != "" {
		q.Assignee = assignee
	}
	// --unassigned narrows the queue to beads nobody has claimed, via the
	// daemon's `assignee IS NULL OR assignee = <empty>` predicate. This is the
	// work-seeking clause `gb ready` is FOR — every claim-then-work agent wants
	// "open, unblocked, nobody on it yet" — and it had no server-side spelling:
	// `--assignee ""` reads as "no assignee filter" and returns claimed beads
	// too. See ka-0000007209.
	if o.Unassigned {
		q.Unassigned = true
	}
	// --plan <epic> scopes the ready set to the transitive parent-child subtree
	// under that plan (the daemon's cycle-safe recursive CTE, same one
	// `kd list --parent` and the scheduler's bead_query{parent} gate use). This
	// is the ergonomic fix for ka-0000001367: a plan-gardener's own step beads are
	// genuinely READY (open, unblocked — parent-child is not a blocking dep) but
	// sink below the default limit under hundreds of project-wide ready beads.
	// An ID-anchored subtree query must NOT also carry the project label — the
	// subtree already IS the scope, and steps may live under a different project
	// (ka-0000008241); so --plan takes precedence over the project label below.
	if plan != "" {
		q.Parent = plan
	}
	if !allProjects && project != "" && plan == "" {
		q.Labels = append(q.Labels, "project:"+project)
	}
	// Append user label filters AFTER the project label so project scoping is
	// preserved when -l is also given. --label ANDs; --label-any ORs; and
	// --exclude-label drops. Mirrors kd list semantics.
	q.Labels = append(q.Labels, labels...)
	q.LabelsAny = append(q.LabelsAny, labelsAny...)
	q.ExcludeLabels = append(q.ExcludeLabels, excludeLabels...)
	applyLeafDefault(&q, o)
	return q
}

// applyLeafDefault narrows the ready query to claimable LEAF WORK for a
// work-seeking agent — the core of epic ka-0000000542 / audit ka-0000001484.
// Measured: a bare default `gb ready` returned a top-20 window that was ~85%
// non-claimable (intake thread tasks + epics), so a work-seeker saw ~2% of the
// queue and almost none of it was leaf work. The scope resolution honors the
// audit's precedence rules in strict order:
//
//  1. --all — restore exact pre-default behavior (no category scope, no
//     container-type exclusion). Nothing implicit is applied.
//  2. Explicit --category — the caller stated the scope; apply it verbatim and
//     apply nothing implicit.
//  3. --plan/--parent subtree — already ID-anchored to one plan's descendants;
//     do NOT also impose a leaf scope (a plan's steps may be epics/molecules,
//     mirroring why the project label is suppressed for a subtree query).
//  4. No agent identity (human/local dev) — fail OPEN to the full labeled view;
//     never hide work from a human by accident.
//  5. Otherwise (agent, un-anchored) — the leaf default: category=intermediate
//     AND exclude the container types (epic/molecule). --include-epics keeps the
//     containers while retaining the category scope.
//
// Explicit -t/--type is orthogonal and preserved: a caller who asks for a type
// still gets it; the container exclusion only appends to ExcludeTypes.
func applyLeafDefault(q *beadsapi.ListBeadsQuery, o readyOptions) {
	switch {
	case o.All:
		return
	case len(o.Categories) > 0:
		q.Categories = append(q.Categories, o.Categories...)
		return
	case o.Plan != "":
		return
	case !o.AgentContext:
		return
	}
	// Deliverable artifact types (bundle/dashboard/rich-report/mr/merge-request)
	// are dropped by the category=intermediate scope alone: DeriveCategory rule 3 +
	// migration 043 (MR !7580 / ka-0000007274) stamp them `artifact`, and the
	// post-deploy `kd admin backfill-category` sweep has re-stamped every legacy row
	// (0 `intermediate`-stamped deliverables remain fleet-wide, ka-0000004790). The
	// order-independent type-exclusion backstop (ka-0000006718) that used to cover
	// unswept legacy rows is therefore retired.
	q.Categories = append(q.Categories, leafDefaultCategory)
	if !o.IncludeEpics {
		for _, t := range readyContainerTypes {
			if !slices.Contains(q.ExcludeTypes, t) {
				q.ExcludeTypes = append(q.ExcludeTypes, t)
			}
		}
	}
}

// satisfyGateOnReady satisfies the decision gate when gb ready is called,
// provided the agent has no open decision beads. This allows the stop hook
// to pass after the agent checks for more work (the normal persistent-agent
// lifecycle: finish task → gb ready → claim next or stop).
func satisfyGateOnReady(ctx context.Context) {
	// A land door does not serve the gate route (admin-only), and an agent
	// on one has no Stop gate to release.
	if landDoor() != "" {
		return
	}
	agentID, err := resolveAgentIDWithFallback(ctx, "")
	if err != nil {
		return // No agent identity — can't satisfy gate.
	}

	// Only satisfy the gate if this agent has no open decisions of its own.
	// If the agent has an unresolved decision, it should yield on that first.
	// Use a high limit to avoid missing this agent's decisions when many
	// other agents have open decisions (the API doesn't support field filtering).
	decisions, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Limit:    200,
	})
	if err == nil {
		for _, d := range decisions.Beads {
			if d.Fields["requesting_agent_bead_id"] == agentID {
				return // Open decision exists — don't auto-satisfy.
			}
		}
	}

	satisfyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := daemon.SatisfyGate(satisfyCtx, agentID, "decision"); err != nil {
		return // Best-effort — don't fail the command.
	}

	// Mark how the gate was satisfied so stop-gate.sh can verify.
	markCtx, markCancel := context.WithTimeout(ctx, 5*time.Second)
	defer markCancel()
	_ = daemon.UpdateBeadFields(markCtx, agentID, map[string]string{
		"gate_satisfied_by": "ready",
	})

	// Clear the cooldown file since the gate is now satisfied.
	clearStopGateCooldown()
}

func init() {
	readyCmd.Flags().StringSliceP("type", "t", nil, "filter by type (repeatable)")
	readyCmd.Flags().StringSlice("exclude-type", nil, "drop beads whose type is ANY of these (NOT IN; repeatable/comma; e.g. --exclude-type epic,molecule). The type analogue of --exclude-label, and flag-parity with 'kd list --exclude-type' — lets you drop the container types from the queue without post-filtering the JSON in jq.")
	readyCmd.Flags().String("assignee", "", "filter by assignee")
	readyCmd.Flags().Bool("unassigned", false, "only beads with no assignee (server-side assignee IS NULL OR assignee = ''), i.e. nobody has claimed it yet. Completes the work-seeking query: gb ready -t task --unassigned --exclude-type epic. Mutually exclusive with --assignee.")
	readyCmd.Flags().IntP("limit", "n", readyDefaultLimit, "maximum number of results (human TTY default). With --json and no explicit --limit, the FULL ready set is returned — JSON is treated as programmatic so a dispatch lane never silently reasons over a 20-row browsing page (ka-0000006944).")
	readyCmd.Flags().String("project", defaultProject(), "filter by project label (default: $KD_PROJECT or $BOAT_PROJECT)")
	readyCmd.Flags().Bool("all-projects", false, "show beads from all projects (disables project filter)")
	readyCmd.Flags().String("plan", "", "scope to the ready steps under a plan/epic (transitive parent-child subtree). Surfaces a plan-gardener's own open, unblocked steps without them sinking below the limit under project-wide work. ID-anchored, so it overrides --project.")
	readyCmd.Flags().Bool("hold-overlaps", false, "with --plan: mark a step HELD when a file its description names in backticks is changed by an open PR of another step in the same plan; dispatch it after that PR merges. A hold older than 12h is marked EXPIRED: dispatch it anyway and comment why. Steps that name no files are never held. --json adds overlap_hold to held rows (gd-h7awQXGs8Z).")
	readyCmd.Flags().String("parent", "", "alias of --plan: scope to the transitive parent-child subtree of this bead id (flag-parity with 'kd list --parent')")
	readyCmd.Flags().Bool("include-scheduled", false, "include scheduled-task beads (cron-spawned; default: hidden)")
	readyCmd.Flags().StringSliceP("label", "l", nil, "filter by label — ANDed: a bead must carry ALL given labels (repeatable/comma; e.g. -l needs-intake). For OR/any-of use --label-any. The project filter still applies.")
	readyCmd.Flags().StringSlice("label-any", nil, "filter by ANY of these labels (OR; repeatable/comma; e.g. --label-any release-stuck,preflight-broken). Contrast -l/--label which ANDs.")
	readyCmd.Flags().StringSlice("exclude-label", nil, "drop beads carrying ANY of these labels (NOT IN; repeatable/comma; e.g. --exclude-label needs-intake). Lets you express a lane query's NOT-clause without hand-rolling jq.")
	readyCmd.Flags().Bool("all", false, "show the full ready set with NO leaf-work scoping — restores the pre-default behavior (all categories, epics/molecules and tasks awaiting their release included). By default an agent context (BOAT_ROLE/KD_AGENT_ID set) sees only claimable leaf work (category=intermediate, no epics); --all opts back into the flat view.")
	readyCmd.Flags().StringSlice("category", nil, "scope to these materialized categories (intake,intermediate,action,config; repeatable/comma). An explicit --category disables the implicit leaf default. e.g. --category intake to see thread/source intake work.")
	readyCmd.Flags().Bool("include-epics", false, "keep container types (epic/molecule) in the default leaf view. No effect with --all or --plan.")
}

// loadReadyRunner reads this machine's runner registration; a var so tests
// can stand one in.
var loadReadyRunner = func() (runnerreg.Runner, error) {
	r, _, err := runnerreg.Load()
	return r, err
}

// readyRunner reports the runner this `gb ready` runs on: an agent (not a
// human, whose view is never narrowed) on a machine with a runner
// registration, i.e. a `gb runner join` session.
func readyRunner(o readyOptions) (runnerreg.Runner, bool) {
	if !o.AgentContext {
		return runnerreg.Runner{}, false
	}
	me, err := loadReadyRunner()
	if err != nil {
		return runnerreg.Runner{}, false
	}
	return me, true
}

// filterForRunner keeps the ready beads a local runner may take (spec §7): a
// task placed on it (placement=local and its runner_id, or one of its tags),
// or an unplaced task in an allowlisted project that its owner opened from
// their own thread (fields.opened_by_operator == myOwner). An unplaced task
// without that field is not this runner's.
func filterForRunner(beads []*beadsapi.BeadDetail, me runnerreg.Runner, myOwner string) []*beadsapi.BeadDetail {
	allowed := map[string]bool{}
	for _, p := range me.Projects {
		allowed[p] = true
	}
	tags := map[string]bool{}
	for _, t := range me.Tags {
		tags[t] = true
	}
	var out []*beadsapi.BeadDetail
	for _, b := range beads {
		if b.Fields["placement"] == "local" {
			if (b.Fields["runner_id"] != "" && b.Fields["runner_id"] == me.RunnerID) || (b.Fields["runner_tag"] != "" && tags[b.Fields["runner_tag"]]) {
				out = append(out, b)
			}
			continue
		}
		if myOwner != "" && allowed[spawnProjectFromLabels(b.Labels)] && b.Fields["opened_by_operator"] == myOwner {
			out = append(out, b)
		}
	}
	return out
}

// dropLocallyPlaced removes tasks placed on a local runner from a cluster
// agent's ready queue: only that runner's agent takes them.
func dropLocallyPlaced(beads []*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
	out := beads[:0:0]
	for _, b := range beads {
		if b.Fields["placement"] != "local" {
			out = append(out, b)
		}
	}
	return out
}

// dropAwaitingRelease removes tasks parked behind their release
// (beadsapi.AwaitsRelease): the merge is done and the release closes them.
func dropAwaitingRelease(beads []*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
	out := beads[:0:0]
	for _, b := range beads {
		if !beadsapi.AwaitsRelease(b.Fields) {
			out = append(out, b)
		}
	}
	return out
}
