// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::{json, Value};

use super::{AppServerClient, ClientConfig, InputAction, LineDiscipline, Reaction};

const APP_SERVER: &str =
    include_str!("../../../../../tests/fixtures/codex/app-server-session.jsonl");

fn client() -> AppServerClient {
    AppServerClient::new(ClientConfig { cwd: Some("/workspace".into()), ..Default::default() })
}

fn method(v: &Value) -> &str {
    v["method"].as_str().unwrap_or_default()
}

/// Drive the handshake to a ready thread; returns the client.
fn ready_client() -> anyhow::Result<AppServerClient> {
    let mut c = client();
    let init = c.start();
    assert_eq!(init.len(), 1);
    let id = init[0]["id"].clone();
    let r = c.on_message(&json!({"id": id, "result": {"userAgent": "x"}}));
    let thread_id = r.send[1]["id"].clone();
    c.on_message(
        &json!({"id": thread_id, "result": {"thread": {"id": "T1", "path": "/r/rollout.jsonl"}}}),
    );
    Ok(c)
}

#[test]
fn handshake_initializes_then_opens_a_never_approve_thread() -> anyhow::Result<()> {
    let mut c = client();
    let init = c.start();
    assert_eq!(method(&init[0]), "initialize");
    assert_eq!(init[0]["params"]["clientInfo"]["name"], "coop");

    let r = c.on_message(&json!({"id": init[0]["id"], "result": {}}));
    assert_eq!(method(&r.send[0]), "initialized");
    assert_eq!(method(&r.send[1]), "thread/start");
    let p = &r.send[1]["params"];
    assert_eq!(p["approvalPolicy"], "never");
    assert_eq!(p["sandbox"], "danger-full-access");
    assert_eq!(p["cwd"], "/workspace");

    let ready = c.on_message(
        &json!({"id": r.send[1]["id"], "result": {"thread": {"id": "T1", "path": "/r.jsonl"}}}),
    );
    assert_eq!(method(&ready.synthetic[0]), "coop/ready");
    assert_eq!(c.thread_id(), Some("T1"));
    assert_eq!(c.rollout_path(), Some("/r.jsonl"));
    Ok(())
}

#[test]
fn resume_opens_thread_resume_with_the_session_id() -> anyhow::Result<()> {
    let mut c = AppServerClient::new(ClientConfig {
        resume_thread: Some("S-9".into()),
        model: Some("m".into()),
        ..Default::default()
    });
    let init = c.start();
    let r = c.on_message(&json!({"id": init[0]["id"], "result": {}}));
    assert_eq!(method(&r.send[1]), "thread/resume");
    assert_eq!(r.send[1]["params"]["threadId"], "S-9");
    assert_eq!(r.send[1]["params"]["model"], "m");
    assert_eq!(r.send[1]["params"]["approvalPolicy"], "never");
    Ok(())
}

#[test]
fn real_thread_start_response_from_the_fixture_opens_the_thread() -> anyhow::Result<()> {
    let lines: Vec<Value> =
        APP_SERVER.lines().map(serde_json::from_str).collect::<Result<_, _>>()?;
    let mut c = client();
    let init = c.start();
    let mut init_resp = lines[0].clone();
    init_resp["id"] = init[0]["id"].clone();
    let r = c.on_message(&init_resp);
    let mut thread_resp = lines[3].clone();
    thread_resp["id"] = r.send[1]["id"].clone();
    c.on_message(&thread_resp);
    assert_eq!(c.thread_id(), Some("01a13cb7-104f-7611-8852-5d9a6d492166"));
    assert!(c.rollout_path().is_some_and(|p| p.ends_with(".jsonl")));
    Ok(())
}

#[test]
fn text_submitted_before_the_thread_is_ready_starts_a_turn_once_it_is() -> anyhow::Result<()> {
    let mut c = client();
    let init = c.start();
    assert_eq!(c.submit("early".into()), Reaction::default());
    let r = c.on_message(&json!({"id": init[0]["id"], "result": {}}));
    let ready = c.on_message(&json!({"id": r.send[1]["id"], "result": {"thread": {"id": "T1"}}}));
    assert_eq!(method(&ready.send[0]), "turn/start");
    assert_eq!(ready.send[0]["params"]["threadId"], "T1");
    assert_eq!(ready.send[0]["params"]["input"][0]["text"], "early");
    Ok(())
}

#[test]
fn idle_thread_starts_a_turn_and_busy_thread_steers() -> anyhow::Result<()> {
    let mut c = ready_client()?;
    let r = c.submit("one".into());
    assert_eq!(method(&r.send[0]), "turn/start");

    // turn/start response lands; the turn is in flight.
    c.on_message(
        &json!({"id": r.send[0]["id"], "result": {"turn": {"id": "U1", "status": "inProgress"}}}),
    );
    assert!(c.turn_active());

    let r = c.submit("two".into());
    assert_eq!(method(&r.send[0]), "turn/steer");
    assert_eq!(r.send[0]["params"]["expectedTurnId"], "U1");
    assert_eq!(r.send[0]["params"]["threadId"], "T1");
    assert_eq!(r.send[0]["params"]["input"][0]["text"], "two");

    c.on_message(&json!({"method": "turn/completed", "params": {"turn": {"id": "U1", "status": "completed"}}}));
    assert!(!c.turn_active());
    let r = c.submit("three".into());
    assert_eq!(method(&r.send[0]), "turn/start");
    Ok(())
}

#[test]
fn text_queued_behind_an_inflight_turn_start_steers_after_it_lands() -> anyhow::Result<()> {
    let mut c = ready_client()?;
    let first = c.submit("one".into());
    // No response yet: a second message must not race a second turn/start.
    assert_eq!(c.submit("two".into()), Reaction::default());
    let r = c.on_message(&json!({"id": first.send[0]["id"], "result": {"turn": {"id": "U1", "status": "inProgress"}}}));
    assert_eq!(method(&r.send[0]), "turn/steer");
    assert_eq!(r.send[0]["params"]["input"][0]["text"], "two");
    Ok(())
}

#[test]
fn rejected_steer_falls_back_to_a_fresh_turn() -> anyhow::Result<()> {
    let mut c = ready_client()?;
    let start = c.submit("one".into());
    c.on_message(&json!({"id": start.send[0]["id"], "result": {"turn": {"id": "U1", "status": "inProgress"}}}));
    let steer = c.submit("late".into());
    let r = c.on_message(
        &json!({"id": steer.send[0]["id"], "error": {"code": -32600, "message": "no active turn"}}),
    );
    assert_eq!(method(&r.send[0]), "turn/start");
    assert_eq!(r.send[0]["params"]["input"][0]["text"], "late");
    Ok(())
}

#[test]
fn interrupt_targets_the_active_turn_and_is_a_noop_when_idle() -> anyhow::Result<()> {
    let mut c = ready_client()?;
    assert_eq!(c.interrupt(), Reaction::default());
    c.on_message(
        &json!({"method": "turn/started", "params": {"threadId": "T1", "turn": {"id": "U7"}}}),
    );
    let r = c.interrupt();
    assert_eq!(method(&r.send[0]), "turn/interrupt");
    assert_eq!(r.send[0]["params"], json!({"threadId": "T1", "turnId": "U7"}));
    Ok(())
}

#[test]
fn interrupt_during_turn_start_is_applied_when_it_lands() -> anyhow::Result<()> {
    let mut c = ready_client()?;
    let start = c.submit("one".into());
    assert_eq!(c.interrupt(), Reaction::default());
    let r = c.on_message(&json!({"id": start.send[0]["id"], "result": {"turn": {"id": "U1", "status": "inProgress"}}}));
    assert_eq!(method(&r.send[0]), "turn/interrupt");
    assert_eq!(r.send[0]["params"]["turnId"], "U1");
    Ok(())
}

fn server_request(id: i64, method: &str) -> Value {
    json!({"id": id, "method": method, "params": {"threadId": "T1"}})
}

#[test]
fn approvals_are_declined_and_surfaced_as_errors() -> anyhow::Result<()> {
    let mut c = ready_client()?;
    let cases = [
        ("item/commandExecution/requestApproval", json!({"decision": "decline"})),
        ("item/fileChange/requestApproval", json!({"decision": "decline"})),
        ("applyPatchApproval", json!({"decision": "denied"})),
        ("execCommandApproval", json!({"decision": "denied"})),
        ("item/permissions/requestApproval", json!({"permissions": {}, "scope": "turn"})),
    ];
    for (i, (m, want)) in cases.into_iter().enumerate() {
        let id = 100 + i as i64;
        let r = c.on_message(&server_request(id, m));
        assert_eq!(r.send, vec![json!({"id": id, "result": want})], "{m}");
        assert_eq!(method(&r.synthetic[0]), "coop/approvalDenied", "{m}");
        assert_eq!(r.synthetic[0]["params"]["request"], m);
    }
    Ok(())
}

#[test]
fn other_server_requests_are_answered_so_no_turn_can_hang() -> anyhow::Result<()> {
    let mut c = ready_client()?;
    let r = c.on_message(&server_request(5, "item/tool/requestUserInput"));
    assert_eq!(r.send, vec![json!({"id": 5, "result": {"answers": {}}})]);
    let r = c.on_message(&server_request(6, "mcpServer/elicitation/request"));
    assert_eq!(r.send, vec![json!({"id": 6, "result": {"action": "decline"}})]);
    let r = c.on_message(&server_request(7, "item/tool/call"));
    assert_eq!(r.send[0]["result"]["success"], false);
    for m in ["account/chatgptAuthTokens/refresh", "attestation/generate", "no/such/method"] {
        let r = c.on_message(&server_request(8, m));
        assert_eq!(r.send[0]["id"], 8, "{m}");
        assert!(r.send[0]["error"]["message"].is_string(), "{m}");
        assert!(r.synthetic.is_empty(), "{m}");
    }
    Ok(())
}

#[test]
fn handshake_failures_surface_as_errors() -> anyhow::Result<()> {
    let mut c = client();
    let init = c.start();
    let r = c.on_message(&json!({"id": init[0]["id"], "error": {"code": 1, "message": "nope"}}));
    assert_eq!(method(&r.synthetic[0]), "coop/error");
    assert!(r.synthetic[0]["params"]["message"].as_str().unwrap_or_default().contains("nope"));
    assert!(r.send.is_empty());
    Ok(())
}

fn feed(ld: &mut LineDiscipline, s: &[u8]) -> Vec<InputAction> {
    ld.feed(s)
}

#[test]
fn line_discipline_submits_on_enter() {
    let mut ld = LineDiscipline::default();
    assert_eq!(feed(&mut ld, b"hel"), vec![]);
    assert_eq!(feed(&mut ld, b"lo\r"), vec![InputAction::Submit("hello".into())]);
    assert_eq!(feed(&mut ld, b"\r"), vec![], "empty lines are dropped");
}

#[test]
fn line_discipline_keeps_embedded_newlines_and_unicode() {
    let mut ld = LineDiscipline::default();
    assert_eq!(feed(&mut ld, "a\nb ✓\r".as_bytes()), vec![InputAction::Submit("a\nb ✓".into())]);
}

#[test]
fn line_discipline_escape_and_ctrl_c_interrupt() {
    let mut ld = LineDiscipline::default();
    assert_eq!(feed(&mut ld, b"draft\x1b"), vec![InputAction::Interrupt]);
    assert_eq!(feed(&mut ld, b"\r"), vec![], "interrupt discards the draft");
    assert_eq!(feed(&mut ld, b"\x03"), vec![InputAction::Interrupt]);
}

#[test]
fn line_discipline_swallows_escape_sequences() {
    let mut ld = LineDiscipline::default();
    assert_eq!(
        feed(&mut ld, b"a\x1b[Ab\x1bOPc\x1b[200~d\x1b[201~\r"),
        vec![InputAction::Submit("abcd".into())]
    );
}

#[test]
fn line_discipline_escape_then_text_is_interrupt_then_input() {
    let mut ld = LineDiscipline::default();
    assert_eq!(
        feed(&mut ld, b"\x1bx\r"),
        vec![InputAction::Interrupt, InputAction::Submit("x".into())]
    );
}

#[test]
fn line_discipline_backspace_edits() {
    let mut ld = LineDiscipline::default();
    assert_eq!(feed(&mut ld, "abc✓\x7f\x7f\r".as_bytes()), vec![InputAction::Submit("ab".into())]);
}
