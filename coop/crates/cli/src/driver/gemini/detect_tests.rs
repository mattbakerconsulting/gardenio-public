// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use bytes::Bytes;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::driver::{AgentState, Detector};

#[tokio::test]
async fn stdout_detector_parses_gemini_stream_json() -> anyhow::Result<()> {
    let (bytes_tx, bytes_rx) = mpsc::channel(32);
    let detector = Box::new(super::new_stdout_detector(bytes_rx, None, None, None));
    assert_eq!(detector.tier(), 3);

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let shutdown_clone = shutdown.clone();

    let handle = tokio::spawn(async move {
        detector.run(state_tx, shutdown_clone).await;
    });

    // Send a tool_use event as raw bytes
    bytes_tx
        .send(Bytes::from(
            "{\"type\":\"tool_use\",\"tool_name\":\"Bash\",\"tool_id\":\"bash-1\",\"parameters\":{\"command\":\"ls\"},\"timestamp\":\"2025-10-10T12:00:00.000Z\"}\n",
        ))
        .await?;

    let state = tokio::time::timeout(std::time::Duration::from_secs(5), state_rx.recv()).await;

    shutdown.cancel();
    let _ = handle.await;

    match state {
        Ok(Some((AgentState::Working, _cause, _))) => {}
        other => anyhow::bail!("expected Working, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn stdout_detector_detects_result_as_idle() -> anyhow::Result<()> {
    let (bytes_tx, bytes_rx) = mpsc::channel(32);
    let detector = Box::new(super::new_stdout_detector(bytes_rx, None, None, None));

    let (state_tx, mut state_rx) = mpsc::channel(32);
    let shutdown = CancellationToken::new();
    let shutdown_clone = shutdown.clone();

    let handle = tokio::spawn(async move {
        detector.run(state_tx, shutdown_clone).await;
    });

    bytes_tx
        .send(Bytes::from(
            "{\"type\":\"result\",\"status\":\"success\",\"timestamp\":\"2025-10-10T12:00:00.000Z\"}\n",
        ))
        .await?;

    let state = tokio::time::timeout(std::time::Duration::from_secs(5), state_rx.recv()).await;

    shutdown.cancel();
    let _ = handle.await;

    match state {
        Ok(Some((AgentState::Idle, _cause, _))) => {}
        other => anyhow::bail!("expected Idle, got {other:?}"),
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
    let expected_count = events_owned.len();
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
    let timeout = tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some((state, _cause, _)) = state_rx.recv().await {
            states.push(state);
            if states.len() >= expected_count {
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
async fn hook_detector_before_agent() -> anyhow::Result<()> {
    let states =
        run_hook_detector(vec![r#"{"event":"before_agent","data":{"prompt":"Fix the bug"}}"#])
            .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Working));
    Ok(())
}

#[tokio::test]
async fn hook_detector_after_tool() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![
        r#"{"event":"after_tool","data":{"tool_name":"Bash","tool_input":{"command":"ls"},"tool_response":{"llmContent":"output"}}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Working));
    Ok(())
}

#[tokio::test]
async fn hook_detector_session_end() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![r#"{"event":"session_end"}"#]).await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Idle));
    Ok(())
}

#[tokio::test]
async fn hook_detector_notification_tool_permission() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![
        r#"{"event":"notification","data":{"notification_type":"ToolPermission","message":"Allow Bash?","details":"command: rm -rf"}}"#,
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
async fn hook_detector_pre_tool_use_maps_to_working() -> anyhow::Result<()> {
    let states = run_hook_detector(vec![
        r#"{"event":"pre_tool_use","data":{"tool_name":"Bash","tool_input":{"command":"ls"}}}"#,
    ])
    .await?;

    assert_eq!(states.len(), 1);
    assert!(matches!(states[0], AgentState::Working));
    Ok(())
}

mod acp_stream {
    use std::sync::Arc;

    use bytes::Bytes;
    use serde_json::Value;
    use tokio::sync::{mpsc, RwLock};
    use tokio_util::sync::CancellationToken;

    use crate::driver::gemini::acp::{AcpClient, ClientConfig};
    use crate::driver::{AgentState, Detector};
    use crate::usage::UsageState;

    const BASIC: &str = include_str!("../../../../../tests/fixtures/gemini/acp/basic.jsonl");

    fn agent_frames() -> Vec<Value> {
        BASIC
            .lines()
            .filter_map(|l| serde_json::from_str::<Value>(l).ok())
            .filter(|l| l["dir"] == "<-")
            .map(|l| l["frame"].clone())
            .collect()
    }

    /// What the ACP backend would tee for the captured `say pong` session: every
    /// agent frame, plus the client's synthetic notifications, in order.
    fn teed_session() -> Vec<Value> {
        let frames = agent_frames();
        let mut client = AcpClient::new(ClientConfig { cwd: "/work".into(), ..Default::default() });
        client.start();
        let mut tee = Vec::new();
        let feed = |client: &mut AcpClient, frame: &Value, tee: &mut Vec<Value>| {
            let r = client.on_message(frame);
            if r.forward {
                tee.push(frame.clone());
            }
            tee.extend(r.synthetic);
            r.send
        };
        // initialize, authenticate, session/new responses.
        for frame in frames.iter().take(3) {
            feed(&mut client, frame, &mut tee);
        }
        let submit = client.submit("say pong".into());
        tee.extend(submit.synthetic);
        let id = submit.send[0]["id"].clone();
        for frame in frames.iter().skip(3) {
            let mut frame = frame.clone();
            if frame["id"] == 4 {
                frame["id"] = id.clone();
            }
            feed(&mut client, &frame, &mut tee);
        }
        tee
    }

    #[tokio::test]
    async fn a_captured_acp_turn_drives_state_message_and_usage() -> anyhow::Result<()> {
        let last_message = Arc::new(RwLock::new(None));
        let usage = Arc::new(UsageState::new());
        let (bytes_tx, bytes_rx) = mpsc::channel(512);
        let detector = Box::new(super::super::new_stdout_detector(
            bytes_rx,
            Some(Arc::clone(&last_message)),
            None,
            Some(Arc::clone(&usage)),
        ));
        let (state_tx, mut state_rx) = mpsc::channel(512);
        let handle = tokio::spawn(detector.run(state_tx, CancellationToken::new()));
        for entry in teed_session() {
            bytes_tx.send(Bytes::from(format!("{entry}\n"))).await?;
        }
        drop(bytes_tx);
        handle.await?;

        let mut states = Vec::new();
        while let Ok((state, cause, _)) = state_rx.try_recv() {
            states.push((state, cause));
        }
        assert_eq!(states.first(), Some(&(AgentState::Idle, "acp:idle".to_owned())));
        assert!(states.iter().any(|(s, c)| *s == AgentState::Working && c == "acp:working"));
        assert_eq!(states.last(), Some(&(AgentState::Idle, "acp:idle".to_owned())));
        assert!(!states.iter().any(|(s, _)| matches!(s, AgentState::Error { .. })));

        assert_eq!(last_message.read().await.as_deref(), Some("pong"));
        let snap = usage.snapshot().await;
        assert_eq!((snap.input_tokens, snap.output_tokens, snap.request_count), (100, 3, 1));
        assert_eq!(snap.total_cost_usd, 0.0);
        Ok(())
    }
}
