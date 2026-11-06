// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Session struct and the core `tokio::select!` loop.

use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tokio::sync::mpsc;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;
use tracing::{debug, warn};

use crate::backend::BackendInput;
use crate::config::Config;
use crate::driver::{
    AgentState, CompositeDetector, DetectedState, ExitStatus, HardAuth403Watchdog,
    LoginRequiredWatchdog, OptionParser, ScreenClassifier,
};
use crate::event::InputEvent;
use crate::switch::SwitchRequest;
use crate::transport::Store;

use super::transition::{self, DetectAction};
use super::{SessionConfig, SessionOutcome};

/// Mutable state tracked across iterations of the session select-loop.
pub struct SessionState {
    pub state_seq: u64,
    pub last_state: AgentState,
    pub idle_since: Option<tokio::time::Instant>,
    pub idle_timeout: Duration,
    pub pending_switch: Option<SwitchRequest>,
    pub switch_deadline: Option<tokio::time::Instant>,
    pub drain_deadline: Option<tokio::time::Instant>,
    /// Tracks consecutive terminal hard-auth-403 errors so a session that
    /// 403s every inference turn ("403 key not allowed to access model") is
    /// parked + marked zombie instead of spinning idle forever (ka-0000006562).
    pub hard_auth_403: HardAuth403Watchdog,
    /// Tracks consecutive "Not logged in" / login-required turns so a session
    /// spawned without a usable credential (credential-not-ready-at-spawn) is
    /// parked + marked zombie instead of nudging into the void every turn and
    /// burning cron wake-ups (ka-0000007394).
    pub login_required: LoginRequiredWatchdog,
}

/// Core session that runs the select-loop multiplexer.
pub struct Session {
    store: Arc<Store>,
    backend_output_rx: mpsc::Receiver<Bytes>,
    backend_input_tx: mpsc::Sender<BackendInput>,
    resize_tx: mpsc::Sender<(u16, u16)>,
    detector_rx: mpsc::Receiver<DetectedState>,
    shutdown: CancellationToken,
    backend_handle: JoinHandle<anyhow::Result<ExitStatus>>,
    option_parser: Option<OptionParser>,
    screen_classifier: Option<ScreenClassifier>,
}

impl Session {
    /// Build and start a new session.
    ///
    /// Steps:
    /// 1. Sets initial PID on AppState
    /// 2. Sets initial terminal size via `backend.resize()`
    /// 3. Spawns backend.run() on a separate task
    /// 4. Spawns all detectors
    pub fn new(config: &Config, session: SessionConfig) -> Self {
        let SessionConfig {
            mut backend,
            detectors,
            store,
            shutdown,
            option_parser,
            screen_classifier,
        } = session;

        // Set initial PID (Release so signal-delivery loads with Acquire see it)
        if let Some(pid) = backend.child_pid() {
            store.terminal.child_pid.store(pid, std::sync::atomic::Ordering::Release);
        }

        // Set initial terminal size
        let _ = backend.resize(config.cols, config.rows);

        // Create backend I/O channels
        let (backend_output_tx, backend_output_rx) = mpsc::channel(256);
        let (backend_input_tx, backend_input_rx) = mpsc::channel::<BackendInput>(256);
        let (resize_tx, resize_rx) = mpsc::channel(4);

        // Spawn backend task
        let backend_handle = tokio::spawn(async move {
            backend.run(backend_output_tx, backend_input_rx, resize_rx).await
        });

        // Build and spawn the composite detector (tier resolution + dedup).
        let (detector_tx, detector_rx) = mpsc::channel(64);
        let composite = CompositeDetector { tiers: detectors };
        let detector_shutdown = shutdown.clone();
        tokio::spawn(composite.run(detector_tx, detector_shutdown));

        Self {
            store,
            backend_output_rx,
            backend_input_tx,
            resize_tx,
            detector_rx,
            shutdown,
            backend_handle,
            option_parser,
            screen_classifier,
        }
    }

    /// Run the session without switch support, returning the exit status.
    ///
    /// Convenience wrapper for tests that don't need credential switching.
    pub async fn run_to_exit(
        self,
        config: &Config,
        input_rx: &mut mpsc::Receiver<InputEvent>,
    ) -> anyhow::Result<ExitStatus> {
        let (_, mut switch_rx) = mpsc::channel(1);
        match self.run(config, input_rx, &mut switch_rx).await? {
            SessionOutcome::Exit(status) => Ok(status),
            SessionOutcome::Switch(_) => anyhow::bail!("unexpected switch during run_to_exit"),
        }
    }

    /// Run the session loop until the backend exits or shutdown is triggered.
    ///
    /// Receivers are borrowed — the caller retains ownership so they survive
    /// across switch iterations in [`PreparedSession::run`].
    pub async fn run(
        mut self,
        config: &Config,
        input_rx: &mut mpsc::Receiver<InputEvent>,
        switch_rx: &mut mpsc::Receiver<SwitchRequest>,
    ) -> anyhow::Result<SessionOutcome> {
        let shutdown_timeout = config.shutdown_timeout();
        let graceful_timeout = config.drain_timeout();
        let mut screen_debounce = tokio::time::interval(config.screen_debounce());
        let mut next_escape_at: Option<tokio::time::Instant> = None;
        let mut switch_open = true;

        let switch_timeout = config.switch_timeout();
        let mut state = SessionState {
            state_seq: 0,
            last_state: AgentState::Starting,
            idle_since: None,
            idle_timeout: config.idle_timeout(),
            pending_switch: None,
            switch_deadline: None,
            drain_deadline: None,
            hard_auth_403: HardAuth403Watchdog::new(),
            login_required: LoginRequiredWatchdog::new(),
        };

        loop {
            tokio::select! {
                // 1. Backend output → feed screen, write ring buffer, broadcast
                data = self.backend_output_rx.recv() => {
                    match data {
                        Some(bytes) => {
                            transition::feed_output(&self.store, &bytes).await;
                        }
                        None => break,
                    }
                }

                // 2. Consumer input → forward to backend or handle resize/signal
                event = input_rx.recv() => {
                    if self.handle_input(event).await {
                        break;
                    }
                }

                // 3. Detector state changes
                detected = self.detector_rx.recv() => {
                    if let Some(detected) = detected {
                        let action = transition::process_detected_state(
                            &self.store,
                            detected,
                            &mut state,
                            &self.option_parser,
                            &self.screen_classifier,
                            config,
                        ).await;
                        if matches!(action, DetectAction::Break) {
                            break;
                        }
                    }
                }

                // 4. Screen debounce timer → render once and broadcast a shared snapshot.
                _ = screen_debounce.tick() => {
                    let mut screen = self.store.terminal.screen.write().await;
                    let changed = screen.changed();
                    if changed {
                        screen.clear_changed();
                        let snapshot = screen.snapshot_text();
                        drop(screen);
                        let _ = self.store.channels.screen_tx.send(snapshot);
                    }
                }

                // 5. Idle timeout → trigger shutdown when idle too long
                _ = async {
                    match state.idle_since {
                        Some(since) => tokio::time::sleep_until(since + state.idle_timeout).await,
                        None => std::future::pending().await,
                    }
                }, if state.idle_since.is_some() => {
                    debug!("idle timeout reached, triggering idle-suspend shutdown");
                    // Mark this as an idle-suspend (not a crash / gb-stop /
                    // switch) so the terminal exit path stamps the S3 session
                    // meta exit_status as "suspended": the agent sat idle past
                    // COOP_IDLE_TIMEOUT_MS with its snapshot intact, and the
                    // controller should reclaim its pod onto the warm pool
                    // rather than treat the teardown as a failure or a clean
                    // stop (Phase 1 snapshot-suspend, ka-0000002921).
                    self.store
                        .lifecycle
                        .idle_suspended
                        .store(true, std::sync::atomic::Ordering::Release);
                    // Forward SIGTERM to the child group so it exits cleanly
                    // (code 143, a trapped/normal signal exit) instead of being
                    // reaped by the post-loop SIGKILL grace-timeout (code 137,
                    // scored "error"). exit_status_from_code maps 143 with the
                    // idle_suspended bit set to "suspended"; a SIGKILL 137 would
                    // stay "error" and defeat the reclaim signal. Same discipline
                    // as the drain-deadline branch below.
                    transition::sigterm_child_group(&self.store).await;
                    self.shutdown.cancel();
                    break;
                }

                // 6. Drain escape ticker — periodically send Escape during drain
                _ = async {
                    match next_escape_at {
                        Some(at) => tokio::time::sleep_until(at).await,
                        None => std::future::pending().await,
                    }
                }, if next_escape_at.is_some() => {
                    debug!("drain: sending Escape");
                    let esc = Bytes::from_static(b"\x1b");
                    self.store.lifecycle.bytes_written.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    self.store.input_activity.notify_waiters();
                    let _ = self.backend_input_tx.send(BackendInput::Write(esc)).await;
                    next_escape_at = Some(tokio::time::Instant::now() + Duration::from_secs(2));
                }

                // 7. Drain deadline — force-kill after graceful timeout
                _ = async {
                    match state.drain_deadline {
                        Some(deadline) => tokio::time::sleep_until(deadline).await,
                        None => std::future::pending().await,
                    }
                }, if state.drain_deadline.is_some() => {
                    debug!("drain: deadline reached, force-killing");
                    transition::sigterm_child_group(&self.store).await;
                    break;
                }

                // 8. Switch request from transport
                req = switch_rx.recv(), if state.pending_switch.is_none() && switch_open => {
                    match req {
                        Some(req) => {
                            if matches!(state.last_state, AgentState::Exited { .. }) {
                                state.pending_switch = Some(req);
                                break;
                            }
                            if req.force || matches!(state.last_state, AgentState::Idle) {
                                let cause = if req.credentials.is_some() { "switch" } else { "restart" };
                                state.pending_switch = Some(req);
                                transition::broadcast_restarting(&self.store, &mut state, cause).await;
                                transition::sighup_child_group(&self.store);
                            } else {
                                state.pending_switch = Some(req);
                                // Set a deadline so the switch fires even if the
                                // agent never reaches Idle (e.g. rate-limit stuck
                                // in Working due to nudge-originated log entries).
                                if switch_timeout > Duration::ZERO {
                                    state.switch_deadline = Some(tokio::time::Instant::now() + switch_timeout);
                                }
                            }
                        }
                        None => { switch_open = false; }
                    }
                }

                // 8b. Switch deadline — force SIGHUP when pending switch
                // has waited too long for the agent to reach Idle.
                _ = async {
                    match state.switch_deadline {
                        Some(deadline) => tokio::time::sleep_until(deadline).await,
                        None => std::future::pending().await,
                    }
                }, if state.switch_deadline.is_some() => {
                    warn!("switch: deadline reached (agent stuck in {:?}), forcing SIGHUP", state.last_state);
                    let cause = if state.pending_switch.as_ref().is_some_and(|r| r.credentials.is_some()) {
                        "switch_timeout"
                    } else {
                        "restart_timeout"
                    };
                    transition::broadcast_restarting(&self.store, &mut state, cause).await;
                    transition::sighup_child_group(&self.store);
                    state.switch_deadline = None;
                }

                // 9. Shutdown signal (disabled once drain mode is active)
                _ = self.shutdown.cancelled(), if state.drain_deadline.is_none() => {
                    debug!("shutdown signal received");
                    // Forward SIGTERM to the child process group up front so any
                    // installed TERM trap (bash `trap ... TERM`, claude's own
                    // shutdown handler) runs immediately. The child is in its
                    // own session via forkpty's setsid, so it never sees
                    // signals from the parent's pgroup — we have to relay
                    // explicitly. Without this, the drain-Escape path below
                    // would wait graceful_timeout (default 20s) before
                    // signalling the child, longer than typical supervisor
                    // grace windows (ka-0000001002: the local-mode e2e
                    // harness gives 15s before SIGKILLing `gb agent start`,
                    // so the trap never fired and the agent bead never
                    // closed cleanly).
                    transition::sigterm_child_group(&self.store).await;
                    if graceful_timeout > Duration::ZERO
                        && !matches!(state.last_state, AgentState::Idle)
                    {
                        // Keep drain mode as a safety net for children that
                        // ignore SIGTERM — the Escape ticker may coax claude
                        // out of a partial input, and the drain deadline
                        // re-fires sigterm_child_group before we give up.
                        debug!("entering graceful drain mode (timeout={graceful_timeout:?})");
                        state.drain_deadline = Some(tokio::time::Instant::now() + graceful_timeout);
                        next_escape_at = Some(tokio::time::Instant::now());
                    } else {
                        break;
                    }
                }
            }
        }

        // Post-loop: drain pending output
        transition::drain_pending_output(&self.store, &mut self.backend_output_rx).await;

        // Drop the input sender to signal the backend to stop
        drop(self.backend_input_tx);

        // Wait for backend with timeout
        let status = tokio::select! {
            result = &mut self.backend_handle => {
                match result {
                    Ok(Ok(status)) => status,
                    Ok(Err(e)) => {
                        warn!("backend error: {e}");
                        ExitStatus { code: Some(1), signal: None }
                    }
                    Err(e) => {
                        warn!("backend task panicked: {e}");
                        ExitStatus { code: Some(1), signal: None }
                    }
                }
            }
            _ = tokio::time::sleep(shutdown_timeout) => {
                warn!("backend did not exit within {:?}, sending SIGKILL", shutdown_timeout);
                let pid = self.store.terminal.child_pid.load(std::sync::atomic::Ordering::Acquire);
                if pid != 0 {
                    let _ = nix::sys::signal::kill(
                        nix::unistd::Pid::from_raw(-(pid as i32)),
                        nix::sys::signal::Signal::SIGKILL,
                    );
                }
                self.backend_handle.abort();
                ExitStatus { code: Some(137), signal: Some(9) }
            }
        };

        // If a switch is pending, return Switch outcome — don't broadcast Exited.
        if let Some(req) = state.pending_switch.take() {
            return Ok(SessionOutcome::Switch(req));
        }

        // Broadcast exit
        transition::broadcast_exit(&self.store, status, &mut state.state_seq).await;

        Ok(SessionOutcome::Exit(status))
    }

    /// Get a reference to the shared application state.
    pub fn store(&self) -> &Arc<Store> {
        &self.store
    }

    /// Handle an input event, returning `true` if the loop should break.
    async fn handle_input(&self, event: Option<InputEvent>) -> bool {
        // Notify the enter-retry monitor that input activity occurred.
        // WaitForDrain is excluded because it's an internal sync marker.
        if !matches!(event, Some(InputEvent::WaitForDrain(_)) | None) {
            self.store.input_activity.notify_waiters();
        }
        match event {
            Some(InputEvent::Write(data)) => {
                let len = data.len() as u64;
                self.store
                    .lifecycle
                    .bytes_written
                    .fetch_add(len, std::sync::atomic::Ordering::Relaxed);
                if self.backend_input_tx.send(BackendInput::Write(data)).await.is_err() {
                    debug!("backend input channel closed");
                    return true;
                }
            }
            Some(InputEvent::WaitForDrain(tx)) => {
                if self.backend_input_tx.send(BackendInput::Drain(tx)).await.is_err() {
                    debug!("backend input channel closed");
                    return true;
                }
            }
            Some(InputEvent::Resize { cols, rows }) => {
                {
                    let mut screen = self.store.terminal.screen.write().await;
                    screen.resize(cols, rows);
                }
                let _ = self.resize_tx.try_send((cols, rows));
            }
            Some(InputEvent::Signal(sig)) => {
                let pid = self.store.terminal.child_pid.load(std::sync::atomic::Ordering::Acquire);
                if pid != 0 {
                    let _ = nix::sys::signal::kill(
                        nix::unistd::Pid::from_raw(pid as i32),
                        sig.to_nix(),
                    );
                }
            }
            None => return true,
        }
        false
    }
}
