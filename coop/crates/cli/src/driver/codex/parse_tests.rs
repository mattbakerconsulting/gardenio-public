// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::{json, Value};

use super::{
    extract_codex_message, format_codex_cause, parse_codex_state, render_for_screen,
    CodexUsageTracker,
};
use crate::driver::{classify_error_detail, AgentState, ErrorCategory};

const APP_SERVER: &str =
    include_str!("../../../../../tests/fixtures/codex/app-server-session.jsonl");
const EXEC_TOOL: &str =
    include_str!("../../../../../tests/fixtures/codex/exec-tool-stop-gate.jsonl");
const EXEC_401: &str =
    include_str!("../../../../../tests/fixtures/codex/exec-turn-failed-401.jsonl");

fn lines(src: &str) -> anyhow::Result<Vec<Value>> {
    src.lines().map(|l| Ok(serde_json::from_str(l)?)).collect()
}

fn states(src: &str) -> anyhow::Result<Vec<AgentState>> {
    Ok(lines(src)?.iter().filter_map(parse_codex_state).collect())
}

#[test]
fn app_server_fixture_cycles_working_and_idle() -> anyhow::Result<()> {
    let got = states(APP_SERVER)?;
    assert!(!got.iter().any(|s| matches!(s, AgentState::Error { .. })), "no errors: {got:?}");
    assert_eq!(got.first(), Some(&AgentState::Idle), "thread/started is idle");
    assert!(got.contains(&AgentState::Working));
    // Three turns (completed, interrupted, completed) each end idle.
    let completed = lines(APP_SERVER)?
        .iter()
        .filter(|j| j["method"] == "turn/completed")
        .filter_map(parse_codex_state)
        .collect::<Vec<_>>();
    assert_eq!(completed, vec![AgentState::Idle; 3]);
    Ok(())
}

#[test]
fn app_server_interrupted_turn_is_idle_not_error() {
    let msg =
        json!({"method":"turn/completed","params":{"turn":{"status":"interrupted","error":null}}});
    assert_eq!(parse_codex_state(&msg), Some(AgentState::Idle));
    assert_eq!(format_codex_cause(&msg), "rpc:idle");
}

#[test]
fn app_server_thread_status_is_ignored_except_system_error() {
    let idle = json!({"method":"thread/status/changed","params":{"status":{"type":"idle"}}});
    let active = json!({"method":"thread/status/changed","params":{"status":{"type":"active","activeFlags":[]}}});
    let sick = json!({"method":"thread/status/changed","params":{"status":{"type":"systemError"}}});
    assert_eq!(parse_codex_state(&idle), None);
    assert_eq!(parse_codex_state(&active), None);
    assert!(matches!(parse_codex_state(&sick), Some(AgentState::Error { .. })));
}

#[test]
fn app_server_deltas_and_noise_carry_no_state() {
    for method in [
        "item/agentMessage/delta",
        "item/commandExecution/outputDelta",
        "thread/tokenUsage/updated",
        "warning",
        "configWarning",
        "remoteControl/status/changed",
    ] {
        let msg = json!({"method": method, "params": {}});
        assert_eq!(parse_codex_state(&msg), None, "{method}");
    }
}

fn failed_turn(info: Value) -> Value {
    json!({"method":"turn/completed","params":{"turn":{
        "status":"failed",
        "error":{"message":"boom","codexErrorInfo":info}
    }}})
}

fn category_of(msg: &Value) -> anyhow::Result<ErrorCategory> {
    match parse_codex_state(msg) {
        Some(AgentState::Error { detail }) => Ok(classify_error_detail(&detail)),
        other => anyhow::bail!("expected Error, got {other:?}"),
    }
}

#[test]
fn failed_turn_error_classes_map_onto_existing_categories() -> anyhow::Result<()> {
    let cases = [
        (json!("unauthorized"), ErrorCategory::Unauthorized),
        (json!("usageLimitExceeded"), ErrorCategory::RateLimited),
        (json!("rateLimitExceeded"), ErrorCategory::RateLimited),
        (json!("sessionBudgetExceeded"), ErrorCategory::BudgetExceeded),
        (json!("serverOverloaded"), ErrorCategory::ServerError),
        (json!("internalServerError"), ErrorCategory::ServerError),
        (json!({"responseStreamDisconnected":{"httpStatusCode":null}}), ErrorCategory::ServerError),
        (json!({"httpConnectionFailed":{"httpStatusCode":null}}), ErrorCategory::NoInternet),
        (json!({"httpConnectionFailed":{"httpStatusCode":401}}), ErrorCategory::Unauthorized),
        (
            json!({"responseStreamConnectionFailed":{"httpStatusCode":403}}),
            ErrorCategory::Unauthorized,
        ),
        (json!({"httpConnectionFailed":{"httpStatusCode":502}}), ErrorCategory::NoInternet),
        (json!("contextWindowExceeded"), ErrorCategory::Other),
        (json!("badRequest"), ErrorCategory::Other),
    ];
    for (info, want) in cases {
        assert_eq!(category_of(&failed_turn(info.clone()))?, want, "{info}");
    }
    Ok(())
}

#[test]
fn typed_class_beats_misleading_prose() -> anyhow::Result<()> {
    // "credits" would classify as OutOfCredits by substring; the tag wins.
    let msg = json!({"method":"turn/completed","params":{"turn":{"status":"failed","error":{
        "message":"You've hit your usage limit. Buy more credits.",
        "codexErrorInfo":"usageLimitExceeded"
    }}}});
    assert_eq!(category_of(&msg)?, ErrorCategory::RateLimited);
    Ok(())
}

#[test]
fn error_notification_respects_will_retry() {
    let retry = json!({"method":"error","params":{"willRetry":true,"error":{"message":"Reconnecting 1/5"}}});
    let fatal = json!({"method":"error","params":{"willRetry":false,"error":{"message":"dead"}}});
    assert_eq!(parse_codex_state(&retry), None);
    assert_eq!(parse_codex_state(&fatal), Some(AgentState::Error { detail: "dead".into() }));
}

#[test]
fn rate_limit_reached_raises_rate_limit_error() -> anyhow::Result<()> {
    let reached = json!({"method":"account/rateLimits/updated","params":{"rateLimits":{"rateLimitReachedType":"primary"}}});
    assert_eq!(category_of(&reached)?, ErrorCategory::RateLimited);
    // The fixture's updates carry rateLimitReachedType:null — no signal.
    assert!(states(APP_SERVER)?.iter().all(|s| !matches!(s, AgentState::Error { .. })));
    Ok(())
}

#[test]
fn approval_denied_is_an_error_state_not_a_prompt() -> anyhow::Result<()> {
    let msg = json!({"method":"coop/approvalDenied","params":{"request":"item/commandExecution/requestApproval"}});
    let Some(AgentState::Error { detail }) = parse_codex_state(&msg) else {
        anyhow::bail!("expected Error");
    };
    assert!(detail.contains("item/commandExecution/requestApproval"), "{detail}");
    assert!(detail.contains("approval_policy=never"), "{detail}");
    Ok(())
}

#[test]
fn exec_tool_fixture_is_working_then_idle() -> anyhow::Result<()> {
    let got = states(EXEC_TOOL)?;
    // The leading `error` item (model-metadata warning) is informational.
    assert_eq!(got.first(), Some(&AgentState::Working), "thread.started");
    assert_eq!(got.last(), Some(&AgentState::Idle), "turn.completed");
    assert!(!got.iter().any(|s| matches!(s, AgentState::Error { .. })));
    Ok(())
}

#[test]
fn exec_401_fixture_fails_only_on_turn_failed() -> anyhow::Result<()> {
    let all = lines(EXEC_401)?;
    for entry in &all {
        let ty = entry["type"].as_str().unwrap_or_default();
        match ty {
            "error" => assert_eq!(parse_codex_state(entry), None, "retry noise"),
            "item.completed" => assert_eq!(parse_codex_state(entry), None, "error item"),
            _ => {}
        }
    }
    let failed = all
        .iter()
        .find(|j| j["type"] == "turn.failed")
        .ok_or_else(|| anyhow::anyhow!("no turn.failed"))?;
    assert_eq!(category_of(failed)?, ErrorCategory::Unauthorized);
    assert_eq!(format_codex_cause(failed), "exec:error");
    Ok(())
}

#[test]
fn exec_turn_failed_infers_class_from_message() -> anyhow::Result<()> {
    let mk = |m: &str| json!({"type":"turn.failed","error":{"message":m}});
    assert_eq!(
        category_of(&mk("You've hit your usage limit. Upgrade to Pro."))?,
        ErrorCategory::RateLimited
    );
    assert_eq!(
        category_of(&mk("Your input exceeds the context window of this model."))?,
        ErrorCategory::Other
    );
    assert_eq!(category_of(&mk("something odd"))?, ErrorCategory::Other);
    Ok(())
}

#[test]
fn extracts_last_assistant_message_from_both_shapes() -> anyhow::Result<()> {
    let rpc: Vec<_> = lines(APP_SERVER)?.iter().filter_map(extract_codex_message).collect();
    assert_eq!(rpc, vec!["app-server reply one", "after interrupt reply"]);
    let exec: Vec<_> = lines(EXEC_TOOL)?.iter().filter_map(extract_codex_message).collect();
    assert_eq!(exec, vec!["first done", "after gate done"]);
    Ok(())
}

#[test]
fn renders_a_readable_transcript() -> anyhow::Result<()> {
    let rendered: Vec<_> = lines(APP_SERVER)?.iter().filter_map(render_for_screen).collect();
    assert!(rendered.contains(&"> hello app-server".to_owned()), "{rendered:?}");
    assert!(rendered.contains(&"● app-server reply one".to_owned()), "{rendered:?}");
    assert!(rendered.contains(&"-- turn interrupted --".to_owned()), "{rendered:?}");
    let exec: Vec<_> = lines(EXEC_TOOL)?.iter().filter_map(render_for_screen).collect();
    assert!(exec.contains(&"$ /bin/bash -lc 'echo hello-from-tool'".to_owned()), "{exec:?}");
    Ok(())
}

#[test]
fn exec_turn_completed_usage_is_one_delta_per_turn() {
    let mut t = CodexUsageTracker::default();
    let done = json!({"type": "turn.completed", "usage": {
        "input_tokens": 300, "cached_input_tokens": 100, "cache_write_input_tokens": 20,
        "output_tokens": 60, "reasoning_output_tokens": 5}});
    let d = t.observe(&done).unwrap();
    // OpenAI's input_tokens includes cached + cache-write; coop keeps them disjoint.
    assert_eq!(
        (d.input_tokens, d.cache_read_input_tokens, d.cache_creation_input_tokens, d.output_tokens),
        (180, 100, 20, 60)
    );
    // A second turn is independent (no differencing for exec).
    assert_eq!(t.observe(&done).unwrap().output_tokens, 60);
    assert!(t.observe(&json!({"type": "turn.completed"})).is_none());
    assert!(t.observe(&json!({"type": "turn.completed", "usage": {}})).is_none());
    assert!(t.observe(&json!({"type": "turn.started"})).is_none());
}

#[test]
fn app_server_usage_is_the_growth_of_the_running_total() {
    let note = |total_in: u64, total_out: u64, last_in: u64, last_out: u64| {
        json!({"method": "thread/tokenUsage/updated", "params": {"tokenUsage": {
            "total": {"inputTokens": total_in, "outputTokens": total_out},
            "last": {"inputTokens": last_in, "outputTokens": last_out}}}})
    };
    let mut t = CodexUsageTracker::default();
    // First notification of a (possibly resumed) thread: only `last` counts.
    let d = t.observe(&note(5000, 900, 100, 20)).unwrap();
    assert_eq!((d.input_tokens, d.output_tokens), (100, 20));
    let d = t.observe(&note(5250, 940, 250, 40)).unwrap();
    assert_eq!((d.input_tokens, d.output_tokens), (250, 40));
    // A repeated total adds nothing.
    assert!(t.observe(&note(5250, 940, 250, 40)).is_none());
}
