// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use bytes::Bytes;
use tokio::sync::{broadcast, mpsc, RwLock};

use crate::driver::hook_detect::HookDetector;
use crate::driver::hook_recv::HookReceiver;
use crate::driver::stdout_detect::StdoutDetector;
use crate::driver::{AgentState, Detector, HookEvent};
use crate::event::{RawHookEvent, RawMessageEvent};
use crate::usage::UsageState;

use super::parse::{
    extract_codex_message, format_codex_cause, parse_codex_state, CodexUsageTracker,
};

/// Map a Codex hook event to an `(AgentState, cause)` pair.
///
/// - `TurnStart` / `ToolBefore` / `ToolAfter` → `Working`
/// - `TurnEnd` / `SessionEnd` → `Idle`
///
/// Codex has no permission-notification hook (approvals are disabled), and
/// `SessionStart` carries no state.
pub fn map_codex_hook(event: HookEvent) -> Option<(AgentState, String)> {
    match event {
        HookEvent::TurnStart | HookEvent::ToolBefore { .. } | HookEvent::ToolAfter { .. } => {
            Some((AgentState::Working, "hook:working".into()))
        }
        HookEvent::TurnEnd | HookEvent::SessionEnd => Some((AgentState::Idle, "hook:idle".into())),
        HookEvent::Notification { .. } | HookEvent::SessionStart => None,
    }
}

/// Create a Tier 1 hook detector for Codex.
pub fn new_hook_detector(
    receiver: HookReceiver,
    raw_hook_tx: Option<broadcast::Sender<RawHookEvent>>,
    usage: Option<Arc<UsageState>>,
) -> impl Detector {
    HookDetector { receiver, map_event: map_codex_hook, raw_hook_tx, usage }
}

/// Create a Tier 3 detector over Codex's JSON stream.
///
/// Fed by the PTY-less backends, which tee the raw JSON-RPC (app-server) or
/// JSONL (`exec --json`) lines. [`parse_codex_state`] understands both.
pub fn new_stdout_detector(
    stdout_rx: mpsc::Receiver<Bytes>,
    last_message: Option<Arc<RwLock<Option<String>>>>,
    raw_message_tx: Option<broadcast::Sender<RawMessageEvent>>,
    usage: Option<Arc<UsageState>>,
) -> impl Detector {
    let mut tracker = CodexUsageTracker::default();
    StdoutDetector {
        stdout_rx,
        classify: Box::new(|json| {
            let state = parse_codex_state(json)?;
            Some((state, format_codex_cause(json)))
        }),
        extract_message: Some(Box::new(extract_codex_message)),
        last_message,
        raw_message_tx,
        extract_usage: Some(Box::new(move |json| tracker.observe(json))),
        usage,
    }
}

#[cfg(test)]
#[path = "detect_tests.rs"]
mod tests;
