// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Session retirement: rename a Claude session log (and its companion state)
//! so that future `--resume` discoveries skip it.
//!
//! Mirrors `retireStaleSession` in `gardenio/controller/cmd/gb/agent_k8s_lifecycle.go`.
//! The gb restart loop calls this (via `POST /api/v1/session/retire`) when a
//! coop session exited fast — non-zero or under the minimum-runtime threshold —
//! to prevent the next restart from resuming the same broken transcript.
//!
//! Retirement renames three things, each idempotent:
//!   - `<config>/projects/<project>/<uuid>.jsonl`         → `…<uuid>.jsonl.stale`
//!   - `<config>/projects/<project>/<uuid>/`              → `…<uuid>.stale`     (companion dir)
//!   - `<config>/session-env/<uuid>/`                     → `…<uuid>.stale`     (v2.1+ env state)

use std::path::{Path, PathBuf};

use super::setup::claude_config_dir;

/// Outcome of a retire call. Each path is set when its rename succeeded.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct RetireOutcome {
    pub log_retired: Option<PathBuf>,
    pub companion_retired: Option<PathBuf>,
    pub session_env_retired: Option<PathBuf>,
}

impl RetireOutcome {
    pub fn any(&self) -> bool {
        self.log_retired.is_some()
            || self.companion_retired.is_some()
            || self.session_env_retired.is_some()
    }
}

/// Retire `log_path` and its companion state.
///
/// `log_path` must be the `.jsonl` of a Claude session (not already `.stale`).
/// All renames are best-effort: a missing path is not an error, only a failed
/// rename of an existing path is. The function logs but does not propagate
/// such failures, returning whatever subset succeeded.
pub fn retire_session_log(log_path: &Path) -> RetireOutcome {
    let mut out = RetireOutcome::default();

    if log_path.as_os_str().is_empty() {
        return out;
    }

    // 1) Rename the .jsonl itself.
    if log_path.exists() {
        let stale = with_stale_suffix(log_path);
        match std::fs::rename(log_path, &stale) {
            Ok(()) => out.log_retired = Some(stale),
            Err(e) => {
                tracing::warn!("retire: rename {} failed: {}", log_path.display(), e);
            }
        }
    }

    // 2) Rename the companion directory: <log without .jsonl>/.
    let companion = strip_jsonl(log_path);
    if let Some(c) = companion.as_ref() {
        if c.is_dir() {
            let stale = append_stale(c);
            match std::fs::rename(c, &stale) {
                Ok(()) => out.companion_retired = Some(stale),
                Err(e) => {
                    tracing::warn!("retire: rename companion {} failed: {}", c.display(), e);
                }
            }
        }
    }

    // 3) Rename the session-env directory.
    if let Some(uuid) = session_uuid(log_path) {
        let env_dir = claude_config_dir().join("session-env").join(&uuid);
        if env_dir.is_dir() {
            let stale = append_stale(&env_dir);
            match std::fs::rename(&env_dir, &stale) {
                Ok(()) => out.session_env_retired = Some(stale),
                Err(e) => {
                    tracing::warn!(
                        "retire: rename session-env {} failed: {}",
                        env_dir.display(),
                        e
                    );
                }
            }
        }
    }

    out
}

fn with_stale_suffix(p: &Path) -> PathBuf {
    let mut s = p.as_os_str().to_owned();
    s.push(".stale");
    PathBuf::from(s)
}

fn append_stale(p: &Path) -> PathBuf {
    let mut s = p.as_os_str().to_owned();
    s.push(".stale");
    PathBuf::from(s)
}

fn strip_jsonl(p: &Path) -> Option<PathBuf> {
    let s = p.to_str()?;
    s.strip_suffix(".jsonl").map(PathBuf::from)
}

fn session_uuid(p: &Path) -> Option<String> {
    let stem = p.file_stem()?.to_str()?;
    Some(stem.to_owned())
}

#[cfg(test)]
#[path = "retire_tests.rs"]
mod tests;
