// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use crate::driver::HookEvent;

use super::{parse_hook_line, HookReceiver};

#[test]
fn parses_tool_complete_event() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"post_tool_use","data":{"tool_name":"Bash","tool_input":{"command":"ls"}}}"#,
    )
    .expect("should parse");
    match event {
        HookEvent::ToolAfter { tool, tool_input, .. } => {
            assert_eq!(tool, "Bash");
            let input = tool_input.expect("tool_input should be Some");
            assert_eq!(input["command"], "ls");
        }
        other => panic!("expected ToolAfter, got {other:?}"),
    }
}

#[test]
fn parses_before_agent_event() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"before_agent","data":{"prompt":"Fix the bug","hook_event_name":"BeforeAgent"}}"#,
    )
    .expect("should parse");
    assert_eq!(event, HookEvent::TurnStart);
}

#[test]
fn parses_stop_event() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"stop","data":{"hook_event_name":"Stop","stop_hook_active":false}}"#,
    )
    .expect("should parse");
    assert_eq!(event, HookEvent::TurnEnd);
}

#[test]
fn parses_session_end_event() {
    let (event, _raw) = parse_hook_line(r#"{"event":"session_end"}"#).expect("should parse");
    assert_eq!(event, HookEvent::SessionEnd);
}

#[test]
fn parses_notification_idle_prompt() {
    let (event, _raw) =
        parse_hook_line(r#"{"event":"notification","data":{"notification_type":"idle_prompt"}}"#)
            .expect("should parse");
    assert_eq!(event, HookEvent::Notification { notification_type: "idle_prompt".to_string() });
}

#[test]
fn parses_notification_permission_prompt() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"notification","data":{"notification_type":"permission_prompt"}}"#,
    )
    .expect("should parse");
    assert_eq!(
        event,
        HookEvent::Notification { notification_type: "permission_prompt".to_string() }
    );
}

#[test]
fn parses_pre_tool_use_ask_user() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"pre_tool_use","data":{"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Which DB?"}]}}}"#,
    )
    .expect("should parse");
    match event {
        HookEvent::ToolBefore { tool, tool_input, .. } => {
            assert_eq!(tool, "AskUserQuestion");
            assert!(tool_input.is_some());
            let input = tool_input.expect("tool_input should be Some");
            assert!(input.get("questions").is_some());
        }
        other => panic!("expected ToolBefore, got {other:?}"),
    }
}

#[test]
fn parses_pre_tool_use_exit_plan() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"pre_tool_use","data":{"tool_name":"ExitPlanMode","tool_input":{}}}"#,
    )
    .expect("should parse");
    match event {
        HookEvent::ToolBefore { tool, tool_input, .. } => {
            assert_eq!(tool, "ExitPlanMode");
            assert!(tool_input.is_some());
        }
        other => panic!("expected ToolBefore, got {other:?}"),
    }
}

#[test]
fn parses_pre_tool_use_without_tool_input() {
    let (event, _raw) =
        parse_hook_line(r#"{"event":"pre_tool_use","data":{"tool_name":"EnterPlanMode"}}"#)
            .expect("should parse");
    match event {
        HookEvent::ToolBefore { tool, tool_input, .. } => {
            assert_eq!(tool, "EnterPlanMode");
            assert!(tool_input.is_none());
        }
        other => panic!("expected ToolBefore, got {other:?}"),
    }
}

#[test]
fn notification_missing_type_returns_none() {
    let event = parse_hook_line(r#"{"event":"notification","data":{}}"#);
    assert!(event.is_none());
}

#[test]
fn pre_tool_use_missing_tool_name_returns_empty() {
    let (event, _raw) =
        parse_hook_line(r#"{"event":"pre_tool_use","data":{}}"#).expect("should parse");
    assert_eq!(
        event,
        HookEvent::ToolBefore { tool: "".to_string(), tool_input: None, tool_use_id: None }
    );
}

#[test]
fn parses_after_tool_event() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"after_tool","data":{"tool_name":"Bash","tool_input":{"command":"ls"},"tool_response":{"llmContent":"output"}}}"#,
    )
    .expect("should parse");
    match event {
        HookEvent::ToolAfter { tool, tool_input, .. } => {
            assert_eq!(tool, "Bash");
            let input = tool_input.expect("tool_input should be Some");
            assert_eq!(input["command"], "ls");
        }
        other => panic!("expected ToolAfter, got {other:?}"),
    }
}

#[test]
fn after_tool_missing_tool_name_returns_empty() {
    let (event, _raw) =
        parse_hook_line(r#"{"event":"after_tool","data":{}}"#).expect("should parse");
    assert_eq!(
        event,
        HookEvent::ToolAfter {
            tool: "".to_string(),
            tool_input: None,
            tool_use_id: None,
            duration_ms: None
        }
    );
}

#[test]
fn parses_pre_tool_use_tool_use_id() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"pre_tool_use","data":{"tool_name":"Bash","tool_use_id":"toolu_1"}}"#,
    )
    .expect("should parse");
    match event {
        HookEvent::ToolBefore { tool_use_id, .. } => {
            assert_eq!(tool_use_id, Some("toolu_1".to_string()));
        }
        other => panic!("expected ToolBefore, got {other:?}"),
    }
}

#[test]
fn parses_post_tool_use_tool_use_id_and_duration_ms() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"post_tool_use","data":{"tool_name":"Bash","tool_use_id":"toolu_1","duration_ms":1500}}"#,
    )
    .expect("should parse");
    match event {
        HookEvent::ToolAfter { tool_use_id, duration_ms, .. } => {
            assert_eq!(tool_use_id, Some("toolu_1".to_string()));
            assert_eq!(duration_ms, Some(1500));
        }
        other => panic!("expected ToolAfter, got {other:?}"),
    }
}

#[test]
fn parses_user_prompt_submit_event() {
    let (event, _raw) =
        parse_hook_line(r#"{"event":"user_prompt_submit","data":{"prompt":"Fix the bug"}}"#)
            .expect("should parse");
    assert_eq!(event, HookEvent::TurnStart);
}

#[test]
fn parses_user_prompt_submit_without_data() {
    let (event, _raw) = parse_hook_line(r#"{"event":"user_prompt_submit"}"#).expect("should parse");
    assert_eq!(event, HookEvent::TurnStart);
}

#[test]
fn parses_start_event() {
    let (event, _raw) = parse_hook_line(
        r#"{"event":"start","data":{"session_type":"init","session_id":"abc-123"}}"#,
    )
    .expect("should parse");
    assert_eq!(event, HookEvent::SessionStart);
}

#[test]
fn parses_start_event_without_data() {
    let (event, _raw) = parse_hook_line(r#"{"event":"start"}"#).expect("should parse");
    assert_eq!(event, HookEvent::SessionStart);
}

#[test]
fn ignores_malformed_lines() {
    assert!(parse_hook_line("not json").is_none());
    assert!(parse_hook_line("{}").is_none());
    assert!(parse_hook_line(r#"{"event":"unknown_event"}"#).is_none());
    assert!(parse_hook_line("").is_none());
}

#[test]
fn raw_json_is_preserved() {
    let line = r#"{"event":"stop","data":{"hook_event_name":"Stop","stop_hook_active":false}}"#;
    let (event, raw_json) = parse_hook_line(line).expect("should parse");
    assert_eq!(event, HookEvent::TurnEnd);
    assert_eq!(raw_json["event"], "stop");
    assert_eq!(raw_json["data"]["hook_event_name"], "Stop");
}

#[test]
fn creates_pipe_and_cleans_up() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let pipe_path = dir.path().join("test.pipe");

    {
        let recv = HookReceiver::new(&pipe_path)?;
        assert!(pipe_path.exists());
        assert_eq!(recv.pipe_path(), pipe_path);
    }
    // Drop should remove the pipe
    assert!(!pipe_path.exists());
    Ok(())
}

#[tokio::test]
async fn reads_event_from_pipe() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let pipe_path = dir.path().join("hook.pipe");

    let mut recv = HookReceiver::new(&pipe_path)?;

    // Write to the pipe from another task
    let pipe = pipe_path.clone();
    tokio::spawn(async move {
        // Small delay to let the reader open first
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        // Open for write explicitly (tokio::fs::write uses create+truncate which
        // doesn't work on FIFOs)
        let mut file = match tokio::fs::OpenOptions::new().write(true).open(&pipe).await {
            Ok(f) => f,
            Err(_) => return,
        };
        use tokio::io::AsyncWriteExt;
        let _ = file.write_all(b"{\"event\":\"stop\",\"data\":{}}\n").await;
    });

    let (event, _raw) = recv.next_event().await.expect("should receive event");
    assert_eq!(event, HookEvent::TurnEnd);
    Ok(())
}
