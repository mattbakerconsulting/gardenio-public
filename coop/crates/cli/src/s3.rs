// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! S3 persistence for session data (transcripts, recordings, event logs).
//!
//! When configured via `COOP_S3_BUCKET`, subscribes to broadcast channels
//! and uploads session artifacts to S3 in real time. Supports downloading
//! artifacts from S3 for session resume.
//!
//! The [`S3Storage`] trait abstracts storage operations so that tests can
//! use an in-memory mock without requiring a real S3 endpoint.

use std::future::Future;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;
use tracing::{info, warn};

use crate::event::TransitionEvent;
use crate::record::RecordingEntry;
use crate::transcript::TranscriptEvent;

/// Trait abstracting S3 storage operations for testability.
///
/// [`S3Client`] implements this for real AWS S3. Tests can supply a mock.
pub trait S3Storage: Send + Sync + 'static {
    fn upload_file(
        &self,
        session_id: &str,
        s3_path: &str,
        local_path: &Path,
    ) -> impl Future<Output = anyhow::Result<()>> + Send;

    fn upload_bytes(
        &self,
        session_id: &str,
        s3_path: &str,
        data: Vec<u8>,
    ) -> impl Future<Output = anyhow::Result<()>> + Send;

    fn download_file(
        &self,
        session_id: &str,
        s3_path: &str,
        local_path: &Path,
    ) -> impl Future<Output = anyhow::Result<()>> + Send;

    fn list_transcripts(
        &self,
        session_id: &str,
    ) -> impl Future<Output = anyhow::Result<Vec<u32>>> + Send;

    fn upload_meta(
        &self,
        session_id: &str,
        meta: &SessionMeta,
    ) -> impl Future<Output = anyhow::Result<()>> + Send;

    fn download_meta(
        &self,
        session_id: &str,
    ) -> impl Future<Output = anyhow::Result<Option<SessionMeta>>> + Send;

    fn exists(&self, session_id: &str, s3_path: &str) -> impl Future<Output = bool> + Send;

    /// List all session IDs that have a meta.json in S3.
    fn list_sessions(&self) -> impl Future<Output = anyhow::Result<Vec<String>>> + Send;

    /// List spawn sub-prefixes directly under a bead-scoped prefix.
    ///
    /// Used when a bead is keying the prefix (`<bead>/<spawn>/...`) to enumerate
    /// the per-spawn sub-keys for that bead. Returns the spawn segment only
    /// (not the full `<bead>/<spawn>` key). Returns an empty list if the bead
    /// has no spawn-shaped sub-prefixes (e.g. legacy flat layout, or no data).
    fn list_spawns(
        &self,
        bead_id: &str,
    ) -> impl Future<Output = anyhow::Result<Vec<String>>> + Send;

    /// List recording chunk names for a session (e.g. "1-50.jsonl").
    fn list_recording_chunks(
        &self,
        session_id: &str,
    ) -> impl Future<Output = anyhow::Result<Vec<String>>> + Send;

    /// List workflow companion-dir files for a session.
    ///
    /// Returns paths relative to the `workflow/` prefix (e.g.
    /// `agent-12.jsonl`, or a nested `runs/<id>.json`). Used by
    /// [`restore_workflow_dir`] to rehydrate the dynamic-workflow journals
    /// (Claude `agent-*.jsonl` + run-state) on an S3-only resume.
    fn list_workflow_files(
        &self,
        session_id: &str,
    ) -> impl Future<Output = anyhow::Result<Vec<String>>> + Send;

    /// Download bytes from S3 (used by peek to fetch transcript/recording content).
    fn download_bytes(
        &self,
        session_id: &str,
        s3_path: &str,
    ) -> impl Future<Output = anyhow::Result<Vec<u8>>> + Send;
}

/// S3 client wrapper for session persistence.
///
/// The inner `aws_sdk_s3::Client` lives behind an `RwLock` so it can be
/// rebuilt in place on a thaw ([`S3Client::reload_credentials`]). Rebuilding
/// re-runs the default credentials chain, which re-reads
/// `AWS_WEB_IDENTITY_TOKEN_FILE` and discards any STS creds cached under the
/// pre-freeze (Pod-A) identity — the AWS half of Stage 1 "poison in-RAM creds"
/// in the rehydration design (ka-0000001515 §4 Stage 1/2, build bead
/// ka-0000007211). The aws client is a cheap `Arc`-backed handle, so each call
/// clones it out from under a brief read lock rather than holding the lock
/// across the network round-trip.
pub struct S3Client {
    client: parking_lot::RwLock<aws_sdk_s3::Client>,
    bucket: String,
    prefix: String,
    /// Region the client was built with, retained so a thaw can rebuild an
    /// equivalent client without re-plumbing the caller's config.
    region: Option<String>,
    /// Endpoint URL override the client was built with (e.g. in-box MinIO).
    /// Retained alongside `region` so a thaw rebuilds an equivalent client.
    /// When `Some`, the client uses path-style addressing.
    endpoint: Option<String>,
    /// Path to a file containing the object-store access key ID
    /// (`COOP_S3_ACCESS_KEY_ID_FILE`), endpoint mode only (gd-W8OwRoHLLn).
    /// Retained so a thaw re-reads the file rather than reusing a stale value.
    access_key_id_file: Option<PathBuf>,
    /// Path to a file containing the object-store secret access key
    /// (`COOP_S3_SECRET_ACCESS_KEY_FILE`), endpoint mode only
    /// (gd-W8OwRoHLLn). Retained so a thaw re-reads the file rather than
    /// reusing a stale value.
    secret_access_key_file: Option<PathBuf>,
}

/// Session metadata stored alongside artifacts in S3.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SessionMeta {
    pub session_id: String,
    pub agent_type: String,
    pub started_at: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ended_at: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
    #[serde(default)]
    pub labels: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub agent_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub project: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub role: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bead_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_status: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub transcript_count: Option<u32>,
    /// Agent harness that wrote the session (`claude`, `codex`, ...). Decides how
    /// `session.jsonl` is restored: a Claude transcript and a Codex rollout are
    /// not interchangeable (ADR 0004: no cross-harness migration). Absent in
    /// sessions written before this field existed; see [`SessionMeta::harness_name`].
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub harness: Option<String>,
}

impl SessionMeta {
    /// The harness that wrote this session, falling back to `agent_type` for
    /// metas that predate the explicit field.
    pub fn harness_name(&self) -> &str {
        self.harness.as_deref().unwrap_or(&self.agent_type)
    }
}

/// Resolve the S3 session key (prefix component) used for upload and restore.
///
/// Sessions in S3 live under `<s3_prefix>/<session-key>/...`. Historically
/// the session key was the coop-generated session UUID — pod-scoped, lost
/// on restart. To make a session belong to an agent (bead) rather than a
/// pod, callers can pass an explicit key (e.g. the agent bead ID).
///
/// Resolution order:
/// 1. `explicit` — `--s3-session-key` / `COOP_S3_SESSION_KEY`
/// 2. `resume` — `--s3-resume-session` / `COOP_S3_RESUME_SESSION`
/// 3. `fallback` — coop's internal session UUID
///
/// # `--s3-resume-session` / `COOP_S3_RESUME_SESSION` is a manual operator override
///
/// Audit (ka-0000006166, parent ka-0000003664 G13, 2026-05-03): no
/// production caller in the gardenio helm chart, controller (`init_agent_loop.go`),
/// or RWX e2e configs sets `COOP_S3_RESUME_SESSION`. The live wire is
/// `COOP_S3_SESSION_KEY` (set by the controller from the agent bead ID).
///
/// `--s3-resume-session` / `COOP_S3_RESUME_SESSION` is retained intentionally
/// as a manual operator override — used when running coop directly, in the
/// legacy flat S3 layout, or as the documented recovery escape hatch in
/// `gardenio/docs/runbooks/s3-resume.md`. The Go side (`gb session.go:347`)
/// also branches on it as a fallback when no local resume log is loadable.
///
/// Removing this knob would silently break those paths, so we keep it and
/// require an explicit unit test (`resolve_session_key_uses_resume_env_var`
/// in `s3_tests.rs`) that exercises the env-var → clap → resolve path so
/// regressions are caught at compile / test time.
pub fn resolve_session_key(explicit: Option<&str>, resume: Option<&str>, fallback: &str) -> String {
    if let Some(s) = explicit {
        let s = s.trim();
        if !s.is_empty() {
            return s.to_owned();
        }
    }
    if let Some(s) = resume {
        let s = s.trim();
        if !s.is_empty() {
            return s.to_owned();
        }
    }
    fallback.to_owned()
}

/// Compose the per-spawn upload key for a coop session.
///
/// When a bead ID is supplied, every spawn writes under
/// `<bead>/<coop_uuid>/...` so concurrent / sequential spawns sharing a bead
/// remain distinguishable. Without a bead, falls back to the legacy flat
/// layout: `--s3-resume-session` value if set, else the coop UUID.
pub fn compose_session_key(
    bead: Option<&str>,
    resume: Option<&str>,
    coop_session_id: &str,
) -> String {
    if let Some(bead) = bead.map(str::trim).filter(|s| !s.is_empty()) {
        return format!("{bead}/{coop_session_id}");
    }
    if let Some(resume) = resume.map(str::trim).filter(|s| !s.is_empty()) {
        return resume.to_owned();
    }
    coop_session_id.to_owned()
}

/// Pick the most recent spawn under a bead prefix.
///
/// Lists spawn-shaped sub-prefixes under `<bead>/`, downloads each spawn's
/// `meta.json`, and returns the `<bead>/<spawn>` key with the largest
/// `started_at`. Spawns without a readable meta are skipped. Ties are broken
/// by spawn ID (lexicographic, descending) for determinism. Returns `None`
/// if no spawn has a readable meta.
pub async fn pick_latest_spawn<S: S3Storage>(s3: &S, bead_id: &str) -> Option<String> {
    let spawns = s3.list_spawns(bead_id).await.ok()?;
    let mut best: Option<(u64, String)> = None;
    for spawn in spawns {
        let composed = format!("{bead_id}/{spawn}");
        match s3.download_meta(&composed).await {
            Ok(Some(meta)) => {
                let started = meta.started_at;
                let take = match best {
                    None => true,
                    Some((bs, ref bk)) => {
                        started > bs || (started == bs && composed.as_str() > bk.as_str())
                    }
                };
                if take {
                    best = Some((started, composed));
                }
            }
            Ok(None) | Err(_) => continue,
        }
    }
    best.map(|(_, key)| key)
}

/// Pick the S3 key to restore from for a given (bead, resume) pair.
///
/// Resolution:
/// - If `bead` is set: return the most recent spawn under `<bead>/`. If no
///   spawn has a readable meta (fresh start, or legacy flat layout), fall
///   back to `<bead>` itself — `restore_*` calls are tolerant of empty keys.
/// - Else if `resume` is set: use it directly (legacy flat layout).
/// - Else: `None`.
pub async fn pick_restore_key<S: S3Storage>(
    s3: &S,
    bead: Option<&str>,
    resume: Option<&str>,
) -> Option<String> {
    if let Some(bead) = bead.map(str::trim).filter(|s| !s.is_empty()) {
        if let Some(spawn_key) = pick_latest_spawn(s3, bead).await {
            return Some(spawn_key);
        }
        return Some(bead.to_owned());
    }
    if let Some(resume) = resume.map(str::trim).filter(|s| !s.is_empty()) {
        return Some(resume.to_owned());
    }
    None
}

/// Derive a semantic exit status from a child's exit `code` and/or `signal`.
///
/// `shutdown_requested` is the session's graceful-shutdown bit
/// (`lifecycle.shutdown.is_cancelled()`), true once an operator-delivered
/// SIGTERM/SIGINT/SIGHUP — the normal `gb stop` / pod-teardown path — or a
/// transport shutdown request has cancelled the shutdown token.
///
/// Termination by signal reaches us two ways (see `backend/spawn.rs`
/// `wait_for_exit`): a child that *traps* the forwarded signal exits with
/// `code = Some(128 + signum)` (143=SIGTERM, 130=SIGINT, 129=SIGHUP), while a
/// child *killed outright* by the signal yields `code = None, signal =
/// Some(signum)`. We normalize the latter to `128 + signum` so both forms map
/// identically.
///
/// Under the old `Some(_) => "error"` mapping every clean `gb stop` looked like
/// a crash, which poisoned the `gb audit` fleet-health signal (ka-0000001328).
/// We now fold the graceful-shutdown bit in: a signal-termination code WITH a
/// requested shutdown is a clean teardown (`"stopped"`); the SAME code WITHOUT
/// the bit is a genuine mid-task kill and stays `"error"` so real crashes still
/// surface. SIGKILL (137 / signal 9) stays `"error"` even on the shutdown path —
/// it only arises when the child failed to exit within the grace window
/// (session/run.rs), which is worth flagging.
///
/// `idle_suspended` is the session's idle-suspend bit
/// (`lifecycle.idle_suspended`), true when the session loop tore down because
/// the agent sat idle past `COOP_IDLE_TIMEOUT_MS` (session/run.rs cancels the
/// shutdown token from the idle-timeout branch, so `shutdown_requested` is also
/// true here). An idle-suspend is scored `"suspended"` — a distinct, benign
/// signal from a `gb stop` `"stopped"` — so the controller can reclaim the
/// idle agent's pod onto the warm pool while its snapshot stays intact, rather
/// than read the teardown as a failure or a terminal stop (Phase 1
/// snapshot-suspend, ka-0000002921). Idle-suspend is checked BEFORE the plain
/// shutdown mapping precisely because it rides the same signal-termination
/// codes on a requested shutdown.
pub fn exit_status_from_code(
    code: Option<i32>,
    signal: Option<i32>,
    shutdown_requested: bool,
    idle_suspended: bool,
) -> String {
    // Collapse a kill-by-signal into the conventional 128 + signum code so the
    // trapped-exit and killed-outright cases are scored the same.
    let effective = code.or_else(|| signal.map(|s| 128 + s));
    match effective {
        Some(0) => "success".into(),
        Some(crate::run::EXIT_CODE_CREDENTIAL_SWITCH) => "switch".into(),
        Some(124) => "timeout".into(),
        // 128 + {SIGTERM=15, SIGINT=2, SIGHUP=1}: an idle-suspend teardown
        // (checked first — it rides the same codes as a clean stop but carries
        // the distinct reclaim-onto-warm-pool meaning) or a clean teardown when
        // the session itself requested shutdown (gb stop / pod terminate).
        Some(143 | 130 | 129) if idle_suspended => "suspended".into(),
        Some(143 | 130 | 129) if shutdown_requested => "stopped".into(),
        Some(_) => "error".into(),
        None => "error".into(),
    }
}

type ExitInfo = Arc<parking_lot::Mutex<Option<(Option<i32>, String)>>>;

/// Handle for providing final session info to the S3 subscriber.
///
/// Returned by [`spawn_subscriber`]. Call [`set_exit`](S3SessionHandle::set_exit)
/// before triggering shutdown so the final meta upload includes exit info.
///
/// The handle also carries the subscriber's [`tokio::task::JoinHandle`] so
/// callers can deterministically wait for the drain to complete via
/// [`await_drain`](S3SessionHandle::await_drain) — important on credential
/// switch / SIGHUP paths where the process is about to exit and tokio tasks
/// would otherwise be aborted before the final flush lands in S3.
pub struct S3SessionHandle {
    exit_info: ExitInfo,
    drain: CancellationToken,
    join: parking_lot::Mutex<Option<tokio::task::JoinHandle<()>>>,
}

impl S3SessionHandle {
    /// Record exit information. Call before [`finalize`](Self::finalize).
    pub fn set_exit(&self, code: Option<i32>, status: String) {
        *self.exit_info.lock() = Some((code, status));
    }

    /// Drain the subscriber and wait for the final flush to land in S3.
    ///
    /// Cancels the subscriber's *own* drain token — which is deliberately
    /// independent of the session shutdown token — then awaits the join
    /// handle. Decoupling the tokens is what fixes the coverage gap
    /// (ka-0000001666): when an external SIGTERM/SIGHUP/idle-timeout cancels
    /// the session shutdown token, the subscriber must NOT race ahead and
    /// upload a placeholder `meta.json` (exit_status="error",
    /// transcript_count=0) before the session loop has saved its final
    /// transcript snapshot and stamped exit info via [`set_exit`](Self::set_exit).
    /// Callers finalize explicitly, after the snapshot + set_exit, on every
    /// terminal exit path.
    pub async fn finalize(&self, timeout: Duration) -> bool {
        self.drain.cancel();
        self.await_drain(timeout).await
    }

    /// Wait for the subscriber task to finish flushing pending state to S3.
    ///
    /// Caller must cancel the drain token first (the subscriber's drain
    /// arm fires on cancellation) — use [`finalize`](Self::finalize) to do
    /// both. This method only awaits the join handle with a timeout so a
    /// hung S3 endpoint cannot wedge the process. Returns `true` if the
    /// subscriber completed within `timeout`.
    pub async fn await_drain(&self, timeout: Duration) -> bool {
        let join = self.join.lock().take();
        let Some(handle) = join else {
            return true;
        };
        match tokio::time::timeout(timeout, handle).await {
            Ok(Ok(())) => true,
            Ok(Err(e)) => {
                warn!(error = %e, "s3: subscriber task panicked during drain");
                false
            }
            Err(_) => {
                warn!(timeout_secs = timeout.as_secs(), "s3: drain timed out");
                false
            }
        }
    }
}

fn now_unix_secs() -> u64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap_or_default().as_secs()
}

impl S3Client {
    /// Create a new S3 client from environment/IRSA credentials, or from the
    /// static access/secret key files when given (in-box object-store,
    /// gd-W8OwRoHLLn).
    pub async fn new(
        bucket: String,
        prefix: String,
        region: Option<String>,
        endpoint: Option<String>,
        access_key_id_file: Option<PathBuf>,
        secret_access_key_file: Option<PathBuf>,
    ) -> anyhow::Result<Self> {
        let client = Self::build_aws_client(
            region.as_deref(),
            endpoint.as_deref(),
            access_key_id_file.as_deref(),
            secret_access_key_file.as_deref(),
        )
        .await;

        // Verify connectivity with a lightweight HEAD request.
        match client.head_bucket().bucket(&bucket).send().await {
            Ok(_) => info!(bucket = %bucket, prefix = %prefix, "s3: connected"),
            Err(e) => {
                warn!(bucket = %bucket, error = %e, "s3: bucket HEAD failed (may still work)");
            }
        }

        Ok(Self {
            client: parking_lot::RwLock::new(client),
            bucket,
            prefix,
            region,
            endpoint,
            access_key_id_file,
            secret_access_key_file,
        })
    }

    /// Read the object-store static credential files (gd-W8OwRoHLLn) into an
    /// explicit `Credentials` value, when both are given.
    ///
    /// Mirrors `NatsParams::token_resolved()` (transport/nats_supervisor.rs):
    /// read the file fresh via `std::fs::read_to_string`, trim, and hold the
    /// result only as an in-memory value — **never** `std::env::set_var`, so
    /// the access/secret key never appear in `/proc/self/environ`. A missing
    /// or unreadable file logs a warning and falls back to the default AWS
    /// credential chain (e.g. IRSA) rather than failing the client build.
    fn static_credentials_from_files(
        access_key_id_file: Option<&Path>,
        secret_access_key_file: Option<&Path>,
    ) -> Option<aws_sdk_s3::config::Credentials> {
        let (access_key_id_file, secret_access_key_file) =
            (access_key_id_file?, secret_access_key_file?);

        let access_key_id = match std::fs::read_to_string(access_key_id_file) {
            Ok(s) => s.trim().to_owned(),
            Err(e) => {
                warn!(path = %access_key_id_file.display(), error = %e, "s3: failed to read access key id file, falling back to default credential chain");
                return None;
            }
        };
        let secret_access_key = match std::fs::read_to_string(secret_access_key_file) {
            Ok(s) => s.trim().to_owned(),
            Err(e) => {
                warn!(path = %secret_access_key_file.display(), error = %e, "s3: failed to read secret access key file, falling back to default credential chain");
                return None;
            }
        };
        if access_key_id.is_empty() || secret_access_key.is_empty() {
            warn!("s3: access key id or secret access key file is empty, falling back to default credential chain");
            return None;
        }

        Some(aws_sdk_s3::config::Credentials::new(
            access_key_id,
            secret_access_key,
            None,
            None,
            "coop-object-store-secret-file",
        ))
    }

    /// Build a fresh `aws_sdk_s3::Client`. Each call resolves credentials
    /// anew — for IRSA pods this re-reads `AWS_WEB_IDENTITY_TOKEN_FILE` rather
    /// than reusing a cached STS exchange; for in-box object-store pods it
    /// re-reads the static credential files.
    ///
    /// When both `access_key_id_file` and `secret_access_key_file` are given
    /// and readable, the client is built with those explicit static
    /// credentials (in-box object-store, no IRSA — gd-W8OwRoHLLn). Otherwise
    /// the client falls back to the default AWS credential provider chain
    /// (env vars, IRSA web-identity token, IMDS, etc).
    ///
    /// When `endpoint` is `Some` (e.g. an in-box MinIO), the client targets
    /// that endpoint and enables **path-style** addressing
    /// (`bucket.endpoint/key` rather than `bucket.endpoint`). The Rust AWS SDK
    /// cannot enable path-style from env/config — it must be set in code via
    /// `force_path_style(true)` (aws-sdk-rust #1230, #390). MinIO and most
    /// S3-compatible stores require path-style. When `endpoint` is `None`, the
    /// client is left at the AWS S3 default (virtual-hosted addressing).
    async fn build_aws_client(
        region: Option<&str>,
        endpoint: Option<&str>,
        access_key_id_file: Option<&Path>,
        secret_access_key_file: Option<&Path>,
    ) -> aws_sdk_s3::Client {
        let mut config_loader = aws_config::defaults(aws_config::BehaviorVersion::latest());
        if let Some(region) = region {
            config_loader = config_loader.region(aws_config::Region::new(region.to_owned()));
        }
        if let Some(credentials) =
            Self::static_credentials_from_files(access_key_id_file, secret_access_key_file)
        {
            config_loader = config_loader.credentials_provider(credentials);
        }
        let sdk_config = config_loader.load().await;

        match endpoint {
            Some(endpoint) => {
                let s3_config = aws_sdk_s3::config::Builder::from(&sdk_config)
                    .endpoint_url(endpoint)
                    .force_path_style(true)
                    .build();
                aws_sdk_s3::Client::from_conf(s3_config)
            }
            None => aws_sdk_s3::Client::new(&sdk_config),
        }
    }

    /// Clone out the current aws client under a brief read lock. The handle is
    /// `Arc`-backed, so the clone is cheap and the network round-trip happens
    /// without holding the lock (and without blocking a concurrent reload).
    fn aws(&self) -> aws_sdk_s3::Client {
        self.client.read().clone()
    }

    /// Rebuild the inner aws client so the next call re-resolves credentials —
    /// the AWS half of Stage 1 cache-poison on thaw (rehydration-design §4).
    ///
    /// The old client (and any STS creds it cached under the pre-freeze
    /// identity) is dropped; the replacement re-runs the default chain, which
    /// re-reads `AWS_WEB_IDENTITY_TOKEN_FILE`. Idempotent and safe to call more
    /// than once per epoch (a second call just costs one more provider build).
    pub async fn reload_credentials(&self) {
        let fresh = Self::build_aws_client(
            self.region.as_deref(),
            self.endpoint.as_deref(),
            self.access_key_id_file.as_deref(),
            self.secret_access_key_file.as_deref(),
        )
        .await;
        *self.client.write() = fresh;
        info!(bucket = %self.bucket, "s3: reloaded aws credentials provider (thaw)");
    }

    /// S3 key for a session artifact.
    fn key(&self, session_id: &str, path: &str) -> String {
        format!("{}/{}/{}", self.prefix, session_id, path)
    }
}

impl S3Storage for S3Client {
    async fn upload_file(
        &self,
        session_id: &str,
        s3_path: &str,
        local_path: &Path,
    ) -> anyhow::Result<()> {
        let key = self.key(session_id, s3_path);
        let body = aws_sdk_s3::primitives::ByteStream::from_path(local_path).await?;
        self.aws().put_object().bucket(&self.bucket).key(&key).body(body).send().await?;
        Ok(())
    }

    async fn upload_bytes(
        &self,
        session_id: &str,
        s3_path: &str,
        data: Vec<u8>,
    ) -> anyhow::Result<()> {
        let key = self.key(session_id, s3_path);
        let body = aws_sdk_s3::primitives::ByteStream::from(data);
        self.aws().put_object().bucket(&self.bucket).key(&key).body(body).send().await?;
        Ok(())
    }

    async fn download_file(
        &self,
        session_id: &str,
        s3_path: &str,
        local_path: &Path,
    ) -> anyhow::Result<()> {
        let key = self.key(session_id, s3_path);
        let output = self.aws().get_object().bucket(&self.bucket).key(&key).send().await?;
        let bytes = output.body.collect().await?.into_bytes();
        if let Some(parent) = local_path.parent() {
            tokio::fs::create_dir_all(parent).await?;
        }
        tokio::fs::write(local_path, bytes).await?;
        Ok(())
    }

    async fn list_transcripts(&self, session_id: &str) -> anyhow::Result<Vec<u32>> {
        let prefix = self.key(session_id, "transcripts/");
        let mut numbers = Vec::new();
        let mut continuation = None;

        loop {
            let mut req = self.aws().list_objects_v2().bucket(&self.bucket).prefix(&prefix);
            if let Some(token) = continuation {
                req = req.continuation_token(token);
            }
            let output = req.send().await?;

            for obj in output.contents() {
                if let Some(key) = obj.key() {
                    // Extract number from "prefix/session-id/transcripts/N.jsonl"
                    if let Some(filename) = key.rsplit('/').next() {
                        if let Some(num_str) = filename.strip_suffix(".jsonl") {
                            if let Ok(n) = num_str.parse::<u32>() {
                                numbers.push(n);
                            }
                        }
                    }
                }
            }

            if output.is_truncated() == Some(true) {
                continuation = output.next_continuation_token().map(|s| s.to_owned());
            } else {
                break;
            }
        }

        numbers.sort();
        Ok(numbers)
    }

    async fn upload_meta(&self, session_id: &str, meta: &SessionMeta) -> anyhow::Result<()> {
        let json = serde_json::to_vec_pretty(meta)?;
        self.upload_bytes(session_id, "meta.json", json).await
    }

    async fn download_meta(&self, session_id: &str) -> anyhow::Result<Option<SessionMeta>> {
        let key = self.key(session_id, "meta.json");
        match self.aws().get_object().bucket(&self.bucket).key(&key).send().await {
            Ok(output) => {
                let bytes = output.body.collect().await?.into_bytes();
                let meta: SessionMeta = serde_json::from_slice(&bytes)?;
                Ok(Some(meta))
            }
            Err(_) => Ok(None),
        }
    }

    async fn exists(&self, session_id: &str, s3_path: &str) -> bool {
        let key = self.key(session_id, s3_path);
        self.aws().head_object().bucket(&self.bucket).key(&key).send().await.is_ok()
    }

    async fn list_sessions(&self) -> anyhow::Result<Vec<String>> {
        let prefix = format!("{}/", self.prefix);
        let mut session_ids = Vec::new();
        let mut continuation = None;

        loop {
            let mut req = self.aws().list_objects_v2().bucket(&self.bucket).prefix(&prefix);
            // Use delimiter "/" to get only top-level "directories" under prefix.
            req = req.delimiter("/");
            if let Some(token) = continuation {
                req = req.continuation_token(token);
            }
            let output = req.send().await?;

            // Common prefixes represent session ID "directories".
            for cp in output.common_prefixes() {
                if let Some(p) = cp.prefix() {
                    // Strip the base prefix and trailing slash to get session ID.
                    // e.g. "coop/sessions/abc-123/" → "abc-123"
                    if let Some(rest) = p.strip_prefix(&prefix) {
                        let sid = rest.trim_end_matches('/');
                        if !sid.is_empty() {
                            session_ids.push(sid.to_owned());
                        }
                    }
                }
            }

            if output.is_truncated() == Some(true) {
                continuation = output.next_continuation_token().map(|s| s.to_owned());
            } else {
                break;
            }
        }

        session_ids.sort();
        Ok(session_ids)
    }

    async fn list_spawns(&self, bead_id: &str) -> anyhow::Result<Vec<String>> {
        // A spawn root is identified by `<bead>/<spawn>/meta.json`. Listing
        // by the meta.json suffix avoids false positives from flat-layout
        // sub-directories like `<bead>/transcripts/` that share the same
        // first-level common prefix.
        let prefix = format!("{}/{}/", self.prefix, bead_id);
        let mut spawns = Vec::new();
        let mut continuation = None;

        loop {
            let mut req = self.aws().list_objects_v2().bucket(&self.bucket).prefix(&prefix);
            if let Some(token) = continuation {
                req = req.continuation_token(token);
            }
            let output = req.send().await?;

            for obj in output.contents() {
                if let Some(key) = obj.key() {
                    if let Some(rest) = key.strip_prefix(&prefix) {
                        if let Some(spawn) = rest.strip_suffix("/meta.json") {
                            if !spawn.is_empty() && !spawn.contains('/') {
                                spawns.push(spawn.to_owned());
                            }
                        }
                    }
                }
            }

            if output.is_truncated() == Some(true) {
                continuation = output.next_continuation_token().map(|s| s.to_owned());
            } else {
                break;
            }
        }

        spawns.sort();
        Ok(spawns)
    }

    async fn list_recording_chunks(&self, session_id: &str) -> anyhow::Result<Vec<String>> {
        let prefix = self.key(session_id, "recording/");
        let mut chunks = Vec::new();
        let mut continuation = None;

        loop {
            let mut req = self.aws().list_objects_v2().bucket(&self.bucket).prefix(&prefix);
            if let Some(token) = continuation {
                req = req.continuation_token(token);
            }
            let output = req.send().await?;

            for obj in output.contents() {
                if let Some(key) = obj.key() {
                    if let Some(filename) = key.rsplit('/').next() {
                        if filename.ends_with(".jsonl") {
                            chunks.push(filename.to_owned());
                        }
                    }
                }
            }

            if output.is_truncated() == Some(true) {
                continuation = output.next_continuation_token().map(|s| s.to_owned());
            } else {
                break;
            }
        }

        chunks.sort();
        Ok(chunks)
    }

    async fn list_workflow_files(&self, session_id: &str) -> anyhow::Result<Vec<String>> {
        let prefix = self.key(session_id, "workflow/");
        let mut files = Vec::new();
        let mut continuation = None;

        loop {
            let mut req = self.aws().list_objects_v2().bucket(&self.bucket).prefix(&prefix);
            if let Some(token) = continuation {
                req = req.continuation_token(token);
            }
            let output = req.send().await?;

            for obj in output.contents() {
                if let Some(key) = obj.key() {
                    // Keep the path relative to the `workflow/` prefix so the
                    // restore side can reconstruct the (possibly nested) layout
                    // under the local companion directory.
                    if let Some(rel) = key.strip_prefix(&prefix) {
                        if !rel.is_empty() {
                            files.push(rel.to_owned());
                        }
                    }
                }
            }

            if output.is_truncated() == Some(true) {
                continuation = output.next_continuation_token().map(|s| s.to_owned());
            } else {
                break;
            }
        }

        files.sort();
        Ok(files)
    }

    async fn download_bytes(&self, session_id: &str, s3_path: &str) -> anyhow::Result<Vec<u8>> {
        let key = self.key(session_id, s3_path);
        let output = self.aws().get_object().bucket(&self.bucket).key(&key).send().await?;
        let bytes = output.body.collect().await?.into_bytes();
        Ok(bytes.to_vec())
    }
}

/// Spawn the S3 persistence subscriber.
///
/// Listens to transcript and recording broadcast channels and uploads
/// artifacts to S3 as they are created. Also periodically uploads the
/// session log for crash resilience.
///
/// Returns an [`S3SessionHandle`] — call [`S3SessionHandle::set_exit`]
/// before triggering shutdown so the final meta upload includes exit info.
// TODO(refactor): group session metadata params into an S3SubscriberConfig struct
#[allow(clippy::too_many_arguments)]
pub fn spawn_subscriber<S: S3Storage>(
    s3: Arc<S>,
    session_id: String,
    session_dir: Option<PathBuf>,
    transcript_tx: &broadcast::Sender<TranscriptEvent>,
    record_tx: &broadcast::Sender<RecordingEntry>,
    state_tx: &broadcast::Sender<TransitionEvent>,
    session_log_path: Arc<RwLock<Option<PathBuf>>>,
    upload_interval: Duration,
    agent_type: String,
    labels: Vec<String>,
    agent_name: Option<String>,
    project: Option<String>,
    role: Option<String>,
    bead_id: Option<String>,
    shutdown: CancellationToken,
) -> S3SessionHandle {
    let mut transcript_rx = transcript_tx.subscribe();
    let mut record_rx = record_tx.subscribe();
    let mut state_rx = state_tx.subscribe();
    let transcripts_dir = session_dir
        .as_ref()
        .map(|d| d.join("transcripts"))
        .unwrap_or_else(|| PathBuf::from("/tmp/coop-transcripts"));

    let exit_info: ExitInfo = Arc::new(parking_lot::Mutex::new(None));
    let exit_info_sub = Arc::clone(&exit_info);

    let started_at = now_unix_secs();

    // The workflow companion dir (`agent-*.jsonl` journals) is a Claude Code
    // feature; a Codex rollout has none, so skip the upload for it.
    let has_workflow_dir = agent_type != "codex";

    // Main subscriber task.
    let s3_main = Arc::clone(&s3);
    let sid_main = session_id.clone();
    let sd = shutdown.clone();

    let task = tokio::spawn(async move {
        // Build a snapshot of the current session metadata. Used for the
        // initial upload, every state-transition flush, and the periodic
        // refresh — so even on a non-clean exit (SIGHUP, crash) the session
        // is at least *discoverable* in S3 by its meta.json.
        let build_meta = |transcript_count: u32| SessionMeta {
            session_id: sid_main.clone(),
            agent_type: agent_type.clone(),
            started_at,
            ended_at: None,
            exit_code: None,
            labels: labels.clone(),
            agent_name: agent_name.clone(),
            project: project.clone(),
            role: role.clone(),
            bead_id: bead_id.clone(),
            exit_status: None,
            transcript_count: if transcript_count > 0 { Some(transcript_count) } else { None },
            harness: Some(agent_type.clone()),
        };

        // Upload initial session metadata before entering the select loop, so
        // that even if the pod is killed within milliseconds of startup the
        // session is discoverable in S3.
        if let Err(e) = s3_main.upload_meta(&sid_main, &build_meta(0)).await {
            warn!(error = %e, "s3: failed to upload session metadata");
        }

        let periodic_upload = tokio::time::sleep(upload_interval);
        tokio::pin!(periodic_upload);
        let mut recording_buffer: Vec<RecordingEntry> = Vec::new();
        let recording_batch_size: usize = 50;
        let mut transcript_count: u32 = 0;

        loop {
            tokio::select! {
                _ = sd.cancelled() => {
                    // Drain any pending transcript events that arrived before
                    // cancellation but haven't been processed yet.
                    while let Ok(e) = transcript_rx.try_recv() {
                        transcript_count += 1;
                        let path = transcripts_dir.join(format!("{}.jsonl", e.number));
                        let s3_path = format!("transcripts/{}.jsonl", e.number);
                        if let Err(err) = s3_main.upload_file(&sid_main, &s3_path, &path).await {
                            warn!(number = e.number, error = %err, "s3: failed to upload transcript on shutdown");
                        } else {
                            info!(number = e.number, "s3: uploaded transcript (shutdown drain)");
                        }
                    }

                    // Final flush on shutdown.
                    flush_recording_buffer(&*s3_main, &sid_main, &mut recording_buffer).await;
                    {
                        let p = session_log_path.read().await.clone();
                        upload_session_log(&*s3_main, &sid_main, p.as_deref()).await;
                        if has_workflow_dir {
                            upload_workflow_dir(&*s3_main, &sid_main, p.as_deref()).await;
                        }
                    }
                    upload_event_logs(&*s3_main, &sid_main, session_dir.as_deref()).await;

                    // Upload final session metadata with exit info.
                    let (exit_code, exit_status) = exit_info_sub
                        .lock()
                        .take()
                        .unwrap_or_else(|| (None, "error".into()));
                    let mut final_meta = build_meta(transcript_count);
                    final_meta.ended_at = Some(now_unix_secs());
                    final_meta.exit_code = exit_code;
                    final_meta.exit_status = Some(exit_status);
                    final_meta.transcript_count = Some(transcript_count);
                    if let Err(e) = s3_main.upload_meta(&sid_main, &final_meta).await {
                        warn!(error = %e, "s3: failed to upload final session metadata");
                    }
                    break;
                }

                event = transcript_rx.recv() => {
                    match event {
                        Ok(e) => {
                            transcript_count += 1;
                            let path = transcripts_dir.join(format!("{}.jsonl", e.number));
                            let s3_path = format!("transcripts/{}.jsonl", e.number);
                            if let Err(err) = s3_main.upload_file(&sid_main, &s3_path, &path).await {
                                warn!(
                                    number = e.number,
                                    error = %err,
                                    "s3: failed to upload transcript"
                                );
                            } else {
                                info!(number = e.number, "s3: uploaded transcript");
                            }

                            // Also upload session log after each transcript save
                            // (the session log changes when context compacts).
                            {
                                let p = session_log_path.read().await.clone();
                                upload_session_log(&*s3_main, &sid_main, p.as_deref()).await;
                            }
                        }
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            warn!("s3: transcript subscriber lagged by {n}");
                        }
                        Err(_) => break,
                    }
                }

                event = record_rx.recv() => {
                    match event {
                        Ok(entry) => {
                            recording_buffer.push(entry);
                            if recording_buffer.len() >= recording_batch_size {
                                flush_recording_buffer(&*s3_main, &sid_main, &mut recording_buffer).await;
                            }
                        }
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            warn!("s3: recording subscriber lagged by {n}");
                        }
                        Err(_) => break,
                    }
                }

                event = state_rx.recv() => {
                    match event {
                        Ok(_) => {
                            // Re-flush meta.json so a session is at least
                            // discoverable in S3 across every state
                            // transition — even when no transcript chunks
                            // have been uploaded yet (e.g. early SIGHUP).
                            if let Err(e) = s3_main.upload_meta(&sid_main, &build_meta(transcript_count)).await {
                                warn!(error = %e, "s3: failed to refresh meta on state transition");
                            }
                            // State transitions are significant audit moments;
                            // flush any buffered recording entries now so they
                            // survive a crash that follows the transition.
                            flush_recording_buffer(&*s3_main, &sid_main, &mut recording_buffer).await;
                            // Refresh session log too, in case the agent has
                            // logged interesting context since the last
                            // transcript event.
                            {
                                let p = session_log_path.read().await.clone();
                                upload_session_log(&*s3_main, &sid_main, p.as_deref()).await;
                                if has_workflow_dir {
                            upload_workflow_dir(&*s3_main, &sid_main, p.as_deref()).await;
                        }
                            }
                            upload_event_logs(&*s3_main, &sid_main, session_dir.as_deref()).await;
                        }
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            warn!("s3: state subscriber lagged by {n}");
                        }
                        Err(_) => break,
                    }
                }

                _ = &mut periodic_upload => {
                    // Periodic session log upload for crash resilience.
                    {
                        let p = session_log_path.read().await.clone();
                        upload_session_log(&*s3_main, &sid_main, p.as_deref()).await;
                        if has_workflow_dir {
                            upload_workflow_dir(&*s3_main, &sid_main, p.as_deref()).await;
                        }
                    }
                    flush_recording_buffer(&*s3_main, &sid_main, &mut recording_buffer).await;
                    upload_event_logs(&*s3_main, &sid_main, session_dir.as_deref()).await;
                    // Refresh meta on the periodic timer too — belt-and-braces
                    // so a session that goes quiet (no transitions, no
                    // transcripts) still ticks meta.json forward.
                    if let Err(e) = s3_main.upload_meta(&sid_main, &build_meta(transcript_count)).await {
                        warn!(error = %e, "s3: periodic meta refresh failed");
                    }
                    periodic_upload.as_mut().reset(tokio::time::Instant::now() + upload_interval);
                }
            }
        }
    });

    S3SessionHandle { exit_info, drain: shutdown, join: parking_lot::Mutex::new(Some(task)) }
}

/// Upload the session log file to S3.
async fn upload_session_log<S: S3Storage>(s3: &S, session_id: &str, log_path: Option<&Path>) {
    let Some(path) = log_path else { return };
    if !path.exists() {
        return;
    }
    if let Err(e) = s3.upload_file(session_id, "session.jsonl", path).await {
        warn!(error = %e, "s3: failed to upload session log");
    }
}

/// Upload event log files (state_events.jsonl, hook_events.jsonl) to S3.
async fn upload_event_logs<S: S3Storage>(s3: &S, session_id: &str, session_dir: Option<&Path>) {
    let Some(dir) = session_dir else { return };

    for filename in &["state_events.jsonl", "hook_events.jsonl"] {
        let path = dir.join(filename);
        if path.exists() {
            if let Err(e) = s3.upload_file(session_id, filename, &path).await {
                warn!(error = %e, file = %filename, "s3: failed to upload event log");
            }
        }
    }
}

/// Derive the Claude workflow companion directory from a session log path.
///
/// Claude Code stores its session transcript at
/// `<config>/projects/<project>/<uuid>.jsonl` and, when a dynamic Workflow
/// run is active, writes the per-run journals (`agent-*.jsonl` + run-state)
/// into the sibling directory `<config>/projects/<project>/<uuid>/` — the
/// same path with the `.jsonl` extension stripped (see
/// `driver/claude/retire.rs`, which retires this dir alongside the log).
///
/// Returns `None` when `log_path` does not end in `.jsonl`.
pub fn workflow_companion_dir(log_path: &Path) -> Option<PathBuf> {
    let s = log_path.to_str()?;
    s.strip_suffix(".jsonl").map(PathBuf::from)
}

/// Recursively collect files under `dir`, returning paths relative to `dir`.
///
/// Used to mirror the (possibly nested) workflow companion directory layout
/// into S3 under a flat-but-pathful set of `workflow/<rel>` keys.
fn collect_files_rel(dir: &Path) -> Vec<PathBuf> {
    let mut out = Vec::new();
    let mut stack = vec![dir.to_path_buf()];
    while let Some(cur) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&cur) else { continue };
        for entry in entries.flatten() {
            let path = entry.path();
            match entry.file_type() {
                Ok(ft) if ft.is_dir() => stack.push(path),
                Ok(ft) if ft.is_file() => {
                    if let Ok(rel) = path.strip_prefix(dir) {
                        out.push(rel.to_path_buf());
                    }
                }
                _ => {}
            }
        }
    }
    out
}

/// Upload the dynamic-workflow companion directory to S3.
///
/// Mirrors every file under the Claude companion dir (derived from the active
/// session log path) to `workflow/<rel>` keys. This carries the per-run
/// workflow journals (`agent-*.jsonl` + run-state) so that an S3-only restore
/// (PVC destroyed) can repopulate them and the Workflow tool can resume an
/// interrupted run via `resumeFromRunId` instead of re-running from scratch.
///
/// No-op when the log path is unset, has no companion dir, or the companion
/// dir does not yet exist (Claude creates it lazily — a session with no
/// active workflow run has nothing to upload).
async fn upload_workflow_dir<S: S3Storage>(s3: &S, session_id: &str, log_path: Option<&Path>) {
    let Some(log_path) = log_path else { return };
    let Some(dir) = workflow_companion_dir(log_path) else { return };
    if !dir.is_dir() {
        return;
    }
    for rel in collect_files_rel(&dir) {
        let local = dir.join(&rel);
        // S3 keys always use forward slashes regardless of platform.
        let rel_key =
            rel.components().filter_map(|c| c.as_os_str().to_str()).collect::<Vec<_>>().join("/");
        if rel_key.is_empty() {
            continue;
        }
        let s3_path = format!("workflow/{rel_key}");
        if let Err(e) = s3.upload_file(session_id, &s3_path, &local).await {
            warn!(error = %e, file = %rel_key, "s3: failed to upload workflow journal");
        }
    }
}

/// Flush buffered recording entries to S3 as a JSONL append.
async fn flush_recording_buffer<S: S3Storage>(
    s3: &S,
    session_id: &str,
    buffer: &mut Vec<RecordingEntry>,
) {
    if buffer.is_empty() {
        return;
    }

    let mut data = Vec::new();
    for entry in buffer.iter() {
        if let Ok(line) = serde_json::to_string(entry) {
            data.extend_from_slice(line.as_bytes());
            data.push(b'\n');
        }
    }

    // Upload recording chunks as numbered files to avoid overwrite races.
    // The first entry's seq gives us a unique, ordered name.
    let first_seq = buffer.first().map(|e| e.seq).unwrap_or(0);
    let last_seq = buffer.last().map(|e| e.seq).unwrap_or(0);
    let chunk_name = format!("recording/{first_seq}-{last_seq}.jsonl");

    if let Err(e) = s3.upload_bytes(session_id, &chunk_name, data).await {
        warn!(error = %e, "s3: failed to upload recording chunk");
        return;
    }
    buffer.clear();
}

/// Download all transcripts from S3 into a local session directory.
///
/// Used during session resume to restore transcripts before coop starts.
/// Returns the number of transcripts downloaded.
pub async fn restore_transcripts<S: S3Storage>(
    s3: &S,
    session_id: &str,
    transcripts_dir: &Path,
) -> anyhow::Result<u32> {
    tokio::fs::create_dir_all(transcripts_dir).await?;

    let numbers = s3.list_transcripts(session_id).await?;
    let mut downloaded = 0u32;

    for number in &numbers {
        let s3_path = format!("transcripts/{number}.jsonl");
        let local_path = transcripts_dir.join(format!("{number}.jsonl"));

        // Skip if already exists locally (e.g., from a previous partial restore).
        if local_path.exists() {
            downloaded += 1;
            continue;
        }

        match s3.download_file(session_id, &s3_path, &local_path).await {
            Ok(()) => {
                downloaded += 1;
                info!(number, "s3: restored transcript from S3");
            }
            Err(e) => {
                warn!(number, error = %e, "s3: failed to download transcript");
            }
        }
    }

    Ok(downloaded)
}

/// Download the session log from S3 for resume.
///
/// Downloads to the standard Claude session log location so that
/// `--resume` can discover it. Returns the local path if successful.
pub async fn restore_session_log<S: S3Storage>(
    s3: &S,
    source_session_id: &str,
    dest_path: &Path,
) -> anyhow::Result<()> {
    if let Some(parent) = dest_path.parent() {
        tokio::fs::create_dir_all(parent).await?;
    }
    s3.download_file(source_session_id, "session.jsonl", dest_path).await?;
    info!(
        source = %source_session_id,
        dest = %dest_path.display(),
        "s3: restored session log"
    );
    Ok(())
}

/// Download the dynamic-workflow companion directory from S3 for resume.
///
/// Repopulates the Claude companion dir (`<config>/projects/<project>/<uuid>/`)
/// with the per-run workflow journals (`agent-*.jsonl` + run-state) archived
/// under the session's `workflow/` prefix. After an S3-only restore (PVC
/// destroyed), this lets the Workflow tool resume an interrupted run via
/// `resumeFromRunId` rather than re-authoring it from the transcript.
///
/// Files already present locally are not overwritten (mirrors
/// [`restore_transcripts`] — a partial PVC may already carry fresher state).
/// Returns the number of files materialized (downloaded or already present).
pub async fn restore_workflow_dir<S: S3Storage>(
    s3: &S,
    source_session_id: &str,
    companion_dir: &Path,
) -> anyhow::Result<u32> {
    let files = s3.list_workflow_files(source_session_id).await?;
    if files.is_empty() {
        return Ok(0);
    }
    tokio::fs::create_dir_all(companion_dir).await?;

    let mut restored = 0u32;
    for rel in &files {
        // `rel` is a forward-slash S3 path relative to `workflow/`; rebuild it
        // as a local path under the companion dir.
        let mut local = companion_dir.to_path_buf();
        for seg in rel.split('/').filter(|s| !s.is_empty()) {
            local.push(seg);
        }

        if local.exists() {
            restored += 1;
            continue;
        }

        let s3_path = format!("workflow/{rel}");
        match s3.download_file(source_session_id, &s3_path, &local).await {
            Ok(()) => {
                restored += 1;
                info!(file = %rel, "s3: restored workflow journal from S3");
            }
            Err(e) => {
                warn!(file = %rel, error = %e, "s3: failed to download workflow journal");
            }
        }
    }

    Ok(restored)
}

#[cfg(test)]
#[path = "s3_tests.rs"]
mod tests;
