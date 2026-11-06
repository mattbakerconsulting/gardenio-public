package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// autoCloseSystemOperator is the X-System-Operator source label the
// auto-closer stamps on its hint, triage and witness writes when the thread
// bead carries no human-shaped operator. It MUST satisfy the daemon's
// sanitizeSystemOperatorHeader rules (ASCII lowercase/digits/single
// non-leading/trailing dash, len 1..64) so the daemon binds the
// "system:slack-bridge" operator row instead of refusing the write. Mirrors
// the bridge's client-level default (cmd/slack-bridge/main.go
// systemOperatorSource); pinned by TestAutoCloseSystemOperatorIsValidLabel.
const autoCloseSystemOperator = "slack-bridge"

// DefaultThreadTaskCloseWindow is the grace period between a slack-thread agent
// closing successfully and the parent thread task being idle-hinted if no
// further agent has been spawned for the same thread. The auto-closer never
// closes a wish (gd-Zhcpn9IZVl): the herder and the user are the only closers.
const DefaultThreadTaskCloseWindow = 30 * time.Minute

// DefaultThreadTaskSweepInterval is how often the durable backfill/reconcile
// sweep re-scans open slack-thread task beads and idle-hints the ones whose
// bound agents are all terminal (done/failed/closed) and idle past the window.
// The sweep is the safety net for the event-driven MaybeSchedule fast path:
// it heals orphans left by a bridge restart (in-memory timer lost), by the
// spawn-path linking gap (agent never carried slack_thread_bead_id), and by
// thread beads created before the event path shipped (ka-0000001874).
const DefaultThreadTaskSweepInterval = 15 * time.Minute

// DefaultSpawnStallGrace is how long a non-terminal member (a still-spawning /
// working agent bound to a wish) may sit without its updated_at advancing before
// the sweep stops treating it as a LIVE suppressor (1b-9, ka-0000007017). A
// wedged mid-spawn agent — one that registered but never progressed — would
// otherwise suppress its wish's hint forever, since a non-terminal state never
// becomes a terminal witness and never ages out on its own. Past the grace, the
// stalled member stops suppressing and instead stands as a witness that an
// agent WAS dispatched for the wish, so a stalled spawn nudges (hint).
const DefaultSpawnStallGrace = 30 * time.Minute

// threadBeadLabelPrefix is the canonical label prefix that identifies a Slack
// thread task bead. The sweep keys off this LABEL (always present) rather than
// the agent's slack_thread_bead_id field, so it is robust to the linking gap.
const threadBeadLabelPrefix = "slack-thread:"

// idleHintMarker is a stable, machine-recognizable token embedded in every
// idle-hint comment the auto-closer posts.
// It is the dedupe key: alreadyHintedThisEpisode scans the bead's comments for
// the most recent comment carrying this marker and suppresses a new hint when
// that prior hint is still "current" for the present idle episode. Keeping the
// marker out of the human-facing prose (it rides at the end after a separator)
// means the rest of the hint text can evolve without breaking dedupe.
const idleHintMarker = "[gardenio-idle-hint]"

// triageLabel is the canonical label the auto-closer stamps on a wish (thread
// task) whose bound agent ended in a FAILED terminal state. It marks the wish
// as needing operator triage rather than a "looks done?" hint (1b-7, ka-0000003060).
// The label is the durable, queryable signal that survives a bridge restart:
// the backfill sweep keys off it to SKIP the thread (never a "looks done"
// idle-hint) long after the failed agent bead itself has closed
// and dropped out of ListAgentBeads' active-status window.
const triageLabel = "needs-triage"

// triageReasonAgentFailed is the value stamped on the thread bead's
// triage_reason field when an agent bound to the wish ended failed. It mirrors
// the close_reason vocabulary so triage provenance is machine-readable.
const triageReasonAgentFailed = "slack-thread-agent-failed"

// triageMarker is the machine-recognizable token embedded in the triage
// comment, analogous to idleHintMarker. It lets alreadyTriaged recognise a
// triage comment we already posted without re-parsing prose.
const triageMarker = "[gardenio-needs-triage]"

// threadTerminalWitnessField is the DURABLE terminal-witness marker stamped on
// the THREAD (wish) bead itself the moment a bound agent is observed reaching a
// clean terminal (done) close (ka-0000006383). It carries the RFC3339 timestamp
// of that observation.
//
// Why it exists: the sweep's live terminal-witness (buildMemberEvidence) is
// derived from ListAgentBeads, which is ACTIVE-STATUS ONLY (beadsapi
// activeStatuses = open,in_progress,blocked,deferred). So the in-listing
// witness is visible only in the brief window between an agent reaching
// agent_state=done and the AGENT BEAD ITSELF closing (dropping out of the
// active-status listing). If no sweep runs inside that window (bridge restart,
// sweep interval lands after the agent bead closed, lost event), the witness
// ages out forever and the wish is stranded open — the exact failure this bug
// fixes. Stamping the fact ON THE THREAD BEAD at terminal time makes the proof
// DURABLE: the sweep can then authorize an idle hint on "durable terminal
// marker + idle past window + no live member" without racing the agent-bead
// reap. This is the positive-evidence-preserving twin of the failed→needs-triage
// durable-label split already in this file.
const threadTerminalWitnessField = "thread_agent_terminal_at"

// threadTerminalWitnessAgentField records WHICH agent's clean terminal close
// produced the durable witness. Provenance only; the sweep keys off the
// timestamp field's presence.
const threadTerminalWitnessAgentField = "thread_agent_terminal_last"

// ThreadTaskAutoCloserDaemon is the subset of BeadClient the auto-closer needs.
type ThreadTaskAutoCloserDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	ListAgentBeads(ctx context.Context) ([]beadsapi.AgentBead, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	AddComment(ctx context.Context, beadID, author, text string) error
	GetComments(ctx context.Context, beadID string) ([]beadsapi.Comment, error)
	// AddLabel / UpdateBeadFields back the failed→needs-triage split (1b-7,
	// ka-0000003060): a failed agent marks the wish for triage (label + field)
	// and leaves it OPEN instead of letting it be idle-hinted.
	AddLabel(ctx context.Context, beadID, label string) error
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// ThreadTaskAutoCloserConfig configures the auto-closer.
type ThreadTaskAutoCloserConfig struct {
	Daemon ThreadTaskAutoCloserDaemon
	Logger *slog.Logger
	// Window is the grace period before checking whether to idle-hint the
	// parent thread task. Defaults to DefaultThreadTaskCloseWindow when zero.
	Window time.Duration
	// SweepInterval is how often the durable backfill sweep re-scans open
	// slack-thread task beads. Defaults to DefaultThreadTaskSweepInterval when
	// zero. The sweep is disabled if Daemon does not implement
	// ListBeadsFiltered (it always does for the real beadsapi client).
	SweepInterval time.Duration

	// SpawnStallGrace is how long a non-terminal member may sit without its
	// updated_at advancing before the sweep stops treating it as a live
	// suppressor (1b-9, ka-0000007017). Defaults to DefaultSpawnStallGrace when
	// zero. Wired from SLACK_THREAD_TASK_SPAWN_STALL_GRACE.
	SpawnStallGrace time.Duration
}

// ThreadTaskAutoCloser propagates a slack-thread agent's terminal close to the
// parent thread task bead (the wish): after a grace window, if no other agent
// has been spawned (or remains active) for the same
// slack-thread:{channel}:{ts} label, it posts one actionable idle hint and
// leaves the wish OPEN; a failed agent marks the wish needs-triage instead.
//
// It never closes a wish (gd-Zhcpn9IZVl, plan gd-r4GCQRjEei): the herder
// closes fulfilled wishes (herder-fulfilled / herder-owner-close) and a person
// closes theirs (slack-thread-user-close). The name predates that change.
type ThreadTaskAutoCloser struct {
	daemon        ThreadTaskAutoCloserDaemon
	logger        *slog.Logger
	window        time.Duration
	sweepInterval time.Duration

	// spawnStallGrace bounds how long a non-terminal member keeps suppressing
	// the sweep before its staleness (updated_at older than the grace) demotes
	// it from a live suppressor to a hint-only witness (1b-9, ka-0000007017).
	spawnStallGrace time.Duration

	// afterFunc allows tests to substitute a synchronous scheduler.
	// Defaults to time.AfterFunc.
	afterFunc func(d time.Duration, f func()) *time.Timer

	// nowFunc returns the current time; overridable in tests so the sweep's
	// idle-since-update comparison is deterministic. Defaults to time.Now.
	nowFunc func() time.Time

	mu     sync.Mutex
	timers map[string]*time.Timer // threadBeadID → pending timer
}

// NewThreadTaskAutoCloser constructs a new auto-closer.
func NewThreadTaskAutoCloser(cfg ThreadTaskAutoCloserConfig) *ThreadTaskAutoCloser {
	w := cfg.Window
	if w <= 0 {
		w = DefaultThreadTaskCloseWindow
	}
	sweep := cfg.SweepInterval
	if sweep <= 0 {
		sweep = DefaultThreadTaskSweepInterval
	}
	stall := cfg.SpawnStallGrace
	if stall <= 0 {
		stall = DefaultSpawnStallGrace
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ThreadTaskAutoCloser{
		daemon:          cfg.Daemon,
		logger:          logger,
		window:          w,
		sweepInterval:   sweep,
		spawnStallGrace: stall,
		afterFunc:       time.AfterFunc,
		nowFunc:         time.Now,
		timers:          make(map[string]*time.Timer),
	}
}

// Window returns the configured grace window.
func (c *ThreadTaskAutoCloser) Window() time.Duration {
	if c == nil {
		return 0
	}
	return c.window
}

// now returns the current time, honoring an injected nowFunc for tests and
// falling back to time.Now when unset (some test constructors build the struct
// literal directly and leave nowFunc nil).
func (c *ThreadTaskAutoCloser) now() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

// stampDurableTerminalWitness records, ON THE THREAD (wish) BEAD itself, that a
// bound agent was observed reaching a clean terminal (done) close — the durable
// terminal-witness marker (ka-0000006383). It is stamped SYNCHRONOUSLY at
// observation time (MaybeSchedule), while the agent's binding is fresh and
// BEFORE the agent bead is reaped out of the active-status ListAgentBeads
// window, so the sweep can later authorize an idle hint on the durable marker
// alone — no longer racing the agent-bead reap.
//
// Best-effort: a failed stamp is logged and swallowed. The in-listing witness
// (buildMemberEvidence) and the event-path runCheck still function without it;
// the durable marker is the SAFETY NET that heals the restart / lost-timer /
// aged-out-witness cases, so a transient write failure must never break the
// caller's normal scheduling. The write is attributed to the machine sink via
// X-System-Operator (this is a bridge-internal bookkeeping stamp, not a
// human-authored change), mirroring autoCloseOperatorContext's human-less
// fallback.
func (c *ThreadTaskAutoCloser) stampDurableTerminalWitness(ctx context.Context, threadBeadID, agentID string) {
	writeCtx := beadsapi.WithSystemOperator(ctx, autoCloseSystemOperator)
	fields := map[string]string{
		threadTerminalWitnessField:      c.now().UTC().Format(time.RFC3339),
		threadTerminalWitnessAgentField: agentID,
	}
	if err := c.daemon.UpdateBeadFields(writeCtx, threadBeadID, fields); err != nil {
		c.logger.Warn("thread-task-autoclose: failed to stamp durable terminal witness (continuing)",
			"thread_bead", threadBeadID, "agent_bead", agentID, "error", err)
		return
	}
	c.logger.Info("thread-task-autoclose: stamped durable terminal witness on thread bead",
		"thread_bead", threadBeadID, "agent_bead", agentID)
}

// MaybeSchedule inspects a closed agent bead and, when it was spawned for a
// Slack thread and reached a terminal state (agent_state ∈ {done, failed}),
// schedules a deferred check on the parent thread task. Calls for agents that
// aren't thread-spawned or are still live are silently dropped.
//
// A done close drives the idle hint. A FAILED close drives the
// failed→needs-triage split (1b-7, ka-0000003060): the deferred check marks the
// wish needs-triage and leaves it OPEN, so an operator triages the failure.
//
// Scheduling is per-thread-bead: a fresh schedule for the same thread replaces
// any pending timer so the window restarts from the latest agent close. This
// means a failure followed by a clean re-run (or vice-versa) is decided by the
// LATEST terminal state, which is the correct outcome.
func (c *ThreadTaskAutoCloser) MaybeSchedule(agent BeadEvent) {
	if c == nil || c.daemon == nil {
		return
	}
	if agent.Type != "agent" {
		return
	}
	// Only terminal closes propagate. A done close drives the idle hint; a
	// failed close drives the needs-triage split. Any other state (a live agent
	// whose bead momentarily closed, etc.) is dropped.
	agentState := agent.Fields["agent_state"]
	if agentState != "done" && agentState != "failed" {
		return
	}
	failed := agentState == "failed"
	threadBeadID := agent.Fields["slack_thread_bead_id"]
	if threadBeadID == "" {
		return
	}
	channel := agent.Fields["slack_thread_channel"]
	threadTS := agent.Fields["slack_thread_ts"]
	if !isValidThreadBinding(channel, threadTS) {
		return
	}

	agentID := agent.ID
	c.logger.Info("thread-task-autoclose: scheduling check",
		"agent_bead", agentID,
		"thread_bead", threadBeadID,
		"channel", channel,
		"thread_ts", threadTS,
		"failed", failed,
		"window", c.window)

	// Durable terminal-witness stamp (ka-0000006383): on a CLEAN (done) terminal
	// close, record the proof-of-work ON THE THREAD BEAD synchronously, right now
	// — while the agent's binding is fresh and BEFORE its agent bead is reaped out
	// of the active-status ListAgentBeads window. This is what lets the backfill
	// sweep authorize an idle hint long after the agent bead has closed, healing
	// the aged-out-witness / bridge-restart / lost-timer strandings.
	//
	// A FAILED close is deliberately NOT stamped: it drives the needs-triage split
	// (runCheck → markNeedsTriage) and must never look like a clean
	// ran-and-finished witness that authorizes a "looks done?" hint.
	if !failed {
		c.stampDurableTerminalWitness(context.Background(), threadBeadID, agentID)
	}

	c.mu.Lock()
	if old, ok := c.timers[threadBeadID]; ok {
		old.Stop()
	}
	t := c.afterFunc(c.window, func() {
		c.runCheck(context.Background(), agentID, threadBeadID, channel, threadTS, failed)
	})
	c.timers[threadBeadID] = t
	c.mu.Unlock()
}

// runCheck performs the deferred check: if the thread bead is still open and
// no other agent is active or has been spawned for the thread since the
// completed agent, post an idle hint (deduped per idle episode) and leave the
// wish open. It never closes the wish (gd-Zhcpn9IZVl).
//
// When failed is true the bound agent ended in a failed terminal state: rather
// than idle-hinting the wish, runCheck marks it needs-triage and leaves it
// OPEN so an operator triages the failure (1b-7, ka-0000003060).
func (c *ThreadTaskAutoCloser) runCheck(ctx context.Context, agentID, threadBeadID, channel, threadTS string, failed bool) {
	// Clear the pending-timer entry first so a fresh agent close after this
	// fires schedules a new check.
	c.mu.Lock()
	delete(c.timers, threadBeadID)
	c.mu.Unlock()

	bead, err := c.daemon.GetBead(ctx, threadBeadID)
	if err != nil {
		c.logger.Warn("thread-task-autoclose: failed to fetch thread bead, skipping",
			"thread_bead", threadBeadID, "error", err)
		return
	}
	if bead == nil {
		return
	}
	if bead.Status == "closed" {
		c.logger.Debug("thread-task-autoclose: thread bead already closed, skipping",
			"thread_bead", threadBeadID)
		return
	}

	// runCheck runs only because MaybeSchedule observed a real terminal agent
	// close bound to this thread, so it is positive-evidence by construction
	// (1b-8, ka-0000000126). The listing only detects a *surviving* live agent
	// that should suppress the hint.
	agents, err := c.daemon.ListAgentBeads(ctx)
	if err != nil {
		c.logger.Warn("thread-task-autoclose: failed to list agent beads, skipping",
			"thread_bead", threadBeadID, "error", err)
		return
	}
	for _, a := range agents {
		if a.ID == agentID {
			continue
		}
		if a.Metadata["slack_thread_channel"] != channel {
			continue
		}
		if a.Metadata["slack_thread_ts"] != threadTS {
			continue
		}
		if a.AgentState != "done" && a.AgentState != "failed" {
			c.logger.Info("thread-task-autoclose: live agent for thread, skipping hint",
				"thread_bead", threadBeadID, "live_agent", a.ID, "state", a.AgentState)
			return
		}
	}

	// The daemon's AddComment path fail-closes when no operator resolves, so
	// every write carries per-call attribution: the thread bead's own human
	// operator, else the system sink (ka-0000006293).
	writeCtx := autoCloseOperatorContext(ctx, bead)

	// Failed→needs-triage split (1b-7, ka-0000003060): a crash is the opposite
	// of "looks done", so a failed agent marks the wish needs-triage instead of
	// hinting.
	if failed {
		c.markNeedsTriage(writeCtx, bead, agentID)
		return
	}

	// Post an ACTIONABLE hint and leave the bead open (ka-0000007276). The hint
	// is deduped per idle episode (1b-6, ka-0000001768) so repeated checks /
	// sweeps don't spam the bead or re-hint a dormant thread forever.
	if c.alreadyHintedThisEpisode(ctx, bead) {
		c.logger.Debug("thread-task-autoclose: idle hint already posted this episode, skipping",
			"thread_bead", threadBeadID, "last_agent", agentID)
		return
	}
	hint := actionableIdleHint(c.window, agentID)
	if err := c.daemon.AddComment(writeCtx, threadBeadID, "gardenio", hint); err != nil {
		c.logger.Warn("thread-task-autoclose: failed to add idle hint comment",
			"thread_bead", threadBeadID, "error", err)
		return
	}
	c.logger.Info("thread-task-autoclose: posted idle hint, leaving thread task open",
		"thread_bead", threadBeadID,
		"last_agent", agentID,
		"channel", channel,
		"thread_ts", threadTS)
}

// Run starts the durable backfill/reconcile sweep and blocks until ctx is
// cancelled. It fires sweepOnce immediately (catching the historical-orphan
// backlog on boot) and then on every sweepInterval tick. Nil receiver / nil
// daemon makes Run a no-op so callers can wire it unconditionally.
//
// The sweep is the safety net for the event-driven MaybeSchedule fast path:
// MaybeSchedule's in-memory timer is lost on a bridge restart and only ever
// keys off the agent's slack_thread_bead_id field (the linking gap). The sweep
// re-derives idleness from the slack-thread:{channel}:{ts} LABEL, which is
// always present, so it reaches orphans the event path can never reach.
func (c *ThreadTaskAutoCloser) Run(ctx context.Context) {
	if c == nil || c.daemon == nil {
		return
	}
	c.logger.Info("thread-task-autoclose: starting backfill sweep",
		"interval", c.sweepInterval, "window", c.window)

	// Immediate first pass so open wishes are reconciled on boot rather than
	// after the first interval (ka-0000001874).
	c.sweepOnce(ctx)

	ticker := time.NewTicker(c.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweepOnce(ctx)
		}
	}
}

// sweepOnce lists every open slack-thread task bead and idle-hints each one
// whose bound agents are all terminal (or spawn-stalled) and idle past the
// window, leaving it open. It never closes a wish (gd-Zhcpn9IZVl). Matching
// keys off the thread LABEL (slack-thread:{channel}:{ts}), so it is independent
// of whether any agent ever stamped slack_thread_bead_id (ka-0000001874).
func (c *ThreadTaskAutoCloser) sweepOnce(ctx context.Context) {
	if c == nil || c.daemon == nil {
		return
	}

	open, err := c.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"task"},
		Statuses: []string{"open"},
		Labels:   []string{"source:slack"},
		Limit:    1000,
	})
	if err != nil {
		c.logger.Warn("thread-task-autoclose sweep: failed to list open thread beads, skipping",
			"error", err)
		return
	}
	if open == nil || len(open.Beads) == 0 {
		return
	}

	// One agent-bead listing serves the whole sweep: build the per-thread-label
	// liveness + terminal-witness indexes once instead of re-listing per bead.
	agents, err := c.daemon.ListAgentBeads(ctx)
	if err != nil {
		c.logger.Warn("thread-task-autoclose sweep: failed to list agent beads, skipping",
			"error", err)
		return
	}
	// Open thread beads exist yet the agent listing is EMPTY: that is
	// indeterminate liveness (boot, degraded daemon, SSE lag), not proof every
	// agent is gone, so skip the pass (1b-8, ka-0000000126).
	if len(agents) == 0 {
		c.logger.Info("thread-task-autoclose sweep: empty agent listing with open thread beads, skipping pass (indeterminate liveness)",
			"open_threads", len(open.Beads))
		return
	}

	now := c.nowFunc()

	// Per-wish member evidence, keyed on both the slack-thread LABEL and the
	// wish head's id via a derivative's fields.thread_id (1b-9, ka-0000007017).
	evidence := c.buildMemberEvidence(agents, now)

	hinted := 0
	for _, bead := range open.Beads {
		if bead == nil || bead.Status == "closed" {
			continue
		}
		label := threadLabelFromBead(bead)
		if label == "" {
			continue // not a slack-thread task bead (no slack-thread:… label)
		}
		// A wish marked needs-triage (1b-7, ka-0000003060) is owned by an
		// operator: never "looks done?"-hint a crash. Leave it untouched.
		if beadHasLabel(bead, triageLabel) {
			c.logger.Debug("thread-task-autoclose sweep: wish marked needs-triage, leaving it open",
				"thread_bead", bead.ID, "label", label)
			continue
		}
		ev := evidence.forWish(label, bead.ID)

		// The durable terminal witness (ka-0000006383) survives the agent-bead
		// reap that drops the in-listing witness out of ListAgentBeads.
		if hasDurableTerminalWitness(bead) {
			ev.terminalWitness = true
		}

		// A FRESH non-terminal member (direct or deep derivative) is still
		// working this wish; spawn-stall grace has already demoted wedged ones.
		if ev.live {
			continue
		}
		// Idle-grace: updated_at advances on the last agent close / comment, so
		// this keeps the event path's "no further activity" semantics.
		if !bead.UpdatedAt.IsZero() && now.Sub(bead.UpdatedAt) < c.window {
			continue
		}
		// Positive-evidence gate (1b-8, ka-0000000126; 1b-9, ka-0000007017):
		// hint only on proof a member was dispatched for THIS wish — a clean
		// terminal witness or a spawn-stalled member. Absent both, the wish may
		// never have been worked, so leave it alone.
		if !ev.terminalWitness && !ev.stalledWitness {
			c.logger.Debug("thread-task-autoclose sweep: no member witness for wish, leaving it open (positive-evidence-only)",
				"thread_bead", bead.ID, "label", label)
			continue
		}

		// Post an ACTIONABLE hint and leave the bead open (ka-0000007276),
		// deduped per idle episode (1b-6, ka-0000001768) so a thread that stays
		// open across many sweeps is hinted once, not every interval.
		if c.alreadyHintedThisEpisode(ctx, bead) {
			c.logger.Debug("thread-task-autoclose sweep: idle hint already posted this episode, skipping",
				"thread_bead", bead.ID, "label", label)
			continue
		}
		writeCtx := autoCloseOperatorContext(ctx, bead)
		hint := actionableIdleHint(c.window, "")
		if err := c.daemon.AddComment(writeCtx, bead.ID, "gardenio", hint); err != nil {
			c.logger.Warn("thread-task-autoclose sweep: failed to add idle hint comment",
				"thread_bead", bead.ID, "error", err)
			continue
		}
		hinted++
		c.logger.Info("thread-task-autoclose sweep: posted idle hint, leaving thread task open",
			"thread_bead", bead.ID, "label", label)
	}
	if hinted > 0 {
		c.logger.Info("thread-task-autoclose sweep: completed",
			"scanned", len(open.Beads), "hinted", hinted)
	}
}

// memberEvidence is the per-pass rollup the sweep consults to decide, for each
// wish, whether to suppress or hint. It is keyed independently on every
// binding surface a wish member can expose (1b-9, ka-0000007017):
//   - the slack-thread LABEL (agents bound by slack_thread_channel/ts metadata);
//   - the wish head's ka-id, reached via a derivative's fields.thread_id (the
//     deep-subtree surface — a gardener-chain / restart-spawned agent that is
//     auto-parented under the wish but carries NO channel/ts).
//
// forWish unions the facts across both surfaces so a wish is resolved by its
// label AND its own id in one lookup.
type memberEvidence struct {
	// live: at least one FRESH (updated within spawnStallGrace) non-terminal
	// member is working the wish → suppress the hint.
	live map[string]bool
	// terminalWitness: at least one member ran and reached a terminal
	// agent_state and is still listed → clean positive evidence the wish was
	// worked-and-finished; authorizes the hint (1b-8).
	terminalWitness map[string]bool
	// stalledWitness: at least one non-terminal member has stalled past
	// spawnStallGrace (a wedged mid-spawn) → proof an agent was dispatched;
	// also authorizes the hint.
	stalledWitness map[string]bool
}

// wishEvidence is the unioned verdict for a single wish, resolved across both
// its binding surfaces (label + head id).
type wishEvidence struct {
	live            bool
	terminalWitness bool
	stalledWitness  bool
}

// forWish unions the recorded facts for a wish addressed by BOTH its
// slack-thread label and its head ka-id, so a member bound by either surface
// counts. Empty keys are ignored.
func (e memberEvidence) forWish(label, headID string) wishEvidence {
	any := func(m map[string]bool) bool {
		return (label != "" && m[label]) || (headID != "" && m[headID])
	}
	return wishEvidence{
		live:            any(e.live),
		terminalWitness: any(e.terminalWitness),
		stalledWitness:  any(e.stalledWitness),
	}
}

// buildMemberEvidence rolls up the whole active agent listing into the
// per-wish membership index the sweep consults (1b-9, ka-0000007017). It folds
// the three 1b-9 refinements into one pass:
//
//   - Deep-subtree liveness: a member is bound to its wish by its slack-thread
//     LABEL (channel/ts) AND/OR by fields.thread_id (a derivative auto-parented
//     under the wish head). Both binding surfaces are recorded, so a running
//     derivative that carries only thread_id still suppresses the wish's hint.
//
//   - Spawn-stall grace: a non-terminal member counts as LIVE (a suppressor)
//     only while fresh — its updated_at within spawnStallGrace of now. Past the
//     grace it is demoted to a stalled WITNESS, so a wedged mid-spawn agent
//     stops suppressing the hint forever.
//
//   - Self-exclusion: a member never contributes evidence about ITS OWN wish-
//     head bead. The head is a task, not an agent, so it never appears in the
//     agent listing — the generalization of runCheck's `if a.ID == agentID`
//     head self-exclusion is therefore structural here: derivatives point at the
//     head via thread_id but the head never points at (or counts) itself.
func (c *ThreadTaskAutoCloser) buildMemberEvidence(agents []beadsapi.AgentBead, now time.Time) memberEvidence {
	ev := memberEvidence{
		live:            make(map[string]bool),
		terminalWitness: make(map[string]bool),
		stalledWitness:  make(map[string]bool),
	}
	for _, a := range agents {
		// Collect every binding surface this member exposes: the slack-thread
		// label (channel/ts) and the wish-pointer (fields.thread_id). A member
		// may carry one, the other, or both (a directly-bound thread agent
		// carries both; a deep derivative carries only thread_id).
		var keys []string
		if ch, ts := a.Metadata["slack_thread_channel"], a.Metadata["slack_thread_ts"]; ch != "" && ts != "" {
			keys = append(keys, threadBeadLabel(ch, ts))
		}
		if tid := a.Metadata["thread_id"]; tid != "" {
			keys = append(keys, tid)
		}
		if len(keys) == 0 {
			continue // not bound to any wish
		}

		// A member in a terminal agent_state is the clean ran-and-finished
		// witness (1b-8). ListAgentBeads is active-status only, but an
		// active-status bead can still hold a terminal agent_state awaiting reap.
		if isAgentTerminal(a.AgentState) {
			for _, k := range keys {
				ev.terminalWitness[k] = true
			}
			continue
		}

		// Non-terminal member: LIVE while fresh, a stalled witness once
		// its updated_at ages past spawnStallGrace (1b-9). A zero updated_at is
		// treated as fresh (indeterminate age → do not demote), so a listing that
		// omits the timestamp never spuriously abandons a live agent.
		stalled := !a.UpdatedAt.IsZero() && now.Sub(a.UpdatedAt) >= c.spawnStallGrace
		for _, k := range keys {
			if stalled {
				ev.stalledWitness[k] = true
			} else {
				ev.live[k] = true
			}
		}
	}
	return ev
}

// actionableIdleHint builds the idle-nudge comment the auto-closer posts. The
// body carries the close affordance IN-PLACE —
// "react ⏹️ to close" — so the nudge is actionable in Slack without an operator
// hunting for a separate command (ties to 1b-11's user-close handler, whose
// gesture is :black_square_for_stop: / ⏹️ — ka-0000002200). The trailing
// idleHintMarker is the machine-recognizable dedupe token; it rides after a
// separator so the human-facing prose can change without breaking
// alreadyHintedThisEpisode's recognition.
func actionableIdleHint(window time.Duration, lastAgent string) string {
	var b strings.Builder
	if lastAgent != "" {
		fmt.Fprintf(&b,
			"gardenio: this slack-thread task has been idle for %s with no further activity (last completed agent: %s).",
			window, lastAgent)
	} else {
		fmt.Fprintf(&b,
			"gardenio: no live agent for this slack-thread task and it has been idle for at least %s.",
			window)
	}
	b.WriteString(" Looks done? React :black_square_for_stop: (⏹️) on the wish card (or close this bead) to wrap it up — it stays open until then.")
	// Dedupe marker — keep last so prose can evolve independently.
	b.WriteString("\n\n")
	b.WriteString(idleHintMarker)
	return b.String()
}

// alreadyHintedThisEpisode reports whether the auto-closer has already posted
// an idle hint for the CURRENT idle episode of beadID, so the caller can
// suppress a duplicate. This is the dedupe that keeps the hint path from
// re-commenting on every sweep interval (the spam + re-hint-forever
// failure mode 1b-6 exists to fix).
//
// An idle episode is bounded by the bead's last REAL activity: a fresh agent
// close, a human comment, or a field/status write. We compute that boundary as
// max(bead.UpdatedAt, newest non-hint comment) and compare it against the
// newest hint comment we ourselves posted (identified by idleHintMarker). If a
// hint already sits at-or-after the last real activity, we've hinted this
// episode and must stay silent; if real activity is newer than the last hint,
// the thread was touched again and a single fresh hint is warranted.
//
// Note this is intentionally activity-relative, NOT a fixed cooldown: a pure
// "skip if hinted within window" rule would re-hint every window forever on a
// truly dormant thread — exactly the spam we are removing.
func (c *ThreadTaskAutoCloser) alreadyHintedThisEpisode(ctx context.Context, bead *beadsapi.BeadDetail) bool {
	if bead == nil {
		return false
	}
	comments, err := c.daemon.GetComments(ctx, bead.ID)
	if err != nil {
		// Fail OPEN to attribution-safety but CLOSED to spam: if we cannot
		// read history we cannot prove we already hinted, so allow the post.
		// A missed dedupe is a single extra comment; a wrongly-suppressed
		// hint silently strands the thread. Prefer the louder failure.
		c.logger.Warn("thread-task-autoclose: failed to fetch comments for hint dedupe, posting hint",
			"thread_bead", bead.ID, "error", err)
		return false
	}

	var latestHint, lastActivity time.Time
	if !bead.UpdatedAt.IsZero() {
		lastActivity = bead.UpdatedAt
	}
	for _, cm := range comments {
		if strings.Contains(cm.Text, idleHintMarker) {
			if cm.CreatedAt.After(latestHint) {
				latestHint = cm.CreatedAt
			}
			continue
		}
		if cm.CreatedAt.After(lastActivity) {
			lastActivity = cm.CreatedAt
		}
	}
	if latestHint.IsZero() {
		return false // never hinted → post
	}
	// Already hinted this episode iff the hint is not older than the last real
	// activity (i.e. no fresh activity has opened a new episode since).
	return !latestHint.Before(lastActivity)
}

// markNeedsTriage marks a wish (thread task) bead as needing operator triage
// after a bound agent ended in a failed terminal state, and LEAVES IT OPEN
// (1b-7, ka-0000003060). It is the failed-half of the failed→needs-triage
// split: the done-half is the idle hint.
//
// The mark is threefold so it is robust across every downstream surface:
//   - the needs-triage LABEL — durable + queryable, and the signal the backfill
//     sweep keys off to SKIP the wish forever (never idle-hint it),
//     surviving long after the failed agent bead drops out of ListAgentBeads;
//   - the triage_reason FIELD — machine-readable provenance, mirroring the
//     close_reason vocabulary;
//   - a single triage COMMENT (carrying triageMarker) — the human-facing signal
//     in Slack/viewer, deduped per failure episode like the idle hint.
//
// The whole operation is idempotent: a wish already carrying the triage label
// is left untouched (no duplicate comment, no re-stamp), so a re-fired timer or
// a second failed agent on the same already-triaged wish is a no-op.
func (c *ThreadTaskAutoCloser) markNeedsTriage(writeCtx context.Context, bead *beadsapi.BeadDetail, agentID string) {
	if bead == nil {
		return
	}
	if c.alreadyTriaged(writeCtx, bead) {
		c.logger.Debug("thread-task-autoclose: wish already marked needs-triage, skipping",
			"thread_bead", bead.ID, "failed_agent", agentID)
		return
	}

	// Comment first (the human-facing signal); then stamp the durable
	// label + field. Ordering favours the loud signal landing even if a later
	// write transiently fails — the sweep's skip keys off the label, so we
	// attempt the label last and log loudly if it fails.
	comment := triageComment(agentID)
	if err := c.daemon.AddComment(writeCtx, bead.ID, "gardenio", comment); err != nil {
		c.logger.Warn("thread-task-autoclose: failed to add needs-triage comment (continuing to mark)",
			"thread_bead", bead.ID, "error", err)
	}
	if err := c.daemon.UpdateBeadFields(writeCtx, bead.ID, map[string]string{
		"triage_reason": triageReasonAgentFailed,
		"triaged_agent": agentID,
	}); err != nil {
		c.logger.Warn("thread-task-autoclose: failed to stamp triage_reason field",
			"thread_bead", bead.ID, "error", err)
	}
	if err := c.daemon.AddLabel(writeCtx, bead.ID, triageLabel); err != nil {
		c.logger.Warn("thread-task-autoclose: failed to add needs-triage label",
			"thread_bead", bead.ID, "error", err)
		return
	}
	c.logger.Info("thread-task-autoclose: marked wish needs-triage, leaving it open (failed agent)",
		"thread_bead", bead.ID, "failed_agent", agentID)
}

// alreadyTriaged reports whether the wish is already marked needs-triage, so
// markNeedsTriage stays idempotent. The authoritative signal is the durable
// needs-triage label on the bead; we also treat a prior triage comment
// (triageMarker) as proof, covering the window where the comment landed but a
// transient label-write failed on the previous attempt.
func (c *ThreadTaskAutoCloser) alreadyTriaged(ctx context.Context, bead *beadsapi.BeadDetail) bool {
	if bead == nil {
		return false
	}
	for _, l := range bead.Labels {
		if l == triageLabel {
			return true
		}
	}
	comments, err := c.daemon.GetComments(ctx, bead.ID)
	if err != nil {
		// Fail toward re-marking: a duplicate triage comment is cheap; a
		// silently-stranded failed wish is the failure mode we are removing.
		c.logger.Warn("thread-task-autoclose: failed to fetch comments for triage dedupe, marking",
			"thread_bead", bead.ID, "error", err)
		return false
	}
	for _, cm := range comments {
		if strings.Contains(cm.Text, triageMarker) {
			return true
		}
	}
	return false
}

// triageComment builds the needs-triage comment posted when a bound agent ends
// failed. The trailing triageMarker is the machine dedupe token (it rides after
// a separator so the prose can evolve), mirroring actionableIdleHint's shape.
func triageComment(agentID string) string {
	var b strings.Builder
	if agentID != "" {
		fmt.Fprintf(&b,
			"gardenio: the agent working this wish ended in a FAILED state (last agent: %s). "+
				"Marking it needs-triage and leaving it open — it was NOT auto-closed. "+
				"An operator should triage the failure (re-run, reassign, or close).",
			agentID)
	} else {
		b.WriteString(
			"gardenio: an agent working this wish ended in a FAILED state. " +
				"Marking it needs-triage and leaving it open — it was NOT auto-closed. " +
				"An operator should triage the failure (re-run, reassign, or close).")
	}
	b.WriteString("\n\n")
	b.WriteString(triageMarker)
	return b.String()
}

// threadLabelFromBead returns the canonical slack-thread:{channel}:{ts} label
// carried by a thread task bead, or "" if the bead carries no such label.
func threadLabelFromBead(bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, threadBeadLabelPrefix) {
			return l
		}
	}
	return ""
}

// hasDurableTerminalWitness reports whether the thread bead carries the durable
// terminal-witness marker (thread_agent_terminal_at) stamped by MaybeSchedule on
// a clean agent close (ka-0000006383). Its mere presence (any non-empty value)
// is the positive proof that a bound agent ran-and-finished for this wish — it
// survives the agent-bead reap that ages the in-listing witness out of
// ListAgentBeads.
func hasDurableTerminalWitness(bead *beadsapi.BeadDetail) bool {
	if bead == nil {
		return false
	}
	return bead.Fields[threadTerminalWitnessField] != ""
}

// beadHasLabel reports whether the bead carries the exact label.
func beadHasLabel(bead *beadsapi.BeadDetail, label string) bool {
	if bead == nil {
		return false
	}
	for _, l := range bead.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// autoCloseOperatorContext returns a context carrying explicit operator
// attribution for the auto-closer's writes. With the client-level
// SetSystemOperator default removed (epic ka-0000004835, task ka-0000006293),
// this per-call attribution is the only thing keeping the comment write from
// fail-closing.
//
// When the thread bead exposes a human-shaped operator (slack:U… /
// oidc:…@…) it is stamped via WithOperator (X-Operated-By) so the hint
// comment is attributed to the human who owned the thread. Otherwise — only
// for a genuinely human-less thread bead — a per-call X-System-Operator is
// stamped so the daemon binds the machine sink for this one write; the
// comment's NOT NULL operated_by_id is satisfied either way and the write
// never fail-closes. This is a scoped per-call machine attribution, NOT a
// client-wide default that would silently absorb writes that should carry a
// human.
func autoCloseOperatorContext(ctx context.Context, bead *beadsapi.BeadDetail) context.Context {
	if bead != nil && isHumanShapedOperator(bead.OperatedBy) {
		return beadsapi.WithOperator(ctx, bead.OperatedBy)
	}
	return beadsapi.WithSystemOperator(ctx, autoCloseSystemOperator)
}

// isHumanShapedOperator reports whether op is a human operator id the daemon's
// sanitizeOperatedByHeader accepts as an X-Operated-By value — i.e. a
// "slack:<id>" or "oidc:<local>@<domain>" prefix. The daemon re-validates the
// exact shape; this is the cheap client-side gate that decides whether to send
// X-Operated-By (human) or fall back to X-System-Operator (sentinel). System
// and fleet sentinels ("system:…", "fleet:…") are deliberately rejected so
// they are never re-sent as a human operator.
func isHumanShapedOperator(op string) bool {
	return strings.HasPrefix(op, "slack:") || strings.HasPrefix(op, "oidc:")
}
