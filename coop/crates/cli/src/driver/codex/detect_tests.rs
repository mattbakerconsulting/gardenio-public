// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use bytes::Bytes;
use tokio::sync::{mpsc, RwLock};
use tokio_util::sync::CancellationToken;

use super::{map_codex_hook, new_stdout_detector};
use crate::driver::{AgentState, Detector, HookEvent};
use crate::usage::UsageState;

const APP_SERVER: &str =
    include_str!("../../../../../tests/fixtures/codex/app-server-session.jsonl");
const EXEC_TOOL: &str =
    include_str!("../../../../../tests/fixtures/codex/exec-tool-stop-gate.jsonl");
const EXEC_401: &str =
    include_str!("../../../../../tests/fixtures/codex/exec-turn-failed-401.jsonl");

/// Replay `fixture` through the Tier 3 detector, returning every emission.
async fn replay(
    fixture: &str,
    last_message: Option<Arc<RwLock<Option<String>>>>,
    usage: Option<Arc<UsageState>>,
) -> anyhow::Result<Vec<(AgentState, String)>> {
    let (bytes_tx, bytes_rx) = mpsc::channel(512);
    let detector = Box::new(new_stdout_detector(bytes_rx, last_message, None, usage));
    assert_eq!(detector.tier(), 3);
    let (state_tx, mut state_rx) = mpsc::channel(512);
    let shutdown = CancellationToken::new();
    let handle = tokio::spawn(detector.run(state_tx, shutdown.clone()));

    for line in fixture.lines() {
        bytes_tx.send(Bytes::from(format!("{line}\n"))).await?;
    }
    drop(bytes_tx);
    handle.await?;

    let mut out = Vec::new();
    while let Ok((state, cause, tier)) = state_rx.try_recv() {
        assert!(tier.is_none());
        out.push((state, cause));
    }
    Ok(out)
}

#[tokio::test]
async fn detector_replays_the_app_server_session() -> anyhow::Result<()> {
    let slot = Arc::new(RwLock::new(None));
    let got = replay(APP_SERVER, Some(Arc::clone(&slot)), None).await?;
    assert_eq!(got.first(), Some(&(AgentState::Idle, "rpc:idle".to_owned())));
    assert!(got.iter().any(|(s, c)| *s == AgentState::Working && c == "rpc:working"));
    assert_eq!(got.last().map(|(s, _)| s), Some(&AgentState::Idle));
    assert!(!got.iter().any(|(s, _)| matches!(s, AgentState::Error { .. })));
    assert_eq!(slot.read().await.as_deref(), Some("after interrupt reply"));
    Ok(())
}

#[tokio::test]
async fn detector_replays_the_exec_tool_stream() -> anyhow::Result<()> {
    let slot = Arc::new(RwLock::new(None));
    let got = replay(EXEC_TOOL, Some(Arc::clone(&slot)), None).await?;
    assert_eq!(got.first(), Some(&(AgentState::Working, "exec:working".to_owned())));
    assert_eq!(got.last(), Some(&(AgentState::Idle, "exec:idle".to_owned())));
    assert_eq!(slot.read().await.as_deref(), Some("after gate done"));
    Ok(())
}

#[tokio::test]
async fn detector_reports_a_failed_exec_turn_as_unauthorized() -> anyhow::Result<()> {
    use crate::driver::{classify_error_detail, ErrorCategory};
    let got = replay(EXEC_401, None, None).await?;
    let errors: Vec<_> =
        got.iter().filter(|(s, _)| matches!(s, AgentState::Error { .. })).collect();
    assert_eq!(errors.len(), 1, "only turn.failed fails the turn: {got:?}");
    let AgentState::Error { detail } = &errors[0].0 else { anyhow::bail!("not an error") };
    assert_eq!(classify_error_detail(detail), ErrorCategory::Unauthorized);
    assert_eq!(errors[0].1, "exec:error");
    // The turn ran (Working) before it failed.
    assert!(got.iter().any(|(s, _)| *s == AgentState::Working));
    Ok(())
}

#[tokio::test]
async fn exec_turn_completed_usage_reaches_the_session_usage() -> anyhow::Result<()> {
    let usage = Arc::new(UsageState::new());
    replay(EXEC_TOOL, None, Some(Arc::clone(&usage))).await?;
    let snap = usage.snapshot().await;
    assert_eq!((snap.input_tokens, snap.output_tokens, snap.request_count), (300, 60, 1));
    assert_eq!(snap.total_cost_usd, 0.0);
    Ok(())
}

#[tokio::test]
async fn app_server_token_usage_notifications_reach_the_session_usage() -> anyhow::Result<()> {
    let usage = Arc::new(UsageState::new());
    replay(APP_SERVER, None, Some(Arc::clone(&usage))).await?;
    // The fixture's running total ends at 200 in / 40 out (one notification
    // repeats the previous total and adds nothing).
    let snap = usage.snapshot().await;
    assert_eq!((snap.input_tokens, snap.output_tokens), (200, 40));
    Ok(())
}

#[test]
fn hook_events_map_to_working_and_idle() {
    let working = |e| map_codex_hook(e).map(|(s, _)| s);
    assert_eq!(working(HookEvent::TurnStart), Some(AgentState::Working));
    assert_eq!(
        working(HookEvent::ToolBefore { tool: "Bash".into(), tool_input: None, tool_use_id: None }),
        Some(AgentState::Working)
    );
    assert_eq!(
        working(HookEvent::ToolAfter {
            tool: "Bash".into(),
            tool_input: None,
            tool_use_id: None,
            duration_ms: None
        }),
        Some(AgentState::Working)
    );
    assert_eq!(working(HookEvent::TurnEnd), Some(AgentState::Idle));
    assert_eq!(working(HookEvent::SessionEnd), Some(AgentState::Idle));
    assert_eq!(working(HookEvent::SessionStart), None);
    assert_eq!(working(HookEvent::Notification { notification_type: "x".into() }), None);
}
