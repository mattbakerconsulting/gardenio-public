// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Codex ChatGPT-subscription credentials on disk (ADR 0004 §5).
//!
//! coopmux distributes a Codex account's credential as the `auth.json` document
//! under [`AUTH_ENV`]. Codex reads that document from
//! `$CODEX_HOME/auth.json` (`cli_auth_credentials_store = "file"`), so coop
//! writes it there, 0600, via write-to-temp plus rename: Codex's guarded
//! reload then adopts a rotated access token mid-session without a restart.
//! The document never goes into the agent's environment.

use std::path::{Path, PathBuf};

/// Credential-map / env key carrying the `auth.json` document.
pub const AUTH_ENV: &str = "CODEX_AUTH_JSON";

/// Credential-map / env key carrying the account's default Codex model.
pub const MODEL_ENV: &str = "CODEX_MODEL";

/// Resolve `$CODEX_HOME`, defaulting to `~/.codex` like Codex itself.
pub fn codex_home() -> PathBuf {
    codex_home_from(|k| std::env::var(k).ok())
}

fn codex_home_from(get_env: impl Fn(&str) -> Option<String>) -> PathBuf {
    if let Some(home) = get_env("CODEX_HOME").filter(|h| !h.is_empty()) {
        return PathBuf::from(home);
    }
    PathBuf::from(get_env("HOME").unwrap_or_default()).join(".codex")
}

/// Atomically write `document` to `<codex_home>/auth.json` with mode 0600.
pub fn write_auth_json(codex_home: &Path, document: &str) -> anyhow::Result<()> {
    use std::io::Write;
    use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};

    std::fs::create_dir_all(codex_home)?;
    let tmp = codex_home.join(format!(".auth.json.{}.tmp", std::process::id()));
    let result = (|| {
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&tmp)?;
        // `mode` is subject to umask and ignored if the temp file already existed.
        f.set_permissions(std::fs::Permissions::from_mode(0o600))?;
        f.write_all(document.as_bytes())?;
        f.sync_all()?;
        std::fs::rename(&tmp, codex_home.join("auth.json"))
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    Ok(result?)
}

/// Write the auth document carried in `credentials`, if any, to the default
/// `$CODEX_HOME`. Returns `true` when a document was present (written or not).
pub fn apply_credentials(credentials: &std::collections::HashMap<String, String>) -> bool {
    let Some(doc) = credentials.get(AUTH_ENV).filter(|d| !d.is_empty()) else {
        return false;
    };
    if let Err(e) = write_auth_json(&codex_home(), doc) {
        tracing::error!("failed to write codex auth.json: {e}");
    }
    true
}

/// Thread id from a Codex rollout path: `rollout-<timestamp>-<uuid>.jsonl`.
///
/// Codex resumes by thread id, which is the rollout file's trailing UUID.
pub fn thread_id_from_rollout(path: &Path) -> Option<String> {
    let stem = path.file_stem()?.to_str()?;
    stem.strip_prefix("rollout-")?;
    // A UUID is 36 chars (8-4-4-4-12).
    let tail = stem.get(stem.len().checked_sub(36)?..)?;
    let groups: Vec<usize> = tail.split('-').map(str::len).collect();
    let hex = tail.chars().all(|c| c == '-' || c.is_ascii_hexdigit());
    (groups == [8, 4, 4, 4, 12] && hex).then(|| tail.to_owned())
}

#[cfg(test)]
#[path = "auth_tests.rs"]
mod tests;
