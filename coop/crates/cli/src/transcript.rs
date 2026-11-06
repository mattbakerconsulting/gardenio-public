// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Transcript snapshots — numbered copies of the session log.
//!
//! When Claude compacts its context window, the full conversation history
//! would otherwise be lost. This module saves the JSONL session log as a
//! numbered "transcript" before each compaction, creating a recoverable
//! history that clients can list, fetch, and catch up from.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::time::SystemTime;

use serde::{Deserialize, Serialize};
use tokio::sync::broadcast;
use tokio::sync::RwLock;

/// Metadata for a single transcript snapshot.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TranscriptMeta {
    pub number: u32,
    pub timestamp: String,
    pub line_count: u64,
    pub byte_size: u64,
}

/// Broadcast event emitted when a new transcript is saved.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TranscriptEvent {
    pub number: u32,
    pub timestamp: String,
    pub line_count: u64,
    pub seq: u64,
}

/// Response from the catchup endpoint.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CatchupResponse {
    pub transcripts: Vec<CatchupTranscript>,
    pub live_lines: Vec<String>,
    pub current_transcript: u32,
    pub current_line: u64,
}

/// A transcript with its full line content (returned by catchup).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CatchupTranscript {
    pub number: u32,
    pub timestamp: String,
    pub lines: Vec<String>,
}

/// Runtime state for the transcript snapshot system.
pub struct TranscriptState {
    transcripts_dir: PathBuf,
    session_log_path: RwLock<Option<PathBuf>>,
    transcripts: RwLock<Vec<TranscriptMeta>>,
    pub transcript_tx: broadcast::Sender<TranscriptEvent>,
    seq: AtomicU64,
    next_number: AtomicU32,
}

impl TranscriptState {
    /// Create a new `TranscriptState`, scanning for existing transcript files.
    pub fn new(
        transcripts_dir: PathBuf,
        session_log_path: Option<PathBuf>,
    ) -> anyhow::Result<Self> {
        std::fs::create_dir_all(&transcripts_dir)?;

        // Scan for existing <N>.jsonl files (supports session resume).
        let mut existing: Vec<TranscriptMeta> = Vec::new();
        if let Ok(entries) = std::fs::read_dir(&transcripts_dir) {
            for entry in entries.flatten() {
                let name = entry.file_name();
                let name_str = name.to_string_lossy();
                if let Some(num_str) = name_str.strip_suffix(".jsonl") {
                    if let Ok(number) = num_str.parse::<u32>() {
                        if let Ok(metadata) = entry.metadata() {
                            let line_count = count_lines(&entry.path()).unwrap_or(0);
                            let timestamp = metadata
                                .modified()
                                .ok()
                                .map(unix_timestamp_string)
                                .unwrap_or_default();
                            existing.push(TranscriptMeta {
                                number,
                                timestamp,
                                line_count,
                                byte_size: metadata.len(),
                            });
                        }
                    }
                }
            }
        }
        existing.sort_by_key(|m| m.number);

        let next_number = existing.last().map(|m| m.number + 1).unwrap_or(1);
        let (transcript_tx, _) = broadcast::channel(64);

        Ok(Self {
            transcripts_dir,
            session_log_path: RwLock::new(session_log_path),
            transcripts: RwLock::new(existing),
            transcript_tx,
            seq: AtomicU64::new(0),
            next_number: AtomicU32::new(next_number),
        })
    }

    /// Update the session log path.
    ///
    /// Ensures `save_snapshot` copies from the actual session log path.
    pub async fn update_session_log_path(&self, path: Option<PathBuf>) {
        *self.session_log_path.write().await = path;
    }

    /// Save a snapshot of the current session log as the next numbered transcript.
    pub async fn save_snapshot(&self) -> anyhow::Result<TranscriptMeta> {
        let log_path = self
            .session_log_path
            .read()
            .await
            .clone()
            .ok_or_else(|| anyhow::anyhow!("no session log path configured"))?;

        let number = self.next_number.fetch_add(1, Ordering::Relaxed);
        let dest = self.transcripts_dir.join(format!("{number}.jsonl"));

        // Copy the session log to the transcript file.
        tokio::fs::copy(&log_path, &dest).await?;

        let file_meta = tokio::fs::metadata(&dest).await?;
        let line_count = {
            let dest_clone = dest.clone();
            tokio::task::spawn_blocking(move || count_lines(&dest_clone).unwrap_or(0)).await?
        };
        let timestamp = unix_timestamp_string(SystemTime::now());

        let meta = TranscriptMeta {
            number,
            timestamp: timestamp.clone(),
            line_count,
            byte_size: file_meta.len(),
        };

        self.transcripts.write().await.push(meta.clone());

        let seq = self.seq.fetch_add(1, Ordering::Relaxed);
        let event = TranscriptEvent { number, timestamp, line_count, seq };
        let _ = self.transcript_tx.send(event);

        Ok(meta)
    }

    /// List all transcript metadata.
    pub async fn list(&self) -> Vec<TranscriptMeta> {
        self.transcripts.read().await.clone()
    }

    /// Read the full content of a transcript by number.
    pub async fn get_content(&self, number: u32) -> anyhow::Result<String> {
        let path = self.transcripts_dir.join(format!("{number}.jsonl"));
        if !path.exists() {
            anyhow::bail!("transcript {number} not found");
        }
        let content = tokio::fs::read_to_string(&path).await?;
        Ok(content)
    }

    /// Catch up from a given transcript number and line offset.
    ///
    /// Returns all transcripts after `since_transcript`, plus live lines
    /// from the current session log starting after `since_line`.
    pub async fn catchup(
        &self,
        since_transcript: u32,
        since_line: u64,
    ) -> anyhow::Result<CatchupResponse> {
        let all = self.transcripts.read().await;

        // Collect full transcripts after since_transcript.
        let mut transcripts = Vec::new();
        for meta in all.iter() {
            if meta.number > since_transcript {
                let path = self.transcripts_dir.join(format!("{}.jsonl", meta.number));
                let content = tokio::fs::read_to_string(&path).await.unwrap_or_default();
                let lines: Vec<String> = content.lines().map(|l| l.to_owned()).collect();
                transcripts.push(CatchupTranscript {
                    number: meta.number,
                    timestamp: meta.timestamp.clone(),
                    lines,
                });
            }
        }

        // Read live lines from the current session log.
        let current_transcript = self.next_number.load(Ordering::Relaxed).saturating_sub(1);
        let mut live_lines = Vec::new();
        let mut current_line: u64 = 0;

        let log_path = self.session_log_path.read().await.clone();
        if let Some(ref log_path) = log_path {
            if log_path.exists() {
                let content = tokio::fs::read_to_string(log_path).await.unwrap_or_default();
                let all_lines: Vec<&str> = content.lines().collect();
                current_line = all_lines.len() as u64;
                let skip = since_line as usize;
                if skip < all_lines.len() {
                    live_lines = all_lines[skip..].iter().map(|l| (*l).to_owned()).collect();
                }
            }
        }

        Ok(CatchupResponse { transcripts, live_lines, current_transcript, current_line })
    }
}

impl std::fmt::Debug for TranscriptState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TranscriptState").finish()
    }
}

/// Format a `SystemTime` as a Unix timestamp string (seconds since epoch).
fn unix_timestamp_string(t: SystemTime) -> String {
    t.duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs().to_string()).unwrap_or_default()
}

/// Count lines in a file (blocking I/O, call from spawn_blocking if needed).
fn count_lines(path: &Path) -> std::io::Result<u64> {
    use std::io::BufRead;
    let file = std::fs::File::open(path)?;
    let reader = std::io::BufReader::new(file);
    Ok(reader.lines().count() as u64)
}

#[cfg(test)]
#[path = "transcript_tests.rs"]
mod tests;
