// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! End-to-end tests for the Codex driver against `fake-codex.sh`, which replays
//! the spike fixtures (`coop/tests/fixtures/codex/`). No model, network or real
//! `codex` binary is involved.

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

fn fake_codex() -> String {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../tests/fixtures/codex/fake-codex.sh")
        .display()
        .to_string()
}

/// Point the fake at a fresh request log and return its path.
fn request_log(dir: &tempfile::TempDir) -> PathBuf {
    let log = dir.path().join("requests.log");
    std::env::set_var("FAKE_CODEX_LOG", &log);
    log
}

fn codex_config(args: &[&str]) -> Config {
    let mut command = vec![fake_codex()];
    command.extend(args.iter().map(|a| (*a).to_owned()));
    // The fake is not named `codex`, so select the driver explicitly.
    Config { agent: Some("codex".into()), command, ..Config::test() }
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
        .ok_or_else(|| anyhow::anyhow!("codex driver has no nudge encoder"))?;
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

#[tokio::test]
#[serial]
async fn app_server_session_nudge_interrupt_and_resume_of_work() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log = request_log(&dir);

    let prepared = run::prepare(codex_config(&["app-server"])).await?;
    let store = prepared.store.clone();
    let mut rx = store.channels.state_tx.subscribe();
    let shutdown = store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    // The thread opens: coop/ready makes the agent Idle (and the session ready).
    wait_for(&mut rx, idle).await?;

    // Turn 1: a nudge becomes turn/start; the capture replies and completes.
    nudge(&store, "hello app-server").await?;
    wait_for(&mut rx, working).await?;
    wait_for(&mut rx, idle).await?;

    // Turn 2: the capture's "slow one" never completes on its own...
    nudge(&store, "slow one").await?;
    wait_for(&mut rx, working).await?;
    // ...until Escape becomes turn/interrupt, which ends the turn `interrupted`.
    store.channels.input_tx.send(InputEvent::Write(Bytes::from_static(b"\x1b"))).await?;
    wait_for(&mut rx, idle).await?;

    // Turn 3: the same thread accepts a nudge after the interrupt.
    nudge(&store, "nudge after interrupt").await?;
    wait_for(&mut rx, working).await?;
    wait_for(&mut rx, idle).await?;

    assert_eq!(store.driver.last_message.read().await.as_deref(), Some("after interrupt reply"));

    shutdown.cancel();
    handle.await??;

    let requests = std::fs::read_to_string(&log)?;
    let methods: Vec<String> = requests
        .lines()
        .filter_map(|l| serde_json::from_str::<serde_json::Value>(l).ok())
        .filter_map(|v| v["method"].as_str().map(str::to_owned))
        .collect();
    assert_eq!(
        methods,
        [
            "initialize",
            "initialized",
            "thread/start",
            "turn/start",
            "turn/start",
            "turn/interrupt",
            "turn/start"
        ]
    );
    assert!(requests.contains(r#""approvalPolicy":"never""#), "{requests}");
    assert!(requests.contains(r#""sandbox":"danger-full-access""#), "{requests}");
    assert!(requests.contains(r#""text":"slow one""#), "{requests}");
    Ok(())
}

#[tokio::test]
#[serial]
async fn app_server_resume_opens_the_given_session() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log = request_log(&dir);

    let mut config = codex_config(&["app-server"]);
    config.resume = Some("01a13cb7-104f-7611-8852-5d9a6d492166".into());
    config.validate()?;
    let prepared = run::prepare(config).await?;
    let store = prepared.store.clone();
    let mut rx = store.channels.state_tx.subscribe();
    let shutdown = store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, idle).await?;
    shutdown.cancel();
    handle.await??;

    let requests = std::fs::read_to_string(&log)?;
    assert!(requests.contains(r#""method":"thread/resume""#), "{requests}");
    assert!(
        requests.contains(r#""threadId":"01a13cb7-104f-7611-8852-5d9a6d492166""#),
        "{requests}"
    );
    assert!(!requests.contains("thread/start"), "{requests}");
    Ok(())
}

#[tokio::test]
#[serial]
async fn exec_session_runs_a_turn_per_nudge_and_resumes_the_thread() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log = request_log(&dir);

    let prepared = run::prepare(codex_config(&["exec", "--yolo"])).await?;
    let store = prepared.store.clone();
    let mut rx = store.channels.state_tx.subscribe();
    let shutdown = store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, idle).await?;

    nudge(&store, "run the tool").await?;
    wait_for(&mut rx, working).await?;
    wait_for(&mut rx, idle).await?;
    assert_eq!(store.driver.last_message.read().await.as_deref(), Some("after gate done"));

    // The second turn must resume the thread the first one reported.
    nudge(&store, "and again").await?;
    wait_for(&mut rx, working).await?;
    wait_for(&mut rx, idle).await?;

    // Each turn's `turn.completed.usage` (300 in / 60 out in the fixture) is
    // accumulated into the session usage.
    let usage = store.usage.snapshot().await;
    assert_eq!((usage.input_tokens, usage.output_tokens, usage.request_count), (600, 120, 2));

    shutdown.cancel();
    handle.await??;

    let requests = std::fs::read_to_string(&log)?;
    let argv: Vec<&str> = requests.lines().filter(|l| l.starts_with("argv: ")).collect();
    assert_eq!(argv.len(), 2, "{requests}");
    assert!(argv[0].ends_with("exec --yolo --json -"), "{}", argv[0]);
    assert!(
        argv[1].ends_with("exec resume --yolo --json 01a13cb8-970e-74a2-b81f-f40294555aa8 -"),
        "{}",
        argv[1]
    );
    assert!(requests.contains("stdin: run the tool"), "{requests}");
    assert!(requests.contains("stdin: and again"), "{requests}");
    Ok(())
}

#[tokio::test]
#[serial]
async fn exec_failed_turn_is_an_unauthorized_error() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let _log = request_log(&dir);

    let prepared = run::prepare(codex_config(&["exec"])).await?;
    let store = prepared.store.clone();
    let mut rx = store.channels.state_tx.subscribe();
    let shutdown = store.lifecycle.shutdown.clone();
    let handle = tokio::spawn(prepared.run());

    wait_for(&mut rx, idle).await?;
    nudge(&store, "please fail").await?;
    let failed = wait_for(&mut rx, |s| matches!(s, AgentState::Error { .. })).await?;
    let AgentState::Error { detail } = failed.next else { anyhow::bail!("not an error") };
    assert_eq!(classify_error_detail(&detail), ErrorCategory::Unauthorized);

    shutdown.cancel();
    handle.await??;
    Ok(())
}

/// A credential switch on a Codex session writes the pushed ChatGPT login to
/// `$CODEX_HOME/auth.json` (0600, never into the child env) and hands gb init
/// the thread id to resume on the new account (gd-hoYFy5n8mh).
#[tokio::test]
#[serial]
async fn credential_switch_writes_auth_json_and_hands_over_the_thread_id() -> anyhow::Result<()> {
    use std::os::unix::fs::PermissionsExt;

    let dir = tempfile::tempdir()?;
    let _log = request_log(&dir);
    let codex_home = dir.path().join("codex-home");
    std::env::set_var("CODEX_HOME", &codex_home);
    let _ = std::fs::remove_file(run::CREDENTIAL_SWITCH_FILE);

    let prepared = run::prepare(codex_config(&["app-server"])).await?;
    let store = prepared.store.clone();
    let mut rx = store.channels.state_tx.subscribe();
    let handle = tokio::spawn(prepared.run());
    wait_for(&mut rx, idle).await?;

    // The rollout path (and so the thread id) is published asynchronously.
    tokio::time::timeout(Duration::from_secs(15), async {
        while store.switch.session_log_path.read().await.is_none() {
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .map_err(|_| anyhow::anyhow!("rollout path never resolved"))?;

    let doc = r#"{"auth_mode":"chatgpt","tokens":{"access_token":"at-new","refresh_token":"gardenio-broker-managed","account_id":"acct-2"}}"#;
    store
        .switch
        .switch_tx
        .send(coop::switch::SwitchRequest {
            credentials: Some(std::collections::HashMap::from([(
                "CODEX_AUTH_JSON".to_owned(),
                doc.to_owned(),
            )])),
            force: true,
            profile: Some("codex-b".into()),
        })
        .await?;

    let result = tokio::time::timeout(Duration::from_secs(30), handle).await??.map_err(|e| e)?;
    std::env::remove_var("CODEX_HOME");
    assert_eq!(result.status.code, Some(run::EXIT_CODE_CREDENTIAL_SWITCH));

    let auth = codex_home.join("auth.json");
    assert_eq!(std::fs::read_to_string(&auth)?, doc);
    assert_eq!(std::fs::metadata(&auth)?.permissions().mode() & 0o777, 0o600);

    let meta: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(run::CREDENTIAL_SWITCH_FILE)?)?;
    std::fs::remove_file(run::CREDENTIAL_SWITCH_FILE)?;
    assert_eq!(meta["profile"], "codex-b");
    assert_eq!(meta["resume"], "01a13cb7-104f-7611-8852-5d9a6d492166");
    assert_eq!(meta["credentials"]["CODEX_AUTH_JSON"], doc);
    Ok(())
}
