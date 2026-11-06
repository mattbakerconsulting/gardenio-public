package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/deathreport"
	"gardenio/controller/internal/harness"

	"gardenio/clicreds/avareauth"
	"github.com/spf13/cobra"
)

// ── gb hook stop-gate ─────────────────────────────────────────────────────

// stopGateCooldownFile tracks when the last stop-gate block occurred.
// Used for debouncing to prevent repeated text injections.
// Variable (not const) so tests can override it.
var stopGateCooldownFile = "/tmp/stop-gate-last-block"

// stopGateBlockCountFile tracks how many times the stop-gate has blocked
// in the current session. Used for exponential backoff.
var stopGateBlockCountFile = "/tmp/stop-gate-block-count"

// stopGateYieldMarkerFile is dropped by `gb yield` so the Stop hook can
// fast-path block silently while a decision is pending (see yield.go). It lives
// in the same pod-scoped /tmp EmptyDir as the cooldown/block-count files and is
// reset at each session launch for the same reason (ka-0000007830). Variable
// (not const) so tests can override it.
var stopGateYieldMarkerFile = "/tmp/stop-gate-yielding"

// stopGateDefaultCooldownSecs is the base cooldown between stop-gate text
// injections. Doubles with each consecutive block (exponential backoff).
// Overridden by STOP_GATE_COOLDOWN_SECS env var.
//
// Aliased to deathreport.StopGateDefaultCooldownSecs so the base cooldown, the
// exponential-backoff curve (stopGateInCooldown -> deathreport.CooldownWindowSecs),
// and the death-report cadence diff (deathreport.StopHookCadenceDiff) share ONE
// definition — a forensic report can never disagree with the math that killed
// the agent.
const stopGateDefaultCooldownSecs = deathreport.StopGateDefaultCooldownSecs

// stopGateMaxBlocks is the number of consecutive blocks before the stop-gate
// allows the stop as an escape hatch. This prevents infinite cost-sink loops
// where the agent repeatedly creates decisions + yields without making progress.
const stopGateMaxBlocks = deathreport.StopGateMaxBlocks

// Stop-gate output formats. Claude Code reads a block from exit code 2 plus the
// reason on stderr; Codex needs `{"decision":"block","reason":...}` on stdout
// and exit 0 (ADR 0004 §8, verified in the spike). Exit 2 is not relied on for
// Codex. Gemini's AfterAgent hook honours the same JSON form (ADR 0005 §11.3,
// phase 0 spike R2); its `{"continue":true}` does NOT continue a turn, so a
// wrong-format block would fail open.
const (
	stopGateFormatClaude = beadsapi.HarnessClaude
	stopGateFormatCodex  = beadsapi.HarnessCodex
	stopGateFormatGemini = beadsapi.HarnessGemini
)

// jsonStdoutFormat reports whether format blocks with a single JSON document on
// stdout and exit 0 (Codex, Gemini) rather than Claude's exit 2 plus stderr.
func jsonStdoutFormat(format string) bool {
	return format == stopGateFormatCodex || format == stopGateFormatGemini
}

// resolveStopGateFormat validates --format; an empty value defaults to the
// harness the pod runs (GARDENIO_HARNESS), so the hook command in hooks.json
// and settings.json is identical for both.
func resolveStopGateFormat(flag string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "":
		return harness.MustFromEnv(), nil
	case stopGateFormatClaude:
		return stopGateFormatClaude, nil
	case stopGateFormatCodex:
		return stopGateFormatCodex, nil
	case stopGateFormatGemini:
		return stopGateFormatGemini, nil
	}
	return "", fmt.Errorf("unknown --format %q (want claude, codex or gemini)", flag)
}

// renderStopGateBlock renders a gate block for the given format. text is the
// checkpoint prose the Claude path prints to stdout (system-reminder blocks);
// reason is the server's block reason.
//
// Claude: stdout=text, stderr=JSON line, exit 2.
// Codex:  stdout=JSON only, exit 0. Codex treats plain stdout on Stop as
// invalid, so the checkpoint prose is folded into the JSON reason (it comes
// back to the model as a <hook_prompt> user message).
// Gemini: the same JSON-only form. The reason is sent to the model as a new user
// prompt and AfterAgent fires again with stop_hook_active=true.
func renderStopGateBlock(format, text, reason string) (stdout, stderr string, exitCode int) {
	if jsonStdoutFormat(format) {
		combined := strings.TrimSpace(strings.TrimSpace(text) + "\n" + strings.TrimSpace(reason))
		b, _ := json.Marshal(map[string]string{"decision": "block", "reason": combined})
		return string(b) + "\n", "", 0
	}
	b, _ := json.Marshal(map[string]string{"decision": "block", "reason": reason})
	return text, string(b) + "\n", 2
}

// stopGateOutput routes the gate's stdout prose. Claude gets it immediately;
// Codex buffers it so stdout stays a single JSON document (or empty on allow).
type stopGateOutput struct {
	format  string
	pending strings.Builder
}

func init() {
	hookStopGateCmd.Flags().String("format", "", "block output format: claude (exit 2 + stderr), codex or gemini (JSON on stdout, exit 0); default follows GARDENIO_HARNESS")
}

func (o *stopGateOutput) print(s string) {
	if jsonStdoutFormat(o.format) {
		o.pending.WriteString(s)
		return
	}
	fmt.Print(s)
}

// block emits the block for the active format and exits. The prose already
// printed (Claude) is not repeated.
func (o *stopGateOutput) block(reason string) {
	text := ""
	if jsonStdoutFormat(o.format) {
		text = o.pending.String()
	}
	stdout, stderr, code := renderStopGateBlock(o.format, text, reason)
	fmt.Fprint(os.Stdout, stdout)
	fmt.Fprint(os.Stderr, stderr)
	os.Exit(code)
}

var hookStopGateCmd = &cobra.Command{
	Use:   "stop-gate",
	Short: "Emit Stop hook event and handle gate block (replaces stop-gate.sh)",
	Long: `Evaluates the Stop gate for the running agent CLI.

--format claude  block = exit 2, reason JSON on stderr (Claude Code)
--format codex   block = {"decision":"block","reason":...} on stdout, exit 0 (Codex)
--format gemini  same JSON form on stdout, exit 0 (Gemini AfterAgent)

The default follows GARDENIO_HARNESS, so the hook command is the same for both.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		formatFlag, _ := cmd.Flags().GetString("format")
		format, err := resolveStopGateFormat(formatFlag)
		if err != nil {
			return err
		}
		out := &stopGateOutput{format: format}
		// The hook input, read once up front so every verdict below can be
		// published with its session id (gd-fSdUbOmf3g).
		stdinEvent := readStopGateStdin()
		// ── Job-mode bypass ──────────────────────────────────────────
		// Job-mode agents (thread agents, one-shot tasks) idle and wait
		// for follow-up messages. The decision gate is only meaningful
		// for crew-mode agents that should checkpoint before exiting.
		if os.Getenv("BOAT_MODE") == "job" {
			publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "job_mode")
			os.Exit(0)
		}

		// -- Yield-parked fast path (ka-0000002844) -------------------
		// A crew-mode agent genuinely parked on an open human decision
		// drops the yield marker (gb yield) and then ends its model turn
		// cleanly with nothing left to do but wait. That quiet turn-end is
		// the CORRECT resting state -- not a death. The wired Go stop-gate
		// historically lacked the yield-marker fast-path the legacy shell
		// hook has (stop-gate.sh:27), so those quiet turn-ends fell through
		// to the cooldown / escape-hatch path below -- every one of which
		// calls closeAgentBeadOnExit and closes the bead as
		// stop_gate_cooldown_involuntary, KILLING an agent that was
		// correctly waiting on the operator. Mirror the shell hook: if the
		// yield marker is present AND the agent still owns an open decision,
		// exit 0 WITHOUT closing the bead. The reconciler keeps the pod
		// alive on the open bead, and the decision-resolve nudge
		// (nudgeDecisionAgent -> NudgeAgent) re-invokes the live session the
		// moment the operator answers. Requiring a still-open decision (not
		// just the marker) means a stale/leaked marker can never wedge a
		// genuine shutdown, and the check falls through on any daemon error.
		if _, err := os.Stat(stopGateYieldMarkerFile); err == nil {
			if stopGateAgentParkedOnOpenDecision(cmd.Context()) {
				fmt.Fprintln(os.Stderr, "[stop-gate] agent is parked on an open decision (yield marker present); allowing quiet turn-end without closing bead")
				publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "yield_parked")
				os.Exit(0)
			}
		}

		// ── Background-work pass (gd-F91YfKwYXl) ────────────────────
		// A turn that ends while the CLI still runs an async subagent, a
		// Monitor or a run_in_background command is not an idle end of
		// session: the CLI re-invokes the session with a <task-notification>
		// when each finishes. Blocking it made agents hold the turn with a
		// foreground poll instead. Allow it without closing the bead, before
		// the cooldown and escape-hatch paths, which would otherwise close it.
		// Only Claude transcripts carry this evidence.
		if format == stopGateFormatClaude {
			if bg := stopGateBackgroundWork(stdinEvent); bg.Pending() {
				fmt.Fprintf(os.Stderr, "[stop-gate] background work running (%s): ending the turn; the session is re-invoked when it completes\n", bg)
				publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "background_work")
				os.Exit(0)
			}
		}

		// ── Cooldown debouncing ──────────────────────────────────────
		// If we blocked recently, allow stop silently. Exit 0 (not 2)
		// because exit 2 doesn't actually prevent continuation when
		// other Stop hooks (e.g. coop pipe) run later and exit 0, but
		// it DOES generate a "[No stderr output]" feedback injection
		// that wakes Claude up and creates a rapid-fire stop loop.
		// The gate already injected its text on the first block; if
		// Claude still wants to stop, let it.
		//
		// CLAIM GUARD (ka-0000006454, decision ka-0000005194 = "claim-guard"):
		// whether we also CLOSE the bead now turns on whether the agent still
		// holds a claimed in-progress task. Closing is the lethal act — the
		// reconciler stops keeping the pod alive — and it was firing on the
		// single most common shape in the fleet: two quiet turn-ends inside one
		// cooldown window, which is ordinary behavior for a working agent. That
		// made this the #2 involuntary-death vector fleet-wide (34 deaths in 7d,
		// ~8x the stuck-watchdog disabled in !6378), and each death reopened a
		// live task and threw away the work in flight.
		//
		// A claimed in-progress task is the best available evidence that real
		// work is still in flight: `gb stop` REFUSES to despawn while one is held
		// (claimedWorkGate, stop.go), so a cleanly-finished agent never reaches
		// here still holding a claim. Holding a claim therefore means the agent
		// did NOT finish — killing it is always wrong.
		//
		// This does NOT weaken the cost-sink guard the cooldown exists for. The
		// decision->yield->block runaway is bounded by the consecutive-block
		// escape hatch below (stopGateMaxBlocks), whose counter is advanced ONLY
		// by the block path (recordStopGateBlock) — never by this branch. So a
		// genuine loop still walks the counter to 5 and gets closed by the escape
		// hatch, claim or no claim; all we give up is killing it a few turns
		// sooner. Backstops if the agent instead goes quiet holding its claim:
		// the stale reaper (STALE_AGENT_TIMEOUT, 2h) reaps on updated_at, and
		// reopenStrandedIssueTasks returns the claim to `gb ready`.
		if stopGateInCooldown() {
			if sparedReason, spare := stopGateCooldownSparesAgent(cmd.Context()); spare {
				fmt.Fprintf(os.Stderr, "[stop-gate] cooldown reached but agent is still working (%s); allowing quiet turn-end without closing bead (ka-0000006454)\n", sparedReason)
				publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "cooldown_spared")
				os.Exit(0)
			}
			publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "cooldown")
			closeAgentBeadOnExit("cooldown")
			os.Exit(0)
		}

		// ── Escape hatch ─────────────────────────────────────────────
		// After too many consecutive blocks, allow the stop to prevent
		// infinite cost-sink loops (decision → yield → block → repeat).
		if stopGateEscapeHatch() {
			fmt.Fprintf(os.Stderr, "[stop-gate] Escape hatch: %d consecutive blocks, allowing stop\n", readStopGateBlockCount())
			out.print("<system-reminder>Stop gate escape hatch activated after repeated blocks. " +
				"If you have unfinished work, call `gb done` explicitly next time.</system-reminder>\n")
			clearStopGateCooldown()
			publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "escape_hatch")
			closeAgentBeadOnExit("escape_hatch")
			os.Exit(0)
		}

		// ── Rate-limit escape hatch ─────────────────────────────────
		// If the agent is rate-limited, allow the stop unconditionally.
		if isAgentRateLimited() {
			fmt.Fprintf(os.Stderr, "[stop-gate] Agent is rate-limited, allowing stop without checkpoint\n")
			clearStopGateCooldown()
			publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "rate_limited")
			closeAgentBeadOnExit("rate_limited")
			os.Exit(0)
		}

		claudeSessionID, _ := stdinEvent["session_id"].(string)
		cwd, _ := stdinEvent["cwd"].(string)
		if cwd == "" {
			cwd, _ = os.Getwd()
		}

		agentBeadID, _ := resolveAgentID("")
		if agentBeadID == "" {
			agentBeadID = resolveAgentByActor(cmd.Context(), actor)
		}

		req := beadsapi.EmitHookRequest{
			AgentBeadID:     agentBeadID,
			HookType:        "Stop",
			ClaudeSessionID: claudeSessionID,
			SessionID:       claudeSessionID,
			Harness:         format,
			CWD:             cwd,
			Actor:           actor,
		}
		resp, err := emitHookWithRetry(cmd.Context(), req)
		if err != nil {
			// Expired AVA session: don't block the Stop or spew Cloudflare HTML.
			// Print one clean line and fail open — the gate can't run without the
			// daemon, and a lapsed cookie shouldn't wedge the user's session.
			if errors.Is(err, avareauth.ErrReauthRequired) {
				fmt.Fprintln(os.Stderr, "gb hook stop-gate: login session expired — run `gb login` to re-enable hook sync (not blocking stop)")
				publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "reauth_required")
				return nil
			}
			fmt.Fprintf(os.Stderr, "gb hook stop-gate: server error after retries: %v\n", err)
			publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "server_error")
			os.Exit(1)
		}

		for _, w := range resp.Warnings {
			out.print(fmt.Sprintf("<system-reminder>%s</system-reminder>\n", w))
		}
		if resp.Inject != "" {
			out.print(resp.Inject)
		}

		if resp.Block {
			blockCount := readStopGateBlockCount() + 1
			recordStopGateBlock()
			injectStopGateTextTo(out.print)
			if hint := stopGateWaitHint(cmd.Context(), agentBeadID, blockCount); hint != "" {
				out.print(hint)
			}
			// After yield, inject stronger guidance to continue working.
			if blockCount > 1 {
				out.print("<system-reminder>You have been blocked " +
					fmt.Sprintf("%d time(s). ", blockCount) +
					"If your decision is still open and `gb yield` has not returned yet, just reply with one short line " +
					"(e.g. `Holding for decision <id>.`) and take no other action — do NOT cancel or re-create the decision, " +
					"and do NOT call `gb stop`; the yield returns on its own when the operator answers. " +
					"Once `gb yield` returns, CONTINUE WORKING on the decision outcome and only call `gb done` when all work is truly complete.</system-reminder>\n")
			}
			publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionBlock, resp.Reason)
			out.block(resp.Reason)
		}

		// Check wrap-up completeness if agent has stop_requested=true.
		if agentBeadID != "" {
			if blocked, reason := checkWrapUpGate(cmd.Context(), agentBeadID); blocked {
				recordStopGateBlock()
				publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionBlock, reason)
				out.block(reason)
			}
		}

		// Gate allowed — clear cooldown.
		clearStopGateCooldown()
		publishStopGateDecision(cmd.Context(), stdinEvent, stopGateDecisionAllow, "allowed")
		return nil
	},
}

// checkWrapUpGate checks whether the agent has provided a required wrap-up
// message. Returns (blocked, reason) — blocked=true means the stop should
// be prevented.
//
// Only blocks if ALL of:
//  1. The agent bead has stop_requested=true (agent is trying to stop)
//  2. The wrapup-config enforce level is "hard"
//  3. The agent bead has no "wrapup" field
func checkWrapUpGate(ctx context.Context, agentBeadID string) (bool, string) {
	bead, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil {
		return false, "" // can't check — don't block
	}

	// Only enforce during stop.
	if bead.Fields["stop_requested"] != "true" {
		return false, ""
	}

	// Check if wrap-up already provided.
	if bead.Fields[WrapUpFieldName] != "" {
		return false, "" // wrap-up present — no block
	}

	// Load requirements.
	reqs := LoadWrapUpRequirements(ctx, daemon, agentBeadID)
	if reqs.Enforce != "hard" {
		return false, "" // not hard enforcement — don't block
	}

	reason := "Wrap-up message required before stopping. Use:\n" +
		"  gb stop --wrapup '{\"tldr\":\"<one-line outcome>\",\"accomplishments\":\"...\"}'\n" +
		"Required fields: " + strings.Join(reqs.Required, ", ")
	return true, reason
}

// stopGateWaitHint names the right action for an agent the gate just blocked
// while it waits on CI (gd-F91YfKwYXl). Background work the CLI runs never
// reaches a block (the background-work pass allows it), so what is left is CI:
// with a pending `gb wait ci` on the agent's own bead the answer is `gb stop`,
// which parks the task and wakes the agent when CI concludes; without one, the
// first block says how to park instead of polling. Later blocks repeat only
// the pending-wait hint.
func stopGateWaitHint(ctx context.Context, agentBeadID string, blockCount int) string {
	if agentBeadID != "" && daemon != nil {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		bead, err := daemon.GetBead(qctx, agentBeadID)
		cancel()
		if err == nil && bead != nil && beadsapi.CIWaitFromFields(bead.Fields).Pending() {
			return "<system-reminder>Your `gb wait ci` is recorded and still pending. Run `gb stop --wrapup '...'` now: " +
				"the task stays parked and you are woken when CI concludes. Do not poll CI in the foreground.</system-reminder>\n"
		}
	}
	if blockCount > 1 {
		return ""
	}
	return "<system-reminder>Waiting on CI? Push, run `gb wait ci --repo OWNER/REPO --sha \"$(git rev-parse HEAD)\"`, then `gb stop`. " +
		"Do not hold the turn with a foreground `gh run watch` or until-loop.</system-reminder>\n"
}

// stopGateAgentParkedOnOpenDecision reports whether this agent is genuinely
// parked on an OPEN human decision it requested -- the condition under which a
// quiet turn-end (with the yield marker present) is the correct resting state
// rather than an involuntary death. It resolves the agent bead the same way
// the Stop-hook body does (KD_AGENT_ID -> actor lookup), then scans open
// decision beads for one whose requesting_agent_bead_id is this agent. It is
// deliberately conservative: any resolution/daemon failure returns false so
// the gate falls through to its normal close path -- a false negative merely
// reverts to the pre-fix behavior, whereas a false positive could keep a pod
// alive forever. Best-effort, short-timeout, never blocks the hook.
func stopGateAgentParkedOnOpenDecision(ctx context.Context) bool {
	agentBeadID, _ := resolveAgentID("")
	if agentBeadID == "" {
		agentBeadID = resolveAgentByActor(ctx, actor)
	}
	if agentBeadID == "" {
		return false
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := daemon.ListBeadsFiltered(qctx, beadsapi.ListBeadsQuery{
		Statuses: []string{"open"},
		Types:    []string{"decision"},
		Sort:     "-created_at",
		Limit:    200,
	})
	if err != nil {
		return false
	}
	for _, b := range result.Beads {
		if b.Fields["requesting_agent_bead_id"] == agentBeadID {
			return true
		}
	}
	return false
}

// stopGateCooldownSparesAgent is the CLAIM GUARD decision itself: given that
// the stop-gate has already determined it is inside a cooldown window, it
// reports whether the agent must be SPARED (exit 0, bead left open) rather than
// closed. It returns the claimed task ID that justified sparing, for the log
// line and for tests.
//
// This is deliberately a named function rather than an inline condition: the
// branch it guards is the lethal one (closeAgentBeadOnExit stops the reconciler
// from keeping the pod alive), so the kill/spare choice needs to be observable
// by a regression test without executing os.Exit. A test that only exercised
// the underlying claim lookup would pass against the pre-fix code and prove
// nothing (ka-0000006454).
//
// spare == true iff the agent still holds a claimed in-progress task OR its
// pod has a live, non-shell child process still running -- a backgrounded
// build or test is stronger evidence of work in flight than a claimed task,
// and it catches the population the claim guard alone cannot: an agent that
// backgrounded a long-running command and then ended its turn without a
// `gb done`/`gb yield` checkpoint and without a claimed task at all
// (ka-0000008xxx: gd-o0dnX8BWXV, `make test-k8s-native-live` still running
// when the pod was deleted mid-cooldown).
//
// The claimed-task check runs first, so on that path the return value is
// exactly the task ID -- pinned by TestBacktest_kd_golM9uBrlq_* and
// TestStopGateClaimedTask_* against the pre-existing claim-guard contract.
// The live-child path returns a "pid N (cmd)" descriptor instead, since
// there is no task ID to name.
func stopGateCooldownSparesAgent(ctx context.Context) (string, bool) {
	if task := stopGateClaimedTask(ctx); task != "" {
		return task, true
	}
	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		return fmt.Sprintf("pid %d (%s)", pid, cmd), true
	}
	return "", false
}

// stopGateClaimedTask returns the ID of the in-progress issue-kind task this
// agent still holds, or "" if it holds none (or the claim cannot be
// established). Holding one is the claim-guard's evidence that real work is
// still in flight, so the cooldown fast-path must not close the bead
// (ka-0000006454, decision ka-0000005194).
//
// It is the same one-GET lookup `gb stop` uses to REFUSE a despawn while a
// claim is held (claimedWorkGate, stop.go) and that closeAgentBeadOnExit uses
// to reopen the orphan (reopenOrphanedClaimedTask) — deliberately, so the
// predicate that spares the agent and the predicate that would have reopened
// its task can never disagree about whether work was in flight.
//
// Fails CLOSED (returns "") on any resolution/daemon error, matching
// stopGateAgentParkedOnOpenDecision: a blip reverts to the pre-fix close rather
// than keeping a pod alive on an unverified claim. Short-timeout, never blocks
// the hook.
func stopGateClaimedTask(ctx context.Context) string {
	if daemon == nil || actor == "" || actor == "unknown" {
		return ""
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	task, err := daemon.ListAssignedTask(qctx, actor)
	if err != nil || task == nil {
		return ""
	}
	// ListAssignedTask already scopes to status=in_progress + kind=issue for
	// this assignee; re-check defensively so a closed/done bead never spares a
	// genuinely-finished agent (mirrors reopenOrphanedClaimedTask).
	if task.Status != "in_progress" {
		return ""
	}
	return task.ID
}

// ── Stop-gate debouncing helpers ─────────────────────────────────────────

// stopGateInCooldown returns true if the stop-gate blocked within the
// cooldown window and the text should NOT be re-injected.
// Uses exponential backoff: base * 2^(blockCount-1), capped at 5 minutes.
//
// STOP_GATE_COOLDOWN_SECS (read here, per pod) is an interim knob for raising
// the base cooldown -- e.g. for a role/project whose ordinary turns run long
// enough to trip cooldown-family closes even with the claim/live-child guards
// below. It is a plain container env var, so it can be raised without a code
// change via the existing project/role bead `env`/`env_json` field (parsed in
// beadsapi/projects.go and beadsapi/roles.go, merged onto the pod spec in
// cmd/controller/podspec.go, precedence global < project < role < agent):
//
//	kd update <project-or-role-bead> -f 'env_json={"STOP_GATE_COOLDOWN_SECS":"60"}'
func stopGateInCooldown() bool {
	data, err := os.ReadFile(stopGateCooldownFile)
	if err != nil {
		return false
	}
	var lastBlock int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &lastBlock); err != nil {
		return false
	}
	baseCooldown := stopGateDefaultCooldownSecs
	if envVal := os.Getenv("STOP_GATE_COOLDOWN_SECS"); envVal != "" {
		if _, err := fmt.Sscanf(envVal, "%d", &baseCooldown); err != nil {
			baseCooldown = stopGateDefaultCooldownSecs
		}
	}

	// Exponential backoff: base * 2^(blockCount-1), capped at 5 min. Computed by
	// the shared deathreport.CooldownWindowSecs so this live gate and the
	// death-report cadence diff use identical math (ka-0000001381).
	blockCount := readStopGateBlockCount()
	cooldown := deathreport.CooldownWindowSecs(blockCount, baseCooldown)

	return time.Now().Unix()-lastBlock < int64(cooldown)
}

// stopGateEscapeHatch returns true if the block count has exceeded the
// maximum, allowing the stop as a safety valve against infinite cost loops.
func stopGateEscapeHatch() bool {
	return readStopGateBlockCount() >= stopGateMaxBlocks
}

// recordStopGateBlock writes the current timestamp and increments the block counter.
func recordStopGateBlock() {
	_ = os.WriteFile(stopGateCooldownFile, []byte(fmt.Sprintf("%d\n", time.Now().Unix())), 0o644)
	count := readStopGateBlockCount() + 1
	_ = os.WriteFile(stopGateBlockCountFile, []byte(fmt.Sprintf("%d\n", count)), 0o644)
}

// clearStopGateCooldown removes the cooldown and block count files.
func clearStopGateCooldown() {
	_ = os.Remove(stopGateCooldownFile)
	_ = os.Remove(stopGateBlockCountFile)
}

// resetStopGateSessionState wipes ALL per-session stop-gate scratch files so a
// freshly-launched (or cold-resumed) Claude session starts from a clean slate.
//
// ka-0000007830: the cooldown timestamp, the consecutive-block counter, and the
// yield marker all live in /tmp, which is a POD-SCOPED EmptyDir — it survives
// coop/claude process restarts inside gb init's restart loop (only a full pod
// replacement wipes it). These files are otherwise cleared only on the
// gate-allowed / escape-hatch / rate-limited paths (and `gb ready`). When a
// session dies WITHOUT reaching one of those paths — a compaction crash, a
// rollout/drain SIGTERM, or a credential-rotation exit(10) — the counters stay
// dirty. gb init then relaunches coop and resumes the session, which inherits
// the stale block count and can be force-exited on its VERY FIRST turn-end via
// stopGateEscapeHatch() (count>=stopGateMaxBlocks persists indefinitely) or
// stopGateInCooldown() — closing the agent bead as `stop_gate_*_involuntary`
// before it does any work of its own. Resetting at each session launch turns
// that cross-session leak into a clean per-session lifecycle.
//
// Best-effort: os.Remove of a non-existent file is a no-op, and a removal
// failure only means the next Stop hook re-reads slightly stale state — never a
// hard failure, so errors are swallowed. Called from gb init's restart loop.
func resetStopGateSessionState() {
	_ = os.Remove(stopGateCooldownFile)
	_ = os.Remove(stopGateBlockCountFile)
	_ = os.Remove(stopGateYieldMarkerFile)
}

// readStopGateBlockCount returns the current session block count.
func readStopGateBlockCount() int {
	data, err := os.ReadFile(stopGateBlockCountFile)
	if err != nil {
		return 0
	}
	var count int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &count); err != nil {
		return 0
	}
	return count
}

// isAgentRateLimited checks the local coop API to see if the agent is rate-limited.
func isAgentRateLimited() bool {
	coopURL := os.Getenv("COOP_URL")
	if coopURL == "" {
		coopURL = "http://localhost:8080"
	}
	agentURL := strings.TrimRight(coopURL, "/") + "/api/v1/agent"

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(agentURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var state map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return false
	}
	cat, _ := state["error_category"].(string)
	return cat == "rate_limited"
}

// injectStopGateTextTo hands the stop-gate checkpoint text to print. It reads
// the config-bead-materialized file written by gb setup claude; the Codex and
// Gemini formats fold the text into their JSON reason.
func injectStopGateTextTo(print func(string)) {
	stopGateTextFile := filepath.Join(os.Getenv("HOME"), ".claude", "stop-gate-text.md")
	if data, err := os.ReadFile(stopGateTextFile); err == nil {
		print(string(data))
		return
	}
	print("<system-reminder>STOP BLOCKED — decision gate unsatisfied. Create a decision checkpoint (gb decision create + gb yield) or call gb done if work is complete.</system-reminder>")
}
