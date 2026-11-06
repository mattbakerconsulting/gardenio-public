// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use crate::driver::AgentState;
use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx, StubNudgeEncoder};
use crate::transport::ws::{
    handle_client_message, ClientMessage, ServerMessage, SubscriptionFlags,
};

#[test]
fn ping_pong_serialization() -> anyhow::Result<()> {
    let msg = ClientMessage::Ping {};
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"ping\""));

    let pong = ServerMessage::Pong {};
    let json = serde_json::to_string(&pong).anyhow()?;
    assert!(json.contains("\"event\":\"pong\""));
    Ok(())
}

#[test]
fn screen_request_serialization() -> anyhow::Result<()> {
    let msg = ClientMessage::GetScreen { cursor: false };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"screen:get\""));
    Ok(())
}

#[test]
fn output_message_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Pty { data: "aGVsbG8=".to_owned(), offset: 0 };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"pty\""));
    assert!(json.contains("\"data\":\"aGVsbG8=\""));

    // Backwards compat: "output" alias deserializes to Pty
    let back: ServerMessage =
        serde_json::from_str(r#"{"event":"output","data":"aGVsbG8=","offset":0}"#)?;
    assert!(matches!(back, ServerMessage::Pty { .. }));
    Ok(())
}

#[test]
fn state_change_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Transition {
        prev: "working".to_owned(),
        next: "idle".to_owned(),
        seq: 42,
        prompt: Box::new(None),
        error_detail: None,
        error_category: None,
        parked_reason: None,
        resume_at_epoch_ms: None,
        cause: String::new(),
        last_message: None,
    };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"transition\""));
    assert!(json.contains("\"prev\":\"working\""));
    assert!(json.contains("\"next\":\"idle\""));
    // Error fields should be absent (skip_serializing_if = None)
    assert!(!json.contains("error_detail"), "json: {json}");
    assert!(!json.contains("error_category"), "json: {json}");
    // Cause should be absent when empty (skip_serializing_if)
    assert!(!json.contains("cause"), "json: {json}");
    // last_message should be absent when None
    assert!(!json.contains("last_message"), "json: {json}");
    Ok(())
}

#[test]
fn state_change_with_error_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Transition {
        prev: "working".to_owned(),
        next: "error".to_owned(),
        seq: 5,
        prompt: Box::new(None),
        error_detail: Some("rate_limit_error".to_owned()),
        error_category: Some("rate_limited".to_owned()),
        parked_reason: None,
        resume_at_epoch_ms: None,
        cause: "log:error".to_owned(),
        last_message: None,
    };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"transition\""));
    assert!(json.contains("\"next\":\"error\""));
    assert!(json.contains("\"error_detail\":\"rate_limit_error\""), "json: {json}");
    assert!(json.contains("\"error_category\":\"rate_limited\""), "json: {json}");
    Ok(())
}

#[test]
fn subscription_flags_default_is_empty() {
    let flags = SubscriptionFlags::default();
    assert!(!flags.pty);
    assert!(!flags.screen);
    assert!(!flags.state);
    assert!(!flags.hooks);
    assert!(!flags.messages);
}

#[test]
fn subscription_flags_parse_individual() {
    let flags = SubscriptionFlags::parse("pty");
    assert!(flags.pty);
    assert!(!flags.screen);
    assert!(!flags.state);

    // "output" is a backwards-compat alias for "pty"
    let flags = SubscriptionFlags::parse("output");
    assert!(flags.pty);

    let flags = SubscriptionFlags::parse("state");
    assert!(!flags.pty);
    assert!(flags.state);

    let flags = SubscriptionFlags::parse("hooks");
    assert!(flags.hooks);
    assert!(!flags.messages);
}

#[test]
fn subscription_flags_parse_combined() {
    let flags = SubscriptionFlags::parse("output,screen,state");
    assert!(flags.pty);
    assert!(flags.screen);
    assert!(flags.state);
    assert!(!flags.hooks);
    assert!(!flags.messages);
}

#[test]
fn subscription_flags_parse_with_hooks_and_messages() {
    let flags = SubscriptionFlags::parse("output,state,hooks,messages");
    assert!(flags.pty);
    assert!(!flags.screen);
    assert!(flags.state);
    assert!(flags.hooks);
    assert!(flags.messages);
}

#[test]
fn subscription_flags_ignores_unknown() {
    let flags = SubscriptionFlags::parse("output,unknown,state");
    assert!(flags.pty);
    assert!(flags.state);
    assert!(!flags.screen);
}

#[test]
fn error_message_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Error {
        code: "BAD_REQUEST".to_owned(),
        message: "invalid input".to_owned(),
    };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"error\""));
    assert!(json.contains("\"code\":\"BAD_REQUEST\""));
    Ok(())
}

#[test]
fn exit_message_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Exit { code: Some(0), signal: None };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"exit\""));
    assert!(json.contains("\"code\":0"));
    Ok(())
}

#[test]
fn replay_message_serialization() -> anyhow::Result<()> {
    let msg = ClientMessage::GetReplay { offset: 1024, limit: None };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"replay:get\""));
    assert!(json.contains("\"offset\":1024"));

    // With limit
    let msg = ClientMessage::GetReplay { offset: 0, limit: Some(100) };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"limit\":100"));
    Ok(())
}

#[test]
fn auth_message_serialization() -> anyhow::Result<()> {
    let msg = ClientMessage::Auth { token: "secret123".to_owned() };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"auth\""));
    assert!(json.contains("\"token\":\"secret123\""));
    Ok(())
}

#[test]
fn client_message_roundtrip() -> anyhow::Result<()> {
    let messages = vec![
        r#"{"event":"input:send","text":"hello"}"#,
        r#"{"event":"input:send","text":"hello","enter":true}"#,
        r#"{"event":"input:send:raw","data":"aGVsbG8="}"#,
        r#"{"event":"keys:send","keys":["Enter"]}"#,
        r#"{"event":"mouse:send","button":"left","col":3,"row":4,"release":true}"#,
        r#"{"event":"resize","cols":200,"rows":50}"#,
        r#"{"event":"agent:get"}"#,
        r#"{"event":"status:get"}"#,
        r#"{"event":"screen:get"}"#,
        r#"{"event":"replay:get","offset":0}"#,
        r#"{"event":"nudge","message":"fix bug"}"#,
        r#"{"event":"respond","accept":true}"#,
        r#"{"event":"auth","token":"tok"}"#,
        r#"{"event":"signal:send","signal":"SIGINT"}"#,
        r#"{"event":"shutdown"}"#,
        r#"{"event":"health:get"}"#,
        r#"{"event":"ready:get"}"#,
        r#"{"event":"stop:config:get"}"#,
        r#"{"event":"stop:config:put","config":{"mode":"allow"}}"#,
        r#"{"event":"config:start:get"}"#,
        r#"{"event":"config:put:get","config":{}}"#,
        r#"{"event":"stop:resolve","body":{"ok":true}}"#,
        r#"{"event":"ping"}"#,
    ];

    for json in messages {
        let _msg: ClientMessage = serde_json::from_str(json)
            .map_err(|e| anyhow::anyhow!("failed to parse '{json}': {e}"))?;
    }
    Ok(())
}

#[test]
fn shutdown_message_serialization() -> anyhow::Result<()> {
    let msg = ClientMessage::Shutdown {};
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"shutdown\""));

    // Roundtrip
    let _: ClientMessage = serde_json::from_str(r#"{"event":"shutdown"}"#)
        .map_err(|e| anyhow::anyhow!("failed to parse shutdown: {e}"))?;
    Ok(())
}

fn ws_test_state(agent: AgentState) -> StoreCtx {
    StoreBuilder::new()
        .child_pid(1234)
        .agent_state(agent)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build()
}

#[tokio::test]
async fn state_request_returns_agent_state() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new()
        .child_pid(1234)
        .agent_state(AgentState::Error { detail: "authentication_error".to_owned() })
        .build();
    // Populate error fields as session loop would.
    *state.driver.error.write().await = Some(crate::transport::state::ErrorInfo {
        detail: "authentication_error".to_owned(),
        category: crate::driver::ErrorCategory::Unauthorized,
    });

    let msg = ClientMessage::GetAgent {};
    let reply = handle_client_message(&state, msg, "test-client", &mut true).await;
    match reply {
        Some(ServerMessage::Agent {
            agent,
            state: st,
            since_seq: _,
            screen_seq: _,
            detection_tier,
            error_detail,
            error_category,
            ..
        }) => {
            assert!(!agent.is_empty());
            assert_eq!(st, "error");
            assert!(!detection_tier.is_empty());
            assert_eq!(error_detail.as_deref(), Some("authentication_error"));
            assert_eq!(error_category.as_deref(), Some("unauthorized"));
        }
        other => anyhow::bail!("expected AgentState, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn resize_zero_cols_returns_error() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg = ClientMessage::Resize { cols: 0, rows: 24 };
    let reply = handle_client_message(&state, msg, "test-client", &mut true).await;
    match reply {
        Some(ServerMessage::Error { code, .. }) => {
            assert_eq!(code, "BAD_REQUEST");
        }
        other => anyhow::bail!("expected Error, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn resize_zero_rows_returns_error() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg = ClientMessage::Resize { cols: 80, rows: 0 };
    let reply = handle_client_message(&state, msg, "test-client", &mut true).await;
    match reply {
        Some(ServerMessage::Error { code, .. }) => {
            assert_eq!(code, "BAD_REQUEST");
        }
        other => anyhow::bail!("expected Error, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn nudge_delivered_when_agent_working() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    state.ready.store(true, std::sync::atomic::Ordering::Release);
    let client_id = "test-ws";

    let msg = ClientMessage::Nudge { message: "hello".to_owned() };
    let reply = handle_client_message(&state, msg, client_id, &mut true).await;
    match reply {
        Some(ServerMessage::Nudged { delivered, state_before, reason }) => {
            assert!(delivered);
            assert_eq!(state_before.as_deref(), Some("working"));
            assert!(reason.is_none());
        }
        other => anyhow::bail!("expected NudgeResult, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn nudge_accepted_when_agent_waiting() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Idle);
    state.ready.store(true, std::sync::atomic::Ordering::Release);
    let client_id = "test-ws";

    let msg = ClientMessage::Nudge { message: "hello".to_owned() };
    let reply = handle_client_message(&state, msg, client_id, &mut true).await;
    match reply {
        Some(ServerMessage::Nudged { delivered, state_before, reason }) => {
            assert!(delivered);
            assert_eq!(state_before.as_deref(), Some("idle"));
            assert!(reason.is_none());
        }
        other => anyhow::bail!("expected NudgeResult with delivered=true, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn shutdown_cancels_token() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    assert!(!state.lifecycle.shutdown.is_cancelled());

    let msg = ClientMessage::Shutdown {};
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::Shutdown { accepted }) => assert!(accepted),
        other => anyhow::bail!("expected Shutdown, got {other:?}"),
    }
    assert!(state.lifecycle.shutdown.is_cancelled());
    Ok(())
}

#[tokio::test]
async fn shutdown_requires_auth() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);

    let msg = ClientMessage::Shutdown {};
    let reply = handle_client_message(&state, msg, "test-ws", &mut false).await;
    match reply {
        Some(ServerMessage::Error { code, .. }) => {
            assert_eq!(code, "UNAUTHORIZED");
        }
        other => anyhow::bail!("expected Unauthorized error, got {other:?}"),
    }
    assert!(!state.lifecycle.shutdown.is_cancelled());
    Ok(())
}

#[tokio::test]
async fn read_operations_require_auth() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    for msg in [
        ClientMessage::GetScreen { cursor: false },
        ClientMessage::GetAgent {},
        ClientMessage::GetStatus {},
        ClientMessage::GetReplay { offset: 0, limit: None },
    ] {
        let reply = handle_client_message(&state, msg, "test-ws", &mut false).await;
        match reply {
            Some(ServerMessage::Error { code, .. }) => assert_eq!(code, "UNAUTHORIZED"),
            other => anyhow::bail!("expected Unauthorized, got {other:?}"),
        }
    }
    Ok(())
}

#[tokio::test]
async fn signal_delivers_sigint() -> anyhow::Result<()> {
    let StoreCtx { store: state, input_rx: mut rx, .. } = ws_test_state(AgentState::Working);
    let client_id = "test-ws";

    let msg = ClientMessage::SendSignal { signal: "SIGINT".to_owned() };
    let reply = handle_client_message(&state, msg, client_id, &mut true).await;
    match reply {
        Some(ServerMessage::SignalSent { delivered }) => assert!(delivered),
        other => anyhow::bail!("expected SignalResult, got {other:?}"),
    }

    let event = rx.recv().await;
    assert!(
        matches!(event, Some(crate::event::InputEvent::Signal(crate::event::PtySignal::Int))),
        "expected Signal(Int), got {event:?}"
    );
    Ok(())
}

#[tokio::test]
async fn signal_rejects_unknown() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let client_id = "test-ws";

    let msg = ClientMessage::SendSignal { signal: "SIGFOO".to_owned() };
    let reply = handle_client_message(&state, msg, client_id, &mut true).await;
    match reply {
        Some(ServerMessage::Error { code, .. }) => {
            assert_eq!(code, "BAD_REQUEST");
        }
        other => anyhow::bail!("expected BadRequest error, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn keys_rejects_unknown_key() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let client_id = "test-ws";

    let msg = ClientMessage::SendKeys { keys: vec!["Enter".to_owned(), "SuperKey".to_owned()] };
    let reply = handle_client_message(&state, msg, client_id, &mut true).await;
    match reply {
        Some(ServerMessage::Error { code, message }) => {
            assert_eq!(code, "BAD_REQUEST");
            assert!(message.contains("SuperKey"), "message should mention the bad key: {message}");
        }
        other => anyhow::bail!("expected BadRequest error, got {other:?}"),
    }
    Ok(())
}

#[test]
fn signal_message_serialization() -> anyhow::Result<()> {
    let msg = ClientMessage::SendSignal { signal: "SIGTERM".to_owned() };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"signal:send\""));
    assert!(json.contains("\"signal\":\"SIGTERM\""));
    Ok(())
}

#[test]
fn nudge_result_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Nudged {
        delivered: false,
        state_before: Some("working".to_owned()),
        reason: Some("agent is working".to_owned()),
    };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"nudged\""));
    assert!(json.contains("\"delivered\":false"));
    assert!(json.contains("\"state_before\":\"working\""));
    assert!(json.contains("\"reason\":\"agent is working\""));
    Ok(())
}

#[test]
fn nudge_result_omits_none_fields() -> anyhow::Result<()> {
    let msg = ServerMessage::Nudged {
        delivered: true,
        state_before: Some("idle".to_owned()),
        reason: None,
    };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"delivered\":true"));
    assert!(!json.contains("reason"), "json: {json}");
    Ok(())
}

#[test]
fn respond_result_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Response {
        delivered: true,
        prompt_type: Some("permission".to_owned()),
        reason: None,
    };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"response\""));
    assert!(json.contains("\"delivered\":true"));
    assert!(json.contains("\"prompt_type\":\"permission\""));
    Ok(())
}

#[test]
fn status_message_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Status {
        session_id: "test-id".to_owned(),
        state: "running".to_owned(),
        pid: Some(1234),
        uptime_secs: 60,
        exit_code: None,
        screen_seq: 42,
        bytes_read: 1024,
        bytes_written: 512,
        ws_clients: 2,
    };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"status\""));
    assert!(json.contains("\"state\":\"running\""));
    assert!(json.contains("\"pid\":1234"));
    assert!(json.contains("\"uptime_secs\":60"));
    Ok(())
}

#[test]
fn status_request_serialization() -> anyhow::Result<()> {
    let msg = ClientMessage::GetStatus {};
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"status:get\""));
    Ok(())
}

#[test]
fn input_with_enter_serialization() -> anyhow::Result<()> {
    let msg = ClientMessage::SendInput { text: "hello".to_owned(), enter: true };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"enter\":true"));

    // enter defaults to false when omitted
    let parsed: ClientMessage = serde_json::from_str(r#"{"event":"input:send","text":"hello"}"#)
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    match parsed {
        ClientMessage::SendInput { enter, .. } => assert!(!enter),
        other => anyhow::bail!("expected Input, got {other:?}"),
    }
    Ok(())
}

#[test]
fn input_result_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::InputSent { bytes_written: 5 };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"input:sent\""));
    assert!(json.contains("\"bytes_written\":5"));
    Ok(())
}

#[test]
fn resize_result_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Resized { cols: 120, rows: 40 };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"resized\""));
    assert!(json.contains("\"cols\":120"));
    assert!(json.contains("\"rows\":40"));
    Ok(())
}

#[test]
fn signal_result_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::SignalSent { delivered: true };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"signal:sent\""));
    assert!(json.contains("\"delivered\":true"));
    Ok(())
}

#[test]
fn shutdown_result_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::Shutdown { accepted: true };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"shutdown\""));
    assert!(json.contains("\"accepted\":true"));
    Ok(())
}

#[tokio::test]
async fn screen_request_excludes_cursor_by_default() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg: ClientMessage = serde_json::from_str(r#"{"event":"screen:get"}"#)?;
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::Screen { cursor, .. }) => {
            assert!(cursor.is_none(), "cursor should be excluded by default");
        }
        other => anyhow::bail!("expected Screen, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn screen_request_includes_cursor_when_requested() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg: ClientMessage = serde_json::from_str(r#"{"event":"screen:get","cursor":true}"#)?;
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::Screen { cursor, .. }) => {
            assert!(cursor.is_some(), "cursor should be included when requested");
        }
        other => anyhow::bail!("expected Screen, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn input_raw_rejects_bad_base64() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg = ClientMessage::SendInputRaw { data: "not-valid-base64!!!".to_owned() };
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::Error { code, message }) => {
            assert_eq!(code, "BAD_REQUEST");
            assert!(message.contains("base64"), "message: {message}");
        }
        other => anyhow::bail!("expected Error, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn health_request_returns_health() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg = ClientMessage::GetHealth {};
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::Health { status, .. }) => {
            assert_eq!(status, "running");
        }
        other => anyhow::bail!("expected Health, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn ready_request_returns_ready() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg = ClientMessage::GetReady {};
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::Ready { ready }) => {
            assert!(!ready, "default ready is false");
        }
        other => anyhow::bail!("expected Ready, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn get_stop_config_requires_auth() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg = ClientMessage::GetStopConfig {};
    let reply = handle_client_message(&state, msg, "test-ws", &mut false).await;
    match reply {
        Some(ServerMessage::Error { code, .. }) => assert_eq!(code, "UNAUTHORIZED"),
        other => anyhow::bail!("expected Unauthorized, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn stop_config_roundtrip() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);

    // Read default config.
    let msg = ClientMessage::GetStopConfig {};
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::StopConfig { config }) => {
            assert_eq!(config["mode"], "allow");
        }
        other => anyhow::bail!("expected StopConfig, got {other:?}"),
    }

    // Update config.
    let msg = ClientMessage::PutStopConfig {
        config: serde_json::json!({"mode": "auto", "prompt": "wait"}),
    };
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::StopConfigured { updated }) => assert!(updated),
        other => anyhow::bail!("expected ConfigUpdated, got {other:?}"),
    }

    // Verify update.
    let msg = ClientMessage::GetStopConfig {};
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::StopConfig { config }) => {
            assert_eq!(config["mode"], "auto");
        }
        other => anyhow::bail!("expected StopConfig, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn resolve_stop_stores_signal() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let msg = ClientMessage::ResolveStop { body: serde_json::json!({"done": true}) };
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::StopResolved { accepted }) => assert!(accepted),
        other => anyhow::bail!("expected StopResult, got {other:?}"),
    }
    assert!(state.stop.signaled.load(std::sync::atomic::Ordering::Acquire));
    Ok(())
}

#[test]
fn hook_raw_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::HookRaw { data: serde_json::json!({"event": "post_tool_use"}) };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"hook:raw\""), "json: {json}");
    assert!(json.contains("\"data\""), "json: {json}");
    Ok(())
}

#[test]
fn message_raw_serialization() -> anyhow::Result<()> {
    let msg = ServerMessage::MessageRaw {
        data: serde_json::json!({"type": "assistant"}),
        source: "stdout".to_owned(),
    };
    let json = serde_json::to_string(&msg).anyhow()?;
    assert!(json.contains("\"event\":\"message:raw\""), "json: {json}");
    assert!(json.contains("\"source\":\"stdout\""), "json: {json}");
    Ok(())
}

#[tokio::test]
async fn start_config_roundtrip() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);

    // Update start config.
    let msg = ClientMessage::PutStartConfig {
        config: serde_json::json!({"text": "hello", "shell": ["echo hi"]}),
    };
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::StartConfigured { updated }) => assert!(updated),
        other => anyhow::bail!("expected ConfigUpdated, got {other:?}"),
    }

    // Verify.
    let msg = ClientMessage::GetStartConfig {};
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::StartConfig { config }) => {
            assert_eq!(config["text"], "hello");
        }
        other => anyhow::bail!("expected StartConfig, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn replay_returns_next_offset_for_ring_data() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    let data = b"hello world from the PTY";
    state.terminal.ring.write().await.write(data);

    let msg = ClientMessage::GetReplay { offset: 0, limit: None };
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::Replay { offset, next_offset, total_written, .. }) => {
            assert_eq!(offset, 0);
            assert_eq!(next_offset, data.len() as u64);
            assert_eq!(total_written, data.len() as u64);
        }
        other => anyhow::bail!("expected Replay, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn replay_next_offset_advances_past_previous() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);

    // Simulate initial pty output tracked by the connection loop.
    let initial = b"first chunk";
    state.terminal.ring.write().await.write(initial);
    let mut next_offset: u64 = initial.len() as u64;

    // More data arrives (e.g., agent response after user input).
    let extra = b" second chunk";
    state.terminal.ring.write().await.write(extra);

    // Client requests full replay from offset 0 (as expanded terminal does).
    let msg = ClientMessage::GetReplay { offset: 0, limit: None };
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match &reply {
        Some(ServerMessage::Replay { next_offset: replay_next, .. }) => {
            // Apply the same logic as handle_connection: advance next_offset.
            if *replay_next > next_offset {
                next_offset = *replay_next;
            }
        }
        other => anyhow::bail!("expected Replay, got {other:?}"),
    }

    // next_offset should now cover all data, preventing duplicate pty events.
    let total = (initial.len() + extra.len()) as u64;
    assert_eq!(next_offset, total);
    Ok(())
}

#[tokio::test]
async fn screen_request_names_the_mouse_modes() -> anyhow::Result<()> {
    // The Glass relay sets these on its terminal after each reset, since the ring it replays rarely
    // still holds the bytes that set them (gd-bftIGCMNVc).
    let StoreCtx { store: state, .. } = ws_test_state(AgentState::Working);
    state
        .terminal
        .screen
        .write()
        .await
        .feed(b"\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h");
    let msg: ClientMessage = serde_json::from_str(r#"{"event":"screen:get"}"#)?;
    let reply = handle_client_message(&state, msg, "test-ws", &mut true).await;
    match reply {
        Some(ServerMessage::Screen { alt_screen, mouse_tracking, mouse_sgr, .. }) => {
            assert!(alt_screen);
            assert_eq!(mouse_tracking, 1003);
            assert!(mouse_sgr);
        }
        other => anyhow::bail!("expected Screen, got {other:?}"),
    }
    let json = serde_json::to_string(&ServerMessage::Screen {
        lines: vec![],
        cols: 80,
        rows: 24,
        alt_screen: true,
        mouse_tracking: 1003,
        mouse_sgr: true,
        cursor: None,
        seq: 1,
    })?;
    assert!(json.contains("\"mouse_tracking\":1003"), "{json}");
    assert!(json.contains("\"mouse_sgr\":true"), "{json}");
    Ok(())
}
