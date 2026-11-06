// Package bridge — GitLab-mention dispatch catchup watchdog (ka-0000000279, A1).
//
// MentionCatchupWatchdog periodically respawns the crew for a
// spawn_source=gitlab-mention task bead whose webhook-time SpawnAgent was LOST
// — the "webhook created the task bead, then SpawnAgent failed and nothing
// retried" hole that dropped all three of Patrick's !1160 review comments on
// 2026-07-22 (plan ka-0000004007, Track A).
//
// THE FAILURE THIS CLOSES.
//
// MentionHandler.spawnMentionAgent (gitlab_mentions.go) is best-effort and
// fire-once: it CreateBead's the task first, then SpawnAgent's the type=agent
// bead the SSE subscriber + reconciler act on. When the CreateBead succeeds but
// the SpawnAgent fails (or the bridge dies in the window between them), the task
// bead is orphaned forever — open, unassigned, with no pod and no linked agent
// bead. `needs-intake` (applied ~15 min later by the orphan sweep) only SURFACES
// the bead; nothing consumes it to spawn a pod. This watchdog is that missing
// consumer: it is the reconciler-side catchup the incident asked for.
//
// WHY A PERIODIC SWEEP (not just a webhook-time retry). A bounded retry inside
// spawnMentionAgent (A2's territory) still loses the request if the bridge
// process dies mid-retry — the exact node-lifecycle/OOM death class the incident
// was. A daemon-side sweep that re-derives the orphan set from bead STATE on
// every tick is the durable backstop: it survives a bridge restart, and it is
// idempotent by construction (a respawned task gains a live agent bead and drops
// out of the next sweep's orphan set).
//
// THE ORPHAN PREDICATE (the load-bearing correctness rule). A task bead is an
// orphan to respawn iff ALL hold:
//
//  1. spawn_source == "gitlab-mention" — scoped to exactly this dispatch vector.
//     A generic "open task with no agent" reaper would fight every other
//     dispatcher (gardeners, formulas, manual work).
//  2. status is open/in_progress (active) — a closed task is done; never respawn.
//  3. created_at is older than the grace window — a JUST-created task is either
//     mid-dispatch (SpawnAgent in flight) or covered by the webhook path's own
//     nudge/respawn; respawning inside the window races the live spawn and
//     double-dispatches. The window is deliberately longer than the webhook
//     path's dedup window (mentionDedupWindow) so the two never overlap.
//  4. NO live linked agent bead — the decisive signal. "No assignee" ALONE is
//     NOT sufficient: the stale-reaper's reopenStrandedIssueTasks clears the
//     assignee of a crashed crew's task WITHOUT respawning the properly-
//     contextualized mention agent, so a legitimately-dispatched-then-crashed
//     task can sit open+unassigned with a DEAD agent bead. Conversely a task
//     whose crew is alive (agent bead active, non-terminal agent_state) must
//     NEVER be respawned. Liveness is judged from the linked agent bead exactly
//     as findLiveAgentForTask does (agent_state ∉ {done,failed}, active status).
//  5. NO open escalation/decision parking the task (ka-0000000411). When a crew
//     finishes the review/verify work but cannot CLOSE the task — the fix lives
//     on an unmerged MR awaiting one human approval — it files a
//     close-on-execute merge escalation (or a decision) with requesting_task
//     pointing back here, then stops. That handle is a LIVE re-entry point:
//     resolving it auto-respawns the filing crew, which re-picks the task. A
//     watchdog respawn alongside it just re-verifies the identical facts every
//     tick and posts a duplicate MR comment (the 7x-respawn storm on !2261 that
//     motivated this guard). Same signal the stop-gate claimed-work exemption
//     uses to park a claim (gb stop.go findParkHandle).
//
// SINGLE-DISPATCH SAFETY. The respawn calls SpawnAgent with the single_claim_task
// flag, so the daemon's own findLiveAgentForTask guard (keyed on task_id)
// atomically refuses a second crew if one raced in between the sweep's liveness
// check and the spawn — e.g. a re-mention landing on the same task at the same
// moment. That task_id guard, not the agent name, is what prevents a double
// dispatch (the minted name carries a random suffix and is not a dedup key).
//
// This lives in gitlab-bridge alongside the escalation reaper and the MR
// backstop pollers — the same periodic-sweep neighborhood, sharing the narrow
// daemon client surface and the isE2E fixture guard.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// defaultMentionCatchupInterval is how often the watchdog re-derives the
	// orphan set. Coarse on purpose: the orphans it rescues sat open forever, so
	// a 5-minute cadence rescues them promptly without hammering the daemon.
	defaultMentionCatchupInterval = 5 * time.Minute

	// defaultMentionCatchupGrace is how old a mention task must be before the
	// watchdog treats it as orphaned. It must exceed the webhook path's own
	// dedup/nudge window (mentionDedupWindow = 3m) with margin so the sweep never
	// races an in-flight webhook dispatch: inside this window the task is either
	// mid-spawn or the re-mention path owns it. 10 minutes matches the operator
	// tolerance the incident implied ("Patrick waited ~30 min").
	defaultMentionCatchupGrace = 10 * time.Minute

	// mentionCatchupActor is recorded as the author of the audit comment on a
	// respawned task, so a rescue is attributable in the bead's history.
	mentionCatchupActor = "gitlab-mention-catchup"

	// mentionCatchupListLimit bounds the sweep's task listing. The orphan class
	// is tiny (a handful of lost dispatches at a time), but a hard cap keeps a
	// pathological backlog from pulling an unbounded page into a memory-capped
	// bridge (advice ka-0000003555). Newest-first (daemon default) means an
	// over-cap backlog still drains across successive sweeps.
	mentionCatchupListLimit = 500
)

// MentionCatchupDaemon is the narrow bead-daemon surface the watchdog needs.
// Satisfied by *beadsapi.Client. Kept small so tests supply a tiny fake, in the
// same spirit as ReaperBeadClient.
type MentionCatchupDaemon interface {
	// ListBeadsFiltered lists the candidate mention task beads (server-side
	// filtered on type=task + spawn_source=gitlab-mention + active statuses).
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	// SpawnAgent respawns the crew for an orphaned task. Mirrors the webhook
	// path's spawn (role=crew, MR-binding extra fields, single_claim guard).
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
	// AddComment records the rescue audit trail on the task bead.
	AddComment(ctx context.Context, beadID, author, text string) error
}

// Compile-time assertion that the production beads client satisfies the surface.
var _ MentionCatchupDaemon = (*beadsapi.Client)(nil)

// MentionCatchupGitLab is the narrow GitLab surface the watchdog needs to
// re-verify a linked MR's live state before respawning. Satisfied by
// *GitLabClient. Kept small (one method) so tests supply a tiny fake, mirroring
// mrReopenGitLab in gitlab_mr_reopen_drift.go.
//
// State is read from GitLab, never from the task bead: the incident's own
// premise is that mention task beads do NOT carry mr_state/mr_merged (verified
// absent), and even where an mr_state exists it is stale/unset by design
// (advice ka-0000000519). A live fetch is the only staleness-proof signal.
type MentionCatchupGitLab interface {
	// GetMergeRequestByPath fetches a single MR by encoded project path + IID.
	GetMergeRequestByPath(ctx context.Context, projectPath string, mrIID int) (*GitLabMR, error)
}

// Compile-time assertion that the production GitLab client satisfies the surface.
var _ MentionCatchupGitLab = (*GitLabClient)(nil)

// MentionCatchupWatchdog periodically respawns crew for orphaned
// gitlab-mention task beads (ka-0000000279).
type MentionCatchupWatchdog struct {
	daemon   MentionCatchupDaemon
	gitlab   MentionCatchupGitLab
	logger   *slog.Logger
	interval time.Duration
	grace    time.Duration
	// project is the fallback project for a respawned agent when the task bead
	// carries no project:<name> label — mirrors MentionHandler.project.
	project string
	// dryRun logs the intended respawn without spawning or commenting, so an
	// operator can watch what the sweep WOULD rescue before enabling it.
	dryRun bool

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// MentionCatchupConfig configures a MentionCatchupWatchdog.
type MentionCatchupConfig struct {
	Daemon MentionCatchupDaemon
	// GitLab re-verifies a task's linked MR state before respawning, so a
	// months-old merged/closed MR is never re-dispatched (ka-0000007740). May
	// be nil (e.g. no GitLab token configured): the watchdog then falls back to
	// the pre-guard behaviour — respawn without the MR-state check — because
	// dropping a real human ask on a missing client is the worse failure.
	GitLab   MentionCatchupGitLab
	Logger   *slog.Logger
	Interval time.Duration // ≤ 0 → defaultMentionCatchupInterval
	Grace    time.Duration // ≤ 0 → defaultMentionCatchupGrace
	Project  string        // fallback project for respawned agents
	DryRun   bool
}

// NewMentionCatchupWatchdog constructs the watchdog loop.
func NewMentionCatchupWatchdog(cfg MentionCatchupConfig) *MentionCatchupWatchdog {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultMentionCatchupInterval
	}
	grace := cfg.Grace
	if grace <= 0 {
		grace = defaultMentionCatchupGrace
	}
	return &MentionCatchupWatchdog{
		daemon:   cfg.Daemon,
		gitlab:   cfg.GitLab,
		logger:   logger,
		interval: interval,
		grace:    grace,
		project:  cfg.Project,
		dryRun:   cfg.DryRun,
		now:      time.Now,
	}
}

// Run sweeps immediately, then on every tick until ctx is canceled.
func (w *MentionCatchupWatchdog) Run(ctx context.Context) error {
	w.logger.Info("gitlab-mention catchup watchdog starting",
		"interval", w.interval, "grace", w.grace, "dry_run", w.dryRun)
	w.sweepOnce(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("gitlab-mention catchup watchdog stopping")
			return ctx.Err()
		case <-ticker.C:
			w.sweepOnce(ctx)
		}
	}
}

// sweepOnce lists the active gitlab-mention task beads and respawns the crew for
// each one that is orphaned past the grace window with no live linked agent.
//
// The server-side filter (type=task, spawn_source=gitlab-mention, active
// statuses) narrows the scan to exactly the dispatch vector this watchdog owns,
// so the per-bead liveness check runs over a tiny set. Idempotent: a respawned
// task gains a live agent bead and is skipped by the isOrphaned check on the
// next sweep.
func (w *MentionCatchupWatchdog) sweepOnce(ctx context.Context) {
	res, err := w.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"task"},
		Statuses: []string{"open", "in_progress"},
		Fields:   map[string]string{"spawn_source": "gitlab-mention"},
		Limit:    mentionCatchupListLimit,
	})
	if err != nil {
		w.logger.Warn("gitlab-mention catchup sweep: list failed", "error", err)
		return
	}
	if res == nil {
		return
	}
	if res.Total > len(res.Beads) {
		// Surface truncation: the orphan class is tiny, so a full page is a
		// signal worth an operator's attention. The sweep still drains a
		// backlog across successive ticks (respawned tasks drop out).
		w.logger.Warn("gitlab-mention catchup sweep page truncated; surplus drains next tick",
			"returned", len(res.Beads), "total", res.Total, "limit", mentionCatchupListLimit)
	}

	checked, respawned := 0, 0
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		checked++
		if w.evaluateTask(ctx, b) {
			respawned++
		}
	}
	w.logger.Debug("gitlab-mention catchup sweep complete",
		"candidates", len(res.Beads), "checked", checked, "respawned", respawned)
}

// evaluateTask decides whether one mention task bead is an orphan and, if so,
// respawns its crew. Returns true when it respawned (or would have, in dry-run).
func (w *MentionCatchupWatchdog) evaluateTask(ctx context.Context, b *beadsapi.BeadDetail) bool {
	// E2E fixtures must never drive a real SpawnAgent.
	if isE2EFixture(b) {
		return false
	}

	// Belt-and-suspenders: the server-side filter already scoped to
	// spawn_source=gitlab-mention, but re-check the field so a fixture or a
	// future caller that relaxes the query can't slip a foreign bead through.
	if b.Fields["spawn_source"] != "gitlab-mention" {
		return false
	}

	// Grace guard: a just-created task is mid-dispatch or owned by the webhook
	// path's own nudge/respawn. A zero created_at (unknown age) fails safe as
	// "too young" so a missing timestamp never triggers a spurious respawn.
	if b.CreatedAt.IsZero() || w.now().Sub(b.CreatedAt) < w.grace {
		return false
	}

	// The decisive signal: is a live crew already on this task? A live agent
	// bead — even with the task assignee cleared by the stale-reaper — means the
	// work is covered; skip. Only a task with NO live linked agent is an orphan.
	if w.hasLiveAgent(ctx, b.ID) {
		return false
	}

	// Nudge-delivery guard (Track A3, plan ka-0000004007): when the webhook path
	// finds a live agent already on the MR and NUDGES it (rather than spawning a
	// new agent), the mention task it created has no linked agent bead of its
	// own — the work is covered by the nudged agent. That is the same "no live
	// linked agent" shape hasLiveAgent reads as an orphan, so without this guard
	// the watchdog would respawn a redundant crew and post a duplicate MR
	// comment. The webhook stamps nudged_agent on the task; skip the respawn
	// while THAT agent is still live. If it later dies without addressing the
	// mention, hasLiveNudgedAgent goes false and the task correctly falls
	// through to a fresh dispatch — so the guard never permanently drops a
	// request, it only defers to the covering agent while it is alive.
	if w.hasLiveNudgedAgent(ctx, b) {
		return false
	}

	// Parked-handle guard (ka-0000000411): a task whose real work is finished but
	// un-closable — the fix lives on an unmerged MR awaiting operator approval, so
	// the crew filed a close-on-execute merge escalation (or a decision) with
	// requesting_task pointing back here and stopped — already has a LIVE re-entry
	// handle. Resolving that handle auto-respawns the filing agent, which re-picks
	// the task. Respawning crew in the meantime just re-verifies the identical
	// facts every tick and posts a duplicate MR comment (7x on !2261). Skip while
	// such a handle is open; only genuinely orphaned tasks (no live agent AND no
	// live handle) are respawned. This is the same signal the stop-gate's
	// claimed-work exemption uses to park a claim (gb stop.go findParkHandle).
	if w.hasOpenParkHandle(ctx, b.ID) {
		return false
	}

	// Terminal-MR guard (ka-0000007740): never respawn crew onto an MR that is
	// already merged or closed. These orphans accumulated for months — some
	// since May — so a large fraction sit on settled MRs; re-dispatching a crew
	// there is wasted spend and posts confusing comments on finished work. An
	// OPEN MR (the common, load-bearing case: a real human review comment whose
	// webhook-time dispatch was lost) still respawns, so the guard subtracts
	// only the settled tail, never a live ask.
	if w.mrIsSettled(ctx, b) {
		return false
	}

	return w.respawn(ctx, b)
}

// mrIsSettled reports whether the task's linked MR is CONFIRMED merged or
// closed on GitLab — the only case in which the watchdog must NOT respawn.
//
// It is deliberately asymmetric and fail-OPEN: it returns true (skip the
// respawn) ONLY on a live GitLab fetch that comes back merged/closed. A nil
// GitLab client, an unparseable/missing mr_url, or ANY fetch error returns
// false so the respawn proceeds. The rationale is the cost asymmetry: a false
// skip permanently drops a real human ask (the exact bug the catchup watchdog
// exists to fix), whereas a false respawn is one bounded, idempotent-once wasted
// dispatch. We only pay the certain cost (skip) against certain evidence
// (a live merged/closed state), mirroring the fresh-fetch re-verify discipline
// of gitlab_mr_reopen_drift.go.
func (w *MentionCatchupWatchdog) mrIsSettled(ctx context.Context, b *beadsapi.BeadDetail) bool {
	if w.gitlab == nil {
		return false
	}
	mrURL := b.Fields["mr_url"]
	ref := ParseMRURL(mrURL)
	if ref == nil {
		// No parseable MR URL — nothing to verify. Fall through to respawn
		// rather than guess; a mention task always carries an mr_url in
		// practice, so this is a defensive no-op, logged for visibility.
		w.logger.Debug("gitlab-mention catchup: task has no parseable mr_url; skipping terminal-MR guard",
			"task", b.ID, "mr_url", mrURL)
		return false
	}

	mr, err := w.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
	if err != nil || mr == nil {
		// Fetch failed or MR not found: fail OPEN. A transient GitLab hiccup
		// must never drop a real ask — the next sweep re-checks. Under-guarding
		// for one tick (a possible wasted respawn) is the safe direction.
		w.logger.Warn("gitlab-mention catchup: MR state fetch failed — not skipping (fail-open)",
			"task", b.ID, "mr_url", mrURL, "error", err)
		return false
	}

	// GitLab MR states: opened, closed, merged, locked. Only merged/closed are
	// terminal for our purposes — a `locked` MR is still an active discussion an
	// unaddressed mention should reach, and `opened` is the whole point.
	switch mr.State {
	case "merged", "closed":
		w.logger.Info("gitlab-mention catchup: skipping respawn — linked MR is settled",
			"task", b.ID, "mr_url", mrURL, "mr_iid", ref.IID, "mr_state", mr.State)
		return true
	default:
		return false
	}
}

// hasLiveAgent reports whether an active, non-terminal agent bead is already
// linked to the task (fields.task_id == taskID). It mirrors the daemon's own
// findLiveAgentForTask liveness rule (agent_state ∉ terminal, active status) so
// the watchdog's skip decision matches the SpawnAgent single-claim guard that
// would refuse a duplicate spawn — the two agree on what "live" means.
//
// Fail-SAFE on a list error: returns true ("assume covered") so a transient
// daemon hiccup never drives a duplicate spawn. Under-respawning for one tick is
// harmless — the next sweep re-checks — while a false respawn races a live crew.
func (w *MentionCatchupWatchdog) hasLiveAgent(ctx context.Context, taskID string) bool {
	res, err := w.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Fields:   map[string]string{"task_id": taskID},
	})
	if err != nil {
		w.logger.Warn("gitlab-mention catchup: agent liveness check failed — assuming covered",
			"task", taskID, "error", err)
		return true
	}
	if res == nil {
		return false
	}
	for _, a := range res.Beads {
		if a == nil {
			continue
		}
		if isTerminalAgentState(a.Fields["agent_state"]) {
			continue
		}
		return true
	}
	return false
}

// hasLiveNudgedAgent reports whether the mention task was DELIVERED to a live
// agent via a nudge (Track A3, plan ka-0000004007) that is still alive. The
// webhook path stamps nudged_agent on the task when it finds an active agent on
// the MR and nudges it instead of spawning a new one; that task then has no
// linked agent bead of its own, so hasLiveAgent (keyed on task_id) reads false
// and the watchdog would otherwise treat it as an orphan.
//
// This looks up the stamped agent BY NAME and returns true only while that
// agent is non-terminal and active — the same liveness rule hasLiveAgent uses.
// Crucially it is NOT a permanent skip: if the nudged agent dies without
// addressing the mention, this goes false and the task correctly falls through
// to a fresh dispatch. A task with no nudged_agent stamp returns false
// immediately (the common orphan case is unaffected).
//
// Fail-SAFE on a list error: returns true ("assume covered") for one tick, matching
// hasLiveAgent — a transient daemon hiccup must never drive a duplicate spawn.
func (w *MentionCatchupWatchdog) hasLiveNudgedAgent(ctx context.Context, b *beadsapi.BeadDetail) bool {
	agentName := b.Fields["nudged_agent"]
	if agentName == "" {
		return false
	}
	res, err := w.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Fields:   map[string]string{"agent": agentName},
		Limit:    mentionCatchupListLimit,
	})
	if err != nil {
		w.logger.Warn("gitlab-mention catchup: nudged-agent liveness check failed — assuming covered",
			"task", b.ID, "nudged_agent", agentName, "error", err)
		return true
	}
	if res == nil {
		return false
	}
	for _, a := range res.Beads {
		if a == nil {
			continue
		}
		// Belt-and-suspenders: re-check the name so a fixture or a future caller
		// that relaxes the query can't count a foreign agent as coverage.
		if a.Fields["agent"] != agentName {
			continue
		}
		if isTerminalAgentState(a.Fields["agent_state"]) {
			continue
		}
		w.logger.Info("gitlab-mention catchup: skipping respawn — task covered by a live nudged agent",
			"task", b.ID, "nudged_agent", agentName)
		return true
	}
	return false
}

// parkHandleTypes are the bead types that can PARK a mention task: an
// operator-actioned escalation or an operator-answered decision. Both are filed
// with `gb {escalation,decision} create --requesting-task <task>`, which stamps
// the task id into the handle's requesting_task field (escalation.go /
// decision.go) and writes a blocks-edge from the task to the handle. Resolving
// either one auto-respawns the FILING agent, which re-picks the task — so while
// such a handle is open the task already has a live re-entry point and the
// watchdog must not re-dispatch a fresh crew alongside it. Mirrors the stop-gate
// claimed-work exemption's parkHandleTypes (gb stop.go).
var mentionCatchupParkHandleTypes = []string{"escalation", "decision"}

// mentionCatchupParkHandleStatuses are the handle statuses that still await an
// operator and so still park the task. Matches the stop-gate's parkHandleOpen
// allow-list (open/in_progress/blocked/deferred): a handle in any of these is
// pending, so it still parks; a closed handle has nothing left to respawn on and
// no longer parks (the task really is orphaned again and should be rescued).
var mentionCatchupParkHandleStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// hasOpenParkHandle reports whether the task is already parked behind an OPEN
// escalation/decision whose requesting_task points back at it (ka-0000000411).
//
// This is the decisive "a live re-entry handle already exists" signal: the
// concrete case is a mention task whose review/verify work is done but whose
// close is gated on one human MR approval (the gardeniobot bot cannot self-approve
// protected main), so the crew filed a gitlab_mr_merge escalation with
// --close-on-execute --requesting-task <task> and stopped. The escalation
// auto-respawns that crew on merge, so a fresh watchdog respawn every ~10 min
// only re-verifies the identical facts and posts a duplicate MR comment. Any
// open handle pointing here — filed by any agent — means the task is tracked, so
// this is deliberately NOT ownership-scoped (unlike the stop-gate, which asks
// "did *I* park this?"): the watchdog only asks "does a live handle exist?".
//
// Fail-SAFE on a list error: returns true ("assume parked") so a transient
// daemon hiccup never re-triggers the redundant-respawn storm this closes. The
// direction matches hasLiveAgent — both detect an existing coverage mechanism,
// and under-respawning for one tick is harmless (the next sweep re-checks).
func (w *MentionCatchupWatchdog) hasOpenParkHandle(ctx context.Context, taskID string) bool {
	res, err := w.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    mentionCatchupParkHandleTypes,
		Statuses: mentionCatchupParkHandleStatuses,
		Fields:   map[string]string{"requesting_task": taskID},
		Limit:    mentionCatchupListLimit,
	})
	if err != nil {
		w.logger.Warn("gitlab-mention catchup: park-handle check failed — assuming parked",
			"task", taskID, "error", err)
		return true
	}
	if res == nil {
		return false
	}
	for _, h := range res.Beads {
		if h == nil {
			continue
		}
		// Belt-and-suspenders: the server-side field filter already scoped to
		// requesting_task==taskID, but re-check so a fixture or a future caller
		// that relaxes the query can't count a foreign handle as coverage.
		if h.Fields["requesting_task"] != taskID {
			continue
		}
		w.logger.Info("gitlab-mention catchup: skipping respawn — task parked behind an open handle",
			"task", taskID, "handle", h.ID, "handle_type", h.Type, "handle_status", h.Status)
		return true
	}
	return false
}

// respawn re-dispatches the crew for an orphaned mention task, rebuilding an
// equivalent prompt + MR-binding fields from the task bead's stored fields (the
// webhook path stamped them at create-time). Returns true when it acted.
func (w *MentionCatchupWatchdog) respawn(ctx context.Context, b *beadsapi.BeadDetail) bool {
	fields := b.Fields
	if fields == nil {
		fields = map[string]string{}
	}

	mrURL := fields["mr_url"]
	mrIID, _ := strconv.Atoi(fields["gitlab_mr_iid"])
	age := w.now().Sub(b.CreatedAt).Round(time.Second)

	if w.dryRun {
		w.logger.Info("gitlab-mention catchup: WOULD respawn (dry-run)",
			"task", b.ID, "mr_url", mrURL, "mr_iid", mrIID, "age", age.String())
		return true
	}

	// Mint a fresh mention agent name (mr-<iid>-<rand>), the same shape the
	// webhook path uses for a brand-new dispatch. A pure orphan never spawned a
	// pod, so there is no prior PVC/JSONL to resume and no existing name to
	// reuse — this is a first dispatch, just late. Double-spawn safety does NOT
	// rest on the name (the random suffix means two callers would pick different
	// names anyway); it rests on the hasLiveAgent pre-check above plus the
	// single_claim_task guard passed to SpawnAgent, which keys on task_id.
	agentName := mentionAgentName(mrIID)

	project := projectFromLabels(b.Labels)
	if project == "" {
		project = w.project
	}

	prompt := buildMentionPromptFromFields(fields, b.ID)
	extraFields := mentionRespawnExtraFields(fields, b.ID, b.OperatedBy)

	agentBeadID, err := w.daemon.SpawnAgent(ctx, agentName, project, b.ID, "crew", prompt, extraFields)
	if err != nil {
		w.logger.Error("gitlab-mention catchup: respawn failed",
			"task", b.ID, "agent", agentName, "mr_url", mrURL, "error", err)
		return false
	}

	w.logger.Info("gitlab-mention catchup: respawned orphaned mention crew",
		"task", b.ID, "agent", agentName, "agent_bead", agentBeadID,
		"mr_url", mrURL, "mr_iid", mrIID, "project", project, "age", age.String())

	// Audit comment so the rescue is visible in the bead history (best-effort:
	// a comment failure does not undo the respawn). The bridge forwards bead
	// comments to Slack, so this also surfaces the rescue to the operator.
	comment := fmt.Sprintf(
		"🪝 Respawned by %s: this GitLab-mention task sat open %s with no live agent — "+
			"its webhook-time dispatch was lost (SpawnAgent failed, or the bridge died mid-dispatch). "+
			"Re-spawned crew `%s` (bead %s) to pick it up. (catchup watchdog ka-0000000279)",
		mentionCatchupActor, age.String(), agentName, agentBeadID)
	if err := w.daemon.AddComment(ctx, b.ID, mentionCatchupActor, comment); err != nil {
		w.logger.Warn("gitlab-mention catchup: respawned but audit comment failed",
			"task", b.ID, "error", err)
	}
	return true
}

// buildMentionPromptFromFields reconstructs an agent prompt equivalent to
// buildMentionPrompt from the fields the webhook path stamped on the task bead.
// The webhook's buildMentionPrompt takes a live noteContext, which the watchdog
// no longer has; every ingredient it needs was persisted at create-time
// (mr_url, gitlab_mr_iid, mention_author, mention_instruction, mention_file/line,
// gitlab_mr_source_branch), so the reconstruction is faithful.
func buildMentionPromptFromFields(fields map[string]string, taskID string) string {
	mrIID, _ := strconv.Atoi(fields["gitlab_mr_iid"])
	nc := noteContext{
		Author: fields["mention_author"],
		MR: &webhookMR{
			IID: mrIID,
			URL: fields["mr_url"],
		},
	}
	if path := fields["mention_file"]; path != "" {
		line, _ := strconv.Atoi(fields["mention_line"])
		nc.Position = &notePosition{NewPath: path, NewLine: line}
	}
	return buildMentionPrompt(nc, fields["mention_instruction"], taskID, fields["gitlab_mr_source_branch"])
}

// mentionRespawnExtraFields rebuilds the agent-bead binding fields the webhook
// path passes to SpawnAgent (gitlab_mentions.go spawnMentionAgent), plus the
// single_claim_task guard so the daemon atomically refuses a duplicate crew if
// one raced in. taskID is the mention task being respawned (mirrored into the
// agent bead's mention_task field, matching the webhook path). operatedBy is the
// task bead's operator, propagated so the respawned agent inherits the
// originating human via BEADS_OPERATED_BY.
func mentionRespawnExtraFields(fields map[string]string, taskID, operatedBy string) map[string]string {
	extra := map[string]string{
		"spawn_source": "gitlab-mention",
		// mention_task points the agent bead back at its task, exactly as the
		// webhook path's spawnMentionAgent extraFields does.
		"mention_task": taskID,
		// single_claim_task makes SpawnAgent's findLiveAgentForTask guard the
		// last-line defense against a double-spawn race (consumed, not persisted).
		"single_claim_task": "true",
	}

	// Carry the MR binding + identity fields forward so the respawned agent
	// card renders the same spawn-vector line and the agent can act on the MR.
	//
	// Only AGENT-bead field names belong here. The task bead's mr_url and
	// mention_author are task-only spellings — type:agent declares them as
	// gitlab_mr_url and gitlab_username — so carrying them verbatim 400'd the
	// whole agent create ("mr_url: unknown field") and the watchdog could
	// never respawn anything (ka-0000003933). mr_url is mirrored into
	// gitlab_mr_url just below; mention_author and gitlab_username hold the
	// same nc.Author value, so gitlab_username alone carries the identity.
	for _, k := range []string{
		"gitlab_mr_url", "gitlab_mr_iid", "gitlab_project_id",
		"gitlab_mr_source_branch", "gitlab_username",
		"slack_user_id", "user_bead_id",
	} {
		if v := fields[k]; v != "" {
			extra[k] = v
		}
	}
	// The webhook path stores mr_url under "gitlab_mr_url" on the AGENT bead
	// (spawnMentionAgent extraFields), but the TASK bead carries it as "mr_url".
	// Mirror it so the agent bead's MR binding is populated either way.
	if extra["gitlab_mr_url"] == "" {
		if v := fields["mr_url"]; v != "" {
			extra["gitlab_mr_url"] = v
		}
	}
	// Same translation for the commenter identity: the task bead spells it
	// mention_author, type:agent declares only gitlab_username. spawnMentionAgent
	// stamps both from the same nc.Author, so this is normally a no-op — it
	// covers a task bead that carries only the mention_author spelling.
	if extra["gitlab_username"] == "" {
		if v := fields["mention_author"]; v != "" {
			extra["gitlab_username"] = v
		}
	}
	if operatedBy != "" {
		extra["operated_by"] = operatedBy
	} else if op := fields["operated_by"]; op != "" {
		extra["operated_by"] = op
	}
	return extra
}

// isTerminalAgentState reports whether an agent_state value means the agent has
// finished (done/failed) and so is no longer holding its task claim. Mirrors the
// beadsapi terminalAgentStates set; duplicated here (unexported there) to keep
// the watchdog's liveness rule identical to the daemon's single-claim guard.
func isTerminalAgentState(state string) bool {
	switch state {
	case "done", "failed":
		return true
	default:
		return false
	}
}
