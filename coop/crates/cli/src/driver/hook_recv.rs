// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::os::fd::{AsFd, AsRawFd, BorrowedFd, OwnedFd};
use std::path::{Path, PathBuf};

use nix::sys::stat::Mode;
use serde::Deserialize;
use tokio::io::unix::AsyncFd;

use super::HookEvent;

/// Newtype for a FIFO file descriptor, for use with [`AsyncFd`].
struct FifoFd(OwnedFd);

impl AsRawFd for FifoFd {
    fn as_raw_fd(&self) -> std::os::fd::RawFd {
        self.0.as_raw_fd()
    }
}

impl AsFd for FifoFd {
    fn as_fd(&self) -> BorrowedFd<'_> {
        self.0.as_fd()
    }
}

/// Receives structured hook events from a named pipe (FIFO).
///
/// Agent hooks write JSON lines to the pipe. The receiver reads and parses
/// them into [`HookEvent`] values.
///
/// Uses non-blocking I/O via [`AsyncFd`] so that reads are cancellable
/// by `tokio::select!` and don't leak blocked threads on shutdown.
pub struct HookReceiver {
    pipe_path: PathBuf,
    /// Pre-opened fd that keeps a reader on the FIFO so writers don't block.
    /// Consumed by `ensure_fd()` to build the `AsyncFd`.
    raw_fd: Option<OwnedFd>,
    async_fd: Option<AsyncFd<FifoFd>>,
    line_buf: Vec<u8>,
}

/// Intermediate type for parsing hook JSON from the pipe.
#[derive(Deserialize)]
struct RawHookJson {
    event: String,
    data: Option<serde_json::Value>,
}

impl HookReceiver {
    /// Create a new hook receiver, creating the named pipe at `pipe_path`.
    ///
    /// Opens the FIFO immediately with `O_RDWR` so that a reader exists on
    /// the pipe before the agent process starts. Without this, hook scripts
    /// that write to the pipe (`> $COOP_HOOK_PIPE`) would block on
    /// `open(O_WRONLY)` until the async detector task calls `ensure_fd()`,
    /// causing "startup hook error" under load (e.g. mux with many sessions).
    pub fn new(pipe_path: &Path) -> anyhow::Result<Self> {
        // Remove any stale pipe from a previous unclean shutdown.
        let _ = std::fs::remove_file(pipe_path);
        nix::unistd::mkfifo(pipe_path, Mode::from_bits_truncate(0o600))?;

        // Open O_RDWR immediately so the kernel sees a reader on the FIFO.
        // This fd is later consumed by `ensure_fd()` to build the `AsyncFd`.
        let std_file = std::fs::OpenOptions::new().read(true).write(true).open(pipe_path)?;
        let raw_fd: OwnedFd = std_file.into();

        Ok(Self {
            pipe_path: pipe_path.to_path_buf(),
            raw_fd: Some(raw_fd),
            async_fd: None,
            line_buf: Vec::with_capacity(4096),
        })
    }

    /// Path to the named pipe.
    pub fn pipe_path(&self) -> &Path {
        &self.pipe_path
    }

    /// Read the next hook event from the pipe.
    ///
    /// Returns `None` on EOF or unrecoverable error. Skips malformed lines.
    /// The second element is the raw JSON value of the entire hook line.
    pub async fn next_event(&mut self) -> Option<(HookEvent, serde_json::Value)> {
        self.ensure_fd().ok()?;

        loop {
            // Drain complete lines from the buffer first.
            if let Some(pair) = self.try_parse_line() {
                return Some(pair);
            }

            // Read more data from the pipe via non-blocking I/O.
            let afd = self.async_fd.as_ref()?;
            let mut guard = match afd.readable().await {
                Ok(g) => g,
                Err(_) => return None,
            };
            let mut buf = [0u8; 4096];
            match guard.try_io(|inner| {
                nix::unistd::read(inner.get_ref(), &mut buf)
                    .map_err(|e| std::io::Error::from_raw_os_error(e as i32))
            }) {
                Ok(Ok(0)) => return None, // EOF
                Ok(Ok(n)) => self.line_buf.extend_from_slice(&buf[..n]),
                Ok(Err(_)) => return None,
                Err(_would_block) => continue,
            }
        }
    }

    /// Try to extract a parsed event from complete lines in the buffer.
    ///
    /// Drains malformed lines and returns the first valid event, or `None`
    /// if no complete lines remain.
    fn try_parse_line(&mut self) -> Option<(HookEvent, serde_json::Value)> {
        loop {
            let pos = self.line_buf.iter().position(|&b| b == b'\n')?;
            let line = String::from_utf8_lossy(&self.line_buf[..pos]).to_string();
            self.line_buf.drain(..=pos);
            if let Some(pair) = parse_hook_line(line.trim()) {
                return Some(pair);
            }
            // Malformed line — drain it and try the next one.
        }
    }

    /// Register the pipe fd with tokio for async I/O.
    ///
    /// Consumes the pre-opened fd from `new()` (or opens a fresh one as
    /// fallback), sets non-blocking mode, and wraps in [`AsyncFd`].
    fn ensure_fd(&mut self) -> anyhow::Result<()> {
        if self.async_fd.is_none() {
            let owned: OwnedFd = match self.raw_fd.take() {
                Some(fd) => fd,
                None => {
                    std::fs::OpenOptions::new().read(true).write(true).open(&self.pipe_path)?.into()
                }
            };
            crate::backend::nbio::set_nonblocking(&owned)?;
            let fifo_fd = FifoFd(owned);
            let async_fd = AsyncFd::new(fifo_fd)?;
            self.async_fd = Some(async_fd);
        }
        Ok(())
    }
}

impl Drop for HookReceiver {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.pipe_path);
    }
}

/// Parse a raw JSON line from the hook pipe into a [`HookEvent`] and the raw JSON value.
fn parse_hook_line(line: &str) -> Option<(HookEvent, serde_json::Value)> {
    let raw_json: serde_json::Value = serde_json::from_str(line).ok()?;
    let raw: RawHookJson = serde_json::from_value(raw_json.clone()).ok()?;
    let event = match raw.event.as_str() {
        "post_tool_use" | "after_tool" => {
            let tool = raw
                .data
                .as_ref()
                .and_then(|d| d.get("tool_name"))
                .and_then(|v| v.as_str())
                .unwrap_or_default()
                .to_string();
            let tool_input = raw.data.as_ref().and_then(|d| d.get("tool_input").cloned());
            let tool_use_id = raw
                .data
                .as_ref()
                .and_then(|d| d.get("tool_use_id"))
                .and_then(|v| v.as_str())
                .map(str::to_owned);
            let duration_ms =
                raw.data.as_ref().and_then(|d| d.get("duration_ms")).and_then(|v| v.as_u64());
            HookEvent::ToolAfter { tool, tool_input, tool_use_id, duration_ms }
        }
        "before_agent" | "user_prompt_submit" => HookEvent::TurnStart,
        "stop" => HookEvent::TurnEnd,
        "session_end" => HookEvent::SessionEnd,
        "notification" => {
            let data = raw.data?;
            let notification_type =
                data.get("notification_type").and_then(|v| v.as_str())?.to_string();
            HookEvent::Notification { notification_type }
        }
        "pre_tool_use" => {
            let data = raw.data?;
            let tool =
                data.get("tool_name").and_then(|v| v.as_str()).unwrap_or_default().to_string();
            let tool_input = data.get("tool_input").cloned();
            let tool_use_id = data.get("tool_use_id").and_then(|v| v.as_str()).map(str::to_owned);
            HookEvent::ToolBefore { tool, tool_input, tool_use_id }
        }
        "start" => HookEvent::SessionStart,
        _ => return None,
    };
    Some((event, raw_json))
}

#[cfg(test)]
#[path = "hook_recv_tests.rs"]
mod tests;
