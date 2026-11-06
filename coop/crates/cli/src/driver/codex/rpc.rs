// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Sans-IO `codex app-server` JSON-RPC client (ADR 0004 §3).
//!
//! The client never touches a file descriptor: callers feed it server messages
//! and user intents and write out whatever [`Reaction::send`] returns. This keeps
//! the protocol logic unit-testable against the committed fixture; the process
//! plumbing lives in [`crate::backend::codex`].
//!
//! Policy implemented here:
//! - handshake: `initialize` → `initialized` → `thread/start` (or
//!   `thread/resume` when a Codex session id was supplied);
//! - always `approvalPolicy: "never"` + `sandbox: "danger-full-access"`: the pod
//!   is the sandbox;
//! - user text → `turn/steer` when a turn is in flight, else `turn/start`;
//! - interrupt → `turn/interrupt`;
//! - every server-to-client request is answered by policy so a turn can never
//!   hang on an unanswered request. Approvals are declined *and* surfaced as a
//!   synthetic `coop/approvalDenied` notification (an error state).

use std::collections::{HashMap, VecDeque};

use serde_json::{json, Value};

/// What the I/O glue must do after feeding the client an event.
#[derive(Debug, Default, PartialEq)]
pub struct Reaction {
    /// JSON-RPC messages to write to the server's stdin (one per line).
    pub send: Vec<Value>,
    /// Synthetic notifications to surface to the detector/screen alongside the
    /// server's own traffic.
    pub synthetic: Vec<Value>,
}

impl Reaction {
    fn extend(&mut self, other: Reaction) {
        self.send.extend(other.send);
        self.synthetic.extend(other.synthetic);
    }
}

/// Static parameters of the thread the client opens.
#[derive(Debug, Clone, Default)]
pub struct ClientConfig {
    /// Working directory for the thread.
    pub cwd: Option<String>,
    /// Model override (`thread/start` `model`).
    pub model: Option<String>,
    /// Existing Codex session (thread) id to resume instead of starting fresh.
    pub resume_thread: Option<String>,
}

#[derive(Debug)]
enum Pending {
    Initialize,
    Thread,
    TurnStart,
    TurnSteer { text: String },
    Interrupt,
}

#[derive(Debug, PartialEq, Eq)]
enum Phase {
    Handshake,
    Ready,
}

/// Protocol state for one app-server connection.
pub struct AppServerClient {
    cfg: ClientConfig,
    next_id: i64,
    phase: Phase,
    pending: HashMap<i64, Pending>,
    thread_id: Option<String>,
    /// Path of the thread's rollout file, from the `thread/start` response.
    rollout_path: Option<String>,
    active_turn: Option<String>,
    /// User text waiting for the thread (or an in-flight `turn/start`).
    queued: VecDeque<String>,
    /// An interrupt arrived while `turn/start` was still in flight.
    interrupt_after_start: bool,
}

impl AppServerClient {
    pub fn new(cfg: ClientConfig) -> Self {
        Self {
            cfg,
            next_id: 0,
            phase: Phase::Handshake,
            pending: HashMap::new(),
            thread_id: None,
            rollout_path: None,
            active_turn: None,
            queued: VecDeque::new(),
            interrupt_after_start: false,
        }
    }

    /// The Codex thread id (== session id), once the thread is open.
    pub fn thread_id(&self) -> Option<&str> {
        self.thread_id.as_deref()
    }

    /// The rollout file path (`thread.path`), once the thread is open.
    pub fn rollout_path(&self) -> Option<&str> {
        self.rollout_path.as_deref()
    }

    /// True while a turn is running.
    pub fn turn_active(&self) -> bool {
        self.active_turn.is_some()
    }

    fn request(&mut self, method: &str, params: Value, pending: Pending) -> Value {
        self.next_id += 1;
        self.pending.insert(self.next_id, pending);
        json!({ "id": self.next_id, "method": method, "params": params })
    }

    /// The first message of the connection.
    pub fn start(&mut self) -> Vec<Value> {
        let params =
            json!({ "clientInfo": { "name": "coop", "version": env!("CARGO_PKG_VERSION") } });
        vec![self.request("initialize", params, Pending::Initialize)]
    }

    /// Queue or send a user message.
    pub fn submit(&mut self, text: String) -> Reaction {
        self.queued.push_back(text);
        self.drain()
    }

    /// Interrupt the running turn, if any.
    pub fn interrupt(&mut self) -> Reaction {
        let mut out = Reaction::default();
        match (self.thread_id.clone(), self.active_turn.clone()) {
            (Some(thread), Some(turn)) => {
                let params = json!({ "threadId": thread, "turnId": turn });
                out.send.push(self.request("turn/interrupt", params, Pending::Interrupt));
            }
            _ => {
                if self.pending.values().any(|p| matches!(p, Pending::TurnStart)) {
                    self.interrupt_after_start = true;
                }
            }
        }
        out
    }

    /// Send queued messages as far as the protocol state allows.
    fn drain(&mut self) -> Reaction {
        let mut out = Reaction::default();
        if self.phase != Phase::Ready {
            return out;
        }
        let Some(thread) = self.thread_id.clone() else { return out };
        while let Some(text) = self.queued.front() {
            let input = json!([{ "type": "text", "text": text }]);
            if let Some(turn) = self.active_turn.clone() {
                let text = self.queued.pop_front().unwrap_or_default();
                let params = json!({ "threadId": thread, "expectedTurnId": turn, "input": input });
                out.send.push(self.request("turn/steer", params, Pending::TurnSteer { text }));
            } else if self.pending.values().any(|p| matches!(p, Pending::TurnStart)) {
                // One turn/start at a time: later text steers once it lands.
                break;
            } else {
                self.queued.pop_front();
                let params = json!({ "threadId": thread, "input": input });
                out.send.push(self.request("turn/start", params, Pending::TurnStart));
            }
        }
        out
    }

    /// Handle one message from the server.
    pub fn on_message(&mut self, msg: &Value) -> Reaction {
        let id = msg.get("id");
        let method = msg.get("method").and_then(Value::as_str);
        match (id, method) {
            (Some(id), Some(method)) => self.on_server_request(id, method, msg),
            (Some(id), None) => self.on_response(id, msg),
            (None, Some(method)) => self.on_notification(method, msg),
            (None, None) => Reaction::default(),
        }
    }

    fn on_notification(&mut self, method: &str, msg: &Value) -> Reaction {
        let params = msg.get("params").unwrap_or(&Value::Null);
        match method {
            "turn/started" => {
                self.active_turn = params
                    .get("turn")
                    .and_then(|t| t.get("id"))
                    .and_then(Value::as_str)
                    .map(str::to_owned);
                Reaction::default()
            }
            "turn/completed" => {
                self.active_turn = None;
                self.drain()
            }
            _ => Reaction::default(),
        }
    }

    fn on_response(&mut self, id: &Value, msg: &Value) -> Reaction {
        let Some(pending) = id.as_i64().and_then(|id| self.pending.remove(&id)) else {
            return Reaction::default();
        };
        let error = msg.get("error");
        let result = msg.get("result").unwrap_or(&Value::Null);
        let mut out = Reaction::default();
        match pending {
            Pending::Initialize => {
                if let Some(e) = error {
                    out.synthetic.push(fatal("initialize", e));
                    return out;
                }
                out.send.push(json!({ "method": "initialized" }));
                out.send.push(self.open_thread());
            }
            Pending::Thread => {
                if let Some(e) = error {
                    out.synthetic.push(fatal("thread/start", e));
                    return out;
                }
                self.thread_id = result
                    .get("thread")
                    .and_then(|t| t.get("id"))
                    .and_then(Value::as_str)
                    .map(str::to_owned);
                self.rollout_path = result
                    .get("thread")
                    .and_then(|t| t.get("path"))
                    .and_then(Value::as_str)
                    .map(str::to_owned);
                self.phase = Phase::Ready;
                out.synthetic.push(json!({
                    "method": "coop/ready",
                    "params": { "threadId": self.thread_id }
                }));
                out.extend(self.drain());
            }
            Pending::TurnStart => {
                if let Some(e) = error {
                    out.synthetic.push(fatal("turn/start", e));
                    return out;
                }
                let turn = result.get("turn");
                let in_progress = turn.and_then(|t| t.get("status")).and_then(Value::as_str)
                    == Some("inProgress");
                if in_progress {
                    self.active_turn =
                        turn.and_then(|t| t.get("id")).and_then(Value::as_str).map(str::to_owned);
                }
                if std::mem::take(&mut self.interrupt_after_start) {
                    out.extend(self.interrupt());
                }
                out.extend(self.drain());
            }
            Pending::TurnSteer { text } => {
                // The turn ended between our view and the server's: the steer
                // is rejected, so deliver the text as a fresh turn.
                if error.is_some() {
                    self.queued.push_front(text);
                    self.active_turn = None;
                    out.extend(self.drain());
                }
            }
            Pending::Interrupt => {}
        }
        out
    }

    fn open_thread(&mut self) -> Value {
        let mut params = json!({ "approvalPolicy": "never", "sandbox": "danger-full-access" });
        if let Some(cwd) = &self.cfg.cwd {
            params["cwd"] = json!(cwd);
        }
        if let Some(model) = &self.cfg.model {
            params["model"] = json!(model);
        }
        match self.cfg.resume_thread.clone() {
            Some(id) => {
                params["threadId"] = json!(id);
                self.request("thread/resume", params, Pending::Thread)
            }
            None => self.request("thread/start", params, Pending::Thread),
        }
    }

    /// Answer a server-to-client request by policy.
    fn on_server_request(&mut self, id: &Value, method: &str, msg: &Value) -> Reaction {
        let params = msg.get("params").unwrap_or(&Value::Null);
        let mut out = Reaction::default();
        let result = match method {
            "item/commandExecution/requestApproval" | "item/fileChange/requestApproval" => {
                out.synthetic.push(approval_denied(method, params));
                Ok(json!({ "decision": "decline" }))
            }
            // Legacy v1 approval requests use `ReviewDecision`.
            "applyPatchApproval" | "execCommandApproval" => {
                out.synthetic.push(approval_denied(method, params));
                Ok(json!({ "decision": "denied" }))
            }
            // Grant nothing.
            "item/permissions/requestApproval" => {
                out.synthetic.push(approval_denied(method, params));
                Ok(json!({ "permissions": {}, "scope": "turn" }))
            }
            "item/tool/requestUserInput" => Ok(json!({ "answers": {} })),
            "mcpServer/elicitation/request" => Ok(json!({ "action": "decline" })),
            "item/tool/call" => Ok(json!({
                "success": false,
                "contentItems": [{ "type": "inputText", "text": "coop: dynamic tool calls are not supported" }]
            })),
            // The host-owned refresh path (ADR 0004 §5) is gated on R3 (unverified
            // protocol shape) and not wired yet (gd-Nt6Hj1ZofH): fail the request;
            // auth.json is the credential path.
            "account/chatgptAuthTokens/refresh" => {
                Err("coop does not own the ChatGPT token refresh; credentials come from auth.json")
            }
            _ => Err("method not supported by coop"),
        };
        out.send.push(match result {
            Ok(result) => json!({ "id": id, "result": result }),
            Err(message) => json!({ "id": id, "error": { "code": -32601, "message": message } }),
        });
        out
    }
}

fn fatal(what: &str, error: &Value) -> Value {
    let message = error.get("message").and_then(Value::as_str).unwrap_or("unknown error");
    json!({ "method": "coop/error", "params": { "message": format!("codex app-server {what} failed: {message}") } })
}

fn approval_denied(method: &str, params: &Value) -> Value {
    json!({ "method": "coop/approvalDenied", "params": { "request": method, "details": params } })
}

/// What a keystroke stream means to a Codex session without a TUI.
#[derive(Debug, PartialEq, Eq)]
pub enum InputAction {
    /// A complete line of user text (Enter pressed).
    Submit(String),
    /// Escape / Ctrl-C: interrupt the running turn.
    Interrupt,
}

/// Terminal line discipline for the PTY-shaped input path.
///
/// coop's nudge/input/keys paths all emit terminal bytes. For a Codex session
/// driven over JSON-RPC (or per-turn `exec`), this turns that stream back into
/// intents: text accumulates until `\r`, a lone ESC or Ctrl-C interrupts, and
/// CSI/SS3 escape sequences (arrows, bracketed-paste markers) are swallowed.
#[derive(Debug, Default)]
pub struct LineDiscipline {
    buf: Vec<u8>,
    in_escape: Option<Escape>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Escape {
    /// Saw ESC, waiting for the introducer byte.
    Esc,
    /// Inside `ESC [` ... until a final byte.
    Csi,
    /// `ESC O` + one byte.
    Ss3,
}

impl LineDiscipline {
    pub fn feed(&mut self, bytes: &[u8]) -> Vec<InputAction> {
        let mut actions = Vec::new();
        for (i, &b) in bytes.iter().enumerate() {
            let last = i + 1 == bytes.len();
            match self.in_escape {
                Some(Escape::Esc) => match b {
                    b'[' => self.in_escape = Some(Escape::Csi),
                    b'O' => self.in_escape = Some(Escape::Ss3),
                    _ => {
                        // ESC followed by an ordinary byte: a bare Escape press,
                        // then the byte is regular input.
                        self.in_escape = None;
                        self.buf.clear();
                        actions.push(InputAction::Interrupt);
                        self.plain(b, last, &mut actions);
                    }
                },
                Some(Escape::Csi) => {
                    if (0x40..=0x7e).contains(&b) {
                        self.in_escape = None;
                    }
                }
                Some(Escape::Ss3) => self.in_escape = None,
                None => self.plain(b, last, &mut actions),
            }
        }
        actions
    }

    fn plain(&mut self, b: u8, last_in_chunk: bool, actions: &mut Vec<InputAction>) {
        match b {
            // A trailing ESC (end of chunk) is a bare Escape press.
            0x1b if last_in_chunk => {
                self.buf.clear();
                actions.push(InputAction::Interrupt);
            }
            0x1b => self.in_escape = Some(Escape::Esc),
            0x03 => {
                self.buf.clear();
                actions.push(InputAction::Interrupt);
            }
            b'\r' => {
                let line = String::from_utf8_lossy(&self.buf).trim().to_owned();
                self.buf.clear();
                if !line.is_empty() {
                    actions.push(InputAction::Submit(line));
                }
            }
            // Backspace: drop one UTF-8 scalar.
            0x7f | 0x08 => {
                while let Some(last) = self.buf.pop() {
                    if last & 0xC0 != 0x80 {
                        break;
                    }
                }
            }
            _ => self.buf.push(b),
        }
    }
}

#[cfg(test)]
#[path = "rpc_tests.rs"]
mod tests;
