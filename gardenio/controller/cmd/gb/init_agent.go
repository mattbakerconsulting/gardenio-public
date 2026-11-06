package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gardenio/controller/internal/coopapi"
	"gardenio/controller/internal/harness"

	"github.com/spf13/cobra"
)

// errStartupLoginBanner is the sentinel bypassStartup returns when it detects a
// login-required banner at startup (credentials missing/invalid). The session
// lifecycle uses it to classify a credential-not-ready death so the supervisor
// loop can maintain a distinct startup-login failure budget and close the bead
// not_logged_in, instead of silently burning every restart (ka-0000007536).
var errStartupLoginBanner = errors.New("login prompt detected: credentials may be invalid")

var initAgentCmd = &cobra.Command{
	Use:   "init",
	Short: "Agent pod lifecycle manager",
	Long: `Owns the full agent pod lifecycle: starts coop, manages the restart loop,
tracks sessions as beads, and runs per-session lifecycle goroutines.

Responsibilities:
  - Start and restart coop + Claude Code
  - Resume previous sessions (with session-env validation)
  - Register and close session beads (per-session cost tracking)
  - Bypass startup prompts (trust dialog, setup wizard, resume picker)
  - Inject initial work prompt (via nudge-prompt resolution)
  - Monitor agent state (idle/working/exited/rate_limited)
  - Refresh OAuth credentials
  - Handle graceful shutdown signals

Called by entrypoint.sh after one-time setup.`,
	GroupID: "agent",
	RunE:    runInitAgent,
}

var (
	initCoopURL  string
	initStandby  bool
	initMockMode bool
)

func init() { //nolint:gochecknoinits // Cobra requires init() for flag registration
	initAgentCmd.Flags().StringVar(&initCoopURL, "coop-url", "http://localhost:8080", "coop API base URL")
	initAgentCmd.Flags().BoolVar(&initStandby, "standby", false, "enable standby mode (prewarmed pool agents)")
	initAgentCmd.Flags().BoolVar(&initMockMode, "mock", false, "mock mode (skip OAuth refresh)")
}

func runInitAgent(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// Before doing anything, check if the agent bead has stop_requested=true.
	// This breaks the restart loop when Claude has already called gb stop but
	// kubelet restarted the container (exit code 0 + restartPolicy: Always).
	if shouldExitBeforeStart(ctx) {
		logInit("stop_requested is set — exiting")
		return nil
	}

	if _, err := harness.FromEnv(); err != nil {
		return err
	}
	cfg := buildInitConfig(initStandby, initMockMode)
	logInit("starting %s agent: %s (project: %s)", cfg.role, cfg.agent, orStr(cfg.project, "none"))

	// One-time setup: git, SSH, repos, PVC, credentials, hooks.
	if err := runSetup(cfg); err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	// Smoke-test JIRA service-account access. No-op if creds are unset; squawks
	// loudly on 401/403/404 so we don't silently produce blind triage agents.
	probeJIRA(ctx)

	// Resume-chain recovery (ka-0000003160): a crew agent respawned after an
	// involuntary kill mints a NEW bead ID, so keying coop's S3 prefix on that
	// id would probe an empty archive and cold-start — orphaning the prior
	// transcript sitting under the ORIGINAL bead ID. resolveCanonicalS3Key
	// follows the resume_s3_key breadcrumb on the (stable) worked task bead back
	// to the canonical prefix and adopts it for this pod. Runs BEFORE
	// runInitLoop wires COOP_S3_SESSION_KEY (buildCoopEnv) and before the first
	// resume probe. BOAT_TASK_ID is set at pod-spec time for gardener-respawned
	// crew (the documented case); a no-op when no task or no prior key exists.
	resolveCanonicalS3Key(ctx)

	// Controller-driven force-clean context-blowup policy (ka-0000007242): decide
	// ONCE, now, whether the prior life's transcript is too big to resume (past a
	// turn/token/wake threshold). If so, latch force-clean so buildCoopEnv wires
	// COOP_S3_NO_RESTORE and the restart loop purges local session logs — coop
	// then starts fresh instead of dying on a doomed `coop --resume` at boot.
	// Runs AFTER resolveCanonicalS3Key (which may adopt a canonical key that
	// s3ResumeIntended and agentOwnBeadID rely on) and BEFORE runInitLoop wires
	// the coop env / probes resume.
	resolveForceCleanPolicy(ctx)

	// Signal handling: SIGTERM/SIGINT cancel the context, which propagates
	// to coop via cmd.Cancel (SIGTERM) in the restart loop.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Per-pod aggregator for gardenio_tool_harness_overhead_seconds (gd-RpflV68KX1):
	// hooks only append to a spool; this long-lived process folds + pushes it.
	go runHarnessOverheadFlusher(ctx, harnessOverheadFlushInterval)

	return runInitLoop(ctx, cfg)
}

// bypassStartupForHarness runs bypassStartup whenever coop uses the Claude
// driver (claude pods, and the claudeless mock even on a codex pod) and is a
// no-op for a real codex app-server, which has no TUI prompts to answer.
func bypassStartupForHarness(ctx context.Context, coop *coopapi.Client, h, cmd string, hasResume bool) error {
	if !harness.ClaudeDriver(h, cmd) {
		return nil
	}
	return bypassStartup(ctx, coop, hasResume)
}

// bypassStartup handles startup prompts (trust dialog, setup wizard, resume picker).
// hasResume indicates whether coop was started with --resume; when false and a
// resume picker appears, we select "Start new session" rather than resuming.
func bypassStartup(ctx context.Context, coop *coopapi.Client, hasResume bool) error {
	logInit("Bypassing startup prompts")
	falsePositiveCount := 0
	bypassAccepted := false

	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		time.Sleep(2 * time.Second)

		state, err := coop.GetAgentState(ctx)
		if err != nil {
			continue
		}

		promptType := ""
		promptSubtype := ""
		if state.Prompt != nil {
			promptType = state.Prompt.Type
			promptSubtype = state.Prompt.Subtype
		}

		// Handle interactive prompts while agent is starting.
		if state.State == "starting" {
			if handled, err := handleStartingScreen(ctx, coop, hasResume); err != nil {
				return err
			} else if handled {
				continue
			}
		}

		// Handle "Detected a custom API key" prompt.
		// This prompt is NOT auto-dismissed by coop's groom, so we handle it here.
		if state.State == "starting" || promptType == "permission" {
			screen, err := coop.GetScreenText(ctx)
			if err == nil {
				if strings.Contains(screen, "Detected a custom API key") {
					logInit("Detected API key prompt, selecting 'Yes'")
					_ = coop.SendKeys(ctx, []string{"Up", "Return"})
					time.Sleep(3 * time.Second)
					continue
				}
			}
		}

		// claudeless 0.4.0's "Bypass Permissions mode" dialog: groom leaves
		// this subtype alone, so nothing else answers it and a mock pod never
		// reaches idle (gd-K77LsUtqhK). Down+Return picks "Yes, I accept",
		// as bypassLocalStartup does; the pod already runs with
		// --dangerously-skip-permissions.
		if promptType == "setup" && promptSubtype == "bypass_permissions" && !bypassAccepted {
			logInit("Accepting bypass_permissions setup prompt")
			if err := coop.SendKeys(ctx, []string{"Down", "Return"}); err != nil {
				logInit("warning: failed to accept bypass_permissions prompt: %v", err)
			} else {
				bypassAccepted = true
			}
			continue
		}

		// Trust and setup prompts are handled by coop's groom=auto (default).
		// Do NOT call Respond() here — it races with groom's auto-dismiss,
		// causing a duplicate keystroke ("1") that bleeds into the next screen
		// and becomes the agent's first user message instead of the nudge.
		if promptType == "permission" || promptType == "setup" {
			logInit("Waiting for coop groom to auto-dismiss %s prompt (subtype: %s)", promptType, promptSubtype)
			falsePositiveCount++
			if falsePositiveCount >= 15 {
				logInit("WARNING: prompt not auto-dismissed after %d checks (subtype: %s)", falsePositiveCount, promptSubtype)
				return nil
			}
			continue
		}

		// If agent is past setup prompts, we're done.
		if state.State == "idle" || state.State == "working" {
			logInit("Startup bypass complete (agent state: %s)", state.State)
			return nil
		}
	}

	logInit("WARNING: auto-bypass timed out after 120s")
	return nil
}

// handleStartingScreen answers the screens a "starting" agent can show
// before coop's groom takes over: a failed resume, a login banner, or the
// resume picker. handled means a key was sent and the caller should poll
// again; an error ends startup.
func handleStartingScreen(ctx context.Context, coop *coopapi.Client, hasResume bool) (handled bool, err error) {
	screen, err := coop.GetScreenText(ctx)
	if err != nil {
		return false, nil
	}
	// Resume failed: pre-flight checks passed but Claude Code
	// couldn't load the session (corrupt state, version mismatch,
	// etc.). Request coop shutdown so the entrypoint retires the
	// stale session and restarts fresh.
	if strings.Contains(screen, "No conversation found") {
		logInit("Resume failed: No conversation found — requesting shutdown")
		_ = coop.Shutdown(ctx)
		return false, fmt.Errorf("resume failed: No conversation found")
	}

	// Login prompt: credentials are missing or invalid. This
	// can happen after a credential switch if the new token is
	// expired or malformed. Shut down immediately instead of
	// burning 120s waiting for a prompt that won't auto-dismiss.
	// Uses the shared loginBannerPhrases set so this early-startup
	// guard and isSessionAlive's credential-ready gate recognize
	// the same banner (ka-0000007394).
	if screenHasAnyPhrase(screen, loginBannerPhrases) {
		logInit("Login prompt detected — credentials may be invalid, requesting shutdown")
		_ = coop.Shutdown(ctx)
		return false, errStartupLoginBanner
	}

	// "Resume Session" picker.
	if containsCI(screen, "resume") && containsCI(screen, "session") {
		if hasResume {
			// We passed --resume: press Enter to confirm.
			logInit("Detected resume session picker, selecting resume")
			_ = coop.SendKeys(ctx, []string{"Return"})
		} else {
			// No resume expected (no S3 bucket or no agent bead key):
			// a stale session survived in ~/.claude. Navigate down
			// to "Start new session" and confirm.
			logInit("Detected resume session picker (no resume expected) — selecting new session")
			_ = coop.SendKeys(ctx, []string{"Down", "Return"})
		}
		time.Sleep(3 * time.Second)
		return true, nil
	}
	return false, nil
}

// recoverFromErrorAndNudge drives an agent that may be wedged on an error
// screen back into a working turn, then re-delivers the work prompt.
//
// WHY THIS EXISTS (ka-0000006423): a single 429 leaves coop's authoritative
// (log-tier) state at "error". That state is sticky — the low-confidence
// screen-parse tier is not allowed to downgrade "error" (priority 2) to "idle"
// (priority 1), so it only clears when a NEW turn supersedes it. The old
// post-backoff recovery called injectPrompt, which (a) waits up to 120s for
// idle/working — a wait that ALWAYS times out while the state is stuck at
// "error" — and even if it didn't, (b) coop.Nudge refuses to deliver in the
// "error" state (handle_nudge returns delivered=false for any non
// idle/working/prompt state). The nudge was silently dropped and the agent
// oscillated rate_limited<->idle forever, never making another API call.
//
// The recovery mirrors the manually-verified sequence from the bug report:
//
//	Return   — dismiss the error screen
//	Ctrl-U   — clear any stale input buffer
//	<prompt> — type the work prompt via raw PTY input (POST /api/v1/input)
//	Enter    — SUBMIT (text alone sits unsubmitted; append via enter=true)
//
// SendInput writes raw bytes to the PTY, bypassing the nudge state-gate that
// blocks Nudge in the "error" state — this is the whole point. Typing a prompt
// and submitting forces Claude to start a fresh turn, which transitions coop to
// "working" and clears the sticky error. We confirm that turn actually started
// (confirmNudgeTurn) and retry the full sequence when it did not, so a
// rate-limit park can finally CONVERGE instead of wedging forever.
func recoverFromErrorAndNudge(ctx context.Context, coop *coopapi.Client) error {
	nudgeMsg := mockSafeNudge(os.Getenv("BOAT_COMMAND"), resolveNudgeMessage())
	role := os.Getenv("BOAT_ROLE")
	logInit("Recovering from error screen and re-injecting work prompt (role: %s)", role)

	// request_count before the re-inject: turnRecovered calls the retry
	// recovered only once a usage-bearing turn advances past it (gd-a0jdFgwngn).
	baseline := noRequestBaseline
	if state, err := coop.GetAgentState(ctx); err == nil {
		baseline = state.RequestCount
	}

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Dismiss the error screen and clear any stale input buffer. A single
		// Return does not always clear the error TUI, so we pair it with Ctrl-U
		// and re-do both on every retry.
		_ = coop.SendKeys(ctx, []string{"Return"})
		time.Sleep(recoveryKeyDelay)
		_ = coop.SendKeys(ctx, []string{"Ctrl-U"})
		time.Sleep(recoveryKeyDelay)

		// Type the work prompt AND submit it (enter=true). Raw PTY input
		// bypasses the nudge state-gate that would otherwise refuse delivery
		// while coop is in the "error" state.
		if err := coop.SendInput(ctx, nudgeMsg, true); err != nil {
			logInit("WARNING: recovery input attempt %d/%d failed: %v", attempt, maxAttempts, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// Confirm the turn actually started, then classify the OUTCOME. A raw
		// write is fire-and-forget; confirmNudgeTurn only proves coop moved off
		// the pre-existing wedge — it returns true for rate_limited /
		// budget_exceeded / parked / exited as well as working, because for the
		// server-error / connectivity paths any of those is a legitimate "the
		// turn ran" signal.
		//
		// But "the turn ran" is NOT "the turn SUCCEEDED". A rate-limit re-inject
		// that immediately 429s again drives coop working → rate_limited within
		// the round-trip, and the old code logged "Error-screen recovery
		// succeeded — agent working again" on that doomed retry. That single line
		// is what kept the health probe green through the 11-14h livelock
		// (ka-0000007458, ka-0000001209): a human/probe scanning logs saw
		// "working again" once per park and concluded the agent had recovered.
		// Re-read the state and log HONESTLY — only claim recovery when the turn
		// did not land straight back in an error/limit state.
		if confirmNudgeTurn(ctx, coop, baseline) {
			recovered, cat := turnRecovered(ctx, coop, baseline)
			switch {
			case recovered:
				logInit("Error-screen recovery succeeded — agent working again (attempt %d)", attempt)
			case cat == turnOutcomeInFlight:
				logInit("Error-screen recovery re-injected; turn still in flight after %s with no completed request — outcome unconfirmed (attempt %d)", turnOutcomeTimeout, attempt)
			default:
				logInit("Error-screen recovery re-injected but the turn landed back in %q — NOT recovered (attempt %d); a sustained streak is bounded by the rate-limit livelock guard", cat, attempt)
			}
			return nil
		}

		logInit("WARNING: recovery attempt %d/%d delivered but agent never started a turn — retrying", attempt, maxAttempts)
	}

	logInit("WARNING: error-screen recovery exhausted %d attempts without a work turn", maxAttempts)
	return nil
}

// noRequestBaseline is the request_count baseline used when none could be
// read: no count exceeds it, so request_count never stands in as turn proof.
const noRequestBaseline = ^uint64(0)

// injectPrompt resolves the nudge prompt and injects it into the agent. It
// returns coop's request_count from just before the prompt was delivered
// (noRequestBaseline if the agent never became ready), so monitorExit can
// count a turn that landed during injection as proof the agent worked.
func injectPrompt(ctx context.Context, coop *coopapi.Client) (uint64, error) {
	// Wait for agent to reach a nudge-ready state (idle or working).
	//
	// After startup prompt bypass, the agent may briefly transition to
	// "working" as SessionStart hooks fire (e.g., gb hook prime). Coop
	// accepts nudges in both idle and working states — the agent picks up
	// the message when the current generation finishes. We must NOT skip
	// on "working" or the initial nudge is lost.
	var readyState string
	// baselineRequests is coop's request_count when the agent was found ready;
	// a genuine turn consuming the prompt advances it (see confirmPromptConsumed).
	baselineRequests := noRequestBaseline
	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return noRequestBaseline, ctx.Err()
		default:
		}

		state, err := coop.GetAgentState(ctx)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		if state.State == "idle" || state.State == "working" {
			readyState = state.State
			baselineRequests = state.RequestCount
			break
		}

		time.Sleep(2 * time.Second)
	}

	if readyState == "" {
		logInit("WARNING: timed out waiting for agent to be ready for nudge")
		return noRequestBaseline, nil
	}

	// Resolve nudge prompt.
	nudgeMsg := mockSafeNudge(os.Getenv("BOAT_COMMAND"), resolveNudgeMessage())

	role := os.Getenv("BOAT_ROLE")
	logInit("Injecting initial work prompt (role: %s, agent state: %s)", role, readyState)

	// Clear any stale input that may have leaked during startup bypass.
	// The trust-folder Respond(1) can leave a "1" in the input field if
	// the prompt auto-dismissed before the byte arrived. Ctrl-U (kill-line)
	// safely clears the input buffer without side effects.
	_ = coop.SendKeys(ctx, []string{"Ctrl-U"})
	time.Sleep(200 * time.Millisecond)

	// Deliver with retry. Coop accepts nudges during both idle and working
	// states, but transient errors or brief state transitions may cause a
	// single attempt to fail.
	//
	// CRITICAL (ka-0000002714): coop.Nudge returns Delivered=true the instant
	// the message bytes are QUEUED for detached PTY delivery — NOT when Claude
	// actually starts a turn (see coop handle_nudge in
	// coop/crates/cli/src/transport/handler.rs). Coop also reports state="idle"
	// during the startup window — after the pod goes Running but before
	// Claude's first hook fires (documented in statusreporter, ka-0000000427).
	// A nudge fired into that startup-idle window is accepted (Delivered=true)
	// but the bytes land before the TUI is live and are silently dropped; coop's
	// only safety net re-sends a trailing Enter (\r), never the message text.
	// The agent then sits idle forever and is reaped at the stale timeout as
	// 'stale: unknown state "idle"', never executing its prompt — exactly the
	// trigger-spawned-gardener regression this guards against.
	//
	// So Delivered=true is necessary but NOT sufficient. When we nudged an idle
	// agent, confirm the work turn actually STARTED (a transition to "working")
	// before declaring success. If it never does, the nudge was lost — re-send
	// the full message (coop's own retry only re-sends \r).
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := coop.Nudge(ctx, nudgeMsg)
		if err != nil {
			lastErr = err
			logInit("WARNING: nudge attempt %d/%d failed: %v", attempt, maxAttempts, err)
			time.Sleep(2 * time.Second)
			continue
		}

		if !resp.Delivered {
			lastErr = fmt.Errorf("not delivered: %s", resp.Reason)
			logInit("WARNING: nudge attempt %d/%d not delivered: %s", attempt, maxAttempts, resp.Reason)
			time.Sleep(2 * time.Second)
			continue
		}

		// Bytes queued to an agent coop already reports as "working". That
		// state is NOT proof the prompt will be consumed (gd-udek4CJVRY): after
		// a coop restart with --resume, coop's log/process tiers pin the state
		// at "working" (the resumed transcript ends mid-tool-call and the TUI
		// repaint counts as activity) while Claude Code is actually idle at its
		// input line showing "Interrupted". The nudge text then lands as a
		// bracketed paste whose trailing Enter is swallowed, coop skips its
		// own Enter safety net because state_before was "working", and the
		// agent sits on "❯ [Pasted text #1 +2 lines]" forever — 8h46m in the
		// incident — with every watchdog trusting "working". So demand proof
		// that a genuine turn landed (request_count advanced), and when the
		// screen shows the prompt sitting unsubmitted, press Enter before
		// falling back to a full re-send.
		if readyState == "working" {
			if confirmPromptConsumed(ctx, coop, baselineRequests) {
				logInit("Initial prompt delivered and a turn landed (attempt %d, agent was already working)", attempt)
				return baselineRequests, nil
			}
			if screenShowsUnsentInput(ctx, coop) {
				logInit("prompt is sitting unsubmitted in the input line after %s — pressing Enter (attempt %d)", promptConsumeTimeout, attempt)
				_ = coop.SendKeys(ctx, []string{"Return"})
				if confirmPromptConsumed(ctx, coop, baselineRequests) {
					logInit("Initial prompt submitted with a trailing Enter and a turn landed (attempt %d)", attempt)
					return baselineRequests, nil
				}
			}
			lastErr = fmt.Errorf("delivered to a working agent but no turn landed within %s", promptConsumeTimeout)
			logInit("WARNING: nudge attempt %d/%d delivered to a working agent but no turn landed — re-sending", attempt, maxAttempts)
			_ = coop.SendKeys(ctx, []string{"Ctrl-U"})
			time.Sleep(200 * time.Millisecond)
			continue
		}

		// Idle delivery: verify the turn actually started before trusting it.
		if confirmNudgeTurn(ctx, coop, baselineRequests) {
			logInit("Initial prompt delivered and turn started (attempt %d)", attempt)
			return baselineRequests, nil
		}

		lastErr = fmt.Errorf("delivered but no turn started (likely lost in startup-idle window)")
		logInit("WARNING: nudge attempt %d/%d delivered but agent never started a turn — re-sending", attempt, maxAttempts)
		// Re-clear any stale input before the re-send (state may have drifted).
		_ = coop.SendKeys(ctx, []string{"Ctrl-U"})
		time.Sleep(200 * time.Millisecond)
	}

	logInit("WARNING: all %d nudge attempts failed: %v", maxAttempts, lastErr)
	return baselineRequests, nil
}

// promptConsumeTimeout bounds how long injectPrompt waits, after delivering the
// work prompt to an agent coop already reports as "working", for proof that a
// genuine turn consumed it. Three minutes comfortably covers a fresh spawn's
// SessionStart hooks (gb hook prime) plus the first model round-trip; a resume
// whose paste lost its Enter never produces a turn, so it falls through to the
// Enter / re-send recovery. Overridable for tests.
var promptConsumeTimeout = 3 * time.Minute

// promptConsumePoll is confirmPromptConsumed's state poll interval.
// Overridable for tests.
var promptConsumePoll = 2 * time.Second

// confirmPromptConsumed reports whether, within promptConsumeTimeout, the
// agent gave proof that the just-delivered prompt was consumed. The proof is
// coop's request_count advancing past baseline: it moves only when a turn
// produces a usage-bearing assistant message, so it is immune to the pinned
// "working" a --resume leaves behind (gd-udek4CJVRY) and to the transient
// "working" flip of a doomed rate-limit retry (ka-0000001209). A state that
// means the session engaged or is gone (exited / parked / rate_limited /
// budget_exceeded / error / prompt) also counts: a re-send cannot help there
// and monitorExit owns those paths. "working" with a flat request_count and
// "idle" both keep waiting — idle is exactly what the lost-Enter paste looks
// like underneath.
func confirmPromptConsumed(ctx context.Context, coop *coopapi.Client, baseline uint64) bool {
	deadline := time.Now().Add(promptConsumeTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		if state, err := coop.GetAgentState(ctx); err == nil {
			if state.RequestCount > baseline {
				return true
			}
			switch state.State {
			case "exited", "parked", "rate_limited", "budget_exceeded", "error", "prompt":
				return true
			}
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(promptConsumePoll):
		}
	}
	return false
}

// screenShowsUnsentInput reports whether coop's current screen shows text
// sitting unsubmitted in Claude Code's input line — the "❯ [Pasted text #1 +2
// lines]" a lost trailing Enter leaves behind. Claude-only (a codex pod has
// no TUI screen); a screen-fetch error returns false so a transient API
// hiccup never produces a stray Enter.
func screenShowsUnsentInput(ctx context.Context, coop *coopapi.Client) bool {
	if !harness.ClaudeDriver(harness.MustFromEnv(), os.Getenv("BOAT_COMMAND")) {
		return false
	}
	screen, err := coop.GetScreenText(ctx)
	if err != nil {
		return false
	}
	return screenInputLineHasText(screen)
}

// screenInputLineHasText finds Claude Code's input line — the LAST line that
// starts with the ❯ (U+276F) glyph, mirroring coop's screen classifier — and
// reports whether anything follows the glyph. An empty "❯" is a healthy idle
// prompt; "❯ 1. No, exit" is a dialog option marker, not the input line; the
// transcript's echo of an earlier submitted prompt also starts with ❯ but sits
// ABOVE the input line, which is why the scan runs bottom-up.
func screenInputLineHasText(screen string) bool {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "\u276f") {
			continue
		}
		after := strings.TrimSpace(strings.TrimPrefix(trimmed, "\u276f"))
		if after != "" && after[0] >= '0' && after[0] <= '9' {
			// Dialog option marker ("❯ 1. ..."), keep looking upward.
			continue
		}
		return after != ""
	}
	return false
}

// recoveryKeyDelay is the pause recoverFromErrorAndNudge leaves between the
// error-screen dismiss keystrokes (Return, Ctrl-U) and the raw prompt input, so
// the TUI settles between steps. Overridable for tests.
var recoveryKeyDelay = 200 * time.Millisecond

// confirmTurnTimeout bounds how long confirmNudgeTurn waits for a delivered
// nudge to produce a turn. Overridable for tests.
var confirmTurnTimeout = 30 * time.Second

// confirmTurnPoll is confirmNudgeTurn's state poll interval. Overridable for
// tests.
var confirmTurnPoll = 1 * time.Second

// confirmNudgeTurn reports whether a just-delivered nudge actually started a
// work turn. coop.Nudge only confirms the bytes were queued, not that Claude
// consumed them (ka-0000002714) — a nudge dropped in the startup-idle window
// leaves the agent idle forever. We poll for a transition to "working", or
// request_count advancing past baseline, as proof the turn started. The
// request_count proof is for a harness that answers instantly (claudeless):
// coop goes idle -> idle and no poll ever sees "working" (gd-acTuM7LA75).
//
// "exited" / "parked" / "rate_limited" / "budget_exceeded" also count as
// confirmation: in each case the agent engaged (or the session is gone), so a
// re-send cannot help and is handled by the caller's isSessionAlive /
// monitorExit paths. Only a persistent "idle" with no observed transition
// returns false, triggering a re-send.
func confirmNudgeTurn(ctx context.Context, coop *coopapi.Client, baseline uint64) bool {
	deadline := time.Now().Add(confirmTurnTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		if state, err := coop.GetAgentState(ctx); err == nil {
			if state.RequestCount > baseline {
				return true
			}
			switch state.State {
			case "working", "exited", "parked", "rate_limited", "budget_exceeded":
				return true
			}
			// "idle" (or any other startup state) → keep waiting.
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(confirmTurnPoll):
		}
	}
	return false
}

// turnOutcomeTimeout bounds how long turnRecovered waits for a re-injected
// turn to finish one way or the other. Overridable for tests.
var turnOutcomeTimeout = 60 * time.Second

// turnOutcomeInFlight is the category turnRecovered returns when the turn is
// still "working" at turnOutcomeTimeout with no completed request.
const turnOutcomeInFlight = "in_flight"

// turnRecovered reports whether a recovery re-inject produced a GENUINE turn,
// versus a doomed retry that lands straight back in an error or rate/limit
// state. It exists to make recoverFromErrorAndNudge's log honest:
// confirmNudgeTurn deliberately treats rate_limited / budget_exceeded / parked
// as "the turn ran", but those are NOT a recovery, and logging "agent working
// again" on them is exactly what kept the health probe green through the
// rate-limit livelock (ka-0000001209).
//
// A single sample is not enough: the doomed retry itself reads "working" for
// its round-trip, so a sample taken right after confirmNudgeTurn returns says
// "recovered" for a turn that fails a second later (gd-a0jdFgwngn). So it waits
// for the outcome: request_count advancing past baseline (a usage-bearing turn
// landed) or the state leaving "working". Returns (true, "") when recovered,
// (false, turnOutcomeInFlight) when the turn is still working with no
// completed request at turnOutcomeTimeout, or (false, <state-or-error_category>)
// naming why it is not.
func turnRecovered(ctx context.Context, coop *coopapi.Client, baseline uint64) (bool, string) {
	deadline := time.Now().Add(turnOutcomeTimeout)
	for {
		state, err := coop.GetAgentState(ctx)
		if err != nil {
			return false, "unknown"
		}
		if state.RequestCount > baseline {
			return true, ""
		}
		switch state.State {
		case "working":
			// Still in flight — keep waiting for the outcome.
		case "idle":
			return true, ""
		case "error":
			// Name the error family (rate_limited / budget_exceeded / server_error /
			// …) so the honest log line says what the turn actually hit.
			if state.ErrorCategory != "" {
				return false, state.ErrorCategory
			}
			return false, "error"
		default:
			// rate_limited / budget_exceeded / parked / exited / restarting — the
			// turn ran but did not recover to a productive state.
			return false, state.State
		}
		if !time.Now().Before(deadline) {
			return false, turnOutcomeInFlight
		}
		select {
		case <-ctx.Done():
			return false, "unknown"
		case <-time.After(confirmTurnPoll):
		}
	}
}

// standbyWait polls the agent bead until the pool manager assigns work. It
// returns whether the assignment specialized BOAT_PROJECT or BOAT_ROLE, so the
// caller knows whether it must re-materialize the pod's Claude config.
func standbyWait(ctx context.Context, coop *coopapi.Client) (bool, error) {
	// Signal monitor_agent_idle to skip state updates.
	if err := os.WriteFile("/tmp/standby_active", []byte("1"), 0644); err != nil {
		logInit("WARNING: failed to write standby sentinel: %v", err)
	}

	beadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	logInit("Standby mode: Claude is idle, waiting for assignment (bead: %s)", beadID)

	pollInterval := envDuration("BOAT_STANDBY_POLL", 5*time.Second)
	maxWait := envDuration("BOAT_STANDBY_MAX_TTL", 24*time.Hour)
	deadline := time.Now().Add(maxWait)

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}

		if time.Now().After(deadline) {
			logInit("Standby safety-net TTL (%s) exceeded, shutting down", maxWait)
			os.Remove("/tmp/standby_active")
			_ = os.WriteFile("/tmp/standby_expired", []byte("1"), 0644)
			_ = coop.Shutdown(ctx)
			return false, fmt.Errorf("standby TTL exceeded")
		}

		// Check agent_state via beads daemon.
		currentState := "prewarmed"
		if daemon != nil {
			bead, err := daemon.GetBead(ctx, beadID)
			if err == nil {
				if s := bead.Fields["agent_state"]; s != "" {
					currentState = s
				}
			}
		}

		if currentState != "prewarmed" {
			logInit("Assignment received (state: %s), exiting standby", currentState)

			// Hydrate env vars from the assigned bead's fields.
			specialized := false
			if daemon != nil {
				specialized = hydrateAssignmentEnv(ctx, beadID)
			}

			// Mark this pod as assigned so prompt/prime resolution stops treating
			// it as a parked prewarmed agent. BOAT_AGENT_STATE stays "prewarmed"
			// in the pod env forever (it is never rewritten on assignment), so
			// without this sentinel detectNudgeType / gb prime would keep handing
			// the agent the "wait for a nudge, do no work" standby text instead of
			// the real thread/adhoc nudge — the agent then sits idle and never
			// wraps up. See ka-0000004623.
			if err := os.WriteFile(standbyAssignedSentinelPath, []byte("1"), 0644); err != nil {
				logInit("WARNING: failed to write standby-assigned sentinel: %v", err)
			}

			os.Remove("/tmp/standby_active")
			return specialized, nil
		}

		time.Sleep(pollInterval)
	}
}

// rematerializeAfterSpecialization rewrites the pod's Claude config when a
// warm-pool assignment gave it a concrete (project, role) AFTER runSetup had
// already written ~/.claude/*. Without this, a pooled pod's settings.json,
// .mcp.json and CLAUDE.md are resolved with NO project subscription for the
// pod's whole life, while gb prime (which reads env at call time) sees the
// specialized project — the files and the prompts disagree.
//
// The role comes from the env, not cfg.role: hydrateAssignmentEnv can change
// BOAT_ROLE as well as BOAT_PROJECT.
func rematerializeAfterSpecialization(cfg initConfig, specialized bool) {
	if !specialized {
		return
	}
	role := os.Getenv("BOAT_ROLE")
	if role == "" {
		role = cfg.role
	}
	project := os.Getenv("BOAT_PROJECT")
	if project == "" {
		project = "none"
	} else {
		// resolveCoopWorkdir below keys off cfg.project; without this, a pod
		// that started generic (cfg.project == "") keeps resolving the
		// scaffold workspace forever, even after hydration gave it a real
		// project to clone.
		cfg.project = project
	}
	logInit("Re-materializing Claude config after pool specialization (project=%s role=%s)", project, role)
	setupClaudeSettingsFn(resolveCoopWorkdir(initToK8sConfig(cfg)), role, cfg.mockMode)
}

// hydrateAssignmentEnv fetches the agent bead and exports thread/task env vars.
// It returns true when the assignment specialized BOAT_PROJECT or BOAT_ROLE to
// a concrete value different from what the pod already had, so callers know
// whether the pod's already-materialized Claude config needs a rewrite.
func hydrateAssignmentEnv(ctx context.Context, beadID string) bool {
	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil {
		return false
	}

	if ch := bead.Fields["slack_thread_channel"]; ch != "" {
		os.Setenv("SLACK_THREAD_CHANNEL", ch)
		logInit("Hydrated SLACK_THREAD_CHANNEL=%s", ch)
	}
	if ts := bead.Fields["slack_thread_ts"]; ts != "" {
		os.Setenv("SLACK_THREAD_TS", ts)
		logInit("Hydrated SLACK_THREAD_TS=%s", ts)
	}
	if proj := bead.Fields["project"]; proj != "" && os.Getenv("PROJECT") == "" {
		os.Setenv("PROJECT", proj)
		logInit("Hydrated PROJECT=%s", proj)
	}
	if tid := bead.Fields["task_id"]; tid != "" {
		os.Setenv("BOAT_TASK_ID", tid)
		logInit("Hydrated BOAT_TASK_ID=%s", tid)
	}
	// Assign-time specialization of a generic-warm pod (W1, ka-0000007566). A
	// project-agnostic pool member boots with a sentinel/empty project+role; the
	// pool manager stamped the concrete project+role (and, for a genuinely generic
	// pod, a recredential marker) in the atomic assign write. Adopt the target
	// (project,role) into the pod's env so gb prime, advice/role-instruction
	// resolution, and project-scoped kd/gb operations all run AS the specialized
	// (project,role) on spawn — this is the pod half of removing the project/role
	// bake-in. BOAT_PROJECT/BOAT_ROLE are the canonical env keys the k8s agent
	// config reads (agent_start_k8s.go); PROJECT is set above for the same reason.
	specialized := false
	if proj := bead.Fields["project"]; proj != "" && !isGenericSpecializationValue(proj) {
		if os.Getenv("BOAT_PROJECT") != proj {
			specialized = true
		}
		os.Setenv("BOAT_PROJECT", proj)
		logInit("Hydrated BOAT_PROJECT=%s", proj)
	}
	if role := bead.Fields["role"]; role != "" && !isGenericSpecializationValue(role) {
		if os.Getenv("BOAT_ROLE") != role {
			specialized = true
		}
		os.Setenv("BOAT_ROLE", role)
		os.Setenv("ROLE", role)
		logInit("Hydrated BOAT_ROLE=%s", role)
	}
	// Re-credential signal. When the pool specialized a genuinely GENERIC pod, it
	// stamps specialized_from_generic=true + recredential_project=<target>. The
	// per-project ServiceAccount and ExternalSecret-mounted env are pod-spec
	// fields (immutable on a running pod), so the full SA/token swap is the W1
	// follow-up spike (mount a superset/broker token or re-exec with new projected
	// creds — see the bead). What the pod CAN do now is re-provision the
	// credentials it owns at runtime and surface the swap clearly; account-scoped
	// Claude creds are re-checked and a sentinel is dropped for the credential
	// path + operator visibility. This mirrors the freeze/thaw resume hook's
	// "re-read secrets fresh" contract (ka-0000004088), scoped to what a running
	// K8s pod can safely re-credential today.
	if bead.Fields["specialized_from_generic"] == "true" {
		target := bead.Fields["recredential_project"]
		logInit("Assign-time specialization: generic-warm pod re-credentialing for project=%s role=%s",
			orStr(target, "?"), orStr(bead.Fields["role"], "?"))
		if err := os.WriteFile(specializedRecredentialSentinelPath, []byte(target), 0644); err != nil {
			logInit("WARNING: failed to write specialization re-credential sentinel: %v", err)
		}
	}
	return specialized
}

// specializedRecredentialSentinelPath is written by hydrateAssignmentEnv when a
// generic-warm pod is specialized to a concrete project at assign time and the
// pool stamped a re-credential marker. Its content is the target project. The
// credential path + operator tooling read it to know the pod left the pool as a
// generic member that acquired project scope at assign (W1, ka-0000007566).
const specializedRecredentialSentinelPath = "/tmp/standby_recredential_project"

// isGenericSpecializationValue reports whether a project/role field still
// carries the generic-pool sentinel, so hydration never adopts "*" as a concrete
// project or role. Kept in sync with poolmanager.GenericPoolValue.
func isGenericSpecializationValue(v string) bool {
	return v == genericPoolSentinel
}

// genericPoolSentinel mirrors poolmanager.GenericPoolValue — the sentinel a
// generic-warm pool member carries in its project/role fields. Duplicated as a
// local const (rather than imported) to keep the in-pod gb binary decoupled from
// the controller's poolmanager package, matching how other assign-time field
// names are referenced as string literals here.
const genericPoolSentinel = "*"

// terminalHardAuth403ParkPrefix is the parked_reason prefix coop sets when its
// terminal hard-auth-403 watchdog trips (N consecutive
// "403 key not allowed to access model"). Kept in sync with coop's
// TERMINAL_HARD_AUTH_403_PARK_PREFIX (crates/cli/src/session/transition.rs).
const terminalHardAuth403ParkPrefix = "terminal_hard_auth_403"

// isTerminalHardAuth403 reports whether a coop agent state represents a
// terminal hard-auth-403 park: the session is "parked" and its parked_reason
// carries the terminalHardAuth403ParkPrefix. Such an agent can no longer run
// a single inference turn (so it cannot process its own gb stop) and must be
// torn down with a clear failure reason instead of zombying (ka-0000006562).
func isTerminalHardAuth403(state *coopapi.AgentState) bool {
	return state != nil && state.State == "parked" &&
		strings.HasPrefix(state.ParkedReason, terminalHardAuth403ParkPrefix)
}

// terminalNotLoggedInParkPrefix is the parked_reason prefix coop sets when its
// terminal login-required watchdog trips (N consecutive
// "Not logged in · Please run /login" turns). Kept in sync with coop's
// TERMINAL_LOGIN_REQUIRED_PARK_PREFIX (crates/cli/src/session/transition.rs).
const terminalNotLoggedInParkPrefix = "terminal_login_required"

// isTerminalNotLoggedIn reports whether a coop agent state represents a
// terminal login-required park: the session is "parked" and its parked_reason
// carries the terminalNotLoggedInParkPrefix. Such an agent never received a
// usable credential at spawn (credential-not-ready-at-spawn), cannot run a
// single inference turn — so it cannot process its own gb stop — and must be
// torn down with a clear failure reason instead of nudging into the void and
// burning cron wake-ups (ka-0000007394).
func isTerminalNotLoggedIn(state *coopapi.AgentState) bool {
	return state != nil && state.State == "parked" &&
		strings.HasPrefix(state.ParkedReason, terminalNotLoggedInParkPrefix)
}

// terminalAuthFailureBound is the number of CONSECUTIVE polls observing a
// terminal, non-recoverable credential error (error_category=unauthorized or
// out_of_credits) after which monitorExit gives up LOUDLY. Small enough to trip
// fast (win the race against the K8s /livez restart — probe period 15s ×
// failureThreshold 3 ≈ 45s to SIGTERM, vs this bound × 5s poll ≈ 15s), large
// enough to tolerate a transient credential-switch blip. Kept in the same
// spirit as coop's HARD_AUTH_403_THRESHOLD / LOGIN_REQUIRED_THRESHOLD (both 3).
var terminalAuthFailureBound = 3

// isTerminalAuthFailure reports whether a coop agent state carries a terminal,
// non-recoverable credential error: error_category=unauthorized (a 401/403 key
// ACL — the key is not allowed to run this model) or out_of_credits (the account
// balance is spent). coop marks BOTH categories zombie the instant it sees them
// (session/transition.rs: Unauthorized | OutOfCredits | BudgetExceeded →
// lifecycle.zombie=true → /livez 503), because rotating credentials cannot fix a
// key ACL or a spent account — a failover would cold-restart the pod forever
// (ka-0000001718). BudgetExceeded is deliberately EXCLUDED here: it has its own
// budget-marker + auth-failover path (maybe_publish_auth_exhausted) and a fresh
// per-agent key can clear it, so it is not terminal for the agent the way an ACL
// denial or an empty account is.
//
// Unlike the hard-auth-403 and login-required cases, coop does NOT park on these
// categories — it leaves the session in "error" with error_category set — so
// there is no ParkedReason to key on; the signal is the category itself. Without
// a gb-side handler the zombie pod simply waits for the K8s liveness probe to
// SIGTERM it, at which point the generic context_cancelled teardown stamps
// close_reason=context_cancelled_involuntary and the auto-forensics report blames
// "node drain, rollout, or Spot reclaim" — a FALSE infra attribution for what is
// a credential failure already recorded on the same bead (ka-0000001098: a
// chronic site-deployment auth storm mislabeled as Spot reclaim on an all-on-
// demand cluster). This watchdog closes the bead with the HONEST cause instead.
func isTerminalAuthFailure(state *coopapi.AgentState) bool {
	return state != nil &&
		(state.ErrorCategory == "unauthorized" || state.ErrorCategory == "out_of_credits")
}

// monitorExitInitialSleep is how long monitorExit waits before its first poll,
// and monitorExitPollSleep the gap between subsequent ones. Named rather than
// inline so tests can assert the production values without spending them:
// every monitorExit test sets monitorSleepOverride, so nothing would otherwise
// notice if these drifted.
const (
	monitorExitInitialSleep = 10 * time.Second
	monitorExitPollSleep    = 5 * time.Second
)

// monitorExit polls agent state and triggers shutdown when agent exits or is
// rate-limited. requestBaseline is injectPrompt's pre-prompt request_count: a
// count above it proves a turn landed even if no poll ever saw "working".
func monitorExit(ctx context.Context, coop *coopapi.Client, productiveSink *atomic.Bool, requestBaseline uint64) error {
	initialSleep := monitorExitInitialSleep
	pollSleep := monitorExitPollSleep
	if monitorSleepOverride != 0 {
		initialSleep = monitorSleepOverride
		pollSleep = monitorSleepOverride
	}
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(initialSleep):
	}

	// Zombie watchdog: if the agent never transitions to "working" (or lands a
	// turn past requestBaseline) within
	// zombieWatchdogTimeout after the nudge was delivered, Claude is stuck
	// (e.g. failed to authenticate, hung during startup). Shut down coop
	// so the restart loop can retry or terminate the pod.
	watchdogTimeout := zombieWatchdogTimeout
	if watchdogTimeout == 0 {
		watchdogTimeout = 10 * time.Minute
	}
	watchdogDeadline := time.Now().Add(watchdogTimeout)
	everWorked := false

	// Rate-limit livelock bound (ka-0000001209, the deferred harness-side item 1
	// of ka-0000007458). Counts CONSECUTIVE rate-limit park cycles (account
	// rate_limited OR model_rate_limited) that produced NO successful turn. When
	// the account's monthly spend cap is hit, EVERY sibling model group on the
	// same account 429s too, so a LiteLLM fallback arm cannot help by
	// construction — the re-inject loop is structurally incapable of recovering
	// and would otherwise park forever (~300 times over 11-14h in the incident),
	// health-probe green the whole time. Once the streak reaches
	// rateLimitLivelockBound the agent gives up LOUDLY: requeue the claimed task
	// and close the bead failed (a counted involuntary death) instead of spinning
	// silently. A fresh agent gets a fresh per-agent key, so requeue is the
	// recovery (advice ka-0000004373).
	//
	// The streak's reset discriminator is coop's request_count, NOT the agent
	// state or the existing backoff streak: a rate-limit recovery re-inject flips
	// coop to "working" for the few seconds of the doomed round-trip and coop
	// clears driver.error on that flip (session/transition.rs), so state-based
	// signals CANNOT distinguish a real turn from a doomed retry — which is
	// exactly why the existing rateLimitNoHintStreak keeps resetting and the
	// backoff never escalates to give-up territory. request_count advances ONLY
	// on a usage-bearing assistant message; a 429 surfaces an isApiErrorMessage
	// carrying no usage, so it does not advance. lastGoodRequestCount snapshots it
	// each poll; the park branches compare against it to decide "did a genuine
	// turn land since the last park?" before incrementing the streak.
	//
	// The streak itself is package-level (rateLimitLivelockStreak), so a restart
	// of coop in the same pod does not wipe it (gd-a0jdFgwngn). When it carries
	// parks over from a previous life, the first poll's request_count becomes
	// the baseline instead of resetting it, so a count the new coop starts above
	// zero is not mistaken for a genuine turn.
	lastGoodRequestCount := uint64(0)
	adoptRequestCountBaseline := rateLimitLivelockStreak > 0

	// Terminal auth-failure streak (ka-0000001098). Counts CONSECUTIVE polls
	// observing error_category=unauthorized / out_of_credits with no intervening
	// productive turn. coop marks these categories zombie immediately but does
	// not park on them, so — unlike the hard-auth-403 / login-required watchdogs
	// above — there is no ParkedReason to trip on; the give-up is driven by this
	// streak reaching terminalAuthFailureBound. Reset the instant a genuine turn
	// lands (request_count advances, in the block below) so a transient
	// credential-switch blip that recovers never trips it.
	terminalAuthFailureStreak := 0

	// Connectivity (no_internet) livelock streak (gd-fr3xjSxIws). Counts
	// CONSECUTIVE no_internet recoveries with no genuine turn in between. Keyed on
	// request_count — NOT the transient "working" flip a doomed re-inject causes —
	// for the same reason as the rate-limit streak above.
	noInternetLivelockStreak := 0

	// Progress-liveness heartbeat throttle (ka-0000000286): last_progress_at is
	// consumed by a 90m staleness guard, so stamping on EVERY turn (~1/1.3min at
	// median fleet cadence) would only add SSE/activity-feed churn without any
	// benefit to the guard's resolution. Stamp at most once per
	// progressStampInterval; zero value means "never stamped yet" so the first
	// genuine turn always stamps.
	var lastProgressStamp time.Time

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollSleep):
		}

		state, err := coop.GetAgentState(ctx)
		if err != nil {
			// coop is gone, agent exited
			return nil
		}

		if state.State == "working" {
			everWorked = true
			// Signal the supervisor loop that this session reached a productive
			// (working) state, so the durable credential-switch terminal cap
			// re-arms (a genuine work turn proves the flap resolved). This is
			// distinct from session wall-clock: the model-429 flap sessions ran
			// ~277s yet never worked, so elapsed time alone is NOT a safe
			// productivity proxy (ka-0000001718 Part B).
			if productiveSink != nil {
				productiveSink.Store(true)
			}
			// Clear the transient server-error streak: a productive turn proves
			// the upstream 500/api_error cleared, so the next one starts its
			// backoff from base instead of inheriting a stale exponent. (The
			// rate-limit backoff streaks reset on request_count instead, below.)
			serverErrorStreak = 0
			// The agent is making progress again, so any prior upstream outage
			// has cleared: forget the observation so a later, unrelated teardown
			// is not misattributed to upstream_api_error (ka-0000000202).
			clearUpstreamOutageObservation()
		}

		// A harness that answers instantly (claudeless) goes idle -> idle, so the
		// "working" block above never fires; a usage-bearing turn past the
		// pre-prompt baseline is the same proof (gd-acTuM7LA75). The baseline is
		// this coop process's own count, so a resumed coop that starts above zero
		// does not count as having worked.
		if !everWorked && state.RequestCount > requestBaseline {
			everWorked = true
			if productiveSink != nil {
				productiveSink.Store(true)
			}
		}

		if adoptRequestCountBaseline {
			adoptRequestCountBaseline = false
			if state.RequestCount > lastGoodRequestCount {
				lastGoodRequestCount = state.RequestCount
			}
		}

		// Rate-limit livelock bound (ka-0000001209): reset the give-up streak the
		// instant a GENUINE turn lands, on ANY observed state — not just
		// "working". The discriminator is request_count advancing, which only a
		// usage-bearing (successful) turn does; a doomed rate-limit retry flips
		// coop to "working" for its round-trip yet never advances it, so keying
		// off request_count (rather than the "working" block above) is what makes
		// the reset immune to that transient flip. This runs every poll so a real
		// turn that completes and returns to "idle" before the next poll still
		// clears the streak.
		if state.RequestCount > lastGoodRequestCount {
			lastGoodRequestCount = state.RequestCount
			rateLimitLivelockStreak = 0
			// Same for both rate-limit backoff streaks: a genuine turn proves the
			// throttle cleared, so the next one starts from base. Keyed here, not on
			// the "working" flip, because a doomed retry's flip used to drop the
			// backoff to base mid-livelock (gd-a0jdFgwngn).
			rateLimitNoHintStreak = 0
			modelRateLimitStreak = 0
			// A genuine usage-bearing turn proves the credential works, so any
			// accumulated terminal-auth streak was a transient blip — clear it
			// (ka-0000001098).
			terminalAuthFailureStreak = 0
			// Same for the connectivity (no_internet) streaks: a genuine turn
			// proves the network/DNS outage cleared, so the next drop starts its
			// backoff from base instead of inheriting a stale exponent. Keyed on
			// request_count, not the "working" flip: a doomed re-inject (e.g. a
			// Codex 401 surfaced as httpConnectionFailed) flips coop to working
			// every cycle, which used to zero the backoff and hide the loop
			// (gd-fr3xjSxIws).
			noInternetStreak = 0
			noInternetLivelockStreak = 0

			// Progress heartbeat (ka-0000000286): a genuine, usage-bearing turn
			// just landed (request_count advanced — the same discriminator the
			// livelock bound above uses, immune to the doomed-retry "working"
			// flip). Stamp last_progress_at so the server-side single-claim guard
			// (findLiveAgentForTask) and gb news can tell a working crew from a
			// livelocked one whose pod stays Running and whose agent_state stays
			// "working". Throttled to progressStampInterval (the guard is a 90m
			// bound, so finer resolution is wasted SSE churn). Best-effort: a
			// stamp failure must never perturb the monitor loop — the guards fail
			// open on a missing/stale stamp.
			now := time.Now()
			if now.Sub(lastProgressStamp) >= progressStampInterval {
				if agentID := os.Getenv("KD_AGENT_ID"); agentID != "" && daemon != nil {
					if err := daemon.StampAgentProgress(ctx, agentID, now); err != nil {
						logInit("warning: failed to stamp last_progress_at on agent bead %s: %v", agentID, err)
					} else {
						lastProgressStamp = now
					}
				}
			}
		}

		if state.State == "exited" {
			logInit("Agent exited, requesting coop shutdown")
			_ = coop.Shutdown(ctx)
			return nil
		}

		// Terminal hard-auth-403 watchdog. coop parks the session with a
		// "terminal_hard_auth_403" reason after N consecutive
		// "403 key not allowed to access model" errors — the agent can no
		// longer run a single inference turn (so it cannot process its own
		// `gb stop`) and would otherwise zombie the pod indefinitely
		// (ka-0000006562). Close the bead with the clear failure reason and
		// shut down coop so the pod is recycled instead of spinning idle.
		if isTerminalHardAuth403(state) {
			reason := state.ParkedReason
			if reason == "" {
				reason = terminalHardAuth403ParkPrefix
			}
			logInit("FATAL: terminal hard-auth-403 — %s; closing bead and requesting coop shutdown", reason)
			agentID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
			if agentID != "" && daemon != nil {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := daemon.CloseBead(closeCtx, agentID, map[string]string{
					"agent_state":  "failed",
					"close_reason": reason,
				}); err != nil {
					logInit("warning: failed to close agent bead on terminal hard-auth-403: %v", err)
				}
				closeCancel()
			}
			_ = coop.Shutdown(ctx)
			return nil
		}

		// Terminal login-required watchdog. coop parks the session with a
		// "terminal_login_required" reason after N consecutive
		// "Not logged in · Please run /login" turns — the credential was never
		// provisioned/validated for this pod (credential-not-ready-at-spawn),
		// so the agent cannot run a single inference turn (it cannot process
		// its own `gb stop`) and would otherwise nudge into the void on every
		// turn, burning cron wake-ups (33 in the original incident,
		// ka-0000007394). Close the bead with the distinct `not_logged_in`
		// reason (so gb audit can flag credential-provisioning failures —
		// ties into ka-0000001328) and shut down coop so the pod is recycled.
		if isTerminalNotLoggedIn(state) {
			reason := state.ParkedReason
			if reason == "" {
				reason = terminalNotLoggedInParkPrefix
			}
			logInit("FATAL: terminal login-required (credential not ready at spawn) — %s; closing bead and requesting coop shutdown", reason)
			agentID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
			if agentID != "" && daemon != nil {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := daemon.CloseBead(closeCtx, agentID, map[string]string{
					"agent_state":  "failed",
					"close_reason": "not_logged_in",
				}); err != nil {
					logInit("warning: failed to close agent bead on terminal login-required: %v", err)
				}
				closeCancel()
			}
			_ = coop.Shutdown(ctx)
			return nil
		}

		// Terminal auth-failure watchdog (ka-0000001098). coop marks
		// error_category=unauthorized / out_of_credits zombie the instant it sees
		// them but does NOT park (so the two watchdogs above cannot catch them),
		// leaving the pod to wait for the K8s /livez probe to SIGTERM it — at which
		// point the generic context_cancelled teardown FALSELY attributes the death
		// to "node drain, rollout, or Spot reclaim" even though the real cause
		// (the auth error) is already on the bead. Count consecutive polls that
		// observe the terminal category with no intervening productive turn; once
		// the streak reaches the bound, give up LOUDLY with the honest cause:
		// requeue the claimed task and close the bead failed. A fresh agent gets a
		// fresh per-agent key, so requeue is the recovery path — but a chronically
		// mis-provisioned project (ka-0000001098: site-deployment) will re-hit the
		// same wall, which is exactly the signal an operator needs to see clearly
		// rather than buried under a phantom infra-churn attribution.
		if isTerminalAuthFailure(state) {
			terminalAuthFailureStreak++
			if terminalAuthFailureStreak >= terminalAuthFailureBound {
				failAgentTerminalAuth(ctx, coop, state.ErrorCategory, terminalAuthFailureStreak, state.LastMessage)
				return nil
			}
			logInit("terminal auth failure #%d (bound %d): category=%s %s",
				terminalAuthFailureStreak, terminalAuthFailureBound, state.ErrorCategory, strings.TrimSpace(state.LastMessage))
		}

		// Zombie watchdog: agent has never worked and deadline passed.
		if !everWorked && time.Now().After(watchdogDeadline) {
			logInit("FATAL: agent never started working after %s — requesting coop shutdown to prevent zombie pod", watchdogTimeout)
			_ = coop.Shutdown(ctx)
			return nil
		}

		// Detect rate-limited state and park the pod.
		if state.ErrorCategory == "rate_limited" {
			parkStart := time.Now()
			logInit("Agent rate-limited, dismissing prompt")
			_ = coop.SendKeys(ctx, []string{"Return"})
			logInit("Rate limit info: %s", state.LastMessage)

			// Rate-limit livelock bound (ka-0000001209). This park produced no
			// successful turn since the last one (request_count did not advance, or
			// the reset above would have zeroed the streak), so count it. Once the
			// streak reaches the bound, an account-monthly-spend cap is the only
			// explanation that fits (a transient limit clears well inside a handful
			// of exponential-backoff parks), and no re-inject can clear it — a
			// LiteLLM fallback arm 429s on the same account too. Give up loudly.
			rateLimitLivelockStreak++
			if rateLimitLivelockStreak >= rateLimitLivelockBound {
				failAgentRateLimitLivelock(ctx, coop, "rate_limited", rateLimitLivelockStreak, state.LastMessage)
				return nil
			}

			// Report rate_limited status to agent bead.
			agentID := os.Getenv("KD_AGENT_ID")
			if agentID != "" && daemon != nil {
				_ = daemon.UpdateAgentState(ctx, agentID, "rate_limited")
			}

			// Sleep WITHOUT shutting down coop so port 9090 (liveness probe) stays
			// alive during the backoff. Calling coop.Shutdown() here caused the
			// liveness probe to fail and the pod to be killed before entrypoint.sh
			// could sleep — creating a crash-restart loop.
			sleepDur := rateLimitBackoff(state.LastMessage)
			logInit("Rate limited — sleeping %v before retry (coop health probe stays up)", sleepDur)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(sleepDur):
			}

			logInit("Rate limit backoff complete, re-injecting work prompt")
			if agentID != "" && daemon != nil {
				_ = daemon.UpdateAgentState(ctx, agentID, "idle")
			}
			// coop is still wedged at state="error" from the 429 — dismiss the
			// error screen and drive a fresh turn via raw PTY input, which the
			// nudge state-gate would otherwise block (ka-0000006423).
			_ = recoverFromErrorAndNudge(ctx, coop)
			// The park is time gb chose to spend, not time the agent failed to
			// start: keep it off the never-worked watchdog, or a backoff longer
			// than the watchdog restarts coop after one park (gd-a0jdFgwngn).
			watchdogDeadline = watchdogDeadline.Add(time.Since(parkStart))
			// Continue the monitoring loop (don't return).
		}

		// Detect a MODEL-group (pool-exhaustion) rate-limit and park IN PLACE.
		// coop classifies a 429 carrying a no-fallback / no-deployments marker
		// ("Available Model Group Fallbacks=None" / "no deployments available")
		// as ErrorCategory::ModelRateLimited and surfaces
		// error_category=model_rate_limited on /api/v1/agent (ka-0000001718,
		// residual of P0 ka-0000003464). Unlike the account-level rate_limited
		// case above, ModelRateLimited is deliberately NOT is_auth_exhaustion in
		// coop, so it never fired handle_rate_limit → try_auto_rotate → the
		// exit-10 credential switch. That is the whole point: every pooled
		// credential PROFILE serves the SAME throttled model on the SAME upstream,
		// so rotating cannot help — the old behavior cold-restarted the pod every
		// ~277s (a fresh session bead each cycle, resumed=False) and never
		// recovered. Operator intent (gardenio/docs/litellm-hardening-ha-plan.md,
		// Matt 2026-06-16): a 429 on a model group with "Fallbacks=None" SHOULD
		// PARK, not rotate — cross-role Bedrock spillover is forbidden
		// (policy ka-0000002660).
		//
		// The recovery is identical in SHAPE to the account rate-limit path
		// (dismiss the error screen, sleep with jittered exponential backoff,
		// re-inject the work prompt, keep the SAME session and the liveness probe
		// up) but with NO profile rotation / auth failover and a distinct
		// model_rate_limited agent_state pill (the operator H7 ask: surface
		// "model throttle" distinctly from an account cap).
		if state.ErrorCategory == "model_rate_limited" {
			parkStart := time.Now()
			logInit("Agent model-rate-limited (pool exhausted, no fallback), dismissing prompt")
			_ = coop.SendKeys(ctx, []string{"Return"})
			logInit("Model rate limit info: %s", state.LastMessage)

			// Rate-limit livelock bound (ka-0000001209) — shared with the account
			// rate_limited branch above. A model-group throttle with no fallback and
			// no deployments is likewise unrecoverable by re-injection (rotation is
			// deliberately disabled here because every pooled profile serves the same
			// throttled model), so an unbounded park is the same silent-livelock
			// trap. Count parks with no intervening successful turn and give up
			// loudly once the bound is reached so the bead requeues instead of
			// spinning green-but-dead.
			rateLimitLivelockStreak++
			if rateLimitLivelockStreak >= rateLimitLivelockBound {
				failAgentRateLimitLivelock(ctx, coop, "model_rate_limited", rateLimitLivelockStreak, state.LastMessage)
				return nil
			}

			// Report model_rate_limited status to agent bead so the beads-viewer
			// and Slack cards show the model-throttle distinctly from an account
			// cap or a rotation.
			agentID := os.Getenv("KD_AGENT_ID")
			if agentID != "" && daemon != nil {
				_ = daemon.UpdateAgentState(ctx, agentID, "model_rate_limited")
			}

			// Sleep WITHOUT shutting down coop so port 9090 (liveness probe) stays
			// alive during the backoff — same rationale as the account rate-limit
			// path. The session is preserved (no exit-10, no fresh session bead).
			sleepDur := modelRateLimitBackoff()
			logInit("Model rate limited — sleeping %v before retry (coop health probe stays up)", sleepDur)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(sleepDur):
			}

			logInit("Model rate limit backoff complete, re-injecting work prompt")
			if agentID != "" && daemon != nil {
				_ = daemon.UpdateAgentState(ctx, agentID, "idle")
			}
			// Same wedge as the account rate-limit path: coop is stuck at
			// state="error" and Nudge cannot clear it (ka-0000006423).
			_ = recoverFromErrorAndNudge(ctx, coop)
			// Same as the account rate-limit path: the park does not count toward
			// the never-worked watchdog (gd-a0jdFgwngn).
			watchdogDeadline = watchdogDeadline.Add(time.Since(parkStart))
			// Continue the monitoring loop (don't return).
		}

		// Detect a transient upstream server error (HTTP 500 / api_error /
		// overloaded / 502 / 503) and re-nudge in place. coop classifies these
		// as ErrorCategory::ServerError and surfaces error_category=server_error
		// on /api/v1/agent, but — unlike rate_limited — nothing acted on it: the
		// agent sat on the "API Error: 500 … Internal server error" screen until
		// the stale reaper eventually killed it (ka-0000002780).
		//
		// A 500 is server-side and "usually temporary — try again in a moment",
		// so the recovery is identical in SHAPE to the rate-limit path (dismiss
		// the error screen, sleep, re-inject the work prompt, keep the liveness
		// probe up) but DIFFERENT in two ways:
		//   1. Backoff is short (base 5s, capped at 2m) — a 500 clears in seconds,
		//      not the minutes a spend-limit needs.
		//   2. NO profile rotation / auth failover. Rotating accounts cannot fix
		//      a provider-side 500, so this is deliberately handled ONLY here in
		//      the controller and is NOT mapped to RateLimited in coop (that would
		//      trip is_auth_exhaustion → spurious failover + account churn).
		if state.ErrorCategory == "server_error" {
			logInit("Agent hit a transient server error, dismissing prompt")
			_ = coop.SendKeys(ctx, []string{"Return"})
			logInit("Server error info: %s", state.LastMessage)

			// Record that the agent is currently sitting on a Claude-upstream
			// outage so that, if a teardown (SIGTERM / max-restarts) fires before
			// it recovers, closeAgentBeadBestEffort attributes the death to the
			// upstream_api_error family rather than the incidental proximate cause
			// (ka-0000000202). Cleared on the next productive turn.
			recordUpstreamOutageObservation("server_error")

			// Sleep WITHOUT shutting down coop so the liveness probe (port 9090)
			// stays up during the backoff — same rationale as the rate-limit path.
			// State is left untouched (no dedicated agent_state pill for a
			// transient 500); a successful re-inject flips it back to working.
			sleepDur := serverErrorBackoff()
			logInit("Server error — sleeping %v before retry (coop health probe stays up)", sleepDur)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(sleepDur):
			}

			logInit("Server error backoff complete, re-injecting work prompt")
			// coop is stuck at state="error" from the 500/api_error and Nudge
			// cannot clear it — dismiss the error screen and drive a fresh turn
			// via raw PTY input (ka-0000006423).
			_ = recoverFromErrorAndNudge(ctx, coop)
			// Continue the monitoring loop (don't return).
		}

		// Detect a transient local connectivity drop (bare "connection refused" /
		// ECONNREFUSED / "connection reset" / DNS failure / timeout) and re-nudge
		// in place. coop classifies these as ErrorCategory::NoInternet and
		// surfaces error_category=no_internet on /api/v1/agent, but — like the
		// pre-fix server_error case — nothing acted on it: the agent sat on the
		// "Connection refused" error screen mid-session until the stale reaper
		// eventually killed it, losing all conversation context (ka-0000001822).
		//
		// The zombie watchdog above cannot catch this: it only fires before the
		// agent's FIRST work turn (!everWorked). Once a session is productive, a
		// mid-session connectivity blip has no recovery path at all.
		//
		// A bare connection-refused is canonically a transient local/network
		// hiccup (proxy mid-restart, brief DNS flap, node network blip) that
		// "usually clears in a moment", so the recovery is identical in SHAPE to
		// the server-error path (dismiss the error screen, sleep, re-inject the
		// work prompt, keep the liveness probe up) but DIFFERENT in two ways:
		//   1. Backoff cap is 5m — wider than server_error's 2m (a node/DNS
		//      outage can run longer than a provider 500) but far tighter than
		//      the rate-limit 30m cap (we want to resume promptly once the
		//      network returns, not idle for half an hour).
		//   2. NO profile rotation / auth failover. Rotating accounts cannot fix
		//      local connectivity, so — exactly like server_error — this is
		//      handled ONLY here in the controller and is deliberately NOT mapped
		//      to RateLimited in coop (that would trip is_auth_exhaustion →
		//      spurious failover + account churn).
		//
		// Note: API-WRAPPED transient bounces ("API Error: Connection error.",
		// "socket connection was closed", "API Error: terminated", "API Error:
		// Connection closed mid-response. The response above may be incomplete.")
		// are classified ServerError by coop and recover via the server_error
		// branch above (ka-0000006871 for the mid-response variant); only
		// a BARE connection error with no "API Error:" wrapper reaches here as
		// no_internet (see classify_error_detail in coop's error_category.rs).
		// (coop's parse_claude_state extracts the API-wrapped banner from the
		// isApiErrorMessage text block, since real Claude Code pairs it with a
		// coarse error:"unknown" sentinel that would otherwise classify as Other —
		// ka-0000003334.)
		if state.ErrorCategory == "no_internet" {
			logInit("Agent hit a transient connectivity error, dismissing prompt")
			_ = coop.SendKeys(ctx, []string{"Return"})
			logInit("Connectivity error info: %s", state.LastMessage)

			// Connectivity livelock bound (gd-fr3xjSxIws). A real network blip
			// clears within a few backoff cycles and a genuine turn zeroes the
			// streak. Reaching the bound with request_count never advancing means
			// the "connectivity" error is really a persistent failure that
			// re-injecting cannot clear, so give up loudly instead of looping while
			// the bead reads "working".
			noInternetLivelockStreak++
			if noInternetLivelockStreak >= noInternetLivelockBound {
				failAgentNoInternetLivelock(ctx, coop, noInternetLivelockStreak, state.LastMessage)
				return nil
			}

			// Same as the server_error branch: record the upstream outage so a
			// teardown before recovery attributes the death to upstream_api_error
			// (ka-0000000202). A bare local connectivity drop is still an
			// outage-driven loss of work from the agent's point of view.
			recordUpstreamOutageObservation("no_internet")

			// Sleep WITHOUT shutting down coop so the liveness probe (port 9090)
			// stays up during the backoff — same rationale as the rate-limit and
			// server-error paths. State is left untouched (no dedicated
			// agent_state pill for a transient network drop); a successful
			// re-inject flips it back to working.
			sleepDur := noInternetBackoff()
			logInit("Connectivity error — sleeping %v before retry (coop health probe stays up)", sleepDur)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(sleepDur):
			}

			logInit("Connectivity backoff complete, re-injecting work prompt")
			// coop is stuck at state="error" from the connectivity drop and
			// Nudge cannot clear it — dismiss the error screen and drive a fresh
			// turn via raw PTY input (ka-0000006423).
			_ = recoverFromErrorAndNudge(ctx, coop)
			// Continue the monitoring loop (don't return).
		}
	}
}

// failAgentRateLimitLivelock is the terminal give-up path for the rate-limit
// livelock bound (ka-0000001209). It fires when monitorExit has parked on the
// SAME account/model rate limit rateLimitLivelockBound times in a row with no
// successful turn in between — the signature of an account-monthly-spend cap
// that no re-inject, backoff, or LiteLLM fallback arm can clear (the exact class
// ka-0000007458's model-layer fix could not close). Rather than keep the
// probe-green livelock spinning silently, it degrades LOUDLY, mirroring the
// existing terminal watchdogs (hard-auth-403, terminal-login-required):
//
//  1. Requeue the claimed ISSUE task to the ready queue (reopenOrphanedClaimedTask)
//     so the genuinely-stalled work is picked up by a fresh agent — which gets a
//     fresh per-agent key and so is not itself spend-capped (advice ka-0000004373).
//     A failure the whole role shares (model, image, client version) follows
//     the fresh agent too, which is why the close_reason names the last error
//     instead of asserting a spend cap (gd-a0jdFgwngn).
//  2. Close the agent bead failed with an `_involuntary` close_reason in the
//     rate_limit_livelock family, so gardenio_agent_involuntary_deaths_total counts
//     it (family=rate_limit_livelock) and the Slack card names the cause honestly
//     instead of reading as an idle-out.
//  3. Shut down coop and return nil — the restart loop's
//     agentBeadClosedOutOfProcess sees the closed-with-reason bead and exits the
//     pod WITHOUT respawning against already-lost work.
//
// category is the coop error_category that tripped the bound ("rate_limited" or
// "model_rate_limited"); streak and lastMsg are folded into the audit trail.
func failAgentRateLimitLivelock(ctx context.Context, coop *coopapi.Client, category string, streak int, lastMsg string) {
	lastErr := strings.TrimSpace(lastMsg)
	logInit(
		"FATAL: rate-limit livelock — %d consecutive %s parks with no successful turn "+
			"(no re-inject or fallback cleared it); "+
			"requeuing the task and failing the agent instead of spinning green-but-dead. last: %s",
		streak, category, lastErr,
	)

	agentID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentID != "" && daemon != nil {
		// Best-effort, bounded — a wedged daemon must not deadlock the give-up.
		reopenCtx, reopenCancel := context.WithTimeout(context.Background(), 10*time.Second)
		reopened, lookupFailed := reopenOrphanedClaimedTask(reopenCtx, "rate_limit_livelock")
		reopenCancel()

		// Name the last error rather than asserting a spend cap: the same give-up
		// fires on any failure every retry shares, such as a permanent 400 coop
		// classed as model_rate_limited (gd-a0jdFgwngn, gd-XEGnlJpiGC).
		closeReason := "rate_limit_livelock_involuntary: " +
			fmt.Sprintf("gave up after %d consecutive %s parks with no successful turn", streak, category)
		if lastErr != "" {
			closeReason += "; last error: " + truncateRunes(lastErr, livelockLastErrorRunes)
		}
		if reopened != "" {
			closeReason += "; reopened " + reopened
		}
		if lookupFailed {
			closeReason += "; claim lookup failed, unverified"
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := daemon.CloseBead(closeCtx, agentID, map[string]string{
			"agent_state":  "failed",
			"close_reason": closeReason,
		}); err != nil {
			logInit("warning: failed to close agent bead on rate-limit livelock: %v", err)
		}
		closeCancel()
	}

	_ = coop.Shutdown(ctx)
}

// livelockLastErrorRunes caps the last error quoted in a rate-limit livelock
// close_reason, so a long API error body cannot bloat the bead or the card.
const livelockLastErrorRunes = 200

// failAgentTerminalAuth is the terminal auth-failure give-up (ka-0000001098).
// Symmetric to failAgentRateLimitLivelock, but for a non-recoverable CREDENTIAL
// failure — error_category=unauthorized (a 401/403 key ACL) or out_of_credits
// (a spent account) — that coop marks zombie without parking, so neither the
// hard-auth-403 nor the login-required watchdog catches it.
//
// The value it adds over "just let the K8s /livez probe SIGTERM the pod" is an
// HONEST death record. The generic SIGTERM teardown (init_agent_loop.go) stamps
// close_reason=context_cancelled_involuntary, whose auto-forensics report reads
// "pod was SIGTERM'd mid-turn (node drain, rollout, or Spot reclaim)". That is a
// false infra attribution — the real cause is a credential the agent never held,
// already visible as error_category on the same bead, on a cluster with zero Spot
// nodes. Closing here instead:
//  1. Requeues the claimed ISSUE task (reopenOrphanedClaimedTask) so genuinely
//     stalled work returns to the ready queue — a fresh agent gets a fresh key.
//  2. Closes the agent bead failed carrying BOTH a precise close_reason AND the
//     error_category / error_detail, so gb audit and the auto-forensics deathCause
//     name the credential failure rather than a phantom node drain. The reason is
//     deliberately NOT in the context_cancelled family, so it is subtracted from
//     the involuntary-death (node-churn) buckets the operator watches.
//  3. Shuts down coop and returns nil — the restart loop sees a closed-with-reason
//     bead and exits WITHOUT respawning against a credential that is still absent.
//
// category is the coop error_category that tripped the bound ("unauthorized" or
// "out_of_credits"); streak and lastMsg are folded into the audit trail.
func failAgentTerminalAuth(ctx context.Context, coop *coopapi.Client, category string, streak int, lastMsg string) {
	logInit(
		"FATAL: terminal auth failure — %d consecutive polls at error_category=%s with no successful turn "+
			"(a key ACL denial or a spent account: no re-inject or credential rotation can clear it); "+
			"requeuing the task and failing the agent instead of zombying until SIGTERM. last: %s",
		streak, category, strings.TrimSpace(lastMsg),
	)

	agentID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentID != "" && daemon != nil {
		// Best-effort, bounded — a wedged daemon must not deadlock the give-up.
		reopenCtx, reopenCancel := context.WithTimeout(context.Background(), 10*time.Second)
		reopened, lookupFailed := reopenOrphanedClaimedTask(reopenCtx, "terminal_auth_failure")
		reopenCancel()

		closeReason := "terminal_auth_failure: " +
			fmt.Sprintf("gave up after %d consecutive polls at error_category=%s (credential not usable — key ACL or spent account)", streak, category)
		if reopened != "" {
			closeReason += "; reopened " + reopened
		}
		if lookupFailed {
			closeReason += "; claim lookup failed, unverified"
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := daemon.CloseBead(closeCtx, agentID, map[string]string{
			"agent_state":    "failed",
			"close_reason":   closeReason,
			"error_category": category,
			"error_detail":   "authentication_failed",
		}); err != nil {
			logInit("warning: failed to close agent bead on terminal auth failure: %v", err)
		}
		closeCancel()
	}

	_ = coop.Shutdown(ctx)
}

// failAgentNoInternetLivelock is the give-up for a connectivity-recovery loop
// that never produces a turn (gd-fr3xjSxIws). Symmetric to
// failAgentRateLimitLivelock: requeue the claimed task, close the agent bead
// failed with a precise close_reason and the error category/detail, and shut coop
// down so the restart loop sees a closed-with-reason bead and exits instead of
// re-spawning into the same failure.
func failAgentNoInternetLivelock(ctx context.Context, coop *coopapi.Client, streak int, lastMsg string) {
	logInit(
		"FATAL: connectivity livelock — %d consecutive no_internet recoveries with no successful turn "+
			"(re-injecting cannot clear it); requeuing the task and failing the agent instead of "+
			"looping while reporting working. last: %s",
		streak, strings.TrimSpace(lastMsg),
	)

	agentID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentID != "" && daemon != nil {
		// Best-effort, bounded — a wedged daemon must not deadlock the give-up.
		reopenCtx, reopenCancel := context.WithTimeout(context.Background(), 10*time.Second)
		reopened, lookupFailed := reopenOrphanedClaimedTask(reopenCtx, "no_internet_livelock")
		reopenCancel()

		closeReason := "no_internet_livelock: " +
			fmt.Sprintf("gave up after %d consecutive no_internet recoveries with no successful turn", streak)
		if reopened != "" {
			closeReason += "; reopened " + reopened
		}
		if lookupFailed {
			closeReason += "; claim lookup failed, unverified"
		}
		fields := map[string]string{
			"agent_state":    "failed",
			"close_reason":   closeReason,
			"error_category": "no_internet",
		}
		if detail := strings.TrimSpace(lastMsg); detail != "" {
			fields["error_detail"] = detail
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := daemon.CloseBead(closeCtx, agentID, fields); err != nil {
			logInit("warning: failed to close agent bead on connectivity livelock: %v", err)
		}
		closeCancel()
	}

	_ = coop.Shutdown(ctx)
}

// monitorIdleSettle / monitorIdlePoll are the initial-settle and per-tick sleeps
// monitorIdle uses. They are vars (not consts) so tests can shrink them from the
// production 15s/10s to milliseconds; production behaviour is unchanged.
var (
	monitorIdleSettle = 15 * time.Second
	monitorIdlePoll   = 10 * time.Second
)

// monitorIdle polls agent state and updates the agent bead.
func monitorIdle(ctx context.Context, coop *coopapi.Client) {
	agentID := os.Getenv("KD_AGENT_ID")
	if agentID == "" || daemon == nil {
		return
	}

	time.Sleep(monitorIdleSettle)
	prevState := ""

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		time.Sleep(monitorIdlePoll)

		state, err := coop.GetAgentState(ctx)
		if err != nil {
			return // coop gone
		}

		if state.State != prevState && state.State != "" {
			// Skip state updates while in standby.
			if _, err := os.Stat("/tmp/standby_active"); err != nil {
				switch state.State {
				case "idle", "working":
					// Stop-latch guard (ka-0000006721): once the agent has been
					// LATCHED to a stop/suspend/terminal state — a closed bead, or
					// agent_state in {stopping, done, failed}, or stop_requested=true
					// (see agentStopLatched) — the live coop state ("idle"/"working")
					// must NOT be written back.
					// coop keeps reporting the SAME live session for the seconds/
					// minutes it takes the controller to freeze+tear down (or the
					// entrypoint to close) the pod; a blind overwrite un-latches the
					// stop the instant it is set. Observed prod loop (agent
					// ka-0000007854): every `gb stop` suspend set agent_state=stopping,
					// this poller flipped it back to idle ~10s later, and the podless
					// in_progress+idle bead was cold-restarted ~30min later replaying
					// the original trigger text — 30+ cycles over 12h, ~$30->$92 spend,
					// stop_requested never latched. Mirrors the same latch guards in
					// monitorIdleTTL (init_agent_idle_ttl.go) and the teardown path
					// (init_agent_loop.go). On a latched bead we STOP monitoring: this
					// pod's session is ending, so there is no further live state worth
					// reporting.
					if agentStopLatched(ctx, agentID) {
						logInit("monitor-idle: agent bead %s is stop-latched — halting live-state reporting (leaving the latch intact)", agentID)
						return
					}
					_ = daemon.UpdateAgentState(ctx, agentID, state.State)
				}
			}
			prevState = state.State
		}

		if state.State == "exited" {
			return
		}
	}
}

// idleMonitorLatchedStates are the NON-RECOVERABLE agent_state values that LATCH
// the agent into a stop/suspend/terminal disposition. Once monitorIdle sees the
// bead in one of these (or closed, or stop_requested=true) it must stop writing
// coop's live "idle"/"working" state — the pod is being suspended or torn down
// and any overwrite would defeat the latch (ka-0000006721). "stopping" is the
// frozen-awaiting-thaw suspend signal (suspendAgent, stop.go); "done"/"failed"
// are the terminal despawn states.
//
// Deliberately EXCLUDED: the RECOVERABLE in-place park states rate_limited /
// model_rate_limited / budget_exceeded. The in-session loop (init_agent.go /
// auth-failover) owns those — it parks the SAME pod, then flips it back to idle
// and re-injects the work prompt on recovery. Latching on them would make
// monitorIdle exit permanently on a transient park, leaving the card frozen
// after the agent resumes. The bug this guard fixes is the un-latching of a
// non-recoverable stop, so the set is exactly the non-recoverable dispositions.
var idleMonitorLatchedStates = map[string]bool{
	"stopping": true,
	"done":     true,
	"failed":   true,
}

// agentStopLatched reports whether the agent bead has been latched to a
// stop/suspend/terminal disposition that monitorIdle must not overwrite: a
// closed bead, stop_requested=true, or an agent_state in
// idleMonitorLatchedStates. A GetBead error is treated as NOT latched
// (fail-open) so a transient daemon blip never wedges live-state reporting for a
// healthy working agent — the guard exists to protect a real latch, not to
// silence a flaky read.
func agentStopLatched(ctx context.Context, agentID string) bool {
	b, err := daemon.GetBead(ctx, agentID)
	if err != nil || b == nil {
		return false
	}
	if b.Status == "closed" {
		return true
	}
	if b.Fields["stop_requested"] == "true" {
		return true
	}
	return idleMonitorLatchedStates[b.Fields["agent_state"]]
}

// oauthCredentials holds the Claude AI OAuth credentials file structure.
type oauthCredentials struct {
	ClaudeAiOauth *oauthTokenData `json:"claudeAiOauth,omitempty"`
}

type oauthTokenData struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

// refreshCredentials handles OAuth token refresh in a loop.
func refreshCredentials(ctx context.Context, coop *coopapi.Client) {
	// Skip if API key mode (no OAuth credentials to refresh).
	credsFile := os.Getenv("XDG_STATE_HOME")
	if credsFile == "" {
		credsFile = os.Getenv("HOME") + "/.local/state"
	}
	credsFile += "/claude-code/.credentials.json"

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey != "" {
		if _, err := os.Stat(credsFile); os.IsNotExist(err) {
			logInit("API key mode — skipping OAuth refresh loop")
			return
		}
	}

	time.Sleep(30 * time.Second) // let Claude start first
	consecutiveFailures := 0
	maxFailures := 5

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		time.Sleep(5 * time.Minute)

		data, err := os.ReadFile(credsFile)
		if err != nil {
			continue
		}

		var creds oauthCredentials
		if err := json.Unmarshal(data, &creds); err != nil || creds.ClaudeAiOauth == nil {
			continue
		}

		token := creds.ClaudeAiOauth
		if token.RefreshToken == "" || token.ExpiresAt == 0 {
			continue
		}

		// Coop-provisioned credentials use a sentinel expiresAt (>= 10^12 ms).
		if token.ExpiresAt >= 9999999999000 {
			consecutiveFailures = 0
			continue
		}

		// Check if within 1 hour of expiry (3600000ms).
		nowMs := time.Now().UnixMilli()
		remainingMs := token.ExpiresAt - nowMs
		if remainingMs > 3600000 {
			consecutiveFailures = 0
			continue
		}

		logInit("OAuth token expires in %dm, refreshing...", remainingMs/60000)

		newAccess, newRefresh, expiresIn, err := doOAuthRefresh(ctx, token.RefreshToken)
		if err != nil {
			consecutiveFailures++
			logInit("WARNING: OAuth refresh failed (attempt %d/%d): %v", consecutiveFailures, maxFailures, err)
			if consecutiveFailures >= maxFailures {
				if shouldTerminateOnAuthFailure(ctx, coop) {
					logInit("FATAL: OAuth refresh failed %d consecutive times, terminating pod", maxFailures)
					syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
					return
				}
				consecutiveFailures = 0
			}
			continue
		}

		consecutiveFailures = 0
		newExpiresAt := time.Now().UnixMilli() + int64(expiresIn)*1000

		// Update credentials file.
		creds.ClaudeAiOauth.AccessToken = newAccess
		creds.ClaudeAiOauth.RefreshToken = newRefresh
		creds.ClaudeAiOauth.ExpiresAt = newExpiresAt

		updatedData, err := json.MarshalIndent(creds, "", "  ")
		if err != nil {
			logInit("WARNING: failed to marshal updated credentials: %v", err)
			continue
		}

		tmpFile := credsFile + ".tmp"
		if err := os.WriteFile(tmpFile, updatedData, 0600); err != nil {
			logInit("WARNING: failed to write credentials: %v", err)
			continue
		}
		if err := os.Rename(tmpFile, credsFile); err != nil {
			logInit("WARNING: failed to rename credentials: %v", err)
			continue
		}

		logInit("OAuth credentials refreshed (expires in %dh)", expiresIn/3600)
	}
}

// oauthRefreshRequest is the request body for token refresh.
type oauthRefreshRequest struct {
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
}

// oauthRefreshResponse is the response from the token endpoint.
type oauthRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// doOAuthRefresh performs the actual token refresh HTTP request.
func doOAuthRefresh(ctx context.Context, refreshToken string) (accessToken, newRefreshToken string, expiresIn int, err error) {
	reqBody := oauthRefreshRequest{
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
		ClientID:     oauthClientID,
	}

	bodyData, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(string(bodyData)))
	if err != nil {
		return "", "", 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var tokenResp oauthRefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", "", 0, fmt.Errorf("decode response: %w", err)
	}

	if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" {
		return "", "", 0, fmt.Errorf("response missing tokens")
	}

	return tokenResp.AccessToken, tokenResp.RefreshToken, tokenResp.ExpiresIn, nil
}

// shouldTerminateOnAuthFailure checks if the agent is still active (don't kill a working agent).
func shouldTerminateOnAuthFailure(ctx context.Context, coop *coopapi.Client) bool {
	state, err := coop.GetAgentState(ctx)
	if err != nil {
		return true
	}
	if state.State == "working" || state.State == "idle" {
		logInit("WARNING: OAuth refresh failing but agent is %s, not terminating", state.State)
		return false
	}
	return true
}

// isSessionAlive checks whether the agent reached an active state (idle or
// working). Called after startup bypass and prompt injection to detect zombie
// sessions where Claude failed to start (e.g. missing credentials). A brief
// grace period accounts for hook execution between injectPrompt and the first
// idle/working poll.
func isSessionAlive(ctx context.Context, coop *coopapi.Client) bool {
	// injectPrompt already waited up to 120s for idle/working. If we're here
	// and the agent is still not active, give a short grace period (30s) for
	// late-arriving credentials or slow startup, then give up.
	gracePeriod := sessionAliveGrace
	if gracePeriod == 0 {
		gracePeriod = 30 * time.Second
	}
	pollInterval := sessionAlivePoll
	if pollInterval == 0 {
		pollInterval = 5 * time.Second
	}

	deadline := time.Now().Add(gracePeriod)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		state, err := coop.GetAgentState(ctx)
		if err != nil {
			return false // coop gone
		}
		if state.State == "working" {
			return true
		}
		if state.State == "idle" {
			// Idle is normally "alive". But the credential-not-ready-at-spawn
			// failure (ka-0000007394) reaches idle with a persistent
			// "Not logged in · Please run /login" banner: coop's screen
			// classifier returns Idle the moment it sees the `❯` input prompt,
			// BEFORE its login-banner detection, so a credential-less agent
			// looks idle-alive and would zombie here until the slow never-worked
			// watchdog (or never, on a short-lived pod). Treat idle-with-login-
			// banner as NOT yet alive and keep polling: a transient
			// credential-switch banner clears within the grace window and the
			// next poll returns true; a genuine no-credential banner persists,
			// the grace window elapses, and we return false so coop is shut down
			// with a clear cause instead of nudging a dead agent.
			if !sessionLoginBlocked(ctx, coop, state) {
				return true
			}
			logInit("agent idle but a login banner is present — credentials may not be ready, continuing to poll")
		}
		if state.State == "exited" {
			return false
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(pollInterval):
		}
	}
	return false
}

// loginBannerPhrases are the case-insensitive screen substrings that indicate
// Claude Code has no usable credential ("Not logged in · Please run /login" and
// kin). Shared by bypassStartup's early-startup guard and isSessionAlive's
// credential-ready gate so both layers recognize the same banner (ka-0000007394).
var loginBannerPhrases = []string{
	"please sign in",
	"not logged in",
	"login required",
	"sign in at",
	"please run /login",
}

// screenHasAnyPhrase reports whether screen contains any of phrases
// (case-insensitive).
func screenHasAnyPhrase(screen string, phrases []string) bool {
	for _, phrase := range phrases {
		if containsCI(screen, phrase) {
			return true
		}
	}
	return false
}

// screenShowsLoginBanner reports whether coop's current screen text contains a
// login-required banner. Best-effort: a screen-fetch error returns false (treat
// as no banner) so a transient API hiccup never trips the credential gate.
func screenShowsLoginBanner(ctx context.Context, coop *coopapi.Client) bool {
	// loginBannerPhrases are Claude TUI text; a codex pod has no screen, and its
	// auth failures surface as driver error events instead.
	if !harness.ClaudeDriver(harness.MustFromEnv(), os.Getenv("BOAT_COMMAND")) {
		return false
	}
	screen, err := coop.GetScreenText(ctx)
	if err != nil {
		return false
	}
	return screenHasAnyPhrase(screen, loginBannerPhrases)
}

// sessionLoginBlocked reports whether an otherwise-idle session is actually
// blocked on a login banner — either coop already classified the screen as a
// `startup_login` setup prompt, or the raw screen text carries a login banner
// (the common case, since the screen classifier returns Idle for the `❯` prompt
// before it ever reaches login detection).
func sessionLoginBlocked(ctx context.Context, coop *coopapi.Client, state *coopapi.AgentState) bool {
	if state != nil && state.Prompt != nil && state.Prompt.Subtype == "startup_login" {
		return true
	}
	return screenShowsLoginBanner(ctx, coop)
}

// sessionAliveGrace overrides the grace period in isSessionAlive for tests.
var sessionAliveGrace time.Duration

// sessionAlivePoll overrides the poll interval in isSessionAlive for tests.
var sessionAlivePoll time.Duration

// containsCI performs a case-insensitive substring search.
func containsCI(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// envDuration reads a duration from an environment variable. It accepts either
// a Go duration string ("500ms", "5s", "24h") or a bare integer count of
// seconds ("30" == 30s) for backward compatibility.
//
// The Go-duration form is tried FIRST because the controller writes several of
// these vars as time.Duration.String() — e.g. podspec.go stamps
// BOAT_STANDBY_POLL = cfg.StandbyPoll.String(), whose warm-pool default is
// "500ms". The old integer-only parser (fmt.Sscanf "%d") read the leading digits
// of "500ms" as 500 *seconds*, turning the deliberately sub-second standby poll
// (ka-0000003812) into a 500s poll and silently defeating warm-pool spawn
// latency (ka-0000000001). Whole-second values like "5s"/"30s" happened to
// survive that bug (Sscanf read the leading int); sub-second/fractional values
// did not. time.ParseDuration handles both correctly.
//
// The bare-integer fallback preserves the historical contract for any caller or
// operator override that still sets a plain seconds count (e.g. "30").
func envDuration(key string, defaultVal time.Duration) time.Duration {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return defaultVal
	}
	// Preferred: a Go duration string (units required), e.g. "500ms", "5s".
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	// Backward-compat: a bare integer is interpreted as seconds.
	var secs int
	if _, err := fmt.Sscanf(s, "%d", &secs); err != nil {
		return defaultVal
	}
	return time.Duration(secs) * time.Second
}

// shouldExitBeforeStart checks the agent bead for stop_requested=true before
// starting the lifecycle. This prevents the restart loop where Claude calls
// gb stop, coop exits cleanly (code 0), kubelet restarts the container, and
// gb init re-injects the prompt — causing an infinite stop/restart cycle.
func shouldExitBeforeStart(ctx context.Context) bool {
	agentBeadID := os.Getenv("KD_AGENT_ID")
	if agentBeadID == "" {
		agentBeadID = os.Getenv("BOAT_AGENT_BEAD_ID")
	}
	if agentBeadID == "" {
		return false
	}
	// The local stop sentinel (written by a gb stop whose stop_requested write
	// failed) survives a container restart in the pod-scoped /tmp, so it counts
	// here too. If the close is then denied, park rather than exit: exiting only
	// brings the container back (gd-aoSsfpwPGG).
	if stopSentinelSet(agentBeadID) {
		closeOrPark(ctx, agentBeadID)
		return true
	}
	if daemon == nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	bead, err := daemon.GetBead(checkCtx, agentBeadID)
	if err != nil {
		return false
	}
	if bead.Fields["stop_requested"] == "true" {
		// Close the bead so the reconciler stops tracking this pod.
		closeOrPark(ctx, agentBeadID)
		return true
	}
	return false
}

// zombieWatchdogTimeout overrides the zombie watchdog deadline in monitorExit for tests.
var zombieWatchdogTimeout time.Duration

// progressStampInterval is the minimum gap between consecutive last_progress_at
// stamps (ka-0000000286). The stamp feeds a 90m staleness guard
// (beadsapi.ProgressStaleThreshold), so a 5m throttle keeps the signal an order
// of magnitude fresher than the guard needs while collapsing the per-turn write
// rate (~1/1.3min median) to at most 1/5min — one field patch, one SSE event.
// It is a var so tests can shrink it to zero and assert a stamp on every turn.
var progressStampInterval = 5 * time.Minute

// rateLimitBackoffOverride is set by tests to skip the 30-minute production wait.
var rateLimitBackoffOverride time.Duration

// rateLimitLivelockBound is how many CONSECUTIVE rate-limit park cycles (account
// rate_limited OR model_rate_limited) with no successful turn in between
// monitorExit tolerates before it gives up and fails the agent (ka-0000001209).
// A transient / recoverable limit clears well inside a handful of parks (a real
// turn lands, request_count advances, the streak zeroes), so reaching this many
// back-to-back parks with request_count never advancing means the limit is a
// hard account-monthly-spend cap that no re-inject or fallback arm can clear —
// the exact livelock ka-0000007458's model-layer fix could not fix.
//
// Wall-clock to give-up: the inter-park sleep is the rateLimitNoHintStreak /
// modelRateLimitStreak exponential backoff (base 1m → 30m cap), which, like
// this streak, resets only when request_count advances. So 8 parks with no
// genuine turn take roughly 1+2+4+8+16+30+30+30 ≈ 2h of backoff. Long enough to
// ride out a genuinely long-but-recoverable throttle, short enough that a stuck
// pod requeues its work and frees its crew slot instead of holding it dead for
// 11-14h. The parks do not count toward the 10m never-worked watchdog, which
// used to restart coop before the bound could trip (gd-a0jdFgwngn).
// Overridable for tests.
var rateLimitLivelockBound = 8

// rateLimitLivelockStreak counts CONSECUTIVE rate-limit parks with no genuine
// turn in between (see monitorExit). It is package-level, like the backoff
// streaks, so it survives coop restarts in the same pod: when it lived inside
// monitorExit, every never-worked restart zeroed it and the bound could only
// trip by luck (gd-a0jdFgwngn). Reset only when request_count advances.
var rateLimitLivelockStreak int

// noInternetLivelockBound is how many CONSECUTIVE no_internet recoveries with no
// genuine turn in between monitorExit tolerates before it fails the agent
// (gd-fr3xjSxIws). With the jittered 5s→5m backoff, 10 hits span roughly 25
// minutes of a sustained outage — long enough to ride out a real network/proxy
// blip, short enough that a mislabelled persistent failure (Codex 401 as
// httpConnectionFailed) frees the slot and requeues the task. Overridable for
// tests.
var noInternetLivelockBound = 10

// monitorSleepOverride replaces both the initial (10s) and poll (5s) sleeps in
// monitorExit when non-zero. Used by tests to avoid multi-second waits.
var monitorSleepOverride time.Duration

const (
	// rateLimitBackoffBase is the starting backoff for the first no-hint
	// rate-limit hit; it doubles on each consecutive hit.
	rateLimitBackoffBase = time.Minute
	// rateLimitBackoffCap caps the no-hint exponential backoff ceiling.
	rateLimitBackoffCap = 30 * time.Minute
)

// rateLimitNoHintStreak counts consecutive rate-limit hits that carry NO reset
// hint (the account-spend-limit case has no reset time). It drives the
// exponential growth in rateLimitBackoff and is reset to zero when the agent
// recovers (see monitorExit) or when a reset-hint rate-limit is observed.
var rateLimitNoHintStreak int

// rateLimitBackoff returns how long to sleep when rate-limited.
//
// When the message carries a reset hint (e.g. "resets 9pm"), parseResetDuration
// is used and we sleep until just past that boundary — unchanged behavior.
//
// When there is NO reset hint — the account-spend-limit case has no reset time —
// we fall back to a JITTERED EXPONENTIAL backoff keyed on the number of
// consecutive no-hint hits: base 1m doubling up to a 30m cap, with equal jitter
// so the fleet does not retry in lockstep (thundering herd).
func rateLimitBackoff(msg string) time.Duration {
	if rateLimitBackoffOverride != 0 {
		return rateLimitBackoffOverride
	}
	if d := parseResetDuration(msg); d > 0 {
		// Reset hint present: a different rate-limit regime — clear the
		// no-hint streak and sleep until just past the reset boundary.
		rateLimitNoHintStreak = 0
		return d + time.Minute // buffer past the reset boundary
	}

	// No reset hint: jittered exponential backoff.
	rateLimitNoHintStreak++
	return jitteredExpBackoff(rateLimitNoHintStreak, rateLimitBackoffBase, rateLimitBackoffCap)
}

// serverErrorBackoffOverride is set by tests to skip the production server-error wait.
var serverErrorBackoffOverride time.Duration

const (
	// serverErrorBackoffBase is the starting backoff for the first transient
	// server-error hit; it doubles on each consecutive hit.
	serverErrorBackoffBase = 5 * time.Second
	// serverErrorBackoffCap caps the server-error exponential backoff ceiling.
	// A 500/overloaded clears in seconds-to-low-minutes, so the cap is far
	// tighter than the rate-limit cap (30m) — we want to resume promptly once
	// the upstream recovers, not sit idle for half an hour.
	serverErrorBackoffCap = 2 * time.Minute
)

// serverErrorStreak counts consecutive transient server-error hits. It drives
// the exponential growth in serverErrorBackoff and is reset to zero when the
// agent recovers to "working" (see monitorExit), so an isolated 500 always
// retries fast (base 5s) and only a sustained upstream outage backs off toward
// the cap.
var serverErrorStreak int

// serverErrorBackoff returns how long to sleep after a transient upstream
// server error (HTTP 500 / api_error / overloaded / 502 / 503) before
// re-injecting the work prompt. Jittered exponential backoff (base 5s doubling
// to a 2m cap) so a brief blip retries almost immediately while a sustained
// outage does not hammer the upstream in lockstep across the fleet.
func serverErrorBackoff() time.Duration {
	if serverErrorBackoffOverride != 0 {
		return serverErrorBackoffOverride
	}
	serverErrorStreak++
	return jitteredExpBackoff(serverErrorStreak, serverErrorBackoffBase, serverErrorBackoffCap)
}

// noInternetBackoffOverride is set by tests to skip the production connectivity wait.
var noInternetBackoffOverride time.Duration

const (
	// noInternetBackoffBase is the starting backoff for the first transient
	// connectivity (no_internet) hit; it doubles on each consecutive hit.
	noInternetBackoffBase = 5 * time.Second
	// noInternetBackoffCap caps the connectivity exponential backoff ceiling.
	// A bare connection-refused / DNS flap can run longer than a provider 500
	// (e.g. a node network blip or proxy restart), so the cap is wider than the
	// server-error cap (2m) — but far tighter than the rate-limit cap (30m): we
	// want to resume promptly once the network returns, not idle for half an
	// hour on a blip that cleared in seconds.
	noInternetBackoffCap = 5 * time.Minute
)

// noInternetStreak counts consecutive transient connectivity hits. It drives
// the exponential growth in noInternetBackoff and is reset to zero when the
// agent recovers to "working" (see monitorExit), so an isolated drop always
// retries fast (base 5s) and only a sustained outage backs off toward the cap.
var noInternetStreak int

// noInternetBackoff returns how long to sleep after a transient local
// connectivity drop (bare connection refused / ECONNREFUSED / connection reset
// / DNS failure / timeout) before re-injecting the work prompt. Jittered
// exponential backoff (base 5s doubling to a 5m cap) so a brief blip retries
// almost immediately while a sustained network outage does not hammer the
// upstream in lockstep across the fleet.
func noInternetBackoff() time.Duration {
	if noInternetBackoffOverride != 0 {
		return noInternetBackoffOverride
	}
	noInternetStreak++
	return jitteredExpBackoff(noInternetStreak, noInternetBackoffBase, noInternetBackoffCap)
}

// upstreamOutageObservedAt records, as unix-nanos, when monitorExit last saw the
// agent sitting on a Claude-upstream outage (error_category server_error or
// no_internet). Reset to 0 by a productive "working" turn that proves the outage
// cleared. The restart-loop teardown (closeAgentBeadBestEffort,
// init_agent_loop.go) reads it to attribute an outage-driven death to the
// upstream_api_error death family rather than to the incidental proximate cause
// (context_cancelled from a SIGTERM during the outage, or max_restarts_reached
// when the loop gave up) — the misattribution ka-0000000202 fixes.
//
// Atomic because monitorExit writes it from the per-session lifecycle goroutine
// while the supervisor loop reads it on teardown; the two can overlap on the
// ctx-cancelled teardown path.
var upstreamOutageObservedAt atomic.Int64

// upstreamOutageCategory is the coop error_category string of the most recent
// upstream-outage observation (server_error / no_internet), so the stamped
// close_reason can name which shape was active.
var upstreamOutageCategory atomic.Pointer[string]

// upstreamOutageStaleWindow bounds how recent the last upstream observation must
// be for a teardown to attribute the death to an upstream outage. Sized well
// above the server-error (2m) and no-internet (5m) backoff caps so an agent
// still mid-backoff on a sustained outage always counts, yet small enough that a
// stale observation from a blip that cleared long before an unrelated later
// death is discarded (the false-positive guard, complementing the reset-on-work
// clear). Overridable for tests.
var upstreamOutageStaleWindow = 10 * time.Minute

// recordUpstreamOutageObservation marks that the agent is currently sitting on a
// Claude-upstream outage of the given coop error_category. Called from the
// monitorExit server_error / no_internet branches on every observation while the
// agent is stuck, so the timestamp tracks the LAST time the outage was seen.
func recordUpstreamOutageObservation(category string) {
	upstreamOutageObservedAt.Store(time.Now().UnixNano())
	c := category
	upstreamOutageCategory.Store(&c)
}

// clearUpstreamOutageObservation forgets any prior upstream-outage observation.
// Called on a productive "working" turn (the outage cleared and the agent is
// making progress again) so a later, unrelated death is not misattributed.
func clearUpstreamOutageObservation() {
	upstreamOutageObservedAt.Store(0)
}

// upstreamOutageActive reports whether a Claude-upstream outage was observed
// recently enough (within upstreamOutageStaleWindow) to attribute a death to it,
// and the coop error_category string of that observation.
func upstreamOutageActive() (category string, active bool) {
	at := upstreamOutageObservedAt.Load()
	if at == 0 {
		return "", false
	}
	if time.Since(time.Unix(0, at)) > upstreamOutageStaleWindow {
		return "", false
	}
	if p := upstreamOutageCategory.Load(); p != nil {
		category = *p
	}
	return category, true
}

// modelRateLimitBackoffOverride is set by tests to skip the production wait.
var modelRateLimitBackoffOverride time.Duration

const (
	// modelRateLimitBackoffBase is the starting backoff for the first
	// model-group (pool-exhaustion) rate-limit hit; it doubles per consecutive
	// hit.
	modelRateLimitBackoffBase = time.Minute
	// modelRateLimitBackoffCap caps the model-group rate-limit exponential
	// backoff ceiling. A pool-exhaustion / spend-cap model throttle clears on
	// the upstream's own (often multi-minute to hourly) schedule, and rotation
	// cannot shorten it, so the cap matches the account rate-limit cap (30m)
	// rather than the tighter server-error (2m) / connectivity (5m) caps — we
	// must not hammer an exhausted model group in lockstep across the fleet.
	modelRateLimitBackoffCap = 30 * time.Minute
)

// modelRateLimitStreak counts consecutive model-group (pool-exhaustion)
// rate-limit hits. It drives the exponential growth in modelRateLimitBackoff
// and is reset to zero when the agent recovers to "working" (see monitorExit),
// so an isolated model throttle retries reasonably soon (base 1m) and only a
// sustained pool exhaustion backs off toward the 30m cap.
var modelRateLimitStreak int

// modelRateLimitBackoff returns how long to sleep after a model-group
// (pool-exhaustion, no-fallback) rate limit before re-injecting the work
// prompt, WITHOUT rotating credentials (rotation cannot clear a model-level
// throttle — every pooled profile serves the same model). Jittered exponential
// backoff (base 1m doubling to a 30m cap) so a brief throttle resumes promptly
// while a sustained pool exhaustion does not hammer the upstream in lockstep
// across the fleet. See the model_rate_limited branch in monitorExit and
// ka-0000001718.
func modelRateLimitBackoff() time.Duration {
	if modelRateLimitBackoffOverride != 0 {
		return modelRateLimitBackoffOverride
	}
	modelRateLimitStreak++
	return jitteredExpBackoff(modelRateLimitStreak, modelRateLimitBackoffBase, modelRateLimitBackoffCap)
}

// jitteredExpBackoff computes an exponentially-growing, capped, jittered backoff
// for the Nth consecutive hit (attempt >= 1) given a base and cap.
//
// The deterministic ceiling for attempt N is min(base*2^(N-1), cap). We then
// apply "equal jitter": half the ceiling is fixed and the other half is random,
// so the returned value lies in [ceil/2, ceil]. This spreads fleet-wide retries
// across the window while guaranteeing a meaningful minimum wait.
func jitteredExpBackoff(attempt int, base, maxBackoff time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ceil := maxBackoff
	// base * 2^(attempt-1), guarding against shift overflow. Once the scaled
	// value would exceed the cap we simply keep the cap.
	if shift := attempt - 1; shift >= 0 && shift < 30 {
		if scaled := base << shift; scaled > 0 && scaled < ceil {
			ceil = scaled
		}
	}
	half := ceil / 2
	// Equal jitter: random offset in [0, half] added to the fixed half →
	// final value in [ceil/2, ceil].
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func logInit(format string, args ...interface{}) {
	log.Printf("[gb init] "+format, args...)
}
