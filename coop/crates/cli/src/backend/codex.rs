// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Codex backends: drive `codex` without a PTY (ADR 0004 §3).
//!
//! Both backends present the same terminal-shaped [`Backend`] contract as a
//! PTY so every existing coop surface (nudge, `/input`, `/input/keys`, the
//! ring buffer, recordings) keeps working:
//!
//! - **input** bytes pass through a [`LineDiscipline`]: text + `\r` becomes a
//!   user message, Escape / Ctrl-C becomes an interrupt;
//! - **output** is a compact human-readable transcript (a Codex pod has no TUI),
//!   while the raw JSON lines are teed to the Tier 3 detector.
//!
//! [`CodexAppServerBackend`] (primary) speaks JSON-RPC to one long-lived
//! `codex app-server`. [`CodexExecBackend`] (fallback) runs `codex exec --json`
//! once per turn, resuming the same thread, because `exec` has no interrupt or
//! steer short of killing the process.

use std::future::Future;
use std::pin::Pin;
use std::process::Stdio;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;

use anyhow::Context;
use bytes::Bytes;
use serde_json::Value;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin, Command};
use tokio::sync::mpsc;

use super::{Backend, BackendInput};
use crate::driver::codex::parse::render_for_screen;
use crate::driver::codex::rpc::{
    AppServerClient, ClientConfig, InputAction, LineDiscipline, Reaction,
};
use crate::driver::ExitStatus;

/// Where the backend's output goes: the terminal side and the detector tee.
pub(super) struct Emitter {
    pub(super) screen: mpsc::Sender<Bytes>,
    /// Raw JSON lines for the Tier 3 detector. `None` when no detector listens.
    pub(super) tee: Option<mpsc::Sender<Bytes>>,
    /// One-line rendering of a JSON object for the screen (harness-specific).
    pub(super) render: fn(&Value) -> Option<String>,
}

impl Emitter {
    /// Surface one JSON object: raw to the detector, rendered to the screen.
    /// Returns `false` once the screen side is gone (session shutting down).
    pub(super) async fn json(&self, value: &Value) -> bool {
        if let Some(tee) = &self.tee {
            let mut line = value.to_string().into_bytes();
            line.push(b'\n');
            let _ = tee.send(Bytes::from(line)).await;
        }
        match (self.render)(value) {
            Some(text) => self.text(&text).await,
            None => true,
        }
    }

    /// Surface a plain line on the screen only.
    pub(super) async fn text(&self, line: &str) -> bool {
        // Terminal line endings, and no embedded newlines breaking the layout.
        let rendered = format!("{}\r\n", line.replace('\n', "\r\n"));
        self.screen.send(Bytes::from(rendered)).await.is_ok()
    }

    async fn reaction(&self, reaction: &Reaction) -> bool {
        for synthetic in &reaction.synthetic {
            if !self.json(synthetic).await {
                return false;
            }
        }
        true
    }
}

pub(super) fn exit_status(status: std::process::ExitStatus) -> ExitStatus {
    use std::os::unix::process::ExitStatusExt;
    ExitStatus { code: status.code(), signal: status.signal() }
}

pub(super) fn spawn_child(
    command: &[String],
    extra_env: &[(String, String)],
) -> anyhow::Result<Child> {
    let (program, args) = command.split_first().context("no command specified")?;
    let mut cmd = Command::new(program);
    cmd.args(args)
        .env_remove(crate::driver::codex::auth::AUTH_ENV)
        .env("COOP", "1")
        .envs(extra_env.iter().map(|(k, v)| (k.as_str(), v.as_str())))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    // Own process group (what forkpty's setsid gives the PTY agents): the
    // session loop restarts and stops the agent with `kill(-pid, ..)`, which
    // finds nothing for a child that shares coop's group. Without this a
    // credential switch on a Codex session never took effect.
    cmd.process_group(0);
    cmd.spawn().with_context(|| format!("failed to spawn {program}"))
}

/// Forward a child's stderr to the log; it is diagnostics, not protocol.
pub(super) fn log_stderr(child: &mut Child) {
    if let Some(stderr) = child.stderr.take() {
        tokio::spawn(async move {
            let mut lines = BufReader::new(stderr).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                tracing::debug!(target: "coop::codex", "stderr: {line}");
            }
        });
    }
}

pub(super) async fn write_json(stdin: &mut ChildStdin, value: &Value) -> std::io::Result<()> {
    let mut line = value.to_string().into_bytes();
    line.push(b'\n');
    stdin.write_all(&line).await?;
    stdin.flush().await
}

/// Nothing reads terminal resizes for a PTY-less backend, but the sender must
/// not block.
pub(super) fn drain_resizes(mut resize_rx: mpsc::Receiver<(u16, u16)>) {
    tokio::spawn(async move { while resize_rx.recv().await.is_some() {} });
}

/// Options shared by both Codex backends.
#[derive(Debug, Clone, Default)]
pub struct CodexBackendOptions {
    pub cwd: Option<String>,
    pub model: Option<String>,
    /// Codex session (thread) id to resume.
    pub resume_thread: Option<String>,
    /// Slot to publish the rollout file path into once the thread opens, so
    /// transcript snapshots and S3 uploads follow the real file.
    pub rollout_slot: Option<RolloutSlot>,
}

/// Shared slot the backend publishes the thread's rollout path into.
pub type RolloutSlot = Arc<tokio::sync::RwLock<Option<std::path::PathBuf>>>;

/// Long-lived `codex app-server` driven over JSON-RPC on stdio.
pub struct CodexAppServerBackend {
    child: Option<Child>,
    pid: Option<u32>,
    options: CodexBackendOptions,
    tee: Option<mpsc::Sender<Bytes>>,
}

impl CodexAppServerBackend {
    /// Spawn `command` (e.g. `codex app-server`) with piped stdio.
    pub fn spawn(
        command: &[String],
        extra_env: &[(String, String)],
        options: CodexBackendOptions,
        tee: Option<mpsc::Sender<Bytes>>,
    ) -> anyhow::Result<Self> {
        let mut child = spawn_child(command, extra_env)?;
        log_stderr(&mut child);
        let pid = child.id();
        Ok(Self { child: Some(child), pid, options, tee })
    }
}

impl Backend for CodexAppServerBackend {
    fn run(
        &mut self,
        output_tx: mpsc::Sender<Bytes>,
        mut input_rx: mpsc::Receiver<BackendInput>,
        resize_rx: mpsc::Receiver<(u16, u16)>,
    ) -> Pin<Box<dyn Future<Output = anyhow::Result<ExitStatus>> + Send + '_>> {
        Box::pin(async move {
            drain_resizes(resize_rx);
            let mut child = self.child.take().context("codex app-server already consumed")?;
            let mut stdin = child.stdin.take().context("codex app-server stdin unavailable")?;
            let stdout = child.stdout.take().context("codex app-server stdout unavailable")?;
            let mut lines = BufReader::new(stdout).lines();
            let out =
                Emitter { screen: output_tx, tee: self.tee.take(), render: render_for_screen };

            let mut client = AppServerClient::new(ClientConfig {
                cwd: self.options.cwd.clone(),
                model: self.options.model.clone(),
                resume_thread: self.options.resume_thread.clone(),
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
                            if !out.text(&line).await { break }
                            continue;
                        };
                        let reaction = client.on_message(&value);
                        if let (Some(slot), Some(path)) = (&self.options.rollout_slot, client.rollout_path()) {
                            let mut guard = slot.write().await;
                            if guard.is_none() {
                                *guard = Some(std::path::PathBuf::from(path));
                            }
                        }
                        if !out.json(&value).await || !out.reaction(&reaction).await {
                            break;
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
                            None => {
                                let _ = child.start_kill();
                                break;
                            }
                        }
                    }
                }
            }

            drop(stdin);
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

/// Per-turn `codex exec --json` fallback.
///
/// `command` is the invocation up to and including its options, e.g.
/// `codex exec --yolo`. Each user message runs
/// `codex exec [resume] <options> --json [<thread-id>] -` with the text on
/// stdin; the thread id is learned from the first `thread.started` event, so
/// every turn after the first resumes the same Codex session.
pub struct CodexExecBackend {
    command: Vec<String>,
    extra_env: Vec<(String, String)>,
    thread_id: Option<String>,
    rollout_slot: Option<RolloutSlot>,
    tee: Option<mpsc::Sender<Bytes>>,
    pid: Arc<AtomicU32>,
}

impl CodexExecBackend {
    pub fn new(
        command: &[String],
        extra_env: &[(String, String)],
        options: CodexBackendOptions,
        tee: Option<mpsc::Sender<Bytes>>,
    ) -> anyhow::Result<Self> {
        anyhow::ensure!(!command.is_empty(), "no command specified");
        Ok(Self {
            command: command.to_vec(),
            extra_env: extra_env.to_vec(),
            thread_id: options.resume_thread,
            rollout_slot: options.rollout_slot,
            tee,
            pid: Arc::new(AtomicU32::new(0)),
        })
    }

    /// argv for one turn. `codex exec resume` takes its options after the
    /// subcommand, so they are repeated there (the spike's `--yolo` finding).
    pub fn turn_command(command: &[String], thread_id: Option<&str>) -> Vec<String> {
        let mut argv: Vec<String> = command.to_vec();
        // `command` is `[codex, exec, options...]`.
        let insert_at = argv.iter().position(|a| a == "exec").map_or(argv.len(), |i| i + 1);
        if thread_id.is_some() {
            argv.insert(insert_at, "resume".to_owned());
        }
        argv.push("--json".to_owned());
        if let Some(id) = thread_id {
            argv.push(id.to_owned());
        }
        argv.push("-".to_owned());
        argv
    }

    /// Publish the rollout path once Codex has created the file. `exec --json`
    /// never reports it, so find it by thread id: the file may not exist at
    /// `thread.started`, hence the retry on every turn end until it does.
    async fn publish_rollout(&self) {
        let (Some(slot), Some(id)) = (&self.rollout_slot, &self.thread_id) else { return };
        if slot.read().await.is_some() {
            return;
        }
        let home = self
            .extra_env
            .iter()
            .find(|(k, _)| k == "CODEX_HOME")
            .map(|(_, v)| std::path::PathBuf::from(v))
            .unwrap_or_else(crate::driver::codex::rollout::codex_home);
        if let Some(path) = crate::driver::codex::rollout::find_rollout(&home, id) {
            *slot.write().await = Some(path);
        }
    }

    /// Run one turn to completion, teeing its events while still servicing
    /// input: text queues up, Escape / Ctrl-C kills the turn's process.
    ///
    /// Returns `false` once the input side has closed (session shutdown).
    async fn run_turn(
        &mut self,
        text: &str,
        out: &Emitter,
        input_rx: &mut mpsc::Receiver<BackendInput>,
        discipline: &mut LineDiscipline,
        queue: &mut std::collections::VecDeque<String>,
    ) -> anyhow::Result<bool> {
        let argv = Self::turn_command(&self.command, self.thread_id.as_deref());
        let mut child = spawn_child(&argv, &self.extra_env)?;
        log_stderr(&mut child);
        self.pid.store(child.id().unwrap_or(0), Ordering::Release);
        if let Some(mut stdin) = child.stdin.take() {
            stdin.write_all(text.as_bytes()).await?;
            // Dropping stdin sends EOF, which terminates the prompt.
        }
        let stdout = child.stdout.take().context("codex exec stdout unavailable")?;
        let mut lines = BufReader::new(stdout).lines();

        let mut input_open = true;
        let mut interrupted = false;
        let mut finished = false;
        loop {
            tokio::select! {
                line = lines.next_line() => {
                    let Ok(Some(line)) = line else { break };
                    let Ok(value) = serde_json::from_str::<Value>(&line) else {
                        out.text(&line).await;
                        continue;
                    };
                    match value.get("type").and_then(Value::as_str) {
                        Some("thread.started") => {
                            if let Some(id) = value.get("thread_id").and_then(Value::as_str) {
                                self.thread_id = Some(id.to_owned());
                            }
                        }
                        Some("turn.completed" | "turn.failed") => finished = true,
                        _ => {}
                    }
                    if finished || value.get("type").and_then(Value::as_str) == Some("thread.started") {
                        self.publish_rollout().await;
                    }
                    if !out.json(&value).await {
                        input_open = false;
                        let _ = child.start_kill();
                        break;
                    }
                }
                input = input_rx.recv(), if input_open => {
                    match input {
                        Some(BackendInput::Write(bytes)) => {
                            for action in discipline.feed(&bytes) {
                                match action {
                                    InputAction::Submit(text) => queue.push_back(text),
                                    InputAction::Interrupt => {
                                        interrupted = true;
                                        let _ = child.start_kill();
                                    }
                                }
                            }
                        }
                        Some(BackendInput::Drain(tx)) => {
                            let _ = tx.send(());
                        }
                        None => {
                            input_open = false;
                            let _ = child.start_kill();
                        }
                    }
                }
            }
        }
        let status = child.wait().await;
        self.pid.store(0, Ordering::Release);

        // A killed or crashed process never emits its own terminal event, so
        // synthesize one or the detector would stay Working forever.
        if !finished && input_open {
            let synthetic = if interrupted {
                serde_json::json!({
                    "method": "turn/completed",
                    "params": { "turn": { "status": "interrupted" } }
                })
            } else {
                let how = status.map_or_else(|e| e.to_string(), |s| s.to_string());
                serde_json::json!({
                    "method": "coop/error",
                    "params": { "message": format!("codex exec ended without completing the turn ({how})") }
                })
            };
            out.json(&synthetic).await;
        }
        Ok(input_open)
    }
}

impl Backend for CodexExecBackend {
    fn run(
        &mut self,
        output_tx: mpsc::Sender<Bytes>,
        mut input_rx: mpsc::Receiver<BackendInput>,
        resize_rx: mpsc::Receiver<(u16, u16)>,
    ) -> Pin<Box<dyn Future<Output = anyhow::Result<ExitStatus>> + Send + '_>> {
        Box::pin(async move {
            drain_resizes(resize_rx);
            let out =
                Emitter { screen: output_tx, tee: self.tee.take(), render: render_for_screen };
            let mut discipline = LineDiscipline::default();
            let mut queue = std::collections::VecDeque::<String>::new();

            // No process runs between turns; announce readiness ourselves.
            out.json(&serde_json::json!({
                "method": "coop/ready",
                "params": { "threadId": self.thread_id }
            }))
            .await;

            loop {
                let Some(text) = queue.pop_front() else {
                    match input_rx.recv().await {
                        Some(BackendInput::Write(bytes)) => {
                            for action in discipline.feed(&bytes) {
                                if let InputAction::Submit(text) = action {
                                    queue.push_back(text);
                                }
                            }
                        }
                        Some(BackendInput::Drain(tx)) => {
                            let _ = tx.send(());
                        }
                        None => break,
                    }
                    continue;
                };
                match self.run_turn(&text, &out, &mut input_rx, &mut discipline, &mut queue).await {
                    Ok(true) => {}
                    Ok(false) => break,
                    Err(e) => {
                        out.json(&serde_json::json!({
                            "method": "coop/error",
                            "params": { "message": format!("codex exec failed: {e}") }
                        }))
                        .await;
                    }
                }
            }
            Ok(ExitStatus { code: Some(0), signal: None })
        })
    }

    fn resize(&self, _cols: u16, _rows: u16) -> anyhow::Result<()> {
        Ok(())
    }

    fn child_pid(&self) -> Option<u32> {
        match self.pid.load(Ordering::Acquire) {
            0 => None,
            pid => Some(pid),
        }
    }
}
