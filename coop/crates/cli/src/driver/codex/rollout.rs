// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Codex rollout files: locate, restore and retire (ADR 0004 §9).
//!
//! A Codex session lives in one file,
//! `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<session-id>.jsonl`, whose
//! first record is `session_meta` carrying the session (thread) id. That file is
//! the unit coop uploads to S3 (as `session.jsonl`) and the only thing a
//! restore needs: Codex resumes by thread id and finds the rollout by the id
//! suffix of its filename, so the sqlite state under `$CODEX_HOME` is not
//! required.

use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};

use serde_json::Value;

/// `$CODEX_HOME`, falling back to `~/.codex` as Codex itself does.
pub fn codex_home() -> PathBuf {
    if let Some(home) = std::env::var_os("CODEX_HOME").filter(|h| !h.is_empty()) {
        return PathBuf::from(home);
    }
    let home = std::env::var("HOME").unwrap_or_else(|_| "/tmp".into());
    PathBuf::from(home).join(".codex")
}

/// What a rollout's `session_meta` record says about its session.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RolloutMeta {
    /// Session (thread) id; a valid UUID.
    pub thread_id: String,
    /// `YYYY-MM-DDTHH:MM:SS` prefix of the session start timestamp.
    pub started: String,
}

/// Parse the leading `session_meta` record of the rollout at `path`.
///
/// Returns `None` when the file is unreadable, empty, or its first record is not
/// a `session_meta` with a UUID id and an RFC 3339 timestamp. The values end up
/// in a filesystem path, so anything else is rejected rather than sanitised.
pub fn parse_rollout_meta(path: &Path) -> Option<RolloutMeta> {
    let file = std::fs::File::open(path).ok()?;
    let mut first = String::new();
    BufReader::new(file).read_line(&mut first).ok()?;
    let record: Value = serde_json::from_str(first.trim()).ok()?;
    if record.get("type").and_then(Value::as_str) != Some("session_meta") {
        return None;
    }
    let payload = record.get("payload")?;
    let id = payload.get("id").or_else(|| payload.get("session_id")).and_then(Value::as_str)?;
    let thread_id = uuid::Uuid::parse_str(id).ok()?.hyphenated().to_string();
    let ts =
        payload.get("timestamp").or_else(|| record.get("timestamp")).and_then(Value::as_str)?;
    let started = ts.get(..19)?;
    valid_timestamp(started).then(|| RolloutMeta { thread_id, started: started.to_owned() })
}

/// `YYYY-MM-DDTHH:MM:SS`, digits and separators only.
fn valid_timestamp(s: &str) -> bool {
    s.len() == 19
        && s.bytes().enumerate().all(|(i, b)| match i {
            4 | 7 => b == b'-',
            10 => b == b'T',
            13 | 16 => b == b':',
            _ => b.is_ascii_digit(),
        })
}

/// Where Codex keeps the rollout for `meta` under `codex_home`.
pub fn rollout_path(codex_home: &Path, meta: &RolloutMeta) -> PathBuf {
    let date = &meta.started[..10];
    let mut dir = codex_home.join("sessions");
    dir.extend(date.split('-'));
    dir.join(format!("rollout-{}-{}.jsonl", meta.started.replace(':', "-"), meta.thread_id))
}

/// Find the live rollout for `thread_id` under `codex_home`, if it exists yet.
pub fn find_rollout(codex_home: &Path, thread_id: &str) -> Option<PathBuf> {
    let suffix = format!("-{thread_id}.jsonl");
    let mut stack = vec![codex_home.join("sessions")];
    while let Some(dir) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&dir) else { continue };
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                stack.push(path);
            } else if path
                .file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|n| n.starts_with("rollout-") && n.ends_with(&suffix))
            {
                return Some(path);
            }
        }
    }
    None
}

/// Copy a restored rollout (`src`, S3's `session.jsonl`) to where Codex expects
/// it under `codex_home`, returning the thread id to resume.
///
/// An existing local rollout for the same thread is kept when it is at least as
/// large as the restored copy: it is the same append-only file, so the larger
/// one is the fresher. Returns `None` (and writes nothing) when `src` is not a
/// rollout.
pub fn place_restored_rollout(src: &Path, codex_home: &Path) -> std::io::Result<Option<String>> {
    let Some(meta) = parse_rollout_meta(src) else { return Ok(None) };
    let src_len = std::fs::metadata(src)?.len();
    let existing = find_rollout(codex_home, &meta.thread_id);
    if let Some(local) = existing.as_ref().and_then(|p| std::fs::metadata(p).ok()) {
        if local.len() >= src_len {
            return Ok(Some(meta.thread_id));
        }
    }
    let dest = existing.unwrap_or_else(|| rollout_path(codex_home, &meta));
    if let Some(parent) = dest.parent() {
        std::fs::create_dir_all(parent)?;
    }
    std::fs::copy(src, &dest)?;
    Ok(Some(meta.thread_id))
}

/// Retire a rollout so the next start does not resume it: rename `x.jsonl` to
/// `x.jsonl.stale`. Codex resolves a thread id by the `-<id>.jsonl` filename
/// suffix, so the renamed file is invisible to it. Idempotent; a missing file is
/// not an error. This is the Codex counterpart of
/// [`crate::driver::claude::retire`], minus the companion directories, which
/// Codex does not have.
pub fn retire_rollout(path: &Path) -> Option<PathBuf> {
    if !path.exists() {
        return None;
    }
    let mut stale = path.as_os_str().to_owned();
    stale.push(".stale");
    let stale = PathBuf::from(stale);
    match std::fs::rename(path, &stale) {
        Ok(()) => Some(stale),
        Err(e) => {
            tracing::warn!("retire: rename {} failed: {e}", path.display());
            None
        }
    }
}

#[cfg(test)]
#[path = "rollout_tests.rs"]
mod tests;
