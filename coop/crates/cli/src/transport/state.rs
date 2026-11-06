// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU32, AtomicU64};
use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::sync::{broadcast, mpsc, RwLock};
use tokio_util::sync::CancellationToken;

use crate::config::GroomLevel;
use crate::driver::{
    AgentState, AgentType, ErrorCategory, ExitStatus, NudgeEncoder, RespondEncoder,
};
use crate::event::{
    InputEvent, OutputEvent, PromptOutcome, RawHookEvent, RawMessageEvent, TransitionEvent,
};
use crate::event_log::EventLog;
use crate::profile::ProfileState;
use crate::record::RecordingState;
use crate::ring::RingBuffer;
use crate::screen::{Screen, ScreenTextSnapshot};
use crate::start::StartState;
use crate::stop::StopState;
use crate::switch::SwitchState;
use crate::transcript::TranscriptState;
use crate::usage::UsageState;

/// Shared application state passed to all handlers via axum `State` extractor.
///
/// Organized into focused sub-structs by concern:
/// - `terminal`: screen, ring buffer, child process
/// - `driver`: agent detection state
/// - `channels`: channel endpoints for session ↔ transport communication
/// - `config`: static session settings
/// - `lifecycle`: runtime lifecycle primitives
pub struct Store {
    pub terminal: Arc<TerminalState>,
    pub driver: Arc<DriverState>,
    pub channels: TransportChannels,
    pub config: SessionSettings,
    pub lifecycle: LifecycleState,

    /// Agent session ID (UUID). Updated on switch.
    pub session_id: RwLock<String>,
    /// Whether the agent has transitioned out of `Starting` and is ready.
    pub ready: Arc<AtomicBool>,
    /// Stop hook gating state. Always present (defaults to mode=allow).
    pub stop: Arc<StopState>,
    /// Session switch state. Always present.
    pub switch: Arc<SwitchState>,
    /// Start hook state. Always present (defaults to empty config).
    pub start: Arc<StartState>,
    /// Transcript snapshot state. Always present.
    pub transcript: Arc<TranscriptState>,
    /// Per-session API usage tracking. Always present.
    pub usage: Arc<UsageState>,
    /// Named credential profiles for rotation. Always present (defaults to empty).
    pub profile: Arc<ProfileState>,
    /// Serializes structured input delivery (nudge, respond) and enforces
    /// a minimum inter-delivery gap to prevent garbled terminal input.
    pub input_gate: Arc<InputGate>,
    /// Notified by the session loop whenever any `InputEvent` is processed.
    /// Used by the enter-retry monitor to cancel itself if other input
    /// activity occurs on the PTY (e.g. raw keys, resize, signal, new delivery).
    pub input_activity: Arc<tokio::sync::Notify>,
    /// File-backed event log for state/hook event catchup on WS reconnect.
    pub event_log: Arc<EventLog>,
    /// Session recording state. Always present (defaults to disabled).
    pub record: Arc<RecordingState>,
    /// Session directory for file uploads. `None` in attach mode.
    pub session_dir: Option<PathBuf>,
    /// One-shot report of the `--resume` conversation-restore attempt,
    /// captured during session preparation and rendered on `/metrics`
    /// (ka-0000000550). `None` when no resume was requested. Every non-runsc
    /// agent resumes through this path after an interrupt; before this report
    /// the restore outcome was invisible outside pod logs.
    pub resume_report: Option<ResumeReport>,
}

/// Outcome of the `--resume` conversation-restore attempt, for `/metrics`.
#[derive(Debug, Clone)]
pub struct ResumeReport {
    /// Bounded outcome enum (metric label):
    /// - `resumed`: a prior transcript was discovered and parsed; Claude was
    ///   launched with `--resume <uuid>`.
    /// - `fresh_no_session`: no resumable transcript (missing, or under the
    ///   256-byte stub threshold — the truncated/partial-log shape).
    /// - `fresh_parse_error`: a transcript was found but failed to parse.
    /// - `fresh_discovery_error`: the discovery scan itself failed (IO).
    /// - `fresh_no_uuid`: transcript parsed but carried no session UUID, so
    ///   setup fell through to a fresh session (`claude_setup::prepare`).
    pub outcome: &'static str,
    /// Wall-clock seconds spent on resume preparation (local discovery +
    /// JSONL parse; S3 restore time is included when it ran first).
    pub prepare_seconds: f64,
    /// Byte length of the transcript the resume state was parsed from (0 when
    /// none). The partial-log signal: a restore that lost conversation shows
    /// as a sharp drop vs the prior session's transcript size.
    pub log_bytes: u64,
}

/// Terminal I/O: screen, ring buffer, child process.
pub struct TerminalState {
    pub screen: RwLock<Screen>,
    pub ring: RwLock<RingBuffer>,
    /// Lock-free mirror of `ring.total_written()` updated by the session loop.
    ///
    /// Arc-wrapped so the session loop can hand a cheap clone to the
    /// [`CompositeDetector`] activity callback without holding a reference
    /// to the entire `TerminalState`.
    pub ring_total_written: Arc<AtomicU64>,
    pub child_pid: AtomicU32,
    /// ORDERING: must be written before `DriverState.agent_state` is set to
    /// `Exited` so readers who see the exited state always find this populated.
    pub exit_status: RwLock<Option<ExitStatus>>,
}

impl TerminalState {
    /// Build a closure that captures `self` and returns a text-only screen snapshot.
    ///
    /// Used by detectors that need periodic screen access without holding a
    /// reference to the full `TerminalState`.
    pub fn snapshot_fn(
        self: &Arc<Self>,
    ) -> Arc<dyn Fn() -> Arc<crate::screen::ScreenTextSnapshot> + Send + Sync> {
        let terminal = Arc::clone(self);
        Arc::new(move || {
            terminal
                .screen
                .try_read()
                .map(|s| s.snapshot_text())
                .unwrap_or_else(|_| Arc::new(crate::screen::ScreenTextSnapshot::empty()))
        })
    }

    /// Build a closure that returns the child PID (or `None` if not yet spawned).
    pub fn child_pid_fn(self: &Arc<Self>) -> Arc<dyn Fn() -> Option<u32> + Send + Sync> {
        let terminal = Arc::clone(self);
        Arc::new(move || {
            let v = terminal.child_pid.load(std::sync::atomic::Ordering::Acquire);
            if v == 0 {
                None
            } else {
                Some(v)
            }
        })
    }

    /// Build a closure that returns the current `ring_total_written` value.
    pub fn ring_total_written_fn(self: &Arc<Self>) -> Arc<dyn Fn() -> u64 + Send + Sync> {
        let rtw = Arc::clone(&self.ring_total_written);
        Arc::new(move || rtw.load(std::sync::atomic::Ordering::Relaxed))
    }

    /// Reset terminal state for a new session iteration (switch).
    pub async fn reset(&self, cols: u16, rows: u16, ring_size: usize) {
        *self.screen.write().await = Screen::new(cols, rows);
        {
            let mut ring = self.ring.write().await;
            *ring = RingBuffer::new(ring_size);
            self.ring_total_written.store(0, std::sync::atomic::Ordering::Relaxed);
        }
        self.child_pid.store(0, std::sync::atomic::Ordering::Release);
        *self.exit_status.write().await = None;
    }
}

impl DriverState {
    /// Reset driver state for a new session iteration (switch).
    pub async fn reset(&self) {
        *self.agent_state.write().await = AgentState::Starting;
        self.state_seq.store(0, std::sync::atomic::Ordering::Release);
        *self.detection.write().await = DetectionInfo { tier: u8::MAX, cause: String::new() };
        *self.error.write().await = None;
        *self.last_message.write().await = None;
    }
}

/// Classified error detail and category, stored atomically under a single lock.
#[derive(Debug, Clone)]
pub struct ErrorInfo {
    pub detail: String,
    pub category: ErrorCategory,
}

/// Detection tier and cause, stored atomically under a single lock.
#[derive(Debug, Clone)]
pub struct DetectionInfo {
    pub tier: u8,
    pub cause: String,
}

impl DetectionInfo {
    /// Format the detection tier as a display string.
    pub fn tier_str(&self) -> String {
        if self.tier == u8::MAX {
            "none".to_owned()
        } else {
            self.tier.to_string()
        }
    }
}

/// Driver detection state.
pub struct DriverState {
    pub agent_state: RwLock<AgentState>,
    pub state_seq: AtomicU64,
    /// Detection tier and freeform cause string from the detector that produced
    /// the current state. Combined into a single lock to prevent readers from
    /// seeing a stale cause with a new tier (or vice versa).
    pub detection: RwLock<DetectionInfo>,
    /// Error detail + category when agent is in `Error` state, `None` otherwise.
    /// Combined into a single lock to prevent readers from seeing a torn state
    /// (e.g. detail=Some with category=None).
    pub error: RwLock<Option<ErrorInfo>>,
    /// Last assistant message text (concatenated text blocks from the most recent
    /// assistant JSONL entry). Written directly by log/stdout detectors.
    pub last_message: Arc<RwLock<Option<String>>>,
}

/// Channel endpoints for consumer ↔ session communication.
pub struct TransportChannels {
    pub input_tx: mpsc::Sender<InputEvent>,
    pub output_tx: broadcast::Sender<OutputEvent>,
    /// Dedicated channel for cached text-only screen snapshots, separated from
    /// `output_tx` so that ScreenUpdate events cannot evict Raw PTY events
    /// from the broadcast ring (which caused silent 1024-byte gaps).
    pub screen_tx: broadcast::Sender<Arc<ScreenTextSnapshot>>,
    pub state_tx: broadcast::Sender<TransitionEvent>,
    pub prompt_tx: broadcast::Sender<PromptOutcome>,
    pub hook_tx: broadcast::Sender<RawHookEvent>,
    pub message_tx: broadcast::Sender<RawMessageEvent>,
}

/// Static session configuration (immutable after construction).
pub struct SessionSettings {
    pub started_at: Instant,
    pub agent: AgentType,
    /// Operator-facing bearer token. Required on all non-hook routes when set.
    pub auth_token: Option<String>,
    /// Per-session hook token, separate from `auth_token`. Generated at
    /// session start, written to `<session_dir>/hook.token` (0600), and
    /// presented by the hook subprocess as a `Bearer` header. Required on
    /// the hook callback paths (`/api/v1/hooks/{stop,start}`,
    /// `/api/v1/stop/resolve`) when set.
    pub hook_token: Option<String>,
    pub nudge_encoder: Option<Arc<dyn NudgeEncoder>>,
    pub respond_encoder: Option<Arc<dyn RespondEncoder>>,
    /// Timeout for the enter-retry safety net after nudge delivery.
    /// `Duration::ZERO` disables the retry.
    pub nudge_timeout: Duration,
    /// Maximum number of Enter retries with exponential backoff after nudge.
    pub nudge_enter_max_retries: u32,
    /// How aggressively coop auto-responds to agent prompts.
    pub groom: GroomLevel,
}

/// Runtime lifecycle primitives.
pub struct LifecycleState {
    pub shutdown: CancellationToken,
    pub ws_client_count: AtomicI32,
    pub bytes_written: AtomicU64,
    /// Set to `true` when the agent enters an unrecoverable state (exited,
    /// fatal auth errors). The `/api/v1/livez` endpoint reads this to report
    /// the pod as unhealthy so K8s can restart it.
    pub zombie: AtomicBool,
    /// Epoch-millisecond timestamp of the last agent state transition.
    /// Used by the liveness endpoint to detect stale sessions stuck in
    /// non-productive states (e.g. setup prompts that persist indefinitely).
    pub last_transition_ms: AtomicU64,
    /// Set to `true` when the session loop tears down because the agent sat
    /// idle past `COOP_IDLE_TIMEOUT_MS` (idle-suspend), as opposed to a crash,
    /// a `gb stop` / pod-teardown signal, or a credential switch. The terminal
    /// exit path reads this to stamp the S3 session meta `exit_status` as
    /// `"suspended"` so the controller can distinguish "idle agent parked —
    /// snapshot intact, reclaim its pod onto the warm pool" (Phase 1
    /// snapshot-suspend, ka-0000002921) from a failure or a clean stop.
    pub idle_suspended: AtomicBool,
}

/// Serializes structured input delivery sequences (nudge, respond) and
/// enforces a minimum inter-delivery gap (debounce) to prevent garbled
/// terminal input when deliveries arrive in rapid succession.
///
/// Terminal-based agents can drop or mis-interpret keystrokes when input
/// arrives faster than the TUI can process.  The gate ensures at least
/// `debounce` time elapses between the end of one delivery and the start
/// of the next.
pub struct InputGate {
    lock: Arc<tokio::sync::Mutex<DeliveryGateInner>>,
    debounce: Duration,
}

struct DeliveryGateInner {
    last_delivery: Option<Instant>,
    /// Cancel token for any pending enter-retry from the previous delivery.
    retry_cancel: Option<CancellationToken>,
}

impl InputGate {
    pub fn new(debounce: Duration) -> Self {
        Self {
            lock: Arc::new(tokio::sync::Mutex::new(DeliveryGateInner {
                last_delivery: None,
                retry_cancel: None,
            })),
            debounce,
        }
    }

    /// Acquire exclusive delivery access, sleeping if the minimum
    /// inter-delivery gap has not yet elapsed.
    ///
    /// Cancels any pending enter-retry from the previous delivery.
    ///
    /// Returns an owned guard that is `'static` and movable into spawned
    /// tasks, so the caller can detach the actual delivery work onto a
    /// background task while the guard continues to serialise against the
    /// next acquirer.
    pub async fn acquire(&self) -> DeliveryGuard {
        let guard = Arc::clone(&self.lock).lock_owned().await;
        // Cancel pending retry from previous delivery
        if let Some(ref token) = guard.retry_cancel {
            token.cancel();
        }
        if let Some(last) = guard.last_delivery {
            let elapsed = last.elapsed();
            if elapsed < self.debounce {
                tokio::time::sleep(self.debounce - elapsed).await;
            }
        }
        DeliveryGuard { inner: guard }
    }
}

/// RAII guard returned by [`InputGate::acquire`].
///
/// Holds an `OwnedMutexGuard` so the guard is `'static` and movable into
/// spawned tasks.  Records the delivery completion timestamp on drop so
/// that subsequent acquisitions can enforce the debounce interval.
pub struct DeliveryGuard {
    inner: tokio::sync::OwnedMutexGuard<DeliveryGateInner>,
}

impl DeliveryGuard {
    /// Store a cancellation token for the enter-retry monitor spawned
    /// after this delivery.  The next `acquire()` call will cancel it.
    pub fn set_retry_cancel(&mut self, token: CancellationToken) {
        self.inner.retry_cancel = Some(token);
    }
}

impl Drop for DeliveryGuard {
    fn drop(&mut self) {
        self.inner.last_delivery = Some(Instant::now());
    }
}

impl Store {
    /// Register credential profiles, keeping the live agent's login current.
    ///
    /// For a Codex session, a push that carries a rotated `auth.json` for the
    /// ACTIVE profile is written to `$CODEX_HOME/auth.json` in place (temp +
    /// rename) so Codex's guarded reload adopts the new access token without a
    /// respawn: the keeper rotates the access token about hourly, and a switch
    /// per rotation would restart the agent every time. Other profiles are only
    /// staged; moving to one stays the switch path's job.
    pub async fn register_profiles(&self, entries: Vec<crate::profile::ProfileEntry>) {
        if self.config.agent == AgentType::Codex {
            self.refresh_active_codex_auth(&entries).await;
        }
        self.profile.register(entries).await;
    }

    async fn refresh_active_codex_auth(&self, entries: &[crate::profile::ProfileEntry]) {
        use crate::driver::codex::auth;
        let Some(active) = self.profile.active_name().await else { return };
        let Some(doc) = entries
            .iter()
            .find(|e| e.name == active)
            .and_then(|e| e.credentials.get(auth::AUTH_ENV))
            .filter(|d| !d.is_empty())
        else {
            return;
        };
        let unchanged = self
            .profile
            .resolve_credentials(&active)
            .await
            .is_some_and(|c| c.get(auth::AUTH_ENV) == Some(doc));
        if unchanged {
            return;
        }
        match auth::write_auth_json(&auth::codex_home(), doc) {
            Ok(()) => tracing::info!(profile = %active, "codex auth.json refreshed in place"),
            Err(e) => tracing::error!(profile = %active, "failed to refresh codex auth.json: {e}"),
        }
    }
}

impl std::fmt::Debug for Store {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("AppState")
            .field("agent", &self.config.agent)
            .field("auth_token", &self.config.auth_token.is_some())
            .finish()
    }
}

#[cfg(test)]
#[path = "state_tests.rs"]
mod tests;

#[cfg(test)]
mod codex_auth_tests {
    use std::collections::HashMap;

    use crate::driver::AgentType;
    use crate::profile::ProfileEntry;
    use crate::test_support::StoreBuilder;

    fn entry(name: &str, doc: &str) -> ProfileEntry {
        ProfileEntry {
            name: name.to_owned(),
            credentials: HashMap::from([("CODEX_AUTH_JSON".to_owned(), doc.to_owned())]),
            usable: None,
        }
    }

    #[tokio::test]
    #[serial_test::serial]
    async fn rotated_login_for_the_active_profile_is_written_in_place() {
        let dir = tempfile::tempdir().unwrap();
        std::env::set_var("CODEX_HOME", dir.path());
        let ctx = StoreBuilder::new().agent(AgentType::Codex).build();
        let store = ctx.store;

        // First registration seeds the Active profile; nothing is rotated yet.
        store.register_profiles(vec![entry("codex-a", "v1")]).await;
        assert!(!dir.path().join("auth.json").exists());

        // The keeper's rotation arrives for the Active profile: written in place.
        store.register_profiles(vec![entry("codex-a", "v2")]).await;
        assert_eq!(std::fs::read_to_string(dir.path().join("auth.json")).unwrap(), "v2");

        // A staged (non-active) profile never touches the live login.
        store.register_profiles(vec![entry("codex-b", "other")]).await;
        store.register_profiles(vec![entry("codex-b", "other-2")]).await;
        assert_eq!(std::fs::read_to_string(dir.path().join("auth.json")).unwrap(), "v2");
        std::env::remove_var("CODEX_HOME");
    }

    #[tokio::test]
    #[serial_test::serial]
    async fn non_codex_sessions_never_write_auth_json() {
        let dir = tempfile::tempdir().unwrap();
        std::env::set_var("CODEX_HOME", dir.path());
        let store = StoreBuilder::new().agent(AgentType::Claude).build().store;
        store.register_profiles(vec![entry("a", "v1")]).await;
        store.register_profiles(vec![entry("a", "v2")]).await;
        std::env::remove_var("CODEX_HOME");
        assert!(!dir.path().join("auth.json").exists());
    }
}
