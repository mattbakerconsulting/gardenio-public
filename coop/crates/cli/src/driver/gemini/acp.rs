// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Sans-IO Agent Client Protocol client for `gemini --acp` (ADR 0005 §4, §11).
//!
//! Like [`crate::driver::codex::rpc`], the client never touches a file
//! descriptor: callers feed it server messages and user intents and write out
//! whatever [`Reaction::send`] returns. The process plumbing lives in
//! [`crate::backend::gemini`]; the wire shapes are pinned by
//! `coop/tests/fixtures/gemini/acp/`.
//!
//! Policy implemented here:
//! - handshake: `initialize` -> `authenticate` (API key, taken from the
//!   environment by the CLI) -> `session/new`, or `session/load` when a Gemini
//!   session id was supplied and the agent advertises `loadSession`;
//! - tool approvals: the session is switched to `yolo` (the pod is the sandbox)
//!   and any `session/request_permission` that still arrives is answered by
//!   [`Approval`] policy, so a turn can never hang on an unanswered request;
//! - **input is serialised**: ACP has no queue, and a second `session/prompt`
//!   while one is running *cancels* the first (fixture `acp/queue.jsonl`), so
//!   text typed mid-turn waits for the turn to finish;
//! - interrupt -> `session/cancel`.
//!
//! ACP has no "turn started" or "turn ended" notification (a turn ends when the
//! `session/prompt` *response* arrives), so the client injects synthetic
//! notifications for the Tier 3 detector: `coop/ready`, `coop/prompt`,
//! `coop/turnStarted`, `coop/agentMessage`, `coop/turnCompleted`, `coop/error`
//! and `coop/permission`. [`super::parse`] understands them.

use std::collections::{HashMap, VecDeque};

use serde_json::{json, Value};

/// Environment variable naming the model to pin for the session.
pub const MODEL_ENV: &str = "GEMINI_MODEL";

/// What the I/O glue must do after feeding the client an event.
#[derive(Debug, PartialEq)]
pub struct Reaction {
    /// JSON-RPC messages to write to the agent's stdin (one per line).
    pub send: Vec<Value>,
    /// Synthetic notifications to surface to the detector/screen alongside the
    /// agent's own traffic.
    pub synthetic: Vec<Value>,
    /// `false` when the incoming message must not reach the detector or the
    /// screen: the history `session/load` replays is not new activity.
    pub forward: bool,
}

impl Default for Reaction {
    fn default() -> Self {
        Self { send: Vec::new(), synthetic: Vec::new(), forward: true }
    }
}

impl Reaction {
    fn extend(&mut self, other: Reaction) {
        self.send.extend(other.send);
        self.synthetic.extend(other.synthetic);
    }
}

/// How a `session/request_permission` that reaches coop is answered.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum Approval {
    /// Allow it once. The default: the pod is the sandbox (ADR 0005 §8).
    #[default]
    Allow,
    /// Reject it; the tool call fails and the model sees the refusal.
    Deny,
}

impl Approval {
    /// Read `COOP_GEMINI_APPROVAL` (`allow` | `deny`); anything else is `Allow`.
    pub fn from_env() -> Self {
        match std::env::var("COOP_GEMINI_APPROVAL").as_deref() {
            Ok("deny") => Self::Deny,
            _ => Self::Allow,
        }
    }
}

/// Which ACP `authenticate` method the client asks for. The Gemini CLI offers
/// both (`initialize` -> `authMethods`); which one is usable depends on the
/// credential the pod carries.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum AuthMethod {
    /// `GEMINI_API_KEY` from coop's credential push (metered).
    #[default]
    ApiKey,
    /// Google-account login: `~/.gemini/oauth_creds.json`, refreshed by the CLI
    /// itself (free tier or a Google AI / Code Assist subscription).
    OauthPersonal,
}

impl AuthMethod {
    /// ACP method id for `authenticate`.
    pub fn id(self) -> &'static str {
        match self {
            Self::ApiKey => "gemini-api-key",
            Self::OauthPersonal => "oauth-personal",
        }
    }

    /// Read `GEMINI_AUTH_MODE` (`oauth` selects the Google-account login);
    /// anything else is the API key.
    pub fn from_env() -> Self {
        match std::env::var("GEMINI_AUTH_MODE").as_deref() {
            Ok("oauth") => Self::OauthPersonal,
            _ => Self::ApiKey,
        }
    }
}

/// Static parameters of the session the client opens.
#[derive(Debug, Clone, Default)]
pub struct ClientConfig {
    /// Working directory for the session.
    pub cwd: String,
    /// Model to select after the session opens.
    pub model: Option<String>,
    /// Existing Gemini session id to resume instead of starting fresh.
    pub resume_session: Option<String>,
    pub approval: Approval,
    pub auth: AuthMethod,
}

#[derive(Debug)]
enum Pending {
    Initialize,
    Authenticate,
    SessionNew,
    SessionLoad,
    /// `session/set_mode` / `session/set_model`: best effort, errors ignored.
    Tuning,
    Prompt,
}

#[derive(Debug, PartialEq, Eq)]
enum Phase {
    Handshake,
    /// `session/load` is in flight: its `session/update`s replay old history.
    Loading,
    Ready,
}

/// Protocol state for one `gemini --acp` connection.
pub struct AcpClient {
    cfg: ClientConfig,
    next_id: i64,
    phase: Phase,
    pending: HashMap<i64, Pending>,
    session_id: Option<String>,
    /// The agent advertised `agentCapabilities.loadSession`.
    can_load: bool,
    /// User text waiting for the session or for the running turn to end.
    queued: VecDeque<String>,
    /// The `session/prompt` in flight, if any.
    active_prompt: Option<i64>,
    /// Assistant text of the running turn, assembled from message chunks.
    message: String,
}

impl AcpClient {
    pub fn new(cfg: ClientConfig) -> Self {
        Self {
            cfg,
            next_id: 0,
            phase: Phase::Handshake,
            pending: HashMap::new(),
            session_id: None,
            can_load: false,
            queued: VecDeque::new(),
            active_prompt: None,
            message: String::new(),
        }
    }

    /// The Gemini session id, once the session is open.
    pub fn session_id(&self) -> Option<&str> {
        self.session_id.as_deref()
    }

    /// True while a prompt is running.
    pub fn turn_active(&self) -> bool {
        self.active_prompt.is_some()
    }

    fn request(&mut self, method: &str, params: Value, pending: Pending) -> Value {
        self.next_id += 1;
        self.pending.insert(self.next_id, pending);
        json!({ "jsonrpc": "2.0", "id": self.next_id, "method": method, "params": params })
    }

    fn notification(method: &str, params: Value) -> Value {
        json!({ "jsonrpc": "2.0", "method": method, "params": params })
    }

    /// The first message of the connection.
    pub fn start(&mut self) -> Vec<Value> {
        let params = json!({
            "protocolVersion": 1,
            "clientCapabilities": { "fs": { "readTextFile": false, "writeTextFile": false } },
        });
        vec![self.request("initialize", params, Pending::Initialize)]
    }

    /// Queue or send a user message.
    pub fn submit(&mut self, text: String) -> Reaction {
        self.queued.push_back(text);
        self.drain()
    }

    /// Cancel the running turn, if any. Queued text still runs afterwards.
    pub fn interrupt(&mut self) -> Reaction {
        let mut out = Reaction::default();
        if let (Some(session), Some(_)) = (&self.session_id, self.active_prompt) {
            out.send.push(Self::notification("session/cancel", json!({ "sessionId": session })));
        }
        out
    }

    /// Start the next queued prompt if the protocol state allows it.
    fn drain(&mut self) -> Reaction {
        let mut out = Reaction::default();
        if self.phase != Phase::Ready || self.active_prompt.is_some() {
            return out;
        }
        let Some(session) = self.session_id.clone() else { return out };
        let Some(text) = self.queued.pop_front() else { return out };
        let params = json!({ "sessionId": session, "prompt": [{ "type": "text", "text": text }] });
        out.send.push(self.request("session/prompt", params, Pending::Prompt));
        self.active_prompt = Some(self.next_id);
        self.message.clear();
        out.synthetic.push(synthetic("coop/prompt", json!({ "text": text })));
        out.synthetic.push(synthetic("coop/turnStarted", json!({})));
        out
    }

    /// Handle one message from the agent.
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
        let mut out = Reaction::default();
        if method != "session/update" {
            return out;
        }
        if self.phase == Phase::Loading {
            out.forward = false;
            return out;
        }
        let update = msg.get("params").and_then(|p| p.get("update"));
        let kind = update.and_then(|u| u.get("sessionUpdate")).and_then(Value::as_str);
        if kind == Some("agent_message_chunk") && self.active_prompt.is_some() {
            let chunk = update
                .and_then(|u| u.get("content"))
                .and_then(|c| c.get("text"))
                .and_then(Value::as_str);
            self.message.push_str(chunk.unwrap_or_default());
        }
        out
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
                self.can_load = result
                    .get("agentCapabilities")
                    .and_then(|c| c.get("loadSession"))
                    .and_then(Value::as_bool)
                    .unwrap_or(false);
                let params = json!({ "methodId": self.cfg.auth.id() });
                out.send.push(self.request("authenticate", params, Pending::Authenticate));
            }
            Pending::Authenticate => {
                if let Some(e) = error {
                    out.synthetic.push(fatal("authenticate", e));
                    return out;
                }
                out.send.push(self.open_session());
            }
            Pending::SessionNew => {
                if let Some(e) = error {
                    out.synthetic.push(fatal("session/new", e));
                    return out;
                }
                self.session_id =
                    result.get("sessionId").and_then(Value::as_str).map(str::to_owned);
                out.extend(self.on_session_open());
            }
            Pending::SessionLoad => {
                if let Some(e) = error {
                    out.synthetic.push(fatal("session/load", e));
                    return out;
                }
                // `session/load` answers with no session id: it is the one we asked for.
                self.session_id = self.cfg.resume_session.clone();
                out.extend(self.on_session_open());
            }
            Pending::Tuning => {}
            Pending::Prompt => {
                self.active_prompt = None;
                out.extend(self.finish_turn(error, result));
                out.extend(self.drain());
            }
        }
        out
    }

    /// `session/new`, or `session/load` when resuming and the agent can.
    fn open_session(&mut self) -> Value {
        let mut params = json!({ "cwd": self.cfg.cwd, "mcpServers": [] });
        match self.cfg.resume_session.clone() {
            Some(id) if self.can_load => {
                params["sessionId"] = json!(id);
                self.phase = Phase::Loading;
                self.request("session/load", params, Pending::SessionLoad)
            }
            Some(_) => {
                tracing::warn!("gemini: agent cannot load sessions; starting a fresh one");
                self.cfg.resume_session = None;
                self.request("session/new", params, Pending::SessionNew)
            }
            None => self.request("session/new", params, Pending::SessionNew),
        }
    }

    /// The session exists: tune it, announce readiness, and start queued input.
    fn on_session_open(&mut self) -> Reaction {
        let mut out = Reaction::default();
        self.phase = Phase::Ready;
        let Some(session) = self.session_id.clone() else {
            out.synthetic.push(synthetic(
                "coop/error",
                json!({ "message": "gemini acp session opened without a session id" }),
            ));
            return out;
        };
        // `session/load` may answer without a `modes` block, so do not gate on
        // it: an agent without `yolo` just errors, which `Tuning` ignores.
        if self.cfg.approval == Approval::Allow {
            let params = json!({ "sessionId": session, "modeId": "yolo" });
            out.send.push(self.request("session/set_mode", params, Pending::Tuning));
        }
        if let Some(model) = self.cfg.model.clone() {
            let params = json!({ "sessionId": session, "modelId": model });
            out.send.push(self.request("session/set_model", params, Pending::Tuning));
        }
        out.synthetic.push(synthetic("coop/ready", json!({ "sessionId": session })));
        out.extend(self.drain());
        out
    }

    /// Turn the `session/prompt` response into the turn-end synthetics.
    fn finish_turn(&mut self, error: Option<&Value>, result: &Value) -> Reaction {
        let mut out = Reaction::default();
        if let Some(e) = error {
            out.synthetic.push(fatal("session/prompt", e));
            return out;
        }
        let stop_reason = result.get("stopReason").and_then(Value::as_str).unwrap_or("end_turn");
        // A cancelled turn's text is a fragment; keep the previous full reply.
        let message = std::mem::take(&mut self.message);
        if stop_reason != "cancelled" && !message.trim().is_empty() {
            out.synthetic.push(synthetic("coop/agentMessage", json!({ "text": message })));
        }
        out.synthetic.push(synthetic(
            "coop/turnCompleted",
            json!({
                "stopReason": stop_reason,
                "usage": result.get("usage").cloned().unwrap_or(Value::Null),
                "continuing": !self.queued.is_empty(),
            }),
        ));
        out
    }

    /// Answer a request from the agent by policy.
    fn on_server_request(&mut self, id: &Value, method: &str, msg: &Value) -> Reaction {
        let params = msg.get("params").unwrap_or(&Value::Null);
        let mut out = Reaction::default();
        let response = match method {
            "session/request_permission" => {
                let (result, allowed) = self.answer_permission(params);
                let title = params
                    .get("toolCall")
                    .and_then(|t| t.get("title"))
                    .and_then(Value::as_str)
                    .unwrap_or("tool");
                out.synthetic.push(synthetic(
                    "coop/permission",
                    json!({ "outcome": if allowed { "allowed" } else { "denied" }, "title": title }),
                ));
                json!({ "jsonrpc": "2.0", "id": id, "result": result })
            }
            _ => json!({
                "jsonrpc": "2.0",
                "id": id,
                "error": { "code": -32601, "message": "method not supported by coop" },
            }),
        };
        out.send.push(response);
        out
    }

    /// Pick an option for a permission request; the bool is "allowed".
    fn answer_permission(&self, params: &Value) -> (Value, bool) {
        let options = params.get("options").and_then(Value::as_array);
        let pick = |kinds: &[&str]| {
            kinds.iter().find_map(|kind| {
                options?
                    .iter()
                    .find(|o| o.get("kind").and_then(Value::as_str) == Some(kind))
                    .and_then(|o| o.get("optionId").and_then(Value::as_str))
            })
        };
        let wanted: &[&str] = match self.cfg.approval {
            Approval::Allow => &["allow_once", "allow_always"],
            Approval::Deny => &["reject_once", "reject_always"],
        };
        match pick(wanted) {
            Some(option) => (
                json!({ "outcome": { "outcome": "selected", "optionId": option } }),
                self.cfg.approval == Approval::Allow,
            ),
            None => (json!({ "outcome": { "outcome": "cancelled" } }), false),
        }
    }
}

fn synthetic(method: &str, params: Value) -> Value {
    json!({ "method": method, "params": params })
}

fn fatal(what: &str, error: &Value) -> Value {
    let message = error.get("message").and_then(Value::as_str).unwrap_or("unknown error");
    synthetic("coop/error", json!({ "message": format!("gemini acp {what} failed: {message}") }))
}

#[cfg(test)]
#[path = "acp_tests.rs"]
mod tests;
