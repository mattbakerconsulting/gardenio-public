// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

pub mod adapter;
pub mod codex;
pub mod gemini;
pub mod nbio;
pub mod spawn;

use bytes::Bytes;
use std::future::Future;
use std::pin::Pin;
use tokio::sync::mpsc;

use crate::driver::ExitStatus;

/// Input sent to the PTY backend: either raw bytes to write or a drain
/// synchronization point.
#[derive(Debug)]
pub enum BackendInput {
    /// Raw bytes to write to the PTY.
    Write(Bytes),
    /// Drain marker: since the backend processes messages sequentially,
    /// all prior writes are complete when this is received.  The sender
    /// is notified via the oneshot channel.
    Drain(tokio::sync::oneshot::Sender<()>),
}

/// Terminal backend abstraction over PTY or compatibility layers.
///
/// Object-safe for use as `Box<dyn Backend>`.
pub trait Backend: Send + 'static {
    fn run(
        &mut self,
        output_tx: mpsc::Sender<Bytes>,
        input_rx: mpsc::Receiver<BackendInput>,
        resize_rx: mpsc::Receiver<(u16, u16)>,
    ) -> Pin<Box<dyn Future<Output = anyhow::Result<ExitStatus>> + Send + '_>>;

    fn resize(&self, cols: u16, rows: u16) -> anyhow::Result<()>;

    fn child_pid(&self) -> Option<u32>;
}

/// Conversion trait so both concrete backends and `Box<dyn Backend>`
/// can be passed to [`SessionConfig::new`] without explicit boxing.
pub trait Boxed {
    fn boxed(self) -> Box<dyn Backend>;
}

impl<T: Backend> Boxed for T {
    fn boxed(self) -> Box<dyn Backend> {
        Box::new(self)
    }
}

impl Boxed for Box<dyn Backend> {
    fn boxed(self) -> Box<dyn Backend> {
        self
    }
}
