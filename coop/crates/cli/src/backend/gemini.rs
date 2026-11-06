// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Gemini ACP backend: drive `gemini --acp` without a PTY (ADR 0005 §4, §11).
//!
//! Presents the same terminal-shaped [`Backend`] contract as a PTY, exactly as
//! [`super::codex`] does: input bytes pass through a [`LineDiscipline`] (text +
//! `\r` becomes a prompt, Escape / Ctrl-C becomes `session/cancel`), output is a
//! compact transcript for the screen, and the raw JSON lines (plus the client's
//! synthetic notifications) are teed to the Tier 3 detector.

use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;

use anyhow::Context;
use bytes::Bytes;
use serde_json::Value;
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Child;
use tokio::sync::mpsc;

use super::codex::{drain_resizes, exit_status, log_stderr, spawn_child, write_json, Emitter};
use super::{Backend, BackendInput};
use crate::driver::codex::rpc::{InputAction, LineDiscipline};
use crate::driver::gemini::acp::{AcpClient, Approval, AuthMethod, ClientConfig, Reaction};
use crate::driver::gemini::parse::render_for_screen;
use crate::driver::gemini::session::{find_session, gemini_home};
use crate::driver::ExitStatus;

/// Node heap cap, in MiB, applied when the environment sets none. The CLI sizes
/// its own heap from host memory (3.9 GiB on the spike host), not from the
/// 2 GiB pod limit, so an unbounded run is OOM-killed (spike R1, advice
/// gd-QLhMjEsBsR). Override with `NODE_OPTIONS`.
const DEFAULT_HEAP_MIB: u32 = 1024;

/// Options for the ACP backend.
#[derive(Debug, Clone, Default)]
pub struct GeminiBackendOptions {
    pub cwd: String,
    pub model: Option<String>,
    /// Gemini session id to resume.
    pub resume_session: Option<String>,
    pub approval: Approval,
    /// Slot to publish the session record path into once it exists, so
    /// transcript snapshots and S3 uploads follow the real file.
    pub session_slot: Option<SessionSlot>,
}

/// Shared slot the backend publishes the session record path into.
pub type SessionSlot = Arc<tokio::sync::RwLock<Option<PathBuf>>>;

/// Long-lived `gemini --acp` driven over stdio.
pub struct GeminiAcpBackend {
    child: Option<Child>,
    pid: Option<u32>,
    options: GeminiBackendOptions,
    tee: Option<mpsc::Sender<Bytes>>,
    home: PathBuf,
}

/// `extra_env` plus the Node heap cap, unless `NODE_OPTIONS` is already set.
fn child_env(extra_env: &[(String, String)]) -> Vec<(String, String)> {
    let mut env = extra_env.to_vec();
    let has_heap = |opts: &str| opts.contains("--max-old-space-size");
    let set = env
        .iter()
        .find(|(k, _)| k == "NODE_OPTIONS")
        .map(|(_, v)| v.clone())
        .or_else(|| std::env::var("NODE_OPTIONS").ok());
    if !set.as_deref().is_some_and(has_heap) {
        let opts = match set {
            Some(existing) if !existing.is_empty() => {
                format!("{existing} --max-old-space-size={DEFAULT_HEAP_MIB}")
            }
            _ => format!("--max-old-space-size={DEFAULT_HEAP_MIB}"),
        };
        env.retain(|(k, _)| k != "NODE_OPTIONS");
        env.push(("NODE_OPTIONS".to_owned(), opts));
    }
    env
}

impl GeminiAcpBackend {
    /// Spawn `command` (e.g. `gemini --acp`) with piped stdio.
    pub fn spawn(
        command: &[String],
        extra_env: &[(String, String)],
        options: GeminiBackendOptions,
        tee: Option<mpsc::Sender<Bytes>>,
    ) -> anyhow::Result<Self> {
        let env = child_env(extra_env);
        let home = env
            .iter()
            .find(|(k, _)| k == "GEMINI_CLI_HOME")
            .map(|(_, v)| PathBuf::from(v).join(".gemini"))
            .unwrap_or_else(gemini_home);
        let mut child = spawn_child(command, &env)?;
        log_stderr(&mut child);
        let pid = child.id();
        Ok(Self { child: Some(child), pid, options, tee, home })
    }

    /// Publish the session record path once Gemini has created the file. ACP
    /// reports no path, so find it by session id: the file may not exist until
    /// the first message, hence the retry on every turn end until it does.
    async fn publish_session(&self, client: &AcpClient) {
        let (Some(slot), Some(id)) = (&self.options.session_slot, client.session_id()) else {
            return;
        };
        if slot.read().await.is_some() {
            return;
        }
        if let Some(path) = find_session(&self.home, id) {
            *slot.write().await = Some(path);
        }
    }
}

/// True when `reaction` ends a turn or opens the session, the moments the
/// session record may have appeared.
fn session_may_exist(reaction: &Reaction) -> bool {
    reaction.synthetic.iter().any(|s| {
        matches!(s.get("method").and_then(Value::as_str), Some("coop/ready" | "coop/turnCompleted"))
    })
}

impl Backend for GeminiAcpBackend {
    fn run(
        &mut self,
        output_tx: mpsc::Sender<Bytes>,
        mut input_rx: mpsc::Receiver<BackendInput>,
        resize_rx: mpsc::Receiver<(u16, u16)>,
    ) -> Pin<Box<dyn Future<Output = anyhow::Result<ExitStatus>> + Send + '_>> {
        Box::pin(async move {
            drain_resizes(resize_rx);
            let mut child = self.child.take().context("gemini --acp already consumed")?;
            let mut stdin = child.stdin.take().context("gemini --acp stdin unavailable")?;
            let stdout = child.stdout.take().context("gemini --acp stdout unavailable")?;
            let mut lines = BufReader::new(stdout).lines();
            let out =
                Emitter { screen: output_tx, tee: self.tee.take(), render: render_for_screen };

            let mut client = AcpClient::new(ClientConfig {
                cwd: self.options.cwd.clone(),
                model: self.options.model.clone(),
                resume_session: self.options.resume_session.clone(),
                approval: self.options.approval,
                auth: AuthMethod::from_env(),
            });
            let mut discipline = LineDiscipline::default();

            let mut alive = true;
            for msg in client.start() {
                alive &= write_json(&mut stdin, &msg).await.is_ok();
            }

            while alive {
                tokio::select! {
                    line = lines.next_line() => {
                        let Ok(Some(line)) = line else { break };
                        let Ok(value) = serde_json::from_str::<Value>(&line) else {
                            // The CLI prints the odd non-protocol line on stdout.
                            tracing::debug!(target: "coop::gemini", "stdout: {line}");
                            continue;
                        };
                        let reaction = client.on_message(&value);
                        if reaction.forward && !out.json(&value).await {
                            break;
                        }
                        for synthetic in &reaction.synthetic {
                            if !out.json(synthetic).await {
                                alive = false;
                                break;
                            }
                        }
                        if session_may_exist(&reaction) {
                            self.publish_session(&client).await;
                        }
                        for msg in &reaction.send {
                            if write_json(&mut stdin, msg).await.is_err() {
                                alive = false;
                                break;
                            }
                        }
                    }
                    input = input_rx.recv() => {
                        match input {
                            Some(BackendInput::Write(bytes)) => {
                                for action in discipline.feed(&bytes) {
                                    let reaction = match action {
                                        InputAction::Submit(text) => client.submit(text),
                                        InputAction::Interrupt => client.interrupt(),
                                    };
                                    for synthetic in &reaction.synthetic {
                                        out.json(synthetic).await;
                                    }
                                    for msg in &reaction.send {
                                        if write_json(&mut stdin, msg).await.is_err() {
                                            alive = false;
                                            break;
                                        }
                                    }
                                }
                            }
                            Some(BackendInput::Drain(tx)) => {
                                let _ = tx.send(());
                            }
                            None => break,
                        }
                    }
                }
            }

            drop(stdin);
            // The CLI re-spawns itself as a child worker; killing only the
            // launcher orphans the worker (spike R1). Both share the process
            // group `spawn_child` created, so end the whole group.
            if let Some(pid) = self.pid {
                let _ = nix::sys::signal::killpg(
                    nix::unistd::Pid::from_raw(pid as i32),
                    nix::sys::signal::Signal::SIGTERM,
                );
            }
            Ok(exit_status(child.wait().await?))
        })
    }

    fn resize(&self, _cols: u16, _rows: u16) -> anyhow::Result<()> {
        Ok(())
    }

    fn child_pid(&self) -> Option<u32> {
        self.pid
    }
}

#[cfg(test)]
#[path = "gemini_tests.rs"]
mod tests;
