// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Session recording — captures screen snapshots at semantic events.
//!
//! Records state transitions and hook events with full screen snapshots
//! as JSONL to `<session-dir>/recording.jsonl`. First line is a header,
//! subsequent lines are entries.

use std::hash::{Hash, Hasher};
use std::io::{BufWriter, Write};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU16, AtomicU64, Ordering};
use std::sync::{Arc, OnceLock};
use std::time::Instant;

use serde::{Deserialize, Serialize};
use tokio::sync::{broadcast, mpsc, oneshot, Mutex};
use tokio_util::sync::CancellationToken;

use crate::event::{RawHookEvent, TransitionEvent};
use crate::screen::ScreenSnapshot;
use crate::transport::state::TerminalState;

/// Env var that disables session-recording redaction when set to a truthy value
/// (`off`, `0`, `false`, `no`). Intended as a controlled-debugging escape hatch.
pub const REDACTION_ENV: &str = "COOP_RECORD_REDACTION";

/// Replacement token for redacted secret values.
const REDACTED_VALUE: &str = "<redacted>";

/// Compile the redaction regex. Matches `key=value` / `key: value` shapes for
/// common secret keywords (api_key, api-key, apikey, password, token, secret,
/// bearer) and captures the keyword + delimiter so the value can be replaced.
#[allow(clippy::expect_used)]
fn redaction_regex() -> &'static regex::Regex {
    // (?i): case-insensitive. Captures the prefix (keyword + delimiter +
    // surrounding whitespace) so the substitution can keep it intact and only
    // replace the trailing non-whitespace value.
    static REGEX: OnceLock<regex::Regex> = OnceLock::new();
    REGEX.get_or_init(|| {
        regex::Regex::new(r"(?i)((?:api[_-]?key|password|token|secret|bearer)\s*[:=]\s*)\S+")
            .expect("static redaction regex must compile")
    })
}

/// Compile the credential-SHAPE redaction regex (claudemore F16, R4-SEC-3).
///
/// The keyword regex above only fires when a `key=`/`token:` prefix is present.
/// A real reasoning agent can echo a *bare* provider credential onto the screen
/// with no such prefix (e.g. it prints `$ANTHROPIC_API_KEY` or pastes a key into
/// a command). This regex matches the credential's own recognizable prefix so it
/// is redacted whole, no keyword required:
///
///   - `sk-ant-…`      — Anthropic API / OAuth keys.
///   - `sk-…` / `vk-…` — LiteLLM virtual keys and sk-shaped keys (the
///     `x-litellm-api-key` header value the controller injects; see
///     `claudemore-r4-derisking.md` F16).
///   - `ghp_…` / `glpat-…` / `github_pat_…` — SCM tokens that also ride the pod
///     env (per the buildCoopEnv audit) and can be echoed the same way.
///
/// Word-boundary anchored (`\b`) so the token is only matched where its prefix
/// actually starts a word, and the trailing run captures the base64url/hex body
/// (`[A-Za-z0-9_-]`) that these credentials use — stopping at whitespace or any
/// other delimiter so surrounding prose is preserved. Two body-length floors
/// keep false positives out of recordings: the distinctive prefixes (`sk-ant-`,
/// `ghp_`, `glpat-`, `github_pat_`) require a >=8-char body, while the bare
/// 2-char litellm prefixes (`sk-`, `vk-`) require a >=16-char body so an ordinary
/// hyphenated identifier (e.g. `task-sk-workaround`) is never mistaken for a key
/// — real litellm keys are 20+ chars, so this never under-redacts. `sk-ant-` is
/// matched before the shorter `sk-` alternative so the more specific prefix wins.
#[allow(clippy::expect_used)]
fn credential_shape_regex() -> &'static regex::Regex {
    static REGEX: OnceLock<regex::Regex> = OnceLock::new();
    REGEX.get_or_init(|| {
        regex::Regex::new(
            r"\b((sk-ant-|ghp_|glpat-|github_pat_)[A-Za-z0-9_-]{8,}|(sk-|vk-)[A-Za-z0-9_-]{16,})",
        )
        .expect("static credential-shape regex must compile")
    })
}

/// Whether the env-var escape hatch disables redaction. Read once at
/// `RecordingState::new` so the setting is fixed for the lifetime of the
/// recorder (and so tests don't need to mutate process-global env vars,
/// which require `unsafe` and are forbidden workspace-wide).
fn env_disables_redaction() -> bool {
    match std::env::var(REDACTION_ENV) {
        Ok(v) => {
            let v = v.trim().to_ascii_lowercase();
            matches!(v.as_str(), "off" | "0" | "false" | "no")
        }
        Err(_) => false,
    }
}

/// Redact secret-shaped lines in a screen snapshot: values in `lines` and
/// `ansi` matching the redaction regex become `<redacted>`. When `enabled` is
/// `false`, the snapshot is returned unchanged.
fn redact_snapshot_owned(mut snap: ScreenSnapshot, enabled: bool) -> ScreenSnapshot {
    if !enabled {
        return snap;
    }
    snap.lines = snap.lines.into_iter().map(|line| redact_line(&line)).collect();
    snap.ansi = snap.ansi.into_iter().map(|line| redact_line(&line)).collect();
    snap
}

/// Redact a single line with both passes: the keyword-prefixed pass first, then
/// the bare credential-SHAPE pass (F16). Order does not matter for correctness —
/// a value already replaced with `<redacted>` no longer matches either regex —
/// but running keyword-first keeps `token=sk-…` collapsing to a single marker.
fn redact_line(line: &str) -> String {
    let keyword_redacted = redaction_regex()
        .replace_all(line, |caps: &regex::Captures<'_>| format!("{}{}", &caps[1], REDACTED_VALUE))
        .into_owned();
    credential_shape_regex().replace_all(&keyword_redacted, REDACTED_VALUE).into_owned()
}

/// File-backed session recording state.
///
/// Captures screen snapshots at semantic events (state transitions, hook events)
/// and writes them as JSONL entries. The recording can be toggled at runtime.
pub struct RecordingState {
    enabled: AtomicBool,
    started_at: Mutex<Option<Instant>>,
    started_at_unix_ms: AtomicU64,
    path: Option<PathBuf>,
    seq: AtomicU64,
    header_written: AtomicBool,
    cols: AtomicU16,
    rows: AtomicU16,
    /// Whether to redact secret-shaped lines from snapshots before write.
    /// Resolved from `COOP_RECORD_REDACTION` env at construction time.
    redact: bool,
    writer_tx: Option<mpsc::Sender<RecordingCommand>>,
    pub record_tx: broadcast::Sender<RecordingEntry>,
}

const RECORDING_QUEUE_CAPACITY: usize = 256;
const RECORDING_BATCH_SIZE: usize = 64;

enum RecordingCommand {
    Header(RecordingHeader),
    Entry { entry: RecordingEntry, complete: oneshot::Sender<RecordingEntry> },
    Flush(oneshot::Sender<()>),
}

/// A single recording entry (broadcast + serialized to JSONL).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RecordingEntry {
    pub ts: u64,
    pub seq: u64,
    pub kind: String,
    pub detail: serde_json::Value,
    pub screen: ScreenSnapshot,
}

/// Recording header written as the first JSONL line.
#[derive(Debug, Clone, Serialize, Deserialize)]
struct RecordingHeader {
    version: u32,
    cols: u16,
    rows: u16,
    timestamp: u64,
}

/// Status snapshot returned by the status endpoint.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RecordingStatus {
    pub enabled: bool,
    pub path: Option<String>,
    pub entries: u64,
}

fn now_unix_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

impl RecordingState {
    /// Create a new recording state.
    ///
    /// If `session_dir` is `None` (tests/attach mode), no files are written.
    pub fn new(session_dir: Option<&std::path::Path>, cols: u16, rows: u16) -> Self {
        Self::new_with_redact(session_dir, cols, rows, !env_disables_redaction())
    }

    /// Same as `new`, but with explicit control over redaction. Used by tests
    /// to avoid mutating process-global env vars.
    pub fn new_with_redact(
        session_dir: Option<&std::path::Path>,
        cols: u16,
        rows: u16,
        redact: bool,
    ) -> Self {
        let path = session_dir.map(|dir| dir.join("recording.jsonl"));
        let writer_tx = path.as_ref().map(|path| spawn_recording_writer(path.clone(), redact));
        let (record_tx, _) = broadcast::channel(256);
        Self {
            enabled: AtomicBool::new(false),
            started_at: Mutex::new(None),
            started_at_unix_ms: AtomicU64::new(0),
            path,
            seq: AtomicU64::new(0),
            header_written: AtomicBool::new(false),
            cols: AtomicU16::new(cols),
            rows: AtomicU16::new(rows),
            redact,
            writer_tx,
            record_tx,
        }
    }

    /// Enable recording. Writes the header on first enable.
    pub async fn enable(&self) {
        self.enabled.store(true, Ordering::Release);
        let mut started = self.started_at.lock().await;
        if started.is_none() {
            *started = Some(Instant::now());
            self.started_at_unix_ms.store(now_unix_ms(), Ordering::Release);
        }
        self.write_header_once().await;
    }

    /// Disable recording.
    pub fn disable(&self) {
        self.enabled.store(false, Ordering::Release);
    }

    /// Whether recording is currently enabled.
    pub fn is_enabled(&self) -> bool {
        self.enabled.load(Ordering::Acquire)
    }

    /// Append a recording entry with the given kind, detail, and screen snapshot.
    pub async fn push(&self, kind: &str, detail: serde_json::Value, screen: ScreenSnapshot) {
        if !self.is_enabled() {
            return;
        }

        let ts = {
            let started = self.started_at.lock().await;
            match *started {
                Some(ref instant) => instant.elapsed().as_millis() as u64,
                None => 0,
            }
        };

        let seq = self.seq.fetch_add(1, Ordering::Relaxed) + 1;

        let entry = RecordingEntry { ts, seq, kind: kind.to_owned(), detail, screen };
        let entry = match &self.writer_tx {
            Some(writer_tx) => {
                let (complete_tx, complete_rx) = oneshot::channel();
                match writer_tx.send(RecordingCommand::Entry { entry, complete: complete_tx }).await
                {
                    Ok(()) => match complete_rx.await {
                        Ok(entry) => entry,
                        Err(_) => {
                            tracing::warn!("recording writer stopped before completing entry");
                            return;
                        }
                    },
                    Err(error) => {
                        tracing::warn!("recording writer stopped before entry could be queued");
                        redact_recording_entry(error.0.into_entry(), self.redact)
                    }
                }
            }
            None => redact_recording_entry(entry, self.redact),
        };

        // Broadcast to subscribers
        let _ = self.record_tx.send(entry);
    }

    /// Return the current recording status.
    pub fn status(&self) -> RecordingStatus {
        RecordingStatus {
            enabled: self.is_enabled(),
            path: self.path.as_ref().map(|p| p.display().to_string()),
            entries: self.seq.load(Ordering::Relaxed),
        }
    }

    /// Read entries from the recording file with seq > `since_seq`.
    pub async fn catchup(&self, since_seq: u64) -> Vec<RecordingEntry> {
        let Some(ref path) = self.path else {
            return vec![];
        };
        self.flush().await;
        let Ok(contents) = tokio::fs::read_to_string(path).await else {
            return vec![];
        };
        contents
            .lines()
            .filter_map(|line| serde_json::from_str::<RecordingEntry>(line).ok())
            .filter(|e| e.seq > since_seq)
            .collect()
    }

    /// Read the full recording file contents.
    pub async fn download(&self) -> Option<Vec<u8>> {
        let path = self.path.as_ref()?;
        self.flush().await;
        tokio::fs::read(path).await.ok()
    }

    /// Queue the header line for the recording writer (once).
    async fn write_header_once(&self) {
        if self.header_written.swap(true, Ordering::AcqRel) {
            return;
        }
        let Some(writer_tx) = &self.writer_tx else {
            return;
        };
        let header = RecordingHeader {
            version: 1,
            cols: self.cols.load(Ordering::Relaxed),
            rows: self.rows.load(Ordering::Relaxed),
            timestamp: self.started_at_unix_ms.load(Ordering::Acquire),
        };
        if writer_tx.send(RecordingCommand::Header(header)).await.is_err() {
            tracing::warn!("recording writer stopped before header could be queued");
        }
    }

    async fn flush(&self) {
        let Some(writer_tx) = &self.writer_tx else {
            return;
        };
        let (done_tx, done_rx) = oneshot::channel();
        if writer_tx.send(RecordingCommand::Flush(done_tx)).await.is_ok() {
            let _ = done_rx.await;
        }
    }
}

impl RecordingCommand {
    fn into_entry(self) -> RecordingEntry {
        match self {
            Self::Entry { entry, .. } => entry,
            Self::Header(_) | Self::Flush(_) => unreachable!("only entry sends can fail here"),
        }
    }
}

fn spawn_recording_writer(path: PathBuf, redact: bool) -> mpsc::Sender<RecordingCommand> {
    let (writer_tx, mut writer_rx) = mpsc::channel(RECORDING_QUEUE_CAPACITY);
    tokio::task::spawn_blocking(move || {
        if let Some(parent) = path.parent() {
            if let Err(error) = std::fs::create_dir_all(parent) {
                tracing::warn!(%error, "recording writer could not create session directory");
            }
        }

        let mut writer = None;
        while let Some(command) = writer_rx.blocking_recv() {
            process_recording_command(command, &path, redact, &mut writer);
            for _ in 1..RECORDING_BATCH_SIZE {
                let Ok(command) = writer_rx.try_recv() else {
                    break;
                };
                process_recording_command(command, &path, redact, &mut writer);
            }
            flush_recording_writer(&mut writer);
        }
    });
    writer_tx
}

fn process_recording_command(
    command: RecordingCommand,
    path: &std::path::Path,
    redact: bool,
    writer: &mut Option<BufWriter<std::fs::File>>,
) {
    match command {
        RecordingCommand::Header(header) => append_recording_line(path, writer, &header),
        RecordingCommand::Entry { entry, complete } => {
            let entry = redact_recording_entry(entry, redact);
            append_recording_line(path, writer, &entry);
            let _ = complete.send(entry);
        }
        RecordingCommand::Flush(done) => {
            flush_recording_writer(writer);
            let _ = done.send(());
        }
    }
}

fn redact_recording_entry(mut entry: RecordingEntry, enabled: bool) -> RecordingEntry {
    entry.screen = redact_snapshot_owned(entry.screen, enabled);
    entry
}

fn append_recording_line<T: Serialize>(
    path: &std::path::Path,
    writer: &mut Option<BufWriter<std::fs::File>>,
    entry: &T,
) {
    if writer.is_none() {
        match std::fs::OpenOptions::new().create(true).append(true).open(path) {
            Ok(file) => *writer = Some(BufWriter::new(file)),
            Err(error) => {
                tracing::warn!(%error, "recording writer could not open file");
                return;
            }
        }
    }
    let Some(writer) = writer.as_mut() else {
        return;
    };
    if let Err(error) = serde_json::to_writer(&mut *writer, entry) {
        tracing::warn!(%error, "recording writer could not serialize entry");
        return;
    }
    if let Err(error) = writer.write_all(b"\n") {
        tracing::warn!(%error, "recording writer could not append entry");
    }
}

fn flush_recording_writer(writer: &mut Option<BufWriter<std::fs::File>>) {
    if let Some(writer) = writer {
        if let Err(error) = writer.flush() {
            tracing::warn!(%error, "recording writer could not flush file");
        }
    }
}

/// Hash a screen snapshot's visible content for deduplication.
fn screen_hash(snap: &ScreenSnapshot) -> u64 {
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    snap.lines.hash(&mut hasher);
    snap.cursor.row.hash(&mut hasher);
    snap.cursor.col.hash(&mut hasher);
    snap.alt_screen.hash(&mut hasher);
    hasher.finish()
}

/// Spawn the recording subscriber task.
///
/// Listens to state transitions and hook events, capturing screen snapshots
/// at each. Also takes a periodic "screen" snapshot 30s after the last hook
/// event to capture intermediate activity while the agent is working.
///
/// Duplicate screen captures (identical content) are suppressed via hashing.
pub fn spawn_subscriber(
    record: Arc<RecordingState>,
    terminal: Arc<TerminalState>,
    state_tx: &broadcast::Sender<TransitionEvent>,
    hook_tx: &broadcast::Sender<RawHookEvent>,
    shutdown: CancellationToken,
) {
    let mut state_rx = state_tx.subscribe();
    let mut hook_rx = hook_tx.subscribe();
    let screen_delay = std::time::Duration::from_secs(30);
    tokio::spawn(async move {
        let screen_sleep = tokio::time::sleep(screen_delay);
        tokio::pin!(screen_sleep);
        let mut screen_armed = false;
        let mut last_hash: u64 = 0;
        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                event = state_rx.recv() => {
                    match event {
                        Ok(e) => {
                            if record.is_enabled() {
                                let snap = terminal.screen.read().await.snapshot();
                                let h = screen_hash(&snap);
                                if h != last_hash {
                                    last_hash = h;
                                    let detail = serde_json::json!({
                                        "prev": e.prev.as_str(),
                                        "next": e.next.as_str(),
                                        "cause": e.cause,
                                    });
                                    record.push("state", detail, snap).await;
                                }
                            }
                        }
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            tracing::warn!("recording: state subscriber lagged by {n}");
                        }
                        Err(_) => break,
                    }
                }
                event = hook_rx.recv() => {
                    match event {
                        Ok(e) => {
                            if record.is_enabled() {
                                let snap = terminal.screen.read().await.snapshot();
                                let h = screen_hash(&snap);
                                if h != last_hash {
                                    last_hash = h;
                                    let detail = serde_json::json!({
                                        "hook_seq": 0,
                                        "json": e.json,
                                    });
                                    record.push("hook", detail, snap).await;
                                }
                                // Reset the periodic timer on each hook event.
                                screen_sleep.as_mut().reset(tokio::time::Instant::now() + screen_delay);
                                screen_armed = true;
                            }
                        }
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            tracing::warn!("recording: hook subscriber lagged by {n}");
                        }
                        Err(_) => break,
                    }
                }
                _ = &mut screen_sleep, if screen_armed => {
                    screen_armed = false;
                    if record.is_enabled() {
                        let snap = terminal.screen.read().await.snapshot();
                        let h = screen_hash(&snap);
                        if h != last_hash {
                            last_hash = h;
                            record.push("screen", serde_json::json!({}), snap).await;
                        }
                    }
                }
            }
        }
    });
}

#[cfg(test)]
#[path = "record_tests.rs"]
mod tests;
