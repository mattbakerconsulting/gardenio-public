// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::json;

use super::*;

#[test]
fn accumulate_single_delta() {
    let mut usage = SessionUsage::default();
    let delta = UsageDelta {
        input_tokens: 100,
        output_tokens: 50,
        cache_creation_input_tokens: 10,
        cache_read_input_tokens: 20,
        duration_api_ms: Some(1200),
        response_cost_usd: 0.25,
    };
    usage.accumulate(&delta);
    assert_eq!(usage.input_tokens, 100);
    assert_eq!(usage.output_tokens, 50);
    assert_eq!(usage.cache_write_tokens, 10);
    assert_eq!(usage.cache_read_tokens, 20);
    assert_eq!(usage.total_api_ms, 1200);
    assert_eq!(usage.request_count, 1);
    // context_tokens is a level (input + cache_read + cache_creation), not a sum.
    assert_eq!(usage.context_tokens, 130);
    assert_eq!(usage.total_cost_usd, 0.25);
}

#[test]
fn accumulate_multiple_deltas() {
    let mut usage = SessionUsage::default();
    let d1 = UsageDelta { input_tokens: 100, output_tokens: 50, ..Default::default() };
    let d2 = UsageDelta { input_tokens: 200, output_tokens: 75, ..Default::default() };
    usage.accumulate(&d1);
    usage.accumulate(&d2);
    assert_eq!(usage.input_tokens, 300);
    assert_eq!(usage.output_tokens, 125);
    assert_eq!(usage.request_count, 2);
}

#[test]
fn extract_from_result_entry() {
    let entry = json!({
        "type": "result",
        "durationMs": 3400,
        "usage": {
            "input_tokens": 500,
            "output_tokens": 200,
            "cache_creation_input_tokens": 30,
            "cache_read_input_tokens": 100
        }
    });
    let delta = extract_usage_delta(&entry);
    assert!(delta.is_some());
    let d = delta.unwrap();
    assert_eq!(d.input_tokens, 500);
    assert_eq!(d.output_tokens, 200);
    assert_eq!(d.cache_creation_input_tokens, 30);
    assert_eq!(d.cache_read_input_tokens, 100);
    assert_eq!(d.duration_api_ms, Some(3400));
}

#[test]
fn extract_from_assistant_entry() {
    let entry = json!({
        "type": "assistant",
        "message": {
            "role": "assistant",
            "usage": {
                "input_tokens": 3,
                "cache_creation_input_tokens": 7801,
                "cache_read_input_tokens": 17897,
                "output_tokens": 2,
                "service_tier": "standard"
            }
        },
        "requestId": "req_abc"
    });
    let d = extract_usage_delta(&entry).unwrap();
    assert_eq!(d.input_tokens, 3);
    assert_eq!(d.output_tokens, 2);
    assert_eq!(d.cache_creation_input_tokens, 7801);
    assert_eq!(d.cache_read_input_tokens, 17897);
}

#[test]
fn missing_usage_key_returns_none() {
    let entry = json!({ "type": "assistant", "message": {} });
    assert!(extract_usage_delta(&entry).is_none());
}

#[test]
fn zero_tokens_returns_none() {
    let entry = json!({
        "usage": {
            "input_tokens": 0,
            "output_tokens": 0
        }
    });
    assert!(extract_usage_delta(&entry).is_none());
}

#[test]
fn missing_optional_fields_default_to_zero() {
    let entry = json!({
        "usage": {
            "input_tokens": 50,
            "output_tokens": 25
        }
    });
    let d = extract_usage_delta(&entry).unwrap();
    assert_eq!(d.cache_creation_input_tokens, 0);
    assert_eq!(d.cache_read_input_tokens, 0);
    // No `durationMs` on the entry → no latency measured → `None`, NOT `Some(0)`.
    // A `Some(0)` would be observed into the histogram as a real (sub-ms) sample;
    // an unmeasured request must be skipped so the histogram count tracks only
    // measured requests (ka-0000004208).
    assert_eq!(d.duration_api_ms, None);
    assert_eq!(d.response_cost_usd, 0.0);
}

#[test]
fn extract_response_cost() {
    // LiteLLM stamps the per-turn cost at top level; either key is accepted.
    let entry = json!({
        "response_cost": 0.0123,
        "usage": { "input_tokens": 50, "output_tokens": 25 }
    });
    assert_eq!(extract_usage_delta(&entry).unwrap().response_cost_usd, 0.0123);

    let entry = json!({
        "_litellm_response_cost": 0.5,
        "usage": { "input_tokens": 50, "output_tokens": 25 }
    });
    assert_eq!(extract_usage_delta(&entry).unwrap().response_cost_usd, 0.5);
}

#[test]
fn context_tokens_tracks_latest_level_not_sum() {
    // Two turns: context_tokens must reflect the *latest* window occupancy,
    // while cost accumulates across both.
    let mut usage = SessionUsage::default();
    usage.accumulate(&UsageDelta {
        input_tokens: 10,
        cache_read_input_tokens: 90,
        response_cost_usd: 0.10,
        ..Default::default()
    });
    assert_eq!(usage.context_tokens, 100);
    usage.accumulate(&UsageDelta {
        input_tokens: 5,
        cache_read_input_tokens: 195,
        response_cost_usd: 0.20,
        ..Default::default()
    });
    // Latest turn's window = 200 (not 300), cost summed to 0.30.
    assert_eq!(usage.context_tokens, 200);
    assert!((usage.total_cost_usd - 0.30).abs() < 1e-9);
}

#[tokio::test]
async fn usage_state_accumulate_and_snapshot() -> anyhow::Result<()> {
    let state = UsageState::new();
    let mut rx = state.usage_tx.subscribe();

    state
        .accumulate(UsageDelta { input_tokens: 100, output_tokens: 50, ..Default::default() })
        .await;

    let event = rx.recv().await?;
    assert_eq!(event.seq, 1);
    assert_eq!(event.cumulative.input_tokens, 100);
    assert_eq!(event.cumulative.request_count, 1);

    let snap = state.snapshot().await;
    assert_eq!(snap.input_tokens, 100);
    assert_eq!(snap.output_tokens, 50);
    Ok(())
}

#[test]
fn histogram_observe_buckets_and_sum() {
    let mut h = Histogram::new();
    // 0.03s → second bucket (le=0.05); 0.4s → le=0.5; 200s → +Inf overflow.
    h.observe(0.03);
    h.observe(0.4);
    h.observe(200.0);
    assert_eq!(h.count(), 3);
    assert!((h.sum() - 200.43).abs() < 1e-9);

    // Cumulative counts are monotonic non-decreasing, end at the total count.
    let cumulative = h.cumulative_counts();
    assert_eq!(cumulative.len(), LATENCY_BUCKETS_SECONDS.len() + 1);
    assert_eq!(*cumulative.last().unwrap(), 3);
    for window in cumulative.windows(2) {
        assert!(window[1] >= window[0], "cumulative counts must be monotonic");
    }
    // le=0.01 bucket has 0; le=0.05 has 1 (the 0.03 obs); le=0.5 has 2.
    assert_eq!(cumulative[0], 0);
    assert_eq!(cumulative[1], 1);
    let le_half_idx = LATENCY_BUCKETS_SECONDS.iter().position(|&b| b == 0.5).unwrap();
    assert_eq!(cumulative[le_half_idx], 2);
}

#[test]
fn histogram_boundary_value_lands_in_its_bucket() {
    let mut h = Histogram::new();
    // A value exactly equal to a bound is `<= bound`, so it lands in that bucket.
    h.observe(0.05);
    let cumulative = h.cumulative_counts();
    assert_eq!(cumulative[1], 1); // le=0.05
    assert_eq!(cumulative[0], 0); // le=0.01
}

#[test]
fn histogram_negative_clamped_to_zero() {
    let mut h = Histogram::new();
    h.observe(-5.0);
    assert_eq!(h.count(), 1);
    assert_eq!(h.sum(), 0.0);
    // Clamped to 0.0, lands in the lowest bucket.
    assert_eq!(h.cumulative_counts()[0], 1);
}

#[tokio::test]
async fn usage_state_records_llm_latency_on_accumulate() {
    let state = UsageState::new();
    // 1200ms duration → 1.2s observation in the llm_request histogram.
    state
        .accumulate(UsageDelta {
            input_tokens: 10,
            output_tokens: 5,
            duration_api_ms: Some(1200),
            ..Default::default()
        })
        .await;
    // A second request with no duration must not be recorded.
    state.accumulate(UsageDelta { input_tokens: 1, output_tokens: 1, ..Default::default() }).await;

    let hist = state.histogram_snapshot().await;
    assert_eq!(hist.llm_request.count(), 1);
    assert!((hist.llm_request.sum() - 1.2).abs() < 1e-9);
    assert_eq!(hist.tool_call.count(), 0);
}

#[tokio::test]
async fn usage_state_records_tool_call_latency() {
    let state = UsageState::new();
    state.observe_tool_call(0.25).await;
    state.observe_tool_call(2.0).await;
    let hist = state.histogram_snapshot().await;
    assert_eq!(hist.tool_call.count(), 2);
    assert!((hist.tool_call.sum() - 2.25).abs() < 1e-9);
}

/// End-to-end regression for ka-0000005556: a transcript whose entries carry
/// NUMERIC-OFFSET RFC 3339 timestamps (e.g. `+00:00`, as claudeless and any
/// non-Claude-Code transcript producer write) must still drive the
/// `llm_request` histogram. This mirrors the `stream.rs` log-watch path:
/// feed each entry to [`LlmLatencyTracker::update`], plug the derived gap into
/// `duration_api_ms`, and accumulate. Before the offset-parsing fix,
/// `parse_rfc3339_ms` returned `None` for every offset-form entry, so the
/// derived gap was always `None`, `duration_api_ms` stayed 0, and the
/// histogram observed nothing.
#[tokio::test]
async fn offset_form_timestamps_drive_llm_request_histogram() {
    // request-start marker at t=0, first response block 1.2s later — both in
    // +00:00 offset form rather than the Z form real Claude Code writes.
    let entries = [
        json!({ "type": "user", "timestamp": "1970-01-01T00:00:00.000000000+00:00" }),
        assistant_entry("msg_A", "1970-01-01T00:00:01.200000000+00:00"),
    ];

    let state = UsageState::new();
    let mut tracker = LlmLatencyTracker::default();
    for json in &entries {
        let derived_ms = tracker.update(json);
        if let Some(mut delta) = extract_usage_delta(json) {
            if delta.duration_api_ms.is_none() {
                delta.duration_api_ms = derived_ms;
            }
            state.accumulate(delta).await;
        }
    }

    let hist = state.histogram_snapshot().await;
    // The histogram must observe exactly one request, at the 1.2s gap.
    assert_eq!(hist.llm_request.count(), 1);
    assert!((hist.llm_request.sum() - 1.2).abs() < 1e-9);
}

/// Regression for ka-0000004208: a request whose derived latency gap floors to
/// `Some(0)` (a sub-millisecond gap, or two log entries stamped in the same
/// millisecond) must STILL be observed into the `llm_request` histogram — it is
/// a real measurement, not the absence of one. Before the fix, `duration_api_ms`
/// was a bare `u64` and `accumulate` gated the observation on `> 0`, so every
/// same-millisecond request silently dropped from the histogram while
/// `request_count` still ticked. That made the histogram count trail
/// `request_count` by a NONDETERMINISTIC amount (one per same-ms collapse),
/// which is exactly what flaked the coop-live-usage E2E between release runs.
/// Post-fix, `llm_request.count()` == the number of MEASURED requests, so the
/// only requests missing from the histogram are those with no predecessor to
/// gap against (`None`) — a deterministic relationship.
#[tokio::test]
async fn zero_gap_derived_latency_is_still_observed() {
    // Two entries stamped in the SAME millisecond: the request-start marker and
    // the first response block land together (the byte-identical startup ack
    // shape, and any fast same-ms tool-fanout message). The derived gap is
    // `Some(0)`, which is a genuine "near-instant" measurement.
    let entries = [
        json!({ "type": "user", "timestamp": "1970-01-01T00:00:05.500Z" }),
        assistant_entry("msg_same_ms", "1970-01-01T00:00:05.500Z"),
    ];

    let state = UsageState::new();
    let mut tracker = LlmLatencyTracker::default();
    for json in &entries {
        let derived_ms = tracker.update(json);
        if let Some(mut delta) = extract_usage_delta(json) {
            if delta.duration_api_ms.is_none() {
                delta.duration_api_ms = derived_ms;
            }
            state.accumulate(delta).await;
        }
    }

    let snapshot = state.snapshot().await;
    let hist = state.histogram_snapshot().await;
    // One usage-bearing entry → one request. The zero-gap sample IS observed,
    // so the histogram count equals request_count — no silent drop.
    assert_eq!(snapshot.request_count, 1);
    assert_eq!(hist.llm_request.count(), 1, "a Some(0) gap must still be observed");
    assert_eq!(hist.llm_request.sum(), 0.0, "the observed sample is a 0s latency");
}

/// The tracker returns `Some(0)` — a real measurement — for two entries stamped
/// in the same instant, and `None` only when there is no predecessor to measure
/// against. The `stream.rs`/`accumulate` path relies on this distinction to
/// decide what reaches the histogram (ka-0000004208).
#[test]
fn llm_latency_tracker_same_timestamp_is_some_zero_not_none() {
    let mut t = LlmLatencyTracker::default();
    // No predecessor yet → skip (this is the `None`, not a zero measurement).
    assert_eq!(t.update(&json!({"type": "user", "timestamp": "1970-01-01T00:00:01Z"})), None);
    // Assistant stamped in the SAME millisecond as its predecessor → measured
    // near-instant, `Some(0)` — the case a `> 0` gate would wrongly drop.
    assert_eq!(t.update(&assistant_entry("msg_A", "1970-01-01T00:00:01Z")), Some(0));
}

#[test]
fn rfc3339_parses_claude_log_timestamps() {
    // The exact shape Claude Code writes. 2026-07-18T10:09:58.158Z.
    assert_eq!(parse_rfc3339_ms("2026-07-18T10:09:58.158Z"), Some(1_784_369_398_158));
    // Epoch itself, and no fractional part.
    assert_eq!(parse_rfc3339_ms("1970-01-01T00:00:00Z"), Some(0));
    assert_eq!(parse_rfc3339_ms("1970-01-01T00:00:01Z"), Some(1000));
    // Short fraction pads (0.1s = 100ms); long fraction truncates to ms.
    assert_eq!(parse_rfc3339_ms("1970-01-01T00:00:00.1Z"), Some(100));
    assert_eq!(parse_rfc3339_ms("1970-01-01T00:00:00.123456Z"), Some(123));
    // Garbage and out-of-range fields are still rejected.
    assert_eq!(parse_rfc3339_ms("not a timestamp"), None);
    assert_eq!(parse_rfc3339_ms("2026-13-18T10:09:58Z"), None);
    assert_eq!(parse_rfc3339_ms("2026-07-18T24:09:58Z"), None);
}

#[test]
fn rfc3339_parses_numeric_offset_timestamps() {
    // Numeric offsets are normalized to UTC, not rejected (ka-0000005556).
    // claudeless and any non-Claude-Code transcript producer write offset
    // form; the histogram is dead unless we parse it. Since latency is a GAP
    // between two entries, the only requirement is that every timestamp maps
    // to the same epoch — which normalizing to UTC guarantees.
    //
    // +00:00 is literally UTC: identical epoch to the Z form.
    assert_eq!(
        parse_rfc3339_ms("2026-07-18T10:09:58.158+00:00"),
        parse_rfc3339_ms("2026-07-18T10:09:58.158Z"),
    );
    assert_eq!(parse_rfc3339_ms("1970-01-01T00:00:00.000000000+00:00"), Some(0));
    assert_eq!(parse_rfc3339_ms("1970-01-01T00:00:00.000000000-00:00"), Some(0));
    // +02:00 means local is 2h AHEAD of UTC, so the UTC instant is 2h EARLIER.
    // 1970-01-01T02:00:00+02:00 == 1970-01-01T00:00:00Z == epoch 0.
    assert_eq!(parse_rfc3339_ms("1970-01-01T02:00:00+02:00"), Some(0));
    // -05:00 means local is 5h BEHIND UTC, so the UTC instant is 5h LATER.
    // 1969-12-31T19:00:00-05:00 == 1970-01-01T00:00:00Z == epoch 0.
    assert_eq!(parse_rfc3339_ms("1969-12-31T19:00:00-05:00"), Some(0));
    // A non-zero-minute offset (India, +05:30).
    assert_eq!(parse_rfc3339_ms("1970-01-01T05:30:00.500+05:30"), Some(500));
    // Malformed offsets are rejected.
    assert_eq!(parse_rfc3339_ms("2026-07-18T10:09:58.158+0200"), None);
    assert_eq!(parse_rfc3339_ms("2026-07-18T10:09:58.158+02:XX"), None);
    assert_eq!(parse_rfc3339_ms("2026-07-18T10:09:58.158+99:00"), None);
}

/// Build an assistant log entry with a message id and timestamp.
fn assistant_entry(msg_id: &str, ts: &str) -> serde_json::Value {
    json!({
        "type": "assistant",
        "timestamp": ts,
        "message": { "id": msg_id, "usage": { "input_tokens": 10, "output_tokens": 5 } }
    })
}

#[test]
fn llm_latency_tracker_derives_gap_for_new_message() {
    let mut t = LlmLatencyTracker::default();
    // Tool result written at t=0, first block of the response 1.2s later.
    assert_eq!(t.update(&json!({"type": "user", "timestamp": "1970-01-01T00:00:00Z"})), None);
    assert_eq!(t.update(&assistant_entry("msg_A", "1970-01-01T00:00:01.200Z")), Some(1200));
}

#[test]
fn llm_latency_tracker_dedups_continuation_blocks() {
    let mut t = LlmLatencyTracker::default();
    t.update(&json!({"type": "user", "timestamp": "1970-01-01T00:00:00Z"}));
    assert_eq!(t.update(&assistant_entry("msg_A", "1970-01-01T00:00:02Z")), Some(2000));
    // Later content blocks of the same message must not observe again —
    // Claude writes one entry per block, all sharing the message id.
    assert_eq!(t.update(&assistant_entry("msg_A", "1970-01-01T00:00:03Z")), None);
    assert_eq!(t.update(&assistant_entry("msg_A", "1970-01-01T00:00:04Z")), None);
    // The next response measures from the LAST entry before it (t=4 → t=5.5).
    assert_eq!(t.update(&assistant_entry("msg_B", "1970-01-01T00:00:05.500Z")), Some(1500));
}

#[test]
fn llm_latency_tracker_skips_first_message_without_predecessor() {
    // Log starts mid-message (resume offset): no request-start reference.
    let mut t = LlmLatencyTracker::default();
    assert_eq!(t.update(&assistant_entry("msg_A", "1970-01-01T00:00:01Z")), None);
    // But the next message pairs normally.
    assert_eq!(t.update(&assistant_entry("msg_B", "1970-01-01T00:00:03Z")), Some(2000));
}

#[test]
fn llm_latency_tracker_ignores_sidechain_and_untimestamped_entries() {
    let mut t = LlmLatencyTracker::default();
    t.update(&json!({"type": "user", "timestamp": "1970-01-01T00:00:00Z"}));
    // A sidechain (subagent) entry must not advance the request-start clock…
    assert_eq!(
        t.update(&json!({
            "type": "user", "isSidechain": true, "timestamp": "1970-01-01T00:00:09Z"
        })),
        None
    );
    // …nor entries without a timestamp (mode/last-prompt records).
    assert_eq!(t.update(&json!({"type": "mode", "mode": "normal"})), None);
    assert_eq!(t.update(&assistant_entry("msg_A", "1970-01-01T00:00:03Z")), Some(3000));
}

#[test]
fn llm_latency_tracker_skips_backwards_clock() {
    let mut t = LlmLatencyTracker::default();
    t.update(&json!({"type": "user", "timestamp": "1970-01-01T00:00:10Z"}));
    // Entry timestamp earlier than its predecessor: skip, don't underflow.
    assert_eq!(t.update(&assistant_entry("msg_A", "1970-01-01T00:00:05Z")), None);
}

#[test]
fn usage_deduper_accumulates_first_block_only() {
    let mut d = UsageDeduper::default();
    // Claude writes one entry per content block, all sharing one message.id
    // and repeating the identical usage — only the first may accumulate.
    assert!(d.should_accumulate(&assistant_entry("msg_A", "1970-01-01T00:00:01Z")));
    assert!(!d.should_accumulate(&assistant_entry("msg_A", "1970-01-01T00:00:02Z")));
    assert!(!d.should_accumulate(&assistant_entry("msg_A", "1970-01-01T00:00:03Z")));
    assert!(d.should_accumulate(&assistant_entry("msg_B", "1970-01-01T00:00:05Z")));
}

#[test]
fn usage_deduper_survives_interleaved_messages() {
    // Sidechain (subagent) entries interleave with the parent's content
    // blocks; a returning id must still be recognised as already counted.
    let mut d = UsageDeduper::default();
    assert!(d.should_accumulate(&assistant_entry("msg_A", "1970-01-01T00:00:01Z")));
    assert!(d.should_accumulate(&assistant_entry("msg_side", "1970-01-01T00:00:02Z")));
    assert!(!d.should_accumulate(&assistant_entry("msg_A", "1970-01-01T00:00:03Z")));
    assert!(!d.should_accumulate(&assistant_entry("msg_side", "1970-01-01T00:00:04Z")));
}

#[test]
fn usage_deduper_passes_idless_entries_through() {
    // --print result entries carry no message.id; each counts as its own
    // request (mirrors gb session_usage.go).
    let mut d = UsageDeduper::default();
    let result = json!({"type": "result", "usage": {"input_tokens": 1, "output_tokens": 1}});
    assert!(d.should_accumulate(&result));
    assert!(d.should_accumulate(&result));
}
