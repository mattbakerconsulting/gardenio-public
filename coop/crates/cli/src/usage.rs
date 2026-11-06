// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Per-session API usage tracking.
//!
//! Extracts token counts from Claude session log JSONL `result` entries
//! and accumulates them into a cumulative snapshot. Exposed via HTTP, WS,
//! and gRPC.

use std::collections::HashSet;
use std::sync::atomic::{AtomicU64, Ordering};

use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::sync::{broadcast, RwLock};

/// Prometheus histogram upper bounds (`le`), in seconds. Shared by both the
/// tool-call and LLM-request latency histograms. Covers sub-10ms tool calls
/// through multi-minute LLM requests. The implicit `+Inf` bucket is appended
/// at render time.
pub const LATENCY_BUCKETS_SECONDS: &[f64] =
    &[0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0, 120.0];

/// A cumulative Prometheus-style histogram with fixed bucket bounds.
///
/// Bucket bounds are the [`LATENCY_BUCKETS_SECONDS`] upper bounds (`le`).
/// `counts[i]` is the number of observations that fall in bucket `i` (i.e.
/// each entry is *non-cumulative* internally); the cumulative counts required
/// by the Prometheus wire format are produced by
/// [`Histogram::cumulative_counts`] at render time.
#[derive(Debug, Clone)]
pub struct Histogram {
    /// Per-bucket observation counts, aligned to [`LATENCY_BUCKETS_SECONDS`]
    /// plus a trailing `+Inf` overflow bucket (`bounds.len() + 1` entries).
    counts: Vec<u64>,
    /// Sum of all observed values (seconds).
    sum: f64,
    /// Total number of observations.
    count: u64,
}

impl Default for Histogram {
    fn default() -> Self {
        Self::new()
    }
}

impl Histogram {
    /// Create an empty histogram sized for [`LATENCY_BUCKETS_SECONDS`].
    pub fn new() -> Self {
        Self { counts: vec![0; LATENCY_BUCKETS_SECONDS.len() + 1], sum: 0.0, count: 0 }
    }

    /// Record one observation (seconds). Negative values are clamped to 0.
    pub fn observe(&mut self, value: f64) {
        let value = value.max(0.0);
        // Lowest bucket whose upper bound is >= value; fall through to the
        // +Inf overflow bucket (the last slot) when value exceeds all bounds.
        let idx = LATENCY_BUCKETS_SECONDS
            .iter()
            .position(|&bound| value <= bound)
            .unwrap_or(LATENCY_BUCKETS_SECONDS.len());
        self.counts[idx] += 1;
        self.sum += value;
        self.count += 1;
    }

    /// Total observation count.
    pub fn count(&self) -> u64 {
        self.count
    }

    /// Sum of all observed values (seconds).
    pub fn sum(&self) -> f64 {
        self.sum
    }

    /// Cumulative per-bucket counts in Prometheus wire order: each entry is the
    /// number of observations `<= bound`. The final entry corresponds to the
    /// `+Inf` bucket and always equals [`Histogram::count`].
    pub fn cumulative_counts(&self) -> Vec<u64> {
        let mut acc = 0u64;
        self.counts
            .iter()
            .map(|&c| {
                acc += c;
                acc
            })
            .collect()
    }
}

/// Per-entry extraction from a single Claude API response.
#[derive(Debug, Clone, Default)]
pub struct UsageDelta {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cache_creation_input_tokens: u64,
    pub cache_read_input_tokens: u64,
    /// Per-request LLM API latency in milliseconds, when one could be
    /// determined. `Some(n)` — including `Some(0)` — is a REAL measurement and
    /// is observed into the `llm_request` histogram exactly once; `None` means
    /// no latency was derivable (a `--print` entry without `durationMs`, or an
    /// interactive entry with no predecessor timestamp to gap against) and is
    /// skipped. Keeping this an `Option` — not a bare `u64` with `0` as a
    /// sentinel — is load-bearing: a genuinely sub-millisecond gap truncates to
    /// `0` ms, and a `> 0` observation gate would then silently drop it from the
    /// histogram while `request_count` still ticks, i.e. a partially-dead
    /// observer whose count nondeterministically trails `request_count`
    /// (ka-0000004208).
    pub duration_api_ms: Option<u64>,
    /// LiteLLM per-turn `response_cost` (USD) for this request, when present.
    /// Entries without a cost field contribute 0. This is the same in-pod
    /// estimate the gb CLI sums (session_usage.go) — non-authoritative, but
    /// good enough for a live "cost so far" display on the agent card. The
    /// controller still reconciles the authoritative total at session close.
    pub response_cost_usd: f64,
}

/// Cumulative session usage counters.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SessionUsage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cache_read_tokens: u64,
    pub cache_write_tokens: u64,
    pub request_count: u64,
    pub total_api_ms: u64,
    /// Running sum of per-turn `response_cost_usd` deltas (in-pod estimate).
    pub total_cost_usd: f64,
    /// Size of the live context window at the most recent request, in tokens:
    /// `input_tokens + cache_read + cache_creation` for that turn. Unlike the
    /// cumulative counters above this is a *level*, not a sum — it tracks how
    /// full the model's context window is right now, which is what operators
    /// want to see on the agent card. Zero until the first request lands.
    pub context_tokens: u64,
}

impl SessionUsage {
    /// Add a delta to the cumulative counters and refresh the live context
    /// level. Token/cost/duration fields accumulate; `context_tokens` is
    /// *set* to this turn's window occupancy (the latest request wins).
    pub fn accumulate(&mut self, delta: &UsageDelta) {
        self.input_tokens += delta.input_tokens;
        self.output_tokens += delta.output_tokens;
        self.cache_read_tokens += delta.cache_read_input_tokens;
        self.cache_write_tokens += delta.cache_creation_input_tokens;
        self.total_api_ms += delta.duration_api_ms.unwrap_or(0);
        self.total_cost_usd += delta.response_cost_usd;
        self.context_tokens =
            delta.input_tokens + delta.cache_read_input_tokens + delta.cache_creation_input_tokens;
        self.request_count += 1;
    }
}

/// Broadcast payload for usage updates.
#[derive(Debug, Clone, Serialize)]
pub struct UsageEvent {
    pub cumulative: SessionUsage,
    pub seq: u64,
}

/// Snapshot of the latency histograms tracked alongside the usage counters.
#[derive(Debug, Clone, Default)]
pub struct LatencyHistograms {
    /// Wall-clock latency from `ToolBefore` to `ToolAfter` per tool call.
    pub tool_call: Histogram,
    /// Per-request LLM API latency, sourced from `duration_api_ms` — either a
    /// literal `durationMs` on the log entry (`--print` mode) or the
    /// [`LlmLatencyTracker`] timestamp-gap derivation (interactive sessions,
    /// which never write `durationMs`).
    pub llm_request: Histogram,
}

/// Shared usage state, safe to access from multiple tasks.
pub struct UsageState {
    usage: RwLock<SessionUsage>,
    /// Latency histograms (tool-call + LLM-request), rendered alongside the
    /// session counters on the Prometheus `/metrics` endpoint.
    histograms: RwLock<LatencyHistograms>,
    pub usage_tx: broadcast::Sender<UsageEvent>,
    seq: AtomicU64,
}

impl Default for UsageState {
    fn default() -> Self {
        Self::new()
    }
}

impl UsageState {
    /// Create with default (zero) usage and a broadcast channel.
    pub fn new() -> Self {
        let (usage_tx, _) = broadcast::channel(64);
        Self {
            usage: RwLock::new(SessionUsage::default()),
            histograms: RwLock::new(LatencyHistograms::default()),
            usage_tx,
            seq: AtomicU64::new(0),
        }
    }

    /// Accumulate a delta, broadcast the updated snapshot.
    ///
    /// When the delta carries a determined `duration_api_ms` (`Some`, including
    /// `Some(0)` for a genuinely sub-millisecond gap), the per-request LLM
    /// latency is recorded into the `llm_request` histogram exactly once
    /// (converted to seconds — the Prometheus convention for `*_seconds`
    /// metrics). A `None` duration — no derivable latency — is skipped, so the
    /// histogram count tracks the number of *measured* requests rather than
    /// silently dropping the ones whose gap floored to zero (ka-0000004208).
    pub async fn accumulate(&self, delta: UsageDelta) {
        if let Some(duration_ms) = delta.duration_api_ms {
            self.observe_llm_request_ms(duration_ms).await;
        }
        let snapshot = {
            let mut usage = self.usage.write().await;
            usage.accumulate(&delta);
            usage.clone()
        };
        let seq = self.seq.fetch_add(1, Ordering::Relaxed) + 1;
        let _ = self.usage_tx.send(UsageEvent { cumulative: snapshot, seq });
    }

    /// Record a completed tool call's wall-clock latency (seconds).
    pub async fn observe_tool_call(&self, seconds: f64) {
        self.histograms.write().await.tool_call.observe(seconds);
    }

    /// Record an LLM API request latency given in milliseconds.
    pub async fn observe_llm_request_ms(&self, duration_ms: u64) {
        self.histograms.write().await.llm_request.observe(duration_ms as f64 / 1000.0);
    }

    /// Read-lock and clone the current snapshot.
    pub async fn snapshot(&self) -> SessionUsage {
        self.usage.read().await.clone()
    }

    /// Read-lock and clone the current latency histograms.
    pub async fn histogram_snapshot(&self) -> LatencyHistograms {
        self.histograms.read().await.clone()
    }
}

/// Derives per-response LLM request latency from session-log JSONL entry
/// timestamps.
///
/// Interactive Claude Code sessions never write a `durationMs` field (it only
/// appears on `--print`-mode `result` entries), so
/// `gardenio_agent_llm_request_latency_seconds` sat registered-but-dead
/// fleet-wide (ka-0000004093). The log itself still carries the signal: every
/// entry has an RFC 3339 `timestamp`, and the FIRST entry of a new assistant
/// `message.id` is written when the response's first content block lands —
/// while the entry immediately before it (user prompt, tool result, hook
/// attachment) marks when the API request could start. The gap between those
/// two timestamps is the per-response request latency (to first content
/// block).
///
/// Feed every parsed log entry to [`LlmLatencyTracker::update`] in order; it
/// returns `Some(millis)` exactly once per assistant message. Sidechain
/// (subagent) entries are skipped so their interleaved timestamps can't
/// corrupt the pairing. Because latency comes from file timestamps, values
/// stay correct even when the watcher catches up on a burst of lines.
#[derive(Debug, Default)]
pub struct LlmLatencyTracker {
    /// Timestamp (epoch ms) of the previous timestamped, non-sidechain entry.
    prev_entry_ms: Option<i64>,
    /// `message.id` of the most recent assistant entry, for first-block dedup
    /// (Claude writes one log entry per content block, all sharing one id).
    last_message_id: Option<String>,
}

impl LlmLatencyTracker {
    /// Observe one session-log entry; returns the derived request latency in
    /// milliseconds when `json` is the first entry of a new assistant message.
    pub fn update(&mut self, json: &Value) -> Option<u64> {
        if json.get("isSidechain").and_then(Value::as_bool) == Some(true) {
            return None;
        }
        let ts_ms = json.get("timestamp").and_then(Value::as_str).and_then(parse_rfc3339_ms)?;
        let prev = self.prev_entry_ms.replace(ts_ms);

        if json.get("type").and_then(Value::as_str) != Some("assistant") {
            return None;
        }
        let msg_id = json.get("message").and_then(|m| m.get("id")).and_then(Value::as_str)?;
        if self.last_message_id.as_deref() == Some(msg_id) {
            return None; // continuation block of the same response
        }
        self.last_message_id = Some(msg_id.to_owned());

        // No predecessor (log starts mid-message, e.g. resume offset) or a
        // clock step backwards: skip the sample rather than record garbage.
        let gap = ts_ms - prev?;
        u64::try_from(gap).ok()
    }
}

/// Collapses the per-content-block fan-out of session-log entries so usage
/// accumulates once per API request.
///
/// Claude Code writes one JSONL entry PER CONTENT BLOCK of an assistant
/// message (thinking / text / each tool_use); every one of those entries
/// repeats the same `message.id` and the identical `message.usage` object.
/// Accumulating each entry therefore multiplies tokens, cost, and
/// request_count by the block fan-out (~2-5× on real transcripts,
/// ka-0000005055). The gb CLI parser (session_usage.go) already dedups by
/// message.id; this mirrors it so coop-scraped and gb-pushed
/// `gardenio_session_*` metrics agree.
///
/// A `HashSet` (not just the last id, as [`LlmLatencyTracker`] keeps) so
/// sidechain entries interleaved between a message's content blocks cannot
/// reset the dedup — sidechain usage is intentionally counted, matching gb.
/// Entries with no `message.id` (`--print` result entries) each count once.
#[derive(Debug, Default)]
pub struct UsageDeduper {
    /// `message.id`s already accumulated.
    seen: HashSet<String>,
}

impl UsageDeduper {
    /// Returns whether this entry's usage should be accumulated: true for the
    /// first entry of each distinct `message.id` and for every id-less entry.
    pub fn should_accumulate(&mut self, json: &Value) -> bool {
        match json.get("message").and_then(|m| m.get("id")).and_then(Value::as_str) {
            Some(id) => self.seen.insert(id.to_owned()),
            None => true,
        }
    }
}

/// Parse an RFC 3339 timestamp to epoch milliseconds.
///
/// Accepts both the `Z`-suffixed UTC form Claude Code writes
/// (`2026-07-18T10:09:58.158Z`) and the numeric-offset form other transcript
/// producers write (`2026-08-05T05:50:18.638+00:00`, e.g. claudeless). A
/// numeric offset is normalized to UTC by subtracting it, so the same instant
/// written in any zone maps to a single epoch value. That normalization is all
/// the gap-based latency math needs: without it, offset-form transcripts left
/// `gardenio_agent_llm_request_latency_seconds` observing nothing (ka-0000005556).
/// A missing/malformed zone designator returns `None` (the caller skips the
/// sample).
fn parse_rfc3339_ms(s: &str) -> Option<i64> {
    // Split the timezone designator off the end: literal `Z` (UTC) or a
    // numeric `±HH:MM` offset. The `-`/`+` sign is located within the time
    // portion (after `T`) so a leading `-` in the date can't be mistaken for it.
    let (body, offset_ms) = if let Some(body) = s.strip_suffix('Z') {
        (body, 0)
    } else {
        let t_idx = s.find('T')?;
        let sign_rel = s[t_idx + 1..].rfind(['+', '-'])?;
        let (body, zone) = s.split_at(t_idx + 1 + sign_rel);
        let (sign, hhmm) = zone.split_at(1);
        let (off_h, off_m) = hhmm.split_once(':')?;
        let off_h: i64 = off_h.parse().ok()?;
        let off_m: i64 = off_m.parse().ok()?;
        if off_h > 23 || off_m > 59 {
            return None;
        }
        let magnitude = (off_h * 60 + off_m) * 60 * 1000;
        let offset_ms = match sign {
            "+" => magnitude,
            "-" => -magnitude,
            _ => return None,
        };
        (body, offset_ms)
    };
    let (date, time) = body.split_once('T')?;

    let mut date_parts = date.splitn(3, '-');
    let year: i64 = date_parts.next()?.parse().ok()?;
    let month: u32 = date_parts.next()?.parse().ok()?;
    let day: u32 = date_parts.next()?.parse().ok()?;
    if !(1..=12).contains(&month) || !(1..=31).contains(&day) {
        return None;
    }

    let (hms, frac) = match time.split_once('.') {
        Some((hms, frac)) => (hms, frac),
        None => (time, ""),
    };
    let mut time_parts = hms.splitn(3, ':');
    let hour: i64 = time_parts.next()?.parse().ok()?;
    let minute: i64 = time_parts.next()?.parse().ok()?;
    let second: i64 = time_parts.next()?.parse().ok()?;
    if hour > 23 || minute > 59 || second > 60 {
        return None;
    }
    // Fractional seconds truncate to millisecond precision.
    let mut millis: i64 = 0;
    for (i, c) in frac.chars().take(3).enumerate() {
        let d = c.to_digit(10)? as i64;
        millis += d * 10_i64.pow(2 - i as u32);
    }

    let days = days_from_civil(year, month, day);
    let local_ms = (((days * 24 + hour) * 60 + minute) * 60 + second) * 1000 + millis;
    // Local wall-clock minus its UTC offset yields the UTC instant.
    Some(local_ms - offset_ms)
}

/// Days from 1970-01-01 for a proleptic Gregorian civil date
/// (Howard Hinnant's `days_from_civil` algorithm).
fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400; // [0, 399]
    let mp = i64::from((m + 9) % 12); // March=0 .. February=11
    let doy = (153 * mp + 2) / 5 + i64::from(d) - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe - 719468
}

/// Extract a [`UsageDelta`] from a Claude session log JSONL entry.
///
/// Looks for usage data at `json["usage"]` (legacy/result entries) or
/// `json["message"]["usage"]` (assistant entries). Returns `None` if
/// the entry has no usage data.
pub fn extract_usage_delta(json: &Value) -> Option<UsageDelta> {
    let usage = json.get("usage").or_else(|| json.get("message").and_then(|m| m.get("usage")))?;

    let input = usage.get("input_tokens").and_then(|v| v.as_u64()).unwrap_or(0);
    let output = usage.get("output_tokens").and_then(|v| v.as_u64()).unwrap_or(0);

    // Skip entries with zero tokens (e.g. system entries without real usage).
    if input == 0 && output == 0 {
        return None;
    }

    let cache_creation =
        usage.get("cache_creation_input_tokens").and_then(|v| v.as_u64()).unwrap_or(0);
    let cache_read = usage.get("cache_read_input_tokens").and_then(|v| v.as_u64()).unwrap_or(0);

    // `--print` result entries carry a literal `durationMs`; interactive
    // entries carry none, leaving this `None` so the log-watch path can fill it
    // from the derived timestamp gap (ka-0000004093). Kept `Option` rather than
    // defaulting to `0`: a real latency of `0` (or a sub-ms derived gap) must
    // still be observed, so `None` uniquely means "not measured" (ka-0000004208).
    let duration =
        json.get("durationMs").or_else(|| usage.get("durationMs")).and_then(|v| v.as_u64());

    // LiteLLM stamps the per-turn cost at one of these top-level keys. Mirror
    // the gb CLI's extractResponseCost (session_usage.go) so the live estimate
    // here matches the close-time figure. Missing/zero → no cost contribution.
    let cost = ["response_cost", "_litellm_response_cost"]
        .iter()
        .find_map(|k| json.get(*k).and_then(|v| v.as_f64()))
        .filter(|c| *c > 0.0)
        .unwrap_or(0.0);

    Some(UsageDelta {
        input_tokens: input,
        output_tokens: output,
        cache_creation_input_tokens: cache_creation,
        cache_read_input_tokens: cache_read,
        duration_api_ms: duration,
        response_cost_usd: cost,
    })
}

#[cfg(test)]
#[path = "usage_tests.rs"]
mod tests;
