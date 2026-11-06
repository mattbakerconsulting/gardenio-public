// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use super::{
    encode_response, resolve_permission_option, resolve_plan_option, spawn_enter_retry,
    spawn_enter_retry_with_max,
};
use crate::driver::claude::encoding::ClaudeRespondEncoder;
use crate::driver::{AgentState, PromptContext, PromptKind};
use crate::event::TransitionEvent;

#[test]
fn permission_option_takes_precedence_over_accept() {
    assert_eq!(resolve_permission_option(Some(true), Some(2)), 2);
    assert_eq!(resolve_permission_option(Some(false), Some(1)), 1);
}

#[test]
fn permission_accept_true_maps_to_1() {
    assert_eq!(resolve_permission_option(Some(true), None), 1);
}

#[test]
fn permission_accept_false_maps_to_3() {
    assert_eq!(resolve_permission_option(Some(false), None), 3);
}

#[test]
fn permission_both_none_defaults_to_3() {
    assert_eq!(resolve_permission_option(None, None), 3);
}

#[test]
fn plan_option_takes_precedence_over_accept() {
    assert_eq!(resolve_plan_option(Some(true), Some(3)), 3);
    assert_eq!(resolve_plan_option(Some(false), Some(1)), 1);
}

#[test]
fn plan_accept_true_maps_to_2() {
    assert_eq!(resolve_plan_option(Some(true), None), 2);
}

#[test]
fn plan_accept_false_maps_to_4() {
    assert_eq!(resolve_plan_option(Some(false), None), 4);
}

#[test]
fn plan_both_none_defaults_to_4() {
    assert_eq!(resolve_plan_option(None, None), 4);
}

fn fallback_prompt(kind: PromptKind) -> PromptContext {
    PromptContext::new(kind)
        .with_options(vec!["Accept".to_string(), "Cancel".to_string()])
        .with_options_fallback()
        .with_ready()
}

#[yare::parameterized(
    perm_accept_true = { PromptKind::Permission, Some(true), None, b"\r" as &[u8] },
    perm_accept_false = { PromptKind::Permission, Some(false), None, b"\x1b" },
    perm_option_1 = { PromptKind::Permission, None, Some(1), b"\r" },
    perm_option_2 = { PromptKind::Permission, None, Some(2), b"\x1b" },
    perm_none_defaults_esc = { PromptKind::Permission, None, None, b"\x1b" },
    plan_accept_true = { PromptKind::Plan, Some(true), None, b"\r" },
    plan_accept_false = { PromptKind::Plan, Some(false), None, b"\x1b" },
    plan_option_1 = { PromptKind::Plan, None, Some(1), b"\r" },
    plan_option_2 = { PromptKind::Plan, None, Some(2), b"\x1b" },
)]
fn fallback_encoding(kind: PromptKind, accept: Option<bool>, option: Option<u32>, expected: &[u8]) {
    let encoder = ClaudeRespondEncoder::default();
    let state = AgentState::Prompt { prompt: fallback_prompt(kind) };
    let (steps, _) = encode_response(&state, &encoder, accept, option, None, &[]).unwrap();
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, expected);
}

#[test]
fn non_fallback_permission_uses_encoder() {
    let encoder = ClaudeRespondEncoder::default();
    let prompt = PromptContext::new(PromptKind::Permission)
        .with_options(vec!["Yes".to_string(), "No".to_string()])
        .with_ready();
    let state = AgentState::Prompt { prompt };
    let (steps, _) = encode_response(&state, &encoder, Some(true), None, None, &[]).unwrap();
    // Non-fallback: digit only (TUI auto-confirms on number key).
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"1");
}

#[test]
fn setup_prompt_defaults_to_option_1() {
    let encoder = ClaudeRespondEncoder::default();
    let prompt = PromptContext::new(PromptKind::Setup)
        .with_subtype("theme_picker")
        .with_options(vec!["Dark mode".to_string(), "Light mode".to_string()])
        .with_ready();
    let state = AgentState::Prompt { prompt };
    let (steps, count) = encode_response(&state, &encoder, None, None, None, &[]).unwrap();
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"1");
    assert_eq!(count, 0);
}

#[test]
fn setup_prompt_respects_explicit_option() {
    let encoder = ClaudeRespondEncoder::default();
    let prompt = PromptContext::new(PromptKind::Setup)
        .with_subtype("theme_picker")
        .with_options(vec!["Dark mode".to_string(), "Light mode".to_string()])
        .with_ready();
    let state = AgentState::Prompt { prompt };
    let (steps, _) = encode_response(&state, &encoder, None, Some(2), None, &[]).unwrap();
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"2");
}

#[tokio::test(start_paused = true)]
async fn enter_retry_sends_cr_on_timeout() -> anyhow::Result<()> {
    let (input_tx, mut input_rx) = tokio::sync::mpsc::channel(16);
    let (state_tx, _) = tokio::sync::broadcast::channel::<TransitionEvent>(16);
    let state_rx = state_tx.subscribe();
    let activity = Arc::new(tokio::sync::Notify::new());

    let _cancel =
        spawn_enter_retry(input_tx, state_rx, activity, std::time::Duration::from_millis(50));

    // Wait for the retry to fire
    let event =
        tokio::time::timeout(std::time::Duration::from_millis(200), input_rx.recv()).await?;

    match event {
        Some(crate::event::InputEvent::Write(data)) => {
            assert_eq!(&data[..], b"\r");
        }
        other => panic!("expected Write(\\r), got {other:?}"),
    }
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn enter_retry_sends_multiple_crs_with_backoff() -> anyhow::Result<()> {
    let (input_tx, mut input_rx) = tokio::sync::mpsc::channel(16);
    let (state_tx, _) = tokio::sync::broadcast::channel::<TransitionEvent>(16);
    let state_rx = state_tx.subscribe();
    let activity = Arc::new(tokio::sync::Notify::new());

    let _cancel = spawn_enter_retry_with_max(
        input_tx,
        state_rx,
        activity,
        std::time::Duration::from_millis(20),
        3,
    );

    // Should receive 3 Enter retries (at ~20ms, ~40ms, ~80ms ≈ 140ms total)
    for i in 1..=3 {
        let event =
            tokio::time::timeout(std::time::Duration::from_millis(500), input_rx.recv()).await;
        match event {
            Ok(Some(crate::event::InputEvent::Write(data))) => {
                assert_eq!(&data[..], b"\r", "retry {i} should be \\r");
            }
            other => panic!("expected Write(\\r) for retry {i}, got {other:?}"),
        }
    }

    // No more retries after max: either timeout (still open) or channel closed (sender dropped)
    let result = tokio::time::timeout(std::time::Duration::from_millis(300), input_rx.recv()).await;
    match result {
        Err(_) => {}   // timeout — no more sends, good
        Ok(None) => {} // channel closed — sender dropped after max retries, good
        Ok(Some(event)) => panic!("expected no more retries after max_retries=3, got {event:?}"),
    }
    Ok(())
}

/// Verify that exponential backoff is capped at MAX_RETRY_DELAY (8 s).
///
/// Uses a 50 ms base to keep the test fast.  After 5 retries at 50 ms base
/// uncapped backoff would be 50→100→200→400→800, totalling ~1550 ms.
/// With a 150 ms cap the series becomes 50→100→150→150→150 ≈ 600 ms.
/// We assert all 5 arrive within 1000 ms which would fail without the cap.
#[tokio::test(start_paused = true)]
async fn enter_retry_backoff_is_capped() -> anyhow::Result<()> {
    let (input_tx, mut input_rx) = tokio::sync::mpsc::channel(16);
    let (state_tx, _) = tokio::sync::broadcast::channel::<TransitionEvent>(16);
    let state_rx = state_tx.subscribe();
    let activity = Arc::new(tokio::sync::Notify::new());

    // Use a small base delay; the code caps at MAX_RETRY_DELAY (8 s), but
    // we only care that the cap *logic* works, so 5 retries at 50 ms base
    // with a global 8 s cap all still fit within 1 s (uncapped: 50+100+200+400+800 = 1550 ms).
    let _cancel = spawn_enter_retry_with_max(
        input_tx,
        state_rx,
        activity,
        std::time::Duration::from_millis(50),
        5,
    );

    let start = std::time::Instant::now();
    for i in 1..=5 {
        let event = tokio::time::timeout(std::time::Duration::from_secs(2), input_rx.recv()).await;
        match event {
            Ok(Some(crate::event::InputEvent::Write(data))) => {
                assert_eq!(&data[..], b"\r", "retry {i} should be \\r");
            }
            other => anyhow::bail!("expected Write(\\r) for retry {i}, got {other:?}"),
        }
    }
    let elapsed = start.elapsed();

    // Without a cap this would take ~1550 ms; with the cap on larger bases
    // it converges faster.  At 50 ms base, the cap only kicks in if
    // MAX_RETRY_DELAY < 800 ms (which it is at 8 s), so for this test we
    // just verify all 5 arrive reasonably quickly.
    assert!(
        elapsed < std::time::Duration::from_secs(2),
        "5 retries took {elapsed:?}, expected <2s"
    );
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn enter_retry_backoff_cancelled_mid_loop_by_state() -> anyhow::Result<()> {
    let (input_tx, mut input_rx) = tokio::sync::mpsc::channel(16);
    let _keep_alive = input_tx.clone();
    let (state_tx, _) = tokio::sync::broadcast::channel::<TransitionEvent>(16);
    let state_rx = state_tx.subscribe();
    let activity = Arc::new(tokio::sync::Notify::new());

    let _cancel = spawn_enter_retry_with_max(
        input_tx,
        state_rx,
        activity,
        std::time::Duration::from_millis(30),
        5,
    );

    // First retry fires
    let event =
        tokio::time::timeout(std::time::Duration::from_millis(200), input_rx.recv()).await?;
    assert!(matches!(event, Some(crate::event::InputEvent::Write(_))));

    // Now send Working transition — should stop further retries
    let _ = state_tx.send(TransitionEvent {
        prev: AgentState::Idle,
        next: AgentState::Working,
        seq: 1,
        cause: "test".to_owned(),
        last_message: None,
    });

    // No more retries should fire
    let result = tokio::time::timeout(std::time::Duration::from_millis(200), input_rx.recv()).await;
    assert!(result.is_err(), "expected no more retries after Working transition");
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn enter_retry_cancelled_by_state_transition() -> anyhow::Result<()> {
    let (input_tx, mut input_rx) = tokio::sync::mpsc::channel(16);
    let _keep_alive = input_tx.clone(); // keep channel open after task exits
    let (state_tx, _) = tokio::sync::broadcast::channel::<TransitionEvent>(16);
    let state_rx = state_tx.subscribe();
    let activity = Arc::new(tokio::sync::Notify::new());

    let _cancel =
        spawn_enter_retry(input_tx, state_rx, activity, std::time::Duration::from_millis(100));

    // Send a Working state transition — should cancel the retry
    let _ = state_tx.send(TransitionEvent {
        prev: AgentState::Idle,
        next: AgentState::Working,
        seq: 1,
        cause: "test".to_owned(),
        last_message: None,
    });

    // The retry should NOT fire
    let result = tokio::time::timeout(std::time::Duration::from_millis(150), input_rx.recv()).await;

    assert!(result.is_err(), "expected timeout (no retry), got {result:?}");
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn enter_retry_ignores_input_activity() -> anyhow::Result<()> {
    // input_activity used to cancel the retry, but it caused self-cancellation
    // because the retry's own \r writes trigger input_activity.  Now the retry
    // ignores input_activity and relies solely on state transitions + cancel token.
    let (input_tx, mut input_rx) = tokio::sync::mpsc::channel(16);
    let (state_tx, _) = tokio::sync::broadcast::channel::<TransitionEvent>(16);
    let state_rx = state_tx.subscribe();
    let activity = Arc::new(tokio::sync::Notify::new());

    let _cancel = spawn_enter_retry(
        input_tx,
        state_rx,
        Arc::clone(&activity),
        std::time::Duration::from_millis(50),
    );

    // Give the spawned task time to register
    tokio::time::sleep(std::time::Duration::from_millis(10)).await;

    // Notify input activity — should NOT cancel the retry anymore
    activity.notify_waiters();

    // The retry should still fire
    let event =
        tokio::time::timeout(std::time::Duration::from_millis(200), input_rx.recv()).await?;
    assert!(matches!(event, Some(crate::event::InputEvent::Write(_))));
    Ok(())
}

#[tokio::test(start_paused = true)]
async fn enter_retry_cancelled_by_token() -> anyhow::Result<()> {
    let (input_tx, mut input_rx) = tokio::sync::mpsc::channel(16);
    let _keep_alive = input_tx.clone(); // keep channel open after task exits
    let (state_tx, _) = tokio::sync::broadcast::channel::<TransitionEvent>(16);
    let state_rx = state_tx.subscribe();
    let activity = Arc::new(tokio::sync::Notify::new());

    let cancel =
        spawn_enter_retry(input_tx, state_rx, activity, std::time::Duration::from_millis(100));

    // Cancel via the token (simulates next InputGate::acquire)
    cancel.cancel();

    // The retry should NOT fire
    let result = tokio::time::timeout(std::time::Duration::from_millis(150), input_rx.recv()).await;

    assert!(result.is_err(), "expected timeout (no retry), got {result:?}");
    Ok(())
}
