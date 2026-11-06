// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use bytes::Bytes;
use std::future::Future;
use std::pin::Pin;
use std::str::FromStr;
use std::time::Duration;
use tokio::sync::mpsc;

use crate::backend::{Backend, BackendInput};
use crate::driver::ExitStatus;

/// Specifies which terminal multiplexer session to attach to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AdapterSpec {
    Tmux { session: String },
    Screen { session: String },
}

impl FromStr for AdapterSpec {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let (prefix, name) = s.split_once(':').ok_or_else(|| {
            anyhow::anyhow!("invalid attach spec: expected 'tmux:NAME' or 'screen:NAME'")
        })?;
        if name.is_empty() {
            anyhow::bail!("invalid attach spec: session name cannot be empty");
        }
        match prefix {
            "tmux" => Ok(AdapterSpec::Tmux { session: name.to_string() }),
            "screen" => Ok(AdapterSpec::Screen { session: name.to_string() }),
            other => anyhow::bail!("invalid attach spec: unknown backend '{other}'"),
        }
    }
}

/// Compatibility backend that attaches to an existing tmux session.
pub struct TmuxBackend {
    session: String,
    target: String,
    socket: Option<std::path::PathBuf>,
    poll_interval: Duration,
}

impl TmuxBackend {
    /// Create a new `TmuxBackend` for the given tmux session.
    ///
    /// Validates the session exists via `tmux has-session`.
    pub fn new(session: String) -> anyhow::Result<Self> {
        Self::with_socket(session, None)
    }

    /// Create a new `TmuxBackend` targeting a specific tmux server socket.
    ///
    /// When `socket` is `Some`, every tmux invocation uses `-S <path>` to
    /// address an isolated server instead of the user's default.
    pub fn with_socket(
        session: String,
        socket: Option<std::path::PathBuf>,
    ) -> anyhow::Result<Self> {
        let mut cmd = std::process::Command::new("tmux");
        if let Some(ref s) = socket {
            cmd.arg("-S").arg(s);
        }
        cmd.args(["has-session", "-t", &session])
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null());

        match cmd.status() {
            Ok(s) if s.success() => {}
            Ok(_) => anyhow::bail!("tmux session '{session}' does not exist"),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                anyhow::bail!("tmux is not installed or not in PATH")
            }
            Err(e) => return Err(anyhow::Error::new(e).context("failed to check tmux session")),
        }

        let target = session.clone();
        Ok(Self { session, target, socket, poll_interval: Duration::from_secs(1) })
    }

    pub fn with_poll_interval(mut self, interval: Duration) -> Self {
        self.poll_interval = interval;
        self
    }

    /// Returns the session name.
    pub fn session(&self) -> &str {
        &self.session
    }

    /// Build a `std::process::Command` for tmux, prepending `-S <socket>` if set.
    fn tmux_cmd(&self) -> std::process::Command {
        let mut cmd = std::process::Command::new("tmux");
        if let Some(ref s) = self.socket {
            cmd.arg("-S").arg(s);
        }
        cmd
    }

    /// Build a `tokio::process::Command` for tmux, prepending `-S <socket>` if set.
    fn tmux_async_cmd(&self) -> tokio::process::Command {
        let mut cmd = tokio::process::Command::new("tmux");
        if let Some(ref s) = self.socket {
            cmd.arg("-S").arg(s);
        }
        cmd
    }
}

impl Backend for TmuxBackend {
    fn run(
        &mut self,
        output_tx: mpsc::Sender<Bytes>,
        mut input_rx: mpsc::Receiver<BackendInput>,
        mut resize_rx: mpsc::Receiver<(u16, u16)>,
    ) -> Pin<Box<dyn Future<Output = anyhow::Result<ExitStatus>> + Send + '_>> {
        Box::pin(async move {
            let mut interval = tokio::time::interval(self.poll_interval);
            let mut prev_capture = String::new();

            loop {
                tokio::select! {
                    _ = interval.tick() => {
                        let output = self.tmux_async_cmd()
                            .args(["capture-pane", "-p", "-e", "-t", &self.target])
                            .output()
                            .await;

                        match output {
                            Ok(out) if out.status.success() => {
                                let capture = String::from_utf8_lossy(&out.stdout)
                                    .into_owned();
                                if capture != prev_capture {
                                    prev_capture = capture.clone();
                                    let frame = format!("\x1b[H\x1b[2J{capture}");
                                    if output_tx.send(Bytes::from(frame)).await.is_err() {
                                        return Ok(ExitStatus {
                                            code: None,
                                            signal: None,
                                        });
                                    }
                                }
                            }
                            _ => {
                                // Session is gone
                                return Ok(ExitStatus {
                                    code: None,
                                    signal: None,
                                });
                            }
                        }
                    }
                    data = input_rx.recv() => {
                        match data {
                            Some(BackendInput::Write(bytes)) => {
                                let text = String::from_utf8_lossy(&bytes);
                                let status = self.tmux_async_cmd()
                                    .args([
                                        "send-keys", "-l", "-t", &self.target, &text,
                                    ])
                                    .stdout(std::process::Stdio::null())
                                    .stderr(std::process::Stdio::null())
                                    .status()
                                    .await;
                                if status.is_err() {
                                    return Ok(ExitStatus {
                                        code: None,
                                        signal: None,
                                    });
                                }
                            }
                            Some(BackendInput::Drain(tx)) => {
                                let _ = tx.send(());
                            }
                            None => {
                                return Ok(ExitStatus {
                                    code: None,
                                    signal: None,
                                });
                            }
                        }
                    }
                    resize = resize_rx.recv() => {
                        if let Some((cols, rows)) = resize {
                            let _ = self.tmux_async_cmd()
                                .args([
                                    "resize-pane", "-t", &self.target,
                                    "-x", &cols.to_string(),
                                    "-y", &rows.to_string(),
                                ])
                                .stdout(std::process::Stdio::null())
                                .stderr(std::process::Stdio::null())
                                .status()
                                .await;
                        }
                    }
                }
            }
        })
    }

    fn resize(&self, cols: u16, rows: u16) -> anyhow::Result<()> {
        let status = self
            .tmux_cmd()
            .args([
                "resize-pane",
                "-t",
                &self.target,
                "-x",
                &cols.to_string(),
                "-y",
                &rows.to_string(),
            ])
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()?;

        if !status.success() {
            anyhow::bail!("tmux resize-pane failed");
        }
        Ok(())
    }

    fn child_pid(&self) -> Option<u32> {
        let output = self
            .tmux_cmd()
            .args(["display-message", "-p", "-t", &self.target, "#{pane_pid}"])
            .output()
            .ok()?;

        if !output.status.success() {
            return None;
        }

        let text = String::from_utf8_lossy(&output.stdout);
        text.trim().parse().ok()
    }
}

#[cfg(test)]
#[path = "adapter_tests.rs"]
mod tests;
