// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! End-to-end tests for the Gemini ACP driver against `fake-gemini.sh`, which
//! speaks the frames captured in the phase 0 spike (`coop/tests/fixtures/gemini/`).
//! No model, network or real `gemini` binary is involved.

use std::path::PathBuf;
use std::time::Duration;

use bytes::Bytes;
use coop::config::Config;
use coop::driver::{classify_error_detail, AgentState, ErrorCategory};
use coop::event::{InputEvent, TransitionEvent};
use coop::run;
use coop::transport::{deliver_steps, Store};
use serial_test::serial;
use tokio::sync::broadcast;

fn fake_gemini() -> String {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../tests/fixtures/gemini/fake-gemini.sh")
        .display()
        .to_string()
}

/// Point the fake at a fresh request log and return its path.
fn request_log(dir: &tempfile::TempDir) -> PathBuf {
    let log = dir.path().join("requests.log");
    std::env::set_var("FAKE_GEMINI_LOG", &log);
    log
}

fn gemini_config(resume: Option<&str>) -> Config {
    // The fake is not named `gemini`, so select the driver explicitly.
    Config {
        agent: Some("gemini".into()),
        command: vec![fake_gemini(), "--acp".into()],
        resume: resume.map(str::to_owned),
        ..Config::test()
    }
}

async fn wait_for(
    rx: &mut broadcast::Receiver<TransitionEvent>,
    pred: impl Fn(&AgentState) -> bool,
) -> anyhow::Result<TransitionEvent> {
    tokio::time::timeout(Duration::from_secs(30), async {
        loop {
            match rx.recv().await {
                Ok(e) if pred(&e.next) => return Ok(e),
                Ok(_) => continue,
                Err(e) => anyhow::bail!("state channel closed: {e}"),
            }
        }
    })
    .await
    .map_err(|_| anyhow::anyhow!("timed out waiting for expected state"))?
}

/// Deliver a nudge exactly as the HTTP handler does: encode, then write.
async fn nudge(store: &Store, message: &str) -> anyhow::Result<()> {
    let encoder = store
        .config
        .nudge_encoder
        .clone()
        .ok_or_else(|| anyhow::anyhow!("gemini driver has no nudge encoder"))?;
    deliver_steps(&store.channels.input_tx, encoder.encode(message))
        .await
        .map_err(|e| anyhow::anyhow!("nudge delivery failed: {e:?}"))
}

fn idle(s: &AgentState) -> bool {
    matches!(s, AgentState::Idle)
}

fn working(s: &AgentState) -> bool {
    matches!(s, AgentState::Working)
}

fn logged_methods(log: &std::path::Path) -> anyhow::Result<Vec<String>> {
    Ok(std::fs::read_to_string(log)?
        .lines()
        .filter_map(|l| serde_json::from_str::<serde_json::Value>(l).ok())
        .filter_map(|v| v["method"].as_str().map(str::to_owned))
        .collect())
}

#[tokio::test]
#[serial]
async fn acp_session_nudge_usage_last_message_and_interrupt() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log = request_log(&dir);

    let prepared = run::prepare(gemini_config(None)).await?;
    let store = prepared.store.clone();
    let mut rx = store.channels.state_tx.subscribe();
    let shutdown = store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    // The session opens: coop/ready makes the agent Idle.
    wait_for(&mut rx, idle).await?;

    // Turn 1: a nudge becomes session/prompt; the reply and usage come back.
    nudge(&store, "hello acp").await?;
    wait_for(&mut rx, working).await?;
    wait_for(&mut rx, idle).await?;
    assert_eq!(store.driver.last_message.read().await.as_deref(), Some("pong: hello acp"));
    let usage = store.usage.snapshot().await;
    assert_eq!((usage.input_tokens, usage.output_tokens, usage.request_count), (100, 3, 1));

    // Turn 2: never completes on its own; Escape becomes session/cancel.
    nudge(&store, "slow one").await?;
    wait_for(&mut rx, working).await?;
    store.channels.input_tx.send(InputEvent::Write(Bytes::from_static(b"\x1b"))).await?;
    wait_for(&mut rx, idle).await?;
    // The cancelled fragment is not the last message.
    assert_eq!(store.driver.last_message.read().await.as_deref(), Some("pong: hello acp"));

    // Turn 3: the same session accepts a nudge after the cancel.
    nudge(&store, "after cancel").await?;
    wait_for(&mut rx, working).await?;
    wait_for(&mut rx, idle).await?;
    assert_eq!(store.driver.last_message.read().await.as_deref(), Some("pong: after cancel"));

    shutdown.cancel();
    handle.await??;

    assert_eq!(
        logged_methods(&log)?,
        [
            "initialize",
            "authenticate",
            "session/new",
            "session/set_mode",
            "session/prompt",
            "session/prompt",
            "session/cancel",
            "session/prompt"
        ]
    );
    let requests = std::fs::read_to_string(&log)?;
    assert!(requests.contains(r#""modeId":"yolo""#), "{requests}");
    assert!(requests.contains(r#""methodId":"gemini-api-key""#), "{requests}");
    Ok(())
}

#[tokio::test]
#[serial]
async fn a_quota_failure_is_a_rate_limited_error_state() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    request_log(&dir);

    let prepared = run::prepare(gemini_config(None)).await?;
    let store = prepared.store.clone();
    let mut rx = store.channels.state_tx.subscribe();
    let shutdown = store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, idle).await?;
    nudge(&store, "please fail").await?;
    let failed = wait_for(&mut rx, |s| matches!(s, AgentState::Error { .. })).await?;
    let AgentState::Error { detail } = failed.next else { anyhow::bail!("not an error") };
    assert_eq!(classify_error_detail(&detail), ErrorCategory::RateLimited);

    shutdown.cancel();
    handle.await??;
    Ok(())
}

#[tokio::test]
#[serial]
async fn resume_loads_the_session_and_hides_the_replayed_history() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log = request_log(&dir);
    let session = "b70b593d-8f2c-4dba-84ab-1cce54577642";

    let prepared = run::prepare(gemini_config(Some(session))).await?;
    let store = prepared.store.clone();
    let mut rx = store.channels.state_tx.subscribe();
    let shutdown = store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    let first = wait_for(&mut rx, |_| true).await?;
    assert!(idle(&first.next), "replayed history must not read as work: {first:?}");
    assert_eq!(store.driver.last_message.read().await.as_deref(), None);

    shutdown.cancel();
    handle.await??;

    let methods = logged_methods(&log)?;
    assert_eq!(methods, ["initialize", "authenticate", "session/load", "session/set_mode"]);
    let requests = std::fs::read_to_string(&log)?;
    assert!(requests.contains(&format!(r#""sessionId":"{session}""#)), "{requests}");
    Ok(())
}
