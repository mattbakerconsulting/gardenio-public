package main

// init_agent_loop.go — coop process management and restart loop for gb init.
//
// gb init owns the full agent pod lifecycle: it starts coop, manages the
// restart loop, registers/closes session beads, and runs per-session lifecycle
// goroutines (startup bypass, prompt injection, exit/idle monitoring, OAuth refresh).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
	"gardenio/controller/internal/harness"
)

// exitCodeCredentialSwitch is the exit code coop uses to signal that a
// credential switch was requested.  gb init reads the switch metadata file
// and restarts coop with the new credentials and the correct --resume flag.
//
// Aliased to the beadsapi constant rather than re-declared: the session-status
// classifier there has to agree that this code is a healthy restart and not a
// crash (ka-0000003165), and two independent literals could drift apart.
const exitCodeCredentialSwitch = beadsapi.ExitCodeCredentialSwitch

// credentialSwitchFile is the well-known path where coop writes switch
// metadata before exiting with exitCodeCredentialSwitch.
const credentialSwitchFile = "/tmp/coop_credential_switch.json"

// lastCoopExitCode/lastCoopExitKnown record the exit code of the most
// recently completed coop session so closeAgentBeadBestEffort can stamp it
// durably on the AGENT bead's own fields (gd-k3kzZeIlT0 Ask 1). Today that
// evidence exists only on the linked SESSION bead (registerSessionBead /
// closeSessionBead) — reconstructing "was this a SIGTERM?" requires an
// investigator to cross-reference a session_of dependency by hand, and if the
// pod is deleted before that cross-reference happens the terminal agent bead
// itself gives no hint at all. Package-level rather than threaded through
// closeAgentBeadBestEffort's signature: that function has ~20 call sites
// across tests and production code, and the exit code is only ever known
// within this same loop goroutine that also calls closeAgentBeadBestEffort.
var (
	lastCoopExitCode  atomic.Int32
	lastCoopExitKnown atomic.Bool
)

// describeCoopExitCode renders a coop process exit code as a short
// human-readable label, reusing the 128+signal convention already applied by
// beadsapi.SessionStatusForExit.
func describeCoopExitCode(code int) string {
	switch code {
	case 0:
		return "clean exit"
	case exitCodeCredentialSwitch:
		return "credential switch"
	case 143: // 128 + SIGTERM
		return "SIGTERM"
	case 130: // 128 + SIGINT
		return "SIGINT"
	case 129: // 128 + SIGHUP
		return "SIGHUP"
	default:
		return fmt.Sprintf("exit code %d", code)
	}
}

// addLastCoopExitFields stamps the most recently observed coop exit code onto
// fields, when one has been recorded. No-op before the first coop session of
// this pod has exited (e.g. a pod that never got past startup).
func addLastCoopExitFields(fields map[string]string) {
	if !lastCoopExitKnown.Load() {
		return
	}
	code := int(lastCoopExitCode.Load())
	fields["last_coop_exit_code"] = strconv.Itoa(code)
	fields["last_coop_exit_reason"] = describeCoopExitCode(code)
}

// initConfig holds configuration for the gb init restart loop.
// Values are read from environment variables set by the pod manager.
type initConfig struct {
	coopPort        int
	coopHealthPort  int
	coopMetricsPort int
	maxRestarts     int
	command         string // agent command (e.g. "claude --dangerously-skip-permissions")
	role            string
	project         string
	agent           string
	workspace       string // scaffold workspace path
	harness         string // resolved agent CLI: claude | codex | gemini (GARDENIO_HARNESS)
	claudeStateDir  string // resolved ~/.claude path (PVC-backed)
	coopStateDir    string // resolved .state/coop path
	podIP           string
	hostname        string
	standby         bool
	mockMode        bool
}

// runInitLoop is the main restart loop. It starts coop, manages sessions,
// and restarts on exit. This replaces the bash restart loop in entrypoint.sh.
func runInitLoop(ctx context.Context, cfg initConfig) error {
	// ── Mux registration (survives coop restarts) ────────────────────────
	mux := newMuxClient()
	coopURL := fmt.Sprintf("http://%s:%d", cfg.podIP, cfg.coopPort)
	go func() {
		if err := mux.Register(ctx, cfg.hostname, coopURL, cfg.role, cfg.agent, cfg.project, cfg.hostname, cfg.podIP); err != nil {
			logInit("mux register error: %v", err)
		}
	}()
	defer mux.Deregister(cfg.hostname)

	// ── gb-side thaw watcher (ConsumerGB) ────────────────────────────────
	// On gVisor agent pods (COOP_THAW_STATE_DIR set), arm thaw-epoch.gb.armed at
	// boot and ack thaw-epoch.gb.done at thaw so the controller's {gb,coop}
	// rehydrate gate can release. Mirrors the runLocal call site (agent_start.go);
	// a no-op on non-gVisor pods. See agent_start_thaw.go and bug ka-0000004330.
	startK8sGBThawWatcher(
		ctx,
		envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID")),
		cfg.agent,
		resolveCoopWorkdir(initToK8sConfig(cfg)),
	)

	// GitHub credential refresh: keep ~/.git-credentials and gh's hosts.yml
	// in step with the rotating projected token (gd-uPJ6AEsfyn, gd-jfo7R99vgj).
	startGitCredsRefresh(ctx, logInit)

	// Block until coopmux has been seeded with credentials by the controller's
	// initial syncAccountsToCoopmux pass. Without this gate, the controller's
	// first set_token call after coop registers triggers a distributor switch
	// that kills the in-flight Claude session — see ka-0000006291.
	if !cfg.mockMode {
		if muxURL := os.Getenv("COOP_MUX_URL"); muxURL != "" {
			waitForCoopmuxCredentials(ctx, muxURL, os.Getenv("BOAT_ACCOUNT"), 90*time.Second)
		}
	}

	// Auto-restore salvaged WIP (epic ka-0000005916 step B2 ka-0000003478): a
	// prior incarnation killed mid-work salvaged its uncommitted / committed-but-
	// unpushed WIP to S3 via the preStop `gb salvage`. The successor pod re-cloned
	// fresh (setup ran before runInitLoop) and would otherwise redo that work from
	// the restored transcript's memory. Fetch the salvage bundles back into the
	// local repo(s) under refs/salvage/* and drop a recovery note into the system
	// prompt so the agent cherry-picks instead of starting over. Runs ONCE per
	// boot before the first coop launch; best-effort, time-boxed, always-continue,
	// and a quiet no-op on a fresh spawn (gated on s3ResumeIntended).
	autoRestoreSalvageOnRespawn(ctx, cfg)

	const minRuntimeSecs = 30
	restarts := 0

	// Stdout-tail capture (ka-0000007441): retain the last 256 kB of coop's
	// stdout/stderr per session, plus a fast-crash detector. On fast-crash,
	// context_cancelled, or max_restarts the tail is shipped to S3 alongside
	// session metadata so post-mortems have something to read.
	tailBuf := newTailBuffer(stdoutTailBytes)
	crashes := newCrashTracker(fastCrashThreshold, fastCrashWindow, fastCrashLookback)
	// Durable anti-flap for in-session credential rotation. coop's own switch
	// cap is wiped on every exit(10) restart, so the restart-surviving backoff
	// lives here. See init_agent_switch_backoff.go / ka-0000005014.
	switches := newSwitchTracker(switchFlapThreshold, switchFlapWindow, switchBackoffBase, switchBackoffMax, switchTerminalCap)
	// Durable startup-login failure budget for the credential-not-ready-at-spawn
	// wedge (ka-0000007536). coop's in-session LoginRequiredWatchdog resets on
	// every restart, so a pod that keeps dying on the STARTUP login banner never
	// trips it; this restart-surviving counter closes the bead not_logged_in
	// after a bounded number of consecutive login-blocked startups instead of
	// leaning on maxRestarts (which the original incident never reached).
	loginFailures := newLoginFailureTracker(startupLoginFailureBudget)
	// Original COOP_PROFILE mode, captured so the terminal-cap park (which sets
	// COOP_PROFILE=manual to stop the exit-10 rotation flap) can be UNDONE once a
	// productive session proves the flap resolved — otherwise a one-off flap
	// would permanently disable legitimate multi-account failover
	// (ka-0000001718 Part B).
	origCoopProfile := os.Getenv("COOP_PROFILE")
	var currentSessionID atomic.Pointer[string]

	// Mail subscriber (spec §3.1): this pod's agent receives its nudges and
	// control:kill as mail, delivered to coop on loopback. It runs across
	// coop restarts; coop sessions run under mail.sessionCtx so a kill stops
	// the current one. See init_agent_mail.go.
	mail := startPodMail(ctx, initMailDaemon(), mailCoop(coopapi.New(fmt.Sprintf("http://localhost:%d", cfg.coopPort)), cfg.command),
		envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID")), cfg.agent, logInit)
	defer mail.stop()
	exitOnMailKill := func() bool {
		if !mail.killRequested() {
			return false
		}
		logInit("control:kill mail — closing agent bead and exiting without restart")
		closeAgentBeadOnMailKill(logInit)
		return true
	}

	for {
		if exitOnMailKill() {
			return nil
		}
		if ctx.Err() != nil {
			persistStdoutTailBestEffort(
				context.Background(),
				resolveTailSessionID(context.Background(), derefSessionID(&currentSessionID)),
				tailBuf.Snapshot(),
				"context_cancelled",
			)
			closeAgentBeadBestEffort("context_cancelled")
			return nil
		}
		if restarts >= cfg.maxRestarts {
			persistStdoutTailBestEffort(
				context.Background(),
				resolveTailSessionID(context.Background(), derefSessionID(&currentSessionID)),
				tailBuf.Snapshot(),
				"max_restarts_reached",
			)
			closeAgentBeadBestEffort("max_restarts_reached")
			return fmt.Errorf("max restarts (%d) reached", cfg.maxRestarts)
		}
		// Unresolvable work prompt (gd-PPzLrS2k8e): after nudgeFallbackBound
		// fallback injects in a row, restarting coop again would only repeat the
		// fallback. Exit non-zero WITHOUT closing the agent bead, so the container
		// restart runs gb init as a new process (fresh daemon client, pod-spec env,
		// no switch overrides) on the same bead and conversation.
		if n, exhausted := nudgeFallbacksExhausted(); exhausted {
			logInit("FATAL: work prompt fell back %d times in a row — exiting so the container restarts gb init", n)
			persistStdoutTailBestEffort(
				context.Background(),
				resolveTailSessionID(context.Background(), derefSessionID(&currentSessionID)),
				tailBuf.Snapshot(),
				"nudge_prompt_unresolved",
			)
			return fmt.Errorf("nudge prompt unresolved after %d consecutive fallbacks", n)
		}

		cleanStalePipes(cfg.coopStateDir)
		// Always clean stale PID files from sessions/ — after a pod restart
		// every old PID is invalid, and stale entries confuse Claude's resume
		// picker regardless of whether we intend to resume.
		// Claude-only: sessions/<pid>.json and projects/ are Claude Code's.
		// Codex's state dir holds the restored rollout file coop resumes from,
		// and Gemini's the restored chats/*.jsonl; gb init must not touch either.
		if harness.IsClaude(cfg.harness) {
			cleanStaleSessions(cfg.claudeStateDir)
		}

		// Resume is owned by coop via COOP_S3_SESSION_KEY=<agent-bead-id>:
		// coop downloads prior session state from the S3 prefix and points
		// Claude at it. gb init no longer scans the local PVC for a session
		// log to pass as --resume.
		//
		// When gb init has decided "fresh" (no S3 bucket configured or no
		// agent bead key to scope the prefix), Claude Code would otherwise
		// auto-resume from any .jsonl left in the PVC's projects/<hash>/
		// directory and pop a resume picker that bypassStartup has to fight.
		// Remove the discovery surface up front so fresh stays fresh. The
		// resume path is unaffected: coop's restored .jsonl lives under
		// <state_home>/coop/sessions/, not under projects/. See ka-0000004439.
		//
		// Force-clean (ka-0000007242) also purges here: under force-clean the S3
		// key is still set (so s3ResumeIntended is true), but coop will skip the
		// restore — so any local .jsonl left in projects/ is exactly the oversized
		// transcript we are refusing to resume, and must be removed before it can
		// drive Claude Code's own auto-resume picker.
		//
		// A crash restart with S3 resume intended is the opposite case
		// (gd-HPVt3BCPCH): the local transcript is the only surviving record of the
		// interrupted turn, so it is retired (kept as .jsonl.stale) rather than
		// deleted — see prepareLocalSessionLogs.
		if harness.IsClaude(cfg.harness) {
			prepareLocalSessionLogs(cfg.claudeStateDir)
		}

		sessionBeadID := registerSessionBead(ctx, initToK8sConfig(cfg), "")

		// Reset per-session state — the tail buffer captures only the most
		// recent coop session, since that's where the failure lives.
		tailBuf.Reset()
		currentSessionID.Store(nil)

		// Wipe the stop-gate scratch files (cooldown timestamp, consecutive-block
		// counter, yield marker) before every coop launch. They live in the
		// pod-scoped /tmp EmptyDir that survives process restarts, so a session
		// that died without reaching a gate-clear path (compaction crash, SIGTERM,
		// credential-rotation exit(10)) would otherwise leave a stale block count
		// that force-exits the NEXT resumed session on its first turn-end —
		// stamping it `stop_gate_*_involuntary` before it does any work
		// (ka-0000007830). Resetting here scopes the counter to a single session.
		resetStopGateSessionState()

		// Per-session "reached working" flag, set by monitorExit. Drives the
		// credential-switch terminal-cap reset below: a genuinely productive
		// session (not merely a long-but-cold one) re-arms the flap circuit
		// breaker. (ka-0000001718 Part B)
		var productive atomic.Bool

		// Per-session "died on a credential-not-ready login banner" flag, set by
		// runSessionLifecycle when bypassStartup / isSessionAlive detect a
		// persistent login banner. Feeds the startup-login failure budget below
		// (ka-0000007536).
		var startupLoginBlocked atomic.Bool

		// Per-session "relaunch to adopt a warm-pool resume key" flag, set by
		// runSessionLifecycle after standby assignment when the pool handed this
		// pod a warm_resume_s3_key. The relaunch is a clean, uncounted restart so
		// coop re-boots with COOP_S3_SESSION_KEY=<prior transcript> and resumes
		// the real conversation on the warm pod (ka-0000000983).
		var warmResumeRelaunch atomic.Bool

		start := time.Now()
		stopUsageSnapshot := startCoopUsageSnapshot(ctx, cfg.coopPort)
		exitCode := runCoopSession(mail.sessionCtx, cfg, tailBuf, &currentSessionID, &productive, &startupLoginBlocked, &warmResumeRelaunch)
		stopUsageSnapshot()
		elapsed := time.Since(start)
		lastCoopExitCode.Store(int32(exitCode))
		lastCoopExitKnown.Store(true)

		// Resolve the graceful-shutdown bit BEFORE closing the session bead: the
		// stop_requested check further down runs too late to inform the status the
		// close writes (ka-0000003165). Uses a fresh context for the bead read —
		// ctx is already cancelled on the pod-terminate path.
		shutdownRequested := false
		if beadsapi.SessionStatusDependsOnShutdown(exitCode) {
			shutdownRequested = sessionShutdownRequested(context.Background(), ctx, mail.killRequested())
		}

		closeSessionBead(context.Background(), sessionBeadID, exitCode, "", shutdownRequested)

		// Remember whether the agent stopped ITSELF (gb stop / gb yield closed
		// its bead) before this incarnation's coop exited. The teardown close
		// below may run a minute later; by then the bridge can have reopened
		// the same bead for the NEXT turn, and that must not be mistaken for a
		// mid-turn kill of this pod (gd-elNIGGSqwj).
		selfStoppedBeforeExit.Store(agentBeadClosedNow())

		if exitOnMailKill() {
			return nil
		}

		// A productive session proves any prior credential-switch flap resolved:
		// reset the durable switch tracker so both the escalating backoff and the
		// terminal cap re-arm from scratch. Done unconditionally (any exit code)
		// so a session that worked and then exited normally still clears the flap
		// accounting.
		if productive.Load() {
			wasTerminal := switches.TrippedTerminal()
			switches.Reset()
			// Undo any terminal-cap rotation-park: restore the original
			// COOP_PROFILE so legitimate failover works again on the next flap.
			if wasTerminal && os.Getenv("COOP_PROFILE") != origCoopProfile {
				if origCoopProfile == "" {
					os.Unsetenv("COOP_PROFILE")
				} else {
					os.Setenv("COOP_PROFILE", origCoopProfile)
				}
				logInit("productive session — re-enabled in-session credential rotation (COOP_PROFILE restored)")
			}
			// The agent resumed and worked again: any prior interruption cycle is
			// over, so clear the breadcrumb dedup marker. A genuinely NEW mid-turn
			// kill in this fresh work cycle is then allowed to post one more
			// "interrupted" breadcrumb (ka-0000002138).
			if agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID")); agentBeadID != "" {
				clearCtx, clearCancel := context.WithTimeout(context.Background(), 10*time.Second)
				clearInterruptedBreadcrumbMarker(clearCtx, agentBeadID)
				clearCancel()
			}
		}

		// Warm-pool coop-resume relaunch (ka-0000000983). The lifecycle adopted a
		// warm_resume_s3_key after standby assignment and shut coop down so we can
		// relaunch it keyed on the prior transcript. This is an intended, clean
		// relaunch: loop straight back to runCoopSession WITHOUT counting a restart
		// or recording a fast-crash (this session ran only long enough to reach
		// assignment, so elapsed is short and would otherwise trip crashes.Record).
		// buildCoopEnv now sees the adopted canonicalS3Key and wires
		// COOP_S3_SESSION_KEY=<prior transcript prefix>, so coop resumes the real
		// conversation; the relaunched session's standbyWait no-ops (agent_state is
		// already past prewarmed) and maybeAdoptWarmResumeKey returns false (the key
		// is already adopted), so it falls through to injectPrompt normally. Skipped
		// on pod teardown (ctx cancelled) so a terminating pod does not relaunch.
		if warmResumeRelaunch.Load() && ctx.Err() == nil {
			logInit("warm-pool resume: relaunching coop with adopted resume key (uncounted restart)")
			continue
		}

		// Fast-crash detection: ≥3 sub-30s exits inside fastCrashLookback.
		// When triggered, ship the tail of the most recent session — that's
		// the one carrying the freshest "Claude exited because X" trailer.
		if crashes.Record(elapsed) {
			persistStdoutTailBestEffort(
				context.Background(),
				resolveTailSessionID(context.Background(), derefSessionID(&currentSessionID)),
				tailBuf.Snapshot(),
				"fast_crash",
			)
		}

		if ctx.Err() != nil {
			persistStdoutTailBestEffort(
				context.Background(),
				resolveTailSessionID(context.Background(), derefSessionID(&currentSessionID)),
				tailBuf.Snapshot(),
				"context_cancelled",
			)
			closeAgentBeadBestEffort("context_cancelled")
			return nil
		}

		logInit("coop exited (code %d) after %s", exitCode, elapsed.Round(time.Second))

		// Credential switch: coop exited so gb init can restart with the
		// new credentials and re-inject the work prompt. Load the new
		// credentials, reset counters, and immediately loop back to
		// runCoopSession → runSessionLifecycle.
		if exitCode == exitCodeCredentialSwitch {
			logInit("credential switch requested — loading new credentials and restarting")
			loadSwitchCredentials()
			// Rebuild the agent command so the baked-in `--model` flag tracks
			// the model the just-applied switch credentials carry (ka-0000001014).
			// buildAgentCommand() ran once at boot and froze `--model
			// $CLAUDE_MODEL` into cfg.command. Claude Code's --model CLI flag
			// takes precedence over the CLAUDE_MODEL/ANTHROPIC_MODEL env vars,
			// so a failover onto litellm-bedrock — which rewrites CLAUDE_MODEL
			// to a bedrock-allowlisted model (e.g. bedrock-claude-opus-4-7) via
			// the credential switch file — was silently defeated: the stale
			// flag still requested the global opus-4-8 model, which is not in
			// the bedrock LiteLLM key allowlist, so every turn returned
			// `403 key not allowed to access model`. The agent could not run a
			// single inference turn and became a zombie pod.
			if newCmd := buildAgentCommand(); newCmd != cfg.command {
				logInit("rebuilt agent command after credential switch: %q -> %q", cfg.command, newCmd)
				cfg.command = newCmd
			}
			// Durable anti-flap: if credential switches are coming too fast
			// (coop's in-memory cap can't see across these restarts), sleep
			// with escalating backoff before relaunching. A single failover
			// returns 0 and restarts immediately. (ka-0000005014)
			if backoff := switches.Record(); backoff > 0 {
				logInit("credential switch flap detected — backing off %s before restart", backoff.Round(time.Second))
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
			}
			// Terminal cap (defense-in-depth, ka-0000001718 Part B): if the flap
			// has persisted across switchTerminalCap non-productive sessions, the
			// escalating backoff alone has not broken it. Continuing the tight
			// exit-10 → fresh-session-bead churn (a new session every ~277s,
			// resumed=False) is pure waste. Park instead: disable coop's
			// in-session rotation for the NEXT relaunch (COOP_PROFILE=manual →
			// try_auto_rotate returns Skipped → no more exit-10), so coop stays
			// UP and the underlying 429 is handled IN PLACE by monitorExit
			// (dismiss + backoff + re-nudge, same session, liveness probe alive)
			// rather than cold-restarting forever. The known model-group 429 is
			// already routed to the non-rotating model_rate_limited path by coop;
			// this cap only catches NOVEL / unclassified auth-exhaustion 429
			// shapes that still drive rotation. The since-productive counter is
			// reset by a productive session (below), so this is a self-healing
			// circuit breaker, not a permanent wedge.
			if switches.TrippedTerminal() {
				logInit(
					"credential-switch flap TERMINAL after %d non-productive switches — "+
						"disabling in-session rotation and parking coop IN PLACE "+
						"(no more exit-10 cold-restarts; 429 handled by monitorExit)",
					switches.SinceProductive(),
				)
				os.Setenv("COOP_PROFILE", "manual")
				agentID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
				if agentID != "" && daemon != nil {
					stateCtx, stateCancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := daemon.UpdateAgentState(stateCtx, agentID, "model_rate_limited"); err != nil {
						logInit("warning: failed to mark agent model_rate_limited on terminal switch-flap park: %v", err)
					}
					stateCancel()
				}
			}
			restarts = 0
			continue
		}

		// Check if the agent requested a polite stop. A daemon that denies this
		// agent its own bead parks the loop instead of respawning coop: it can
		// tell neither a finished agent from a crashed one (gd-aoSsfpwPGG).
		if handleStopAfterExit(ctx, envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))) {
			return nil
		}

		// Out-of-process terminal close (ka-0000004483). The stop-gate Stop-hook
		// (cooldown / escape_hatch / rate_limited) closes the agent bead from a
		// SEPARATE process WITHOUT setting stop_requested, so the checks above miss
		// it and this loop would otherwise respawn coop — whose first act
		// (tailBuf.Reset) wipes the just-exited session's stdout. Those beads carry
		// a SYNTHESIZED wrap-up (the agent never explained itself), the exact deaths
		// a post-mortem tail is designed for. Ship the tail NOW — while tailBuf
		// still holds this session and currentSessionID is resolved — then exit
		// cleanly instead of restart-looping against an already-closed bead. Checked
		// with a fresh context so a cancelling session ctx does not skip the lookup.
		if closeReason, closed := agentBeadClosedOutOfProcess(context.Background()); closed {
			logInit("agent bead closed out-of-process (close_reason=%q) — shipping stdout tail and exiting without respawn", closeReason)
			persistStdoutTailBestEffort(
				context.Background(),
				resolveTailSessionID(context.Background(), derefSessionID(&currentSessionID)),
				tailBuf.Snapshot(),
				tailReasonFromCloseReason(closeReason),
			)
			return nil
		}

		// Rate limit: sleep until reset.
		if sleepDur := rateLimitSleepDuration(); sleepDur > 0 {
			logInit("rate limited — sleeping %s before restart", sleepDur.Round(time.Second))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(sleepDur):
			}
			restarts = 0
			continue
		}

		// Startup-login failure budget (ka-0000007536). A session that died on a
		// credential-not-ready login banner counts toward a distinct budget; any
		// other exit resets the streak. Once the budget is reached, close the
		// bead with the credential-specific `not_logged_in` reason and stop the
		// pod — deterministically, before maxRestarts, and with the signal gb
		// audit needs. This is the startup-banner analogue of coop's in-session
		// LoginRequiredWatchdog, which cannot see across coop restarts.
		if startupLoginBlocked.Load() {
			if loginFailures.Record() {
				logInit(
					"FATAL: %d consecutive credential-not-ready startups (login banner) — "+
						"closing bead not_logged_in and stopping pod",
					loginFailures.Count(),
				)
				persistStdoutTailBestEffort(
					context.Background(),
					resolveTailSessionID(context.Background(), derefSessionID(&currentSessionID)),
					tailBuf.Snapshot(),
					"not_logged_in",
				)
				closeAgentBeadBestEffort("not_logged_in")
				return fmt.Errorf("credential not ready at spawn: %d consecutive login-blocked startups", loginFailures.Count())
			}
			logInit("credential-not-ready startup %d/%d (login banner) — restarting", loginFailures.Count(), startupLoginFailureBudget)
		} else {
			loginFailures.Reset()
		}

		if elapsed >= time.Duration(minRuntimeSecs)*time.Second {
			restarts = 0
		}
		restarts++
		logInit("restarting (attempt %d/%d) in 2s...", restarts, cfg.maxRestarts)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// prepareLocalSessionLogs decides what happens to the local projects/ transcripts
// before each coop (re)launch:
//
//   - no S3 resume intended, or force-clean latched: DELETE them
//     (cleanAllSessionLogs). Fresh must stay fresh, and a force-cleaned
//     transcript is one we refuse to resume.
//   - S3 resume intended (a normal crash / credential-switch restart): RETIRE
//     them (retireAllSessionLogs). The bytes survive as .jsonl.stale, but no
//     *.jsonl remains for coop's tier-2 discovery to latch onto — leaving the
//     previous incarnation's transcript in place is what drove the deterministic
//     FATAL loops of gd-2g0LXWF4Sz. Resume itself comes from the S3 archive.
//
// The other cleanAllSessionLogs call site, agent_start_k8s.go (the non-init
// `gb agent start` loop), is a different mechanism: it purges only when its own
// findResumeSession found NO eligible local log, so a resumable transcript is
// already kept there and nothing needs to change.
func prepareLocalSessionLogs(claudeStateDir string) {
	if !s3ResumeIntended() || forceCleanActive() {
		cleanAllSessionLogs(claudeStateDir)
		return
	}
	retireAllSessionLogs(claudeStateDir)
}

// runCoopSession starts coop for a single session, launches per-session
// lifecycle goroutines, waits for coop to exit, and returns its exit code.
//
// tailBuf (when non-nil) tees coop's stdout/stderr into a ring buffer so the
// last N kB can be shipped to S3 on a fast-crash or context_cancelled exit.
// sessionIDSink (when non-nil) is set to the coop session ID once the lifecycle
// goroutine resolves it via the coop API — used as a destination key for the
// tail upload.
func runCoopSession(ctx context.Context, cfg initConfig, tailBuf *tailBuffer, sessionIDSink *atomic.Pointer[string], productiveSink *atomic.Bool, startupLoginBlockedSink *atomic.Bool, warmResumeRelaunchSink *atomic.Bool) int {
	coopArgs := []string{
		"--agent=" + harness.CoopAgent(cfg.harness, cfg.command),
		fmt.Sprintf("--port=%d", cfg.coopPort),
		fmt.Sprintf("--port-health=%d", cfg.coopHealthPort),
		fmt.Sprintf("--port-metrics=%d", cfg.coopMetricsPort),
		"--cols=200", "--rows=50",
	}
	logInit("starting coop (%s/%s)", cfg.role, cfg.agent)
	// Split the command string into individual args so coop receives them
	// as separate argv entries (not wrapped in "sh -c"). This ensures coop
	// can append --settings/--resume args correctly via execvp.
	coopArgs = append(coopArgs, "--")
	coopWorkdir := resolveCoopWorkdir(initToK8sConfig(cfg))
	parts := strings.Fields(cfg.command)
	// prepareAgentPrompt installs $CODEX_HOME/AGENTS.md on the codex harness and
	// returns a file path only for Claude (Codex has no
	// --append-system-prompt-file).
	if path, ok := prepareAgentPrompt(cfg.harness, coopWorkdir, logInit); ok {
		parts = injectSystemPromptArgs(parts, path)
	}
	coopArgs = append(coopArgs, parts...)

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	coopCmd := exec.CommandContext(sessionCtx, "coop", coopArgs...)
	coopCmd.Dir = coopWorkdir
	if tailBuf != nil {
		coopCmd.Stdout = io.MultiWriter(os.Stdout, tailBuf)
		coopCmd.Stderr = io.MultiWriter(os.Stderr, tailBuf)
	} else {
		coopCmd.Stdout = os.Stdout
		coopCmd.Stderr = os.Stderr
	}
	coopCmd.Env = buildCoopEnv(cfg)
	coopCmd.Cancel = func() error {
		logInit("sending SIGTERM to coop (pid %d)", coopCmd.Process.Pid)
		return coopCmd.Process.Signal(syscall.SIGTERM)
	}
	coopCmd.WaitDelay = 20 * time.Second

	if err := coopCmd.Start(); err != nil {
		logInit("failed to start coop: %v", err)
		return 1
	}

	// Per-session lifecycle goroutines.
	coop := coopapi.New(fmt.Sprintf("http://localhost:%d", cfg.coopPort))
	// runSessionLifecycle decides when the session is over. When it returns —
	// either monitorExit observed coop exit, or it bailed early on an
	// unrecoverable session (credential-not-ready login banner, zombie startup,
	// standby TTL) after firing a best-effort coop.Shutdown() — we must GUARANTEE
	// the coop process actually goes down. A bare coop.Shutdown() HTTP POST can be
	// dropped or ignored (coop wedged on the login banner, its HTTP server
	// unresponsive); if it does not take, coopCmd.Wait() below blocks forever, the
	// supervisor restart loop freezes (restarts++ never runs), and the agent bead
	// is never closed — escaping BOTH the max_restarts and the in-session
	// not_logged_in terminal paths. That is the residual credential-not-ready
	// wedge (ka-0000007536). forceTeardownAfterLifecycle closes the gap.
	lifecycleDone := make(chan struct{})
	go func() {
		defer close(lifecycleDone)
		runSessionLifecycle(sessionCtx, coop, cfg, productiveSink, startupLoginBlockedSink, warmResumeRelaunchSink)
	}()
	go forceTeardownAfterLifecycle(sessionCtx, lifecycleDone, sessionCancel)
	if sessionIDSink != nil {
		go captureSessionID(sessionCtx, coop, sessionIDSink)
	}
	go monitorIdle(sessionCtx, coop)
	if !cfg.mockMode {
		go refreshCredentials(sessionCtx, coop)
	}
	// Warm-keepalive idle-TTL: keep thread/DM pods alive while parked-idle so
	// follow-ups hit the live coop session (warm nudge) instead of a cold
	// respawn, then despawn cleanly after the window. Default-off for non-thread
	// agents; opt-in per project/role via an "idle-ttl" config bead. See
	// init_agent_idle_ttl.go and ka-0000005793.
	if ttlCfg := resolveIdleTTL(sessionCtx); ttlCfg.enabled {
		go monitorIdleTTL(sessionCtx, coop, ttlCfg)
	}

	waitErr := coopCmd.Wait()
	sessionCancel()

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				return status.ExitStatus()
			}
		}
		return 1
	}
	return 0
}

// sessionForceTeardownGrace bounds how long forceTeardownAfterLifecycle waits
// for coop to exit on its own (after the lifecycle requested a graceful
// shutdown) before force-killing the session. Overridable for tests.
var sessionForceTeardownGrace = 30 * time.Second

// forceTeardownAfterLifecycle guarantees the coop process is torn down once the
// session lifecycle goroutine has returned. It is the safety net for the
// credential-not-ready-at-spawn wedge (ka-0000007536): the lifecycle's only
// mechanism to end a dead session is a best-effort coop.Shutdown() HTTP POST,
// which can be dropped or ignored by a wedged coop — leaving coopCmd.Wait()
// blocked forever and freezing the supervisor restart loop.
//
// Once the lifecycle is done, a coop that honored the graceful shutdown will
// have its process gone within a second or two and coopCmd.Wait() returns on its
// own (the caller's sessionCancel then wins this select's Done branch — no
// signal is sent, no premature SIGKILL). Only when coop is STILL up past the
// grace window do we force it down: cancelling the session context fires
// coopCmd.Cancel (SIGTERM) and, WaitDelay later, SIGKILL. This runs on EVERY
// session end (not just credential failures), so it also backstops any other
// path where coop ignores a shutdown request.
func forceTeardownAfterLifecycle(sessionCtx context.Context, lifecycleDone <-chan struct{}, sessionCancel context.CancelFunc) {
	select {
	case <-sessionCtx.Done():
		return
	case <-lifecycleDone:
	}
	select {
	case <-sessionCtx.Done():
		return
	case <-time.After(sessionForceTeardownGrace):
	}
	logInit("lifecycle ended but coop still up after %s — forcing session teardown (SIGTERM→SIGKILL)", sessionForceTeardownGrace)
	sessionCancel()
}

// runSessionLifecycle handles the per-session lifecycle: bypass startup prompts,
// wait for standby assignment (if applicable), inject the initial work prompt,
// and monitor for exit/rate-limit. Runs as a goroutine scoped to one coop session.
//
// startupLoginBlockedSink (when non-nil) is set true when the session died on a
// credential-not-ready login banner — bypassStartup returned errStartupLoginBanner
// or isSessionAlive gave up while a login banner was still on screen. The
// supervisor loop uses it to maintain the startup-login failure budget
// (ka-0000007536).
// warmResumeRelaunchSink (when non-nil) is set true after standby assignment
// when the pool handed this pod a warm-pool resume key (warm_resume_s3_key) and
// the key has just been adopted as the canonical S3 prefix. The supervisor loop
// reads it to relaunch coop once — WITHOUT counting a restart — so the new coop
// process boots with COOP_S3_SESSION_KEY=<prior transcript prefix> and resumes
// the real conversation on the already-warm pod (ka-0000000983).
func runSessionLifecycle(ctx context.Context, coop *coopapi.Client, cfg initConfig, productiveSink *atomic.Bool, startupLoginBlockedSink *atomic.Bool, warmResumeRelaunchSink *atomic.Bool) {
	// Wait for coop API to be available.
	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if _, err := coop.GetAgentState(ctx); err == nil {
			break
		}
	}

	// With COOP_S3_SESSION_KEY=<agent-bead-id>, coop restores prior session
	// state from S3 before Claude starts. When S3 is configured, treat the
	// resume picker as a confirmation prompt (press Enter to resume the
	// downloaded session). Without S3, no prior state can exist and the
	// picker indicates a stale local session — start fresh.
	hasResume := s3ResumeIntended()

	// bypassStartup drives Claude Code's TUI (trust dialog, resume picker, login
	// banner); a Codex app-server has no screen and nothing to dismiss.
	if err := bypassStartupForHarness(ctx, coop, cfg.harness, cfg.command, hasResume); err != nil {
		logInit("bypass startup failed: %v", err)
		// A login banner at startup is a credential-not-ready death (bypassStartup
		// already requested coop.Shutdown). Signal the supervisor so it can budget
		// consecutive login-blocked startups and close the bead not_logged_in
		// (ka-0000007536).
		if startupLoginBlockedSink != nil && errors.Is(err, errStartupLoginBanner) {
			startupLoginBlockedSink.Store(true)
		}
		return
	}

	if cfg.standby && os.Getenv("BOAT_AGENT_BEAD_ID") != "" {
		specialized, err := standbyWait(ctx, coop)
		if err != nil {
			logInit("standby wait failed: %v", err)
			return
		}
		rematerializeAfterSpecialization(cfg, specialized)

		// Warm-pool coop-resume graft (ka-0000000983): if the pool assigned this
		// pod to a thread with a prior transcript, it stamped warm_resume_s3_key.
		// Adopt it as the canonical S3 prefix and relaunch coop ONCE so the new
		// process boots with COOP_S3_SESSION_KEY=<prior transcript> and resumes
		// the real conversation — instead of the fresh session this warm pod
		// booted under its own empty prefix. We must relaunch (not resume in
		// place) because coop reads the resume key only at process launch, and the
		// key is unknown until assignment. Signal the supervisor loop and end this
		// session cleanly; do NOT injectPrompt into the soon-to-be-replaced
		// session (the relaunched session will injectPrompt from the persisted
		// bead description). A no-op (returns false) unless the feature is on AND a
		// key was stamped AND it is not already adopted.
		if warmResumeRelaunchSink != nil &&
			maybeAdoptWarmResumeKey(ctx, os.Getenv("BOAT_AGENT_BEAD_ID")) {
			warmResumeRelaunchSink.Store(true)
			logInit("warm-pool resume: ending fresh warm session to relaunch coop with resume key")
			_ = coop.Shutdown(ctx)
			return
		}
	}

	requestBaseline, err := injectPrompt(ctx, coop)
	if err != nil {
		logInit("inject prompt failed: %v", err)
	}

	// Startup health check: verify the agent actually reached an active state
	// (idle or working) after bypass + inject. If it never started — e.g.
	// because credentials were missing — shut down coop so the restart loop
	// can retry or eventually hit maxRestarts and terminate the pod. Without
	// this check, the pod zombies indefinitely (coop health port stays up,
	// K8s liveness passes, but Claude is not doing any work).
	if !isSessionAlive(ctx, coop) {
		logInit("FATAL: agent not active after startup — requesting coop shutdown to prevent zombie pod")
		// If the reason the session never came alive is a persistent login banner
		// (a transient credential-switch banner would have cleared inside
		// isSessionAlive's grace window and returned true), classify it as a
		// credential-not-ready death so the supervisor's startup-login budget can
		// close the bead not_logged_in rather than burning generic restarts
		// (ka-0000007536).
		if startupLoginBlockedSink != nil && screenShowsLoginBanner(ctx, coop) {
			startupLoginBlockedSink.Store(true)
		}
		_ = coop.Shutdown(ctx)
		return
	}

	// Monitor exit and rate limits for the rest of the session.
	if err := monitorExit(ctx, coop, productiveSink, requestBaseline); err != nil {
		logInit("monitor exit: %v", err)
	}
}

// initToK8sConfig converts an initConfig to k8sConfig for functions that
// still use the old struct (resolveCoopWorkdir, registerSessionBead).
func initToK8sConfig(cfg initConfig) k8sConfig {
	return k8sConfig{
		workspace:       cfg.workspace,
		coopPort:        cfg.coopPort,
		coopHealthPort:  cfg.coopHealthPort,
		coopMetricsPort: cfg.coopMetricsPort,
		maxRestarts:     cfg.maxRestarts,
		command:         cfg.command,
		role:            cfg.role,
		project:         cfg.project,
		agent:           cfg.agent,
		podIP:           cfg.podIP,
		hostname:        cfg.hostname,
	}
}

// buildInitConfig reads environment variables and flags to build the init config.
func buildInitConfig(standby, mockMode bool) initConfig {
	// The coopmux session id, so the full pod name: os.Hostname() is cut at 63
	// bytes, and a 64-char plan gardener registered truncated, evicting coop's
	// own full-name registration every heartbeat (Glass showed no terminal).
	hostname := resolveMuxSessionID()
	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		podIP = "localhost"
	}

	workspace := envOr("BOAT_WORKSPACE", "/home/agent/workspace")
	claudeDir := filepath.Join(os.Getenv("HOME"), ".claude")
	stateDir := filepath.Join(workspace, ".state")

	// If ~/.claude is a mount point (subPath from controller), use it directly.
	// Otherwise use .state/claude on the PVC.
	claudeStateDir := filepath.Join(stateDir, "claude")
	if isMountpoint(claudeDir) {
		claudeStateDir = claudeDir
	}
	h := harness.MustFromEnv()

	return initConfig{
		coopPort:        intEnvOr("COOP_PORT", 8080),
		coopHealthPort:  intEnvOr("COOP_HEALTH_PORT", 9090),
		coopMetricsPort: intEnvOr("COOP_METRICS_PORT", 9093),
		maxRestarts:     intEnvOr("COOP_MAX_RESTARTS", 10),
		command:         buildAgentCommand(),
		role:            envOr("BOAT_ROLE", "unknown"),
		project:         os.Getenv("BOAT_PROJECT"),
		agent:           envOr("BOAT_AGENT", "unknown"),
		workspace:       workspace,
		harness:         h,
		claudeStateDir:  claudeStateDir,
		coopStateDir:    filepath.Join(stateDir, "coop"),
		podIP:           podIP,
		hostname:        hostname,
		standby:         standby,
		mockMode:        mockMode,
	}
}

// loadSwitchCredentials reads the credential switch metadata file written
// by coop, sets the new credentials as environment variables, and removes
// the file.  The next coop invocation inherits these env vars.
func loadSwitchCredentials() {
	data, err := os.ReadFile(credentialSwitchFile)
	if err != nil {
		logInit("warning: no credential switch file: %v", err)
		return
	}
	defer os.Remove(credentialSwitchFile)

	var meta struct {
		Credentials map[string]string `json:"credentials"`
		Profile     *string           `json:"profile"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		logInit("warning: parse credential switch file: %v", err)
		return
	}

	for k, v := range meta.Credentials {
		os.Setenv(k, v)
		logInit("applied credential override: %s", k)
	}

	// Re-assert a pinned model override (epic ka-0000005488, scope item 3).
	// The coopmux credential broker is ACCOUNT-keyed: it stores one model per
	// account and pushes it as CLAUDE_MODEL/ANTHROPIC_MODEL to every session on
	// that account during a reload/rotation. The loop above just applied that
	// account-default model, which would silently revert a per-pod role/project/
	// agent model override (the whole point of this epic: one account backing
	// multiple models). The controller stamps BOAT_MODEL_PINNED on pods that
	// carry an override; honor it here so rotation never reverts the model.
	// buildAgentCommand() below then rebuilds `--model $CLAUDE_MODEL` from the
	// re-asserted value, matching the ka-0000001014 model-tracking fix.
	//
	// Safe against the bedrock-failover case (ka-0000001014, which intentionally
	// rewrites CLAUDE_MODEL on a cross-account switch to litellm-bedrock): per
	// the symmetric Bedrock partition (podspec.go), a model override only exists
	// on non-bedrock roles, and those roles never fail over to Bedrock — so a
	// pinned pod never takes the bedrock-model-rewrite path. Pin and bedrock
	// failover are mutually exclusive.
	if pin := os.Getenv("BOAT_MODEL_PINNED"); pin != "" {
		if h := harness.MustFromEnv(); !harness.IsClaude(h) {
			// Codex and Gemini: the broker pushes Claude-shaped model vars, which
			// buildAgentCommand ignores; re-assert CODEX_MODEL / GEMINI_MODEL for
			// the harness's model flag.
			if env := harness.ModelEnv(h); os.Getenv(env) != pin {
				os.Setenv(env, pin)
				logInit("re-asserted pinned model override after credential switch: %s", pin)
			}
		} else if os.Getenv("CLAUDE_MODEL") != pin || os.Getenv("ANTHROPIC_MODEL") != pin {
			os.Setenv("CLAUDE_MODEL", pin)
			os.Setenv("ANTHROPIC_MODEL", pin)
			logInit("re-asserted pinned model override after credential switch: %s", pin)
		}
	}

	if meta.Profile != nil {
		logInit("credential switch profile: %s", *meta.Profile)
		// Update BOAT_ACCOUNT so the next coop restart reports the account it
		// actually switched to. After a credential switch coop's ProfileState
		// starts empty again, so its registration account_name falls back to
		// BOAT_ACCOUNT (see coop run.rs). Without this the restarted session
		// would re-report the ORIGINAL account, diverging coopmux's
		// assigned_account / dashboard from the live credentials. (ka-0000005014)
		if *meta.Profile != "" {
			os.Setenv("BOAT_ACCOUNT", *meta.Profile)
		}
	}
}

// buildAgentCommand constructs the agent command string from env vars,
// matching the logic in entrypoint.sh. The default command and the model flag
// are per-harness (internal/harness): Claude gets `--model $CLAUDE_MODEL` and
// the dev-channel flag, Codex gets `-m $CODEX_MODEL` and ignores GARDENIO_CHANNEL.
func buildAgentCommand() string {
	h := harness.MustFromEnv()
	cmd := envOr("BOAT_COMMAND", harness.DefaultCommand(h))
	mockMode := harness.IsMockCommand(cmd)

	// Append the model flag if the harness's model var is set and not in mock mode.
	cmd = harness.WithModel(h, cmd, os.Getenv(harness.ModelEnv(h)))

	// Gardenio channel: approve the MCP-based channel server if enabled.
	// Claude-only: Codex and Gemini have no channels, nudges go through the driver.
	if harness.IsClaude(h) && os.Getenv("GARDENIO_CHANNEL") == "1" && !mockMode {
		channelScript := filepath.Join("/channel", "gardenio-channel.ts")
		if _, err := os.Stat(channelScript); err == nil {
			cmd += " --dangerously-load-development-channels server:gardenio-channel"
		}
	}

	return cmd
}

// selfStoppedBeforeExit is set by the restart loop when, right after coop
// exited, the agent bead was already closed — i.e. the agent ended its own turn
// via gb stop / gb yield rather than being killed. It is refreshed every loop
// iteration and consumed by closeAgentBeadBestEffort (gd-elNIGGSqwj).
var selfStoppedBeforeExit atomic.Bool

// agentBeadClosedNow reports whether the agent bead is closed at this instant.
// Best-effort: any lookup failure reads as "not closed", which keeps today's
// teardown behaviour.
func agentBeadClosedNow() bool {
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentBeadID == "" || daemon == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := daemon.GetBead(ctx, agentBeadID)
	return err == nil && b != nil && b.Status == "closed"
}

// closeAgentBeadBestEffort closes the agent bead when the restart loop exits.
// This prevents agent leaks where the bead stays open after the pod is gone.
// Best-effort: failures are logged but do not prevent exit.
//
// agent_state depends on reason:
//   - "context_cancelled" → "done" (graceful pod termination: rollout, drain,
//     SIGTERM). The pod shut down cleanly; marking "failed" here triggers a
//     spurious "Agent crashed" Slack notification on every rollout.
//   - anything else (e.g. "max_restarts_reached") → "failed" (a real crash the
//     restart loop could not recover from).
func closeAgentBeadBestEffort(reason string) {
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentBeadID == "" || daemon == nil {
		return
	}
	state := "failed"
	if reason == "context_cancelled" {
		state = "done"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Skip the close if the agent already closed its own bead cleanly (e.g.
	// via gb stop or gb yield). The dying pod otherwise races the agent's
	// own close and overwrites close_reason with "context_cancelled" — see
	// ka-0000000961.
	//
	// `gb stop --wrapup` closes the bead WITHOUT setting close_reason (it
	// writes the wrapup field instead), so a close_reason-only guard lets the
	// teardown path clobber the wrap-up card. Also skip when a wrapup is
	// present so the structured wrap-up survives — see ka-0000002689.
	//
	// alreadyClosed also gates the interrupted breadcrumb below: if the agent
	// closed its OWN bead (any gb stop / gb yield), it was not killed mid-turn,
	// so it must not post a spurious "interrupted" line — even if it left both
	// close_reason and wrapup empty.
	alreadyClosed := false
	var existingFields map[string]string
	existingToolCalls := 0
	if existing, err := daemon.GetBead(ctx, agentBeadID); err == nil && existing != nil {
		alreadyClosed = existing.Status == "closed"
		existingFields = existing.Fields
		existingToolCalls = beadToolCallCount(existing)
		// Frozen-awaiting-thaw guard (ka-0000002104 — the gb-side twin of the
		// controller-side ka-0000007439 fix): an OPEN bead at agent_state=stopping
		// is a SUSPENDED agent. suspendAgent (stop.go) parked it there as the
		// exclusive thaw-target signal, the controller checkpointed the sandbox,
		// and THIS pod's SIGTERM is the controller's own Pod A teardown completing
		// that freeze — not an involuntary death. Closing the bead here (the
		// pre-fix behavior) destroyed the signal seconds after every freeze: the
		// wake paths (bridge respawnThreadAgent, wakePlanAssistant) then saw a
		// plain done bead, minted a differently-named pod, and the name-keyed thaw
		// — the sole RecordResumeLatency call site — never fired (resume-latency
		// emitted zero samples). Leave the bead exactly as the suspend parked it;
		// no breadcrumb either (the agent is resumable by design, not interrupted).
		if !alreadyClosed && existing.Fields["agent_state"] == "stopping" {
			logInit("agent bead %s is frozen awaiting thaw (agent_state=stopping); leaving open for the wake path",
				agentBeadID)
			return
		}
		if existing.Status == "closed" && existing.Fields["close_reason"] != "" {
			logInit("agent bead %s already closed (reason=%q); skipping overwrite",
				agentBeadID, existing.Fields["close_reason"])
			return
		}
		if existing.Status == "closed" && existing.Fields["wrapup"] != "" {
			logInit("agent bead %s already closed with wrapup present; skipping overwrite",
				agentBeadID)
			return
		}
		// ka-0000008254: a clean `gb stop` closes the bead with agent_state=done
		// but may leave BOTH close_reason and wrapup empty (a bare `gb stop`
		// without --wrapup — most gardener-loop recycles). Such an agent already
		// stopped ITSELF cleanly: it is by definition NOT an involuntary death.
		// Without this guard the teardown path below reopens+re-stamps it
		// `context_cancelled_involuntary` purely because beadToolCallCount>0 (a
		// gardener that just ran its watchdog pass always has tool calls), which
		// is exactly the gardener-loop-teardown false-positive that trips the
		// AgentInvoluntaryDeaths alert. A finished, agent_state=done close must
		// never be reclassified as killed-mid-work.
		if existing.Status == "closed" && existing.Fields["agent_state"] == "done" {
			logInit("agent bead %s already closed cleanly (agent_state=done); skipping involuntary overwrite",
				agentBeadID)
			return
		}
	}

	// This incarnation closed its own bead (gb stop / gb yield) before coop
	// exited, so any OPEN bead we see now was reopened by the bridge for the
	// next turn (a new Slack message respawning the same agent), not left open
	// by a mid-turn kill. Closing it here cancels that wake-up, posts a false
	// "interrupted" breadcrumb, and strands the operator's new messages until
	// they nudge again (gd-elNIGGSqwj). The pod that owns the new turn does its
	// own teardown later.
	if selfStoppedBeforeExit.Load() {
		logInit("agent bead %s was self-stopped by this incarnation and has since been reopened for the next turn; skipping teardown close",
			agentBeadID)
		return
	}

	// The bead is still open — this agent was killed mid-turn (SIGTERM from a
	// node drain / Spot reclaim / rollout) before it could gb stop / gb yield.
	// For a Slack-thread-bound agent that means the thread would otherwise go
	// totally silent: the pod dies with an answer half-composed and nothing
	// posted. Drop a one-line "interrupted — mention me to resume" breadcrumb
	// so the human sees why it went quiet and knows the recovery path (a new
	// thread mention cold-resumes the same agent). Best-effort, thread-bound
	// agents only, context_cancelled only, and never when the agent already
	// closed its own bead — see ka-0000001201 / ka-0000004614.
	//
	// Dedup guard (ka-0000002138): during release / node churn a thread agent's
	// pod can be SIGTERM'd and cold-resumed many times in a row against the SAME
	// still-open bead — each teardown posts another identical breadcrumb here
	// (one thread accumulated 20). The breadcrumb POST above happens BEFORE the
	// close below, so a hard-kill in that window leaves the bead open for the
	// next teardown to repost. Gate on a persistent idempotency marker
	// (interruptedBreadcrumbPostedField) stamped on the agent bead: post at most
	// once per open interruption cycle. clearInterruptedBreadcrumbMarker() wipes
	// it when a resumed session reaches a productive "working" state, so a
	// genuinely new interruption in a later work cycle can post once more.
	//
	// ka-0000002080: the marker is stamped BEFORE the POST (mark-before-post,
	// see postInterruptedBreadcrumbOnce) so near-simultaneous teardowns of
	// separate pod incarnations see it set as early as possible. The GET above
	// → PATCH is still a non-atomic cross-process read-modify-write, so the
	// bridge-side reply dedup (internal/bridge/bot_threads_dedup.go) is the
	// backstop that actually holds under that concurrency.
	if reason == "context_cancelled" && !alreadyClosed &&
		existingFields[interruptedBreadcrumbPostedField] == "" {
		postInterruptedBreadcrumbOnce(ctx, agentBeadID)
	}

	// EOL salvage Phase 2 (ka-0000002059, design ka-0000007236): the involuntary
	// SIGTERM restart-loop path (this function) must reuse the same wrap-up synth
	// + claim-reopen salvage the voluntary Claude Stop-hook path already runs
	// (closeAgentBeadOnExit, hook.go). Without it, an involuntary mid-generation
	// kill closes the agent bead with NO wrapup (gardener-audit sees
	// wrapup_present=false and cannot tell the kill from a crash) and leaves the
	// worked ISSUE task orphaned in in_progress with a dead assignee (the
	// stale-claim tax on the next agent, advice ka-0000007726).
	//
	// This applies only to the graceful-termination `context_cancelled` case — a
	// rollout/drain/Spot-reclaim SIGTERM. A max_restarts_reached crash keeps the
	// bare close (state=failed): it is a genuine crash, has no worked-task claim
	// to salvage, and must not be dressed up with an involuntary wrap-up.
	if reason == "context_cancelled" {
		// Return the claimed-but-unfinished issue task to the ready queue, and
		// fold the reopened id into the close reason so the synthesized wrap-up
		// and Slack card read as an involuntary mid-work death, not an idle-out.
		// reopenOrphanedClaimedTask no-ops on actor==""/"unknown" and on no claim
		// (hook.go) — same guards the V-path relies on.
		reopened, lookupFailed := reopenOrphanedClaimedTask(ctx, reason)

		// An involuntary mid-work death is NOT an idle-out. Stamp `_involuntary`
		// on the close reason ONLY when the agent left genuinely-lost work: a
		// claimed in-progress ISSUE task we had to reopen (reopened != ""). A
		// teardown that reopened nothing keeps the plain `context_cancelled`
		// reason. Both close-reason forms keep the `context_cancelled` prefix, so
		// every downstream HasPrefix reader (terminalCardState, closeReasonTLDR)
		// stays compatible.
		//
		// ka-0000008254: we deliberately do NOT fall back to beadToolCallCount>0
		// here. A gardener / plan-gardener that just ran its watchdog pass ALWAYS
		// has tool calls, so a `tool-calls>0` fallback stamps `_involuntary` on
		// every clean gardener-loop recycle and drowns the genuine signal (crew
		// SIGTERM'd mid-task, which reopens a claimed issue). "Did work worth
		// counting as lost" means "left a claimed issue orphaned," not "made any
		// tool call." Gardeners never claim an issue-kind task, so they no longer
		// trip the counter. (The clean-close guard above already caught the
		// common case; this covers a teardown that races before the agent's own
		// close.)
		//
		// C9 (ka-0000001146): the busy-but-taskless kill is still worth COUNTING
		// — it was invisible entirely (plain context_cancelled never reaches the
		// deaths counter). Give it its own close reason so the metric can count
		// it as a SEPARATE family (context_cancelled_unclaimed) without touching
		// the `_involuntary` marker or the context_cancelled family the
		// node-reclaim sentinel exact-matches. The bar is tool-calls>0 on a
		// still-open bead: a genuine zero-work idle keeps the plain reason and
		// stays uncounted, and a cleanly self-closed agent never reaches here
		// (the guards above). total_tool_calls is bead-lifetime cumulative, so
		// this family is an upper bound on mid-turn kills — a warm-parked thread
		// agent SIGTERM'd between turns lands here too; that coarseness is why
		// it must never share the `_involuntary` families' semantics.
		//
		// upstream_api_error (ka-0000000202): if monitorExit observed this agent
		// still sitting on a Claude-upstream outage (server_error / no_internet)
		// recently enough — and no productive turn cleared it since — the honest
		// attribution is "Claude was down", NOT a routine node-churn
		// context_cancelled. Take precedence over the didWork / toolCalls arms:
		// the observation is only ever set when the agent hit an actual error
		// screen from a failed inference attempt, so it is a high-confidence
		// signal that the outage is why the work was lost. It IS involuntary
		// (work lost through no fault of the agent), so it carries the
		// `_involuntary` marker and counts in gardenio_agent_involuntary_deaths_total
		// under family=upstream_api_error. The stamped reason deliberately does
		// NOT keep the context_cancelled prefix — that is the whole point:
		// separating outage deaths out of the context_cancelled / node-reclaim
		// bucket is what makes the post-mortem honest.
		upstreamCat, upstreamActive := upstreamOutageActive()
		didWork := reopened != ""
		closeReason := reason
		switch {
		case upstreamActive:
			closeReason = "upstream_api_error_involuntary: pod terminated during a sustained Claude upstream outage (" + upstreamCat + ")"
			if reopened != "" {
				closeReason += "; reopened " + reopened
			}
		case didWork:
			closeReason = reason + "_involuntary: pod terminated mid-work before gb done"
			if reopened != "" {
				closeReason += "; reopened " + reopened
			}
		case lookupFailed:
			// The claimed-task lookup itself failed (a transient HTTP/network
			// blip during SIGTERM teardown survived the in-process retry) — we
			// never actually confirmed "no claim", so this must not be stamped
			// `_unclaimed`, which asserts exactly that. gd-k3kzZeIlT0 regression:
			// the ListAssignedTask failure was logged but still silently folded
			// into the same bucket as "genuinely holds nothing," orphaning real
			// claimed work (gd-1802PlhMZG, 2026-10-10) until a human happened to
			// run `gb news` and spot it in the stale-claimable list.
			//
			// It outranks existingToolCalls > 0 on purpose: the `_unclaimed`
			// arm below asserts a verified no-claim, which is exactly what is
			// missing here. The metric counts this reason as its own family
			// (context_cancelled_claim_lookup_failed, gd-TYjMaxCMZR), so the
			// death stays in the involuntary-death counter and the Slack death
			// report whether or not the agent made tool calls.
			closeReason = reason + "_claim_lookup_failed: pod terminated and the claimed-task lookup could not be verified"
		case existingToolCalls > 0:
			closeReason = reason + "_unclaimed: pod terminated with tool calls recorded but no claimed issue"
		}

		fields := map[string]string{
			"agent_state":  state,
			"close_reason": closeReason,
		}
		addLastCoopExitFields(fields)
		// closeAgentBeadWithFallback synthesizes a fallback wrap-up ONLY when the
		// bead carries none (a real gb stop/yield wrap-up always wins) — the
		// already-closed guard above has separately protected a clean wrap-up
		// from ever reaching here. Best-effort: a failure is logged, never fatal.
		if err := closeAgentBeadWithFallback(ctx, agentBeadID, reason, closeReason, fields); err != nil {
			logInit("warning: failed to close agent bead on exit (%s): %v", closeReason, err)
		} else {
			logInit("closed agent bead %s (%s, state=%s)", agentBeadID, closeReason, state)
		}
		return
	}

	// upstream_api_error (ka-0000000202): the restart loop giving up
	// (max_restarts_reached) while a Claude-upstream outage was active is the
	// canonical misattribution this family fixes — "the retry loop gave up"
	// silently absorbed "Claude was down". When monitorExit observed a recent,
	// uncleared upstream outage, re-attribute the death to upstream_api_error
	// (involuntary, counted) so a post-incident query can tell a provider outage
	// apart from a genuinely broken retry loop. Scoped to max_restarts_reached:
	// not_logged_in and other terminal reasons have their own precise cause and
	// never coincide with an on-screen upstream error observation. State stays
	// "failed" (an outage-driven exhaustion of the restart budget IS a failure,
	// unlike the graceful context_cancelled teardown above).
	closeReason := reason
	if reason == "max_restarts_reached" {
		if upstreamCat, upstreamActive := upstreamOutageActive(); upstreamActive {
			closeReason = "upstream_api_error_involuntary: restart loop exhausted during a sustained Claude upstream outage (" + upstreamCat + ")"
		}
	}
	closeFields := map[string]string{
		"agent_state":  state,
		"close_reason": closeReason,
	}
	addLastCoopExitFields(closeFields)
	if err := daemon.CloseBead(ctx, agentBeadID, closeFields); err != nil {
		logInit("warning: failed to close agent bead on exit (%s): %v", closeReason, err)
	} else {
		logInit("closed agent bead %s (%s, state=%s)", agentBeadID, closeReason, state)
	}
}

// interruptedBreadcrumbPostedField is the agent-bead field that records that a
// "I was interrupted" breadcrumb has already been posted for the current open
// interruption cycle. It gates postInterruptedBreadcrumbOnce so a thread that
// is SIGTERM'd and cold-resumed repeatedly against the SAME still-open bead gets
// at most one breadcrumb per cycle (ka-0000002138). It is cleared by
// clearInterruptedBreadcrumbMarker() once a resumed session works again, so a
// genuinely new interruption can post one more time.
const interruptedBreadcrumbPostedField = "interrupted_breadcrumb_posted"

// postInterruptedBreadcrumbOnce stamps the dedup marker BEFORE posting the
// breadcrumb (mark-before-post, ka-0000002080). The original post-then-mark
// order left a window — POST done, marker not yet written, pod hard-killed —
// in which every next teardown incarnation read the marker as empty and
// reposted (one thread got 12 identical breadcrumbs during node-drain churn).
// Marking first shrinks that window to the daemon write itself: a racing
// teardown that reads after the stamp sees the marker and stays silent.
//
// Trade-offs, chosen deliberately:
//   - Stamp succeeds but the POST then fails → the marker is rolled back
//     (best-effort) so a later legitimate teardown can still post. If the
//     rollback also fails, one breadcrumb is lost — one missed beats twelve
//     duplicates.
//   - Stamp FAILS (daemon unreachable during teardown) → still try the POST;
//     the human seeing why the thread went quiet outranks perfect dedup, and
//     the bridge-side reply dedup (bot_threads_dedup.go) collapses any
//     duplicate that slips through this path.
//
// Concurrent teardowns of separate incarnations can still interleave their
// GET/PATCH cycles (the daemon has no compare-and-set on bead fields); the
// bridge-side window dedup is the backstop that holds in that case.
func postInterruptedBreadcrumbOnce(ctx context.Context, agentBeadID string) {
	// Only thread-bound agents post breadcrumbs. Resolving here (rather than
	// inside the POST helper) keeps gardener/crew teardowns free of the
	// stamp+rollback daemon writes they'd otherwise pay on every
	// context_cancelled exit, and avoids resolving twice (the fallback path
	// does a daemon lookup).
	bridgeURL, channel, threadTS, err := resolveSlackEnv()
	if err != nil {
		return
	}
	marked := markInterruptedBreadcrumbPosted(ctx, agentBeadID)
	if !postThreadInterruptedBreadcrumbTo(bridgeURL, channel, threadTS) && marked {
		// The post failed (bridge down, non-200) — undo the premature stamp so
		// a future interruption in this same open cycle is not gagged by a
		// post that never happened.
		clearInterruptedBreadcrumbMarker(ctx, agentBeadID)
	}
}

// postThreadInterruptedBreadcrumbTo posts a single "interrupted mid-response"
// line into the agent's Slack thread when the pod is torn down mid-turn, so an
// agent killed by a node drain, Spot reclaim or rollout does not leave the
// thread silent (ka-0000004614). postInterruptedBreadcrumbOnce resolves the
// thread coordinates (its thread-bound gate) and dedups.
//
// Returns true iff the bridge returned 200, so the caller keeps the dedup
// marker only on a real post. Best-effort: any failure is logged and
// swallowed; posting must never block or fail pod teardown.
func postThreadInterruptedBreadcrumbTo(bridgeURL, channel, threadTS string) bool {
	const text = ":warning: I was interrupted mid-response — my pod was " +
		"reclaimed by the infra before I could finish. Mention me in this " +
		"thread and I'll resume where I left off."

	payload := map[string]string{
		"channel":   channel,
		"thread_ts": threadTS,
		"text":      text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logInit("interrupted-breadcrumb: marshal failed: %v", err)
		return false
	}

	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/threads/reply"
	resp, err := slackHTTPClient().Post(u, "application/json", bytes.NewReader(body))
	if err != nil {
		logInit("interrupted-breadcrumb: POST failed: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logInit("interrupted-breadcrumb: bridge returned HTTP %d", resp.StatusCode)
		return false
	}
	logInit("posted interrupted breadcrumb to Slack thread %s/%s", channel, threadTS)
	return true
}

// markInterruptedBreadcrumbPosted stamps interruptedBreadcrumbPostedField on the
// agent bead so a subsequent involuntary teardown of the SAME still-open bead
// (release / node churn cold-resumes it many times) does not repost the
// breadcrumb. Called BEFORE the Slack POST (ka-0000002080) so racing teardowns
// see the marker as early as possible. Returns whether the stamp durably
// landed, so the caller knows whether a rollback is needed on a failed post.
// Best-effort: a write failure only means a rare duplicate breadcrumb, so it
// is logged and swallowed — it must never block pod teardown.
func markInterruptedBreadcrumbPosted(ctx context.Context, agentBeadID string) bool {
	if agentBeadID == "" || daemon == nil {
		return false
	}
	if err := daemon.UpdateBeadFields(ctx, agentBeadID, map[string]string{
		interruptedBreadcrumbPostedField: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		logInit("interrupted-breadcrumb: failed to stamp dedup marker on %s: %v", agentBeadID, err)
		return false
	}
	return true
}

// clearInterruptedBreadcrumbMarker wipes interruptedBreadcrumbPostedField once a
// resumed session reaches a productive "working" state — the interruption is
// over and the agent is making progress again, so a genuinely NEW interruption
// in this fresh work cycle is allowed to post one more breadcrumb. Best-effort:
// a failure only risks suppressing one future breadcrumb, so it is logged and
// swallowed. No-op (no daemon round-trip) when the marker is already clear.
func clearInterruptedBreadcrumbMarker(ctx context.Context, agentBeadID string) {
	if agentBeadID == "" || daemon == nil {
		return
	}
	existing, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil || existing == nil || existing.Fields[interruptedBreadcrumbPostedField] == "" {
		return
	}
	if err := daemon.UpdateBeadFields(ctx, agentBeadID, map[string]string{
		interruptedBreadcrumbPostedField: "",
	}); err != nil {
		logInit("interrupted-breadcrumb: failed to clear dedup marker on %s: %v", agentBeadID, err)
	}
}
