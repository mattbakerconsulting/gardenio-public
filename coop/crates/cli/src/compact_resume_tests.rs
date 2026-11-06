// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Tests for the post-compaction resume nudge (ka-0000007525).

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use super::{watch, CompactResumeConfig, ResumeOutcome};
use crate::driver::{AgentState, ExitStatus};
use crate::event::InputEvent;
use crate::test_support::{StoreBuilder, StoreCtx, StubNudgeEncoder};

/// Short, deterministic timings — tests run under `start_paused = true`, so
/// these advance instantly.
fn cfg() -> CompactResumeConfig {
    CompactResumeConfig {
        enabled: true,
        grace: Duration::from_secs(45),
        window: Duration::from_secs(600),
        poll: Duration::from_secs(2),
        message: "resume please".to_owned(),
    }
}

fn idle_store() -> StoreCtx {
    let ctx = StoreBuilder::new()
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    ctx.store.ready.store(true, Ordering::Release);
    ctx
}

#[tokio::test(start_paused = true)]
async fn nudges_after_grace_when_idle() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = idle_store();

    let outcome = watch(&store, &cfg()).await;

    assert_eq!(outcome, ResumeOutcome::Nudged);
    // The nudge encoder types the message, then Enter.
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(_))), "expected a PTY write, got {event:?}");
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn disabled_config_never_nudges() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = idle_store();

    let outcome = watch(&store, &CompactResumeConfig { enabled: false, ..cfg() }).await;

    assert_eq!(outcome, ResumeOutcome::Disabled);
    assert!(input_rx.try_recv().is_err(), "disabled watcher must not touch the PTY");
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn working_agent_is_not_nudged_within_window() -> anyhow::Result<()> {
    // Auto-compaction that flows straight into the next turn keeps the agent
    // Working — the watcher must expire rather than interrupt a live turn.
    let ctx = StoreBuilder::new()
        .agent_state(AgentState::Working)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    ctx.store.ready.store(true, Ordering::Release);
    let StoreCtx { store, mut input_rx, .. } = ctx;

    let outcome = watch(&store, &cfg()).await;

    assert_eq!(outcome, ResumeOutcome::WindowElapsed);
    assert!(input_rx.try_recv().is_err(), "a working agent must not be nudged");
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn brief_idle_shorter_than_grace_does_not_nudge() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = idle_store();

    // Flip back to Working before the grace elapses: the agent picked its own
    // work back up, so the idle stretch must not accumulate to a nudge.
    let flipper = Arc::clone(&store);
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_secs(6)).await;
        *flipper.driver.agent_state.write().await = AgentState::Working;
    });

    let outcome =
        watch(&store, &CompactResumeConfig { window: Duration::from_secs(120), ..cfg() }).await;

    assert_eq!(outcome, ResumeOutcome::WindowElapsed);
    assert!(input_rx.try_recv().is_err(), "grace was never satisfied; no nudge expected");
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn other_input_disarms_the_watcher() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = idle_store();

    // Mid-watch, somebody else resumes the agent (operator nudge, gb
    // re-inject, raw keys) — every PTY write bumps bytes_written off the
    // baseline the watcher recorded when it armed.
    let writer = Arc::clone(&store);
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_secs(6)).await;
        writer.lifecycle.bytes_written.fetch_add(12, Ordering::Relaxed);
    });

    let outcome = watch(&store, &cfg()).await;

    assert_eq!(outcome, ResumeOutcome::Interrupted);
    assert!(input_rx.try_recv().is_err(), "must not stack a second prompt");
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn exited_agent_stands_down() -> anyhow::Result<()> {
    let ctx = StoreBuilder::new()
        .agent_state(AgentState::Exited { status: ExitStatus { code: Some(0), signal: None } })
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    ctx.store.ready.store(true, Ordering::Release);
    let StoreCtx { store, mut input_rx, .. } = ctx;

    let outcome = watch(&store, &cfg()).await;

    assert_eq!(outcome, ResumeOutcome::Exited);
    assert!(input_rx.try_recv().is_err());
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn shutdown_stands_down() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = idle_store();
    store.lifecycle.shutdown.cancel();

    let outcome = watch(&store, &cfg()).await;

    assert_eq!(outcome, ResumeOutcome::ShuttingDown);
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn not_ready_session_reports_nudge_failure() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = idle_store();
    store.ready.store(false, Ordering::Release);

    let outcome = watch(&store, &cfg()).await;

    assert_eq!(outcome, ResumeOutcome::NudgeFailed);
    Ok(())
}

#[test]
fn config_from_env_overrides_defaults() {
    let defaults = CompactResumeConfig::default();
    assert!(defaults.enabled);
    assert_eq!(defaults.grace, Duration::from_millis(45_000));
    assert_eq!(defaults.window, Duration::from_millis(600_000));

    std::env::set_var("COOP_COMPACT_RESUME_GRACE_MS", "1234");
    std::env::set_var("COOP_COMPACT_RESUME_WINDOW_MS", "not-a-number");
    std::env::set_var("COOP_COMPACT_RESUME_MESSAGE", "  ");
    let cfg = CompactResumeConfig::from_env();
    assert_eq!(cfg.grace, Duration::from_millis(1234), "valid override applies");
    assert_eq!(cfg.window, defaults.window, "unparseable value falls back to the default");
    assert_eq!(cfg.message, super::DEFAULT_MESSAGE, "blank message falls back to the default");

    std::env::set_var("COOP_COMPACT_RESUME", "0");
    assert!(!CompactResumeConfig::from_env().enabled);
    std::env::set_var("COOP_COMPACT_RESUME", "1");
    assert!(CompactResumeConfig::from_env().enabled);

    std::env::remove_var("COOP_COMPACT_RESUME");
    std::env::remove_var("COOP_COMPACT_RESUME_GRACE_MS");
    std::env::remove_var("COOP_COMPACT_RESUME_WINDOW_MS");
    std::env::remove_var("COOP_COMPACT_RESUME_MESSAGE");
}

#[test]
fn default_message_is_single_line_and_not_a_slash_command() {
    // The encoder types the message then presses Enter: a newline would submit
    // a half-written prompt, and a leading `/` or `@` opens a completion menu.
    assert!(!super::DEFAULT_MESSAGE.contains('\n'));
    assert!(!super::DEFAULT_MESSAGE.starts_with('/'));
    assert!(!super::DEFAULT_MESSAGE.starts_with('@'));
}
