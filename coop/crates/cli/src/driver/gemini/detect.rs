// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use bytes::Bytes;
use tokio::sync::{broadcast, mpsc, RwLock};

use crate::driver::hook_detect::HookDetector;
use crate::driver::hook_recv::HookReceiver;
use crate::driver::HookEvent;
use crate::driver::{AgentState, Detector, PromptContext, PromptKind};
use crate::event::{RawHookEvent, RawMessageEvent};
use crate::usage::UsageState;

use super::parse::{
    extract_gemini_message, format_gemini_cause, parse_gemini_state, GeminiUsageTracker,
};

/// Map a Gemini hook event to an `(AgentState, cause)` pair.
///
/// - `TurnStart` / `ToolBefore` / `ToolAfter` → `Working`
/// - `TurnEnd` / `SessionEnd` → `Idle`
/// - `Notification("ToolPermission")` → `Prompt(Permission)`
///
/// Returns `None` for events that should be ignored (e.g. `SessionStart`, unrecognised notifications).
pub fn map_gemini_hook(event: HookEvent) -> Option<(AgentState, String)> {
    match event {
        HookEvent::TurnStart | HookEvent::ToolAfter { .. } => {
            Some((AgentState::Working, "hook:working".into()))
        }
        HookEvent::ToolBefore { .. } => {
            // BeforeTool fires for every tool call (including
            // auto-approved ones). Map to Working; actual
            // permission prompts are detected via Notification.
            Some((AgentState::Working, "hook:working".into()))
        }
        HookEvent::TurnEnd | HookEvent::SessionEnd => Some((AgentState::Idle, "hook:idle".into())),
        HookEvent::Notification { notification_type } => match notification_type.as_str() {
            "ToolPermission" => Some((
                AgentState::Prompt {
                    prompt: PromptContext::new(PromptKind::Permission).with_subtype("tool"),
                },
                "hook:prompt(permission)".into(),
            )),
            _ => None,
        },
        HookEvent::SessionStart => None,
    }
}

/// Create a Tier 1 hook detector for Gemini.
pub fn new_hook_detector(
    receiver: HookReceiver,
    raw_hook_tx: Option<broadcast::Sender<RawHookEvent>>,
    usage: Option<Arc<UsageState>>,
) -> impl Detector {
    HookDetector { receiver, map_event: map_gemini_hook, raw_hook_tx, usage }
}

/// Create a Tier 3 detector over Gemini's JSON stream.
///
/// Fed by the ACP backend, which tees the raw JSON-RPC lines plus its own
/// synthetic notifications; also understands `--output-format stream-json`
/// JSONL. [`parse_gemini_state`] tells the two apart and classifies each entry;
/// the last assistant message and per-turn usage come from the same stream.
pub fn new_stdout_detector(
    stdout_rx: mpsc::Receiver<Bytes>,
    last_message: Option<Arc<RwLock<Option<String>>>>,
    raw_message_tx: Option<broadcast::Sender<RawMessageEvent>>,
    usage: Option<Arc<UsageState>>,
) -> impl Detector {
    use crate::driver::stdout_detect::StdoutDetector;
    let mut tracker = GeminiUsageTracker;
    StdoutDetector {
        stdout_rx,
        classify: Box::new(|json| {
            let state = parse_gemini_state(json)?;
            let cause = format_gemini_cause(json);
            Some((state, cause))
        }),
        extract_message: Some(Box::new(extract_gemini_message)),
        last_message,
        raw_message_tx,
        extract_usage: Some(Box::new(move |json| tracker.observe(json))),
        usage,
    }
}

#[cfg(test)]
#[path = "detect_tests.rs"]
mod tests;
