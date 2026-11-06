package main

// gb stop — request polite agent despawn.
//
// Sets stop_requested=true on the agent bead and POSTs to the local coop
// API to trigger a graceful session shutdown. The entrypoint restart loop
// checks the stop_requested flag after coop exits and terminates cleanly
// instead of restarting, then closes the bead so the reconciler stops
// tracking this pod.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/beadsapi"
)

var stopCmd = &cobra.Command{
	Use:     "stop",
	Aliases: []string{"done"},
	Short:   "Request polite agent despawn — prevents pod restart after exit",
	Long: `Mark this agent as stop-requested.

After calling 'gb stop', exit normally (finish your current turn). The entrypoint
will detect the stop request and close the agent bead instead of restarting,
causing the pod to terminate cleanly.

This is the correct way to despawn an agent voluntarily. Crashing or simply
exiting without calling 'gb stop' will trigger an automatic restart.

By default, the session JSONL is left intact so a re-spawned agent (with the same
name) resumes where it left off via coop --resume. Use --clean to mark all session
logs as stale instead, causing the next spawn to start a fresh conversation.

'gb stop' is the correct terminal action even when you have an open decision, so
long as you have no further work this turn and the answer only triggers a FUTURE
action (a merge awaiting operator approval, an indefinite/multi-day wait). A
decision filed with --requesting-task persists as the durable re-entry handle and
auto-respawns you the moment the operator resolves it — so stopping loses nothing.
Reserve 'gb yield' for a decision whose answer you need to finish the CURRENT turn;
re-yielding across a long wait just gets torn down by cooldown and re-spawned in a
loop (see bug ka-0000003244).

Usage:
  gb stop                          # request despawn, then exit normally
  gb stop --reason "finished work" # leave a note on the agent bead
  gb stop --wrapup '{"tldr":"Closed 3 bugs; blocked on API key","accomplishments":"Closed 3 bugs","blockers":"API key pending"}'
  gb stop --clean                  # also retire session JSONL (fresh start on re-spawn)
  gb stop --force                  # skip in-progress work check`,
	GroupID: "session",
	RunE:    runStop,
}

var (
	stopForce            bool
	stopReason           string
	stopClean            bool
	stopWrapUp           string
	stopDone             bool
	stopSuccessor        string
	stopGoalUnachievable bool
)

func init() {
	stopCmd.Flags().BoolVar(&stopForce, "force", false, "skip in-progress work check")
	stopCmd.Flags().StringVar(&stopReason, "reason", "", "reason for stopping (added as a comment on the agent bead)")
	stopCmd.Flags().BoolVar(&stopClean, "clean", false, "retire session JSONL so next spawn starts a fresh conversation")
	stopCmd.Flags().StringVar(&stopWrapUp, "wrapup", "", "structured wrap-up message as JSON (see WrapUp schema)")
	stopCmd.Flags().BoolVar(&stopDone, "done", false, "force terminal despawn (discard, no freeze snapshot) even for a freeze-eligible agent")
	stopCmd.Flags().StringVar(&stopSuccessor, "successor", "", "succession intent: none (default, terminal) | same (recreate this name after wake) | new (mint a fresh successor). Convenience for the wrap-up successor field; --wrapup JSON wins if it also sets successor")
	stopCmd.Flags().BoolVar(&stopGoalUnachievable, "goal-unachievable", false, "declare the goal DETERMINISTICALLY unachievable: overrides any successor to none and stamps a terminal marker so no further pod is burned retrying. Convenience for the wrap-up goal_unachievable field; --wrapup JSON wins if it also sets goal_unachievable")
}

func runStop(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	agentID, err := resolveAgentIDWithFallback(ctx, "")
	if err != nil {
		return fmt.Errorf("agent identity required: %w", err)
	}

	// --- Pre-flight checks (before any mutations) ---
	// All checks that can reject the stop must run here, before we set
	// stop_requested or post comments. Otherwise we end up in a zombie
	// state: stop_requested=true but bead still open.

	// Fold the --successor convenience flag into the raw --wrapup JSON so the
	// single processWrapUp path validates + reconciles it. --wrapup JSON stays
	// authoritative (a successor in the JSON wins over the flag). An unknown
	// --successor is rejected here, before any other gate.
	mergedWrapUp, err := applySuccessorFlag(stopWrapUp, stopSuccessor)
	if err != nil {
		return err
	}
	stopWrapUp = mergedWrapUp

	// Fold the --goal-unachievable convenience flag into the raw --wrapup JSON,
	// same contract as --successor: --wrapup JSON stays authoritative (a
	// goal_unachievable key in the JSON wins over the flag). P3, ka-0000005843.
	mergedWrapUp, err = applyGoalUnachievableFlag(stopWrapUp, stopGoalUnachievable)
	if err != nil {
		return err
	}
	stopWrapUp = mergedWrapUp

	// Parse wrap-up next — every downstream gate (decision-bead carve-out,
	// wrap-up validation, ask-gate) needs it, and a malformed
	// --wrapup is a caller input error worth surfacing before anything else.
	// The parse itself is side-effect-free (JSON decode + tldr normalization
	// warnings); it early-returns only on genuinely malformed input. Keeping it
	// ahead of the decision-bead gate lets that gate distinguish a finished
	// agent from a silent exit (ka-0000007734).
	var wrapup *WrapUp
	var wrapupCommentText string
	if stopWrapUp != "" {
		var err error
		wrapup, wrapupCommentText, err = processWrapUp(stopWrapUp)
		if err != nil {
			return err
		}
	}

	// --- Read-only despawn gates (batch-evaluated, single rejection) ---
	// Every gate here is read-only — one GET (claimed work), a git status read
	// (delivery), a decision-bead list, two GETs + a pure parse (progress), and
	// pure-local wrap-up validation — with NO ordering dependency between them.
	// So we evaluate ALL of them and collect each failure instead of returning
	// on the first. Pre-fix these were sequential early-returns, so an agent
	// discovered one knowable-at-once failure per invocation: the CED audit
	// (ka-0000005647) caught a gardener burning two extra round-trips learning
	// "claimed work pending" then "wrap-up missing tldr" — both known at
	// attempt #1. renderStopGateRejection reports them all at once. --force
	// skips every gate (semantics unchanged: each check is force-guarded).
	var gateFailures []error

	// The delivery check is read here, ahead of the batch, because the PR park
	// below must not put an unpushed head in front of the merge desk. Its
	// failure joins the batch further down.
	var deliveryErr error
	if !stopForce {
		deliveryErr = checkDelivery()
	}

	// PR park (gd-x7HUu7s3gD): an open PR with no merge escalation gets one
	// before the agent goes, so it cannot orphan. This is the one step ahead
	// of the gates with a side effect, and it has to be: the escalation's
	// blocks-edge is what lets the claimed-work gate below pass. A gate that
	// fails later leaves the escalation in place, which is still right: the
	// PR is open and the desk now sees it. See stop_prpark.go.
	var prParks []prParkResult
	if prParkEnabled(stopForce, deliveryErr, wrapup) {
		prParks = parkOpenPRs(ctx, daemon, agentID, actor, wrapup)
		for _, r := range prParks {
			if r.Filed || r.Held != "" || r.SentBack != "" {
				fmt.Println(prParkNote([]prParkResult{r}))
			} else if r.Err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not file a merge escalation for %s: %v\nFile it by hand (gb pr ready, or gb escalation create --operation-type gitlab_mr_merge --requesting-task <task> --close-on-execute) so the PR does not orphan.\n", r.URL, r.Err)
			}
		}
	}

	// A pending CI wait is checked against GitHub before any gate reads it, so
	// a woken agent cannot re-park on a run that has already concluded
	// (gd-3iOqZlkZLV): a concluded wait drops the park exemption below and
	// trips checkCIWaitConcluded instead. Like the PR park it writes ahead of
	// the gates, which is safe: a concluded run stays concluded.
	if !stopForce {
		settleCIWaitForStop(ctx, agentID)
	}

	// Gate: agent still holds claimed in-progress work (one GET, plus a
	// dependency read per claimed bead to spot a parked task). A scheduled
	// run's own run-instance task passes and is closed on the terminal path
	// below (ownRunTask).
	var ownRunTask string
	if !stopForce {
		var err error
		if ownRunTask, err = claimedWorkGate(ctx, agentID, heldPRTasks(prParks)); err != nil {
			gateFailures = append(gateFailures, err)
		}
	}

	// Gate: a CI wait on this agent's own bead already concluded (`gb wait ci`,
	// plan wake-on-ci-completion.md §2.1). CI finished before the agent stopped,
	// so stopping would park on a wake that already happened: read the result
	// first. One GET of the agent bead.
	if !stopForce {
		if err := checkCIWaitConcluded(loadCIWait(ctx, agentID)); err != nil {
			gateFailures = append(gateFailures, err)
		}
	}

	// Gate: unpushed commits on the current branch (git status read above).
	if deliveryErr != nil {
		gateFailures = append(gateFailures, deliveryErr)
	}

	// Gate: at least one decision bead — UNLESS this is a clean DONE stop. A
	// valid --wrapup (accomplishments present) is the "finished all work" path;
	// since the claimed-work gate above is collected (not fatal) the carve-out
	// still keys off the wrap-up, so a done-with-no-open-work stop needs no
	// decision (ka-0000007734, subsumes sub-defect A of ka-0000004255). Blocked
	// and bare (no-wrap-up) stops still hit the gate so work is offered for
	// review rather than silently completing.
	if decisionGateRequired(stopForce, wrapup) {
		if err := requireDecisionBead(ctx, actor); err != nil {
			gateFailures = append(gateFailures, err)
		}
	}

	// Gate: an operator-resolved decision this lane owns still owes its declared
	// artifact. `gb yield` hard-fails on a pending required_artifact
	// (yield.go printYieldResult), but the fleet is steered toward
	// stop-and-be-respawned rather than yield (advice ka-0000003244) — so the
	// path agents actually take had NO artifact check, and a specced deliverable
	// could be silently dropped with artifact_status stuck pending forever
	// (ka-0000006794). This closes that asymmetry. Read-only (one list query,
	// keyed on the DURABLE owner_seat so a respawn inherits the obligation rather
	// than shedding it at handoff — see checkRequiredArtifact).
	if !stopForce {
		if err := checkRequiredArtifact(ctx); err != nil {
			gateFailures = append(gateFailures, err)
		}
	}

	// Gate: schedule-driven progress fields (progress_score + progress_summary)
	// when the agent's schedule declares progress.surface.in_wrapup. The
	// detailed per-field guidance is printed here (from the returned error); the
	// numbered rejection below carries a concise headline.
	if err := enforceProgressWrapup(ctx, agentID, wrapup, stopForce); err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		gateFailures = append(gateFailures, fmt.Errorf("progress wrap-up incomplete — schedule requires progress_score + progress_summary"))
	}

	// Gate: wrap-up requirement enforcement. Runs on EVERY gb stop — including a
	// bare `gb stop` with no --wrapup — so crew and thread agents (which rarely
	// pass --wrapup) are still pushed to produce a structured wrap-up with the
	// tldr that renders the Slack agent card. wrapup is nil when --wrapup was
	// omitted; Validate treats nil as "all required fields empty". --force
	// bypasses (hard-enforce only); soft enforcement warns and does NOT fail.
	reqs := LoadWrapUpRequirements(ctx, daemon, agentID)
	if issues := reqs.Validate(wrapup); len(issues) > 0 {
		if reqs.Enforce == "hard" && !stopForce {
			if stopWrapUp == "" {
				fmt.Fprintf(os.Stderr, "Wrap-up required — re-run with --wrapup to summarize this session:\n")
			} else {
				fmt.Fprintf(os.Stderr, "Wrap-up validation failed:\n")
			}
			for _, issue := range issues {
				fmt.Fprintf(os.Stderr, "  - %s\n", issue)
			}
			fmt.Fprintf(os.Stderr, "\nExample:\n  gb stop --wrapup '{\"tldr\":\"<one-line outcome>\",\"accomplishments\":\"<what you changed/merged/closed>\"}'\n")
			gateFailures = append(gateFailures, fmt.Errorf("wrap-up incomplete: %s", strings.Join(issues, "; ")))
		} else {
			// Soft enforcement: warn but proceed.
			for _, issue := range issues {
				fmt.Fprintf(os.Stderr, "Warning: %s\n", issue)
			}
		}
	}

	// One consolidated rejection for every read-only gate that failed.
	if msg := renderStopGateRejection(gateFailures); msg != "" {
		fmt.Fprint(os.Stderr, msg)
		return fmt.Errorf("cannot stop: %d despawn gate(s) not satisfied — fix them or use --force", len(gateFailures))
	}

	// --- Stateful despawn gates (evaluated after the batch, still sequential) ---
	// These CANNOT join the single-rejection batch above because they have side
	// effects that must not fire until the read-only gates pass: the ask-gate
	// writes a one-shot marker + stamps the agent bead.

	// Stranded-operator-ask soft-gate (epic ka-0000002541, decision
	// ka-0000001640 → ship_soft). The decisionGateRequired carve-out above is
	// too coarse: a DONE stop (accomplishments present) bypasses it even when
	// the agent parked a live operator-directed ask in tldr/blockers/
	// handoff_notes — e.g. "Awaiting Matt to pick (a)/(b)" or "a human must
	// mint AWS secret X". The audit found 23% of such asks strand with no
	// decision/escalation and no open handle (~51 in never-respawn roles =
	// permanent). evaluateAskGate detects that case (scanning ONLY the
	// forward-looking fields, never accomplishments), routes decision-vs-
	// escalation, and emits a one-shot remediation nudge + telemetry. It is
	// SOFT by default (GB_STOP_ASK_GATE=warn, ship_soft): it NEVER blocks in
	// warn mode, NEVER blocks a despawn-class role, and self-releases on a
	// second identical stop — so it can never restart-loop (anti-ka-0000007734).
	if gate := evaluateAskGate(ctx, actor, agentID, wrapup, stopForce); gate.block {
		return fmt.Errorf("operator-directed ask detected with no decision/escalation filed — " +
			"file the handle named above, restate the wrap-up without the ask, or re-run with --force")
	}

	commentAuthor := actor
	if commentAuthor == "" || commentAuthor == "unknown" {
		commentAuthor = agentID
	}

	// Handle structured wrap-up if --wrapup is provided.
	if stopWrapUp != "" {
		// Say in the wrap-up what the PR park filed. Applied after the
		// ask-gate, which reads handoff_notes.
		if prParkNote(prParks) != "" {
			applyPRParkToWrapUp(wrapup, prParks)
			wrapupCommentText = WrapUpToComment(wrapup)
		}

		// Store structured wrap-up as a JSON field on the agent bead.
		wrapupJSON, err := MarshalWrapUp(wrapup)
		if err != nil {
			return fmt.Errorf("serializing wrap-up: %w", err)
		}
		if err := daemon.UpdateBeadFields(ctx, agentID, map[string]string{
			WrapUpFieldName: wrapupJSON,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not store wrap-up: %v\n", err)
		}

		// Also add a human-readable comment for the activity log.
		if err := daemon.AddComment(ctx, agentID, commentAuthor, wrapupCommentText); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not add wrap-up comment: %v\n", err)
		}

		// MR auto-link guard: if the wrap-up lists GitLab MR URLs that were
		// never `gb mr link`-ed, attach mr_url to the agent's task bead so the
		// gitlab-bridge dual-writes the MR bead + produces edge. Warn-only —
		// never blocks the despawn. See stop_mrlink.go and ka-0000000816.
		if !stopForce {
			for _, msg := range autoLinkWrapupMRs(ctx, daemon, agentID, wrapup) {
				fmt.Fprintf(os.Stderr, "%s\n", msg)
			}
		}

		fmt.Println("Wrap-up recorded.")
	} else {
		// Legacy path: use --reason or default message.
		reason := stopReason
		if reason == "" {
			reason = "agent self-stopped"
		}
		if note := prParkNote(prParks); note != "" {
			reason += "\n" + note
		}
		if err := daemon.AddComment(ctx, agentID, commentAuthor, "gb stop: "+reason); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not add stop comment: %v\n", err)
		}
	}

	// Best-effort human-exit-survey prompt (dark by default). Runs on every
	// gb stop regardless of wrap-up — many thread agents call `gb stop`
	// without `--wrapup` and we still want the rating prompt to post.
	if err := postHumanSurveyPrompt(ctx, agentID); err != nil {
		fmt.Fprintf(os.Stderr, "Note: human-survey prompt skipped: %v\n", err)
	}

	// --clean: retire all session JSONL files so next spawn starts fresh.
	if stopClean {
		if retired, errs := retireAgentSessions(); retired > 0 || len(errs) > 0 {
			fmt.Printf("Retired %d session log(s) (next spawn will start fresh).\n", retired)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "Warning: %v\n", e)
			}
		}
	}

	// successor=new: this run retires in favor of a freshly-minted successor
	// (persistent-agents Phase 1, task ka-0000006931, design memo §2.5). The
	// predecessor MUST close terminally — never suspend/freeze, which would keep
	// this same identity resumable and contradict "continued on a new card". So
	// the mint forces the terminal path below regardless of freeze-eligibility.
	wantSuccessorNew := wrapup != nil && wrapup.Successor == SuccessorNew

	// goal_unachievable forces the terminal close path, never a freeze/suspend:
	// a deterministically-unachievable identity must NOT stay resumable (P3,
	// ka-0000005843). Mirrors the wantSuccessorNew terminal-forcing below.
	goalUnachievable := wrapup != nil && wrapup.GoalUnachievable

	// SUSPEND vs DONE. A freeze-eligible agent (runsc + freeze/thaw enabled,
	// signalled by the controller via BOAT_FREEZE_ELIGIBLE) defaults to SUSPEND:
	// transition agent_state=stopping instead of closing the bead to done. That
	// transition is the sole AgentStop trigger (subscriber/sse.go), which drives
	// the controller to `runsc checkpoint --leave-running`, persist a thaw-able
	// snapshot, and tear down Pod A — leaving the bead in a state a later
	// AgentSpawn thaws from (fast-hot-resume epic ka-0000003080). --done or
	// --clean force the terminal done path (discard, no snapshot); a non-
	// freeze-eligible (runc) agent always takes it, unchanged from before.
	// successor=new also forces terminal (see above).
	if shouldSuspend() && !wantSuccessorNew && !goalUnachievable {
		return suspendAgent(ctx, agentID)
	}

	// Truthful-close claimed run-instance tasks on the terminal --force path
	// (ka-0000006105). --force skipped the claimed-work check above, so a
	// single-shot schedule/cron run-instance task can still be in_progress. If
	// we leave it, the stale-reaper closes it ~15min later with close_reason
	// "run-instance assignee agent gone (crash/OOM, not reaped)" — a false crash
	// record for an agent that stopped cleanly here (often with a full wrap-up).
	// Close it now with a truthful reason so the audit trail is honest and the
	// reaper never sees it. Only on the terminal path: the suspend (freeze/thaw)
	// path returned above and must keep its claim to resume later.
	if stopForce {
		closeForcedRunInstanceTasks(ctx, actor)
	} else if ownRunTask != "" {
		closeOwnRunInstanceTask(ctx, ownRunTask, actor)
	}

	// A pending CI wait (`gb wait ci`) keeps this identity revivable: the close
	// below stamps successor=same so the bridge's CI resolver can event-wake it
	// in place. Read after the gates so it reflects the wait as of this stop.
	ciWait := loadCIWait(ctx, agentID)
	if ciWait.Pending() && wantSuccessorNew {
		fmt.Fprintf(os.Stderr, "Warning: successor=new retires this identity, so the pending CI wait on %s will wake a fresh agent rather than resume this one.\n", ciWait.Key)
	}

	if err := markStopRequested(ctx, agentID); err != nil {
		return err
	}

	fmt.Printf("Stop requested for agent %s.\n", agentID)

	// Satisfy the decision gate so the Stop hook allows through.
	// gb done bypasses the normal yield flow when all work is complete.
	// Using "operator" prevents gate consumption so back-to-back stop
	// hook firings don't re-block before the process exits.
	satisfyStopGate(ctx, agentID)

	// Fsync session transcripts before CloseBead. The reconciler scales the
	// pod down shortly after the bead closes; if coop/claude has buffered
	// writes on the PVC, the .jsonl disappears on respawn (P0: lost resume).
	syncAgentSessions()

	// successor=new: mint the successor BEFORE closing this bead, then close the
	// predecessor with close_reason "superseded: continued as <successor-id>" so
	// the bridge card-chaining renderer links the retired card forward to the
	// fresh successor (persistent-agents Phase 1, task ka-0000006931). Best-effort
	// at the boundary: a mint failure falls back to the plain terminal close
	// below, so a stop is never dead-ended — worst case is a normal done card
	// with no successor, never a stuck agent.
	successorMinted := false
	if wantSuccessorNew {
		if successorID, mErr := mintSuccessorAgent(ctx, agentID, wrapup, actor); mErr != nil {
			// A spent lineage budget is not an error — it is the enforcer doing its
			// job (ka-0000001361): the chain hit its ceiling, so we close terminally
			// with no continuation rather than mint a fresh bead+pod forever. Log it
			// distinctly from a genuine mint failure.
			if errors.Is(mErr, errLineageBudgetSpent) {
				fmt.Fprintf(os.Stderr, "Notice: lineage continuation budget spent, closing terminally without a successor: %v\n", mErr)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: successor=new mint failed, closing without continuation: %v\n", mErr)
			}
		} else {
			if cErr := closePredecessorAsSuperseded(ctx, agentID, successorID, actor); cErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not close predecessor as superseded: %v\n", cErr)
			} else {
				successorMinted = true
				fmt.Printf("Agent bead %s closed — continued as %s.\n", agentID, successorID)
			}
		}
	}

	// Close the agent bead immediately so the reconciler stops tracking
	// this pod. Previously this was deferred to the entrypoint restart
	// loop, but if the entrypoint doesn't check stop_requested (e.g. the
	// gb init path), the bead stays open and the pod restart-loops.
	// Skipped when the successor mint already closed it as superseded above.
	if !successorMinted {
		closeFields := map[string]string{
			"agent_state":       "done",
			"gate_satisfied_by": "operator",
		}
		// goal_unachievable: stamp the durable terminal marker + a distinct
		// close_reason, and CLEAR any wake metadata a prior park may have left on
		// the bead (successor=same + next_respawn_at) so the wake-scanner can never
		// revive this deterministically-failing identity (P3, ka-0000005843).
		if goalUnachievable {
			closeFields[AgentBeadFieldGoalUnachievable] = "true"
			closeFields["close_reason"] = CloseReasonGoalUnachievable
			closeFields["successor"] = SuccessorNone
			closeFields[AgentBeadFieldNextRespawnAt] = ""
		}
		ciParked := applyCIWaitPark(closeFields, ciWait, goalUnachievable, time.Now())
		if err := daemon.CloseBead(ctx, agentID, closeFields); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not close agent bead: %v\n", err)
		} else if goalUnachievable {
			fmt.Printf("Agent bead %s closed (goal unachievable — terminal, no respawn).\n", agentID)
		} else if ciParked {
			fmt.Printf("Agent bead %s closed, parked on CI wait %s — you are resumed in place when it concludes, or by %s to check it with `gb wait ci --result`.\n", agentID, ciWait.Key, closeFields[AgentBeadFieldNextRespawnAt])
		} else {
			fmt.Printf("Agent bead %s closed.\n", agentID)
		}
	}

	// Request coop shutdown so the session actually terminates.
	requestCoopShutdown()

	fmt.Printf("Coop shutdown requested — pod will terminate after session ends.\n")
	return nil
}

// markStopRequested sets stop_requested=true on the agent bead. When that write
// fails (a daemon that denies this agent its own bead answers 403), it writes
// the local stop sentinel instead, which the restart loop in this pod honors
// as stop_requested (gd-aoSsfpwPGG). It fails only when both writes fail.
//
// The sentinel is written on ANY write error, not only a denial, on purpose:
// it records a fact local to this pod (the agent ran gb stop), which holds
// however the daemon failed. Narrowing it to 401/403 would turn a transient
// 5xx or timeout into a failed gb stop whose finished agent the restart loop
// then respawns to re-run its prompt. The sentinel only ever stops this agent
// in this pod (it is keyed by bead ID and lives in the pod-scoped /tmp), and
// the loop still closes the bead once it reads it.
func markStopRequested(ctx context.Context, agentID string) error {
	err := daemon.UpdateBeadFields(ctx, agentID, map[string]string{
		"stop_requested": "true",
	})
	if err == nil {
		return nil
	}
	if sErr := writeStopSentinel(agentID); sErr != nil {
		return fmt.Errorf("setting stop_requested on bead %s: %w (local stop sentinel also failed: %v)", agentID, err, sErr)
	}
	fmt.Fprintf(os.Stderr, "Warning: could not set stop_requested on bead %s (%v); wrote local stop sentinel %s, which this pod's restart loop honors instead.\n", agentID, err, stopSentinelPath)
	return nil
}

// shouldSuspend reports whether this gb stop should SUSPEND (freeze) the agent
// rather than terminally close it to done. True iff the agent is freeze-eligible
// (BOAT_FREEZE_ELIGIBLE=true, set by the controller for runsc + freeze/thaw
// agents) AND the caller did not force a terminal despawn via --done or --clean.
// --clean explicitly asks for a fresh next-spawn conversation, which is
// incompatible with thawing the frozen process, so it forces done.
func shouldSuspend() bool {
	return !stopDone && !stopClean && freezeEligible()
}

// freezeEligible reads the controller-injected BOAT_FREEZE_ELIGIBLE signal.
// The controller sets it at spawn (cmd/controller/podspec.go markFreezeEligible)
// only when freeze/thaw is enabled AND the pod resolved to the runsc
// RuntimeClass — mirroring its own shouldFreeze gate so the in-pod suspend
// decision and the controller's freeze decision agree.
func freezeEligible() bool {
	return os.Getenv("BOAT_FREEZE_ELIGIBLE") == "true"
}

// suspendAgent transitions a freeze-eligible agent into the SUSPEND path:
// agent_state=stopping (the sole AgentStop trigger) WITHOUT closing the bead or
// requesting coop shutdown. The controller observes the stopping transition,
// runs `runsc checkpoint --leave-running` on the live sandbox, persists the
// snapshot, and tears down Pod A. Two deliberate differences from the done
// path:
//
//   - We do NOT set stop_requested=true. stop_requested drives the entrypoint
//     restart loop to CloseBead(agent_state=done) (agent_start_k8s.go), which
//     would overwrite the stopping state and defeat the freeze. The controller's
//     pod teardown — not the entrypoint — ends this pod's life, after the
//     checkpoint has captured the live process.
//   - We do NOT request coop shutdown. The checkpoint must capture the LIVE
//     Claude process (true hot resume); shutting coop down first would freeze a
//     dead session. coop is terminated by the controller's Pod A teardown only
//     after the --leave-running checkpoint has been persisted.
//
// The decision gate is still satisfied so the Stop hook lets the turn end.
func suspendAgent(ctx context.Context, agentID string) error {
	// Satisfy the decision gate so the Stop hook allows the turn to end (same
	// rationale as the done path: "operator" prevents gate consumption).
	satisfyStopGate(ctx, agentID)

	// Record HOW the gate was satisfied. The server-side Stop hook
	// (kbeads http_hooks.go) treats a satisfied gate with an empty
	// gate_satisfied_by as a bypass and re-BLOCKS the stop ("gate was not
	// satisfied via 'gb yield'"), looping until the stop-gate.sh escape hatch
	// fires. The terminal done path sets this to "operator" inside CloseBead;
	// the suspend path must do the same or every freeze-eligible agent ending
	// a normal turn hits a multi-block cost-sink instead of a clean suspend.
	// "operator" is also non-consuming server-side, so a re-mention/thaw does
	// not need to re-satisfy the gate from scratch.
	if err := daemon.UpdateBeadFields(ctx, agentID, map[string]string{
		"gate_satisfied_by": "operator",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not set gate_satisfied_by on bead %s: %v\n", agentID, err)
	}

	// Fsync session transcripts before the controller checkpoints + tears down
	// Pod A, so buffered coop/claude writes are on the PVC if the snapshot ever
	// falls back to a cold resume (mirrors the done path's pre-close fsync).
	syncAgentSessions()

	// Transition to stopping — the sole AgentStop trigger. NOT a CloseBead: the
	// bead must stay open in a thaw-able state for a later AgentSpawn.
	if err := daemon.UpdateAgentState(ctx, agentID, "stopping"); err != nil {
		return fmt.Errorf("transitioning agent %s to stopping (suspend): %w", agentID, err)
	}

	fmt.Printf("Agent %s suspending (freeze) — controller will checkpoint and tear down the pod; a later spawn thaws it.\n", agentID)
	return nil
}

// decisionGateRequired reports whether the decision-bead gate should fire for
// this stop. It does NOT fire when:
//   - --force was passed (explicit override), or
//   - the agent supplied a valid DONE wrap-up (accomplishments present). By the
//     time this runs, the in-progress-work check has already returned early if
//     the agent still holds claimed work, so a valid wrap-up means
//     done-with-no-open-work — a legitimate clean stop with nothing to decide.
//
// This is the carve-out that breaks the circular guidance loop in
// ka-0000007734: a no-op / done cycle can now stop with a wrap-up and without a
// decision bead it could never validly create.
func decisionGateRequired(force bool, wrapup *WrapUp) bool {
	if force {
		return false
	}
	if isDoneWrapUp(wrapup) {
		return false
	}
	return true
}

// isDoneWrapUp reports whether wrapup represents a completed-work stop — i.e. a
// non-nil wrap-up whose accomplishments field is populated. Accomplishments is
// the wrap-up's only baseline-required content field (DefaultWrapUpRequirements),
// so its presence is the signal that the agent finished real work rather than
// silently exiting.
func isDoneWrapUp(wrapup *WrapUp) bool {
	return wrapup != nil && strings.TrimSpace(wrapup.Accomplishments) != ""
}

// requireDecisionBead checks that this agent created at least one decision bead.
// Returns an error if none found, preventing silent completion without review.
//
// Callers gate this on decisionGateRequired, so by the time the error path is
// reached the stop is NOT a clean done stop (no valid wrap-up). The remediation
// hint therefore must NOT point at a command that rejects a no-op cycle: the
// decision-options validator refuses an option-less decision and tells the
// agent to "use gb stop" — which is the very command that landed here. We avoid
// that loop by (a) pointing the BLOCKED case at `gb decision create` with the
// options requirement spelled out, and (b) stating the DONE path explicitly
// (finish with a --wrapup, or --force) rather than `kd create --type decision`.
func requireDecisionBead(ctx context.Context, agentActor string) error {
	if daemon == nil || agentActor == "" {
		return nil // can't check without daemon or actor identity
	}
	beads, err := daemon.ListDecisionBeads(ctx)
	if err != nil {
		// Non-fatal: don't block stop if the daemon is unreachable.
		fmt.Fprintf(os.Stderr, "Warning: could not check decision beads: %v\n", err)
		return nil
	}
	for _, b := range beads {
		if b.CreatedBy == agentActor {
			return nil // found one
		}
	}
	fmt.Fprintf(os.Stderr, "Error: no decision bead found from this agent (%s).\n", agentActor)
	fmt.Fprintf(os.Stderr, "If you are DONE and not blocked: re-run with a wrap-up — that clears this gate with no decision needed:\n")
	fmt.Fprintf(os.Stderr, "  gb stop --wrapup '{\"tldr\":\"<one-line outcome>\",\"accomplishments\":\"<what you did>\"}'\n")
	fmt.Fprintf(os.Stderr, "If you are BLOCKED on a real fork (divergent next actions): create a decision with options and yield:\n")
	fmt.Fprintf(os.Stderr, "  gb decision create --prompt \"<question>\" --options '[{\"id\":\"a\",\"short\":\"...\",\"label\":\"...\",\"artifact_type\":\"plan\"},{\"id\":\"b\",...}]' && gb yield\n")
	fmt.Fprintf(os.Stderr, "(A decision needs >=1 real option; for a status message do NOT create a decision — just stop with a wrap-up.)\n\n")
	fmt.Fprintf(os.Stderr, "Use --force to override.\n")
	return fmt.Errorf("no decision bead — stop with a --wrapup if done, or 'gb decision create' if blocked, or use --force")
}

// checkRequiredArtifact rejects a stop while a decision THIS lane owns is
// resolved and still owes the artifact its chosen option declared — i.e. a
// closed decision with required_artifact set and artifact_status != "submitted".
// It prints the exact remedy (gb decision report <id> --content @<file>) to
// stderr and returns a concise headline for the batched rejection.
//
// The mechanism it enforces: when an operator resolves a decision whose chosen
// option declared an artifact_type, `gb decision respond` stamps
// required_artifact=<type> + artifact_status=pending (decision.go), and the
// agent is supposed to deliver via `gb decision report`, which advances
// artifact_status pending → submitted (decision.go). `gb yield` already
// hard-fails on a pending artifact (yield.go); `gb stop` did not, so on the
// stop-and-be-respawned path the fleet is steered toward (advice
// ka-0000003244) the deliverable could be dropped silently (ka-0000006794).
//
// SCOPE — this is the bead's open design question, settled from source. The
// obvious key, requesting_agent_bead_id, is the EPHEMERAL agent-INSTANCE bead
// id, which changes on every respawn (decision.go, mine_filter.go): a NEW pod
// for the same logical agent carries a different requesting_agent_bead_id, so a
// gate keyed on it would be SILENTLY CLEARED at handoff — barely better than no
// gate, and exactly the regression the bead flagged. So we key on the DURABLE
// owner_seat, stamped from the agent bead's stable seat_key precisely so
// decision ownership survives respawn (ka-0000005031) — the same key `gb
// decision list --mine` uses. A respawn resolving its own seat therefore
// INHERITS the obligation rather than shedding it. When the caller has no
// resolvable seat (no agent bead / no seat_key) we cannot attribute ownership,
// so we fail OPEN (return nil): the gate never fires on an unidentifiable
// caller rather than firing spuriously or wedging an unrelated pod.
//
// Read-only: a single owner_seat-scoped list query, no writes. artifact_status
// is already written correctly by gb decision report — this is a read, not new
// bookkeeping. Fail-open on any daemon/lookup error, like every sibling gate: a
// flaky daemon must never wedge a stop.
func checkRequiredArtifact(ctx context.Context) error {
	if daemon == nil {
		return nil
	}
	seat := resolveActorSeat(ctx)
	if seat == "" {
		// No durable lane identity — cannot attribute a decision's obligation to
		// this caller. Fail open rather than gate an unidentifiable stop.
		return nil
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"closed"},
		Fields:   map[string]string{"owner_seat": seat},
		Sort:     "-created_at",
		Limit:    200,
	})
	if err != nil {
		// Non-fatal: a flaky daemon never wedges a stop.
		fmt.Fprintf(os.Stderr, "Warning: could not check required artifacts: %v\n", err)
		return nil
	}
	var pending []*beadsapi.BeadDetail
	for _, b := range result.Beads {
		ra := b.Fields["required_artifact"]
		if ra == "" {
			continue // this decision declared no artifact
		}
		// pending → still owed; submitted → delivered. Anything other than the
		// terminal "submitted" (incl. the canonical "pending") still owes.
		if b.Fields["artifact_status"] == "submitted" {
			continue
		}
		pending = append(pending, b)
	}
	if len(pending) == 0 {
		return nil
	}
	d := pending[0]
	ra := d.Fields["required_artifact"]
	fmt.Fprintf(os.Stderr, "Warning: a decision your lane owns still owes its required %s artifact:\n", ra)
	fmt.Fprintf(os.Stderr, "  %s: %s\n\n", d.ID, d.Title)
	fmt.Fprintf(os.Stderr, "Deliver it before stopping — write the %s to a file and report it:\n", ra)
	fmt.Fprintf(os.Stderr, "  gb decision report %s --content @<file>\n", d.ID)
	fmt.Fprintf(os.Stderr, "Use --force to override.\n")
	return fmt.Errorf("decision %s owes a %s artifact — run 'gb decision report %s --content @<file>', or use --force", d.ID, ra, d.ID)
}

// requestCoopShutdown POSTs to the local coop API to trigger a graceful
// session shutdown. This mirrors what monitor_agent_exit() does in the
// entrypoint. Failures are non-fatal since the agent may not be running
// inside coop (e.g. local dev).
func requestCoopShutdown() {
	coopURL := os.Getenv("COOP_URL")
	if coopURL == "" {
		coopURL = "http://localhost:8080"
	}
	shutdownURL := strings.TrimRight(coopURL, "/") + "/api/v1/shutdown"

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(shutdownURL, "", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: could not reach coop at %s (not running inside coop?): %v\n", shutdownURL, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "Note: coop shutdown returned status %d\n", resp.StatusCode)
	}
}

// parkHandleTypes are the bead types that can park a claimed task: an
// operator-actioned escalation or an operator-answered decision. Both are
// filed with `--requesting-task`, which writes the blocks-edge this lookup
// reads, and both auto-respawn the filing agent when they resolve.
var parkHandleTypes = map[string]bool{"escalation": true, "decision": true}

// parkHandleOpen are the handle statuses that still await an operator and can
// therefore park a claim. Live prod data uses only open/closed for both handle
// types (`gb escalation reject` closes the bead), but the schema also permits
// blocked/deferred — a handle in either is still pending, so it still parks.
var parkHandleOpen = map[string]bool{
	"open": true, "in_progress": true, "blocked": true, "deferred": true,
}

// parkHandleAgentFields are the field keys under which a handle records the
// agent-bead id of the agent that filed it. Escalations stamp
// `requesting_agent` (escalation.go); decisions stamp
// `requesting_agent_bead_id` (decision.go) — the two commands genuinely use
// different keys, so both must be consulted or the exemption silently never
// fires for one of the handle types.
var parkHandleAgentFields = []string{"requesting_agent", "requesting_agent_bead_id"}

// parkHandleTaskField is the field key under which a handle records the task
// it parks. Unlike the agent-id keys above, escalations (escalation.go) and
// decisions (decision.go) agree on this ONE key, so a single lookup covers
// both handle types.
const parkHandleTaskField = "requesting_task"

// findParkHandle reports the id of an OPEN escalation/decision parking taskID,
// or "" when the task is not parked behind such a handle.
//
// "Parked" means the sanctioned sequence in the crew contract: work is
// finished but un-closable (e.g. the fix lives on an unmerged MR, so the
// daemon's close guard correctly refuses), so the agent files a handle with
// `--requesting-task` — writing a blocks-edge from task to handle — and stops.
// The handle is the durable re-entry point: resolving it auto-respawns the
// agent. Such a task is NOT an abandoned claim and must not trip the gate.
//
// Ownership is keyed on the parked TASK, not the filing agent (ka-0000007825).
// A re-spawn continuing the same worked bead is a different agent bead AND a
// different actor than the lineage sibling that filed the handle, so an
// agent-identity check reads its own park as foreign and forces the re-spawn
// to `gb stop --force` — which overrides EVERY gate at once, exactly the
// blunt instrument the exemption exists to avoid. The handle's blocks-edge
// plus a `requesting_task` naming this very task is the sanctioned park
// regardless of which sibling filed it. Fails closed on a read error: an
// unreadable handle is treated as no exemption, so the gate still fires.
func findParkHandle(ctx context.Context, taskID, agentID string) string {
	deps, err := daemon.GetDependencies(ctx, taskID)
	if err != nil {
		return ""
	}
	for _, d := range deps {
		if d.Type != "blocks" || d.DependsOnID == "" {
			continue
		}
		handle, err := daemon.GetBead(ctx, d.DependsOnID)
		if err != nil || handle == nil {
			continue
		}
		// Only an OPEN handle parks: a resolved one has nothing left to
		// respawn on, so the claim really is abandoned. Allow-list the live
		// states rather than excluding "closed" — the bead schema also permits
		// blocked/deferred, and a future terminal state must fail closed
		// (gate fires) rather than silently grant a permanent exemption.
		if !parkHandleTypes[handle.Type] || !parkHandleOpen[handle.Status] {
			continue
		}
		// PRIMARY signal: the handle names this very task as the work it
		// parks. Lineage-agnostic, so a re-spawn is exempt too.
		if handle.Fields[parkHandleTaskField] == taskID {
			return handle.ID
		}
		// FALLBACK: a handle filed by this same agent that carries no
		// requesting_task (filed without --requesting-task, then edge-linked
		// by hand) still parks — preserves the ka-0000004133 behavior.
		if handleFiledBy(handle, agentID) {
			return handle.ID
		}
	}
	// THIRD branch: a pending CI wait on the stopping agent's OWN bead that names
	// this task (`gb wait ci`, plan wake-on-ci-completion.md §2.1). The wait is
	// fields on the agent bead, not a handle bead, so there is no blocks-edge to
	// follow; the agent bead itself is the re-entry point, woken in place when CI
	// concludes. Only `pending` parks: a concluded wait is refused outright by
	// checkCIWaitConcluded, and a consumed one has nothing left to wake on.
	if w := loadCIWait(ctx, agentID); w.Pending() && taskID != "" && w.Task == taskID {
		return agentID
	}
	return ""
}

// loadCIWait reads the ci_wait_* fields off the stopping agent's own bead. A
// read error returns the zero CIWait (no wait): the park exemption then fails
// closed (the gate still fires) and the concluded refusal fails open (a flaky
// daemon never wedges a stop).
func loadCIWait(ctx context.Context, agentID string) beadsapi.CIWait {
	if agentID == "" {
		return beadsapi.CIWait{}
	}
	bead, err := daemon.GetBead(ctx, agentID)
	if err != nil || bead == nil {
		return beadsapi.CIWait{}
	}
	return beadsapi.CIWaitFromFields(bead.Fields)
}

// checkCIWaitConcluded refuses a stop when this agent's CI wait already
// resolved or timed out (plan §2.1). That is the race where CI finishes before
// the agent stops: the resolver's wake found a live agent, so a stop now would
// park on a wake that has already been spent. Reading the result
// (`gb wait ci --result`) marks the wait consumed and releases this gate.
func checkCIWaitConcluded(w beadsapi.CIWait) error {
	if !w.Concluded() {
		return nil
	}
	fmt.Fprintf(os.Stderr, "Your CI wait on %s already concluded (%s) — nothing will wake you for it.\n", w.Key, w.State)
	fmt.Fprintf(os.Stderr, "Read the conclusions with 'gb wait ci --result', act on them, then stop.\n")
	return fmt.Errorf("CI already concluded (%s): run `gb wait ci --result`", w.State)
}

// applyCIWaitPark stamps the revivable-close marker on closeFields when the
// agent stops with a pending CI wait, and reports whether it did. successor=same
// is the dormant-identity marker the bridge's event-wake keys on
// (isDormantParkedIdentity), so the CI resolver can reopen this bead in place:
// same name, same claimed task, same transcript. It also stamps next_respawn_at
// (ciWaitNextWake) so the controller wake-scanner revives the identity on a
// timer even with no bridge resolver: none has shipped yet (plan P2-P4), and
// without the timer a CI park was never woken at all (gd-PwyvuF0fHp). The woken
// agent's `gb wait ci --result` reads GitHub itself. A goal-unachievable stop
// stays terminal regardless.
func applyCIWaitPark(closeFields map[string]string, w beadsapi.CIWait, goalUnachievable bool, now time.Time) bool {
	if goalUnachievable || !w.Pending() {
		return false
	}
	closeFields["successor"] = SuccessorSame
	closeFields["close_reason"] = CloseReasonCIWaitParked
	closeFields[AgentBeadFieldNextRespawnAt] = ciWaitNextWake(w, now)
	return true
}

// CloseReasonCIWaitParked is the close_reason of an agent that stopped parked
// on a pending CI wait, distinguishing it from a terminal done in the audit trail.
const CloseReasonCIWaitParked = "ci_wait_parked"

// handleFiledBy reports whether handle was filed by the stopping agent —
// matching either the recorded requesting-agent bead id or the bead's
// created_by actor name.
//
// This is the FALLBACK ownership signal; findParkHandle prefers a
// `requesting_task` match, which is lineage-agnostic and so also covers a
// re-spawn parked behind a sibling's handle (ka-0000007825). This check still
// earns its keep for a handle filed without `--requesting-task` and linked to
// the task by a hand-written blocks-edge, where the field is absent entirely.
func handleFiledBy(handle *beadsapi.BeadDetail, agentID string) bool {
	for _, f := range parkHandleAgentFields {
		if v := handle.Fields[f]; v != "" && agentID != "" && v == agentID {
			return true
		}
	}
	return handle.CreatedBy != "" && handle.CreatedBy == actor
}

// claimedWorkGate rejects a stop while the agent still holds a claimed
// in-progress issue-kind bead. It prints the full remediation (close /
// unclaim / --force) to stderr and returns a concise headline error for the
// batched rejection. A daemon error or no-claimed-work returns nil (fail-open
// — a flaky daemon never wedges a stop). Extracted from the inline pre-flight
// check so the claimed-work gate can be collected alongside the other
// read-only gates rather than early-returning (ka-0000005647).
//
// A claimed task PARKED behind an open escalation/decision that names it via
// `requesting_task` is exempt (ka-0000004133, widened by ka-0000007825): that
// is the sanctioned "finished, but the close is gated on an operator action"
// path, and the handle already auto-respawns on resolve. Before this carve-out
// the only way past the gate was `gb stop --force`, which overrides EVERY gate
// at once — so following the park contract cost the agent all its other safety
// checks. Keying on the parked task rather than the filing agent extends that
// to a re-spawn continuing the same bead behind a lineage sibling's handle.
// This is an exemption, not a bypass: a claim with no open handle parking it
// still trips the gate, so abandoned claims are still caught.
//
// A scheduled run's own run-instance task (the `Schedule:` bead its trigger
// assigned it, named by task_id on the agent bead) is exempt too
// (gd-mOnsZZyug9): the run IS that task, so it is done when the run stops.
// Refusing sent every scheduled run that followed its "end with gb stop"
// prompt round a refuse / kd close / stop again loop. The exemption is that one
// bead only: any other run-instance or ordinary claim still trips the gate.
//
// It also returns the id of the scheduled run's own run-instance task when it
// let it pass, so the terminal stop can close it (closeOwnRunInstanceTask). A parked run-instance task is
// reported as parked, not as own, so a stop parked on it keeps the claim.
//
// A task with awaits_release=true is parked behind its release (kbeads
// releases_awaits.go, gd-ZM7NEBPAqZ): its work is merged and the deployment
// record of a release that carries it closes it.
//
// heldPRs names the tasks whose open PR the PR park held back on the task's
// own hold (heldPRTasks, gd-L9Bfqv2Ndn): the hold parks them, so they pass
// and keep the claim. A hold on a task with no open PR does not count.
func claimedWorkGate(ctx context.Context, agentID string, heldPRs map[string]string) (string, error) {
	tasks, taskErr := daemon.ListAssignedTasks(ctx, actor)
	if taskErr != nil || len(tasks) == 0 {
		return "", nil
	}
	// Every claimed bead must be accounted for — a parked first claim must not
	// mask an abandoned second one.
	var pending []*beadsapi.BeadDetail
	var ownRun string
	var ownTaskID *string // agent bead's task_id, read once on the first run-instance claim
	for _, task := range tasks {
		if handle := findParkHandle(ctx, task.ID, agentID); handle != "" {
			fmt.Fprintf(os.Stderr, "%s parked behind %s — allowing stop; will respawn on resolve.\n", task.ID, handle)
			continue
		}
		if beadsapi.AwaitsRelease(task.Fields) {
			fmt.Fprintf(os.Stderr, "%s awaits its release (awaits_release=true) — allowing stop; it closes when a deployment carries it.\n", task.ID)
			continue
		}
		if hold := heldPRs[task.ID]; hold != "" {
			fmt.Fprintf(os.Stderr, "%s holds its PR (%s) — allowing stop; the claim stays.\n", task.ID, hold)
			continue
		}
		if isRunInstanceTaskDetail(task) {
			if ownTaskID == nil {
				id := agentTaskID(ctx, agentID)
				ownTaskID = &id
			}
			if *ownTaskID != "" && task.ID == *ownTaskID {
				fmt.Fprintf(os.Stderr, "%s is this scheduled run's own task — allowing stop; it closes with the run.\n", task.ID)
				ownRun = task.ID
				continue
			}
		}
		pending = append(pending, task)
	}
	if len(pending) == 0 {
		return ownRun, nil
	}
	task := pending[0]
	fmt.Fprintf(os.Stderr, "Warning: you have claimed in-progress work:\n")
	fmt.Fprintf(os.Stderr, "  %s: %s\n\n", task.ID, task.Title)
	fmt.Fprintf(os.Stderr, "Close it ('kd close %s'), release it ('kd unclaim %s'), or use --force to override.\n", task.ID, task.ID)
	fmt.Fprintf(os.Stderr, "If the work is DONE but the close is gated on an operator action (e.g. an unmerged MR),\n")
	fmt.Fprintf(os.Stderr, "park it instead: 'gb escalation create ... --requesting-task %s --close-on-execute' then stop.\n", task.ID)
	return "", fmt.Errorf("claimed work pending: %s — close ('kd close %s') or unclaim ('kd unclaim %s')", task.ID, task.ID, task.ID)
}

// agentTaskID returns the task_id the spawner stamped on the agent bead (for a
// scheduled run, its run-instance task). A read error returns "", so the
// own-run exemption fails closed and the gate still fires.
func agentTaskID(ctx context.Context, agentID string) string {
	if agentID == "" {
		return ""
	}
	bead, err := daemon.GetBead(ctx, agentID)
	if err != nil || bead == nil {
		return ""
	}
	return bead.Fields["task_id"]
}

// renderStopGateRejection formats every collected read-only despawn-gate
// failure into a single numbered rejection under one "Cannot stop" header, so
// gb stop surfaces all knowable-at-once failures in ONE invocation instead of
// one per round-trip (ka-0000005647). Returns "" when there are no failures
// (the stop proceeds). Each gate has already printed its own detailed guidance
// to stderr; this is the concise consolidated recap plus the single --force
// escape note.
func renderStopGateRejection(failures []error) string {
	if len(failures) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nCannot stop — %d despawn gate(s) not satisfied:\n", len(failures))
	for i, e := range failures {
		fmt.Fprintf(&b, "  [%d] %s\n", i+1, e.Error())
	}
	b.WriteString("Address all of the above, or re-run with --force to override every gate at once.\n")
	return b.String()
}

// checkDelivery verifies that the agent has pushed its work before stopping.
// Returns an error if there are unpushed commits on the current branch.
func checkDelivery() error {
	// Find the workspace directory — check common agent workspace paths.
	workspace := os.Getenv("WORKSPACE")
	if workspace == "" {
		workspace = os.Getenv("HOME") + "/workspace"
	}

	// Check if it's a git repo.
	gitDir := filepath.Join(workspace, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return nil // not a git repo, nothing to check
	}

	// Check for unpushed commits: compare HEAD with upstream.
	// git rev-list @{u}..HEAD counts commits ahead of upstream.
	cmd := exec.Command("git", "-C", workspace, "rev-list", "--count", "@{u}..HEAD")
	out, err := cmd.Output()
	if err != nil {
		// No upstream set — check if there are any local commits at all.
		cmd2 := exec.Command("git", "-C", workspace, "log", "--oneline", "-1")
		if out2, err2 := cmd2.Output(); err2 == nil && len(strings.TrimSpace(string(out2))) > 0 {
			// There are commits but no upstream — agent never pushed.
			branch := currentBranch(workspace)
			if branch == "main" || branch == "master" {
				fmt.Fprintf(os.Stderr, "Warning: you have local commits on '%s' that were never pushed to a feature branch.\n", branch)
				fmt.Fprintf(os.Stderr, "Create a branch, push it, and open a PR before stopping.\n")
				fmt.Fprintf(os.Stderr, "Use --force to override.\n")
				return fmt.Errorf("unpushed commits on %s — create a branch + PR, or use --force", branch)
			}
			fmt.Fprintf(os.Stderr, "Warning: branch '%s' has no upstream. Push with: git push -u origin %s\n", branch, branch)
			fmt.Fprintf(os.Stderr, "Use --force to override.\n")
			return fmt.Errorf("unpushed branch '%s' — push and create a PR, or use --force", branch)
		}
		return nil // no commits at all, fine
	}

	count := strings.TrimSpace(string(out))
	if count != "0" {
		branch := currentBranch(workspace)
		fmt.Fprintf(os.Stderr, "Warning: %s unpushed commit(s) on branch '%s'.\n", count, branch)
		fmt.Fprintf(os.Stderr, "Push your branch and create a PR before stopping.\n")
		fmt.Fprintf(os.Stderr, "Use --force to override.\n")
		return fmt.Errorf("%s unpushed commit(s) on '%s' — push and create a PR, or use --force", count, branch)
	}

	return nil
}

// currentBranch returns the current git branch name in the given directory.
func currentBranch(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// agentSessionDuration returns the elapsed time since the agent bead was
// created. Used as a proxy for session length in human-survey eligibility.
// Returns 0 when the bead can't be fetched (best-effort).
func agentSessionDuration(ctx context.Context, agentID string) time.Duration {
	if daemon == nil || agentID == "" {
		return 0
	}
	b, err := daemon.GetBead(ctx, agentID)
	if err != nil || b == nil || b.CreatedAt.IsZero() {
		return 0
	}
	return time.Since(b.CreatedAt)
}

// applySuccessorFlag folds the --successor convenience flag into the raw
// --wrapup JSON so all successor validation/reconciliation happens in one place
// (processWrapUp). --wrapup JSON is authoritative: when the JSON already carries
// a "successor" key that value wins and the flag is ignored (with a conflict
// warning). When the flag is set and the JSON omits successor, the flag value
// is injected. With no --wrapup at all, a minimal {"successor":...} object is
// synthesized so the flag is not silently dropped (the wrap-up gate still
// applies downstream). The flag value is validated here so an unknown
// --successor is rejected at parse, matching the JSON path (design memo §3.2).
func applySuccessorFlag(rawWrapup, flag string) (string, error) {
	if strings.TrimSpace(flag) == "" {
		return rawWrapup, nil
	}
	canonical, err := ValidateSuccessor(flag)
	if err != nil {
		return "", fmt.Errorf("invalid --successor flag: %w", err)
	}
	if strings.TrimSpace(rawWrapup) == "" {
		out, err := json.Marshal(map[string]string{"successor": canonical})
		if err != nil {
			return "", fmt.Errorf("synthesizing successor wrap-up: %w", err)
		}
		return string(out), nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawWrapup), &fields); err != nil {
		// Not a JSON object — let processWrapUp surface the parse error verbatim.
		return rawWrapup, nil
	}
	if _, ok := fields["successor"]; ok {
		fmt.Fprintf(os.Stderr, "Warning: both --successor and a successor in --wrapup JSON were provided; the --wrapup value wins.\n")
		return rawWrapup, nil
	}
	enc, err := json.Marshal(canonical)
	if err != nil {
		return rawWrapup, nil
	}
	fields["successor"] = enc
	out, err := json.Marshal(fields)
	if err != nil {
		return rawWrapup, nil
	}
	return string(out), nil
}

// applyGoalUnachievableFlag folds the --goal-unachievable convenience flag into
// the raw --wrapup JSON, mirroring applySuccessorFlag (P3, ka-0000005843). The
// flag is a no-op when false so the common path is untouched. --wrapup JSON is
// authoritative: when it already carries a "goal_unachievable" key that value
// wins and the flag is ignored (with a conflict warning). With no --wrapup at
// all, a minimal {"goal_unachievable":true} object is synthesized so the flag is
// not silently dropped (the wrap-up gate still applies downstream).
func applyGoalUnachievableFlag(rawWrapup string, flag bool) (string, error) {
	if !flag {
		return rawWrapup, nil
	}
	if strings.TrimSpace(rawWrapup) == "" {
		out, err := json.Marshal(map[string]bool{"goal_unachievable": true})
		if err != nil {
			return "", fmt.Errorf("synthesizing goal_unachievable wrap-up: %w", err)
		}
		return string(out), nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawWrapup), &fields); err != nil {
		// Not a JSON object — let processWrapUp surface the parse error verbatim.
		return rawWrapup, nil
	}
	if _, ok := fields["goal_unachievable"]; ok {
		fmt.Fprintf(os.Stderr, "Warning: both --goal-unachievable and a goal_unachievable in --wrapup JSON were provided; the --wrapup value wins.\n")
		return rawWrapup, nil
	}
	fields["goal_unachievable"] = json.RawMessage("true")
	out, err := json.Marshal(fields)
	if err != nil {
		return rawWrapup, nil
	}
	return string(out), nil
}

// processWrapUp parses the --wrapup flag value into a WrapUp struct and
// generates a human-readable comment. The flag value must be valid JSON
// matching the WrapUp schema, except that a bare-string pull_requests is
// forgiven (coerced to a 1-element array with a warning).
func processWrapUp(raw string) (*WrapUp, string, error) {
	coerced, coerceWarnings := coerceWrapUpJSON(raw)
	for _, warn := range coerceWarnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", warn)
	}

	var w WrapUp
	if err := json.Unmarshal([]byte(coerced), &w); err != nil {
		return nil, "", fmt.Errorf("invalid --wrapup JSON: %w\nExpected format: %s", err, wrapUpExpectedFormatHint())
	}

	// Normalize + validate the optional tldr before it is persisted or
	// emitted in the lifecycle event payload. A hard-cap violation is fatal
	// (the agent should retry with a shorter summary); collapse/soft-cap
	// notes are surfaced as warnings but do not block the stop.
	clean, warnings, err := normalizeAndValidateTLDR(w.TLDR)
	if err != nil {
		return nil, "", fmt.Errorf("invalid --wrapup tldr: %w", err)
	}
	w.TLDR = clean
	for _, warn := range warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", warn)
	}

	// Validate + canonicalize the successor enum (reject unknown at parse),
	// then apply the goal_satisfied/successor contradiction rule (design memo
	// §2.5). An unknown successor is fatal — the agent should retry with a
	// valid value; the contradiction is a warn-and-coerce, never fatal.
	successor, err := ValidateSuccessor(w.Successor)
	if err != nil {
		return nil, "", fmt.Errorf("invalid --wrapup successor: %w", err)
	}
	successor, contradiction := reconcileSuccessorGoal(successor, w.GoalSatisfied)
	if contradiction != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", contradiction)
	}
	// goal_unachievable is the strongest circuit breaker: it overrides ANY
	// requested successor (same or new) to none so a deterministically-failing
	// task burns no further pods (P3, ka-0000005843). Runs last so it wins over
	// the contradiction rule above.
	successor, unachievableNotice := reconcileGoalUnachievable(successor, w.GoalUnachievable)
	if unachievableNotice != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", unachievableNotice)
	}
	// Persist the canonical value only when it carries intent; leave "none"
	// out of the stored JSON (omitempty) so an inert wrap-up stays minimal.
	if successor == SuccessorNone {
		w.Successor = ""
	} else {
		w.Successor = successor
	}

	return &w, WrapUpToComment(&w), nil
}

// syncAgentSessions fsyncs every .jsonl transcript under ~/.claude/projects/
// and each containing project directory, then pauses briefly so the PVC
// captures the latest coop/claude writes before CloseBead triggers pod teardown.
//
// Best-effort: open/sync failures are logged and never block stop.
func syncAgentSessions() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: could not resolve home dir for fsync: %v\n", err)
		return
	}
	projectsDir := filepath.Join(homeDir, ".claude", "projects")

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return // nothing to sync (dir may not exist)
	}

	synced := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		subDir := filepath.Join(projectsDir, entry.Name())
		subEntries, err := os.ReadDir(subDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Note: read %s: %v\n", subDir, err)
			continue
		}
		for _, se := range subEntries {
			if se.IsDir() || !strings.HasSuffix(se.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(subDir, se.Name())
			if err := fsyncPath(path, os.O_RDWR); err != nil {
				fmt.Fprintf(os.Stderr, "Note: fsync %s: %v\n", path, err)
				continue
			}
			fmt.Printf("fsynced %s\n", path)
			synced++
		}
		// Sync the directory so dirent updates (new files, renames) persist.
		if err := fsyncPath(subDir, os.O_RDONLY); err != nil {
			fmt.Fprintf(os.Stderr, "Note: fsync dir %s: %v\n", subDir, err)
		}
	}

	// Short drain so any in-flight userspace buffer from coop/claude reaches
	// the kernel — fsync is synchronous but only flushes what the kernel has.
	if synced > 0 {
		time.Sleep(2 * time.Second)
	}
}

// fsyncPath opens path with the given flags, calls Sync(), and closes the fd.
func fsyncPath(path string, flags int) error {
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// retireAgentSessions renames all .jsonl session logs under ~/.claude/projects/
// to .jsonl.stale, causing the restart loop's findResumeSession to skip them.
// Returns the count of retired files and any errors encountered.
func retireAgentSessions() (int, []error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return 0, []error{fmt.Errorf("finding home dir: %w", err)}
	}
	projectsDir := filepath.Join(homeDir, ".claude", "projects")

	var errs []error
	retired := 0
	_ = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") || strings.Contains(path, "/subagents/") {
			return nil
		}
		stalePath := path + ".stale"
		if renameErr := os.Rename(path, stalePath); renameErr != nil {
			errs = append(errs, fmt.Errorf("retiring %s: %w", path, renameErr))
		} else {
			retired++
		}
		return nil
	})
	return retired, errs
}

// satisfyStopGate satisfies agentID's decision gate so the Stop hook lets
// the turn end. On a land door it does nothing: the gate route is admin-only
// and off the door, and an agent there (a laptop agent) has no Stop gate.
func satisfyStopGate(ctx context.Context, agentID string) {
	if landDoor() != "" {
		return
	}
	if err := daemon.SatisfyGate(ctx, agentID, "decision"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not satisfy decision gate: %v\n", err)
	}
}
