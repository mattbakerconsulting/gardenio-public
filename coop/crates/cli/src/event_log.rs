// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! File-backed append-only event log for state transitions and hook events.
//!
//! Events are appended as JSONL to files in the session directory. Catchup
//! reads from the file and filters by sequence number — no in-memory buffer.

use std::fs::File;
use std::io::{BufWriter, Write};
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};

use serde::{Deserialize, Serialize};
use tokio::sync::{mpsc, oneshot};

use crate::event::{RawHookEvent, TransitionEvent};

/// File-backed append-only event log.
///
/// State transitions and hook events are appended as JSONL lines. Catchup
/// reads the file and filters by seq — no in-memory buffer needed.
///
/// The log is never truncated — events accumulate across credential switches
/// so reconnecting clients can always catch up from their last known position.
pub struct EventLog {
    state_path: Option<PathBuf>,
    hook_path: Option<PathBuf>,
    hook_seq: AtomicU64,
    writer_tx: Option<mpsc::Sender<EventLogCommand>>,
}

const EVENT_LOG_QUEUE_CAPACITY: usize = 256;
const EVENT_LOG_BATCH_SIZE: usize = 64;

enum EventLogCommand {
    Transition(TransitionEntry),
    Hook(HookEntry),
    Flush(oneshot::Sender<()>),
}

/// A serialized state transition entry.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TransitionEntry {
    pub prev: String,
    pub next: String,
    pub seq: u64,
    pub cause: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_message: Option<String>,
    pub timestamp_ms: u64,
}

/// A serialized hook event entry.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HookEntry {
    pub hook_seq: u64,
    pub json: serde_json::Value,
    pub timestamp_ms: u64,
}

/// Catchup response combining both event types.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CatchupResponse {
    pub state_events: Vec<TransitionEntry>,
    pub hook_events: Vec<HookEntry>,
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

impl EventLog {
    /// Create a new event log. If `session_dir` is `None` (tests/attach mode),
    /// no files are written and catchup always returns empty.
    pub fn new(session_dir: Option<&std::path::Path>) -> Self {
        let (state_path, hook_path) = match session_dir {
            Some(dir) => {
                (Some(dir.join("state_events.jsonl")), Some(dir.join("hook_events.jsonl")))
            }
            None => (None, None),
        };
        let writer_tx = match (&state_path, &hook_path) {
            (Some(state_path), Some(hook_path)) => {
                Some(spawn_event_log_writer(state_path.clone(), hook_path.clone()))
            }
            _ => None,
        };
        Self { state_path, hook_path, hook_seq: AtomicU64::new(0), writer_tx }
    }

    /// Queue a state transition for the blocking log writer.
    pub async fn push_transition(&self, event: &TransitionEvent) {
        let Some(writer_tx) = &self.writer_tx else {
            return;
        };
        let entry = TransitionEntry {
            prev: event.prev.as_str().to_owned(),
            next: event.next.as_str().to_owned(),
            seq: event.seq,
            cause: event.cause.clone(),
            last_message: event.last_message.clone(),
            timestamp_ms: now_ms(),
        };
        if writer_tx.send(EventLogCommand::Transition(entry)).await.is_err() {
            tracing::warn!("event log writer stopped before transition could be queued");
        }
    }

    /// Queue a hook event for the blocking log writer.
    pub async fn push_hook(&self, event: &RawHookEvent) {
        let Some(writer_tx) = &self.writer_tx else {
            return;
        };
        let hook_seq = self.hook_seq.fetch_add(1, Ordering::Relaxed);
        let entry = HookEntry { hook_seq, json: event.json.clone(), timestamp_ms: now_ms() };
        if writer_tx.send(EventLogCommand::Hook(entry)).await.is_err() {
            tracing::warn!("event log writer stopped before hook could be queued");
        }
    }

    /// Read state transition events with seq > `since_seq`.
    pub async fn catchup_state(&self, since_seq: u64) -> Vec<TransitionEntry> {
        let Some(ref path) = self.state_path else {
            return vec![];
        };
        self.flush().await;
        let Ok(contents) = tokio::fs::read_to_string(path).await else {
            return vec![];
        };
        contents
            .lines()
            .filter_map(|line| serde_json::from_str::<TransitionEntry>(line).ok())
            .filter(|e| e.seq > since_seq)
            .collect()
    }

    /// Read hook events with hook_seq > `since_hook_seq`.
    pub async fn catchup_hooks(&self, since_hook_seq: u64) -> Vec<HookEntry> {
        let Some(ref path) = self.hook_path else {
            return vec![];
        };
        self.flush().await;
        let Ok(contents) = tokio::fs::read_to_string(path).await else {
            return vec![];
        };
        contents
            .lines()
            .filter_map(|line| serde_json::from_str::<HookEntry>(line).ok())
            .filter(|e| e.hook_seq > since_hook_seq)
            .collect()
    }

    async fn flush(&self) {
        let Some(writer_tx) = &self.writer_tx else {
            return;
        };
        let (done_tx, done_rx) = oneshot::channel();
        if writer_tx.send(EventLogCommand::Flush(done_tx)).await.is_ok() {
            let _ = done_rx.await;
        }
    }
}

fn spawn_event_log_writer(
    state_path: PathBuf,
    hook_path: PathBuf,
) -> mpsc::Sender<EventLogCommand> {
    let (writer_tx, mut writer_rx) = mpsc::channel(EVENT_LOG_QUEUE_CAPACITY);
    tokio::task::spawn_blocking(move || {
        if let Some(parent) = state_path.parent() {
            if let Err(error) = std::fs::create_dir_all(parent) {
                tracing::warn!(%error, "event log writer could not create session directory");
            }
        }

        let mut state_writer = None;
        let mut hook_writer = None;
        while let Some(command) = writer_rx.blocking_recv() {
            process_event_log_command(
                command,
                &state_path,
                &hook_path,
                &mut state_writer,
                &mut hook_writer,
            );
            for _ in 1..EVENT_LOG_BATCH_SIZE {
                let Ok(command) = writer_rx.try_recv() else {
                    break;
                };
                process_event_log_command(
                    command,
                    &state_path,
                    &hook_path,
                    &mut state_writer,
                    &mut hook_writer,
                );
            }
            flush_writer(&mut state_writer, "state");
            flush_writer(&mut hook_writer, "hook");
        }
    });
    writer_tx
}

fn process_event_log_command(
    command: EventLogCommand,
    state_path: &std::path::Path,
    hook_path: &std::path::Path,
    state_writer: &mut Option<BufWriter<File>>,
    hook_writer: &mut Option<BufWriter<File>>,
) {
    match command {
        EventLogCommand::Transition(entry) => {
            append_json_line(state_path, state_writer, &entry, "state");
        }
        EventLogCommand::Hook(entry) => {
            append_json_line(hook_path, hook_writer, &entry, "hook");
        }
        EventLogCommand::Flush(done) => {
            flush_writer(state_writer, "state");
            flush_writer(hook_writer, "hook");
            let _ = done.send(());
        }
    }
}

fn append_json_line<T: Serialize>(
    path: &std::path::Path,
    writer: &mut Option<BufWriter<File>>,
    entry: &T,
    kind: &str,
) {
    if writer.is_none() {
        match std::fs::OpenOptions::new().create(true).append(true).open(path) {
            Ok(file) => *writer = Some(BufWriter::new(file)),
            Err(error) => {
                tracing::warn!(%error, kind, "event log writer could not open file");
                return;
            }
        }
    }
    let Some(writer) = writer.as_mut() else {
        return;
    };
    if let Err(error) = serde_json::to_writer(&mut *writer, entry) {
        tracing::warn!(%error, kind, "event log writer could not serialize entry");
        return;
    }
    if let Err(error) = writer.write_all(b"\n") {
        tracing::warn!(%error, kind, "event log writer could not append entry");
    }
}

fn flush_writer(writer: &mut Option<BufWriter<File>>, kind: &str) {
    if let Some(writer) = writer {
        if let Err(error) = writer.flush() {
            tracing::warn!(%error, kind, "event log writer could not flush file");
        }
    }
}

#[cfg(test)]
#[path = "event_log_tests.rs"]
mod tests;
