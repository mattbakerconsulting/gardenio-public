// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! End-to-end integration tests exercising the Claude driver against claudeless,
//! a scenario-driven Claude CLI mock.
//!
//! Requires `claudeless` in PATH (install via `brew install alfredjeanlab/tap/claudeless`).

use std::path::PathBuf;
use std::process::Stdio;
use std::time::Duration;

use bytes::Bytes;
use coop::config::Config;
use coop::driver::{AgentState, QuestionAnswer};
use coop::event::{InputEvent, TransitionEvent};
use coop::run;
use coop::transport::{deliver_steps, encode_response};
use tokio::sync::broadcast;

/// Panics if `claudeless` is not installed.
fn expect_claudeless() {
    let ok = std::process::Command::new("claudeless")
        .arg("--version")
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false);
    assert!(
        ok,
        "claudeless not found in PATH — install via: brew install alfredjeanlab/tap/claudeless"
    );
}

fn scenario_path(name: &str) -> String {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/scenarios")
        .join(name)
        .display()
        .to_string()
}

fn claude_config(scenario: &str, prompt: &str) -> Config {
    Config {
        agent: None, // auto-detected from "claudeless" command
        command: vec![
            "claudeless".into(),
            "--scenario".into(),
            scenario_path(scenario),
            prompt.into(),
        ],
        ..Config::test()
    }
}

/// Wait for a state transition matching `pred`, with a 30s timeout.
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

#[tokio::test]
async fn claude_basic_session_lifecycle() -> anyhow::Result<()> {
    expect_claudeless();

    let prepared = run::prepare(claude_config("claude_hello.toml", "hello")).await?;
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    shutdown.cancel();
    handle.await??;

    Ok(())
}

#[tokio::test]
async fn claude_tool_use_session_lifecycle() -> anyhow::Result<()> {
    expect_claudeless();

    let prepared = run::prepare(claude_config("claude_tool_use.toml", "read a file")).await?;
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, |s| matches!(s, AgentState::Working)).await?;
    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    shutdown.cancel();
    handle.await??;

    Ok(())
}

#[tokio::test]
async fn claude_ask_user_session_lifecycle() -> anyhow::Result<()> {
    expect_claudeless();

    let prepared = run::prepare(claude_config("claude_ask_user.toml", "help me choose")).await?;
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let input_tx = prepared.store.channels.input_tx.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, |s| matches!(s, AgentState::Prompt { .. })).await?;

    // Emulate user selecting first option in the elicitation dialog.
    tokio::time::sleep(Duration::from_millis(200)).await;
    input_tx.send(InputEvent::Write(Bytes::from_static(b"1"))).await?;

    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    shutdown.cancel();
    handle.await??;

    Ok(())
}

#[tokio::test]
async fn claude_multi_question_session_lifecycle() -> anyhow::Result<()> {
    expect_claudeless();

    let prepared =
        run::prepare(claude_config("claude_multi_question.toml", "help me decide")).await?;
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let input_tx = prepared.store.channels.input_tx.clone();
    let handle = tokio::spawn(prepared.run());

    // Multi-question is a single dialog with tabs: Q1 → Q2 → Confirm.
    wait_for(&mut rx, |s| matches!(s, AgentState::Prompt { .. })).await?;

    // Answer first question (select option 1).
    tokio::time::sleep(Duration::from_millis(100)).await;
    input_tx.send(InputEvent::Write(Bytes::from_static(b"1"))).await?;

    // Answer second question (select option 2).
    tokio::time::sleep(Duration::from_millis(100)).await;
    input_tx.send(InputEvent::Write(Bytes::from_static(b"2"))).await?;

    // Confirm.
    tokio::time::sleep(Duration::from_millis(100)).await;
    input_tx.send(InputEvent::Write(Bytes::from_static(b"\r"))).await?;

    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    shutdown.cancel();
    handle.await??;

    Ok(())
}

/// After a claudeless session exits, verify that a session log is produced
/// and that `parse_resume_state` recovers the last known agent state.
#[tokio::test]
async fn claude_session_produces_resumable_log() -> anyhow::Result<()> {
    expect_claudeless();

    // Use a temporary config dir so we don't pollute the real one.
    let config_dir = tempfile::tempdir()?;
    std::env::set_var("CLAUDE_CONFIG_DIR", config_dir.path());

    let prepared = run::prepare(claude_config("claude_hello.toml", "hello")).await?;
    let session_dir = prepared.store.session_dir.clone();
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    shutdown.cancel();
    handle.await??;

    // 1. Verify coop-side session artifacts (event log files) were created.
    if let Some(ref dir) = session_dir {
        let state_events = dir.join("state_events.jsonl");
        assert!(
            state_events.exists(),
            "state_events.jsonl not found in session dir: {}",
            dir.display()
        );
    }

    // 2. Find the claudeless-generated session log.
    let projects_dir = config_dir.path().join("projects");
    let mut found_log: Option<std::path::PathBuf> = None;
    if projects_dir.is_dir() {
        for entry in std::fs::read_dir(&projects_dir)?.flatten() {
            if entry.path().is_dir() {
                for file in std::fs::read_dir(entry.path())?.flatten() {
                    if file.path().extension().and_then(|e| e.to_str()) == Some("jsonl") {
                        found_log = Some(file.path());
                        break;
                    }
                }
            }
        }
    }

    let log_path = found_log.expect("claudeless should produce a session log");

    // 3. Verify discover_session_log finds the session (no companion dir needed —
    //    Claude Code creates those lazily for subagents/tool-results only).
    let discovered =
        coop::driver::claude::resume::discover_session_log(log_path.to_str().unwrap())?;
    assert_eq!(discovered.as_ref(), Some(&log_path));

    // 5. Verify parse_resume_state recovers correct state.
    // The Tier 1 hook detector broadcasts Idle before claudeless finishes flushing
    // the assistant message to the session log. Poll until the log stabilizes with
    // the final Idle state instead of relying on a fixed sleep.
    let state = tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            let s = coop::driver::claude::resume::parse_resume_state(&log_path)?;
            if s.last_state == AgentState::Idle {
                return Ok::<_, anyhow::Error>(s);
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    })
    .await
    .map_err(|_| anyhow::anyhow!("timed out waiting for resume state to reach Idle"))??;
    assert_eq!(state.last_state, AgentState::Idle);
    assert!(state.log_offset > 0, "log offset should be > 0");

    std::env::remove_var("CLAUDE_CONFIG_DIR");
    Ok(())
}

/// After a claudeless session exits with recording enabled, verify all session
/// artifacts are produced: event log (state transitions), recording entries,
/// and the session directory structure.
#[tokio::test]
async fn claude_session_produces_all_artifacts() -> anyhow::Result<()> {
    expect_claudeless();

    let mut config = claude_config("claude_hello.toml", "hello");
    config.record = true;

    let prepared = run::prepare(config).await?;
    let session_dir = prepared.store.session_dir.clone();
    let record = prepared.store.record.clone();
    let event_log = prepared.store.event_log.clone();
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    // Give subscribers a moment to flush entries before shutdown.
    tokio::time::sleep(Duration::from_millis(100)).await;
    shutdown.cancel();
    handle.await??;

    // 1. Verify event log has state transitions.
    let state_events = event_log.catchup_state(0).await;
    assert!(!state_events.is_empty(), "should have at least one state transition");

    // 2. Verify recording is enabled and has entries.
    let rec_status = record.status();
    assert!(rec_status.enabled, "recording should be enabled");
    assert!(rec_status.entries > 0, "recording should have entries, got {}", rec_status.entries);

    // 3. Verify session_dir contains event log files.
    if let Some(ref dir) = session_dir {
        let state_file = dir.join("state_events.jsonl");
        assert!(state_file.exists(), "state_events.jsonl should exist in {}", dir.display());
        let content = std::fs::read_to_string(&state_file)?;
        assert!(!content.is_empty(), "state_events.jsonl should not be empty");
    } else {
        anyhow::bail!("session_dir should be set for a claudeless session");
    }

    Ok(())
}

/// Test Respond API with single-question answer.
#[tokio::test]
async fn claude_ask_user_respond_api() -> anyhow::Result<()> {
    expect_claudeless();

    let prepared = run::prepare(claude_config("claude_ask_user.toml", "help me choose")).await?;
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let app = prepared.store.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, |s| matches!(s, AgentState::Prompt { .. })).await?;
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Use encode_response with structured answer.
    let answers = vec![QuestionAnswer { option: Some(1), text: None }];
    let encoder = app.config.respond_encoder.as_ref().unwrap();
    let agent = app.driver.agent_state.read().await;
    let (steps, _count) = encode_response(&agent, encoder.as_ref(), None, None, None, &answers)
        .map_err(|e| anyhow::anyhow!("encode_response failed: {:?}", e))?;
    drop(agent);
    deliver_steps(&app.channels.input_tx, steps)
        .await
        .map_err(|e| anyhow::anyhow!("deliver_steps failed: {:?}", e))?;

    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    shutdown.cancel();
    handle.await??;

    Ok(())
}

/// Test Respond API with multi-question all-at-once answers.
#[tokio::test]
async fn claude_multi_question_respond_api() -> anyhow::Result<()> {
    expect_claudeless();

    let prepared =
        run::prepare(claude_config("claude_multi_question.toml", "help me decide")).await?;
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let app = prepared.store.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, |s| matches!(s, AgentState::Prompt { .. })).await?;
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Use encode_response with structured answers (all-at-once mode).
    let answers = vec![
        QuestionAnswer { option: Some(1), text: None },
        QuestionAnswer { option: Some(2), text: None },
    ];
    let encoder = app.config.respond_encoder.as_ref().unwrap();
    let agent = app.driver.agent_state.read().await;
    let (steps, count) = encode_response(&agent, encoder.as_ref(), None, None, None, &answers)
        .map_err(|e| anyhow::anyhow!("encode_response failed: {:?}", e))?;
    drop(agent);
    assert_eq!(count, 2);
    deliver_steps(&app.channels.input_tx, steps)
        .await
        .map_err(|e| anyhow::anyhow!("deliver_steps failed: {:?}", e))?;

    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    shutdown.cancel();
    handle.await??;

    Ok(())
}

#[tokio::test]
async fn claude_plan_mode_session_lifecycle() -> anyhow::Result<()> {
    expect_claudeless();

    let prepared = run::prepare(claude_config("claude_plan_mode.toml", "plan a feature")).await?;
    let mut rx = prepared.store.channels.state_tx.subscribe();
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let input_tx = prepared.store.channels.input_tx.clone();
    let handle = tokio::spawn(prepared.run());

    // EnterPlanMode → Working.
    wait_for(&mut rx, |s| matches!(s, AgentState::Working)).await?;

    // ExitPlanMode → PlanPrompt (user must approve).
    wait_for(&mut rx, |s| matches!(s, AgentState::Prompt { .. })).await?;
    tokio::time::sleep(Duration::from_millis(100)).await;
    input_tx.send(InputEvent::Write(Bytes::from_static(b"1"))).await?;

    wait_for(&mut rx, |s| matches!(s, AgentState::Idle)).await?;

    shutdown.cancel();
    handle.await??;

    Ok(())
}
