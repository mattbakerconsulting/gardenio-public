// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

#[test]
fn extract_text_from_assistant() -> anyhow::Result<()> {
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "text", "text": "Hello world"}
                ]
            }
        }"#,
    )?;
    assert_eq!(extract_text(&json), Some("Hello world".to_owned()));
    Ok(())
}

#[test]
fn extract_text_ignores_non_assistant() -> anyhow::Result<()> {
    let json: Value = serde_json::from_str(
        r#"{
            "type": "user",
            "message": {
                "content": [
                    {"type": "text", "text": "user input"}
                ]
            }
        }"#,
    )?;
    assert_eq!(extract_text(&json), None);
    Ok(())
}

#[test]
fn extract_text_empty_blocks() -> anyhow::Result<()> {
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "tool_use", "name": "Bash"}
                ]
            }
        }"#,
    )?;
    assert_eq!(extract_text(&json), None);
    Ok(())
}

#[test]
fn extract_thinking_from_assistant() -> anyhow::Result<()> {
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "thinking", "thinking": "Let me reason about this..."}
                ]
            }
        }"#,
    )?;
    assert_eq!(extract_thinking(&json), Some("Let me reason about this...".to_owned()));
    Ok(())
}

#[test]
fn extract_thinking_none_when_absent() -> anyhow::Result<()> {
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "text", "text": "No thinking here"}
                ]
            }
        }"#,
    )?;
    assert_eq!(extract_thinking(&json), None);
    Ok(())
}

#[test]
fn extract_usage_from_message() -> anyhow::Result<()> {
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "model": "claude-opus-4-8",
                "usage": {
                    "input_tokens": 1000,
                    "output_tokens": 500,
                    "cache_read_input_tokens": 200,
                    "cache_creation_input_tokens": 100
                }
            },
            "durationMs": 1234
        }"#,
    )?;
    let kind = extract_usage(&json);
    assert!(kind.is_some());
    if let Some(MessageKind::Usage {
        input_tokens,
        output_tokens,
        cache_read,
        cache_write,
        duration_ms,
        model,
    }) = kind
    {
        assert_eq!(input_tokens, 1000);
        assert_eq!(output_tokens, 500);
        assert_eq!(cache_read, 200);
        assert_eq!(cache_write, 100);
        assert_eq!(duration_ms, 1234);
        assert_eq!(model, "claude-opus-4-8");
    } else {
        anyhow::bail!("expected MessageKind::Usage");
    }
    Ok(())
}

#[test]
fn extract_usage_model_absent() -> anyhow::Result<()> {
    // A turn with no model must yield an empty model. coop carries no cost
    // concept — there is no response_cost field to extract.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "usage": {
                    "input_tokens": 10,
                    "output_tokens": 5
                }
            }
        }"#,
    )?;
    if let Some(MessageKind::Usage { model, .. }) = extract_usage(&json) {
        assert_eq!(model, "");
    } else {
        anyhow::bail!("expected MessageKind::Usage");
    }
    Ok(())
}

#[test]
fn extract_usage_top_level_model() -> anyhow::Result<()> {
    // Non-nested log shape: model at the top level alongside a top-level usage
    // block.
    let json: Value = serde_json::from_str(
        r#"{
            "model": "claude-sonnet-4-6",
            "usage": {
                "input_tokens": 42,
                "output_tokens": 7
            }
        }"#,
    )?;
    if let Some(MessageKind::Usage { model, .. }) = extract_usage(&json) {
        assert_eq!(model, "claude-sonnet-4-6");
    } else {
        anyhow::bail!("expected MessageKind::Usage");
    }
    Ok(())
}

// --- Cost-free guards (T1, epic ka-0000000758 / I3 ka-0000002983) -----------
//
// coop forwards token usage and model only; it carries NO cost concept. Per-
// session USD spend is reconciled controller-side from LiteLLM at session close
// (reconcile_session_spend.go). These guards lock that invariant so a future
// change cannot quietly resurrect coop-side cost forwarding (the structurally
// dead `response_cost` plumbing removed under I3).

#[test]
fn extract_usage_ignores_response_cost_in_jsonl() -> anyhow::Result<()> {
    // Even if a JSONL line carries a `response_cost` field (e.g. a future driver
    // or a hand-crafted log), coop must NOT pick it up: MessageKind::Usage has no
    // cost field to hold it, and extraction yields only the token/model signal.
    // The exhaustive destructure below is itself a compile-time lock — if a cost
    // field were ever added back to the variant, this test would fail to compile.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "model": "claude-opus-4-8",
                "usage": {
                    "input_tokens": 1000,
                    "output_tokens": 500,
                    "cache_read_input_tokens": 0,
                    "cache_creation_input_tokens": 0
                }
            },
            "response_cost": 0.42,
            "_litellm_response_cost": 0.99
        }"#,
    )?;
    if let Some(MessageKind::Usage {
        input_tokens,
        output_tokens,
        cache_read,
        cache_write,
        duration_ms,
        model,
    }) = extract_usage(&json)
    {
        assert_eq!(input_tokens, 1000);
        assert_eq!(output_tokens, 500);
        assert_eq!(cache_read, 0);
        assert_eq!(cache_write, 0);
        assert_eq!(duration_ms, 0);
        assert_eq!(model, "claude-opus-4-8");
        // There is no cost binding to assert — the variant has none. That is the
        // guarantee: a response_cost in the JSONL is structurally inexpressible.
    } else {
        anyhow::bail!("expected MessageKind::Usage");
    }
    Ok(())
}

#[test]
fn nats_forward_source_has_no_response_cost() {
    // Source-level guard mirroring the Go spend_forwarder deletion guard: the
    // forwarder source must not reference `response_cost` at all (not as a field,
    // an extraction, nor a publish key). Comments are allowed to mention it only
    // in the test file (this assertion reads nats_forward.rs, not the tests).
    let src = include_str!("nats_forward.rs");
    assert!(
        !src.contains("response_cost"),
        "nats_forward.rs references `response_cost` — coop is cost-free (I3 \
         ka-0000002983); per-session spend is reconciled controller-side from \
         LiteLLM, never forwarded per-turn by coop"
    );
}

#[test]
fn extract_usage_skips_zero_tokens() -> anyhow::Result<()> {
    let json: Value = serde_json::from_str(
        r#"{
            "usage": {
                "input_tokens": 0,
                "output_tokens": 0
            }
        }"#,
    )?;
    assert!(extract_usage(&json).is_none());
    Ok(())
}

// --- Refusal extraction (ka-0000004364 → ka-0000006122) ---------------------

#[test]
fn extract_refusal_from_assistant() -> anyhow::Result<()> {
    // A refusal turn: stop_reason "refusal" on the nested message, with a served
    // model and a refusal text block.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "model": "claude-opus-4-8",
                "stop_reason": "refusal",
                "content": [
                    {"type": "text", "text": "I can't help with that."}
                ]
            }
        }"#,
    )?;
    if let Some(MessageKind::Refusal { model, text }) = extract_refusal(&json) {
        assert_eq!(model, "claude-opus-4-8");
        assert_eq!(text, "I can't help with that.");
    } else {
        anyhow::bail!("expected MessageKind::Refusal");
    }
    Ok(())
}

#[test]
fn extract_refusal_none_for_end_turn() -> anyhow::Result<()> {
    // A normal completed turn must NOT be treated as a refusal.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "model": "claude-opus-4-8",
                "stop_reason": "end_turn",
                "content": [
                    {"type": "text", "text": "Sure, here you go."}
                ]
            }
        }"#,
    )?;
    assert!(extract_refusal(&json).is_none());
    Ok(())
}

#[test]
fn extract_refusal_none_when_stop_reason_absent() -> anyhow::Result<()> {
    // No stop_reason at all (e.g. a streaming intermediate line) => no refusal.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "text", "text": "thinking out loud"}
                ]
            }
        }"#,
    )?;
    assert!(extract_refusal(&json).is_none());
    Ok(())
}

#[test]
fn extract_refusal_ignores_non_assistant() -> anyhow::Result<()> {
    // A user/tool entry that carries the word must never trip detection — only an
    // assistant message's stop_reason is authoritative.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "user",
            "message": {
                "stop_reason": "refusal",
                "content": [
                    {"type": "text", "text": "please refuse"}
                ]
            }
        }"#,
    )?;
    assert!(extract_refusal(&json).is_none());
    Ok(())
}

#[test]
fn extract_refusal_top_level_stop_reason() -> anyhow::Result<()> {
    // Non-nested log shape: stop_reason and model at the top level.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "model": "claude-sonnet-4-6",
            "stop_reason": "refusal",
            "message": {
                "content": []
            }
        }"#,
    )?;
    if let Some(MessageKind::Refusal { model, text }) = extract_refusal(&json) {
        assert_eq!(model, "claude-sonnet-4-6");
        // No text block => empty snippet; the stop_reason alone is the signal.
        assert_eq!(text, "");
    } else {
        anyhow::bail!("expected MessageKind::Refusal");
    }
    Ok(())
}

#[test]
fn extract_refusal_no_text_block_yields_empty_snippet() -> anyhow::Result<()> {
    // A refusal that carries no assistant text block still classifies; the text
    // snippet is empty (the consumer relies on stop_reason, not the snippet).
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "model": "claude-opus-4-8",
                "stop_reason": "refusal",
                "content": [
                    {"type": "tool_use", "name": "Bash"}
                ]
            }
        }"#,
    )?;
    if let Some(MessageKind::Refusal { model, text }) = extract_refusal(&json) {
        assert_eq!(model, "claude-opus-4-8");
        assert_eq!(text, "");
    } else {
        anyhow::bail!("expected MessageKind::Refusal");
    }
    Ok(())
}

#[test]
fn extract_refusal_model_absent() -> anyhow::Result<()> {
    // A refusal with no served model reported still classifies with an empty
    // model — the controller can then flag "served model unknown".
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "stop_reason": "refusal",
                "content": [
                    {"type": "text", "text": "No."}
                ]
            }
        }"#,
    )?;
    if let Some(MessageKind::Refusal { model, text }) = extract_refusal(&json) {
        assert_eq!(model, "");
        assert_eq!(text, "No.");
    } else {
        anyhow::bail!("expected MessageKind::Refusal");
    }
    Ok(())
}

#[test]
fn extract_refusal_case_insensitive_stop_reason() -> anyhow::Result<()> {
    // Defensive: an upper/mixed-case stop_reason from a future log shape still
    // classifies (is_refusal_stop_reason is case-insensitive).
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "model": "claude-opus-4-8",
                "stop_reason": "Refusal",
                "content": []
            }
        }"#,
    )?;
    assert!(matches!(extract_refusal(&json), Some(MessageKind::Refusal { .. })));
    Ok(())
}

// --- Tool-use extraction (data slice ka-0000004690) --------------------------

#[test]
fn extract_tool_uses_bash_with_label() -> anyhow::Result<()> {
    // A Bash tool_use forwards the tool name + the extract_tool_label summary
    // (description preferred over command, per the shared label contract).
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "tool_use", "name": "Bash",
                     "input": {"command": "ls -la", "description": "List files"}}
                ]
            }
        }"#,
    )?;
    let tools = extract_tool_uses(&json);
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name, "Bash");
    assert_eq!(tools[0].args_summary.as_deref(), Some("List files"));
    Ok(())
}

#[test]
fn extract_tool_uses_read_basename() -> anyhow::Result<()> {
    // Read summarises to the file basename.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "tool_use", "name": "Read",
                     "input": {"file_path": "/home/agent/foo/bar.go"}}
                ]
            }
        }"#,
    )?;
    let tools = extract_tool_uses(&json);
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name, "Read");
    assert_eq!(tools[0].args_summary.as_deref(), Some("bar.go"));
    Ok(())
}

#[test]
fn extract_tool_uses_multiple_parallel_blocks() -> anyhow::Result<()> {
    // An assistant turn with N parallel tool_use blocks forwards N tool events,
    // in emission order.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "text", "text": "Working on it"},
                    {"type": "tool_use", "name": "Grep", "input": {"pattern": "TODO"}},
                    {"type": "tool_use", "name": "Read", "input": {"file_path": "x.rs"}}
                ]
            }
        }"#,
    )?;
    let tools = extract_tool_uses(&json);
    assert_eq!(tools.len(), 2);
    assert_eq!(tools[0].name, "Grep");
    assert_eq!(tools[0].args_summary.as_deref(), Some("TODO"));
    assert_eq!(tools[1].name, "Read");
    assert_eq!(tools[1].args_summary.as_deref(), Some("x.rs"));
    Ok(())
}

#[test]
fn extract_tool_uses_no_summarizable_args_is_none() -> anyhow::Result<()> {
    // A tool with no summarizable input yields the name with a None summary
    // (extract_tool_label returns None) — the tool call is still forwarded.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "tool_use", "name": "Bash", "input": {}}
                ]
            }
        }"#,
    )?;
    let tools = extract_tool_uses(&json);
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name, "Bash");
    assert_eq!(tools[0].args_summary, None);
    Ok(())
}

#[test]
fn extract_tool_uses_skips_ask_user_question() {
    // AskUserQuestion is an interactive prompt (Prompt state), not a transcript
    // tool call — it is skipped.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "tool_use", "name": "AskUserQuestion",
                     "input": {"questions": [{"question": "Which DB?"}]}}
                ]
            }
        }"#,
    )
    .expect("valid json");
    assert!(extract_tool_uses(&json).is_empty());
}

#[test]
fn extract_tool_uses_ignores_non_assistant() {
    // A user/tool_result entry carries no assistant tool_use blocks to forward.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "user",
            "message": {
                "content": [
                    {"type": "tool_result", "content": "done"}
                ]
            }
        }"#,
    )
    .expect("valid json");
    assert!(extract_tool_uses(&json).is_empty());
}

#[test]
fn extract_tool_uses_empty_when_no_tool_blocks() {
    // An assistant turn that is only text forwards no tool events.
    let json: Value = serde_json::from_str(
        r#"{
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "text", "text": "Just talking"}
                ]
            }
        }"#,
    )
    .expect("valid json");
    assert!(extract_tool_uses(&json).is_empty());
}

#[test]
fn truncate_str_short() {
    assert_eq!(truncate_str("hello", 10), "hello");
}

#[test]
fn truncate_str_exact() {
    assert_eq!(truncate_str("hello", 5), "hello");
}

#[test]
fn truncate_str_long() {
    let result = truncate_str("hello world", 5);
    assert_eq!(result, "hello...");
}

#[test]
fn truncate_str_multibyte() {
    // "héllo" — é is 2 bytes in UTF-8
    let s = "héllo";
    let result = truncate_str(s, 3);
    // Should truncate at char boundary: "hé" (3 bytes) + "..."
    assert_eq!(result, "hé...");
}

// --- Text budget derivation (Telepathy v1.1, ka-0000006960) -----------------

#[test]
fn text_budget_default_payload_hits_ceiling() {
    // The 1 MiB NATS protocol default minus the envelope reserve is far larger
    // than the ceiling, so the budget clamps to MAX_TEXT_BYTES_CEILING.
    assert_eq!(text_budget(DEFAULT_NATS_MAX_PAYLOAD), MAX_TEXT_BYTES_CEILING);
}

#[test]
fn text_budget_mid_payload_reserves_envelope() {
    // A 64 KiB server payload leaves 64 KiB - 16 KiB = 48 KiB for text, which
    // is below the ceiling so it is used verbatim.
    let max_payload = 64 * 1024;
    assert_eq!(text_budget(max_payload), max_payload - ENVELOPE_RESERVE_BYTES);
    // And it must always leave room for the envelope under max_payload.
    assert!(text_budget(max_payload) < max_payload);
}

#[test]
fn text_budget_tiny_payload_floors_at_512() {
    // A pathologically small max_payload must not underflow to zero; it floors
    // at the original 512B behaviour.
    assert_eq!(text_budget(0), 512);
    assert_eq!(text_budget(1024), 512);
    assert_eq!(text_budget(ENVELOPE_RESERVE_BYTES), 512);
}

#[test]
fn text_budget_lifts_the_old_512_cap() {
    // Regression guard for the lift: with any reasonable server payload the new
    // budget is strictly larger than the old hard 512B cap, so full thinking
    // traces are forwarded rather than clipped.
    assert!(text_budget(DEFAULT_NATS_MAX_PAYLOAD) > 512);
    assert!(text_budget(128 * 1024) > 512);
}
