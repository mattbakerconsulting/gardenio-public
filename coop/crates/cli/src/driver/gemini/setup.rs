// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Pre-spawn preparation for `--agent gemini` sessions.
//!
//! Centralizes settings file writing and FIFO pipe setup. Must run
//! **before** spawning the backend so the child process finds the
//! FIFO and settings on startup.

use std::path::{Path, PathBuf};

use super::hooks::generate_hook_config;
use crate::driver::SessionSetup;

/// Prepare a Gemini session setup.
///
/// Dispatches to the appropriate preparation path based on mode:
/// - **pristine**: no FIFO or hooks, optional settings passthrough.
/// - **fresh**: generates new session ID, hooks, and settings.
pub fn prepare(
    coop_url: &str,
    base_settings: Option<&serde_json::Value>,
    mcp_config: Option<&serde_json::Value>,
    pristine: bool,
) -> anyhow::Result<SessionSetup> {
    if pristine {
        prepare_pristine(coop_url, base_settings, mcp_config)
    } else {
        prepare_fresh(coop_url, base_settings, mcp_config)
    }
}

/// Prepare a fresh Gemini session with hook detection.
fn prepare_fresh(
    coop_url: &str,
    base_settings: Option<&serde_json::Value>,
    mcp_config: Option<&serde_json::Value>,
) -> anyhow::Result<SessionSetup> {
    let session_id = uuid::Uuid::new_v4().to_string();
    let session_dir = crate::driver::coop_session_dir(&session_id)?;
    let hook_pipe_path = session_dir.join("hook.pipe");
    let settings_path =
        write_settings_file(&session_dir, &hook_pipe_path, base_settings, mcp_config)?;

    let (hook_token, hook_token_path) = crate::driver::write_hook_token(&session_dir)?;
    let mut env_vars = crate::driver::hook_env_vars(&hook_pipe_path, coop_url);
    env_vars
        .push(("GEMINI_CLI_SYSTEM_SETTINGS_PATH".to_string(), settings_path.display().to_string()));
    env_vars.push(("COOP_HOOK_TOKEN_FILE".to_string(), hook_token_path.display().to_string()));

    Ok(SessionSetup {
        session_id,
        hook_pipe_path: Some(hook_pipe_path),
        session_log_path: None,
        session_dir,
        env_vars,
        extra_args: vec![],
        hook_token: Some(hook_token),
    })
}

/// Prepare a Gemini session in pristine mode (no FIFO, no hooks).
fn prepare_pristine(
    coop_url: &str,
    base_settings: Option<&serde_json::Value>,
    mcp_config: Option<&serde_json::Value>,
) -> anyhow::Result<SessionSetup> {
    let session_id = uuid::Uuid::new_v4().to_string();
    let session_dir = crate::driver::coop_session_dir(&session_id)?;

    let mut env_vars = vec![("COOP_URL".to_string(), coop_url.to_string())];

    if base_settings.is_some() || mcp_config.is_some() {
        let mut settings = base_settings.cloned().unwrap_or(serde_json::json!({}));
        if let Some(mcp) = mcp_config {
            if let Some(obj) = settings.as_object_mut() {
                obj.insert("mcpServers".to_string(), mcp.clone());
            }
        }
        let path = session_dir.join("coop-gemini-settings.json");
        std::fs::write(&path, serde_json::to_string_pretty(&settings)?)?;
        env_vars.push(("GEMINI_CLI_SYSTEM_SETTINGS_PATH".to_string(), path.display().to_string()));
    }

    Ok(SessionSetup {
        session_id,
        hook_pipe_path: None,
        session_log_path: None,
        session_dir,
        env_vars,
        extra_args: vec![],
        hook_token: None,
    })
}

/// Write a Gemini settings JSON file containing the hook configuration.
fn write_settings_file(
    dir: &Path,
    pipe_path: &Path,
    base_settings: Option<&serde_json::Value>,
    mcp_config: Option<&serde_json::Value>,
) -> anyhow::Result<PathBuf> {
    let coop_config = generate_hook_config(pipe_path);
    let mut merged = match base_settings {
        Some(orch) => crate::config::merge_settings(orch, coop_config),
        None => coop_config,
    };
    if let Some(mcp) = mcp_config {
        if let Some(obj) = merged.as_object_mut() {
            obj.insert("mcpServers".to_string(), mcp.clone());
        }
    }
    let path = dir.join("coop-gemini-settings.json");
    let contents = serde_json::to_string_pretty(&merged)?;
    std::fs::write(&path, contents)?;
    Ok(path)
}

#[cfg(test)]
#[path = "setup_tests.rs"]
mod tests;
