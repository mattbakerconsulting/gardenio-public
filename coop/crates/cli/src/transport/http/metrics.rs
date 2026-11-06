// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Prometheus `/metrics` endpoint for per-session usage metrics.
//!
//! Renders the Prometheus text exposition format directly from the
//! cumulative [`UsageState`] snapshot. Labels (`agent_name`, `project`,
//! `account`) are resolved from environment variables and runtime state.

use std::fmt::Write;
use std::sync::Arc;

use axum::extract::State;
use axum::http::header;
use axum::response::IntoResponse;

use crate::transport::state::Store;
use crate::usage::{Histogram, LATENCY_BUCKETS_SECONDS};

/// Prometheus text content type (OpenMetrics-compatible).
const CONTENT_TYPE: &str = "text/plain; version=0.0.4; charset=utf-8";

/// `GET /metrics` — Prometheus-compatible per-session usage metrics.
pub async fn metrics(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let snap = s.usage.snapshot().await;

    let agent_name = std::env::var("BOAT_AGENT").unwrap_or_default();
    let project = std::env::var("BOAT_PROJECT").unwrap_or_default();
    let account = s.profile.active_name().await.unwrap_or_default();

    let labels = format!(
        "agent_name=\"{}\",project=\"{}\",account=\"{}\"",
        prom_escape(&agent_name),
        prom_escape(&project),
        prom_escape(&account),
    );

    let mut buf = String::with_capacity(1024);

    write_counter(
        &mut buf,
        "gardenio_session_input_tokens_total",
        "Cumulative input tokens consumed by this session",
        snap.input_tokens,
        &labels,
    );
    write_counter(
        &mut buf,
        "gardenio_session_output_tokens_total",
        "Cumulative output tokens produced by this session",
        snap.output_tokens,
        &labels,
    );
    write_counter(
        &mut buf,
        "gardenio_session_cache_read_tokens_total",
        "Cumulative cache read tokens for this session",
        snap.cache_read_tokens,
        &labels,
    );
    write_counter(
        &mut buf,
        "gardenio_session_cache_write_tokens_total",
        "Cumulative cache write tokens for this session",
        snap.cache_write_tokens,
        &labels,
    );
    write_counter(
        &mut buf,
        "gardenio_session_request_count_total",
        "Cumulative API request count for this session",
        snap.request_count,
        &labels,
    );
    write_counter(
        &mut buf,
        "gardenio_session_api_ms_total",
        "Cumulative API latency in milliseconds for this session",
        snap.total_api_ms,
        &labels,
    );

    let hist = s.usage.histogram_snapshot().await;
    write_histogram(
        &mut buf,
        "gardenio_agent_tool_call_latency_seconds",
        "Wall-clock latency of agent tool calls (ToolBefore→ToolAfter), in seconds",
        &hist.tool_call,
        &labels,
    );
    write_histogram(
        &mut buf,
        "gardenio_agent_llm_request_latency_seconds",
        "LLM API request latency per response, in seconds",
        &hist.llm_request,
        &labels,
    );

    // One-shot --resume conversation-restore report (ka-0000000550). Rendered
    // on every scrape for the session's lifetime — constant-value counters
    // whose increase() fires once per session in downstream queries. Absent
    // entirely when no resume was requested, so fresh first spawns emit
    // nothing.
    if let Some(ref rr) = s.resume_report {
        let resume_labels = format!("{labels},outcome=\"{}\"", prom_escape(rr.outcome));
        write_counter(
            &mut buf,
            "gardenio_coop_resume_attempts_total",
            "coop --resume conversation-restore attempts by outcome (resumed / fresh_no_session / fresh_parse_error / fresh_discovery_error / fresh_no_uuid). One per session that requested a resume; a fresh_* outcome means the prior conversation was NOT restored and the agent started over",
            1,
            &resume_labels,
        );
        write_gauge(
            &mut buf,
            "gardenio_coop_resume_prepare_seconds",
            "Wall-clock seconds spent preparing the --resume restore (S3 transcript download when configured + local discovery + JSONL parse)",
            rr.prepare_seconds,
            &resume_labels,
        );
        write_counter(
            &mut buf,
            "gardenio_coop_resume_log_bytes",
            "Byte length of the transcript the resume state was parsed from (0 when none) — the partial-log signal: a restore that lost conversation shows a sharp drop vs the prior session",
            rr.log_bytes,
            &resume_labels,
        );
    }

    ([(header::CONTENT_TYPE, CONTENT_TYPE)], buf)
}

/// Write a gauge metric in Prometheus text format.
fn write_gauge(buf: &mut String, name: &str, help: &str, value: f64, labels: &str) {
    let _ = writeln!(buf, "# HELP {name} {help}");
    let _ = writeln!(buf, "# TYPE {name} gauge");
    let _ = writeln!(buf, "{name}{{{labels}}} {value:?}");
}

/// Write a counter metric in Prometheus text format.
fn write_counter(buf: &mut String, name: &str, help: &str, value: u64, labels: &str) {
    let _ = writeln!(buf, "# HELP {name} {help}");
    let _ = writeln!(buf, "# TYPE {name} counter");
    let _ = writeln!(buf, "{name}{{{labels}}} {value}");
}

/// Write a histogram metric in Prometheus text format.
///
/// Emits cumulative `_bucket{le=...}` lines (including the mandatory `+Inf`
/// bucket), a `_sum`, and a `_count`. The existing label set is interleaved
/// with the per-bucket `le` label.
fn write_histogram(buf: &mut String, name: &str, help: &str, hist: &Histogram, labels: &str) {
    let _ = writeln!(buf, "# HELP {name} {help}");
    let _ = writeln!(buf, "# TYPE {name} histogram");

    let cumulative = hist.cumulative_counts();
    for (i, bound) in LATENCY_BUCKETS_SECONDS.iter().enumerate() {
        let count = cumulative.get(i).copied().unwrap_or(0);
        let _ = writeln!(buf, "{name}_bucket{{{labels},le=\"{bound}\"}} {count}");
    }
    // Mandatory +Inf bucket — equals the total observation count.
    let _ = writeln!(buf, "{name}_bucket{{{labels},le=\"+Inf\"}} {}", hist.count());

    // `_sum` is rendered with full precision; `{:?}` on f64 avoids the lossy
    // default Display rounding while staying a valid Prometheus float.
    let _ = writeln!(buf, "{name}_sum{{{labels}}} {:?}", hist.sum());
    let _ = writeln!(buf, "{name}_count{{{labels}}} {}", hist.count());
}

/// Escape a label value per Prometheus text format rules.
/// Backslash, double-quote, and newline must be escaped.
fn prom_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '"' => out.push_str("\\\""),
            '\n' => out.push_str("\\n"),
            _ => out.push(c),
        }
    }
    out
}

#[cfg(test)]
#[path = "metrics_tests.rs"]
mod tests;
