// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Thaw-epoch signal — coop's side of Stage 0 of the agent rehydration design
//! (`gardenio/docs/gvisor-runsc-cr/rehydration-design.md`, ka-0000001515).
//!
//! When a checkpointed gVisor agent is thawed into a fresh kubelet-owned pod,
//! the coop process never restarts — its four NATS transports hold sockets
//! bound to the *old* pod's 4-tuple, dead on first write after the IP change.
//! coop needs an explicit signal that it was thawed so it can tear those down
//! and reconnect (the NATS-reconnect work, ka-0000001901, hangs its handler
//! here).
//!
//! This module is the **signal mechanics only**: it watches the controller-
//! written marker, latches the monotonic epoch, runs a pluggable rehydration
//! handler exactly once per epoch (idempotent), and writes coop's ack so the
//! controller's deadline gate can release the agent. The marker / ack file
//! contract is byte-compatible with the Go `internal/thawepoch` package so the
//! gb supervisor and coop share one on-disk protocol.

use std::path::{Path, PathBuf};
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

/// Filename of the thaw-epoch marker within the state dir. Must match
/// `thawepoch.MarkerName` on the Go side.
pub const MARKER_NAME: &str = "thaw-epoch.json";

/// coop's consumer name. The ack file is `thaw-epoch.coop.done`. Must match
/// `thawepoch.ConsumerCoop`.
pub const CONSUMER: &str = "coop";

/// Default total rehydration budget (rehydration-design §5). Matches
/// `thawepoch.DefaultDeadline`.
pub const DEFAULT_DEADLINE: Duration = Duration::from_secs(10);

/// Poll backstop cadence — the safe floor on gVisor (inotify works but is not
/// guaranteed across a restore). Matches `thawepoch.DefaultPollInterval`.
pub const DEFAULT_POLL_INTERVAL: Duration = Duration::from_secs(1);

/// The thaw-epoch marker the controller writes post-restore. Mirrors the Go
/// `thawepoch.Marker`; unknown/extra fields are ignored on read.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Marker {
    /// Monotonic across thaws of the same logical agent.
    pub epoch: i64,
    /// kubelet pod UID of the pod the agent was thawed into (Pod B).
    #[serde(default)]
    pub pod_uid: String,
    /// Node the thawed pod landed on.
    #[serde(default)]
    pub node: String,
    /// When the controller wrote the marker (RFC3339).
    #[serde(default)]
    pub ts: Option<String>,
    /// Wall-clock of the freeze, when set.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub frozen_at: Option<String>,
    /// Per-thaw deadline override in milliseconds (0/absent = default).
    #[serde(default)]
    pub deadline_ms: i64,
}

impl Marker {
    /// Rehydration budget for this thaw: `deadline_ms` if positive, else the
    /// default.
    pub fn deadline(&self) -> Duration {
        if self.deadline_ms > 0 {
            Duration::from_millis(self.deadline_ms as u64)
        } else {
            DEFAULT_DEADLINE
        }
    }
}

/// A consumer's per-epoch rehydration result. Mirrors the Go `thawepoch.Ack`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Ack {
    pub epoch: i64,
    pub consumer: String,
    /// True iff every required stage of the handler completed. A false ok is
    /// coop fail-closing (C5).
    pub ok: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    /// Per-stage status for observability ("ok"/"failed"/"skipped").
    #[serde(default, skip_serializing_if = "std::collections::BTreeMap::is_empty")]
    pub stages: std::collections::BTreeMap<String, String>,
    /// RFC3339 write time.
    pub ts: String,
}

impl Ack {
    fn new(epoch: i64, ok: bool, error: Option<String>) -> Self {
        Self {
            epoch,
            consumer: CONSUMER.to_string(),
            ok,
            error,
            stages: std::collections::BTreeMap::new(),
            ts: now_ts(),
        }
    }
}

/// Outcome a rehydration handler returns. The watcher attaches epoch/consumer.
#[derive(Debug, Clone, Default)]
pub struct Outcome {
    /// True when every required stage completed.
    pub ok: bool,
    /// Failure reason when `ok` is false.
    pub error: Option<String>,
    /// Per-stage status for observability.
    pub stages: std::collections::BTreeMap<String, String>,
}

impl Outcome {
    /// A fully-successful outcome with no per-stage detail.
    pub fn ok() -> Self {
        Self { ok: true, error: None, stages: std::collections::BTreeMap::new() }
    }

    /// A fail-closed outcome carrying a reason.
    pub fn failed(reason: impl Into<String>) -> Self {
        Self { ok: false, error: Some(reason.into()), stages: std::collections::BTreeMap::new() }
    }
}

fn marker_path(state_dir: &Path) -> PathBuf {
    state_dir.join(MARKER_NAME)
}

fn ack_path(state_dir: &Path, consumer: &str) -> PathBuf {
    state_dir.join(format!("thaw-epoch.{consumer}.done"))
}

fn armed_path(state_dir: &Path, consumer: &str) -> PathBuf {
    state_dir.join(format!("thaw-epoch.{consumer}.armed"))
}

/// Drop the "watcher armed" sentinel for `consumer` so the controller's
/// pre-freeze guard knows this consumer's thaw watcher is live and can
/// participate in a future rehydration handshake (ka-0000006667).
///
/// Byte-compatible with the Go `thawepoch.WriteArmed` / `thawepoch.IsArmed`
/// contract: presence of `thaw-epoch.<consumer>.armed` is the whole signal, so
/// the file body is empty. Atomic create (temp + rename) so the controller
/// never stat-races a half-named file. Idempotent — a restarted (not thawed)
/// coop simply rewrites the same empty file.
pub fn write_armed(state_dir: &Path, consumer: &str) -> anyhow::Result<()> {
    std::fs::create_dir_all(state_dir)?;
    let path = armed_path(state_dir, consumer);
    let tmp = path.with_extension(format!("armed.tmp.{}", std::process::id()));
    std::fs::write(&tmp, b"")?;
    std::fs::rename(&tmp, &path)?;
    Ok(())
}

/// Read the current marker. Returns `Ok(None)` when no marker exists yet.
pub fn read_marker(state_dir: &Path) -> anyhow::Result<Option<Marker>> {
    match std::fs::read(marker_path(state_dir)) {
        Ok(bytes) => Ok(Some(serde_json::from_slice(&bytes)?)),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(e.into()),
    }
}

/// Read coop's ack. Returns `Ok(None)` when absent.
pub fn read_ack(state_dir: &Path, consumer: &str) -> anyhow::Result<Option<Ack>> {
    match std::fs::read(ack_path(state_dir, consumer)) {
        Ok(bytes) => Ok(Some(serde_json::from_slice(&bytes)?)),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(e.into()),
    }
}

/// Atomically write an ack (temp file + rename) so the controller's gate never
/// observes a partial ack.
pub fn write_ack(state_dir: &Path, ack: &Ack) -> anyhow::Result<()> {
    std::fs::create_dir_all(state_dir)?;
    let path = ack_path(state_dir, &ack.consumer);
    let bytes = serde_json::to_vec_pretty(ack)?;
    let tmp = path.with_extension(format!("done.tmp.{}", std::process::id()));
    std::fs::write(&tmp, &bytes)?;
    std::fs::rename(&tmp, &path)?;
    Ok(())
}

fn now_ts() -> String {
    // The ack `ts` field is informational — the controller's gate keys on
    // `epoch` + `ok`, never on `ts`. coop has no date crate as a direct dep,
    // so we emit Unix seconds rather than pull chrono/time in for one stamp.
    // Both sides treat `ts` as an opaque string, so this round-trips cleanly.
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    format!("@{secs}")
}

/// Rehydration handler invoked once per epoch increment. Downstream beads
/// (NATS reconnect, ka-0000001901) implement this to tear down + reconnect the
/// transports. MUST be idempotent — the watcher may invoke it more than once
/// for the same logical thaw (inotify+poll double-fire, or a backstop after the
/// primary).
pub trait RehydrateHandler: Send + Sync {
    /// Run rehydration for `marker`. Implementations should respect
    /// `marker.deadline()` for their own bounded retries.
    fn rehydrate(&self, marker: &Marker) -> impl std::future::Future<Output = Outcome> + Send;
}

/// A no-op handler used for the Stage-0 signal-only path and for tests: a
/// present marker is acked OK so the gate releases. Downstream beads replace
/// this with a real reconnect handler.
#[derive(Debug, Clone, Copy, Default)]
pub struct AckOnlyHandler;

impl RehydrateHandler for AckOnlyHandler {
    async fn rehydrate(&self, _marker: &Marker) -> Outcome {
        Outcome::ok()
    }
}

/// Runs two rehydration handlers in order under a *single* epoch/ack, enforcing
/// the design's load-bearing ordering: `first` (credential re-mint, Stages 1-2)
/// then `second` (NATS reconnect, Stages 3-4). This is the composition
/// `rehydrate.rs` documents ("the coop watcher composes this re-mint stage with
/// that reconnect stage once both land") and that rehydration-design §5
/// mandates — *"reconnecting before re-reading would reconnect with a poisoned
/// token"*, so the creds stage MUST precede the connection stage.
///
/// Both coop rehydration stages key off the **same** on-disk marker, so they
/// belong to one [`ThawWatcher`] writing one `thaw-epoch.coop.done` ack. Wiring
/// them as two separate watchers (the prior shape) was a latent
/// two-writers-one-ack-file hazard *and* armed the re-mint watcher on the wrong
/// directory — the controller only ever writes the marker into
/// `COOP_THAW_STATE_DIR`, so a re-mint watcher pointed elsewhere never fired and
/// Stages 1-2 silently no-op'd on every real thaw (bug ka-0000004088).
///
/// Fail-closed semantics: if `first` fails closed, `second` is **not** run (we
/// must not reconnect under a poisoned/Pod-A token) and the merged outcome is
/// the failed creds outcome with `second`'s stages marked `"skipped"`. If
/// `first` succeeds, `second` runs and its `ok` decides the merged outcome.
/// Per-stage maps are unioned so the ack carries the full picture for the
/// gate's observability.
pub struct Sequence<A: RehydrateHandler, B: RehydrateHandler> {
    first: A,
    second: B,
    /// Stage labels to mark `"skipped"` in the merged outcome when `first`
    /// fail-closes before `second` runs. Lets the ack show *why* the connection
    /// stages did not execute rather than dropping them silently.
    second_skip_stages: &'static [&'static str],
}

impl<A: RehydrateHandler, B: RehydrateHandler> Sequence<A, B> {
    /// Compose `first` (run first) then `second`. `second_skip_stages` are the
    /// stage labels recorded as `"skipped"` in the ack when `first` fail-closes
    /// and short-circuits `second`.
    pub fn new(first: A, second: B, second_skip_stages: &'static [&'static str]) -> Self {
        Self { first, second, second_skip_stages }
    }
}

impl<A: RehydrateHandler, B: RehydrateHandler> RehydrateHandler for Sequence<A, B> {
    async fn rehydrate(&self, marker: &Marker) -> Outcome {
        // Stage 1-2: credential re-mint. Must complete before any reconnect.
        let mut first = self.first.rehydrate(marker).await;
        if !first.ok {
            // Short-circuit: do NOT reconnect under a poisoned/Pod-A token.
            // Record the connection stages as skipped so the ack is legible.
            for s in self.second_skip_stages {
                first.stages.entry((*s).to_string()).or_insert_with(|| "skipped".to_string());
            }
            return first;
        }

        // Stage 3-4: NATS reconnect + presence re-announce.
        let second = self.second.rehydrate(marker).await;

        // Merge: second.ok decides (first already ok); union the stage maps so
        // the gate sees both halves' per-stage detail.
        let mut stages = first.stages;
        stages.extend(second.stages);
        Outcome { ok: second.ok, error: second.error, stages }
    }
}

/// Watches the thaw-epoch marker and runs `handler` once per epoch increment,
/// writing coop's ack each time. Uses `notify` for an eager wake with a poll
/// backstop, mirroring `LogWatcher`.
pub struct ThawWatcher<H: RehydrateHandler> {
    state_dir: PathBuf,
    handler: H,
    poll_interval: Duration,
    last_epoch: i64,
}

impl<H: RehydrateHandler> ThawWatcher<H> {
    pub fn new(state_dir: PathBuf, handler: H) -> Self {
        Self { state_dir, handler, poll_interval: DEFAULT_POLL_INTERVAL, last_epoch: 0 }
    }

    pub fn with_poll_interval(mut self, interval: Duration) -> Self {
        self.poll_interval = interval;
        self
    }

    /// Run the watch loop until `shutdown` is cancelled. On startup, an
    /// already-acked epoch is recovered so a restarted (not thawed) coop does
    /// not re-run it.
    pub async fn run(mut self, shutdown: CancellationToken) {
        self.recover_last_epoch();

        // Signal to the controller's pre-freeze guard that coop's thaw consumer
        // is live (ka-0000006667): a freeze taken before this point would mint a
        // snapshot whose thaw can never be coop-acked. Best-effort — a write
        // failure only ever causes the guard to skip a freeze, never to force one.
        if let Err(e) = write_armed(&self.state_dir, CONSUMER) {
            tracing::warn!(
                "thaw: coop armed-sentinel write failed (freeze guard may skip this agent): {e}"
            );
        }

        // Hold `wake_tx` and re-arm on each tick while the watch fails (state
        // dir not there yet): dropping the only sender closes the channel, and
        // a closed `recv()` resolves at once and spins this loop.
        let (wake_tx, mut wake_rx) = mpsc::channel::<()>(1);
        let mut watcher = self.setup_notify_watcher(wake_tx.clone());

        let mut poll = tokio::time::interval(self.poll_interval);

        // Immediate first check — the marker may already be present.
        self.check_once().await;

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                _ = wake_rx.recv() => {}
                _ = poll.tick() => {
                    if watcher.is_none() {
                        watcher = self.setup_notify_watcher(wake_tx.clone());
                    }
                }
            }
            self.check_once().await;
        }
    }

    fn recover_last_epoch(&mut self) {
        if let Ok(Some(ack)) = read_ack(&self.state_dir, CONSUMER) {
            if ack.ok {
                self.last_epoch = ack.epoch;
            }
        }
    }

    async fn check_once(&mut self) {
        let marker = match read_marker(&self.state_dir) {
            Ok(Some(m)) => m,
            Ok(None) => return,
            Err(e) => {
                tracing::warn!("thaw: read marker failed: {e}");
                return;
            }
        };
        if marker.epoch <= self.last_epoch {
            return; // already handled this epoch
        }
        self.run_handler(&marker).await;
    }

    async fn run_handler(&mut self, marker: &Marker) {
        tracing::info!(
            epoch = marker.epoch,
            pod_uid = %marker.pod_uid,
            node = %marker.node,
            "thaw: rehydration triggered"
        );

        let outcome = match tokio::time::timeout(marker.deadline(), self.handler.rehydrate(marker))
            .await
        {
            Ok(o) => o,
            Err(_) => {
                Outcome::failed(format!("rehydration exceeded deadline {:?}", marker.deadline()))
            }
        };

        let mut ack = Ack::new(marker.epoch, outcome.ok, outcome.error.clone());
        ack.stages = outcome.stages;
        if let Err(e) = write_ack(&self.state_dir, &ack) {
            tracing::error!("thaw: write ack failed: {e}");
            return;
        }

        if outcome.ok {
            // Latch only on success so a corrected marker can re-trigger.
            self.last_epoch = marker.epoch;
            tracing::info!(epoch = marker.epoch, "thaw: rehydration acked OK");
        } else {
            tracing::warn!(
                epoch = marker.epoch,
                error = outcome.error.as_deref().unwrap_or(""),
                "thaw: rehydration fail-closed"
            );
        }
    }

    fn setup_notify_watcher(
        &self,
        wake_tx: mpsc::Sender<()>,
    ) -> Option<notify::RecommendedWatcher> {
        use notify::{RecursiveMode, Watcher};

        let mut watcher = notify::recommended_watcher(move |_: notify::Result<notify::Event>| {
            let _ = wake_tx.try_send(());
        })
        .ok()?;
        // Watch the state dir so we detect marker creation, not just writes.
        watcher.watch(&self.state_dir, RecursiveMode::NonRecursive).ok()?;
        Some(watcher)
    }
}

#[cfg(test)]
#[path = "thaw_tests.rs"]
mod tests;
