// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::Arc;

use tokio::sync::RwLock;

use crate::driver::AgentState;
use crate::ring::RingBuffer;
use crate::screen::Screen;

use super::*;

fn test_terminal() -> Arc<TerminalState> {
    Arc::new(TerminalState {
        screen: RwLock::new(Screen::new(80, 24)),
        ring: RwLock::new(RingBuffer::new(4096)),
        ring_total_written: Arc::new(AtomicU64::new(0)),
        child_pid: AtomicU32::new(0),
        exit_status: RwLock::new(None),
    })
}

fn test_driver() -> Arc<DriverState> {
    Arc::new(DriverState {
        agent_state: RwLock::new(AgentState::Starting),
        state_seq: AtomicU64::new(0),
        detection: RwLock::new(DetectionInfo { tier: u8::MAX, cause: String::new() }),
        error: RwLock::new(None),
        last_message: Arc::new(RwLock::new(None)),
    })
}

#[tokio::test]
async fn terminal_reset_clears_state() -> anyhow::Result<()> {
    let terminal = test_terminal();

    // Dirty the state.
    terminal.child_pid.store(42, Ordering::Release);
    terminal.ring_total_written.store(999, Ordering::Relaxed);
    {
        let mut ring = terminal.ring.write().await;
        ring.write(b"hello");
    }
    *terminal.exit_status.write().await =
        Some(crate::driver::ExitStatus { code: Some(1), signal: None });

    terminal.reset(120, 40, 8192).await;

    assert_eq!(terminal.child_pid.load(Ordering::Acquire), 0);
    assert_eq!(terminal.ring_total_written.load(Ordering::Relaxed), 0);
    assert!(terminal.exit_status.read().await.is_none());
    // Screen should have new dimensions.
    let snap = terminal.screen.read().await.snapshot();
    assert_eq!(snap.cols, 120);
    assert_eq!(snap.rows, 40);
    Ok(())
}

#[tokio::test]
async fn driver_reset_clears_state() -> anyhow::Result<()> {
    let driver = test_driver();

    // Dirty the state.
    *driver.agent_state.write().await = AgentState::Working;
    driver.state_seq.store(42, Ordering::Release);
    *driver.detection.write().await = DetectionInfo { tier: 1, cause: "hook".to_owned() };
    *driver.error.write().await =
        Some(ErrorInfo { detail: "bad".to_owned(), category: crate::driver::ErrorCategory::Other });
    *driver.last_message.write().await = Some("msg".to_owned());

    driver.reset().await;

    assert!(matches!(*driver.agent_state.read().await, AgentState::Starting));
    assert_eq!(driver.state_seq.load(Ordering::Acquire), 0);
    assert_eq!(driver.detection.read().await.tier, u8::MAX);
    assert!(driver.error.read().await.is_none());
    assert!(driver.last_message.read().await.is_none());
    Ok(())
}

#[test]
fn child_pid_fn_returns_none_when_zero() {
    let terminal = test_terminal();
    let f = terminal.child_pid_fn();
    assert_eq!(f(), None);
}

#[test]
fn child_pid_fn_returns_some_when_set() {
    let terminal = test_terminal();
    terminal.child_pid.store(123, Ordering::Release);
    let f = terminal.child_pid_fn();
    assert_eq!(f(), Some(123));
}

#[test]
fn ring_total_written_fn_reads_atomic() {
    let terminal = test_terminal();
    terminal.ring_total_written.store(42, Ordering::Relaxed);
    let f = terminal.ring_total_written_fn();
    assert_eq!(f(), 42);
}

#[test]
fn snapshot_fn_returns_dimensions() {
    let terminal = test_terminal();
    let f = terminal.snapshot_fn();
    let snap = f();
    assert_eq!(snap.cols, 80);
    assert_eq!(snap.rows, 24);
}
