// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Gemini session records: locate, restore and retire (ADR 0005 §6, §11 item 7).
//!
//! A Gemini session lives in one JSONL file,
//! `<gemini-home>/tmp/<slug>/chats/session-<YYYY-MM-DDTHH-MM>-<id8>.jsonl`, where
//! `<slug>` is the workspace directory's basename and `<id8>` the first eight
//! characters of the session id. Its first line is a header carrying
//! `sessionId` and `startTime`. That file is the unit coop uploads to S3 (as
//! `session.jsonl`) and the only thing a restore needs: the phase 0 spike resumed
//! from that one file copied into an otherwise empty HOME
//! (`coop/tests/fixtures/gemini/session/resume-fresh-home.*`).
//!
//! Records are re-appended by `id` as they grow (a tool-call record appears
//! twice); the CLI dedupes on load, last wins, so coop uploads the file as is.

use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};

use serde_json::Value;

/// `<gemini-home>`: `$GEMINI_CLI_HOME/.gemini` when set, else `~/.gemini`.
pub fn gemini_home() -> PathBuf {
    let base = std::env::var_os("GEMINI_CLI_HOME")
        .filter(|h| !h.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(std::env::var("HOME").unwrap_or_else(|_| "/tmp".into())));
    base.join(".gemini")
}

/// What a session file's header says about its session.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionMeta {
    /// Session id; a valid UUID.
    pub session_id: String,
    /// `YYYY-MM-DDTHH-MM`, the start minute as used in the file name.
    pub started: String,
}

/// Parse the header line of the session file at `path`.
///
/// Returns `None` when the file is unreadable, empty, or its first line has no
/// UUID `sessionId` and RFC 3339 `startTime`. The values end up in a filesystem
/// path, so anything else is rejected rather than sanitised.
pub fn parse_session_meta(path: &Path) -> Option<SessionMeta> {
    let file = std::fs::File::open(path).ok()?;
    let mut first = String::new();
    BufReader::new(file).read_line(&mut first).ok()?;
    let header: Value = serde_json::from_str(first.trim()).ok()?;
    let id = header.get("sessionId")?.as_str()?;
    let session_id = uuid::Uuid::parse_str(id).ok()?.hyphenated().to_string();
    let start = header.get("startTime")?.as_str()?;
    let started = start.get(..16)?.replace(':', "-");
    valid_minute(&started).then_some(SessionMeta { session_id, started })
}

/// `YYYY-MM-DDTHH-MM`, digits and separators only.
fn valid_minute(s: &str) -> bool {
    s.len() == 16
        && s.bytes().enumerate().all(|(i, b)| match i {
            4 | 7 | 13 => b == b'-',
            10 => b == b'T',
            _ => b.is_ascii_digit(),
        })
}

/// The project directory name Gemini files sessions under for `cwd`.
pub fn project_slug(cwd: &Path) -> String {
    cwd.file_name().and_then(|n| n.to_str()).filter(|n| !n.is_empty()).unwrap_or("root").to_owned()
}

fn id8(session_id: &str) -> &str {
    session_id.get(..8).unwrap_or(session_id)
}

/// Where Gemini keeps the session record for `meta` under `home`, for project `slug`.
pub fn session_path(home: &Path, slug: &str, meta: &SessionMeta) -> PathBuf {
    home.join("tmp").join(slug).join("chats").join(format!(
        "session-{}-{}.jsonl",
        meta.started,
        id8(&meta.session_id)
    ))
}

/// Find the live session record for `session_id` under `home`, if it exists yet.
pub fn find_session(home: &Path, session_id: &str) -> Option<PathBuf> {
    let suffix = format!("-{}.jsonl", id8(session_id));
    let projects = std::fs::read_dir(home.join("tmp")).ok()?;
    for project in projects.flatten() {
        let Ok(chats) = std::fs::read_dir(project.path().join("chats")) else { continue };
        for entry in chats.flatten() {
            let path = entry.path();
            let named = path
                .file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|n| n.starts_with("session-") && n.ends_with(&suffix));
            // The id8 suffix can collide; the header is the ground truth.
            if named && parse_session_meta(&path).is_some_and(|m| m.session_id == session_id) {
                return Some(path);
            }
        }
    }
    None
}

/// Copy a restored session record (`src`, S3's `session.jsonl`) to where Gemini
/// expects it under `home`, returning the session id to resume.
///
/// An existing local record for the same session is kept when it is at least as
/// large as the restored copy: it is the same append-only file, so the larger
/// one is the fresher. Returns `None` (and writes nothing) when `src` is not a
/// session record.
pub fn place_restored_session(
    src: &Path,
    home: &Path,
    slug: &str,
) -> std::io::Result<Option<String>> {
    let Some(meta) = parse_session_meta(src) else { return Ok(None) };
    let src_len = std::fs::metadata(src)?.len();
    let existing = find_session(home, &meta.session_id);
    if let Some(local) = existing.as_ref().and_then(|p| std::fs::metadata(p).ok()) {
        if local.len() >= src_len {
            return Ok(Some(meta.session_id));
        }
    }
    let dest = existing.unwrap_or_else(|| session_path(home, slug, &meta));
    if let Some(parent) = dest.parent() {
        std::fs::create_dir_all(parent)?;
    }
    std::fs::copy(src, &dest)?;
    Ok(Some(meta.session_id))
}

/// Read the session id out of a session record, for a respawn on a new account.
pub fn session_id_from_record(path: &Path) -> Option<String> {
    parse_session_meta(path).map(|m| m.session_id)
}

/// Retire a session record so the next start does not resume it: rename
/// `x.jsonl` to `x.jsonl.stale`. Gemini lists sessions by the `.jsonl` suffix, so
/// the renamed file is invisible to it. Idempotent; a missing file is not an
/// error. This is the Gemini counterpart of [`crate::driver::codex::rollout::retire_rollout`].
pub fn retire_session(path: &Path) -> Option<PathBuf> {
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
#[path = "session_tests.rs"]
mod tests;
