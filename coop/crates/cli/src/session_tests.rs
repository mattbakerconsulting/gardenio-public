// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;
use std::time::Duration;

use tokio_util::sync::CancellationToken;

use crate::backend::spawn::NativePty;
use crate::config::{Config, GroomLevel};
use crate::driver::{AgentState, PromptContext, PromptKind};
use crate::session::{Session, SessionConfig, SessionOutcome};
use crate::switch::SwitchRequest;
use crate::test_support::{MockDetector, MockPty, StoreBuilder, StoreCtx, StubRespondEncoder};

#[tokio::test]
async fn echo_exits_with_zero() -> anyhow::Result<()> {
    let config = Config::test();
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();

    let backend = NativePty::spawn(&["echo".into(), "hello".into()], 80, 24, &[])?;
    let session = Session::new(&config, SessionConfig::new(store, backend));

    let status = session.run_to_exit(&config, &mut input_rx).await?;
    assert_eq!(status.code, Some(0));
    Ok(())
}

#[tokio::test]
async fn output_captured_in_ring_and_screen() -> anyhow::Result<()> {
    let config = Config::test();
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();

    let backend = NativePty::spawn(&["echo".into(), "hello-ring".into()], 80, 24, &[])?;
    let session = Session::new(&config, SessionConfig::new(Arc::clone(&store), backend));

    let _ = session.run_to_exit(&config, &mut input_rx).await?;

    // Check ring buffer
    let ring = store.terminal.ring.read().await;
    assert!(ring.total_written() > 0);
    let (a, b) = ring.read_from(0).ok_or(anyhow::anyhow!("no data"))?;
    let mut data = a.to_vec();
    data.extend_from_slice(b);
    let text = String::from_utf8_lossy(&data);
    assert!(text.contains("hello-ring"), "ring: {text:?}");

    // Check screen
    let screen = store.terminal.screen.read().await;
    let snap = screen.snapshot();
    let lines = snap.lines.join("\n");
    assert!(lines.contains("hello-ring"), "screen: {lines:?}");

    Ok(())
}

#[tokio::test]
async fn shutdown_cancels_session() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    // Long-running command
    let backend =
        NativePty::spawn(&["/bin/sh".into(), "-c".into(), "sleep 60".into()], 80, 24, &[])?;
    let session =
        Session::new(&config, SessionConfig::new(store, backend).with_shutdown(shutdown.clone()));

    // Cancel after a short delay
    tokio::spawn(async move {
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        shutdown.cancel();
    });

    let status = session.run_to_exit(&config, &mut input_rx).await?;
    // Should have exited (signal or timeout)
    assert!(status.code.is_some() || status.signal.is_some(), "expected exit: {status:?}");
    Ok(())
}

/// Agent is already idle when shutdown fires → immediate SIGHUP (no drain wait).
#[tokio::test]
async fn graceful_drain_kills_when_already_idle() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(2000);
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend = MockPty::new().drain_input();
    // Detector emits Idle almost immediately.
    let detector = MockDetector::new(1, vec![(Duration::from_millis(10), AgentState::Idle)]);

    let session = Session::new(
        &config,
        SessionConfig::new(store, backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(detector)]),
    );

    // Cancel after the detector has fired (10ms) + margin.
    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(100)).await;
        sd.cancel();
    });

    let start = std::time::Instant::now();
    let _ = session.run_to_exit(&config, &mut input_rx).await?;
    let elapsed = start.elapsed();

    // Should exit promptly (not wait for the 2s graceful timeout).
    assert!(elapsed < Duration::from_secs(1), "expected quick exit, took {elapsed:?}");
    Ok(())
}

/// Agent never reaches idle → drain deadline force-kills after timeout.
#[tokio::test]
async fn graceful_drain_timeout_force_kills() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(500);
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend = MockPty::new().drain_input();
    let captured = backend.captured_input();

    let session =
        Session::new(&config, SessionConfig::new(store, backend).with_shutdown(shutdown.clone()));

    // Cancel after a short delay; no detector → state stays Starting → drain entered.
    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(50)).await;
        sd.cancel();
    });

    let start = std::time::Instant::now();
    let _ = session.run_to_exit(&config, &mut input_rx).await?;
    let elapsed = start.elapsed();

    // Should wait for the ~500ms drain deadline, not exit immediately.
    assert!(elapsed >= Duration::from_millis(300), "exited too fast: {elapsed:?}");
    assert!(elapsed < Duration::from_secs(3), "took too long: {elapsed:?}");

    // Verify Escape bytes were sent during drain.
    let input = captured.lock();
    let has_escape = input.iter().any(|b| b.as_ref() == b"\x1b");
    assert!(has_escape, "expected Escape bytes in captured input: {input:?}");

    Ok(())
}

/// Agent sits idle past idle_timeout → the session tears down as an
/// idle-suspend: the lifecycle `idle_suspended` bit is set (so the terminal
/// exit path stamps the S3 meta exit_status "suspended" for warm-pool reclaim)
/// and the loop breaks promptly rather than waiting on a drain deadline
/// (ka-0000002921).
#[tokio::test]
async fn idle_timeout_marks_idle_suspended() -> anyhow::Result<()> {
    let mut config = Config::test();
    // Short idle window; generous drain so a mistaken drain-path exit would be
    // visibly slow (and would NOT set the idle_suspended bit).
    config.idle_timeout_ms = Some(60);
    config.drain_timeout_ms = Some(3000);
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend = MockPty::new().drain_input();
    // Detector emits Idle almost immediately; the agent then stays idle, so the
    // idle_since timer elapses and the idle-timeout branch fires.
    let detector = MockDetector::new(1, vec![(Duration::from_millis(10), AgentState::Idle)]);

    let session = Session::new(
        &config,
        SessionConfig::new(Arc::clone(&store), backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(detector)]),
    );

    let start = std::time::Instant::now();
    let _ = session.run_to_exit(&config, &mut input_rx).await?;
    let elapsed = start.elapsed();

    // The idle-timeout branch sets the bit before cancelling shutdown.
    assert!(
        store.lifecycle.idle_suspended.load(std::sync::atomic::Ordering::Acquire),
        "idle-timeout teardown should set the idle_suspended bit"
    );
    // It also cancels the shutdown token as part of the teardown.
    assert!(shutdown.is_cancelled(), "idle-timeout should cancel the shutdown token");
    // Exit is driven by the ~60ms idle window (+ the sigterm_child_group 1s
    // leaf grace), well under the 3s drain deadline that a wrong path would hit.
    assert!(elapsed < Duration::from_millis(2500), "idle-suspend took too long: {elapsed:?}");

    Ok(())
}

/// A session that never goes idle (idle_timeout disabled by default) must NOT
/// set the idle_suspended bit — a plain shutdown stays "stopped", not
/// "suspended" (ka-0000002921).
#[tokio::test]
async fn plain_shutdown_does_not_mark_idle_suspended() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.idle_timeout_ms = Some(0); // disabled
    config.drain_timeout_ms = Some(0);
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend = MockPty::new().drain_input();
    let detector = MockDetector::new(1, vec![(Duration::from_millis(10), AgentState::Idle)]);

    let session = Session::new(
        &config,
        SessionConfig::new(Arc::clone(&store), backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(detector)]),
    );

    // Cancel externally (the gb-stop / pod-teardown path), not via idle-timeout.
    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(50)).await;
        sd.cancel();
    });

    let _ = session.run_to_exit(&config, &mut input_rx).await?;

    assert!(
        !store.lifecycle.idle_suspended.load(std::sync::atomic::Ordering::Acquire),
        "a non-idle-timeout shutdown must not set the idle_suspended bit"
    );

    Ok(())
}

/// drain_timeout_ms=0 disables drain → immediate SIGHUP like pre-feature.
#[tokio::test]
async fn graceful_drain_disabled_when_zero() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend = MockPty::new().drain_input();
    let captured = backend.captured_input();

    let session =
        Session::new(&config, SessionConfig::new(store, backend).with_shutdown(shutdown.clone()));

    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(50)).await;
        sd.cancel();
    });

    let start = std::time::Instant::now();
    let _ = session.run_to_exit(&config, &mut input_rx).await?;
    let elapsed = start.elapsed();

    // Should exit immediately (no drain mode).
    assert!(elapsed < Duration::from_secs(2), "expected quick exit, took {elapsed:?}");

    // No Escape bytes should have been sent.
    let input = captured.lock();
    let has_escape = input.iter().any(|b| b.as_ref() == b"\x1b");
    assert!(!has_escape, "unexpected Escape bytes in captured input: {input:?}");

    Ok(())
}

fn disruption_prompt() -> AgentState {
    AgentState::Prompt {
        prompt: PromptContext::new(PromptKind::Setup)
            .with_subtype("theme_picker")
            .with_options(vec!["Dark mode".to_owned(), "Light mode".to_owned()])
            .with_ready(),
    }
}

/// groom=Auto auto-dismisses disruption prompts.
#[tokio::test]
async fn groom_auto_dismisses_disruption() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new()
        .ring_size(65536)
        .groom(GroomLevel::Auto)
        .respond_encoder(Arc::new(StubRespondEncoder))
        .build();

    let backend = MockPty::new().drain_input();
    let captured = backend.captured_input();

    // Detector emits a disruption prompt, then after a delay triggers shutdown.
    let detector = MockDetector::new(1, vec![(Duration::from_millis(10), disruption_prompt())]);

    let shutdown = CancellationToken::new();
    let session = Session::new(
        &config,
        SessionConfig::new(Arc::clone(&store), backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(detector)]),
    );

    // Give enough time for the 50ms test auto-dismiss delay + delivery, then shut down.
    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(300)).await;
        sd.cancel();
    });

    let _ = session.run_to_exit(&config, &mut input_rx).await?;

    // The auto-dismiss should have delivered keystrokes to the backend.
    let input = captured.lock();
    assert!(!input.is_empty(), "expected auto-dismiss keystrokes, got none");
    // StubRespondEncoder.encode_setup(1) → "1\r"
    let has_setup_response = input.iter().any(|b| b.as_ref() == b"1\r");
    assert!(has_setup_response, "expected '1\\r' in captured input: {input:?}");

    Ok(())
}

/// groom=Manual does NOT auto-dismiss disruption prompts.
#[tokio::test]
async fn groom_manual_does_not_dismiss() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new()
        .ring_size(65536)
        .groom(GroomLevel::Manual)
        .respond_encoder(Arc::new(StubRespondEncoder))
        .build();

    let backend = MockPty::new().drain_input();
    let captured = backend.captured_input();

    let detector = MockDetector::new(1, vec![(Duration::from_millis(10), disruption_prompt())]);

    let shutdown = CancellationToken::new();
    let session = Session::new(
        &config,
        SessionConfig::new(Arc::clone(&store), backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(detector)]),
    );

    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(300)).await;
        sd.cancel();
    });

    let _ = session.run_to_exit(&config, &mut input_rx).await?;

    // No auto-dismiss keystrokes should have been sent.
    let input = captured.lock();
    let has_setup_response = input.iter().any(|b| b.as_ref() == b"1\r");
    assert!(!has_setup_response, "unexpected auto-dismiss in manual mode: {input:?}");

    Ok(())
}

/// Switch request while agent is idle produces SessionOutcome::Switch.
///
/// MockPty doesn't respond to SIGHUP (PID 0), so we use a shutdown token
/// with drain_timeout_ms=0 to break the loop after the switch is staged.
#[tokio::test]
async fn switch_when_idle_returns_switch_outcome() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);
    let StoreCtx { store, mut input_rx, mut switch_rx } =
        StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend = MockPty::new().drain_input();
    // Detector emits Idle quickly.
    let detector = MockDetector::new(1, vec![(Duration::from_millis(10), AgentState::Idle)]);

    let switch_tx = store.switch.switch_tx.clone();
    let session = Session::new(
        &config,
        SessionConfig::new(Arc::clone(&store), backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(detector)]),
    );

    // Send a switch request after the detector fires, then shutdown to break the loop.
    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(50)).await;
        let _ =
            switch_tx.send(SwitchRequest { credentials: None, force: false, profile: None }).await;
        tokio::time::sleep(Duration::from_millis(100)).await;
        sd.cancel();
    });

    let outcome = session.run(&config, &mut input_rx, &mut switch_rx).await?;
    assert!(matches!(outcome, SessionOutcome::Switch(_)), "expected Switch, got {outcome:?}");
    Ok(())
}

/// Force switch while agent is working produces SessionOutcome::Switch.
#[tokio::test]
async fn force_switch_while_working() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);
    let StoreCtx { store, mut input_rx, mut switch_rx } =
        StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend = MockPty::new().drain_input();
    // Detector emits Working and stays there.
    let detector = MockDetector::new(1, vec![(Duration::from_millis(10), AgentState::Working)]);

    let switch_tx = store.switch.switch_tx.clone();
    let session = Session::new(
        &config,
        SessionConfig::new(Arc::clone(&store), backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(detector)]),
    );

    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(50)).await;
        let _ =
            switch_tx.send(SwitchRequest { credentials: None, force: true, profile: None }).await;
        tokio::time::sleep(Duration::from_millis(100)).await;
        sd.cancel();
    });

    let outcome = session.run(&config, &mut input_rx, &mut switch_rx).await?;
    assert!(matches!(outcome, SessionOutcome::Switch(_)), "expected Switch, got {outcome:?}");
    Ok(())
}

/// Graceful switch waits for idle — agent transitions Working → Idle → switch.
#[tokio::test]
async fn graceful_switch_waits_for_idle() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);
    let StoreCtx { store, mut input_rx, mut switch_rx } =
        StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend = MockPty::new().drain_input();
    // Detector: Working at 10ms, then Idle at 200ms.
    let detector = MockDetector::new(
        1,
        vec![
            (Duration::from_millis(10), AgentState::Working),
            (Duration::from_millis(200), AgentState::Idle),
        ],
    );

    let switch_tx = store.switch.switch_tx.clone();
    let session = Session::new(
        &config,
        SessionConfig::new(Arc::clone(&store), backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(detector)]),
    );

    // Send non-force switch while Working (at 50ms), shutdown after idle (at 400ms).
    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(50)).await;
        let _ =
            switch_tx.send(SwitchRequest { credentials: None, force: false, profile: None }).await;
        tokio::time::sleep(Duration::from_millis(350)).await;
        sd.cancel();
    });

    let start = std::time::Instant::now();
    let outcome = session.run(&config, &mut input_rx, &mut switch_rx).await?;
    let elapsed = start.elapsed();

    assert!(matches!(outcome, SessionOutcome::Switch(_)), "expected Switch, got {outcome:?}");
    // Should have waited for the Idle transition (~200ms), not returned immediately.
    assert!(elapsed >= Duration::from_millis(150), "returned too fast: {elapsed:?}");
    Ok(())
}
