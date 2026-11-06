// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use bytes::Bytes;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::driver::{AgentState, Detector};

use super::LogDetector;

/// Build a LogDetector with fast test-appropriate poll interval.
fn test_log_detector(log_path: std::path::PathBuf) -> Box<LogDetector> {
    Box::new(LogDetector {
        log_path,
        start_offset: 0,
        // Short poll for tests; production uses config.log_poll() (3s default).
        poll_interval: std::time::Duration::from_millis(50),
        last_message: None,
        raw_message_tx: None,
        usage: None,
        log_baseline: None,
        hook_rx: None,
        resolved_log_path: None,
    })
}

#[tokio::test]
async fn log_detector_parses_lines_and_emits_states() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");

    // Write data before starting the detector — the first poll tick reads immediately.
    // Use a user message (→ Working) followed by an assistant text-only (→ Idle).
    // Note: non-meaningful types like "system" and "progress" are intentionally
    // ignored by parse_claude_state to avoid spurious Tier 2 Working emissions.
    std::fs::write(
        &log_path,
        concat!(
            "{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}}\n",
            "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"done\"}]}}\n",
        ),
    )?;

    let detector = test_log_detector(log_path);
    assert_eq!(detector.tier(), 2);

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let shutdown_clone = shutdown.clone();

    let handle = tokio::spawn(async move {
        detector.run(state_tx, shutdown_clone).await;
    });

    // Wait for states to arrive
    let mut states = Vec::new();
    let timeout = tokio::time::timeout(std::time::Duration::from_secs(2), async {
        while let Some((state, _cause, _)) = state_rx.recv().await {
            states.push(state.clone());
            if matches!(state, AgentState::Idle) {
                break;
            }
        }
    })
    .await;

    shutdown.cancel();
    let _ = handle.await;

    // Should have received at least Working (user) and Idle (assistant text-only)
    assert!(timeout.is_ok(), "timed out waiting for states");
    assert!(states.iter().any(|s| matches!(s, AgentState::Working)));
    assert!(states.iter().any(|s| matches!(s, AgentState::Idle)));
    Ok(())
}

#[tokio::test]
async fn log_detector_skips_non_assistant_lines() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");

    // Write data before starting the detector.
    std::fs::write(&log_path, "{\"type\":\"user\",\"message\":{\"content\":[]}}\n")?;

    let detector = test_log_detector(log_path);
    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let shutdown_clone = shutdown.clone();

    let handle = tokio::spawn(async move {
        detector.run(state_tx, shutdown_clone).await;
    });

    // User messages produce Working (not Idle)
    let timeout =
        tokio::time::timeout(std::time::Duration::from_secs(2), async { state_rx.recv().await })
            .await;

    shutdown.cancel();
    let _ = handle.await;

    if let Ok(Some((state, _cause, _))) = timeout {
        assert!(matches!(state, AgentState::Working));
    }
    Ok(())
}

#[tokio::test]
async fn stdout_detector_parses_jsonl_bytes() -> anyhow::Result<()> {
    let (bytes_tx, bytes_rx) = mpsc::channel(32);
    let detector = Box::new(super::new_stdout_detector(bytes_rx, None, None));
    assert_eq!(detector.tier(), 3);

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let shutdown_clone = shutdown.clone();

    let handle = tokio::spawn(async move {
        detector.run(state_tx, shutdown_clone).await;
    });

    // Send a JSONL line as raw bytes
    bytes_tx
        .send(Bytes::from(
            "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"name\":\"Bash\",\"input\":{}}]}}\n",
        ))
        .await?;

    let state = tokio::time::timeout(std::time::Duration::from_secs(2), state_rx.recv()).await;

    shutdown.cancel();
    let _ = handle.await;

    match state {
        Ok(Some((AgentState::Working, _cause, _))) => {} // tool_use → Working
        other => anyhow::bail!("expected Working, got {other:?}"),
    }
    Ok(())
}

/// Helper: create a HookDetector with a named pipe, run it, and send events.
async fn run_hook_detector(events: Vec<&str>) -> anyhow::Result<Vec<AgentState>> {
    use crate::driver::hook_recv::HookReceiver;
    use tokio::io::AsyncWriteExt;

    let dir = tempfile::tempdir()?;
    let pipe_path = dir.path().join("hook.pipe");
    let receiver = HookReceiver::new(&pipe_path)?;
    let detector = Box::new(super::new_hook_detector(receiver, None, None));
    assert_eq!(detector.tier(), 1);

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();

    let handle = tokio::spawn(async move {
        detector.run(state_tx, sd).await;
    });

    // Write events from another task
    let pipe = pipe_path.clone();
    let events_owned: Vec<String> = events.iter().map(|s| s.to_string()).collect();
    tokio::spawn(async move {
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        let mut file = match tokio::fs::OpenOptions::new().write(true).open(&pipe).await {
            Ok(f) => f,
            Err(_) => return,
        };
        for event in events_owned {
            let _ = file.write_all(event.as_bytes()).await;
            let _ = file.write_all(b"\n").await;
        }
    });

    let mut states = Vec::new();
    let timeout = tokio::time::timeout(std::time::Duration::from_secs(2), async {
        while let Some((state, _cause, _)) = state_rx.recv().await {
            states.push(state);
            if states.len() >= events.len() {
                break;
            }
        }
    })
    .await;

    shutdown.cancel();
    let _ = handle.await;

    if timeout.is_err() && states.is_empty() {
        anyhow::bail!("timed out waiting for states");
    }
    Ok(states)
}

#[tokio::test]
async fn hook_detector_notification_idle_prompt() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![
        r#"{"event":"notification","data":{"notification_type":"idle_prompt"}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Idle));
    Ok(())
}

#[tokio::test]
async fn hook_detector_notification_permission_prompt() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![
        r#"{"event":"notification","data":{"notification_type":"permission_prompt"}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Prompt { .. }));
    if let AgentState::Prompt { prompt } = &states[0] {
        assert_eq!(prompt.kind, crate::driver::PromptKind::Permission);
    }
    Ok(())
}

#[tokio::test]
async fn hook_detector_pre_tool_use_ask_user() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![
        r#"{"event":"pre_tool_use","data":{"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Which DB?","options":[{"label":"PostgreSQL"},{"label":"SQLite"}]}]}}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    if let AgentState::Prompt { prompt } = &states[0] {
        assert_eq!(prompt.kind, crate::driver::PromptKind::Question);
        assert_eq!(prompt.questions.len(), 1);
        assert_eq!(prompt.questions[0].question, "Which DB?");
        assert_eq!(prompt.questions[0].options, vec!["PostgreSQL", "SQLite"]);
    } else {
        anyhow::bail!("expected Prompt(Question), got {:?}", states[0]);
    }
    Ok(())
}

#[tokio::test]
async fn hook_detector_pre_tool_use_exit_plan_mode() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![
        r#"{"event":"pre_tool_use","data":{"tool_name":"ExitPlanMode","tool_input":{}}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Prompt { .. }));
    if let AgentState::Prompt { prompt } = &states[0] {
        assert_eq!(prompt.kind, crate::driver::PromptKind::Plan);
        assert!(prompt.ready, "hook-detected Plan prompt should be immediately ready");
    }
    Ok(())
}

#[tokio::test]
async fn hook_detector_pre_tool_use_enter_plan_mode() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![
        r#"{"event":"pre_tool_use","data":{"tool_name":"EnterPlanMode","tool_input":{}}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Working));
    Ok(())
}

#[tokio::test]
async fn hook_detector_tool_complete() -> anyhow::Result<()> {
    // PostToolUse fires only for real agent tool calls (not command mode).
    // It confirms the agent is actively working mid-turn.
    let states = run_hook_detector(vec![
        r#"{"event":"post_tool_use","data":{"tool_name":"Bash","tool_input":{"command":"ls"}}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Working));
    Ok(())
}

#[tokio::test]
async fn hook_detector_pre_tool_use_bash_emits_working() -> anyhow::Result<()> {
    // PreToolUse for non-special tools (Bash, Read, etc.) now emits Working
    // with a rich cause string including the tool label.
    let states = run_hook_detector(vec![
        r#"{"event":"pre_tool_use","data":{"tool_name":"Bash","tool_input":{"command":"git status","description":"Show working tree status"}}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Working));
    Ok(())
}

#[tokio::test]
async fn hook_detector_records_tool_call_latency() -> anyhow::Result<()> {
    // A ToolBefore/ToolAfter pair must land one observation in the tool-call
    // histogram (ka-0000004093: the histogram was registered but never
    // observed because PreToolUse only fired for prompt-shaped tools).
    use crate::driver::hook_recv::HookReceiver;
    use crate::usage::UsageState;
    use std::sync::Arc;
    use tokio::io::AsyncWriteExt;

    let dir = tempfile::tempdir()?;
    let pipe_path = dir.path().join("hook.pipe");
    let receiver = HookReceiver::new(&pipe_path)?;
    let usage = Arc::new(UsageState::new());
    let detector = Box::new(super::new_hook_detector(receiver, None, Some(Arc::clone(&usage))));

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();
    let handle = tokio::spawn(async move {
        detector.run(state_tx, sd).await;
    });

    let mut pipe = tokio::fs::OpenOptions::new().write(true).open(&pipe_path).await?;
    pipe.write_all(
        concat!(
            r#"{"event":"pre_tool_use","data":{"tool_name":"Bash","tool_input":{"command":"ls"}}}"#,
            "\n",
            r#"{"event":"post_tool_use","data":{"tool_name":"Bash","tool_input":{"command":"ls"}}}"#,
            "\n",
        )
        .as_bytes(),
    )
    .await?;

    // Both events emit Working; receipt of the second guarantees the
    // ToolAfter side of the timer has run.
    let deadline = std::time::Duration::from_secs(2);
    for _ in 0..2 {
        let recv = tokio::time::timeout(deadline, state_rx.recv()).await;
        anyhow::ensure!(matches!(recv, Ok(Some(_))), "hook detector state not received");
    }
    shutdown.cancel();
    let _ = handle.await;

    let hist = usage.histogram_snapshot().await;
    assert_eq!(hist.tool_call.count(), 1, "ToolBefore→ToolAfter pair must observe once");
    Ok(())
}

#[tokio::test]
async fn hook_detector_prefers_claudes_duration_ms_over_wall_clock() -> anyhow::Result<()> {
    // When PostToolUse carries duration_ms, it must be used verbatim instead
    // of coop's own before/after Instant delta, which includes coop's
    // hook-dispatch overhead on top of the tool's real run time
    // (gd-Kl42VWWRd6).
    use crate::driver::hook_recv::HookReceiver;
    use crate::usage::UsageState;
    use std::sync::Arc;
    use tokio::io::AsyncWriteExt;

    let dir = tempfile::tempdir()?;
    let pipe_path = dir.path().join("hook.pipe");
    let receiver = HookReceiver::new(&pipe_path)?;
    let usage = Arc::new(UsageState::new());
    let detector = Box::new(super::new_hook_detector(receiver, None, Some(Arc::clone(&usage))));

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();
    let handle = tokio::spawn(async move {
        detector.run(state_tx, sd).await;
    });

    let mut pipe = tokio::fs::OpenOptions::new().write(true).open(&pipe_path).await?;
    pipe.write_all(
        concat!(
            r#"{"event":"pre_tool_use","data":{"tool_name":"Bash","tool_use_id":"call_a"}}"#,
            "\n",
            r#"{"event":"post_tool_use","data":{"tool_name":"Bash","tool_use_id":"call_a","duration_ms":42000}}"#,
            "\n",
        )
        .as_bytes(),
    )
    .await?;

    let deadline = std::time::Duration::from_secs(2);
    for _ in 0..2 {
        let recv = tokio::time::timeout(deadline, state_rx.recv()).await;
        anyhow::ensure!(matches!(recv, Ok(Some(_))), "hook detector state not received");
    }
    shutdown.cancel();
    let _ = handle.await;

    let hist = usage.histogram_snapshot().await;
    assert_eq!(hist.tool_call.count(), 1);
    assert!(
        (hist.tool_call.sum() - 42.0).abs() < 1e-6,
        "expected the reported 42s duration_ms, not wall-clock elapsed, got {}",
        hist.tool_call.sum()
    );
    Ok(())
}

#[tokio::test]
async fn log_detector_derives_llm_latency_from_timestamps() -> anyhow::Result<()> {
    // Interactive session logs have no durationMs; the detector must derive
    // per-response latency from entry-timestamp gaps and feed both the
    // llm_request histogram and total_api_ms (ka-0000004093).
    use crate::usage::UsageState;
    use std::sync::Arc;

    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");
    // Tool result at t=0; two content-block entries of one response at
    // t=1.5s/t=2s (the second block must NOT observe or accumulate again,
    // ka-0000005055); a second response at t=3s as the consumed-everything
    // sentinel (derived gap 1s from the t=2s block before it).
    std::fs::write(
        &log_path,
        concat!(
            r#"{"type":"user","timestamp":"1970-01-01T00:00:00Z","message":{"content":[{"type":"text","text":"hi"}]}}"#,
            "\n",
            r#"{"type":"assistant","timestamp":"1970-01-01T00:00:01.500Z","message":{"id":"msg_A","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":10,"output_tokens":5}}}"#,
            "\n",
            r#"{"type":"assistant","timestamp":"1970-01-01T00:00:02Z","message":{"id":"msg_A","content":[{"type":"text","text":"still"}],"usage":{"input_tokens":10,"output_tokens":5}}}"#,
            "\n",
            r#"{"type":"assistant","timestamp":"1970-01-01T00:00:03Z","message":{"id":"msg_B","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":20,"output_tokens":7}}}"#,
            "\n",
        ),
    )?;

    let usage = Arc::new(UsageState::new());
    let mut detector = test_log_detector(log_path);
    detector.usage = Some(Arc::clone(&usage));

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();
    let handle = tokio::spawn(async move {
        detector.run(state_tx, sd).await;
    });

    // Wait until msg_B lands — everything before it has then been consumed.
    let waited = tokio::time::timeout(std::time::Duration::from_secs(2), async {
        loop {
            if usage.snapshot().await.request_count >= 2 {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        }
    })
    .await;
    shutdown.cancel();
    while state_rx.try_recv().is_ok() {}
    let _ = handle.await;
    anyhow::ensure!(waited.is_ok(), "log detector did not consume usage entries in time");

    let snapshot = usage.snapshot().await;
    // Two responses, not three usage-bearing lines: msg_A's continuation
    // block must not inflate the counters (ka-0000005055).
    assert_eq!(snapshot.request_count, 2, "one request per message id");
    assert_eq!(snapshot.input_tokens, 30, "msg_A counted once (10) + msg_B (20)");
    assert_eq!(snapshot.output_tokens, 12, "msg_A counted once (5) + msg_B (7)");

    let hist = usage.histogram_snapshot().await;
    assert_eq!(hist.llm_request.count(), 2, "one latency sample per response");
    assert!((hist.llm_request.sum() - 2.5).abs() < 1e-9, "derived 1.5s + 1s gaps");
    // The derived durations also revive total_api_ms.
    assert_eq!(snapshot.total_api_ms, 2500);
    Ok(())
}

#[tokio::test]
async fn hook_detector_stop() -> anyhow::Result<()> {
    let states =
        run_hook_detector(vec![r#"{"event":"stop","data":{"stop_hook_active":false}}"#]).await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Idle));
    Ok(())
}

// --- Direct map_claude_hook unit tests for cause strings ---

#[test]
fn map_hook_tool_before_bash_includes_label() {
    use crate::driver::HookEvent;
    let event = HookEvent::ToolBefore {
        tool: "Bash".into(),
        tool_input: Some(serde_json::json!({ "command": "git status" })),
        tool_use_id: None,
    };
    let (state, cause) = super::map_claude_hook(event).expect("should emit");
    assert!(matches!(state, AgentState::Working));
    assert_eq!(cause, "hook:tool(Bash: git status)");
}

#[test]
fn map_hook_tool_before_read_includes_basename() {
    use crate::driver::HookEvent;
    let event = HookEvent::ToolBefore {
        tool: "Read".into(),
        tool_input: Some(serde_json::json!({ "file_path": "/home/user/src/main.rs" })),
        tool_use_id: None,
    };
    let (state, cause) = super::map_claude_hook(event).expect("should emit");
    assert!(matches!(state, AgentState::Working));
    assert_eq!(cause, "hook:tool(Read: main.rs)");
}

#[test]
fn map_hook_tool_before_no_input_plain_cause() {
    use crate::driver::HookEvent;
    let event = HookEvent::ToolBefore { tool: "Bash".into(), tool_input: None, tool_use_id: None };
    let (state, cause) = super::map_claude_hook(event).expect("should emit");
    assert!(matches!(state, AgentState::Working));
    assert_eq!(cause, "hook:tool(Bash)");
}

#[test]
fn map_hook_tool_after_plain_cause() {
    use crate::driver::HookEvent;
    let event = HookEvent::ToolAfter {
        tool: "Bash".into(),
        tool_input: None,
        tool_use_id: None,
        duration_ms: None,
    };
    let (state, cause) = super::map_claude_hook(event).expect("should emit");
    assert!(matches!(state, AgentState::Working));
    assert_eq!(cause, "hook:tool(Bash)");
}

#[tokio::test]
async fn hook_detector_agent_mode_turn() -> anyhow::Result<()> {
    // Agent mode: user submits message (TurnStart → Working), tools execute
    // (PostToolUse → Working confirmations), turn ends (TurnEnd → Idle).
    //
    // The raw detector emits Working for each PostToolUse (which the composite
    // detector would dedup). This test verifies the raw detector output.
    let states = run_hook_detector(vec![
        r#"{"event":"user_prompt_submit","data":{"prompt":"hello"}}"#, // → TurnStart → Working
        r#"{"event":"post_tool_use","data":{"tool_name":"Read","tool_input":{}}}"#, // → Working
        r#"{"event":"post_tool_use","data":{"tool_name":"Write","tool_input":{}}}"#, // → Working
        r#"{"event":"stop","data":{"stop_hook_active":false}}"#,       // → TurnEnd → Idle
    ])
    .await?;

    // Raw detector emits: Working, Working, Working, Idle
    // (The composite detector would dedup the repeated Working states)
    assert_eq!(states.len(), 4, "expected 4 raw states");
    assert!(matches!(states[0], AgentState::Working), "TurnStart → Working");
    assert!(matches!(states[1], AgentState::Working), "PostToolUse(Read) → Working");
    assert!(matches!(states[2], AgentState::Working), "PostToolUse(Write) → Working");
    assert!(matches!(states[3], AgentState::Idle), "TurnEnd → Idle");
    Ok(())
}

/// Run a tier-2 detector whose configured log never exists, so it must
/// discover the real one against `baseline`. Returns the states emitted until
/// the first Idle (or the timeout) and the path it published.
async fn run_discovering_detector(
    baseline: super::resume::SessionLogBaseline,
    write_live: impl FnOnce() -> anyhow::Result<()>,
) -> anyhow::Result<(Vec<AgentState>, Option<std::path::PathBuf>)> {
    run_discovering_detector_with_hooks(baseline, None, write_live).await
}

/// [`run_discovering_detector`] with the detector subscribed to `hook_rx`.
async fn run_discovering_detector_with_hooks(
    baseline: super::resume::SessionLogBaseline,
    hook_rx: Option<tokio::sync::broadcast::Receiver<crate::event::RawHookEvent>>,
    write_live: impl FnOnce() -> anyhow::Result<()>,
) -> anyhow::Result<(Vec<AgentState>, Option<std::path::PathBuf>)> {
    let resolved = std::sync::Arc::new(tokio::sync::RwLock::new(None));
    let mut detector = test_log_detector(std::path::PathBuf::from("/nonexistent/predicted.jsonl"));
    detector.log_baseline = Some(baseline);
    detector.hook_rx = hook_rx;
    detector.resolved_log_path = Some(std::sync::Arc::clone(&resolved));

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let shutdown_clone = shutdown.clone();
    let handle = tokio::spawn(async move {
        detector.run(state_tx, shutdown_clone).await;
    });

    // Claude creates its log lazily, so let discovery poll (every 2s) at
    // least once before it appears: unbounded discovery latched the stale
    // log on that first poll.
    tokio::time::sleep(std::time::Duration::from_millis(2500)).await;
    write_live()?;

    let mut states = Vec::new();
    let _ = tokio::time::timeout(std::time::Duration::from_secs(10), async {
        while let Some((state, _cause, _)) = state_rx.recv().await {
            let idle = matches!(state, AgentState::Idle);
            states.push(state);
            if idle {
                break;
            }
        }
    })
    .await;
    shutdown.cancel();
    let _ = handle.await;
    let published = resolved.read().await.clone();
    Ok((states, published))
}

const API_ERROR_ENTRY: &str = "{\"type\":\"assistant\",\"isApiErrorMessage\":true,\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"API Error: Connection refused\"}]}}\n";
const USER_ENTRY: &str =
    "{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}}\n";
const IDLE_ENTRY: &str =
    "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"done\"}]}}\n";

/// A temp projects dir holding the previous incarnation's transcript, which
/// ends in an API error, and the baseline captured "before spawn".
fn stale_project() -> anyhow::Result<(
    tempfile::TempDir,
    std::path::PathBuf,
    std::path::PathBuf,
    super::resume::SessionLogBaseline,
)> {
    let root = tempfile::tempdir()?;
    let working_dir = root.path().join("work");
    std::fs::create_dir_all(&working_dir)?;
    let project_dir = root
        .path()
        .join("projects")
        .join(crate::driver::claude::setup::project_dir_name(&working_dir));
    std::fs::create_dir_all(&project_dir)?;
    let stale = project_dir.join("previous.jsonl");
    std::fs::write(&stale, format!("{USER_ENTRY}{}{API_ERROR_ENTRY}", " ".repeat(300)))?;
    let baseline =
        super::resume::SessionLogBaseline::capture_in(root.path().join("projects"), &working_dir);
    Ok((root, project_dir, stale, baseline))
}

#[tokio::test]
async fn log_detector_discovery_skips_previous_incarnation_log() -> anyhow::Result<()> {
    let (_root, project_dir, _stale, baseline) = stale_project()?;
    let live = project_dir.join("live.jsonl");
    let live_clone = live.clone();

    let (states, published) = run_discovering_detector(baseline, move || {
        std::fs::write(&live_clone, format!("{USER_ENTRY}{IDLE_ENTRY}{}\n", " ".repeat(300)))?;
        Ok(())
    })
    .await?;

    assert!(
        !states.iter().any(|s| matches!(s, AgentState::Error { .. })),
        "replayed the previous incarnation's API error: {states:?}"
    );
    assert!(states.iter().any(|s| matches!(s, AgentState::Idle)), "states: {states:?}");
    assert_eq!(published, Some(live));
    Ok(())
}

#[tokio::test]
async fn log_detector_discovery_tails_grown_log_from_baseline_size() -> anyhow::Result<()> {
    let (_root, _project_dir, stale, baseline) = stale_project()?;
    let stale_clone = stale.clone();

    let (states, published) = run_discovering_detector(baseline, move || {
        let mut file = std::fs::OpenOptions::new().append(true).open(&stale_clone)?;
        std::io::Write::write_all(&mut file, format!("{USER_ENTRY}{IDLE_ENTRY}").as_bytes())?;
        Ok(())
    })
    .await?;

    assert!(
        !states.iter().any(|s| matches!(s, AgentState::Error { .. })),
        "replayed history from before the baseline: {states:?}"
    );
    assert!(states.iter().any(|s| matches!(s, AgentState::Idle)), "states: {states:?}");
    assert_eq!(published, Some(stale));
    Ok(())
}

/// A raw `SessionStart` hook event naming `transcript` as Claude sends it.
fn session_start_hook(transcript: &std::path::Path) -> crate::event::RawHookEvent {
    crate::event::RawHookEvent {
        json: serde_json::json!({
            "event": "start",
            "data": {
                "session_id": "live",
                "transcript_path": transcript.display().to_string(),
                "hook_event_name": "SessionStart",
            },
        }),
    }
}

fn append(path: &std::path::Path, text: &str) -> anyhow::Result<()> {
    let mut file = std::fs::OpenOptions::new().append(true).open(path)?;
    std::io::Write::write_all(&mut file, text.as_bytes())?;
    Ok(())
}

/// A local agent on a laptop (gd-4tAWXFTMVL): while this Claude has yet to
/// write its log, another session in the same project dir keeps writing. A
/// scan latches that session on its first poll; with hooks wired discovery
/// waits for the hook, which names the right one.
#[tokio::test]
async fn log_detector_binds_to_hook_transcript_not_another_growing_log() -> anyhow::Result<()> {
    let (_root, project_dir, stale, baseline) = stale_project()?;
    let live = project_dir.join("live.jsonl");
    let (hook_tx, hook_rx) = tokio::sync::broadcast::channel(8);
    // The other session grows before the detector's first poll.
    append(&stale, API_ERROR_ENTRY)?;
    let live_clone = live.clone();

    let (states, published) =
        run_discovering_detector_with_hooks(baseline, Some(hook_rx), move || {
            hook_tx.send(session_start_hook(&live_clone))?;
            std::fs::write(&live_clone, format!("{USER_ENTRY}{IDLE_ENTRY}"))?;
            Ok(())
        })
        .await?;

    assert!(
        !states.iter().any(|s| matches!(s, AgentState::Error { .. })),
        "latched the other session's log: {states:?}"
    );
    assert!(states.iter().any(|s| matches!(s, AgentState::Idle)), "states: {states:?}");
    assert_eq!(published, Some(live));
    Ok(())
}

/// If the hook stream closes before naming a transcript, discovery falls
/// back to the baseline scan.
#[tokio::test]
async fn log_detector_scans_once_hook_stream_closes() -> anyhow::Result<()> {
    let (_root, _project_dir, stale, baseline) = stale_project()?;
    let (hook_tx, hook_rx) = tokio::sync::broadcast::channel(8);
    let stale_clone = stale.clone();

    let (states, published) =
        run_discovering_detector_with_hooks(baseline, Some(hook_rx), move || {
            append(&stale_clone, &format!("{USER_ENTRY}{IDLE_ENTRY}"))?;
            drop(hook_tx);
            Ok(())
        })
        .await?;

    // The grown log is read from its pre-spawn size, as without hooks.
    assert!(
        !states.iter().any(|s| matches!(s, AgentState::Error { .. })),
        "replayed history from before the baseline: {states:?}"
    );
    assert!(states.iter().any(|s| matches!(s, AgentState::Idle)), "states: {states:?}");
    assert_eq!(published, Some(stale));
    Ok(())
}

/// `/clear` mid-run (gd-iJuducXXED): Claude starts a new session and its
/// SessionStart hook names a new transcript. Tier 2 must follow it, not keep
/// reading the transcript it bound first.
#[tokio::test]
async fn log_detector_rebinds_when_a_later_hook_names_a_new_transcript() -> anyhow::Result<()> {
    let (_root, project_dir, _stale, baseline) = stale_project()?;
    let first = project_dir.join("first.jsonl");
    let cleared = project_dir.join("cleared.jsonl");
    let (hook_tx, hook_rx) = tokio::sync::broadcast::channel(8);
    let resolved = std::sync::Arc::new(tokio::sync::RwLock::new(None));
    let last_message = std::sync::Arc::new(tokio::sync::RwLock::new(None));
    let mut detector = test_log_detector(std::path::PathBuf::from("/nonexistent/predicted.jsonl"));
    detector.log_baseline = Some(baseline);
    detector.hook_rx = Some(hook_rx);
    detector.resolved_log_path = Some(std::sync::Arc::clone(&resolved));
    detector.last_message = Some(std::sync::Arc::clone(&last_message));

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let shutdown_clone = shutdown.clone();
    let handle = tokio::spawn(async move {
        detector.run(state_tx, shutdown_clone).await;
    });

    hook_tx.send(session_start_hook(&first))?;
    std::fs::write(&first, format!("{USER_ENTRY}{IDLE_ENTRY}"))?;
    let bound = tokio::time::timeout(std::time::Duration::from_secs(10), async {
        while let Some((state, _cause, _)) = state_rx.recv().await {
            if matches!(state, AgentState::Idle) {
                return true;
            }
        }
        false
    })
    .await;
    assert!(matches!(bound, Ok(true)), "never read the first transcript");
    assert_eq!(resolved.read().await.clone(), Some(first.clone()));

    // /clear: the new session's hook names its transcript, which Claude then
    // writes; the old transcript is no longer this session's.
    hook_tx.send(session_start_hook(&cleared))?;
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    std::fs::write(
        &cleared,
        format!("{USER_ENTRY}{}", IDLE_ENTRY.replace("\"done\"", "\"after clear\"")),
    )?;
    append(&first, API_ERROR_ENTRY)?;

    let mut states = Vec::new();
    let _ = tokio::time::timeout(std::time::Duration::from_secs(10), async {
        while let Some((state, _cause, _)) = state_rx.recv().await {
            let idle = matches!(state, AgentState::Idle);
            states.push(state);
            if idle {
                break;
            }
        }
    })
    .await;
    // Give the old watcher a poll to (wrongly) pick up its appended error.
    tokio::time::sleep(std::time::Duration::from_millis(300)).await;
    while let Ok((state, _cause, _)) = state_rx.try_recv() {
        states.push(state);
    }
    shutdown.cancel();
    let _ = handle.await;

    assert!(states.iter().any(|s| matches!(s, AgentState::Idle)), "states: {states:?}");
    assert!(
        !states.iter().any(|s| matches!(s, AgentState::Error { .. })),
        "still reading the transcript from before /clear: {states:?}"
    );
    assert_eq!(resolved.read().await.clone(), Some(cleared));
    assert_eq!(last_message.read().await.as_deref(), Some("after clear"));
    Ok(())
}

/// Every hook repeats the transcript tier 2 is already on; that is not a
/// rebind, so the log isn't re-read from another offset.
#[test]
fn same_file_matches_one_file_spelled_two_ways() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log = dir.path().join("s.jsonl");
    std::fs::write(&log, "")?;
    // A symlinked $HOME spells the same transcript through the link.
    let link = dir.path().join("home");
    std::os::unix::fs::symlink(dir.path(), &link)?;
    assert!(super::same_file(&log, &log));
    assert!(super::same_file(&log, &link.join("s.jsonl")));
    assert!(!super::same_file(&log, &dir.path().join("other.jsonl")));
    Ok(())
}
