// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::HashSet;
use std::future::Future;
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tokio::sync::{broadcast, mpsc, RwLock};
use tokio_util::sync::CancellationToken;

use crate::driver::hook_detect::HookDetector;
use crate::driver::hook_recv::HookReceiver;
use crate::driver::log_watch::LogWatcher;
use crate::driver::HookEvent;
use crate::driver::{AgentState, Detector, DetectorEmission, PromptContext, PromptKind};
use crate::event::{RawHookEvent, RawMessageEvent};
use crate::usage::UsageState;

use super::parse::{
    extract_assistant_text, extract_tool_label, format_claude_cause, parse_claude_state,
};
use super::prompt::extract_ask_user_from_tool_input;
use super::resume;

/// Build a hook cause string for a tool event, including a label when available.
fn hook_tool_cause(tool: &str, tool_input: Option<&serde_json::Value>) -> String {
    match extract_tool_label(tool, tool_input) {
        Some(label) => format!("hook:tool({tool}: {label})"),
        None => format!("hook:tool({tool})"),
    }
}

/// Map a Claude hook event to an `(AgentState, cause)` pair.
///
/// - `TurnStart` → `Working` (agent begins processing)
/// - `TurnEnd` / `SessionEnd` → `Idle`
/// - `ToolAfter` → `Working` (confirms agent is executing tools mid-turn)
/// - `ToolBefore` → `Working` with rich cause (tool name + input label)
/// - `Notification(idle_prompt)` → `Idle`
/// - `Notification(permission_prompt)` → `Prompt(Permission)`
/// - `ToolBefore(AskUserQuestion)` → `Prompt(Question)` with context
/// - `ToolBefore(ExitPlanMode)` → `Prompt(Plan)`
/// - `ToolBefore(EnterPlanMode)` → `Working`
///
/// Note: `PostToolUse` hooks only fire for real agent tool calls — command mode
/// (user running bash commands via the `>` prompt) does not trigger hooks.
/// Command mode instead writes `<bash-input>`/`<bash-stdout>` user messages
/// to the JSONL log, which are filtered in `parse_claude_state()`.
///
/// Returns `None` for events that should be ignored (e.g. `SessionStart`, unrecognised notifications).
pub fn map_claude_hook(event: HookEvent) -> Option<(AgentState, String)> {
    match event {
        HookEvent::TurnEnd | HookEvent::SessionEnd => {
            Some((AgentState::Idle, "hook:idle".to_owned()))
        }
        HookEvent::ToolAfter { ref tool, ref tool_input, .. } => {
            Some((AgentState::Working, hook_tool_cause(tool, tool_input.as_ref())))
        }
        HookEvent::Notification { notification_type } => match notification_type.as_str() {
            "idle_prompt" => Some((AgentState::Idle, "hook:idle".into())),
            "permission_prompt" => Some((
                AgentState::Prompt { prompt: PromptContext::new(PromptKind::Permission) },
                "hook:prompt(permission)".into(),
            )),
            _ => None,
        },
        HookEvent::ToolBefore { ref tool, ref tool_input, .. } => match tool.as_str() {
            "AskUserQuestion" => Some((
                AgentState::Prompt {
                    prompt: extract_ask_user_from_tool_input(tool_input.as_ref()),
                },
                "hook:prompt(question)".into(),
            )),
            "ExitPlanMode" => {
                let mut ctx = PromptContext::new(PromptKind::Plan);
                if let Some(input) = tool_input {
                    if let Ok(s) = serde_json::to_string(input) {
                        ctx = ctx.with_input(s);
                    }
                }
                ctx = ctx.with_ready();
                Some((AgentState::Prompt { prompt: ctx }, "hook:prompt(plan)".into()))
            }
            "EnterPlanMode" => Some((AgentState::Working, "hook:working".into())),
            _ => {
                let cause = hook_tool_cause(tool, tool_input.as_ref());
                Some((AgentState::Working, cause))
            }
        },
        HookEvent::TurnStart => Some((AgentState::Working, "hook:working".into())),
        HookEvent::SessionStart => None,
    }
}

/// Create a Tier 1 hook detector for Claude.
pub fn new_hook_detector(
    receiver: HookReceiver,
    raw_hook_tx: Option<broadcast::Sender<RawHookEvent>>,
    usage: Option<Arc<UsageState>>,
) -> impl Detector {
    HookDetector { receiver, map_event: map_claude_hook, raw_hook_tx, usage }
}

/// Tier 2 detector: watches Claude's session log file for new JSONL entries.
///
/// Parses each new line with `parse_claude_state` and emits the resulting
/// state to the composite detector.
///
/// When `log_path` does not exist at startup (typical for fresh sessions where
/// the stored path uses a coop-generated UUID rather than Claude's actual UUID),
/// the detector binds to the `transcript_path` this Claude's own hooks report
/// on `hook_rx`. Only without hooks does it poll `log_baseline` for the log
/// this Claude writes.
///
/// Once bound it keeps reading `hook_rx`: when a later hook names a different
/// transcript (`/clear` and `/resume` start a new session mid-run), it swaps
/// its `LogWatcher` to that transcript (gd-iJuducXXED).
pub struct LogDetector {
    pub log_path: PathBuf,
    /// Byte offset to start reading from (used for session resume).
    pub start_offset: u64,
    /// Fallback poll interval for the log watcher.
    pub poll_interval: Duration,
    /// Shared last assistant message text (written directly, bypasses detector pipeline).
    pub last_message: Option<Arc<RwLock<Option<String>>>>,
    /// Optional sender for raw message JSON broadcast.
    pub raw_message_tx: Option<broadcast::Sender<RawMessageEvent>>,
    /// Optional usage tracking state.
    pub usage: Option<Arc<UsageState>>,
    /// Session logs on disk before Claude was spawned, used to discover the
    /// real log when `log_path` doesn't exist (fresh sessions, where coop's
    /// predicted UUID differs from Claude's actual session UUID). Discovery
    /// only accepts a log this Claude wrote (gd-2g0LXWF4Sz).
    pub log_baseline: Option<resume::SessionLogBaseline>,
    /// Raw hook events from the Claude process this coop spawned, subscribed
    /// before the spawn. Every Claude hook payload carries the session's
    /// `transcript_path`, so when hooks are wired discovery binds to that path
    /// and never scans `log_baseline`: a scan can latch any other session
    /// that writes to a matching project dir meanwhile, such as the owner's
    /// own Claude on a laptop (gd-4tAWXFTMVL).
    pub hook_rx: Option<broadcast::Receiver<RawHookEvent>>,
    /// Shared slot written when the real session log is discovered.
    ///
    /// Allows subsystems (transcript snapshots, S3 uploads) to learn the
    /// actual path after Claude starts writing with its own UUID.
    pub resolved_log_path: Option<Arc<RwLock<Option<PathBuf>>>>,
}

impl Detector for LogDetector {
    fn run(
        self: Box<Self>,
        state_tx: mpsc::Sender<DetectorEmission>,
        shutdown: CancellationToken,
    ) -> Pin<Box<dyn Future<Output = ()> + Send>> {
        Box::pin(async move {
            // If the configured path doesn't exist, Claude may have written its
            // session log at a different UUID (fresh sessions: coop-predicted UUID
            // != Claude's actual UUID). Wait for this Claude's hooks to name its
            // transcript or, without hooks, poll the baseline until the log this
            // Claude writes appears. A log left by an earlier incarnation is
            // never a match, and one that grows is read from its pre-spawn
            // size, so its history can't become current state.
            let mut hook_rx = self.hook_rx;
            let (resolved_path, start_offset) = if self.log_path.exists() {
                (self.log_path.clone(), self.start_offset)
            } else if let Some(ref baseline) = self.log_baseline {
                let found: Option<(PathBuf, u64)>;
                // Poll until the session log appears or the session shuts down.
                // Claude Code creates the .jsonl lazily (after first assistant
                // output), which can take minutes for thread agents that call
                // tools before responding. The previous 60s cap caused tier2 to
                // give up before the file existed, leaving resolved_log_path
                // unset — breaking transcript snapshots and S3 uploads for the
                // rest of the session.
                loop {
                    tokio::select! {
                        _ = shutdown.cancelled() => return,
                        announced = next_transcript_path(&mut hook_rx) => {
                            match announced {
                                Some(actual) => {
                                    let offset = baseline.offset_of(&actual);
                                    tracing::info!(
                                        configured = ?self.log_path,
                                        actual = ?actual,
                                        offset,
                                        "tier2: session log named by hook"
                                    );
                                    found = Some((actual, offset));
                                    break;
                                }
                                // The hook stream closed: fall back to the scan.
                                None => hook_rx = None,
                            }
                            continue;
                        }
                        _ = tokio::time::sleep(Duration::from_secs(2)) => {}
                    }
                    if self.log_path.exists() {
                        found = Some((self.log_path.clone(), self.start_offset));
                        break;
                    }
                    if hook_rx.is_some() {
                        continue;
                    }
                    if let Some((actual, offset)) = baseline.find_new_log() {
                        tracing::info!(
                            configured = ?self.log_path,
                            actual = ?actual,
                            offset,
                            preexisting_logs = baseline.len(),
                            "tier2: discovered actual session log"
                        );
                        found = Some((actual, offset));
                        break;
                    }
                }
                match found {
                    Some(p) => p,
                    None => {
                        // Only reachable via shutdown cancellation racing with discovery.
                        tracing::debug!(
                            path = ?self.log_path,
                            "tier2: session ended before log was discovered"
                        );
                        return;
                    }
                }
            } else {
                (self.log_path.clone(), self.start_offset)
            };

            // Publish the resolved path so transcript snapshots, S3 uploads,
            // and the switch subsystem use the correct (Claude-generated) log
            // file instead of the stale coop-predicted path.
            let resolved_log_path = self.resolved_log_path;
            if let Some(ref slot) = resolved_log_path {
                *slot.write().await = Some(resolved_path.clone());
            }

            // Every watcher feeds one channel, so batches an old watcher has
            // already sent are still handled after a rebind.
            let (line_tx, mut line_rx) = mpsc::channel(32);
            let poll_interval = self.poll_interval;
            let mut watch_shutdown =
                spawn_watcher(&resolved_path, start_offset, poll_interval, &line_tx, &shutdown);
            let log_baseline = self.log_baseline;
            let mut current_path = resolved_path;
            // Transcripts already followed this run: switching back to one
            // (`/resume` to the earlier session) reads it from where it ends
            // now, so what was already reported doesn't replay.
            let mut followed = HashSet::from([current_path.clone()]);
            let last_message = self.last_message;
            let raw_message_tx = self.raw_message_tx;
            let usage = self.usage;
            // Interactive session logs carry no durationMs, so LLM request
            // latency is derived from entry-timestamp gaps (ka-0000004093).
            let mut llm_latency = crate::usage::LlmLatencyTracker::default();
            // Claude writes one entry per content block, all repeating the
            // same message.usage — accumulate only the first entry per
            // message.id or tokens/request_count multiply by the block
            // fan-out (ka-0000005055).
            let mut usage_dedup = crate::usage::UsageDeduper::default();

            loop {
                tokio::select! {
                    _ = shutdown.cancelled() => break,
                    announced = next_transcript_path(&mut hook_rx) => {
                        let Some(actual) = announced else {
                            hook_rx = None;
                            continue;
                        };
                        if same_file(&actual, &current_path) {
                            continue;
                        }
                        let offset = if followed.contains(&actual) {
                            std::fs::metadata(&actual).map(|m| m.len()).unwrap_or(0)
                        } else {
                            log_baseline.as_ref().map_or(0, |b| b.offset_of(&actual))
                        };
                        tracing::info!(
                            previous = ?current_path,
                            actual = ?actual,
                            offset,
                            "tier2: hook names a new transcript, rebinding"
                        );
                        watch_shutdown.cancel();
                        watch_shutdown =
                            spawn_watcher(&actual, offset, poll_interval, &line_tx, &shutdown);
                        if let Some(ref slot) = resolved_log_path {
                            *slot.write().await = Some(actual.clone());
                        }
                        followed.insert(actual.clone());
                        current_path = actual;
                    }
                    batch = line_rx.recv() => {
                        match batch {
                            Some(lines) => {
                                for line in &lines {
                                    if let Ok(json) = serde_json::from_str::<serde_json::Value>(line) {
                                        if let Some(ref tx) = raw_message_tx {
                                            let _ = tx.send(RawMessageEvent {
                                                json: json.clone(),
                                                source: "log".to_owned(),
                                            });
                                        }
                                        if let Some(text) = extract_assistant_text(&json) {
                                            if let Some(ref lm) = last_message {
                                                *lm.write().await = Some(text);
                                            }
                                        }
                                        if let Some(state) = parse_claude_state(&json) {
                                            let cause = format_claude_cause(&json, "log");
                                            // Interrupt-detected idle and API errors are
                                            // high-confidence signals — emit at Tier 1 so
                                            // they can override the hook detector's stale
                                            // Working state. Without the error override an
                                            // Error (priority 2) emitted at Tier 2 while the
                                            // agent is mid-Working (priority 3) is rejected as
                                            // a downgrade by the composite detector, so the
                                            // rate-limit detail never reaches the classifier
                                            // and no nudge/retry fires (ka-0000006242).
                                            let tier_override =
                                                if cause == "log:user" || cause == "log:error" {
                                                    Some(1)
                                                } else {
                                                    None
                                                };
                                            let _ = state_tx.send((state, cause, tier_override)).await;
                                        }
                                        if let Some(ref u) = usage {
                                            // Fed every entry (in order) so the
                                            // tracker sees the request-start
                                            // timestamps between responses.
                                            let derived_ms = llm_latency.update(&json);
                                            if let Some(mut delta) = crate::usage::extract_usage_delta(&json) {
                                                if usage_dedup.should_accumulate(&json) {
                                                    // A literal durationMs (--print
                                                    // result entries) wins; the
                                                    // derived gap fills the
                                                    // interactive-session hole. Both
                                                    // are `Option`, so a derived
                                                    // `Some(0)` (a sub-ms gap) is
                                                    // preserved and still observed —
                                                    // only a genuinely unmeasurable
                                                    // `None` is dropped (ka-0000004208).
                                                    if delta.duration_api_ms.is_none() {
                                                        delta.duration_api_ms = derived_ms;
                                                    }
                                                    u.accumulate(delta).await;
                                                }
                                            }
                                        }
                                    }
                                }
                            }
                            None => break,
                        }
                    }
                }
            }
        })
    }

    fn tier(&self) -> u8 {
        2
    }
}

/// Start a `LogWatcher` on `path` from `offset`, sending batches to `line_tx`.
///
/// Returns the watcher's own token, a child of `shutdown`, so a rebind can stop
/// it alone.
fn spawn_watcher(
    path: &Path,
    offset: u64,
    poll_interval: Duration,
    line_tx: &mpsc::Sender<Vec<String>>,
    shutdown: &CancellationToken,
) -> CancellationToken {
    let watcher = if offset > 0 {
        LogWatcher::with_offset(path.to_path_buf(), offset)
    } else {
        LogWatcher::new(path.to_path_buf())
    }
    .with_poll_interval(poll_interval);
    let token = shutdown.child_token();
    let watch_shutdown = token.clone();
    let line_tx = line_tx.clone();
    tokio::spawn(async move {
        watcher.run(line_tx, watch_shutdown).await;
    });
    token
}

/// Whether `a` and `b` name the same file. A hook may spell the transcript
/// differently from coop's configured path (a symlinked `$HOME`); rebinding
/// then would re-read the same log from the wrong offset.
fn same_file(a: &Path, b: &Path) -> bool {
    if a == b {
        return true;
    }
    match (std::fs::canonicalize(a), std::fs::canonicalize(b)) {
        (Ok(a), Ok(b)) => a == b,
        _ => false,
    }
}

/// The next `transcript_path` a Claude hook event reports on `hook_rx`.
///
/// Returns `None` once the stream has closed (the caller then drops it), and
/// never resolves while `hook_rx` is `None`.
async fn next_transcript_path(
    hook_rx: &mut Option<broadcast::Receiver<RawHookEvent>>,
) -> Option<PathBuf> {
    let Some(rx) = hook_rx else {
        return std::future::pending().await;
    };
    loop {
        match rx.recv().await {
            Ok(event) => {
                let path = event
                    .json
                    .get("data")
                    .and_then(|d| d.get("transcript_path"))
                    .and_then(|v| v.as_str())
                    .filter(|p| !p.is_empty());
                if let Some(path) = path {
                    return Some(PathBuf::from(path));
                }
            }
            Err(broadcast::error::RecvError::Lagged(_)) => {}
            Err(broadcast::error::RecvError::Closed) => return None,
        }
    }
}

/// Create a Tier 3 stdout detector for Claude.
///
/// Parses structured JSONL from Claude's stdout stream (used when Claude is
/// invoked with `--print --output-format stream-json`). Classifies each entry
/// with `parse_claude_state` and extracts assistant message text.
pub fn new_stdout_detector(
    stdout_rx: mpsc::Receiver<Bytes>,
    last_message: Option<Arc<RwLock<Option<String>>>>,
    raw_message_tx: Option<broadcast::Sender<RawMessageEvent>>,
) -> impl Detector {
    use crate::driver::stdout_detect::StdoutDetector;
    StdoutDetector {
        stdout_rx,
        classify: Box::new(|json| {
            let state = parse_claude_state(json)?;
            let cause = format_claude_cause(json, "stdout");
            Some((state, cause))
        }),
        extract_message: Some(Box::new(extract_assistant_text)),
        last_message,
        raw_message_tx,
        extract_usage: None,
        usage: None,
    }
}

#[cfg(test)]
#[path = "stream_tests.rs"]
mod tests;
