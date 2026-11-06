// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Session resume support for Claude Code.
//!
//! When a coop process restarts, it can reconnect to a previous Claude
//! session by discovering the session log and passing `--resume` with
//! the session ID (derived from the log file stem).

use std::collections::HashMap;
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};

use tracing::info;

use crate::driver::AgentState;

use super::setup::{claude_config_dir, project_dir_name};

/// Recovered state from a previous session log.
#[derive(Debug, Clone)]
pub struct ResumeState {
    /// Last known agent state from the log.
    pub last_state: AgentState,
    /// Byte offset to resume log detection from.
    pub log_offset: u64,
}

/// Returns `true` if the session has persisted state that `--resume` can use.
///
/// Claude Code creates the companion directory lazily — only when subagents
/// run or tool results are cached.  A missing companion dir does NOT mean the
/// session is unresumable; `--resume` works from the `.jsonl` transcript alone.
///
/// We check for a non-empty `.jsonl` file (at least a few hundred bytes of
/// conversation) as the real indicator of a resumable session.
fn session_is_resumable(jsonl_path: &Path) -> bool {
    jsonl_path.metadata().map(|m| m.len() >= MIN_RESUMABLE_BYTES).unwrap_or(false)
}

/// A transcript under this many bytes is likely just the init message with no
/// real conversation to resume.
const MIN_RESUMABLE_BYTES: u64 = 256;

/// Find the most recent Claude session log for a workspace.
///
/// Scans `<config-dir>/projects/<workspace-hash>/` for the latest `.jsonl`
/// file, where `<config-dir>` is `$CLAUDE_CONFIG_DIR` (or `$HOME/.claude`).
///
/// The `workspace_hint` accepts:
/// - A direct path to a `.jsonl` log file (returned as-is).
/// - A workspace path (e.g. `/Users/me/myapp`) — matched against project
///   directory names.
/// - A partial directory name (e.g. `-Users-me-myapp`) — substring-matched
///   against project directory names.
pub fn discover_session_log(workspace_hint: &str) -> anyhow::Result<Option<PathBuf>> {
    // Try the hint directly as a path first.
    let direct = Path::new(workspace_hint);
    if direct.is_file() && matches!(direct.extension().and_then(|e| e.to_str()), Some("jsonl")) {
        if !session_is_resumable(direct) {
            info!("session log {} is too small to resume, starting fresh", direct.display());
            return Ok(None);
        }
        return Ok(Some(direct.to_path_buf()));
    }

    // Scan the Claude projects directory for session logs.
    let projects_dir = claude_config_dir().join("projects");
    let normalized_hint = project_dir_name(Path::new(workspace_hint));

    // Find the most recent resumable .jsonl file across all candidate directories.
    let mut best: Option<(PathBuf, std::time::SystemTime)> = None;
    for (path, metadata) in session_logs(&projects_dir, &normalized_hint) {
        if metadata.len() < MIN_RESUMABLE_BYTES {
            continue;
        }
        if let Ok(modified) = metadata.modified() {
            if best.as_ref().is_none_or(|(_, prev)| modified > *prev) {
                best = Some((path, modified));
            }
        }
    }

    Ok(best.map(|(path, _)| path))
}

/// Every `.jsonl` in the project directories matching `normalized_hint`,
/// with its metadata.
///
/// A project directory matches when its name contains the hint or the hint
/// contains its name. Unreadable directories and entries are skipped.
fn session_logs(projects_dir: &Path, normalized_hint: &str) -> Vec<(PathBuf, std::fs::Metadata)> {
    let mut logs = Vec::new();
    let Ok(entries) = std::fs::read_dir(projects_dir) else {
        return logs;
    };
    for dir in entries.flatten().map(|e| e.path()) {
        if !dir.is_dir() {
            continue;
        }
        let dir_name = dir.file_name().unwrap_or_default().to_string_lossy().to_string();
        if !(dir_name.contains(normalized_hint) || normalized_hint.contains(&dir_name)) {
            continue;
        }
        logs.extend(logs_in(&dir));
    }
    logs
}

/// Every `.jsonl` directly in `dir`, with its metadata. An unreadable or
/// missing `dir` has none; unreadable entries are skipped.
fn logs_in(dir: &Path) -> Vec<(PathBuf, std::fs::Metadata)> {
    let mut logs = Vec::new();
    let Ok(files) = std::fs::read_dir(dir) else {
        return logs;
    };
    for entry in files.flatten() {
        let path = entry.path();
        if path.extension().and_then(|e| e.to_str()) != Some("jsonl") {
            continue;
        }
        if let Ok(metadata) = entry.metadata() {
            logs.push((path, metadata));
        }
    }
    logs
}

/// The session logs already on disk before this incarnation's Claude started.
///
/// Tier 2 discovery must only latch a log written by the Claude process this
/// coop spawned. Session logs survive restarts when gb skips its clean for an
/// intended S3 resume, and Claude creates its `.jsonl` lazily, on the first
/// message. So right after a restart the newest `.jsonl` on disk is the
/// previous incarnation's, and replaying it from offset 0 turns its last
/// state (often an API error, or a turn cut short by SIGHUP) into this
/// session's state (gd-2g0LXWF4Sz).
///
/// Capture the baseline before spawning Claude; [`find_new_log`] then ignores
/// everything it recorded unless it grew.
///
/// [`find_new_log`]: SessionLogBaseline::find_new_log
#[derive(Debug, Clone)]
pub struct SessionLogBaseline {
    /// Claude's project dir for exactly this working dir. Not the substring
    /// match [`discover_session_log`] uses: that also matches an ancestor's
    /// dir (`-home-me` for `/home/me/ws/wt`), whose sessions on a laptop
    /// include the owner's own live Claude (gd-4tAWXFTMVL).
    project_dir: PathBuf,
    /// Size of each log at capture time.
    sizes: HashMap<PathBuf, u64>,
}

impl SessionLogBaseline {
    /// Record the session logs on disk now for `working_dir`'s project.
    pub fn capture(working_dir: &Path) -> Self {
        Self::capture_in(claude_config_dir().join("projects"), working_dir)
    }

    /// [`capture`](Self::capture) against an explicit projects directory.
    pub fn capture_in(projects_dir: PathBuf, working_dir: &Path) -> Self {
        let project_dir = projects_dir.join(project_dir_name(working_dir));
        let sizes = logs_in(&project_dir)
            .into_iter()
            .map(|(path, metadata)| (path, metadata.len()))
            .collect();
        Self { project_dir, sizes }
    }

    /// Number of logs recorded at capture time.
    pub fn len(&self) -> usize {
        self.sizes.len()
    }

    /// Whether no logs were on disk at capture time.
    pub fn is_empty(&self) -> bool {
        self.sizes.is_empty()
    }

    /// The byte offset to start reading `path` from: its size at capture, or
    /// 0 if it was not on disk then.
    pub fn offset_of(&self, path: &Path) -> u64 {
        self.sizes.get(path).copied().unwrap_or(0)
    }

    /// The newest session log written since capture, and the byte offset to
    /// start reading it from.
    ///
    /// A log absent from the baseline is new: it qualifies once it holds at
    /// least [`MIN_RESUMABLE_BYTES`], and is read from offset 0. A log in the
    /// baseline qualifies only if it has grown since, and is read from its
    /// captured size, so its history never replays as current state.
    pub fn find_new_log(&self) -> Option<(PathBuf, u64)> {
        let mut best: Option<(PathBuf, u64, std::time::SystemTime)> = None;
        for (path, metadata) in logs_in(&self.project_dir) {
            let len = metadata.len();
            let offset = match self.sizes.get(&path) {
                Some(&before) if len > before => before,
                Some(_) => continue,
                None if len >= MIN_RESUMABLE_BYTES => 0,
                None => continue,
            };
            let Ok(modified) = metadata.modified() else {
                continue;
            };
            if best.as_ref().is_none_or(|(_, _, prev)| modified > *prev) {
                best = Some((path, offset, modified));
            }
        }
        best.map(|(path, offset, _)| (path, offset))
    }
}

/// Parse a session log to determine the last known agent state and resume offset.
///
/// Reads the log file and processes each JSONL entry to build the resume state.
pub fn parse_resume_state(log_path: &Path) -> anyhow::Result<ResumeState> {
    let file = std::fs::File::open(log_path)?;
    let metadata = file.metadata()?;
    let file_size = metadata.len();

    let reader = BufReader::new(file);
    let mut last_state = AgentState::Starting;

    for line in reader.lines() {
        let line = line?;
        if line.trim().is_empty() {
            continue;
        }

        if let Ok(json) = serde_json::from_str::<serde_json::Value>(&line) {
            if let Some(state) = super::parse::parse_claude_state(&json) {
                last_state = state;
            }
        }
    }

    Ok(ResumeState { last_state, log_offset: file_size })
}

/// Build additional CLI arguments for resuming a Claude session.
pub fn resume_args(session_id: &str) -> Vec<String> {
    vec!["--resume".to_owned(), session_id.to_owned()]
}

/// Parse the Claude session UUID from the JSONL transcript content.
///
/// Each Claude transcript entry carries a `sessionId` field; we read it out of
/// the first entry that has one. This is the **ground truth** for the session
/// UUID — it works for both local resumes (`<uuid>.jsonl`) and S3-restored logs
/// (always named `session.jsonl`) without relying on the file name.
///
/// Replaces the legacy file-stem-based "UUID prediction" path, which
/// historically wrote the literal string `"session"` as `last_session_id` for
/// any S3-restored log (see ka-0000005939 / ka-0000003077).
///
/// Returns `None` if the file cannot be read, contains no JSONL entries with a
/// non-empty `sessionId`, or only carries the `"session"` sentinel.
pub fn parse_session_id(log_path: &Path) -> Option<String> {
    let file = std::fs::File::open(log_path).ok()?;
    let reader = BufReader::new(file);
    for line in reader.lines().map_while(Result::ok) {
        if line.trim().is_empty() {
            continue;
        }
        let Ok(json) = serde_json::from_str::<serde_json::Value>(&line) else {
            continue;
        };
        let Some(sid) = json.get("sessionId").and_then(|v| v.as_str()) else {
            continue;
        };
        let sid = sid.trim();
        if sid.is_empty() || sid == "session" {
            continue;
        }
        return Some(sid.to_owned());
    }
    None
}

#[cfg(test)]
#[path = "resume_tests.rs"]
mod tests;
