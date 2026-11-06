// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::{json, Value};

use super::*;

const BASIC: &str = include_str!("../../../../../tests/fixtures/gemini/acp/basic.jsonl");
const PERM: &str = include_str!("../../../../../tests/fixtures/gemini/acp/perm.jsonl");
const CANCEL: &str = include_str!("../../../../../tests/fixtures/gemini/acp/cancel.jsonl");

/// The agent-to-client frames of a capture, in order.
fn from_agent(capture: &str) -> Vec<Value> {
    capture
        .lines()
        .filter_map(|l| serde_json::from_str::<Value>(l).ok())
        .filter(|l| l["dir"] == "<-")
        .map(|l| l["frame"].clone())
        .collect()
}

/// The first captured agent frame answering request `id`, re-addressed to `to`.
fn response(capture: &str, id: i64, to: i64) -> Value {
    let mut frame = from_agent(capture)
        .into_iter()
        .find(|f| f["id"] == id && f.get("method").is_none())
        .unwrap_or(Value::Null);
    frame["id"] = json!(to);
    frame
}

fn updates(capture: &str) -> Vec<Value> {
    from_agent(capture).into_iter().filter(|f| f["method"] == "session/update").collect()
}

fn config() -> ClientConfig {
    ClientConfig { cwd: "/work".into(), ..ClientConfig::default() }
}

fn methods(msgs: &[Value]) -> Vec<&str> {
    msgs.iter().filter_map(|m| m["method"].as_str()).collect()
}

fn synthetic_methods(r: &Reaction) -> Vec<&str> {
    methods(&r.synthetic)
}

/// Drive a client through the handshake of `capture` (ids 1-3).
fn opened(capture: &str, cfg: ClientConfig) -> (AcpClient, Reaction) {
    let mut client = AcpClient::new(cfg);
    client.start();
    client.on_message(&response(capture, 1, 1));
    client.on_message(&response(capture, 2, 2));
    let ready = client.on_message(&response(capture, 3, 3));
    (client, ready)
}

#[test]
fn handshake_follows_the_captured_sequence() {
    let mut client = AcpClient::new(config());
    let first = client.start();
    assert_eq!(methods(&first), ["initialize"]);
    assert_eq!(first[0]["params"]["protocolVersion"], 1);
    assert_eq!(first[0]["params"]["clientCapabilities"]["fs"]["readTextFile"], false);

    let r = client.on_message(&response(BASIC, 1, 1));
    assert_eq!(methods(&r.send), ["authenticate"]);
    assert_eq!(r.send[0]["params"]["methodId"], "gemini-api-key");

    let r = client.on_message(&response(BASIC, 2, 2));
    assert_eq!(methods(&r.send), ["session/new"]);
    assert_eq!(r.send[0]["params"], json!({ "cwd": "/work", "mcpServers": [] }));

    // The session opens: yolo is selected and the detector is told it is ready.
    let r = client.on_message(&response(BASIC, 3, 3));
    assert_eq!(methods(&r.send), ["session/set_mode"]);
    assert_eq!(r.send[0]["params"]["modeId"], "yolo");
    assert_eq!(synthetic_methods(&r), ["coop/ready"]);
    assert_eq!(client.session_id(), Some("b70b593d-8f2c-4dba-84ab-1cce54577642"));
}

#[test]
fn oauth_login_authenticates_with_oauth_personal() {
    let cfg = ClientConfig { auth: AuthMethod::OauthPersonal, ..config() };
    let mut client = AcpClient::new(cfg);
    client.start();
    let r = client.on_message(&response(BASIC, 1, 1));
    assert_eq!(methods(&r.send), ["authenticate"]);
    assert_eq!(r.send[0]["params"]["methodId"], "oauth-personal");
}

#[test]
fn auth_method_ids() {
    assert_eq!(AuthMethod::default().id(), "gemini-api-key");
    assert_eq!(AuthMethod::OauthPersonal.id(), "oauth-personal");
}

#[test]
fn deny_policy_leaves_the_session_mode_alone() {
    let cfg = ClientConfig { approval: Approval::Deny, ..config() };
    let (_, ready) = opened(BASIC, cfg);
    assert!(ready.send.is_empty(), "{:?}", ready.send);
}

#[test]
fn model_is_selected_after_the_session_opens() {
    let cfg = ClientConfig { model: Some("gemini-2.5-pro".into()), ..config() };
    let (_, ready) = opened(BASIC, cfg);
    assert_eq!(methods(&ready.send), ["session/set_mode", "session/set_model"]);
    assert_eq!(ready.send[1]["params"]["modelId"], "gemini-2.5-pro");
}

#[test]
fn a_prompt_turn_reports_message_and_usage() {
    let (mut client, _) = opened(BASIC, config());

    let r = client.submit("say pong".into());
    assert_eq!(methods(&r.send), ["session/prompt"]);
    assert_eq!(r.send[0]["params"]["prompt"], json!([{ "type": "text", "text": "say pong" }]));
    assert_eq!(synthetic_methods(&r), ["coop/prompt", "coop/turnStarted"]);
    assert!(client.turn_active());
    let prompt_id = r.send[0]["id"].as_i64().unwrap_or(-1);

    for update in updates(BASIC) {
        let r = client.on_message(&update);
        assert!(r.forward && r.send.is_empty() && r.synthetic.is_empty());
    }

    let r = client.on_message(&response(BASIC, 4, prompt_id));
    assert_eq!(synthetic_methods(&r), ["coop/agentMessage", "coop/turnCompleted"]);
    assert_eq!(r.synthetic[0]["params"]["text"], "pong");
    let done = &r.synthetic[1]["params"];
    assert_eq!(done["stopReason"], "end_turn");
    assert_eq!(done["usage"]["inputTokens"], 100);
    assert_eq!(done["usage"]["outputTokens"], 3);
    assert_eq!(done["continuing"], false);
    assert!(!client.turn_active());
}

#[test]
fn input_typed_mid_turn_waits_for_the_turn_to_end() {
    // A second session/prompt would cancel the first (acp/queue.jsonl).
    let (mut client, _) = opened(BASIC, config());
    let first = client.submit("one".into());
    let id = first.send[0]["id"].as_i64().unwrap_or(-1);

    let queued = client.submit("two".into());
    assert!(queued.send.is_empty(), "must not send a second prompt: {:?}", queued.send);
    assert!(queued.synthetic.is_empty());

    let r = client.on_message(&response(BASIC, 4, id));
    // The finished turn says another is coming, so the detector will not dip to Idle.
    let completed = r.synthetic.iter().find(|s| s["method"] == "coop/turnCompleted");
    assert_eq!(completed.map(|c| c["params"]["continuing"].clone()), Some(json!(true)));
    assert_eq!(methods(&r.send), ["session/prompt"]);
    assert_eq!(r.send[0]["params"]["prompt"][0]["text"], "two");
}

#[test]
fn text_submitted_before_the_session_is_open_is_sent_once_it_is() {
    let mut client = AcpClient::new(config());
    client.start();
    assert!(client.submit("early".into()).send.is_empty());
    client.on_message(&response(BASIC, 1, 1));
    client.on_message(&response(BASIC, 2, 2));
    let r = client.on_message(&response(BASIC, 3, 3));
    assert!(methods(&r.send).contains(&"session/prompt"), "{:?}", r.send);
    assert_eq!(synthetic_methods(&r), ["coop/ready", "coop/prompt", "coop/turnStarted"]);
}

#[test]
fn interrupt_cancels_only_a_running_turn() {
    let (mut client, _) = opened(CANCEL, config());
    assert!(client.interrupt().send.is_empty());

    client.submit("MOCK:SLOW".into());
    let r = client.interrupt();
    assert_eq!(methods(&r.send), ["session/cancel"]);
    assert!(r.send[0].get("id").is_none(), "session/cancel is a notification");
    assert_eq!(r.send[0]["params"]["sessionId"], "b8b6ff5c-14c8-410b-91c0-4ff059097f42");
}

#[test]
fn a_cancelled_turn_keeps_the_previous_message() {
    let (mut client, _) = opened(CANCEL, config());
    let sent = client.submit("MOCK:SLOW".into());
    let id = sent.send[0]["id"].as_i64().unwrap_or(-1);
    for update in updates(CANCEL) {
        client.on_message(&update);
    }
    client.interrupt();
    let r = client.on_message(&response(CANCEL, 4, id));
    // "working..." is a fragment: not offered as the last message.
    assert_eq!(synthetic_methods(&r), ["coop/turnCompleted"]);
    assert_eq!(r.synthetic[0]["params"]["stopReason"], "cancelled");
}

#[test]
fn a_failed_prompt_is_a_coop_error_and_frees_the_queue() {
    let (mut client, _) = opened(BASIC, config());
    let id = client.submit("a".into()).send[0]["id"].as_i64().unwrap_or(-1);
    client.submit("b".into());
    let r = client.on_message(&json!({
        "jsonrpc": "2.0", "id": id,
        "error": { "code": -32603, "message": "[API Error: Resource has been exhausted (e.g. check quota).]" }
    }));
    assert_eq!(synthetic_methods(&r)[0], "coop/error");
    let message = r.synthetic[0]["params"]["message"].as_str().unwrap_or_default();
    assert!(
        message.contains("session/prompt failed") && message.contains("exhausted"),
        "{message}"
    );
    // The queued prompt still goes out.
    assert_eq!(methods(&r.send), ["session/prompt"]);
}

#[test]
fn a_failed_handshake_step_is_a_coop_error() {
    let mut client = AcpClient::new(config());
    client.start();
    let r =
        client.on_message(&json!({"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"nope"}}));
    assert_eq!(synthetic_methods(&r), ["coop/error"]);
    assert!(r.send.is_empty());
}

fn permission_request() -> Value {
    from_agent(PERM)
        .into_iter()
        .find(|f| f["method"] == "session/request_permission")
        .unwrap_or(Value::Null)
}

#[test]
fn permission_requests_are_allowed_once_by_default() {
    let (mut client, _) = opened(PERM, config());
    let r = client.on_message(&permission_request());
    assert_eq!(r.send.len(), 1);
    assert_eq!(r.send[0]["id"], 0, "answers the agent's request id");
    assert_eq!(
        r.send[0]["result"],
        json!({ "outcome": { "outcome": "selected", "optionId": "proceed_once" } })
    );
    assert_eq!(r.synthetic[0]["method"], "coop/permission");
    assert_eq!(r.synthetic[0]["params"]["outcome"], "allowed");
}

#[test]
fn permission_requests_are_rejected_under_the_deny_policy() {
    let cfg = ClientConfig { approval: Approval::Deny, ..config() };
    let (mut client, _) = opened(PERM, cfg);
    let r = client.on_message(&permission_request());
    assert_eq!(r.send[0]["result"]["outcome"]["optionId"], "cancel");
    assert_eq!(r.synthetic[0]["params"]["outcome"], "denied");
}

#[test]
fn permission_request_without_a_usable_option_is_cancelled() {
    let (mut client, _) = opened(PERM, config());
    let r = client.on_message(&json!({
        "jsonrpc": "2.0", "id": 9, "method": "session/request_permission",
        "params": { "options": [{ "optionId": "x", "name": "x", "kind": "reject_once" }] }
    }));
    assert_eq!(r.send[0]["result"], json!({ "outcome": { "outcome": "cancelled" } }));
}

#[test]
fn unsupported_agent_requests_get_an_error_reply() {
    let (mut client, _) = opened(BASIC, config());
    let r = client.on_message(
        &json!({"jsonrpc":"2.0","id":7,"method":"fs/read_text_file","params":{"path":"/x"}}),
    );
    assert_eq!(r.send[0]["id"], 7);
    assert_eq!(r.send[0]["error"]["code"], -32601);
}

#[test]
fn resume_loads_the_session_and_swallows_the_replayed_history() {
    let sid = "11111111-2222-4333-8444-555555555555";
    let mut client = AcpClient::new(ClientConfig { resume_session: Some(sid.into()), ..config() });
    client.start();
    // The captured initialize advertises loadSession.
    client.on_message(&response(BASIC, 1, 1));
    let r = client.on_message(&response(BASIC, 2, 2));
    assert_eq!(methods(&r.send), ["session/load"]);
    assert_eq!(r.send[0]["params"]["sessionId"], sid);
    assert_eq!(r.send[0]["params"]["cwd"], "/work");

    // History replays as updates before the response: invisible, no message.
    let replay = client.on_message(&updates(BASIC)[1]);
    assert!(!replay.forward);

    let ready = client.on_message(&json!({"jsonrpc":"2.0","id":3,"result":{}}));
    assert_eq!(synthetic_methods(&ready), ["coop/ready"]);
    assert_eq!(client.session_id(), Some(sid));
    // After the load, the same traffic is live again.
    assert!(client.on_message(&updates(BASIC)[1]).forward);
    // And the replayed text did not leak into the next turn's message.
    let id = client.submit("go".into()).send[0]["id"].as_i64().unwrap_or(-1);
    let r = client.on_message(&json!({"jsonrpc":"2.0","id":id,"result":{"stopReason":"end_turn"}}));
    assert_eq!(synthetic_methods(&r), ["coop/turnCompleted"]);
}

#[test]
fn resume_starts_fresh_when_the_agent_cannot_load_sessions() {
    let cfg = ClientConfig { resume_session: Some("abc".into()), ..config() };
    let mut client = AcpClient::new(cfg);
    client.start();
    let mut init = response(BASIC, 1, 1);
    init["result"]["agentCapabilities"]["loadSession"] = json!(false);
    client.on_message(&init);
    let r = client.on_message(&response(BASIC, 2, 2));
    assert_eq!(methods(&r.send), ["session/new"]);
    assert!(r.send[0]["params"].get("sessionId").is_none());
}

#[test]
fn responses_nobody_asked_for_are_ignored() {
    let (mut client, _) = opened(BASIC, config());
    let r = client.on_message(&json!({"jsonrpc":"2.0","id":999,"result":{}}));
    assert_eq!(r, Reaction::default());
}
