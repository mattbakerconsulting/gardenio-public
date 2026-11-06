package main

// Startup-prompt injection for `gb agent start --local --test-scenario=...`.
//
// Mirrors the K8s `runSessionLifecycle` (init_agent_loop.go) shape so local
// E2E scenarios are driven through their first phase the same way K8s
// agents are nudged after `bypassStartup`.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/coopapi"
)

// startupPromptSidecarPath returns the path to the sidecar
// `<scenario>.startup-prompt.txt` next to a claudeless scenario TOML. The
// prompt lives outside the TOML because claudeless rejects unknown top-level
// keys, so an inline `[test_scenario]` block would break the scenario when
// parsed by `claudeless --scenario`.
func startupPromptSidecarPath(scenarioPath string) string {
	if scenarioPath == "" {
		return ""
	}
	base := strings.TrimSuffix(scenarioPath, ".toml")
	return base + ".startup-prompt.txt"
}

// nudgesSidecarPath returns the path to the sidecar `<scenario>.nudges.txt`
// — one follow-up prompt per non-blank, non-comment line. After the startup
// prompt drives the scenario's first phase, the runner walks this file
// injecting each line as a separate `coop.Nudge` (with idle-poll between)
// so multi-phase scenarios reach the same terminal state as K8s multi-turn
// flows. Resolves ka-0000002034 / decision ka-0000006129 (chosen=multi-nudge).
func nudgesSidecarPath(scenarioPath string) string {
	if scenarioPath == "" {
		return ""
	}
	base := strings.TrimSuffix(scenarioPath, ".toml")
	return base + ".nudges.txt"
}

// resolveNudges reads the sidecar `<scenarioPath>.nudges.txt` and returns
// follow-up nudges to send after the startup prompt. Blank lines and `#`
// comment lines are skipped. Returns nil when no scenario was passed, the
// sidecar is missing, or no non-blank lines remain — callers skip the
// follow-up loop on nil.
func resolveNudges(scenarioPath string) []string {
	if scenarioPath == "" {
		return nil
	}
	sidecar := nudgesSidecarPath(scenarioPath)
	data, err := os.ReadFile(sidecar)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not read %s: %v\n", sidecar, err)
		}
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveStartupPrompt reads the sidecar `<scenarioPath>.startup-prompt.txt`
// and returns its contents trimmed of trailing whitespace. Returns "" when
// no scenario was passed, the sidecar is missing, or the file is empty —
// callers skip injection on empty.
func resolveStartupPrompt(scenarioPath string) string {
	if scenarioPath == "" {
		return ""
	}
	sidecar := startupPromptSidecarPath(scenarioPath)
	data, err := os.ReadFile(sidecar)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("[gb agent start] No %s — skipping startup-prompt injection\n", sidecar)
		} else {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not read %s: %v\n", sidecar, err)
		}
		return ""
	}
	prompt := strings.TrimRight(string(data), " \t\r\n")
	if prompt == "" {
		fmt.Printf("[gb agent start] %s is empty — skipping startup-prompt injection\n", sidecar)
	}
	return prompt
}

// bypassLocalStartup mirrors the K8s init_agent bypassStartup but for the
// local (`gb agent start --local`) path. claudeless 0.4.0 renders a
// "Bypass Permissions mode" setup dialog on boot that coop's groom does not
// auto-dismiss (subtype "bypass_permissions" is not in its disruption
// whitelist), so without active intervention the agent state stalls at
// `prompt` forever: `injectLocalStartupPrompt` times out, and coop refuses
// every mail nudge "agent is prompt" (gd-inbwLZfEYe). runLocal runs it for a
// --command session too, which has no startup prompt to inject.
//
// We poll coop state for up to ~2 minutes. If we observe a setup prompt
// with subtype `bypass_permissions`, we send option 2 ("Yes, I accept")
// via SendKeys. Other setup/permission subtypes are left for coop groom
// to handle (which it does correctly). Returns once state reaches
// idle/working, or on timeout (best-effort; caller still attempts the
// nudge and surfaces its own warning).
// bypassLocalPollInterval is the poll interval used by bypassLocalStartup
// between coop state checks. Test code overrides this to speed up runs.
var bypassLocalPollInterval = 2 * time.Second

func bypassLocalStartup(ctx context.Context, coop *coopapi.Client) {
	dismissed := false
	for range 60 {
		select {
		case <-ctx.Done():
			return
		default:
		}

		state, err := coop.GetAgentState(ctx)
		if err != nil {
			time.Sleep(bypassLocalPollInterval)
			continue
		}

		if state.State == "idle" || state.State == "working" {
			return
		}

		// Actively dismiss the claudeless bypass_permissions setup dialog
		// when we see it — coop groom does not handle this subtype.
		if state.Prompt != nil && state.Prompt.Type == "setup" &&
			state.Prompt.Subtype == "bypass_permissions" && !dismissed {
			fmt.Printf("[gb agent start] Dismissing bypass_permissions setup prompt\n")
			// Select "Yes, I accept" — the second option in the dialog.
			// Down+Return navigates to and confirms the second option,
			// mirroring the resume-picker pattern in K8s bypassStartup.
			if err := coop.SendKeys(ctx, []string{"Down", "Return"}); err != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] warning: failed to dismiss bypass_permissions prompt: %v\n", err)
			} else {
				dismissed = true
			}
			time.Sleep(bypassLocalPollInterval)
			continue
		}

		time.Sleep(bypassLocalPollInterval)
	}
}

// injectLocalStartupPrompt drives the local coop session through the same
// shape as K8s injectPrompt: wait for idle/working, clear stale input with
// Ctrl-U, then deliver the nudge with up to 3 retries.
func injectLocalStartupPrompt(ctx context.Context, coop *coopapi.Client, prompt string) bool {
	bypassLocalStartup(ctx, coop)

	if !waitForReady(ctx, coop) {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: timed out waiting for agent to be ready for startup prompt\n")
		return false
	}

	state, _ := coop.GetAgentState(ctx)
	readyState := ""
	if state != nil {
		readyState = state.State
	}
	fmt.Printf("[gb agent start] Injecting startup prompt (agent state: %s)\n", readyState)
	_ = coop.SendKeys(ctx, []string{"Ctrl-U"})
	time.Sleep(200 * time.Millisecond)

	return deliverNudge(ctx, coop, prompt, "startup prompt")
}

// runLocalNudgeSequence drives the startup prompt and any follow-up nudges
// for a local-mode scenario. Extracted from runLocal so the multi-nudge
// sequencing is testable end-to-end without spinning up the rest of the
// runLocal integration loop.
func runLocalNudgeSequence(ctx context.Context, coop *coopapi.Client, startupPrompt string, followups []string, sidecarPath string) {
	if !injectLocalStartupPrompt(ctx, coop, startupPrompt) {
		return
	}
	if len(followups) > 0 {
		fmt.Printf("[gb agent start] Multi-nudge: %d follow-up prompt(s) queued from %s\n", len(followups), sidecarPath)
		injectLocalFollowupNudges(ctx, coop, followups)
	}
}

// scenarioShutdownGrace is the wall-clock budget for the scenario-mode
// "wait for the last nudge to drain, then ask coop to exit" handshake.
// Variable so tests can shrink it.
var scenarioShutdownGrace = 5 * time.Second

// requestScenarioShutdown finishes the scenario-mode lifecycle: after the
// runLocalNudgeSequence has delivered every nudge the sidecar asks for, we
// wait one more turn-complete signal so the agent's final tool call (e.g.
// `gb stop`, `kd close`) has a chance to settle, then POST /api/v1/shutdown
// to coop. Coop cancels its lifecycle token, the child shell exits, and the
// parent runLocal's coopProc.Wait() unblocks — so `gb agent start --local
// --test-scenario=...` self-exits instead of hanging until the harness
// SIGTERM at 90s (ka-0000003566).
//
// Interactive mode (no scenarioPath) skips this entirely — operators are in
// control of when to detach / exit. We also guard against an already-cancelled
// parent context so a SIGINT mid-scenario doesn't double-trip the shutdown.
func requestScenarioShutdown(ctx context.Context, coop *coopapi.Client) {
	// Wait for the last nudge's working→idle (or Stop hook) so we don't tear
	// coop down while the agent is mid-tool-call. The seq snapshot is taken
	// now — `injectLocalFollowupNudges` does the same after each delivery,
	// so any transition we see here is strictly the final nudge's turn.
	preStateSeq, preHookSeq := observedSeqs(ctx, coop)
	if !waitForTurnComplete(ctx, coop, preStateSeq, preHookSeq) {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: final scenario turn did not complete within budget; requesting coop shutdown anyway\n")
	}

	// Use a fresh, bounded context so a parent-cancel mid-shutdown still
	// gives the POST a chance to land — coop's /api/v1/shutdown just flips
	// a cancellation token, it doesn't block on child wait.
	sctx, scancel := context.WithTimeout(context.Background(), scenarioShutdownGrace)
	defer scancel()
	if err := coop.Shutdown(sctx); err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: coop shutdown after scenario failed: %v\n", err)
		return
	}
	fmt.Printf("[gb agent start] Scenario complete — coop shutdown requested\n")
}

// injectLocalFollowupNudges walks `nudges` and injects each one after the
// previous turn has actually completed. This is the multi-nudge runner from
// decision ka-0000006129 — it lets local-*.toml scenarios advance past
// Phase 1 without requiring the runner to also rewrite each scenario as a
// single-shot blob (option 1) or claudeless to dispatch responses on
// declared order (option 3).
//
// Decision ka-0000008116 (chosen=hard-idle-transition) replaced the prior
// `waitForIdle` (state == "idle") gate with `waitForTurnComplete`, which
// observes coop's event log for a working→idle state transition OR a
// claudeless Stop hook event with seq > the pre-nudge snapshot. The old
// poll was too weak: claudeless may be (or appear) idle continuously when
// non-TTY stdin coalesces input bytes, in which case `state == idle`
// matched immediately and the next nudge raced the model's first turn.
//
// If the per-nudge budget elapses without a transition or Stop hook the
// runner logs a clear diagnostic warning and continues — the scenario's
// own assertions stay authoritative for end-of-run pass/fail.
func injectLocalFollowupNudges(ctx context.Context, coop *coopapi.Client, nudges []string) {
	if len(nudges) == 0 {
		return
	}
	preStateSeq, preHookSeq := observedSeqs(ctx, coop)
	for i, prompt := range nudges {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !waitForTurnComplete(ctx, coop, preStateSeq, preHookSeq) {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: no working→idle transition or Stop hook observed before follow-up nudge %d/%d (claudeless may have coalesced prior input)\n", i+1, len(nudges))
		}
		label := fmt.Sprintf("follow-up nudge %d/%d", i+1, len(nudges))
		if !deliverNudge(ctx, coop, prompt, label) {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: %s undelivered; continuing\n", label)
		}
		// Snapshot seqs AFTER each nudge so the next iteration only
		// counts transitions/Stop hooks that arrive *after* this delivery.
		preStateSeq, preHookSeq = observedSeqs(ctx, coop)
	}
}

// waitForReady polls coop until the agent reaches idle or working state.
// Returns true on success, false on timeout / ctx.
func waitForReady(ctx context.Context, coop *coopapi.Client) bool {
	for range 60 {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		state, err := coop.GetAgentState(ctx)
		if err == nil && (state.State == "idle" || state.State == "working") {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// followupNudgePollInterval / followupNudgeMaxPolls bound the
// per-follow-up turn-complete wait. Variables (not constants) so tests
// can override to keep runtimes fast.
var followupNudgePollInterval = 2 * time.Second
var followupNudgeMaxPolls = 90 // ~180s default, generous for Bash tool turns

// coopTransitionEntry / coopHookEntry mirror coop's TransitionEntry / HookEntry
// (event_log.rs) wire shape — we keep the parse local to this file so
// coopapi.Client can stay a thin transport without exporting coop's
// internal types.
type coopTransitionEntry struct {
	Prev string `json:"prev"`
	Next string `json:"next"`
	Seq  uint64 `json:"seq"`
}

type coopHookEntry struct {
	HookSeq uint64         `json:"hook_seq"`
	JSON    map[string]any `json:"json"`
}

// isStopHook reports whether a coop HookEntry.json payload represents a
// Claude/claudeless Stop hook. Coop stores the raw FIFO line verbatim, which
// for a Stop hook is `{"event":"stop","data":{"hook_event_name":"Stop",...}}`
// (see coop/crates/cli/src/driver/claude/hooks.rs and hook_recv.rs). The
// outer envelope's `event` field is the canonical lower-case marker we control
// end-to-end; we also accept the upstream `hook_event_name` field (either
// nested under `data` or — for older/synthetic payloads — at the top level)
// so this helper stays robust if the envelope ever changes shape.
//
// Resolves ka-0000002926: prior code checked `JSON["hook_event_name"]` at the
// top level only, which never matched the real wire shape — silently breaking
// the Stop-hook gate in waitForTurnComplete.
func isStopHook(j map[string]any) bool {
	if j == nil {
		return false
	}
	if ev, _ := j["event"].(string); ev == "stop" {
		return true
	}
	if name, _ := j["hook_event_name"].(string); name == "Stop" {
		return true
	}
	if data, ok := j["data"].(map[string]any); ok {
		if name, _ := data["hook_event_name"].(string); name == "Stop" {
			return true
		}
	}
	return false
}

// observedSeqs returns the current high-water marks for state events
// (transition seq) and hook events (hook_seq) so a subsequent
// waitForTurnComplete call can ignore events that pre-date the snapshot.
// Best-effort: on transport error returns (0, 0); waitForTurnComplete then
// fires on ANY transition/Stop hook in the log, which is acceptable for a
// post-delivery gate (the worst case is a single false-positive that lets
// the next nudge fire slightly early — still safer than the prior
// state-poll race condition).
func observedSeqs(ctx context.Context, coop *coopapi.Client) (uint64, uint64) {
	ev, err := coop.GetEventsCatchup(ctx)
	if err != nil || ev == nil {
		return 0, 0
	}
	var maxStateSeq, maxHookSeq uint64
	for _, raw := range ev.StateEvents {
		var t coopTransitionEntry
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		if t.Seq > maxStateSeq {
			maxStateSeq = t.Seq
		}
	}
	for _, raw := range ev.HookEvents {
		var h coopHookEntry
		if err := json.Unmarshal(raw, &h); err != nil {
			continue
		}
		if h.HookSeq > maxHookSeq {
			maxHookSeq = h.HookSeq
		}
	}
	return maxStateSeq, maxHookSeq
}

// waitForTurnComplete blocks until the agent has demonstrably finished
// processing a prior nudge by emitting one of:
//
//   - a state transition with prev="working" and next="idle" and
//     seq > preStateSeq, OR
//   - a Stop hook event (matched via isStopHook) with hook_seq > preHookSeq.
//
// Returns true on first matching signal; false on context cancel or after
// followupNudgeMaxPolls * followupNudgePollInterval elapses without a
// signal. This is the hard-idle-transition gate from decision
// ka-0000008116 — it replaces the prior `state == "idle"` poll which
// matched stale-idle and let follow-up nudges race claudeless's first
// turn (ka-0000002034).
func waitForTurnComplete(ctx context.Context, coop *coopapi.Client, preStateSeq, preHookSeq uint64) bool {
	for range followupNudgeMaxPolls {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		ev, err := coop.GetEventsCatchupSince(ctx, preStateSeq, preHookSeq)
		if err == nil && ev != nil {
			for _, raw := range ev.StateEvents {
				var t coopTransitionEntry
				if err := json.Unmarshal(raw, &t); err != nil {
					continue
				}
				if t.Seq > preStateSeq && t.Prev == "working" && t.Next == "idle" {
					return true
				}
			}
			for _, raw := range ev.HookEvents {
				var h coopHookEntry
				if err := json.Unmarshal(raw, &h); err != nil {
					continue
				}
				if h.HookSeq <= preHookSeq {
					continue
				}
				if isStopHook(h.JSON) {
					return true
				}
			}
		}
		time.Sleep(followupNudgePollInterval)
	}
	return false
}

// deliverNudge sends `prompt` with up to 3 retries, mirroring the
// startup-prompt delivery shape. Returns true on Delivered=true response.
func deliverNudge(ctx context.Context, coop *coopapi.Client, prompt, label string) bool {
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := coop.Nudge(ctx, prompt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: %s nudge attempt %d/%d failed: %v\n", label, attempt, maxAttempts, err)
			time.Sleep(2 * time.Second)
			continue
		}
		if resp.Delivered {
			fmt.Printf("[gb agent start] %s delivered (attempt %d)\n", label, attempt)
			return true
		}
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: %s nudge attempt %d/%d not delivered: %s\n", label, attempt, maxAttempts, resp.Reason)
		time.Sleep(2 * time.Second)
	}
	fmt.Fprintf(os.Stderr, "[gb agent start] warning: all %d %s nudge attempts failed\n", maxAttempts, label)
	return false
}
