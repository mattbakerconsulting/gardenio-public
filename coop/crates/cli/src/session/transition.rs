// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! State transition helpers extracted from the session select-loop.
//!
//! Each function is independently testable — it takes the shared [`Store`]
//! plus the minimal set of arguments it needs.

use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use tokio::sync::mpsc;
use tracing::debug;

use crate::config::Config;
use crate::driver::{
    classify_error_detail, AgentState, DetectedState, ErrorCategory, ExitStatus, OptionParser,
    PromptKind, ScreenClassifier,
};
use crate::event::{OutputEvent, TransitionEvent};
use crate::profile::RotateOutcome;
use crate::transport::Store;

use super::groom;
use super::run::SessionState;

/// Action returned by [`process_detected_state`] telling the select-loop what to do.
pub enum DetectAction {
    /// Continue looping.
    Continue,
    /// Break out of the select-loop.
    Break,
}

/// Feed raw backend output into the ring buffer, screen, and broadcast channel.
pub async fn feed_output(store: &Store, bytes: &Bytes) {
    // Write to ring buffer and stamp offset while holding the lock.
    let offset;
    {
        let mut ring = store.terminal.ring.write().await;
        ring.write(bytes);
        offset = ring.total_written() - bytes.len() as u64;
        store
            .terminal
            .ring_total_written
            .store(ring.total_written(), std::sync::atomic::Ordering::Relaxed);
    }
    // Feed screen
    {
        let mut screen = store.terminal.screen.write().await;
        screen.feed(bytes);
    }
    // Broadcast raw output with stamped offset
    let _ = store.channels.output_tx.send(OutputEvent::Raw { data: bytes.clone(), offset });
}

/// Process a detected state change from the composite detector.
///
/// Updates agent_state, handles rate-limiting, broadcasts the transition,
/// spawns prompt enrichment/auto-dismiss, and tracks idle time.
///
/// Returns a [`DetectAction`] telling the select-loop whether to continue or break.
pub async fn process_detected_state(
    store: &Arc<Store>,
    detected: DetectedState,
    session: &mut SessionState,
    option_parser: &Option<OptionParser>,
    screen_classifier: &Option<ScreenClassifier>,
    config: &Config,
) -> DetectAction {
    session.state_seq += 1;
    let mut current = store.driver.agent_state.write().await;
    let prev = current.clone();
    *current = detected.state.clone();
    drop(current);
    session.last_state = detected.state.clone();

    // Mark ready on first transition away from Starting.
    if matches!(prev, AgentState::Starting) && !matches!(detected.state, AgentState::Starting) {
        store.ready.store(true, std::sync::atomic::Ordering::Release);
    }

    // Record transition timestamp for liveness stale-state detection.
    store.lifecycle.last_transition_ms.store(now_epoch_ms(), std::sync::atomic::Ordering::Release);

    // Store error detail + category when entering Error state.
    if let AgentState::Error { ref detail } = detected.state {
        let category = classify_error_detail(detail);
        *store.driver.error.write().await =
            Some(crate::transport::state::ErrorInfo { detail: detail.clone(), category });

        // Mark zombie for fatal, non-recoverable error categories.
        if matches!(
            category,
            ErrorCategory::Unauthorized
                | ErrorCategory::OutOfCredits
                | ErrorCategory::BudgetExceeded
        ) {
            store.lifecycle.zombie.store(true, std::sync::atomic::Ordering::Release);
        }

        // Auto-rotate on rate limit when profiles are registered.
        if category == ErrorCategory::RateLimited {
            handle_rate_limit(Arc::clone(store), session).await;
        }

        // Write marker file for budget exhaustion so hooks can detect it.
        if category == ErrorCategory::BudgetExceeded {
            let account = store.profile.active_name().await.unwrap_or_default();
            write_budget_marker(store, detail, &account);
        }

        // Terminal hard-auth-403 watchdog. A "403 key not allowed to access
        // model" classifies as ErrorCategory::Other (no recoverable category
        // matches), so none of the branches above fire — and an agent that
        // 403s every turn cannot run a single inference turn, so it cannot
        // even process its own `gb stop`. Left alone it spins as a zombie pod
        // holding capacity indefinitely (ka-0000006562). Count consecutive
        // matches; once the threshold trips, park terminally + mark zombie.
        if session.hard_auth_403.record_error(detail) {
            handle_terminal_hard_auth_403(Arc::clone(store), session, detail).await;
        } else if crate::driver::is_hard_auth_403(detail) {
            tracing::warn!(
                "hard-auth-403 #{} (threshold {}): {detail}",
                session.hard_auth_403.consecutive(),
                crate::driver::HARD_AUTH_403_THRESHOLD,
            );
        }

        // Terminal login-required watchdog (ka-0000007394). Symmetric to the
        // hard-auth-403 watchdog above: a "Not logged in · Please run /login"
        // also classifies as ErrorCategory::Other, so none of the recoverable
        // branches fire — and an agent with no usable credential cannot run a
        // single inference turn, so it cannot even process its own `gb stop`.
        // Left alone it nudges into the void every turn and burns cron wake-up
        // after cron wake-up (33 in the original incident). Feeding the
        // watchdog on every Error also keeps it correctly reset: a non-login
        // error detail (e.g. the 403 above) resets the login counter to zero.
        // Threshold > 1 tolerates the transient credential-switch blip.
        if session.login_required.record_error(detail) {
            handle_terminal_login_required(Arc::clone(store), session, detail).await;
        } else if crate::driver::is_login_required(detail) {
            tracing::warn!(
                "login-required #{} (threshold {}): {detail}",
                session.login_required.consecutive(),
                crate::driver::LOGIN_REQUIRED_THRESHOLD,
            );
        }
    } else {
        // Clear zombie on recovery to a productive state.
        if matches!(detected.state, AgentState::Working | AgentState::Idle) {
            store.lifecycle.zombie.store(false, std::sync::atomic::Ordering::Release);
        }

        // A genuine successful turn (reached Idle from Working with no error
        // surfaced in last_message, handled below) resets the consecutive
        // hard-auth-403 counter. Reset is gated on the clean-idle check below
        // rather than on every non-Error state so the Working→Error→Working…
        // cycle of a perpetually-403ing agent does not clear it every turn.

        // Claude Code sometimes reports rate limits inline: the agent goes
        // working → idle with the error in `last_message` instead of
        // transitioning to Error state. Detect and handle that here.
        if matches!(detected.state, AgentState::Idle) && matches!(prev, AgentState::Working) {
            let last_msg = store.driver.last_message.read().await.clone();
            match last_msg {
                // Hard-auth-403 surfaced inline (Working→Idle with the 403 in
                // last_message instead of a distinct Error state). Feed the
                // same watchdog so the inline path counts toward the threshold.
                Some(ref msg) if crate::driver::is_hard_auth_403(msg) => {
                    // Cross-reset the OTHER watchdog: this turn was a 403, not a
                    // login-required, so any accumulated login streak is broken
                    // (a non-login error resets the run). This keeps the inline
                    // path's reset semantics identical to the Error-state path
                    // above, which feeds every detail to BOTH watchdogs — see
                    // the login_and_403_watchdogs_are_independent test.
                    session.login_required.record_recovery();
                    if session.hard_auth_403.record_error(msg) {
                        handle_terminal_hard_auth_403(Arc::clone(store), session, msg).await;
                    } else {
                        tracing::warn!(
                            "hard-auth-403 #{} (inline, threshold {}): {msg}",
                            session.hard_auth_403.consecutive(),
                            crate::driver::HARD_AUTH_403_THRESHOLD,
                        );
                    }
                }
                // Login-required surfaced inline (Working→Idle with the
                // "Not logged in" banner in last_message rather than a distinct
                // Error state — the common Tier-2 log shape, since a text-only
                // assistant message classifies as Idle). Feed the same watchdog
                // so the inline path counts toward the threshold (ka-0000007394).
                Some(ref msg) if crate::driver::is_login_required(msg) => {
                    // Cross-reset the OTHER watchdog (symmetric to the 403 arm):
                    // a login-required turn breaks any accumulated 403 streak.
                    session.hard_auth_403.record_recovery();
                    if session.login_required.record_error(msg) {
                        handle_terminal_login_required(Arc::clone(store), session, msg).await;
                    } else {
                        tracing::warn!(
                            "login-required #{} (inline, threshold {}): {msg}",
                            session.login_required.consecutive(),
                            crate::driver::LOGIN_REQUIRED_THRESHOLD,
                        );
                    }
                }
                Some(ref msg) => {
                    // Any other inline trailer (rate-limit, budget, server
                    // error, or ordinary text) is NOT a 403 / login-required
                    // turn, so it breaks both consecutive auth-failure streaks —
                    // mirroring the Error-state path, which feeds every
                    // non-matching detail to both watchdogs (resetting them).
                    session.hard_auth_403.record_recovery();
                    session.login_required.record_recovery();
                    let category = classify_error_detail(msg);
                    if category == ErrorCategory::RateLimited {
                        tracing::warn!(
                            "rate limit detected in last_message on idle transition: {msg}"
                        );
                        *store.driver.error.write().await =
                            Some(crate::transport::state::ErrorInfo {
                                detail: msg.clone(),
                                category,
                            });
                        handle_rate_limit(Arc::clone(store), session).await;
                    } else if category == ErrorCategory::ModelRateLimited {
                        // Model-group pool-exhaustion 429 surfaced inline
                        // (Working→Idle with the no-fallback marker in
                        // last_message rather than a distinct Error state).
                        // Store the category so the controller's
                        // `model_rate_limited` branch parks in place, but do
                        // NOT call handle_rate_limit — rotation cannot clear a
                        // model-level throttle (ka-0000001718). No budget marker
                        // either: this is not a zombie-making budget cap.
                        tracing::warn!(
                            "model-group rate limit (pool exhausted, no fallback) detected in \
                             last_message on idle transition — parking in place, NOT rotating: {msg}"
                        );
                        *store.driver.error.write().await =
                            Some(crate::transport::state::ErrorInfo {
                                detail: msg.clone(),
                                category,
                            });
                    } else if category == ErrorCategory::BudgetExceeded {
                        tracing::warn!(
                            "budget exceeded detected in last_message on idle transition: {msg}"
                        );
                        *store.driver.error.write().await =
                            Some(crate::transport::state::ErrorInfo {
                                detail: msg.clone(),
                                category,
                            });
                        let account = store.profile.active_name().await.unwrap_or_default();
                        write_budget_marker(store, msg, &account);
                    }
                    // (Both auth-failure streaks were already reset at the top of
                    // this arm — any non-403/non-login trailer, including a clean
                    // productive turn, breaks a consecutive run.)
                }
                // Reached Idle from Working with no message at all → a clean,
                // productive turn. Reset the consecutive 403 / login counters.
                None => {
                    session.hard_auth_403.record_recovery();
                    session.login_required.record_recovery();
                }
            }
        }
        if !matches!(detected.state, AgentState::Idle) || store.driver.error.read().await.is_none()
        {
            *store.driver.error.write().await = None;
        }
    }

    // Store metadata for the HTTP/gRPC API.
    store.driver.state_seq.store(session.state_seq, std::sync::atomic::Ordering::Release);
    *store.driver.detection.write().await = crate::transport::state::DetectionInfo {
        tier: detected.tier,
        cause: detected.cause.clone(),
    };

    let last_message = store.driver.last_message.read().await.clone();
    let _ = store.channels.state_tx.send(TransitionEvent {
        prev,
        next: detected.state.clone(),
        seq: session.state_seq,
        cause: detected.cause,
        last_message,
    });

    // Spawn deferred option enrichment for Permission/Plan prompts.
    if let AgentState::Prompt { ref prompt } = detected.state {
        if matches!(prompt.kind, PromptKind::Permission | PromptKind::Plan) {
            if let Some(ref parser) = option_parser {
                groom::spawn_enrichment(store, session.state_seq, parser);
            }
        }
    }

    // Auto-dismiss disruption prompts in groom=auto mode.
    if let AgentState::Prompt { ref prompt } = detected.state {
        groom::spawn_auto_dismiss(store, prompt, config, session.state_seq, screen_classifier);

        // A prompt with no subtype carries no dismiss policy — the Tier-1 hook
        // path emits exactly that for `Notification(permission_prompt)`. When
        // the thing on screen is really a screen-classified dialog (a Fable
        // consent gate, a weekly-limit dialog, a protected-dir edit prompt),
        // nothing above can answer it and the Tier-5 detector will not re-emit
        // it either, because it dedupes on its own last-emitted state and the
        // wedged screen is static. Re-run the classifier on the current screen
        // so the dialog gets a subtype — and therefore an answer (ka-0000002659).
        if prompt.subtype.is_none() {
            if let Some(ref classifier) = screen_classifier {
                groom::spawn_reclassify(store, config, session.state_seq, classifier);
            }
        }
    }

    // Track idle time for idle_timeout.
    if matches!(detected.state, AgentState::Idle) && session.idle_timeout > Duration::ZERO {
        if session.idle_since.is_none() {
            session.idle_since = Some(tokio::time::Instant::now());
        }
    } else {
        session.idle_since = None;
    }

    // Switch check: agent reached idle during pending switch → SIGHUP now.
    if session.pending_switch.is_some() && matches!(detected.state, AgentState::Idle) {
        debug!("switch: agent reached idle, sending SIGHUP");
        let cause = if session.pending_switch.as_ref().is_some_and(|r| r.credentials.is_some()) {
            "switch"
        } else {
            "restart"
        };
        session.switch_deadline = None; // cancel timeout — normal path succeeded
        broadcast_restarting(store, session, cause).await;
        sighup_child_group(store);
    }

    // Drain check: agent reached idle during drain → kill now.
    if session.drain_deadline.is_some() && matches!(detected.state, AgentState::Idle) {
        debug!("drain: agent reached idle, sending SIGTERM");
        sigterm_child_group(store).await;
        return DetectAction::Break;
    }

    DetectAction::Continue
}

/// Handle a rate-limit error by attempting profile rotation or parking.
async fn handle_rate_limit(store: Arc<Store>, session: &mut SessionState) {
    match store.profile.try_auto_rotate().await {
        RotateOutcome::Switch(req) => {
            let _ = store.switch.switch_tx.try_send(req);
        }
        RotateOutcome::Exhausted { retry_after } => {
            let resume_at = now_epoch_ms() + retry_after.as_millis() as u64;
            let parked = AgentState::Parked {
                reason: "all_profiles_rate_limited".into(),
                resume_at_epoch_ms: resume_at,
            };
            session.state_seq += 1;
            let mut current = store.driver.agent_state.write().await;
            let prev = current.clone();
            *current = parked.clone();
            drop(current);
            session.last_state = parked.clone();
            store.driver.state_seq.store(session.state_seq, std::sync::atomic::Ordering::Release);
            let last_message = store.driver.last_message.read().await.clone();
            let _ = store.channels.state_tx.send(TransitionEvent {
                prev,
                next: parked,
                seq: session.state_seq,
                cause: "all_profiles_rate_limited".to_owned(),
                last_message,
            });
            store.profile.schedule_retry(retry_after, store.clone());
        }
        RotateOutcome::Skipped => {}
    }
}

/// Parked-reason prefix written when the terminal hard-auth-403 watchdog
/// trips. The controller's restart loop matches on this prefix to close the
/// agent bead with a clear failure reason and tear the pod down instead of
/// letting it spin idle (ka-0000006562). Kept in sync with the controller's
/// `terminalHardAuth403ParkPrefix`.
pub const TERMINAL_HARD_AUTH_403_PARK_PREFIX: &str = "terminal_hard_auth_403";

/// Handle a terminal (threshold-reached) hard-auth-403 by parking the session
/// permanently and marking it a zombie.
///
/// Unlike [`handle_rate_limit`]'s park, this is **not** recoverable: there is
/// no `resume_at` in the future and no retry is scheduled. The session has
/// 403'd on every one of the last [`HARD_AUTH_403_THRESHOLD`] turns, so the
/// key/model-allowlist mismatch will not clear by waiting. We:
///   * transition to `AgentState::Parked` with a descriptive reason so the
///     controller can read it off `/api/v1/agent` and close the bead;
///   * set `lifecycle.zombie` so the `/livez` probe returns 503 and K8s
///     recycles the pod even if the controller path is unavailable;
///   * write a marker file so hooks / post-mortems can detect it.
///
/// [`HARD_AUTH_403_THRESHOLD`]: crate::driver::HARD_AUTH_403_THRESHOLD
async fn handle_terminal_hard_auth_403(
    store: Arc<Store>,
    session: &mut SessionState,
    detail: &str,
) {
    let threshold = crate::driver::HARD_AUTH_403_THRESHOLD;
    tracing::error!(
        "TERMINAL hard-auth-403: {threshold} consecutive '403 not allowed to access model' \
         errors — parking + marking zombie so the pod is recycled instead of zombied: {detail}"
    );

    let reason = format!(
        "{TERMINAL_HARD_AUTH_403_PARK_PREFIX}: key not allowed to access model after \
         {threshold} consecutive turns"
    );
    let parked = AgentState::Parked {
        // resume_at in the past: this park is terminal, not a timed cooldown.
        reason: reason.clone(),
        resume_at_epoch_ms: now_epoch_ms(),
    };

    session.state_seq += 1;
    let mut current = store.driver.agent_state.write().await;
    let prev = current.clone();
    *current = parked.clone();
    drop(current);
    session.last_state = parked.clone();
    store.driver.state_seq.store(session.state_seq, std::sync::atomic::Ordering::Release);

    // Mark zombie so /livez 503s and K8s recycles the pod as a backstop even
    // if the controller never reads the parked_reason.
    store.lifecycle.zombie.store(true, std::sync::atomic::Ordering::Release);

    let last_message = store.driver.last_message.read().await.clone();
    let _ = store.channels.state_tx.send(TransitionEvent {
        prev,
        next: parked,
        seq: session.state_seq,
        cause: reason.clone(),
        last_message,
    });

    write_hard_auth_403_marker(&store, detail, &reason);
}

/// Parked-reason prefix written when the terminal login-required watchdog
/// trips. The controller's restart loop matches on this prefix to close the
/// agent bead with a clear `not_logged_in` failure reason and tear the pod down
/// instead of letting it nudge into the void (ka-0000007394). Kept in sync with
/// the controller's `terminalNotLoggedInParkPrefix`.
pub const TERMINAL_LOGIN_REQUIRED_PARK_PREFIX: &str = "terminal_login_required";

/// Handle a terminal (threshold-reached) login-required condition by parking
/// the session permanently and marking it a zombie.
///
/// Symmetric to [`handle_terminal_hard_auth_403`]. This is the
/// credential-not-ready-at-spawn failure (ka-0000007394): the session has hit
/// "Not logged in · Please run /login" on every one of the last
/// [`LOGIN_REQUIRED_THRESHOLD`] turns with zero intervening successful
/// inference, so no usable credential reached the pod and waiting/rotating will
/// not help. We:
///   * transition to `AgentState::Parked` with a descriptive reason so the
///     controller can read it off `/api/v1/agent` and close the bead;
///   * set `lifecycle.zombie` so the `/livez` probe returns 503 and K8s
///     recycles the pod even if the controller path is unavailable;
///   * write a marker file so hooks / post-mortems can detect it.
///
/// [`LOGIN_REQUIRED_THRESHOLD`]: crate::driver::LOGIN_REQUIRED_THRESHOLD
async fn handle_terminal_login_required(
    store: Arc<Store>,
    session: &mut SessionState,
    detail: &str,
) {
    let threshold = crate::driver::LOGIN_REQUIRED_THRESHOLD;
    tracing::error!(
        "TERMINAL login-required: {threshold} consecutive 'Not logged in' turns — \
         credential not ready at spawn; parking + marking zombie so the pod is recycled \
         instead of nudging into the void: {detail}"
    );

    let reason = format!(
        "{TERMINAL_LOGIN_REQUIRED_PARK_PREFIX}: not logged in after \
         {threshold} consecutive turns (credential not ready at spawn)"
    );
    let parked = AgentState::Parked {
        // resume_at in the past: this park is terminal, not a timed cooldown.
        reason: reason.clone(),
        resume_at_epoch_ms: now_epoch_ms(),
    };

    session.state_seq += 1;
    let mut current = store.driver.agent_state.write().await;
    let prev = current.clone();
    *current = parked.clone();
    drop(current);
    session.last_state = parked.clone();
    store.driver.state_seq.store(session.state_seq, std::sync::atomic::Ordering::Release);

    // Mark zombie so /livez 503s and K8s recycles the pod as a backstop even
    // if the controller never reads the parked_reason.
    store.lifecycle.zombie.store(true, std::sync::atomic::Ordering::Release);

    let last_message = store.driver.last_message.read().await.clone();
    let _ = store.channels.state_tx.send(TransitionEvent {
        prev,
        next: parked,
        seq: session.state_seq,
        cause: reason.clone(),
        last_message,
    });

    write_login_required_marker(&store, detail, &reason);
}

/// Write a terminal login-required marker file, mirroring
/// [`write_hard_auth_403_marker`] so hooks and external consumers can detect the
/// condition off the filesystem.
fn write_login_required_marker(store: &Store, detail: &str, reason: &str) {
    let marker = serde_json::json!({
        "error": "terminal_login_required",
        "reason": reason,
        "detail": detail,
        "threshold": crate::driver::LOGIN_REQUIRED_THRESHOLD,
        "timestamp": now_epoch_ms(),
        "agent": std::env::var("BOAT_AGENT").unwrap_or_default(),
        "project": std::env::var("BOAT_PROJECT").unwrap_or_default(),
    });
    let payload = match serde_json::to_vec_pretty(&marker) {
        Ok(p) => p,
        Err(e) => {
            tracing::warn!("login_required_marker: serialize failed: {e}");
            return;
        }
    };

    if let Some(ref dir) = store.session_dir {
        let path = dir.join("login_required.json");
        if let Err(e) = std::fs::write(&path, &payload) {
            tracing::warn!("login_required_marker: write to {}: {e}", path.display());
        }
    }

    let state_home = std::env::var("XDG_STATE_HOME").unwrap_or_else(|_| {
        std::env::var("HOME").map(|h| format!("{h}/.local/state")).unwrap_or_default()
    });
    if !state_home.is_empty() {
        let dir = std::path::PathBuf::from(&state_home).join("coop");
        let _ = std::fs::create_dir_all(&dir);
        let path = dir.join("login_required.json");
        if let Err(e) = std::fs::write(&path, &payload) {
            tracing::warn!("login_required_marker: write to {}: {e}", path.display());
        }
    }
}

/// Write a terminal hard-auth-403 marker file, mirroring [`write_budget_marker`]
/// so hooks and external consumers can detect the condition off the filesystem.
fn write_hard_auth_403_marker(store: &Store, detail: &str, reason: &str) {
    let marker = serde_json::json!({
        "error": "terminal_hard_auth_403",
        "reason": reason,
        "detail": detail,
        "threshold": crate::driver::HARD_AUTH_403_THRESHOLD,
        "timestamp": now_epoch_ms(),
        "agent": std::env::var("BOAT_AGENT").unwrap_or_default(),
        "project": std::env::var("BOAT_PROJECT").unwrap_or_default(),
    });
    let payload = match serde_json::to_vec_pretty(&marker) {
        Ok(p) => p,
        Err(e) => {
            tracing::warn!("hard_auth_403_marker: serialize failed: {e}");
            return;
        }
    };

    if let Some(ref dir) = store.session_dir {
        let path = dir.join("hard_auth_403.json");
        if let Err(e) = std::fs::write(&path, &payload) {
            tracing::warn!("hard_auth_403_marker: write to {}: {e}", path.display());
        }
    }

    let state_home = std::env::var("XDG_STATE_HOME").unwrap_or_else(|_| {
        std::env::var("HOME").map(|h| format!("{h}/.local/state")).unwrap_or_default()
    });
    if !state_home.is_empty() {
        let dir = std::path::PathBuf::from(&state_home).join("coop");
        let _ = std::fs::create_dir_all(&dir);
        let path = dir.join("hard_auth_403.json");
        if let Err(e) = std::fs::write(&path, &payload) {
            tracing::warn!("hard_auth_403_marker: write to {}: {e}", path.display());
        }
    }
}

/// Broadcast an `AgentState::Restarting` transition and update tracking state.
pub async fn broadcast_restarting(store: &Store, session: &mut SessionState, cause: &str) {
    session.state_seq += 1;
    let mut current = store.driver.agent_state.write().await;
    let prev = current.clone();
    *current = AgentState::Restarting;
    drop(current);
    session.last_state = AgentState::Restarting;
    let last_message = store.driver.last_message.read().await.clone();
    let _ = store.channels.state_tx.send(TransitionEvent {
        prev,
        next: AgentState::Restarting,
        seq: session.state_seq,
        cause: cause.to_owned(),
        last_message,
    });
}

/// Drain any pending output so final bytes are captured.
pub async fn drain_pending_output(store: &Store, rx: &mut mpsc::Receiver<Bytes>) {
    while let Ok(bytes) = rx.try_recv() {
        feed_output(store, &bytes).await;
    }
}

/// Store exit status and broadcast the `Exited` state transition.
pub async fn broadcast_exit(store: &Store, status: ExitStatus, state_seq: &mut u64) {
    // ORDERING: exit_status must be written before agent_state so that
    // any reader who observes AgentState::Exited is guaranteed to find
    // exit_status populated.
    {
        let mut exit = store.terminal.exit_status.write().await;
        *exit = Some(status);
    }
    let mut current = store.driver.agent_state.write().await;
    let prev = current.clone();
    *current = AgentState::Exited { status };
    drop(current);
    *state_seq += 1;

    // Mark zombie — the agent process has exited and the pod should be recycled.
    store.lifecycle.zombie.store(true, std::sync::atomic::Ordering::Release);
    store.lifecycle.last_transition_ms.store(now_epoch_ms(), std::sync::atomic::Ordering::Release);

    let last_message = store.driver.last_message.read().await.clone();
    let _ = store.channels.state_tx.send(TransitionEvent {
        prev,
        next: AgentState::Exited { status },
        seq: *state_seq,
        cause: String::new(),
        last_message,
    });
}

/// Send SIGHUP to the child process group.
///
/// Used for restart/switch paths where we want the child to exit because
/// its controlling terminal has gone away. Bash does not run TERM/INT
/// traps for SIGHUP, so this should NOT be used for graceful shutdown
/// when the child may have a SIGTERM trap installed.
pub fn sighup_child_group(store: &Store) {
    let pid = store.terminal.child_pid.load(std::sync::atomic::Ordering::Acquire);
    if pid != 0 {
        let _ = kill(Pid::from_raw(-(pid as i32)), Signal::SIGHUP);
    }
}

/// Send SIGTERM to the child process group and any descendant pgroups
/// that share the PTY child's session.
///
/// Used for graceful shutdown so a child shell's `trap '...' TERM` runs
/// before exit. The child PID was captured by forkpty, which placed the
/// child in its own session/process-group, so signaling `-pid` reaches
/// the immediate pgroup without affecting coop itself.
///
/// To make sure the leaf bash trap actually runs, we walk `/proc` and
/// signal every process in the PTY child's session except the session
/// leader itself, BEFORE signaling the immediate pgroup. Two cases this
/// covers:
///
/// 1. Wrappers like claudeless that call `setpgid` on their subprocesses
///    — those descendants live in a different pgroup but the same session
///    (ka-0000001002).
///
/// 2. `sh -c "bash <script>"` shape used by `gb agent start --local`,
///    where sh, bash, and any leaf children share the same pgroup as
///    sh (the session leader). If we only signal the pgroup, the kernel
///    SIGHUP-cascade triggered by sh's exit reaches bash before its
///    TERM trap can fire (ka-0000004679).
///
/// Ordering: descendants (leaves) are signaled FIRST, then a short grace,
/// then the immediate pgroup. This gives leaf shells time to run their
/// SIGTERM trap before the session leader's exit triggers a SIGHUP
/// cascade against the rest of the session.
pub async fn sigterm_child_group(store: &Store) {
    let pid = store.terminal.child_pid.load(std::sync::atomic::Ordering::Acquire);
    if pid == 0 {
        return;
    }
    // Signal leaf descendants first so their trap handlers run before the
    // session leader's exit cascades SIGHUP through the remaining session.
    sigterm_session_descendants(pid);
    // Grace so the leaf bash trap (typically `echo ... >> $MARKER; exit`)
    // can fire before we signal the session leader / immediate pgroup.
    // 1s is comfortably longer than a sleep(1) in the leaf script + a
    // single fs write, while staying well inside the harness's SIGTERM
    // grace window.
    tokio::time::sleep(Duration::from_secs(1)).await;
    // Then signal the immediate pgroup (the PTY child / session leader).
    let _ = kill(Pid::from_raw(-(pid as i32)), Signal::SIGTERM);
}

/// SIGTERM every non-leader process whose session id == `session_pid`.
///
/// The session leader itself is skipped because (a) it's signaled by the
/// follow-up `kill(-pgrp)` and (b) when it exits, the kernel sends SIGHUP
/// to every other process in the session — we want that cascade to land
/// AFTER leaves have run their TERM trap, not concurrently with it.
///
/// This intentionally signals descendants regardless of their pgrp: the
/// `sh -c "bash …"` shape used by `gb agent start --local` leaves sh,
/// bash, and any leaf children all sharing sh's pgrp, so a pgrp-only
/// filter (the original ka-0000001002 implementation) skipped the very
/// leaves it was meant to pre-signal (ka-0000004679).
///
/// Best-effort: silently ignores read errors on `/proc/*/stat` (race with
/// process exit, permission denied, etc.).
fn sigterm_session_descendants(session_pid: u32) {
    let entries = match std::fs::read_dir("/proc") {
        Ok(e) => e,
        Err(_) => return,
    };
    for entry in entries.flatten() {
        let file_name = entry.file_name();
        let name = match file_name.to_str() {
            Some(n) => n,
            None => continue,
        };
        let target_pid: u32 = match name.parse() {
            Ok(p) => p,
            Err(_) => continue,
        };
        if target_pid == session_pid {
            continue;
        }
        let stat = match std::fs::read_to_string(entry.path().join("stat")) {
            Ok(s) => s,
            Err(_) => continue,
        };
        // /proc/<pid>/stat layout: pid (comm) state ppid pgrp sid ...
        // comm may contain spaces and parens, so split on the trailing ')'.
        let rparen = match stat.rfind(')') {
            Some(i) => i,
            None => continue,
        };
        let rest: Vec<&str> = stat[rparen + 1..].split_whitespace().collect();
        // After ')' the fields are: state(0) ppid(1) pgrp(2) sid(3) ...
        if rest.len() < 4 {
            continue;
        }
        let sid: i32 = rest[3].parse().unwrap_or(0);
        if sid as u32 == session_pid {
            let _ = kill(Pid::from_raw(target_pid as i32), Signal::SIGTERM);
        }
    }
}

/// Write a budget_exceeded marker file for hooks and external consumers.
///
/// Writes to `<session_dir>/budget_exceeded.json` (if session_dir is set)
/// and to `$XDG_STATE_HOME/coop/budget_exceeded.json` as a well-known path.
fn write_budget_marker(store: &Store, detail: &str, account: &str) {
    let marker = serde_json::json!({
        "error": "budget_exceeded",
        "detail": detail,
        "timestamp": now_epoch_ms(),
        "agent": std::env::var("BOAT_AGENT").unwrap_or_default(),
        "project": std::env::var("BOAT_PROJECT").unwrap_or_default(),
        "account": account,
    });
    let payload = match serde_json::to_vec_pretty(&marker) {
        Ok(p) => p,
        Err(e) => {
            tracing::warn!("budget_marker: serialize failed: {e}");
            return;
        }
    };

    // Write to session directory.
    if let Some(ref dir) = store.session_dir {
        let path = dir.join("budget_exceeded.json");
        if let Err(e) = std::fs::write(&path, &payload) {
            tracing::warn!("budget_marker: write to {}: {e}", path.display());
        }
    }

    // Write to XDG state directory as a well-known path.
    let state_home = std::env::var("XDG_STATE_HOME").unwrap_or_else(|_| {
        std::env::var("HOME").map(|h| format!("{h}/.local/state")).unwrap_or_default()
    });
    if !state_home.is_empty() {
        let dir = std::path::PathBuf::from(&state_home).join("coop");
        let _ = std::fs::create_dir_all(&dir);
        let path = dir.join("budget_exceeded.json");
        if let Err(e) = std::fs::write(&path, &payload) {
            tracing::warn!("budget_marker: write to {}: {e}", path.display());
        }
    }
}

/// Return the current UTC time as milliseconds since the Unix epoch.
pub fn now_epoch_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

#[cfg(test)]
#[path = "transition_login_tests.rs"]
mod transition_login_tests;
