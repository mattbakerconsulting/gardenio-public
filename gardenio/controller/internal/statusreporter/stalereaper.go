package statusreporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

// spawnedFromRe extracts a bead ID from "Spawned from ka-XXXX: ..." descriptions
// produced by the Gardenio iOS app.
var spawnedFromRe = regexp.MustCompile(`^Spawned from ((?:gd|ka)-[A-Za-z0-9]+)`)

// unschedulableReapAfter is the grace period before reaping a pod stuck
// Pending with PodScheduled=Unschedulable. Short enough that operators
// don't sit on zombie cards, long enough to ride out a brief cluster
// scale-up. 10 minutes matches the bead description.
const unschedulableReapAfter = 10 * time.Minute

// noPodReapAfter is the grace period before fast-failing an agent bead that
// is stuck at an EMPTY agent_state (never even "spawning") with NO live pod
// (ka-0000004685). An empty state means the pod-create/status-report hop never
// completed; when no pod exists for the bead either, it will never boot on its
// own — the reconciler create-missing loop is burst-limited (CoopBurstLimit
// pods/pass) so a genuinely-creatable bead gets a pod within a few passes, and
// anything still podless well past that is wedged. Without this, such a bead
// shares the 2h base timeout with "working" agents and lingers as an open,
// terminal-less "gardener with no session" card (exactly what Matt flagged) for
// up to 2h before the reaper cleans it up. Fail it fast instead so the zombie
// card clears in minutes and the fleet re-fires a fresh spawn on the next
// gardener tick. 15m is comfortably longer than the reconciler's realistic
// time-to-pod under burst limits, and dramatically shorter than 2h.
//
// The reap is GATED ON GENUINE POD ABSENCE (Reap() lists live pods and passes
// podExists into reapReason): an empty-state bead that DOES have a live pod is
// merely slow to report status — a reconciler-created pod stays empty-state
// until the next SyncAll maps its Pending phase to "spawning" — and must keep
// the full base timeout so a live-but-slow-to-report pod is never killed.
const noPodReapAfter = 15 * time.Minute

// assigningReapAfter is the grace period before fast-failing an agent bead
// stuck in agent_state="assigning" with NO live pod (G6, ka-0000006802). The
// prewarmed-pool hand-off (gb pool assign / poolmanager warm-resume) flips a
// parked agent from "prewarmed" to "assigning" and sets its task_id; the in-pod
// entrypoint, polling its own bead, then sees the flip, hydrates the assignment
// env, and reports "working". If the pod dies in that window — a post-assign
// crash, OOM, or node loss — the bead is stranded at "assigning": the pool
// reconciler only counts "prewarmed" agents, so it is invisible to pool
// accounting, and nothing else cleans it up until the in-pod 24h standby TTL
// eventually expires. Reap it on a short clock so the leaked slot clears in
// minutes and a fresh spawn re-fires.
//
// GATED ON GENUINE POD ABSENCE, exactly like the empty-state fast-fail above: a
// bead flipped to "assigning" whose pod is alive and merely slow to report
// "working" MUST keep the full base-timeout path and is never early-reaped. Set
// to 10m to match the bead description's "assigning-too-long" threshold —
// comfortably longer than the entrypoint's poll-and-hydrate latency, and far
// shorter than the 2h base timeout the state would otherwise inherit from the
// default arm.
const assigningReapAfter = 10 * time.Minute

// orphanReapAfter is the grace period before orphan-reaping a "working" (or
// empty-state) agent whose source task has already been closed. Without it the
// reaper killed live, actively-reporting pods the instant their task closed,
// with zero grace and no busy-check (ka-0000000018). The grace is measured
// against updated_at, which is a reliable busy/idle proxy: SyncAll bumps
// updated_at every cycle while the pod is busy but deliberately skips the bump
// when coop reports idle (ka-0000003615). So a still-busy pod (mid-wrap-up:
// commit/push/MR-link/gb stop) keeps a fresh updated_at and is never
// orphan-reaped, while a genuinely-idle pod that forgot `gb stop` ages past
// this window and gets cleaned up.
const orphanReapAfter = 5 * time.Minute

// gardenerOrphanReapAfter is the longer grace for gardener agents. A gardener
// closes its per-cycle run-instance task as its success signal and then either
// loops into the next iteration or parks briefly between cycles. A busy gardener
// is already protected by the updated_at freshness proxy; the extra runway here
// covers the idle-between-cycles window so a self-looping gardener is not reaped
// mid-sleep. A gardener idle past this window is genuinely dormant and is
// correctly reaped — its schedule/trigger re-spawns a fresh pod on the next fire.
const gardenerOrphanReapAfter = 3 * orphanReapAfter

// StaleReaper detects agent pods that haven't updated their bead state
// within a configurable timeout and terminates them to free pod slots.
type StaleReaper struct {
	daemon    *beadsapi.Client
	pods      podmanager.Manager
	namespace string
	// gvisorNamespace is the PSA-privileged namespace where runsc (gVisor)
	// freeze/thaw agents run (ka-0000008076). When set, a stale runsc agent's
	// pod is deleted there instead of the primary namespace, matching the
	// routing in cmd/controller/podspec.go:routeGVisorNamespace. Empty leaves
	// the single-namespace behaviour unchanged.
	gvisorNamespace string
	timeout         time.Duration
	// runtimeCeilings is the ABSOLUTE runtime ceiling per agent primary role,
	// measured from created_at. It is the reap-side spend-stopper complement to
	// the non-destructive lease-side age bound (bug ka-0000001095 / MR !6050):
	// the lease bound resumes a wedged lane but leaves the wedged pod alive
	// burning spend; this ceiling reaps the pod once its runtime is provably
	// long-dead. Unlike `timeout`, it keys on created_at, not updated_at, so a
	// wedged-but-churning agent that keeps updated_at fresh (the re-arm/nudge
	// treadmill) cannot dodge it. A role with no entry (or a zero/negative
	// value) has no ceiling. See runtimeReapReason and bug ka-0000000716
	// (originally scoped to trigger-spawned agents only; extended to every role
	// by gd-X2eSin2J80).
	runtimeCeilings map[string]time.Duration
	logger          *slog.Logger
	now             func() time.Time // for testing
	// tunables is an optional runtime-tunable source (config-bead backed) used
	// for the open-run-instance close gate. When nil, compiled defaults apply.
	tunables Tunables
	// hasSnapshot reports whether a persisted freeze/thaw snapshot exists for
	// an agent, mirroring reconciler.SnapshotChecker. Wired from the freeze/thaw
	// lifecycle when the feature is enabled; nil otherwise. reapReason uses it
	// to exempt frozen-awaiting-thaw beads (agent_state=stopping with a live
	// snapshot) from reaping — closing one would destroy the thaw-target signal
	// and force the next wake to cold-start (ka-0000002104).
	hasSnapshot func(ctx context.Context, agentName string) bool
	// reclaimTypes, when non-nil, limits the claim-release sweeps
	// (reopenStrandedIssueTasks, clearStrandedOpenAssignees) to issue beads of
	// these types: the ones the daemon lets the controller update without
	// owning them. nil keeps every issue type (static-token bypass). See
	// SetReclaimTypes.
	reclaimTypes map[string]bool
	// prState reads a linked GitHub PR's state for the stranded-task sweep
	// (gd-ovzZSuuHny): a task whose every linked PR is closed or merged is
	// reopened like any other strand instead of flagged task:orphaned-with-pr.
	// nil (no GitHub client wired) keeps flag-on-any-PR. See SetPRStateLookup.
	prState PRStateLookup
	// prStateCache holds recent successful prState answers by PR URL, so a
	// task flagged for an open PR does not cost a GitHub round trip every pass.
	prStateMu    sync.Mutex
	prStateCache map[string]cachedPRState
	// prLookupBudget overrides prLookupPassBudget; zero uses the default.
	prLookupBudget time.Duration
}

// PR states a PRStateLookup reports. A merged PR is "merged", never "closed".
const (
	PRStateOpen   = "open"
	PRStateClosed = "closed"
	PRStateMerged = "merged"
)

// PRStateLookup returns the state (PRStateOpen, PRStateClosed or
// PRStateMerged) of the GitHub pull request at prURL.
type PRStateLookup func(ctx context.Context, prURL string) (string, error)

type cachedPRState struct {
	state string
	at    time.Time
}

// prStateCacheTTL is how long an open or merged PR state answer is reused.
// Long enough that a fleet of flagged tasks with open PRs costs one lookup
// each per half hour at the 5-minute reap cadence; short enough that a PR
// closed since is noticed within the hour. A merged PR never changes again.
const prStateCacheTTL = 30 * time.Minute

// prStateClosedCacheTTL is how long a closed (unmerged) answer is reused. A
// closed PR can be reopened, and one still read as closed would reopen a task
// whose work is back up for review, so the answer is shared only within a pass
// and read again on the next one (gd-wahYHJndnb).
const prStateClosedCacheTTL = 2 * time.Minute

// prStateTTL is how long state may be served from prStateCache.
func prStateTTL(state string) time.Duration {
	if state == PRStateClosed {
		return prStateClosedCacheTTL
	}
	return prStateCacheTTL
}

// maxPRLookupsPerTask bounds the GitHub calls one stranded task can cost. A
// task linking more PRs than this is treated as still having an open one.
const maxPRLookupsPerTask = 5

// prLookupPassBudget bounds the time one reap pass spends waiting on GitHub
// for PR states. Reap runs inline in runPeriodicSync, so without it a hung
// GitHub would stall every sweep behind it for minutes (gd-wahYHJndnb). Once it
// is spent, a PR not in prStateCache reads as unknown, which flags the task
// rather than reopening it; the next pass gets a fresh budget.
const prLookupPassBudget = 60 * time.Second

// prLookupTimeout bounds one PR state read, so one hung call cannot spend the
// whole pass budget on its own.
const prLookupTimeout = 15 * time.Second

// errPRLookupBudgetSpent is the lookup error once a pass has spent its
// prLookupPassBudget.
var errPRLookupBudgetSpent = errors.New("PR state lookup budget for this reap pass is spent")

// prLookupPass is one reap pass's remaining GitHub time for PR state reads.
// Only time spent waiting on GitHub counts, not the pass's daemon calls.
type prLookupPass struct {
	budget    time.Duration
	remaining time.Duration
	// starved counts the lookups refused because remaining ran out.
	starved int
}

func (r *StaleReaper) newPRLookupPass() *prLookupPass {
	budget := r.prLookupBudget
	if budget <= 0 {
		budget = prLookupPassBudget
	}
	return &prLookupPass{budget: budget, remaining: budget}
}

// Tunables is the narrow accessor the reaper reads runtime-tunable values
// through, mirroring reconciler.Tunables. Satisfied by *beadsapi.ConfigWatcher
// and by test stubs. When the reaper has no tunables source attached, compiled
// defaults are used. It exists so the open-run-instance close gate
// (STALEREAPER_CLOSE_OPEN_RUNINSTANCES) can be flipped from dry-run to enabled
// via a config-bead update without a controller restart. See ka-0000000075.
type Tunables interface {
	GetBool(key, envVar string, compiled bool) bool
}

// StaleReaperStats holds metrics from a single reap pass.
type StaleReaperStats struct {
	Checked int
	Reaped  int
	// Reconciled counts stranded schedule/cron run-instance task beads that
	// were closed because their assignee agent is gone (reaped this pass, or
	// already closed/absent from a prior crash/OOM). See ka-0000000889.
	Reconciled int
	// Reopened counts stranded claimed ISSUE task beads that were REOPENED
	// (returned to open/unassigned) because their assignee agent is gone. This
	// is the SIGKILL backstop for EOL salvage (ka-0000002469, design
	// ka-0000007236): when a claimed agent is hard-killed no in-pod code runs,
	// so the involuntary-SIGTERM reopen (Phase 2, hook.go reopenOrphanedClaimedTask)
	// never fires and the issue is stranded in_progress with a dead assignee.
	// The reaper reopens it so the work returns to `gb ready`. Distinct from
	// Reconciled: run-instances are single-shot and CLOSED; issue tasks are
	// retriable work and REOPENED.
	Reopened int
	// OpenRunInstancesClosed counts OPEN, never-claimed schedule/cron
	// run-instance fire-record beads that were closed because they aged past
	// the grace without ever being picked up by an agent. These are the ~930
	// parentless "orphans" that dominated the 2026-07-08 audit (ka-0000000075).
	// When the close gate is in dry-run (the default), this counts what WOULD
	// have been closed but leaves the beads untouched.
	OpenRunInstancesClosed int
	// OpenAssigneesCleared counts ISSUE task beads found status=OPEN but carrying
	// a DEAD assignee — the "half-reap" residue (ka-0000000908): a gardener reaped
	// a zombie with `kd update --status open` but forgot `--assignee ""`. That
	// combination is stranded — open+dead-assignee falls through every dispatch
	// lane (all require assignee=="") and the in_progress zombie sweep no longer
	// matches it. This backstop clears the dead assignee so the bead re-enters
	// `gb ready`. Distinct from Reopened, which handles the in_progress→open
	// SIGKILL case; here the bead is ALREADY open and only the assignee is wrong.
	OpenAssigneesCleared int
	// FlaggedOrphanedWithPR counts in_progress ISSUE task beads that
	// reopenStrandedIssueTasks found with a gone assignee but did NOT reopen,
	// because either (a) a live escalation/decision still parks the task
	// (gd-t0gm8q62Ac Part 1's respawn-on-resolve is what should bring it back),
	// or (b) the task carries a linked PR, so blindly reopening it risks a
	// second crew duplicating work already up for review. These are labeled
	// task:orphaned-with-pr and commented for a follow-through crew instead.
	// See gd-t0gm8q62Ac Part 2.
	FlaggedOrphanedWithPR int
	// Paused counts laptop agents set to agent_state=paused this pass because
	// their runner went away (lid closed, laptop asleep, gb runner leave).
	// Resumed counts paused laptop agents set back to working because their
	// runner is online again. Local-agents D5: disconnect holds, it never reaps.
	Paused  int
	Resumed int
}

// runInstanceReconcileMinAge is the minimum staleness a stranded run-instance
// task must reach before it is reconciled when its assignee agent disappeared
// in a *prior* pass (crash/OOM, not reaped here). Tasks whose assignee was
// reaped in the current pass are reconciled immediately — that agent is
// definitively gone. The grace guards against the race where a task was just
// claimed by an agent whose bead has not yet surfaced in ListAgentBeads.
const runInstanceReconcileMinAge = 15 * time.Minute

// NewStaleReaper creates a reaper that terminates agent pods whose beads
// haven't been updated within timeout. Returns nil if timeout is zero (disabled).
func NewStaleReaper(daemon *beadsapi.Client, pods podmanager.Manager, namespace string, timeout time.Duration, logger *slog.Logger) *StaleReaper {
	if timeout <= 0 {
		return nil
	}
	return &StaleReaper{
		daemon:    daemon,
		pods:      pods,
		namespace: namespace,
		timeout:   timeout,
		logger:    logger.With("component", "stale-reaper"),
		now:       time.Now,
	}
}

// SetGVisorNamespace configures the namespace where runsc (gVisor) freeze/thaw
// agents run, so their stale pods are deleted there rather than in the primary
// namespace. Empty or duplicate-of-primary values are ignored (no-op).
func (r *StaleReaper) SetGVisorNamespace(ns string) {
	if ns == "" || ns == r.namespace {
		return
	}
	r.gvisorNamespace = ns
}

// SetReclaimTypes limits the claim-release sweeps to issue beads of the given
// types. The controller's machine principal may update a bead it does not own
// only when its kbeads ownership grant lists the type
// (gardenio-machine-controller-ownership, kbeads/internal/server/
// machine_grants.go: task in full, bug/feature/chore/epic update-only while the
// assignee is a gone agent). Writing to any other type on that principal is an
// ownership 403 that repeats every pass and never lands (gd-ozX2LfXsrv), so
// main passes the granted types. Empty or nil leaves every issue type in scope.
func (r *StaleReaper) SetReclaimTypes(types []string) {
	if len(types) == 0 {
		r.reclaimTypes = nil
		return
	}
	r.reclaimTypes = make(map[string]bool, len(types))
	for _, t := range types {
		r.reclaimTypes[t] = true
	}
}

// reclaimable reports whether a claim-release sweep may write to t: a type
// in scope (SetReclaimTypes) and an assignee that is not a human. A human
// identity is an email (agent names become pod names and never carry '@').
// The agents-by-name lookup alone cannot rule a human out: a decision-resume
// respawn can file a type=agent bead titled with the operator's email
// (gd-9H3WCBTnmq), and once that bead closes the operator's own claims would
// read as a gone agent's.
func (r *StaleReaper) reclaimable(t *beadsapi.BeadDetail) bool {
	if r.reclaimTypes != nil && !r.reclaimTypes[t.Type] {
		return false
	}
	return !strings.Contains(t.Assignee, "@")
}

// SetTunables installs a runtime-tunable provider (e.g. a ConfigWatcher) used
// for the open-run-instance close gate. When set, the gate is read through it
// on every reap pass, so flipping the backing config bead moves the reaper from
// dry-run (log-only) to actually closing stranded open run-instances without a
// controller restart. See ka-0000000075.
func (r *StaleReaper) SetTunables(t Tunables) {
	r.tunables = t
}

// SetRuntimeCeilings installs the absolute runtime ceiling per agent primary
// role (bug ka-0000000716, extended to every role by gd-X2eSin2J80). A
// positive value for a role arms the created_at-based runtime-ceiling watchdog
// in reapReason's working case for agents of that role; a role that is absent
// or maps to zero (or negative) has no ceiling, preserving the pre-existing
// updated_at-only behaviour. The ceiling still only ACTS when the dry-run arm
// gate is flipped (see runtimeReapEnabled) — a positive value alone runs it in
// log-only mode.
func (r *StaleReaper) SetRuntimeCeilings(ceilings map[string]time.Duration) {
	r.runtimeCeilings = ceilings
}

// ceilingForRole returns the configured runtime ceiling for a (possibly
// comma-separated multi-role) agent role string, or 0 if the role carries no
// ceiling. Mirrors reporter.go's roleIsLongLived: the first role is
// authoritative.
func (r *StaleReaper) ceilingForRole(role string) time.Duration {
	primary, _, _ := strings.Cut(role, ",")
	return r.runtimeCeilings[strings.TrimSpace(primary)]
}

// SetSnapshotChecker installs a freeze/thaw snapshot-presence check so the
// reaper can exempt frozen-awaiting-thaw agents (agent_state=stopping with a
// persisted snapshot) instead of reaping them as unknown-state after the stale
// timeout (ka-0000002104). Mirrors reconciler.SetSnapshotChecker; only wired
// when freeze/thaw is enabled.
func (r *StaleReaper) SetSnapshotChecker(fn func(ctx context.Context, agentName string) bool) {
	r.hasSnapshot = fn
}

// SetPRStateLookup installs the GitHub PR state reader the stranded-task sweep
// uses to tell a still-open linked PR from a closed or merged one
// (gd-ovzZSuuHny). Without one, any linked PR flags the task.
func (r *StaleReaper) SetPRStateLookup(fn PRStateLookup) {
	r.prState = fn
}

// closeOpenRunInstancesEnabled reports whether the reaper should actually CLOSE
// stranded open run-instance beads (true, the compiled default) or run in
// dry-run/log-only mode (false). The gate originally defaulted OFF so the
// rollout could first confirm the candidate count and predicate precision from
// the dry-run log line before any mass-close (ka-0000000075). That confirmation
// arrived: the dry-run ran 37 days with a steady ~226-candidate count and a
// precision sample of 10/10 genuinely-dead fire records (no live agent, no
// in-flight MR), but the rollout's second half was dropped and the gate sat OFF
// while stranded per-run gardener firing beads accumulated (ka-0000006054). The
// compiled default is now ON so the reaper drains and keeps draining the backlog
// without any per-deploy config step. An operator can still set
// STALEREAPER_CLOSE_OPEN_RUNINSTANCES=false (env or config bead) as a kill-switch
// to fall back to dry-run if the predicate ever needs re-validation.
func (r *StaleReaper) closeOpenRunInstancesEnabled() bool {
	if r.tunables == nil {
		// No tunables source wired (e.g. some tests): honor the compiled default.
		return true
	}
	return r.tunables.GetBool(
		"STALEREAPER_CLOSE_OPEN_RUNINSTANCES",
		"STALEREAPER_CLOSE_OPEN_RUNINSTANCES",
		true,
	)
}

// runtimeReapEnabled reports whether the runtime-ceiling watchdog should
// actually REAP an over-ceiling agent (true) or run in dry-run/log-only mode
// (false, the compiled default). This reap is DESTRUCTIVE — it kills a live
// pod, irreversibly losing any in-flight work — so it defaults OFF exactly
// like the open-run-instance close gate: the rollout first confirms from the
// "would reap" log line that the ceiling + confirmation guards match only
// genuine wedges, then an operator flips STALEREAPER_RUNTIME_REAP=true (env or
// config bead) to arm it, no restart. Bug ka-0000000716; renamed from
// STALEREAPER_TRIGGER_RUNTIME_REAP when the ceiling was extended to every role
// (gd-X2eSin2J80).
func (r *StaleReaper) runtimeReapEnabled() bool {
	if r.tunables == nil {
		return false
	}
	return r.tunables.GetBool(
		"STALEREAPER_RUNTIME_REAP",
		"STALEREAPER_RUNTIME_REAP",
		false,
	)
}

// agentHoldsOpenMR reports whether the agent bead holds an OPEN merge-request
// bead reachable via its produces graph (agent → task → merge-request). It is
// the "MR mid-flight" confirmation guard the bug requires: killing an agent that
// has an OPEN MR is the irreversible case (the MR work is already pushed and
// reviewable, so the pod's death costs nothing more to recover, but reaping it
// mid-push could truncate the very MR that makes the run salvageable). When an
// agent holds an open MR we DECLINE the runtime-ceiling reap and let the MR
// terminate the run instead — gardener-delivery merges/closes the MR, which
// closes the MR bead, or the operator intervenes.
//
// Best-effort and fail-CLOSED-on-uncertainty: a graph-walk error returns true
// (treat as holding an MR → do NOT reap) so a transient daemon hiccup can never
// escalate a non-destructive skip into a destructive kill. A merged/closed MR
// bead does not count as "open" and does not block the reap.
func (r *StaleReaper) agentHoldsOpenMR(ctx context.Context, agentBeadID string) bool {
	mrs, err := r.daemon.MRBeadsForAgent(ctx, agentBeadID)
	if err != nil {
		r.logger.Warn("stale reaper: MR-in-flight check failed; declining runtime-ceiling reap this pass",
			"bead", agentBeadID, "error", err)
		return true
	}
	for _, mr := range mrs {
		if mr == nil {
			continue
		}
		if mr.Status != "closed" {
			return true
		}
	}
	return false
}

// podNamespace returns the namespace a given agent bead's pod lives in. runsc
// (gVisor) agents are routed to gvisorNamespace when set (mirroring
// routeGVisorNamespace); everything else stays in the primary namespace. An
// explicitly pinned namespace on the bead always wins.
func (r *StaleReaper) podNamespace(b beadsapi.AgentBead) string {
	if pinned := b.Metadata["namespace"]; pinned != "" {
		return pinned
	}
	if r.gvisorNamespace != "" && b.Metadata["runtime_class"] == podmanager.RuntimeClassRunsc {
		return r.gvisorNamespace
	}
	return r.namespace
}

// podName returns the deterministic pod name for an agent bead, built the same
// way the reconciler does: {mode}-{project}-{sanitized_role}-{agent_name}.
func (r *StaleReaper) podName(b beadsapi.AgentBead) string {
	agentName := strings.TrimPrefix(b.AgentName, b.Project+"-")
	return fmt.Sprintf("%s-%s-%s-%s", b.Mode, b.Project, podmanager.SanitizeRole(b.Role), agentName)
}

// livePodNames returns the set of agent pod names currently present across the
// primary namespace and the gVisor namespace (ka-0000004685). Used to gate the
// empty-state fast-fail on genuine pod absence. Returns nil (not an empty map)
// on any list error so callers can distinguish "looked and found none" from
// "could not look": a nil result makes reapReason treat every bead as
// possibly-podded, so a transient K8s API blip never triggers a spurious early
// reap.
func (r *StaleReaper) livePodNames(ctx context.Context) map[string]bool {
	namespaces := []string{r.namespace}
	if r.gvisorNamespace != "" && r.gvisorNamespace != r.namespace {
		namespaces = append(namespaces, r.gvisorNamespace)
	}
	live := make(map[string]bool)
	for _, ns := range namespaces {
		pods, err := r.pods.ListAgentPods(ctx, ns, map[string]string{
			podmanager.LabelApp: podmanager.LabelAppValue,
		})
		if err != nil {
			r.logger.Warn("stale reaper: failed to list pods; skipping empty-state fast-fail this pass",
				"namespace", ns, "error", err)
			return nil
		}
		for _, p := range pods {
			if _, ok := p.Labels[podmanager.LabelAgent]; !ok {
				continue
			}
			live[p.Name] = true
		}
	}
	return live
}

// Reap checks all active agent beads and closes + deletes pods for any
// that haven't been updated within the configured timeout. It reaps agents
// in "working" state or with empty/unknown agent_state (orphaned beads
// whose pods died without updating state). Agents in "spawning" or
// "yielding" states get a longer grace period (2x timeout) before reaping.
// "prewarmed" agents are excluded entirely.
//
// Additionally, agents whose source task is already closed are reaped as
// orphans — in the "queued" state this prevents agents from sitting in the
// queue forever when another agent completed their task before they got a
// pod slot; in the "working" / empty state it cleans up pods whose agent
// finished work but forgot to call `gb stop`, since SyncAll keeps bumping
// updated_at and the wall-clock staleness check would otherwise never fire.
func (r *StaleReaper) Reap(ctx context.Context) StaleReaperStats {
	// Lean: the reaper reads only runtime keys (gd-zXdZq4Traf).
	beads, err := r.daemon.ListAgentBeadsLean(ctx)
	if err != nil {
		r.logger.Warn("failed to list agent beads for stale check", "error", err)
		return StaleReaperStats{}
	}

	now := r.now()
	stats := StaleReaperStats{Checked: len(beads)}

	// Build the set of pod names that currently exist, so reapReason can gate
	// the empty-state fast-fail on genuine pod absence (ka-0000004685). List
	// across the primary namespace and the gVisor namespace (where runsc
	// gardeners run), mirroring the reconciler's multi-namespace pod scan. A
	// list error leaves livePods nil — reapReason then treats every bead as
	// possibly-podded (podExists=true), so a transient K8s API blip can never
	// cause a spurious early reap; the empty-state bead just falls through to
	// the unchanged 2h base-timeout path.
	livePods := r.livePodNames(ctx)

	// liveAgents holds the names of agents still present (not reaped this pass)
	// so the run-instance reconciler can tell a stranded task (assignee gone)
	// from a live one (assignee still working). reapedAgents holds agents the
	// reaper closed in this pass — their tasks reconcile immediately.
	liveAgents := make(map[string]bool, len(beads))
	reapedAgents := make(map[string]bool)

	for _, b := range beads {
		var reason string
		if podmanager.IsLaptopTarget(b.Metadata["execution_target"]) {
			var held bool
			if reason, held = r.laptopAgentHold(ctx, b, now, &stats); held {
				liveAgents[b.AgentName] = true
				continue
			}
		} else {
			podExists := livePods == nil || livePods[r.podName(b)]
			reason = r.reapReason(ctx, b, now, podExists)
		}
		if reason == "" {
			liveAgents[b.AgentName] = true
			continue
		}
		reapedAgents[b.AgentName] = true

		r.logger.Warn("reaping stale agent",
			"bead", b.ID, "agent", b.AgentName, "project", b.Project,
			"agent_state", b.AgentState, "updated_at", b.UpdatedAt,
			"reason", reason,
			"stale_for", now.Sub(b.UpdatedAt).Round(time.Second))

		// Close the bead so the reconciler stops trying to create a pod for it.
		// Derive agent_state from the reap reason: a benign orphan (source task
		// already finished, including a gardener that completed its loop and went
		// dormant) is not a crash and must not render a ❌ "failed" card. Only
		// genuine staleness/stuck reaps are failures. See ka-0000001336.
		closeState := reapCloseState(reason, b.AgentState)
		closeFields := map[string]string{
			"agent_state":  closeState,
			"close_reason": reason,
		}
		// Synthesize a fallback wrap-up when the bead has none so the Slack card
		// renders a tl;dr headline instead of metadata-only (Fix A,
		// ka-0000007639). The controller has no in-pod gb binary, so this
		// mirrors gb's synthesizeFallbackWrapUp from the beadsapi layer. A real
		// wrap-up already on the bead always wins; the GetBead probe is
		// best-effort — a fetch failure just skips the fallback.
		if detail, err := r.daemon.GetBead(ctx, b.ID); err == nil && detail != nil {
			if strings.TrimSpace(detail.Fields[beadsapi.WrapUpFieldName]) == "" {
				if wu := beadsapi.SynthesizeReapWrapUp(reason, beadsapi.ReapWrapUpTitle(detail)); wu != "" {
					closeFields[beadsapi.WrapUpFieldName] = wu
				}
			}
		}
		if err := r.daemon.CloseBead(ctx, b.ID, closeFields); err != nil {
			r.logger.Warn("failed to close stale bead", "bead", b.ID, "error", err)
			continue
		}

		// Delete the pod if it exists. A laptop agent has none.
		if !podmanager.IsLaptopTarget(b.Metadata["execution_target"]) {
			podName := r.podName(b)
			if err := r.pods.DeleteAgentPod(ctx, podName, r.podNamespace(b)); err != nil {
				r.logger.Warn("failed to delete stale pod (may already be gone)",
					"pod", podName, "bead", b.ID, "error", err)
			}
		}

		stats.Reaped++
	}

	// Reconcile schedule/cron run-instance task beads stranded in_progress by
	// a now-gone assignee agent (reaped this pass, or crashed/OOMed in a prior
	// pass and never reaped). Without this, gardener/schedule run-instances pile
	// up as zombie in_progress tasks — the visible "idle crew" residue. See
	// ka-0000000889.
	stats.Reconciled = r.reconcileRunInstanceTasks(ctx, now, liveAgents, reapedAgents)

	// Reopen ordinary claimed ISSUE tasks stranded in_progress by a now-gone
	// AGENT assignee. This is the SIGKILL backstop for EOL salvage
	// (ka-0000002469): a hard-killed pod runs no in-pod code, so the
	// involuntary-SIGTERM reopen (Phase 2, hook.go) never fires and the issue
	// sits in_progress with a dead assignee — the stale-claim tax on the next
	// agent (advice ka-0000007726). Unlike run-instances (closed), issue tasks
	// are retriable and are RE-OPENED so the work returns to `gb ready`.
	stats.Reopened, stats.FlaggedOrphanedWithPR = r.reopenStrandedIssueTasks(ctx, now, liveAgents, reapedAgents)

	// Reconcile OPEN, never-claimed schedule/cron run-instance fire-record beads.
	// The trigger engine creates one per cron fire; when no agent ever claims it
	// (spawn failed, project paused, capacity exhausted) it lingers open and
	// parentless forever — ~930 such beads dominated the 2026-07-08 orphan audit
	// (ka-0000000075). Distinct from reconcileRunInstanceTasks above, which only
	// sees in_progress (claimed-then-stranded) beads. Gated by a dry-run tunable
	// so the count is confirmed in logs before any mass-close.
	stats.OpenRunInstancesClosed = r.reconcileOpenRunInstanceTasks(ctx, now)

	// Clear DEAD assignees off ISSUE task beads already at status=OPEN — the
	// "half-reap" residue (ka-0000000908). A gardener that reaps a zombie with
	// `kd update --status open` but omits `--assignee ""` leaves the bead
	// open+dead-assignee, which every dispatch lane filters out (all require
	// assignee=="") and the in_progress zombie sweep no longer matches — so it
	// is stranded silently, strictly worse than an un-reaped zombie the 8h sweep
	// would eventually re-catch. reopenStrandedIssueTasks above only scans
	// in_progress, so it never sees this class. This sweep is the code backstop
	// that makes a future half-reap self-heal instead of needing a gardener to
	// notice by hand. It reuses the identical gone-agent liveness guards.
	stats.OpenAssigneesCleared = r.clearStrandedOpenAssignees(ctx, now, liveAgents, reapedAgents)

	if stats.Reaped > 0 || stats.Reconciled > 0 || stats.Reopened > 0 ||
		stats.OpenRunInstancesClosed > 0 || stats.OpenAssigneesCleared > 0 ||
		stats.FlaggedOrphanedWithPR > 0 || stats.Paused > 0 || stats.Resumed > 0 {
		r.logger.Info("stale reap pass complete",
			"checked", stats.Checked, "reaped", stats.Reaped,
			"paused", stats.Paused, "resumed", stats.Resumed,
			"reconciled", stats.Reconciled, "reopened", stats.Reopened,
			"open_run_instances_closed", stats.OpenRunInstancesClosed,
			"open_assignees_cleared", stats.OpenAssigneesCleared,
			"flagged_orphaned_with_pr", stats.FlaggedOrphanedWithPR)
	}

	return stats
}

// runInstanceSourceLabels are the source labels the trigger
// engine stamps on schedule/cron/event run-instance task beads (see
// internal/bridge/triggers.go createTaskBead and the schedule/multi trigger
// sources). A task carrying one of these is a per-cycle run-instance whose
// lifecycle is owned by its short-lived agent — exactly the beads that strand
// in_progress when the agent is reaped without calling `gb stop`.
var runInstanceSourceLabels = []string{
	"source:schedule",        // ScheduleTriggerSource ("Schedule: ...")
	"source:multi",           // MultiTriggerSource ("Multi[cron]/Multi[bead_event]: ...")
	"source:schedule-mirror", // schedule-mirror run instances
}

// reconcileRunInstanceTasks finds schedule/cron run-instance task beads that
// are still in_progress but whose assignee agent is gone, and closes them so
// they stop showing up as perpetual "idle crew". An assignee counts as gone
// when it was reaped in this pass (reapedAgents) or is no longer among the
// active agent beads (liveAgents) — the latter catches agents that crashed or
// OOMed in a prior pass without ever being reaped.
//
// Run-instances are single-shot: a fresh bead is created each cycle, so a
// stranded one will never be retried. We close it (not re-open) to clear the
// residue. A task whose assignee is still live, or that was claimed too
// recently (within runInstanceReconcileMinAge) when the agent vanished in a
// prior pass, is left untouched.
func (r *StaleReaper) reconcileRunInstanceTasks(ctx context.Context, now time.Time, liveAgents, reapedAgents map[string]bool) int {
	result, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"task"},
		Statuses: []string{"in_progress"},
		Limit:    1000,
	})
	if err != nil {
		r.logger.Warn("failed to list in_progress tasks for run-instance reconcile", "error", err)
		return 0
	}

	reconciled := 0
	for _, t := range result.Beads {
		if t.Assignee == "" {
			continue
		}
		if !isRunInstanceTask(t) {
			continue
		}
		// Live assignee → leave alone (agent is still working it).
		if liveAgents[t.Assignee] {
			continue
		}
		reapedNow := reapedAgents[t.Assignee]
		// Assignee gone in a prior pass (crash/OOM): require a grace period so
		// we don't race a freshly-claimed task whose agent bead hasn't yet
		// surfaced in ListAgentBeads. Reaped-this-pass agents are definitively
		// gone and skip the grace.
		if !reapedNow && now.Sub(t.UpdatedAt) < runInstanceReconcileMinAge {
			continue
		}

		reason := "run-instance assignee reaped this pass"
		if !reapedNow {
			// The assignee vanished in a prior pass without being reaped. The
			// default provenance is crash/OOM — but an agent that stopped
			// cleanly via `gb stop --force` also lands here (its agent bead was
			// closed by the stop, not the reaper, so it's absent from
			// liveAgents). Before asserting a crash, check the assignee's most
			// recent agent bead for a genuine agent-recorded wrap-up: if present,
			// the agent stopped on purpose with work unfinished, not a crash.
			// This is defense-in-depth behind the gb stop --force truthful-close
			// (ka-0000006105) — it also covers older agents / non-run-instance
			// forced stops the CLI path doesn't close.
			reason = "run-instance assignee agent gone (crash/OOM, not reaped)"
			if r.assigneeStoppedCleanly(ctx, t.Assignee) {
				reason = "run-instance assignee stopped via gb stop with work unfinished"
			}
		}
		r.logger.Info("reconciling stranded run-instance task",
			"task", t.ID, "assignee", t.Assignee, "title", t.Title, "reason", reason)

		// CloseBead attributes the close to "gardenio" and records close_reason
		// for the audit trail — matching how the reaper closes agent beads in
		// the same pass (rather than a bare status PATCH that leaves closed_by
		// unset).
		if err := r.daemon.CloseBead(ctx, t.ID, map[string]string{
			"close_reason": "reaper: " + reason,
		}); err != nil {
			r.logger.Warn("failed to reconcile stranded run-instance task",
				"task", t.ID, "error", err)
			continue
		}
		reconciled++
	}
	return reconciled
}

// reconcileOpenRunInstanceTasks finds OPEN, never-claimed schedule/cron
// run-instance fire-record beads and closes them once they age past the grace.
//
// The trigger engine creates one run-instance bead per cron fire. The normal
// path is claim → in_progress → close-by-agent (or, if the agent dies,
// reconcileRunInstanceTasks closes the stranded in_progress bead). But a fire
// whose agent never claims it — spawn failed, project paused, no pod capacity,
// or the schedule fired faster than agents drained — leaves the bead sitting
// OPEN with an empty assignee, parentless, forever. These accumulate: ~930 of
// them were 77% of the 1214 "orphans" in the 2026-07-08 audit (ka-0000000075).
// reconcileRunInstanceTasks never touches them because it lists only
// in_progress beads and skips empty-assignee rows.
//
// Guards (all retained deliberately — the blast radius is mass-closing OPEN
// beads):
//   - isRunInstanceTask: strict source-label / fire-key predicate, the same one
//     the in_progress path trusts. The query is ALSO server-side label-bounded
//     to runInstanceSourceLabels so an unlabelled fire-key-only bead is the only
//     way a non-run-instance could slip in, and isRunInstanceTask re-checks.
//   - empty assignee: a claimed (assignee != "") open bead is a live/racing
//     claim, not an abandoned fire record — left to the in_progress path once it
//     transitions. (A non-empty assignee on an OPEN run-instance is anomalous;
//     skip it to stay conservative.)
//   - age past runInstanceReconcileMinAge: a just-fired bead may be seconds away
//     from being claimed; only close ones that have demonstrably gone unclaimed.
//
// Rollout gate: the close itself is gated by closeOpenRunInstancesEnabled(),
// which now defaults ON after the 37-day dry-run confirmed the count and
// predicate precision (ka-0000006054). An operator can set
// STALEREAPER_CLOSE_OPEN_RUNINSTANCES=false as a kill-switch to fall back to
// dry-run/log-only, in which case the function logs each candidate and returns
// the count that WOULD be closed without touching any bead.
//
// These fire records carry no assignee (never claimed), so the liveAgents /
// reapedAgents liveness maps used by the in_progress path are not needed here.
func (r *StaleReaper) reconcileOpenRunInstanceTasks(ctx context.Context, now time.Time) int {
	// Server-side label bound (LabelsAny = runInstanceSourceLabels) keeps this
	// query tight instead of pulling the entire open task backlog and truncating
	// at the limit. Sort updated_at ASC so that if the candidate set ever exceeds
	// the page cap, the OLDEST (most-overdue) fire records stay on the first page
	// and drain first; the surplus is picked up on subsequent passes.
	const openListLimit = 1000
	result, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:     []string{"task"},
		Statuses:  []string{"open"},
		LabelsAny: runInstanceSourceLabels,
		Sort:      "updated_at",
		Limit:     openListLimit,
	})
	if err != nil {
		r.logger.Warn("failed to list open run-instance tasks for reconcile", "error", err)
		return 0
	}
	if result.Total > len(result.Beads) {
		r.logger.Warn("open run-instance reconcile page truncated; oldest prioritized, surplus drains next pass",
			"returned", len(result.Beads), "total", result.Total, "limit", openListLimit)
	}

	// In dry-run the candidate set is the full ~930 backlog on every pass, so
	// cap the per-candidate log to a sample to avoid flooding the log each cycle;
	// the dry-run summary line still reports the exact total.
	const dryRunLogSample = 10

	enabled := r.closeOpenRunInstancesEnabled()
	handled := 0
	for _, t := range result.Beads {
		// A claimed open run-instance (assignee set) is a live/racing claim — the
		// in_progress path owns it once it transitions. Only the never-claimed,
		// empty-assignee fire records are ours.
		if t.Assignee != "" {
			continue
		}
		// Defense in depth behind the server-side label filter: re-check the
		// strict predicate in case an unlabelled fire-key-only bead slipped in.
		if !isRunInstanceTask(t) {
			continue
		}
		// Age gate: only close fire records that have demonstrably gone unclaimed.
		// A freshly-fired bead may be seconds from being claimed.
		if now.Sub(t.UpdatedAt) < runInstanceReconcileMinAge {
			continue
		}

		if !enabled {
			// Dry-run: count it and leave the bead alone so the rollout can
			// confirm the count/precision before enabling closes. Log only the
			// first few as a sample (the summary line reports the exact total).
			if handled < dryRunLogSample {
				r.logger.Info("dry-run: would close stranded open run-instance (never claimed)",
					"task", t.ID, "title", t.Title,
					"age", now.Sub(t.UpdatedAt).Round(time.Second),
					"hint", "close is disabled via STALEREAPER_CLOSE_OPEN_RUNINSTANCES=false; unset or set true to re-enable (default is now on)")
			}
			handled++
			continue
		}

		r.logger.Info("closing stranded open run-instance (never claimed)",
			"task", t.ID, "title", t.Title, "age", now.Sub(t.UpdatedAt).Round(time.Second))
		if err := r.daemon.CloseBead(ctx, t.ID, map[string]string{
			"close_reason": "reaper: stranded open run-instance (never claimed)",
		}); err != nil {
			r.logger.Warn("failed to close stranded open run-instance", "task", t.ID, "error", err)
			continue
		}
		handled++
	}
	if !enabled && handled > 0 {
		r.logger.Info("open run-instance reconcile dry-run summary",
			"candidates", handled,
			"note", "no beads closed; close disabled via STALEREAPER_CLOSE_OPEN_RUNINSTANCES=false (default is now on) — unset or set true to re-enable")
	}
	return handled
}

// reopenIssueReconcileMinAge is the minimum staleness a stranded claimed ISSUE
// task must reach before it is reopened when its assignee agent disappeared in a
// *prior* pass (crash/OOM, not reaped here). Tasks whose assignee was reaped in
// the current pass are reopened immediately — that agent is definitively gone.
// Mirrors runInstanceReconcileMinAge: it guards the race where a task was just
// claimed by an agent whose bead has not yet surfaced in ListAgentBeads.
const reopenIssueReconcileMinAge = 15 * time.Minute

// reopenStrandedIssueTasks finds ordinary claimed ISSUE task beads still
// in_progress whose assignee AGENT is gone, and REOPENS them (status=open,
// assignee cleared) with an audit comment so the work returns to `gb ready`.
//
// This is the SIGKILL backstop for EOL salvage (ka-0000002469, design
// ka-0000007236). When an agent is hard-killed (grace exceeded / OOM / node
// lost) no in-pod code runs, so the involuntary-SIGTERM reopen (Phase 2,
// hook.go reopenOrphanedClaimedTask) never fires and the issue is stranded
// in_progress with a dead assignee — the stale-claim tax on the next agent
// (advice ka-0000007726). Git salvage is unavoidably lost here (the controller
// has no worktree access), but the claim-release still survives.
//
// It deliberately mirrors reconcileRunInstanceTasks' liveness/grace guards but
// differs in two ways:
//   - It REOPENS (retriable issue work), not CLOSES (single-shot run-instances).
//   - Before reopening a prior-pass strand it requires the assignee to resolve
//     to a GONE (closed) AGENT bead via assigneeIsGoneAgent. This clears two
//     hazards at once: a human-claimed issue never resolves to a type=agent
//     bead (so it is never touched), and a still-active agent bead — even one
//     truncated out of the page-capped liveAgents map — proves the agent is
//     alive, so its task is left alone. Reaped-this-pass agents are definitively
//     gone and skip the lookup.
//
// Run-instance tasks are excluded (handled by reconcileRunInstanceTasks), and
// so are awaits_release tasks, which their release closes (gd-aaGqLb2cUk).
//
// Before reopening, two additional guards apply (gd-t0gm8q62Ac Part 2 — the
// liveness-reconciler half of the review-loop-escalation fix):
//   - taskHasOpenHandle: a live (unresolved) escalation or decision bead still
//     parks the task. Reopening out from under a pending verdict would race
//     Part 1's respawn-on-resolve, so the task is left untouched — its
//     resolution is what should bring the work back.
//   - openLinkedPR: the task carries a linked PR that is still open (or whose
//     state cannot be read). Blindly reopening risks a second crew duplicating
//     work already up for review, so the task is flagged with
//     task:orphaned-with-pr and an audit comment for a follow-through crew
//     instead of reopened. A task
//     whose linked PRs are all closed or merged is reopened, shedding the flag
//     (gd-ovzZSuuHny).
//
// Returns (reopened, flaggedOrphanedWithPR).
func (r *StaleReaper) reopenStrandedIssueTasks(ctx context.Context, now time.Time, liveAgents, reapedAgents map[string]bool) (int, int) {
	// Kinds=issue (no type restriction) mirrors Phase 2's ListAssignedTask
	// exactly, so this backstop reopens precisely the set of beads the in-pod
	// involuntary-SIGTERM path (hook.go) would have reopened had it been able to
	// run — task/bug/feature/epic/chore, i.e. the real-work bucket, excluding
	// data/config/agent beads.
	//
	// Sort updated_at ASC (oldest-touched first) so that if the in_progress set
	// ever exceeds the page cap, the beads dropped are the FRESHEST — which are
	// the live-agent claims this pass would skip anyway — while the stalest
	// beads (exactly the SIGKILL-stranded ones this backstop exists to salvage)
	// stay on the first page. The daemon default sort is created_at DESC, which
	// would drop the oldest stranded beads first — the opposite of what we want.
	const reopenListLimit = 1000
	result, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"in_progress"},
		Sort:     "updated_at",
		Limit:    reopenListLimit,
	})
	if err != nil {
		r.logger.Warn("failed to list in_progress issue tasks for reopen reconcile", "error", err)
		return 0, 0
	}
	// Surface page truncation so an operator can see the cap is being hit. The
	// reopen is idempotent (a reopened task drops to assignee="" and out of the
	// in_progress set), so an over-cap backlog still drains across successive
	// passes — but a persistently full page is worth an operator's attention.
	if result.Total > len(result.Beads) {
		r.logger.Warn("reopen reconcile page truncated; oldest stranded issues prioritized, surplus drains next pass",
			"returned", len(result.Beads), "total", result.Total, "limit", reopenListLimit)
	}

	reopened := 0
	flagged := 0
	prPass := r.newPRLookupPass()
	defer func() {
		if prPass.starved > 0 {
			r.logger.Warn("PR state lookup budget spent this pass; uncached linked PRs read as open and flag their tasks",
				"budget", prPass.budget, "starved_lookups", prPass.starved)
		}
	}()
	for _, t := range result.Beads {
		if t.Assignee == "" || !r.reclaimable(t) {
			continue
		}
		// Run-instances are single-shot and CLOSED by reconcileRunInstanceTasks;
		// never reopen them here.
		if isRunInstanceTask(t) {
			continue
		}
		// A task awaiting its release is done, not stranded: the deployment
		// that carries it closes it from in_progress (gd-aaGqLb2cUk).
		if beadsapi.AwaitsRelease(t.Fields) {
			continue
		}
		// Live assignee → leave alone (agent is still working it).
		if liveAgents[t.Assignee] {
			continue
		}
		reapedNow := reapedAgents[t.Assignee]
		// Assignee gone in a prior pass (crash/OOM): require a grace period so
		// we don't race a freshly-claimed task whose agent bead hasn't yet
		// surfaced in ListAgentBeads. Reaped-this-pass agents are definitively
		// gone and skip the grace.
		if !reapedNow && now.Sub(t.UpdatedAt) < reopenIssueReconcileMinAge {
			continue
		}
		// Confirm the assignee is a GONE AGENT before reopening. Two hazards to
		// clear at once:
		//   - Human-claimed issues must never be reopened out from under their
		//     owner (a human name does not resolve to a type=agent bead).
		//   - A LIVE agent's task must never be reopened. liveAgents is built
		//     from ListAgentBeads, which is itself page-capped; on a very large
		//     fleet a live agent can be truncated out of liveAgents, so absence
		//     from that map is NOT sufficient proof the agent is gone.
		// An agent reaped this pass is definitively gone (fast-path). Otherwise
		// require the assignee to resolve to a CLOSED agent bead: agents-by-name
		// only resolves type=agent beads (rules out humans), and a *closed*
		// status is positive proof the agent has terminated (rules out a
		// truncated-but-live agent — an active agent bead means "still alive →
		// skip"). Anything else (unresolvable, or resolvable but still active)
		// fails safe to skip.
		if !reapedNow && !r.assigneeIsGoneAgent(ctx, t.Assignee) {
			continue
		}

		reason := "assignee agent reaped this pass"
		if !reapedNow {
			reason = "assignee agent gone (crash/OOM or hard SIGKILL, not reaped)"
		}

		// A live escalation/decision still parks this task — its resolution
		// (and Part 1's respawn-on-resolve) is what should bring the work
		// back, not a backstop reopen racing that pending verdict.
		// gd-t0gm8q62Ac Part 2.
		if r.taskHasOpenHandle(ctx, t.ID) {
			continue
		}

		// A linked PR means real work is already up for review; reopening
		// here risks a second crew duplicating it. Flag instead. A PR that is
		// closed or merged is no longer up for review: such a task reopens like
		// any other strand (gd-ovzZSuuHny).
		prURL, settledPRs := r.openLinkedPR(ctx, t, prPass)
		if prURL != "" {
			if r.flagOrphanedWithPR(ctx, t, prURL, reason) {
				flagged++
			}
			continue
		}

		// Reopen-attempt cap (ka-0000002985 DEFECT 3). Mirrors the in-pod path
		// (hook.go reopenOrphanedClaimedTask): a task that has been reopened
		// MaxReopenAttempts times keeps hitting the same un-runnable condition and
		// dying the same way — re-dispatch just burns spend. Park it to
		// status=blocked (invisible to BOTH this scan and the in-pod path, which
		// both filter status=in_progress; and to `gb ready`, which scans
		// status=open) and surface it for a human via an audit comment instead of
		// reopening again. The counter lives on the ISSUE task bead so it survives
		// respawns and both reopen paths converge on one running total.
		attempts := beadsapi.ParseReopenAttempts(t.Fields)
		next := attempts + 1
		if next > r.maxReopenAttempts() {
			r.parkCappedStrandedTask(ctx, t, attempts, reason)
			continue
		}

		r.logger.Info("reopening stranded claimed issue task",
			"task", t.ID, "assignee", t.Assignee, "title", t.Title, "reason", reason,
			"attempt", next, "cap", r.maxReopenAttempts())

		openStatus := "open"
		clearedAssignee := ""
		req := beadsapi.UpdateBeadRequest{
			Status:   &openStatus,
			Assignee: &clearedAssignee,
			Fields: map[string]any{
				beadsapi.ReopenAttemptsField: strconv.Itoa(next),
				beadsapi.ReapedAssigneeField: t.Assignee,
			},
		}
		// A task flagged on an earlier pass, whose PR has since closed or
		// merged, sheds the flag as it reopens.
		if slices.Contains(t.Labels, orphanedWithPRLabel) {
			labels := slices.DeleteFunc(slices.Clone(t.Labels), func(l string) bool { return l == orphanedWithPRLabel })
			req.Labels = &labels
		}
		if err := r.daemon.UpdateBead(ctx, t.ID, req); err != nil {
			r.logger.Warn("failed to reopen stranded issue task", "task", t.ID, "error", err)
			continue
		}

		// Audit comment mirrors the in-pod reopen (hook.go reopenOrphanedClaimedTask)
		// so the trail reads the same whichever path fired. Best-effort: a
		// comment failure does not undo the reopen.
		comment := fmt.Sprintf(
			"Reopened by stale-reaper: agent `%s` (%s) left this task in_progress with no `gb done` — "+
				"an involuntary mid-work death (hard SIGKILL/OOM/node loss) where no in-pod salvage could run. "+
				"Git WIP is unrecoverable, but the claim is released: returned to the ready queue for another "+
				"agent. (EOL salvage Phase 2b ka-0000002469)",
			t.Assignee, reason)
		if len(settledPRs) > 0 {
			comment += fmt.Sprintf(" Its linked PR is no longer up for review (%s), so it was reopened rather than "+
				"flagged `%s`; start from main. (gd-ovzZSuuHny)", strings.Join(settledPRs, ", "), orphanedWithPRLabel)
		}
		if err := r.daemon.AddComment(ctx, t.ID, "gardenio", comment); err != nil {
			r.logger.Warn("reopened issue task but audit comment failed", "task", t.ID, "error", err)
		}
		reopened++
	}
	return reopened, flagged
}

// maxReopenAttempts is the reopen cap (ka-0000002985 DEFECT 3), defaulting to
// beadsapi.MaxReopenAttempts and overridable via GB_MAX_REOPEN_ATTEMPTS (a
// positive integer). A non-positive or malformed override falls back to the
// default rather than disabling the cap — the cap is a safety valve and must not
// be silently switched off by a bad value. Kept a method to mirror the in-pod
// helper of the same name (hook.go) so both reopen paths read one env override.
func (r *StaleReaper) maxReopenAttempts() int {
	if v := strings.TrimSpace(os.Getenv("GB_MAX_REOPEN_ATTEMPTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return beadsapi.MaxReopenAttempts
}

// parkCappedStrandedTask stops auto-dispatching a stranded issue task that has hit
// the reopen cap and surfaces it for a human (ka-0000002985 DEFECT 3). It is the
// controller-side mirror of hook.go parkCappedTask: status=blocked (invisible to
// both reopen scans, which filter status=in_progress, and to `gb ready`, which
// scans status=open — so re-dispatch stops with no scheduler change), assignee
// cleared, cap fields stamped, and an audit comment that auto-forwards to Slack so
// a human is notified. attempts is the count BEFORE this (capping) attempt.
// Best-effort: a comment failure does not undo the park.
func (r *StaleReaper) parkCappedStrandedTask(ctx context.Context, t *beadsapi.BeadDetail, attempts int, reason string) {
	blocked := "blocked"
	clearedAssignee := ""
	if err := r.daemon.UpdateBead(ctx, t.ID, beadsapi.UpdateBeadRequest{
		Status:   &blocked,
		Assignee: &clearedAssignee,
		Fields: map[string]any{
			beadsapi.ReopenAttemptsField:  strconv.Itoa(attempts + 1),
			beadsapi.ReopenCappedField:    "true",
			beadsapi.ReopenCapReasonField: reason,
			beadsapi.ReapedAssigneeField:  t.Assignee,
		},
	}); err != nil {
		r.logger.Warn("failed to park capped stranded issue task", "task", t.ID, "error", err)
		return
	}
	r.logger.Warn("PARKED stranded issue task after reopen cap reached; surfaced for a human",
		"task", t.ID, "attempts", attempts+1, "cap", r.maxReopenAttempts(), "reason", reason)
	comment := fmt.Sprintf(
		":no_entry: *Reopen cap reached* — task `%s` has been reopened %d times after involuntary agent deaths "+
			"(last: %s) and keeps failing the same way. Auto-dispatch STOPPED: parked to `status=blocked` so the "+
			"stale-reaper backstop and scheduler no longer respawn against it. A repeated same-way failure almost "+
			"always means the WORK is un-runnable as specified (a project-wide credential/auth failure, a missing "+
			"project record, or a bad task spec) — requeuing a fresh agent cannot fix that. A human should diagnose "+
			"the root cause, then set `status=open` to resume. (ka-0000002985 DEFECT 3)",
		t.ID, attempts+1, reason)
	if err := r.daemon.AddComment(ctx, t.ID, "gardenio", comment); err != nil {
		r.logger.Warn("parked capped stranded issue task but audit comment failed", "task", t.ID, "error", err)
	}
}

// strandedOpenAssigneeWindow bounds how far back clearStrandedOpenAssignees
// looks. A half-reap (ka-0000000908) is a `kd update --status open` write, which
// bumps updated_at — so a genuinely half-reaped bead is ALWAYS recently touched.
// Bounding the scan to a recent updated_at window is both correct (the target
// class is fresh by construction) and mandatory for cost: the fleet carries
// thousands of legitimately open+assigned beads (ancient `Mention:` tasks tagged
// to long-dead threads, molecules, human-claimed work), and an unbounded
// open-status scan would pull megabytes to touch a handful of rows — exactly the
// controller-OOM / silent-truncation footgun that advice ka-0000003555 warns
// against. The window is generous (a gardener cycle plus slack) so a half-reap is
// always inside it on the very next reaper pass. It is deliberately LONGER than
// the reaper interval (~5m) so a bead half-reaped between passes is never missed.
const strandedOpenAssigneeWindow = 6 * time.Hour

// clearStrandedOpenAssignees is the code backstop for the half-reap stranding
// class (ka-0000000908). It finds ISSUE task beads that are ALREADY status=open
// but still carry a DEAD assignee — the residue a gardener leaves when it reaps a
// zombie with `kd update --status open` but forgets `--assignee ""`. That
// combination is invisible to every net: every dispatch lane filters on
// assignee=="", `gb ready` hides assigned beads, and the in_progress zombie sweep
// (and reopenStrandedIssueTasks above) only look at status=in_progress. So the
// bead sits open+assigned+dead forever, silently — a real P2 bug cost ~16h of
// invisibility this way. This sweep clears the dead assignee so the work
// re-enters `gb ready` on the next gardener tick, making a future half-reap
// self-heal rather than needing a human to notice.
//
// It is the status=open sibling of reopenStrandedIssueTasks and reuses its
// EXACT liveness guards so the two agree on which assignees are gone:
//   - Run-instances are excluded (single-shot; owned by the run-instance path).
//   - A LIVE assignee (in liveAgents, or resolving by-name to an ACTIVE agent
//     bead even if truncated out of the page cap) is left alone.
//   - A prior-pass strand requires the assignee to resolve to a GONE (CLOSED)
//     agent bead via assigneeIsGoneAgent — which is exactly what protects the
//     thousands of legitimately-open beads whose assignee is a human (no agent
//     bead resolves) or a long-dead thread that no longer resolves to an agent
//     bead at all. Only a name that resolves to a closed type=agent bead — a
//     genuinely-reaped crew — has its assignee cleared.
//   - The grace window guards the race where a bead was just claimed and its
//     agent bead has not yet surfaced.
//
// Unlike reopenStrandedIssueTasks it does NOT touch status (the bead is already
// open — that is the whole point) and clears ONLY the assignee.
func (r *StaleReaper) clearStrandedOpenAssignees(ctx context.Context, now time.Time, liveAgents, reapedAgents map[string]bool) int {
	// Server-side updated_at window keeps this query tight: only recently-touched
	// open issue beads can be half-reaps (see strandedOpenAssigneeWindow). This
	// pushes the reduction into the daemon instead of walking the full open
	// backlog (advice ka-0000003555). Sort updated_at ASC so that if the window's
	// candidate set ever exceeds the page cap, the OLDEST (most-overdue) strands
	// stay on the first page and drain first; the sweep is idempotent (a cleared
	// bead drops out of the assignee!="" test next pass) so any surplus drains
	// across successive passes.
	const openListLimit = 1000
	result, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:        []string{"issue"},
		Statuses:     []string{"open"},
		UpdatedAfter: now.Add(-strandedOpenAssigneeWindow),
		Sort:         "updated_at",
		Limit:        openListLimit,
	})
	if err != nil {
		r.logger.Warn("failed to list open issue tasks for half-reap reconcile", "error", err)
		return 0
	}
	if result.Total > len(result.Beads) {
		r.logger.Warn("half-reap reconcile page truncated; oldest strands prioritized, surplus drains next pass",
			"returned", len(result.Beads), "total", result.Total, "limit", openListLimit)
	}

	cleared := 0
	for _, t := range result.Beads {
		// Only open beads that still carry an assignee are candidates — an empty
		// assignee is a correctly-reaped (or never-claimed) bead already in the
		// ready queue.
		if t.Assignee == "" || !r.reclaimable(t) {
			continue
		}
		// Run-instances are single-shot residue owned by the run-instance paths
		// (closed, not reopened); never clear their assignee here.
		if isRunInstanceTask(t) {
			continue
		}
		// Live assignee → the crew is (or may still be) on it; leave alone. This
		// covers a bead legitimately open+assigned by a live agent mid-work.
		if liveAgents[t.Assignee] {
			continue
		}
		reapedNow := reapedAgents[t.Assignee]
		// Assignee gone in a prior pass: require a grace period so we don't race a
		// freshly-claimed bead whose agent bead hasn't surfaced yet. Reaped-this-
		// pass agents are definitively gone and skip the grace.
		if !reapedNow && now.Sub(t.UpdatedAt) < reopenIssueReconcileMinAge {
			continue
		}
		// Confirm the assignee is a GONE AGENT before clearing. This is the guard
		// that makes the sweep safe against the thousands of legitimately
		// open+assigned beads: a human assignee never resolves to a type=agent
		// bead (skip), and a still-active agent — even one truncated out of the
		// page-capped liveAgents map — resolves to an ACTIVE bead (skip). Only a
		// name resolving to a CLOSED type=agent bead is positive proof of a
		// genuinely-reaped crew whose claim was half-cleared.
		if !reapedNow && !r.assigneeIsGoneAgent(ctx, t.Assignee) {
			continue
		}

		reason := "assignee agent reaped this pass"
		if !reapedNow {
			reason = "assignee agent gone (closed), status already open (half-reap)"
		}
		r.logger.Info("clearing dead assignee off stranded open issue task",
			"task", t.ID, "assignee", t.Assignee, "title", t.Title, "reason", reason)

		clearedAssignee := ""
		if err := r.daemon.UpdateBead(ctx, t.ID, beadsapi.UpdateBeadRequest{
			Assignee: &clearedAssignee,
			Fields:   map[string]any{beadsapi.ReapedAssigneeField: t.Assignee},
		}); err != nil {
			r.logger.Warn("failed to clear dead assignee off stranded open issue task", "task", t.ID, "error", err)
			continue
		}

		// Audit comment so the recovery is discoverable. Best-effort: a comment
		// failure does not undo the clear.
		comment := fmt.Sprintf(
			"Assignee cleared by stale-reaper: this task was status=open but still assigned to `%s` (%s) — "+
				"a half-reap (status reset without `--assignee \"\"`) that stranded it open+dead-assignee, "+
				"invisible to every dispatch lane (all require assignee==\"\"). Assignee cleared so the work "+
				"re-enters the ready queue. (half-reap backstop ka-0000000908)",
			t.Assignee, reason)
		if err := r.daemon.AddComment(ctx, t.ID, "gardenio", comment); err != nil {
			r.logger.Warn("cleared dead assignee but audit comment failed", "task", t.ID, "error", err)
		}
		cleared++
	}
	return cleared
}

// assigneeIsGoneAgent reports whether assigneeName resolves to a type=agent
// bead that has TERMINATED (closed status). It is the reopen gate for the
// prior-pass branch (an agent reaped this pass is handled by a fast-path and
// never reaches here).
//
// Two properties matter:
//   - agents-by-name only ever resolves type=agent beads, so a human name
//     usually does not resolve. It can (an agent bead titled with a human's
//     email), so callers also skip email assignees (reclaimable).
//   - It requires a CLOSED agent bead. FindMostRecentAgentBead prefers an
//     ACTIVE agent bead and only falls back to the closed match when none is
//     active (client.go). So if it returns an active bead, the agent is still
//     alive — even if it was truncated out of the page-capped liveAgents map —
//     and we must NOT reopen its task. Only a closed bead is positive proof the
//     agent has terminated.
//
// Best-effort: a lookup miss, error, or still-active bead returns false (fail
// safe — do not reopen), so this never manufactures a gone-agent verdict it
// cannot substantiate.
func (r *StaleReaper) assigneeIsGoneAgent(ctx context.Context, assigneeName string) bool {
	if assigneeName == "" {
		return false
	}
	detail, err := r.daemon.FindMostRecentAgentBead(ctx, assigneeName)
	if err != nil || detail == nil {
		return false
	}
	// A closed agent bead is positive proof the agent terminated. An active
	// one (open/in_progress/blocked/deferred) means still alive → skip.
	return detail.Status == "closed"
}

// stalePendingHandleTypes are the bead types that can serve as a live
// re-entry point for a task — an open escalation or decision whose
// resolution is what should return the task to a runnable state, not a
// backstop reopen. Mirrors bridge.mentionCatchupParkHandleTypes
// (gitlab_mention_catchup.go hasOpenParkHandle); reimplemented locally
// because statusreporter has no dependency on the bridge package.
var stalePendingHandleTypes = []string{"escalation", "decision"}

// stalePendingHandleStatuses are the statuses under which an escalation or
// decision bead still counts as "parking" a task — its resolution has not
// yet landed, so the task must not be silently reopened or flagged out from
// under it.
var stalePendingHandleStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// stalePendingHandleListLimit bounds the open-handle probe query. One task
// carries at most a handful of open escalations/decisions at a time; this
// only guards a runaway query.
const stalePendingHandleListLimit = 20

// taskHasOpenHandle reports whether taskID has a live (unresolved)
// escalation or decision bead pointing at it via requesting_task
// (gd-t0gm8q62Ac Part 2). Such a bead is a live re-entry point: the task must
// be left in_progress so the eventual escalation/decision resolution — and
// Part 1's respawn-on-resolve — can resume it, rather than the reaper
// reopening or reassigning the task out from under a pending verdict.
//
// Fail-safe: a list error returns true ("assume parked") so a daemon hiccup
// can never cause a premature reopen.
func (r *StaleReaper) taskHasOpenHandle(ctx context.Context, taskID string) bool {
	if taskID == "" {
		return false
	}
	res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    stalePendingHandleTypes,
		Statuses: stalePendingHandleStatuses,
		Fields:   map[string]string{"requesting_task": taskID},
		Limit:    stalePendingHandleListLimit,
	})
	if err != nil {
		r.logger.Warn("task-has-open-handle probe failed, assuming parked (fail safe)",
			"task", taskID, "error", err)
		return true
	}
	return len(res.Beads) > 0
}

// orphanedWithPRLabel marks an in_progress ISSUE task whose assignee agent is
// gone but which carries a linked PR — the reaper flags it for a
// follow-through crew instead of blindly reopening it, which would risk a
// second crew duplicating work already up for review. gd-t0gm8q62Ac Part 2.
const orphanedWithPRLabel = "task:orphaned-with-pr"

// prURLPattern matches a GitHub PR URL of the shape crews post when they open
// a pull request (https://github.com/<org>/<repo>/pull/<n>). Group 1 is
// <org>/<repo>.
var prURLPattern = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/pull/\d+`)

// prRepo returns the lower-cased <org>/<repo> of a prURLPattern match, or "".
func prRepo(prURL string) string {
	m := prURLPattern.FindStringSubmatch(prURL)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// linkedPRURLs returns every distinct GitHub PR URL found on task t, the
// task's own first: the PR in its mr_url field (set by `gb mr link` and the
// GitHub bridge), then the comment trail newest first, then the bead's other
// Fields, Notes and Description. The comment trail costs one GetComments
// call, only made for a candidate that has already passed every other
// stranded-task filter; if it fails, the bead's own text still counts.
//
// When mr_url names a GitHub PR, a PR in another repository is a mention (a
// reference in a comment or the description), not this task's work, and is
// left out, so unrelated references do not push the task over
// maxPRLookupsPerTask (gd-wahYHJndnb). Without one, every linked PR counts.
func (r *StaleReaper) linkedPRURLs(ctx context.Context, t *beadsapi.BeadDetail) []string {
	var urls []string
	ownRepo := ""
	if own := prURLPattern.FindString(t.Fields["mr_url"]); own != "" {
		urls = append(urls, own)
		ownRepo = prRepo(own)
	}
	keep := func(m string) bool {
		return !slices.Contains(urls, m) && (ownRepo == "" || prRepo(m) == ownRepo)
	}
	add := func(text string) {
		for _, m := range prURLPattern.FindAllString(text, -1) {
			if keep(m) {
				urls = append(urls, m)
			}
		}
	}
	comments, err := r.daemon.GetComments(ctx, t.ID)
	if err != nil {
		r.logger.Warn("linked-PR comment scan failed, using the bead's own text only", "task", t.ID, "error", err)
	}
	slices.SortStableFunc(comments, func(a, b beadsapi.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for i := len(comments) - 1; i >= 0; i-- {
		// Within one comment the last URL is taken as the newest: "closed
		// <old>, superseded by <new>" names the old PR first.
		found := prURLPattern.FindAllString(comments[i].Text, -1)
		for j := len(found) - 1; j >= 0; j-- {
			if keep(found[j]) {
				urls = append(urls, found[j])
			}
		}
	}
	keys := make([]string, 0, len(t.Fields))
	for k := range t.Fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		add(t.Fields[k])
	}
	add(t.Notes)
	add(t.Description)
	return urls
}

// openLinkedPR decides whether task t still has a PR up for review. It returns
// the PR to flag the task with, or "" when there is none, in which case
// settled lists the linked PRs found closed or merged ("<url> (merged)").
//
// With no PR state reader, every linked PR counts as open (the first is
// returned). Otherwise the first open PR in linkedPRURLs order is returned; a
// lookup that fails, a pass whose lookup budget is spent, or more linked PRs
// than maxPRLookupsPerTask, also returns a PR, so an unknown state flags
// rather than reopens.
func (r *StaleReaper) openLinkedPR(ctx context.Context, t *beadsapi.BeadDetail, pass *prLookupPass) (flag string, settled []string) {
	urls := r.linkedPRURLs(ctx, t)
	if len(urls) == 0 {
		return "", nil
	}
	if r.prState == nil || len(urls) > maxPRLookupsPerTask {
		return urls[0], nil
	}
	unknown := ""
	for _, u := range urls {
		state, err := r.lookupPRState(ctx, u, pass)
		switch {
		case errors.Is(err, errPRLookupBudgetSpent):
			// Logged once per pass by reopenStrandedIssueTasks.
			if unknown == "" {
				unknown = u
			}
		case err != nil:
			r.logger.Warn("linked PR state lookup failed, treating the PR as open", "task", t.ID, "pr", u, "error", err)
			if unknown == "" {
				unknown = u
			}
		case state == PRStateClosed || state == PRStateMerged:
			settled = append(settled, u+" ("+state+")")
		default:
			return u, nil
		}
	}
	if unknown != "" {
		return unknown, nil
	}
	return "", settled
}

// lookupPRState is r.prState behind prStateCache, each call bounded by
// prLookupTimeout and by what is left of pass (nil: no pass budget). Only
// successful answers are cached, so a failed lookup is retried on the next
// pass.
func (r *StaleReaper) lookupPRState(ctx context.Context, prURL string, pass *prLookupPass) (string, error) {
	now := r.now()
	r.prStateMu.Lock()
	c, ok := r.prStateCache[prURL]
	r.prStateMu.Unlock()
	if ok && now.Sub(c.at) < prStateTTL(c.state) {
		return c.state, nil
	}
	timeout := prLookupTimeout
	if pass != nil {
		if pass.remaining <= 0 {
			pass.starved++
			return "", errPRLookupBudgetSpent
		}
		timeout = min(timeout, pass.remaining)
	}
	start := time.Now() // before the deadline is set, so a call cut off at it spends the whole remaining budget
	lctx, cancel := context.WithTimeout(ctx, timeout)
	state, err := r.prState(lctx, prURL)
	cancel()
	if pass != nil {
		pass.remaining -= time.Since(start)
	}
	if err != nil {
		return "", err
	}
	r.prStateMu.Lock()
	if r.prStateCache == nil {
		r.prStateCache = map[string]cachedPRState{}
	}
	for k, v := range r.prStateCache {
		if now.Sub(v.at) >= prStateTTL(v.state) {
			delete(r.prStateCache, k)
		}
	}
	r.prStateCache[prURL] = cachedPRState{state: state, at: now}
	r.prStateMu.Unlock()
	return state, nil
}

// flagOrphanedWithPR labels task t with orphanedWithPRLabel and posts an
// audit comment naming the linked PR, instead of the backstop reopening the
// task out from under work that already has a PR up for review. Idempotent:
// a task already carrying the label is left untouched (no re-label, no
// re-comment) and the return value tells the caller nothing new happened, so
// a repeated pass over the same stranded task does not spam a comment every
// cycle. Best-effort: a comment failure does not undo the label. Returns true
// only when the label was newly added.
func (r *StaleReaper) flagOrphanedWithPR(ctx context.Context, t *beadsapi.BeadDetail, prURL, reason string) bool {
	if slices.Contains(t.Labels, orphanedWithPRLabel) {
		return false
	}
	newLabels := append(append([]string{}, t.Labels...), orphanedWithPRLabel)
	if err := r.daemon.UpdateBead(ctx, t.ID, beadsapi.UpdateBeadRequest{
		Labels: &newLabels,
	}); err != nil {
		r.logger.Warn("failed to label orphaned-with-pr task", "task", t.ID, "error", err)
		return false
	}
	r.logger.Info("flagged orphaned in_progress task with linked PR instead of reopening",
		"task", t.ID, "assignee", t.Assignee, "pr", prURL, "reason", reason)
	comment := fmt.Sprintf(
		"Flagged by stale-reaper: assignee `%s` is gone (%s), but this task has a linked PR (%s) — "+
			"NOT reopened, to avoid a second crew duplicating work already up for review. Labeled `%s` "+
			"for a follow-through crew to pick up the PR (review/address feedback/merge) rather than "+
			"redoing the task from scratch. (gd-t0gm8q62Ac Part 2)",
		t.Assignee, reason, prURL, orphanedWithPRLabel)
	if err := r.daemon.AddComment(ctx, t.ID, "gardenio", comment); err != nil {
		r.logger.Warn("flagged orphaned-with-pr task but audit comment failed", "task", t.ID, "error", err)
	}
	return true
}

// isRunInstanceTask reports whether a task bead is a schedule/cron/event
// run-instance produced by the trigger engine — identified by a source label
// (see runInstanceSourceLabels) or a schedule/multi fire-key field. These are
// the per-cycle beads that strand in_progress when their agent is reaped.
func isRunInstanceTask(t *beadsapi.BeadDetail) bool {
	for _, l := range t.Labels {
		if slices.Contains(runInstanceSourceLabels, l) {
			return true
		}
	}
	// Fallback to fire-key fields in case labels were stripped: the schedule
	// and multi trigger sources stamp these on every run-instance.
	if t.Fields["schedule_fire_key"] != "" || t.Fields["multi_fire_key"] != "" {
		return true
	}
	return false
}

// reapCloseState maps a reap reason to the agent_state the bead is closed with.
// Most reaps are genuine failures (a "working" pod that went silent, a spawn
// that never started) and get "failed" so the card shows ❌. But two reasons
// are benign and must NOT render as a crash:
//
//   - "orphan: source task ... already closed" — the agent's work was finished
//     (often by the agent itself, e.g. a gardener that closed its epic and let
//     the loop go dormant). The reaper is just cleaning up the lingering pod.
//   - "orphan: terminal state ..." — the agent already reached a terminal state
//     (done/failed/rate_limited) and merely failed to close its own bead; the
//     reaper should preserve that prior state, not overwrite a clean done with
//     a failure.
//
// priorState is the bead's agent_state before the reap, used to preserve an
// already-terminal state for the terminal-state orphan case.
func reapCloseState(reason, priorState string) string {
	switch {
	case strings.HasPrefix(reason, "orphan: source task"):
		return "done"
	case strings.HasPrefix(reason, "orphan: frozen thaw target"):
		// A suspended (frozen) agent whose snapshot expired stopped cleanly —
		// the lost snapshot degrades the next wake to a cold start, it does not
		// make the past session a crash (ka-0000002104).
		return "done"
	case strings.HasPrefix(reason, "orphan: terminal state"):
		// Preserve the prior terminal state; default to done if unknown.
		switch priorState {
		case "done", "failed", "rate_limited":
			return priorState
		}
		return "done"
	default:
		return "failed"
	}
}

// runnerAwayAfter is how long after its last heartbeat a runner counts as
// away: three missed 30 s beats (local-agents spec §5; the bridge's
// runnerOnlineWindow).
const runnerAwayAfter = 90 * time.Second

// laptopReapReason is the close reason for a laptop agent whose runner was
// revoked or no longer exists.
const laptopReapReason = "local runner revoked or gone"

type runnerStatus int

const (
	runnerOnline runnerStatus = iota
	runnerAway
	runnerGone
	// runnerUnknown: the runner bead could not be read (a daemon blip). The
	// agent is left as it is until a later pass can tell.
	runnerUnknown
)

// runnerStatusFor reads the runner bead behind a laptop agent. Missing,
// closed or revoked is gone (the agent closes); an explicit away or a stale
// heartbeat is away (the agent pauses and holds, D5).
func (r *StaleReaper) runnerStatusFor(ctx context.Context, runnerID string, now time.Time) runnerStatus {
	detail, err := r.daemon.GetBead(ctx, runnerID)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return runnerGone
		}
		r.logger.Warn("failed to read runner for laptop agent", "runner", runnerID, "error", err)
		return runnerUnknown
	}
	if detail == nil || detail.Status == "closed" {
		return runnerGone
	}
	switch detail.Fields["state"] {
	case "revoked":
		return runnerGone
	case "away":
		return runnerAway
	}
	seen, err := time.Parse(time.RFC3339, detail.Fields["last_seen_at"])
	if err != nil || now.Sub(seen) > runnerAwayAfter {
		return runnerAway
	}
	return runnerOnline
}

// laptopHeldStates are the agent_states the lid-close policy leaves alone: a
// laptop agent already stopping or finished is gb's to close (or /kill's),
// not something to pause or resume.
var laptopHeldStates = map[string]bool{
	"stopping": true, "stop_requested": true, "done": true, "failed": true, "killed": true,
}

// laptopAgentHold applies the lid-close and sleep policy (local-agents D5) to
// a laptop agent (execution_target=laptop), which has no pod and is never
// reaped on staleness. Its runner decides:
//
//   - gone (revoked, closed, missing): reason is laptopReapReason and the
//     agent closes as failed through the normal reap path.
//   - away (lid closed, asleep, gb runner leave): the agent is set to
//     agent_state=paused and held with no timeout. The bridge posts the
//     paused message in its thread; /kill releases it.
//   - online: a paused agent goes back to working (the laptop's mail
//     subscriber drains what arrived meanwhile on reconnect); any other is
//     left to gb, which closes its bead when the session exits.
//
// held reports that the agent stays open this pass. A laptop agent with no
// runtime_id (a session not joined to a runner) has nothing to watch and is
// always held.
func (r *StaleReaper) laptopAgentHold(ctx context.Context, b beadsapi.AgentBead, now time.Time, stats *StaleReaperStats) (reason string, held bool) {
	runnerID := b.Metadata["runtime_id"]
	if runnerID == "" {
		return "", true
	}
	switch r.runnerStatusFor(ctx, runnerID, now) {
	case runnerGone:
		return laptopReapReason, false
	case runnerAway:
		if b.AgentState == "paused" || laptopHeldStates[b.AgentState] {
			return "", true
		}
		if err := r.daemon.UpdateBeadFields(ctx, b.ID, map[string]string{"agent_state": "paused"}); err != nil {
			r.logger.Warn("failed to pause laptop agent", "bead", b.ID, "error", err)
			return "", true
		}
		stats.Paused++
		r.logger.Info("laptop agent paused: runner away",
			"bead", b.ID, "agent", b.AgentName, "runner", runnerID, "prior_state", b.AgentState)
	case runnerOnline:
		if b.AgentState != "paused" {
			return "", true
		}
		if err := r.daemon.UpdateBeadFields(ctx, b.ID, map[string]string{"agent_state": "working"}); err != nil {
			r.logger.Warn("failed to resume laptop agent", "bead", b.ID, "error", err)
			return "", true
		}
		stats.Resumed++
		r.logger.Info("laptop agent resumed: runner online",
			"bead", b.ID, "agent", b.AgentName, "runner", runnerID)
	}
	return "", true
}

// reapReason returns a non-empty reason string if the agent bead should be
// reaped, or "" if it should be left alone. podExists reports whether a live
// pod currently exists for this bead — it gates the empty-state fast-fail so a
// pod that merely hasn't reported status yet is never early-reaped
// (ka-0000004685).
func (r *StaleReaper) reapReason(ctx context.Context, b beadsapi.AgentBead, now time.Time, podExists bool) string {
	// Skip if updated_at is missing (should not happen, but be safe).
	if b.UpdatedAt.IsZero() {
		return ""
	}

	// Never reap a lambda_microvm agent (C5, ka-0000001837). The stale reaper is
	// fundamentally a pod reaper: it kills agents whose pod stopped updating and
	// deletes that pod. A MicroVM agent has NO pod — it runs as a MicroVM whose
	// lifecycle the reconciler owns via runtime status (GetMicrovm) — so its
	// bead legitimately sits "working" with a stale updated_at (no SyncAll bump)
	// and would be wrongly reaped here. Its DeleteAgentPod would also no-op,
	// leaving the runtime orphaned. Exempt it entirely and let the C5 reconcile
	// + leak GC drive its teardown.
	if b.Metadata["execution_target"] == podmanager.ExecutionTargetLambdaMicroVM {
		return ""
	}
	// A local agent (execution_target=laptop) has no pod either, and is held,
	// not reaped, while its laptop is away (local-agents D5). gb closes its
	// bead when the session exits; /kill releases it otherwise. Reap routes it
	// through laptopAgentHold and never gets here; this guard keeps any other
	// caller from applying the pod rules to it.
	if podmanager.IsLaptopTarget(b.Metadata["execution_target"]) {
		return ""
	}

	// Never reap a PARKED persistent identity awaiting its wake (persistent-agents
	// Phase 2, task ka-0000004902). A persistent agent's idle-TTL park stamps
	// successor=same + a FUTURE next_respawn_at on its bead; the controller
	// wake-scanner reopens it when that wake time arrives, and the reconciler then
	// recreates the pod under the same name. Between park and wake such a bead is
	// legitimately podless with no upper age bound — the direct analog of the
	// freeze/thaw snapshot exemption above (agent_state=stopping). Reaping it would
	// destroy the wake intent and turn a resumable identity into a terminal one.
	// Scoped to a wake time STILL IN THE FUTURE: once next_respawn_at ≤ now the
	// wake is due and the scanner (not the reaper) owns the bead; and a bead the
	// scanner already woke no longer carries next_respawn_at at all, so it correctly
	// falls through to the normal rules. Today a parked bead is CLOSED (the reaper
	// lists only active statuses, so it never even reaches here), making this a
	// forward-looking guard for the P3 dormant-open-bead model; it is a no-op for
	// every non-parked bead.
	if b.Metadata[beadsapi.FieldSuccessor] == beadsapi.SuccessorSame &&
		wakeInFuture(b.Metadata[beadsapi.FieldNextRespawnAt], now) {
		return ""
	}

	switch b.AgentState {
	case "working", "":
		// Fast-fail an EMPTY-state bead that never got a pod (ka-0000004685).
		// An empty agent_state means the spawn never reached even "spawning";
		// combined with genuine pod ABSENCE (podExists=false), the bead is
		// wedged — no pod is coming without a fresh spawn — and would otherwise
		// share the 2h base timeout below, sitting as an open, terminal-less
		// "gardener with no session" card for up to 2h. Reap it on the much
		// shorter noPodReapAfter window so the zombie clears in minutes and the
		// gardener re-fires. Only when NO pod exists: an empty-state bead WITH a
		// live pod is merely slow to report (a reconciler-created Pending pod
		// stays empty-state until the next SyncAll stamps "spawning"), so it
		// must keep the full base-timeout path below and never be early-reaped.
		if b.AgentState == "" && !podExists && now.Sub(b.UpdatedAt) > noPodReapAfter {
			return fmt.Sprintf("stale: no pod created for %s (spawn wedged)", now.Sub(b.UpdatedAt).Round(time.Second))
		}
		// Runtime-ceiling watchdog (bug ka-0000000716, extended to every role by
		// gd-X2eSin2J80): an agent that has run past its role's absolute ceiling
		// — measured from created_at, NOT updated_at — is a wedged pod burning
		// spend that the updated_at-based checks below can never catch (a
		// churning agent keeps updated_at fresh). This is the DESTRUCTIVE
		// reap-side complement to the non-destructive lease-side age bound
		// (!6050); it is guarded on pod presence + MR mid-flight and armed by an
		// operator tunable. Checked before the orphan/staleness rules so a
		// still-fresh-updated_at wedge is still caught.
		if reason := r.runtimeReapReason(ctx, b, now, podExists); reason != "" {
			return reason
		}
		// A "working" agent whose source task has already been closed is an
		// orphan: another agent finished the work, or the agent itself closed
		// the task but never ran `gb stop` to despawn.
		//
		// Do NOT reap instantly. The pod may still be busy wrapping up
		// (committing, pushing an MR, posting a squawk, calling `gb stop`), or
		// it may be a gardener that closed its per-cycle task and is looping
		// into the next iteration. Killing it now loses that work and produces
		// the "idle crew" churn operators see (ka-0000000018).
		//
		// Gate on a grace window measured against updated_at. updated_at is a
		// reliable busy/idle proxy: SyncAll bumps it every cycle while the pod
		// is busy but skips the bump when coop reports idle (ka-0000003615).
		// So a busy pod keeps a fresh updated_at and is never orphan-reaped;
		// only a genuinely-idle pod that has been parked past the grace window
		// (and merely forgot `gb stop`) is reaped. Gardeners get a longer window
		// to cover the idle gap between loop cycles.
		if taskID := r.sourceTaskID(b); taskID != "" {
			// A "working" agent with no genuine turn for longer than the
			// single-claim guard's ProgressStaleThreshold, whose task has since
			// been closed or claimed by ANOTHER agent, is dead weight
			// (gd-udek4CJVRY). The updated_at gate below cannot catch it: coop
			// pinned the state at "working" after a --resume whose prompt never
			// submitted, so SyncAll kept bumping updated_at for 8h46m while the
			// gardener had already declared the crew dead, re-dispatched the
			// step and seen the replacement merge the PR. findLiveAgentForTask
			// let the replacement claim the task by skipping the stalled
			// incumbent on this same heartbeat; this is the reap-side completion
			// of that hand-off. Checked before the updated_at-gated orphan rule.
			if reason := r.stalledOrphanReason(ctx, b, taskID, now); reason != "" {
				return reason
			}
			if r.isTaskClosed(ctx, taskID) {
				grace := orphanReapAfter
				if b.Role == "gardener" {
					grace = gardenerOrphanReapAfter
				}
				if now.Sub(b.UpdatedAt) > grace {
					return fmt.Sprintf("orphan: source task %s already closed", taskID)
				}
				// Task closed but pod still busy or within grace — leave it be.
				return ""
			}
		}
		// "working" agents that haven't updated are stale.
		// Empty agent_state means the pod died before the status reporter
		// could set a state — these are orphaned beads that accumulate
		// and consume pod slots via the reconciler's desired-state set.
		if now.Sub(b.UpdatedAt) > r.timeout {
			return fmt.Sprintf("stale: no activity for %s", r.timeout)
		}
		return ""

	case "spawning":
		// Pods stuck Pending+Unschedulable (cluster out of capacity, no
		// matching node, etc.) get reaped well ahead of the normal 2x
		// timeout — there is nothing for the agent to do but consume a slot
		// in the reconciler's desired-state set. SyncAll surfaces this as
		// pod_scheduling_blocked=true while the pod is still Pending.
		if b.Metadata["pod_scheduling_blocked"] == "true" && now.Sub(b.UpdatedAt) > unschedulableReapAfter {
			return fmt.Sprintf("stale: unschedulable, cluster capacity (%s)", now.Sub(b.UpdatedAt).Round(time.Second))
		}
		// "spawning" agents may be slow to start (image pull, init containers)
		// but if they haven't progressed after 2x the timeout, the pod is
		// likely crash-looping. Without this, a stuck spawning bead stays
		// open forever — neither the reconciler nor the reaper cleans it up.
		if now.Sub(b.UpdatedAt) > 2*r.timeout {
			return fmt.Sprintf("stale: stuck in spawning state for %s", now.Sub(b.UpdatedAt).Round(time.Second))
		}
		return ""

	case "assigning":
		// Reclaim sweep for a pod stuck mid-assignment (G6, ka-0000006802). The
		// pool hand-off flipped this bead prewarmed→assigning and set task_id,
		// but the in-pod entrypoint never got as far as reporting "working" — a
		// post-assign crash/OOM/node loss stranded it here. The pool reconciler
		// only counts "prewarmed" agents, so a stranded "assigning" bead is
		// invisible to pool accounting and, without this arm, would sit at the
		// 2h default-branch timeout while its slot leaks.
		//
		// GATE ON GENUINE POD ABSENCE, mirroring the empty-state fast-fail: a
		// bead flipped to "assigning" whose pod is alive is merely slow to report
		// "working" (the entrypoint is still polling its bead / hydrating the
		// assignment env). Only reap when NO pod exists and the flip is older
		// than the short grace window; a live-but-quiet pod keeps the full
		// base-timeout path and is never early-reaped. A pod-list error leaves
		// podExists=true (livePods nil), so a K8s API blip can never trigger a
		// spurious reap here.
		if !podExists && now.Sub(b.UpdatedAt) > assigningReapAfter {
			return fmt.Sprintf("stale: no pod for %s in assigning state (post-assign wedge)", now.Sub(b.UpdatedAt).Round(time.Second))
		}
		return ""

	case "prewarmed":
		// "prewarmed" agents are intentionally idle waiting for assignment.
		return ""

	case "yielding":
		// "yielding" agents are blocked on a human decision (gb yield).
		// But if the pod dies while yielding, the bead stays open forever.
		// Use 2x the normal timeout as a safety net.
		if now.Sub(b.UpdatedAt) > 2*r.timeout {
			return fmt.Sprintf("stale: yielding with no activity for %s", now.Sub(b.UpdatedAt).Round(time.Second))
		}
		return ""

	case "queued":
		// "queued" agents are waiting for pod capacity. Normally the
		// reconciler handles expiry, but that tracking is in-memory and
		// resets on controller restart. Additionally, a queued agent
		// whose source task was already completed by another agent is
		// an orphan and should be cleaned up.
		staleness := now.Sub(b.UpdatedAt)
		if staleness > r.timeout {
			if taskID := r.sourceTaskID(b); taskID != "" {
				if r.isTaskClosed(ctx, taskID) {
					return fmt.Sprintf("orphan: source task %s already closed", taskID)
				}
			}
			// Even if the source task is still open, queued agents that
			// have been stuck for 2x the timeout will never get a slot.
			// Reap them to prevent indefinite accumulation.
			if staleness > 2*r.timeout {
				return fmt.Sprintf("stale: queued for %s without getting a pod slot", staleness.Round(time.Second))
			}
		}
		return ""

	case "done", "failed", "rate_limited":
		// Terminal states with open beads are orphans — the agent finished
		// but the bead was never closed. Use a short timeout (1/4 of the
		// normal timeout, min 15 minutes) since these should have been
		// closed immediately by gb stop or the restart loop.
		terminalTimeout := r.timeout / 4
		if terminalTimeout < 15*time.Minute {
			terminalTimeout = 15 * time.Minute
		}
		if now.Sub(b.UpdatedAt) > terminalTimeout {
			return fmt.Sprintf("orphan: terminal state %q but bead still open after %s",
				b.AgentState, now.Sub(b.UpdatedAt).Round(time.Second))
		}
		return ""

	case "stopping":
		// Frozen-awaiting-thaw (ka-0000002104): suspendAgent (cmd/gb/stop.go)
		// parks a freeze-eligible agent's bead OPEN at agent_state=stopping as
		// the exclusive thaw-target signal — the wake paths key on it to mint a
		// same-name resume bead so the name-keyed thaw (and its resume-latency
		// observation) fires. While the snapshot exists the podless bead is a
		// legitimate parked state with no upper age bound (a plan gardener can
		// rest overnight); reaping it would destroy the signal and force the
		// next wake to cold-start. Without this case, "stopping" fell into the
		// default unknown-state branch and was reaped as failed after the
		// normal timeout. Once the snapshot is gone (thaw consumed it, or it
		// was lost), the bead is a genuine leak — the reconciler's leaked-bead
		// closer usually gets it first; this is the backstop, on the same short
		// clock as the other terminal orphans. With freeze/thaw disabled (no
		// checker wired) there is no snapshot machinery at all, so every
		// "stopping" bead is a leak on that same clock.
		if r.hasSnapshot != nil && r.hasSnapshot(ctx, b.AgentName) {
			return ""
		}
		terminalTimeout := r.timeout / 4
		if terminalTimeout < 15*time.Minute {
			terminalTimeout = 15 * time.Minute
		}
		if now.Sub(b.UpdatedAt) > terminalTimeout {
			return fmt.Sprintf("orphan: frozen thaw target without snapshot, still open after %s",
				now.Sub(b.UpdatedAt).Round(time.Second))
		}
		return ""

	default:
		// Unknown states — reap after the normal timeout.
		if now.Sub(b.UpdatedAt) > r.timeout {
			return fmt.Sprintf("stale: unknown state %q, no activity for %s", b.AgentState, r.timeout)
		}
		return ""
	}
}

// runtimeReapReason returns a non-empty reap reason when an agent has run past
// its role's absolute runtime ceiling, or "" to leave it alone. It is the
// reap-side runtime-ceiling watchdog for bug ka-0000000716 (extended to every
// role by gd-X2eSin2J80) and is called only from the "working"/empty-state arm
// of reapReason (a live running state); spawning/queued/prewarmed/terminal/
// stopping states each have their own path and are never subject to the
// ceiling.
//
// The reap is DESTRUCTIVE (kills a live pod, irreversibly losing in-flight
// work), so it fires only when EVERY confirmation signal agrees the agent is a
// genuine long-dead wedge, not merely a long legitimate run:
//
//  1. Ceiling armed for this role — ceilingForRole(b.Role) > 0 (else disabled
//     for this agent).
//  2. Over the ceiling — created_at older than the ceiling. Measured from
//     created_at, NOT updated_at, so a churning wedge that keeps updated_at
//     fresh cannot dodge it (the whole point vs the base staleness rule).
//  3. Live pod present — podExists. A podless bead is a leak the leaked-bead
//     closer / other reapReason arms own; there is no pod to reap and no spend
//     to stop, so the ceiling stays out of it.
//  4. No MR mid-flight — agentHoldsOpenMR is false. Killing an agent holding an
//     open MR is the irreversible case the bug singles out; we decline and let
//     the MR's own terminal transition (merge/close by gardener-delivery or the
//     operator) end the run.
//
// Even when all four hold, the reap only ACTS when the operator has armed the
// dry-run gate (runtimeReapEnabled). In dry-run (the default) it LOGS the
// would-reap decision and returns "" so nothing is killed — the rollout confirms
// precision from the log before arming. The MR check (a daemon graph walk) is
// deliberately the LAST gate so it is skipped entirely for the overwhelming
// majority of agents that are nowhere near the ceiling.
//
// Before the armed reap actually fires, this files a best-effort escalation
// bead (fileRuntimeCeilingEscalation) naming the agent, its age, session
// count, and held task — silently killing a long-running agent is not
// obviously better than silently restarting it (gd-X2eSin2J80), so a human
// gets a look before the fleet kills it. Escalation filing failures are
// logged and swallowed: they never block or undo the reap decision.
func (r *StaleReaper) runtimeReapReason(ctx context.Context, b beadsapi.AgentBead, now time.Time, podExists bool) string {
	ceiling := r.ceilingForRole(b.Role)
	if ceiling <= 0 {
		return ""
	}
	// A missing/zero created_at cannot prove age — fail toward NOT reaping.
	if b.CreatedAt.IsZero() {
		return ""
	}
	runtime := now.Sub(b.CreatedAt)
	if runtime <= ceiling {
		return ""
	}
	// Only a LIVE pod is worth (and reapable by) a destructive kill; a podless
	// over-ceiling bead is handled by the leak/orphan arms, not here.
	if !podExists {
		return ""
	}
	// MR mid-flight guard — the irreversible case. Checked last so the graph
	// walk runs only for the rare agent that is genuinely over the ceiling.
	if r.agentHoldsOpenMR(ctx, b.ID) {
		r.logger.Info("stale reaper: agent over runtime ceiling but holds an OPEN MR — declining reap",
			"bead", b.ID, "agent", b.AgentName, "project", b.Project,
			"role", b.Role, "spawned_by", b.Metadata["spawned_by"],
			"runtime", runtime.Round(time.Second), "ceiling", ceiling)
		return ""
	}

	reason := fmt.Sprintf("stale: %s agent runtime %s exceeds max %s (wedged, burning spend)",
		b.Role, runtime.Round(time.Second), ceiling)

	if !r.runtimeReapEnabled() {
		// Dry-run: surface the would-reap decision but do NOT kill. An operator
		// flips STALEREAPER_RUNTIME_REAP=true to arm.
		r.logger.Warn("stale reaper: agent over runtime ceiling (DRY-RUN, not reaping) — arm STALEREAPER_RUNTIME_REAP to enable",
			"bead", b.ID, "agent", b.AgentName, "project", b.Project,
			"spawned_by", b.Metadata["spawned_by"], "role", b.Role,
			"created_at", b.CreatedAt.UTC().Format(time.RFC3339),
			"runtime", runtime.Round(time.Second), "ceiling", ceiling,
			"would_reason", reason)
		// Also stamp the would-reap observation onto the bead so it is queryable
		// from the beads plane, not only from a multi-deployment log grep
		// (ka-0000003021, option C).
		r.stampRuntimeReapDryRun(ctx, b, now, runtime, reason)
		return ""
	}

	r.fileRuntimeCeilingEscalation(ctx, b, runtime, ceiling)
	return reason
}

// runtimeReapDryRun* are the agent-bead fields the runtime-ceiling watchdog
// stamps the FIRST time an over-ceiling agent clears every reap guard while the
// arm gate is still OFF (dry-run). They make the "would have reaped" event
// queryable from the beads plane — e.g. `kd list --all-types -f
// runtime_reap_dryrun_at` — instead of requiring a 24h log grep across every
// gardenio-system deployment. The arming decision for
// STALEREAPER_RUNTIME_REAP can then be driven from durable, auditable
// evidence in the graph rather than ephemeral logs (ka-0000003021, option C).
const (
	runtimeReapDryRunAtField      = "runtime_reap_dryrun_at"
	runtimeReapDryRunRuntimeField = "runtime_reap_dryrun_runtime"
	runtimeReapDryRunReasonField  = "runtime_reap_dryrun_reason"
)

// stampRuntimeReapDryRun records, at most ONCE per bead, that the runtime-ceiling
// watchdog crossed all confirmation guards and would have reaped this agent were
// the arm gate flipped on. It is best-effort and idempotent: if the bead already
// carries runtime_reap_dryrun_at the stamp is skipped, so a wedge that stays
// over-ceiling across many reconcile passes produces exactly one daemon write.
// The first-observation timestamp — the signal the arming decision needs — is
// preserved, and the write amplification a per-pass re-stamp would cause is
// avoided. A stamp failure is logged and swallowed: the dry-run path must never
// change the reap outcome (it already returned ""), only its observability.
func (r *StaleReaper) stampRuntimeReapDryRun(ctx context.Context, b beadsapi.AgentBead, now time.Time, runtime time.Duration, reason string) {
	if b.Metadata[runtimeReapDryRunAtField] != "" {
		return // first would-reap observation already recorded on this bead
	}
	if err := r.daemon.UpdateBead(ctx, b.ID, beadsapi.UpdateBeadRequest{
		Fields: map[string]any{
			runtimeReapDryRunAtField:      now.UTC().Format(time.RFC3339),
			runtimeReapDryRunRuntimeField: runtime.Round(time.Second).String(),
			runtimeReapDryRunReasonField:  reason,
		},
	}); err != nil {
		r.logger.Warn("runtime-ceiling dry-run: failed to stamp would-reap observation on bead",
			"bead", b.ID, "agent", b.AgentName, "error", err)
	}
}

// fileRuntimeCeilingEscalation files an advisory escalation bead immediately
// before an armed runtime-ceiling reap kills a live agent pod (gd-X2eSin2J80).
// Matt's ask was "alert on that, do something about it" — silently killing a
// long-running agent is not obviously better than silently restarting it, so a
// human gets the chance to look at it first. This is intentionally
// non-blocking and best-effort: it does not gate, delay, or ever reverse the
// reap decision that follows it (the reap is the reaction to a wedge already
// confirmed by every guard above; the escalation is for post-hoc operator
// review and future threshold tuning, not a veto). A filing failure is logged
// and swallowed.
//
// The escalation carries no operation_type executor and is never auto-acted
// on — it is a "general" advisory record naming the agent, its age, session
// count, and the task it holds, mirroring the escalation-bead shape used by
// PromoteDecisionToEscalation (internal/bridge/api_decisions.go). An
// IdempotencyKey scoped to the agent bead ID means a repeat reap pass (e.g. a
// prior pass's pod delete or bead close failed and the agent is still found
// over-ceiling next cycle) reuses the same escalation instead of filing a
// duplicate.
func (r *StaleReaper) fileRuntimeCeilingEscalation(ctx context.Context, b beadsapi.AgentBead, runtime time.Duration, ceiling time.Duration) {
	taskID := r.sourceTaskID(b)
	fields := map[string]string{
		"operation_type":   "general",
		"context":          fmt.Sprintf("Agent %s (role %s) has run %s, past its %s runtime ceiling, and is about to be reaped as a wedged pod (spend-stopper, bug ka-0000000716 / gd-X2eSin2J80). Session count: %s. Review before the reap fires again elsewhere, and use this as calibration input for the per-role ceiling.", b.AgentName, b.Role, runtime.Round(time.Second), ceiling, b.Metadata["session_count"]),
		"requesting_agent": b.ID,
		"requesting_task":  taskID,
	}
	rawFields, err := json.Marshal(fields)
	if err != nil {
		r.logger.Warn("runtime-ceiling: failed to marshal escalation fields", "bead", b.ID, "error", err)
		return
	}
	escalationID, err := r.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:          fmt.Sprintf("Runtime-ceiling reap: %s (%s, %s over %s ceiling)", b.AgentName, b.Role, runtime.Round(time.Second), ceiling),
		Type:           "escalation",
		Description:    fmt.Sprintf("Agent %s is about to be reaped for exceeding its absolute runtime ceiling. See gd-X2eSin2J80.", b.AgentName),
		Priority:       1,
		Fields:         rawFields,
		IdempotencyKey: "runtime-ceiling-escalation:" + b.ID,
	})
	if err != nil {
		r.logger.Warn("runtime-ceiling: failed to file pre-reap escalation",
			"bead", b.ID, "agent", b.AgentName, "error", err)
		return
	}
	r.logger.Warn("stale reaper: filed pre-reap escalation for over-ceiling agent",
		"bead", b.ID, "agent", b.AgentName, "role", b.Role,
		"runtime", runtime.Round(time.Second), "ceiling", ceiling,
		"escalation", escalationID)
}

// sourceTaskID extracts the source task bead ID from an agent bead.
// It checks the task_id metadata field first (set by bridge-spawned agents),
// then falls back to parsing "Spawned from ka-XXX" in the description
// (set by the Gardenio iOS app).
func (r *StaleReaper) sourceTaskID(b beadsapi.AgentBead) string {
	if taskID := b.Metadata["task_id"]; taskID != "" {
		return taskID
	}
	// The iOS app sets the description to "Spawned from ka-XXXX: <title>".
	// The description is not on AgentBead directly, so we need to fetch
	// the full bead detail.
	detail, err := r.daemon.GetBead(context.Background(), b.ID)
	if err != nil {
		return ""
	}
	if m := spawnedFromRe.FindStringSubmatch(detail.Description); len(m) > 1 {
		return m[1]
	}
	return ""
}

// stalledOrphanReason returns a reap reason when the agent's progress heartbeat
// (last_progress_at, stamped by gb init only on a usage-bearing turn) is older
// than beadsapi.ProgressStaleThreshold AND its source task is closed or now
// assigned to a different agent. Either alone is not enough: a long quiet tool
// call stalls the heartbeat while the task is legitimately still its own, and a
// freshly closed task may still have its agent wrapping up (the updated_at
// orphan rule covers that). Fails open on a missing or unparseable heartbeat
// and on a task fetch error. The "stalled:" prefix maps to agent_state=failed
// in reapCloseState: the agent did not finish its work, someone else did.
func (r *StaleReaper) stalledOrphanReason(ctx context.Context, b beadsapi.AgentBead, taskID string, now time.Time) string {
	if !beadsapi.AgentProgressStalled(b.Metadata, now, beadsapi.ProgressStaleThreshold) {
		return ""
	}
	task, err := r.daemon.GetBead(ctx, taskID)
	if err != nil {
		r.logger.Debug("could not fetch source task for stalled-orphan check",
			"task", taskID, "error", err)
		return ""
	}
	stalledFor := "over " + beadsapi.ProgressStaleThreshold.String()
	if ts, perr := time.Parse(time.RFC3339, b.Metadata["last_progress_at"]); perr == nil {
		stalledFor = now.Sub(ts).Round(time.Minute).String()
	}
	agentName := b.Metadata["agent"]
	if agentName == "" {
		agentName = b.Title
	}
	switch {
	case task.Status == "closed":
		return fmt.Sprintf("stalled: no genuine turn for %s and source task %s already closed", stalledFor, taskID)
	case task.Assignee != "" && task.Assignee != agentName:
		return fmt.Sprintf("stalled: no genuine turn for %s and source task %s now held by %s", stalledFor, taskID, task.Assignee)
	}
	return ""
}

// isTaskClosed checks whether a task bead is in the "closed" status.
func (r *StaleReaper) isTaskClosed(ctx context.Context, taskID string) bool {
	detail, err := r.daemon.GetBead(ctx, taskID)
	if err != nil {
		r.logger.Debug("could not fetch source task for orphan check",
			"task", taskID, "error", err)
		return false
	}
	return detail.Status == "closed"
}

// assigneeStoppedCleanly reports whether the agent named assigneeName has a
// genuine, agent-recorded wrap-up on its most-recent agent bead — the signal
// that it despawned deliberately (e.g. `gb stop --force`) rather than
// crashing/OOMing. Used to avoid stamping a stranded run-instance task with
// false crash provenance (ka-0000006105). The agent bead is typically CLOSED by
// the time the reconciler runs, so this uses the any-status by-name lookup
// (FindMostRecentAgentBead). Best-effort: a lookup miss or error returns false
// (fall back to the crash/OOM default), so this never manufactures a clean-stop
// verdict it can't substantiate.
func (r *StaleReaper) assigneeStoppedCleanly(ctx context.Context, assigneeName string) bool {
	if assigneeName == "" {
		return false
	}
	detail, err := r.daemon.FindMostRecentAgentBead(ctx, assigneeName)
	if err != nil || detail == nil {
		return false
	}
	return beadsapi.HasRealWrapUp(detail.Fields[beadsapi.WrapUpFieldName])
}
