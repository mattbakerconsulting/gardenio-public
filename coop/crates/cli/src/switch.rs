// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Session switch types and shared state.
//!
//! A switch restarts the child process with new credentials (env vars) and
//! `--resume`, preserving all transport connections. The orchestrator sends
//! a switch request via HTTP/WS/gRPC; the session loop drains the current
//! backend and signals `SessionOutcome::Switch`; the `PreparedSession` run
//! loop respawns the child.

use std::collections::HashMap;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};
use tokio::sync::{mpsc, RwLock};

/// External switch request from the transport layer.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SwitchRequest {
    /// Environment variables to merge into the new child process.
    /// Typically contains credential keys like `CLAUDE_CODE_OAUTH_TOKEN`.
    #[serde(default)]
    pub credentials: Option<HashMap<String, String>>,
    /// Skip waiting for idle — SIGHUP immediately.
    #[serde(default)]
    pub force: bool,
    /// Named profile for active tracking (set by auto-rotation or manual switch).
    #[serde(default)]
    pub profile: Option<String>,
}

/// Shared state for the switch subsystem.
///
/// Lives on `Store` so transport handlers can send switch requests.
/// The session loop owns the `Receiver` end of the channel.
pub struct SwitchState {
    /// Capacity-1 channel enforces single-switch-at-a-time.
    /// `try_send` returning `Full` → 409 SwitchInProgress.
    pub switch_tx: mpsc::Sender<SwitchRequest>,
    /// Path to the session log (JSONL) — used to derive the conversation
    /// ID for `--resume`. Updated after each switch.
    pub session_log_path: RwLock<Option<PathBuf>>,
    /// Base settings JSON from the agent config file.
    pub base_settings: Option<serde_json::Value>,
    /// MCP config JSON from the agent config file.
    pub mcp_config: Option<serde_json::Value>,
}

impl std::fmt::Debug for SwitchState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SwitchState").finish()
    }
}

#[cfg(test)]
#[path = "switch_tests.rs"]
mod tests;
