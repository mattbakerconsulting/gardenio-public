// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! NATS message forwarder — publishes classified session log events to NATS.
//!
//! When `COOP_NATS_FORWARD=1`, subscribes to the `raw_message_tx` broadcast
//! channel (fed by Tier 2 LogDetector / Tier 3 stdout parser) and publishes
//! classified events to NATS subjects:
//!
//! - `{prefix}.messages.{actor}.assistant_text` — assistant text generation
//! - `{prefix}.messages.{actor}.thinking` — extended thinking / reasoning blocks
//! - `{prefix}.messages.{actor}.tool_use` — one event per `tool_use` block the
//!   assistant emitted, carrying the tool `name` + a compact
//!   `extract_tool_label` args summary (the same label the status bar / Slack
//!   tool-cause use). The gardenio controller mints these into the /plans chat
//!   transcript as `turn_role=tool` turns so a plan agent's tool calls render
//!   inline (data slice ka-0000004690; render side ka-0000004601).
//! - `{prefix}.messages.{actor}.usage` — per-turn token usage
//! - `{prefix}.messages.{actor}.refusal` — model declined (Anthropic
//!   `stop_reason == "refusal"`), carrying the served + requested model and a
//!   refusal snippet so the controller can persist a durable refusal log for the
//!   surveys section (ka-0000004364 → ka-0000006122).
//!
//! The usage event carries token counts and model only — coop forwards **no**
//! cost concept. Per-session USD spend is reconciled controller-side from
//! LiteLLM at session close (`reconcile_session_spend.go`); coop stays
//! strictly cost-free (ka-0000002983 / epic ka-0000000758).
//!
//! Text and thinking content are forwarded in full up to a generous byte
//! budget derived at connect time from the NATS server's advertised
//! `max_payload` (Telepathy v1.1, ka-0000006960). Earlier this was a hard
//! ~512-byte clip, which chopped agent thinking traces; we now carry the whole
//! trace whenever it fits below the server's payload ceiling, truncating only
//! as a last resort to stay under `max_payload`.

use serde_json::Value;
use tokio::sync::broadcast;
use tokio_util::sync::CancellationToken;

use crate::driver::claude::parse::extract_tool_label;
use crate::driver::is_refusal_stop_reason;
use crate::event::RawMessageEvent;
use crate::transport::nats::{build_connect_options, NatsAuth};
use crate::transport::Store;

/// Absolute ceiling for forwarded text content, regardless of how large the
/// NATS server's `max_payload` is. Full agent thinking traces are typically a
/// few KiB; 256 KiB is generous headroom while still bounding a pathological
/// single message so one trace cannot monopolise a stream consumer's buffer.
const MAX_TEXT_BYTES_CEILING: usize = 256 * 1024;

/// Bytes reserved for the JSON envelope wrapped around the text payload
/// (`event_type`, `session_id`, `ts`, `actor`, `full_length`, plus injected
/// session metadata: agent type, labels, k8s pod info). The derived text
/// budget is `max_payload - ENVELOPE_RESERVE_BYTES`, so the serialized message
/// stays comfortably under the server's `max_payload` even with metadata.
const ENVELOPE_RESERVE_BYTES: usize = 16 * 1024;

/// Fallback `max_payload` used when the server does not advertise one (the NATS
/// protocol default is 1 MiB).
const DEFAULT_NATS_MAX_PAYLOAD: usize = 1024 * 1024;

/// Compute the per-message text byte budget from the server's `max_payload`.
///
/// Returns `min(max_payload - ENVELOPE_RESERVE_BYTES, MAX_TEXT_BYTES_CEILING)`,
/// clamped to a small positive floor so a misconfigured tiny `max_payload`
/// still forwards *something* rather than panicking on underflow. Keeping the
/// budget strictly below `max_payload` guarantees the serialized envelope never
/// trips the server's payload limit.
fn text_budget(max_payload: usize) -> usize {
    // Floor: even on an absurdly small max_payload, keep the original 512B
    // behaviour rather than dropping to zero.
    const FLOOR: usize = 512;
    let usable = max_payload.saturating_sub(ENVELOPE_RESERVE_BYTES);
    usable.clamp(FLOOR, MAX_TEXT_BYTES_CEILING)
}

/// Classifies a raw message JSON into a forwardable event type.
#[derive(Debug)]
enum MessageKind {
    /// Per-turn token usage data.
    Usage {
        input_tokens: u64,
        output_tokens: u64,
        cache_read: u64,
        cache_write: u64,
        duration_ms: u64,
        /// Model id that served this turn (e.g. "claude-opus-4-8"), from the
        /// assistant message's `message.model`. Empty when absent.
        model: String,
    },
    /// The model declined to answer: Anthropic `stop_reason == "refusal"` on the
    /// assistant message. Forwarded as a durable refusal-log signal.
    Refusal {
        /// Model id that served this turn (from `message.model`). Empty when
        /// absent — carried so the controller can spot a refusal served by a
        /// *fallback* model different from what was requested.
        model: String,
        /// The refusal text/snippet emitted with the turn, if any (trimmed).
        /// Empty when the refusal carried no text block.
        text: String,
    },
}

/// NATS message forwarder — publishes classified session log events.
pub struct NatsForwarder {
    client: async_nats::Client,
    prefix: String,
    /// Gardenio agent name (from BOAT_AGENT env var or pod name), used as the
    /// actor segment in NATS subjects so kbeads can route per-agent.
    actor: String,
    /// The model the controller *requested* for this session (from the
    /// `ANTHROPIC_MODEL` env var the controller sets alongside the credential —
    /// see run.rs). Carried on the refusal event so the controller can tell a
    /// refusal served by the requested model from one served by a silent
    /// fallback model. Empty when unset.
    requested_model: String,
    metadata: Value,
    /// Per-message byte budget for forwarded text/thinking content, derived at
    /// connect time from the server's advertised `max_payload` (see
    /// [`text_budget`]). Earlier this was a hard 512B cap.
    text_budget: usize,
    /// The server's advertised `max_payload`. Used as a final belt-and-suspenders
    /// guard in [`NatsForwarder::publish_event`] so a serialized envelope that
    /// somehow exceeds the limit is dropped with a warning rather than rejected.
    max_payload: usize,
}

impl NatsForwarder {
    /// Connect to the NATS server at `url` with optional authentication.
    pub async fn connect(
        url: &str,
        prefix: &str,
        agent: &str,
        labels: &[String],
        auth: NatsAuth,
    ) -> anyhow::Result<Self> {
        // Best-effort transport: single-shot on thaw, so it keeps async-nats's
        // default 5 s connection timeout (no short-cap thaw retry plumbing here)
        // and no background initial-connect retry.
        let opts = build_connect_options(auth, None, false).await?;
        let client = opts.connect(url).await?;
        let metadata = crate::mux_client::detect_metadata(agent, labels);
        // Use BOAT_AGENT (gardenio agent name / pod name) as actor for NATS
        // subject routing. Falls back to "_global" if not set.
        let actor = std::env::var("BOAT_AGENT").unwrap_or_else(|_| "_global".to_owned());
        // Model the controller requested for this session (best-effort context
        // for refusal events; not a secret — see run.rs routing-env comment).
        let requested_model = std::env::var("ANTHROPIC_MODEL").unwrap_or_default();
        // Derive the text budget from the server's advertised max_payload so
        // full thinking traces are forwarded without ever exceeding the server
        // limit. `server_info().max_payload` is an i32; treat <=0 (unset) as the
        // 1 MiB protocol default.
        let max_payload = {
            let raw = client.server_info().max_payload;
            if raw > 0 {
                raw as usize
            } else {
                DEFAULT_NATS_MAX_PAYLOAD
            }
        };
        let text_budget = text_budget(max_payload);
        tracing::debug!(
            "nats-forward: max_payload={max_payload} text_budget={text_budget} (was 512)"
        );
        Ok(Self {
            client,
            prefix: prefix.to_owned(),
            actor,
            requested_model,
            metadata,
            text_budget,
            max_payload,
        })
    }

    /// Subscribe to the raw message broadcast and forward classified events.
    pub async fn run(self, store: &Store, shutdown: CancellationToken) {
        let mut message_rx = store.channels.message_tx.subscribe();

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                event = message_rx.recv() => {
                    match event {
                        Ok(raw) => {
                            self.handle_raw_message(store, &raw).await;
                        }
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            tracing::debug!("nats-forward: message subscriber lagged by {n}");
                        }
                        Err(broadcast::error::RecvError::Closed) => break,
                    }
                }
            }
        }
    }

    /// Classify a raw message and publish matching events.
    async fn handle_raw_message(&self, store: &Store, raw: &RawMessageEvent) {
        let json = &raw.json;

        // A single JSONL entry can contain multiple signals (e.g., an assistant
        // message with text + thinking + usage). Classify and publish each.

        // Assistant text
        if let Some(text) = extract_text(json) {
            self.publish_event(
                store,
                "assistant_text",
                &serde_json::json!({
                    "text": truncate_str(&text, self.text_budget),
                    "full_length": text.len(),
                }),
            )
            .await;
        }

        // Thinking / reasoning blocks
        if let Some(thinking) = extract_thinking(json) {
            self.publish_event(
                store,
                "thinking",
                &serde_json::json!({
                    "text": truncate_str(&thinking, self.text_budget),
                    "full_length": thinking.len(),
                }),
            )
            .await;
        }

        // Tool calls — one event per `tool_use` block the assistant emitted, so
        // the controller can mint each as a `turn_role=tool` transcript turn
        // (data slice ka-0000004690). Each carries the tool name plus the SAME
        // compact `extract_tool_label` args summary the status bar / Slack
        // tool-cause show, so the render side maps 1:1. An assistant turn with N
        // parallel tool_use blocks forwards N events; a turn with none forwards
        // none. AskUserQuestion is skipped: it is an interactive prompt, not a
        // tool call the transcript should paint (it drives the Prompt state, not
        // a tool cause).
        for tool in extract_tool_uses(json) {
            let mut payload = serde_json::json!({
                "tool_name": tool.name,
            });
            if let Value::Object(ref mut map) = payload {
                if let Some(args) = tool.args_summary {
                    map.insert("tool_args_summary".to_owned(), Value::String(args));
                }
            }
            self.publish_event(store, "tool_use", &payload).await;
        }

        // Usage data
        if let Some(MessageKind::Usage {
            input_tokens,
            output_tokens,
            cache_read,
            cache_write,
            duration_ms,
            model,
        }) = extract_usage(json)
        {
            let mut payload = serde_json::json!({
                "input_tokens": input_tokens,
                "output_tokens": output_tokens,
                "cache_read": cache_read,
                "cache_write": cache_write,
                "duration_ms": duration_ms,
            });
            // Only include `model` when present so the downstream consumer can
            // tell "no model reported" from a populated turn. coop forwards no
            // cost concept — per-session USD spend is reconciled controller-side
            // from LiteLLM at session close (reconcile_session_spend.go); coop
            // stays strictly cost-free (ka-0000002983 / epic ka-0000000758).
            if let Value::Object(ref mut map) = payload {
                if !model.is_empty() {
                    map.insert("model".to_owned(), Value::String(model));
                }
            }
            self.publish_event(store, "usage", &payload).await;
        }

        // Refusal — the model declined to answer (Anthropic stop_reason ==
        // "refusal"). A durable, controller-persisted signal (surveys refusal
        // log, ka-0000004364 → ka-0000006122). Distinct from the usage/text
        // signals above: this is the *classification* of the turn, and it carries
        // both the served model and the session's requested model so the
        // controller can flag a refusal served by a silent fallback.
        if let Some(MessageKind::Refusal { model, text }) = extract_refusal(json) {
            let mut payload = serde_json::json!({
                "stop_reason": "refusal",
            });
            if let Value::Object(ref mut map) = payload {
                if !model.is_empty() {
                    map.insert("model".to_owned(), Value::String(model));
                }
                // requested_model lets the consumer detect a refusal answered by
                // a fallback model (served != requested). Only include when known.
                if !self.requested_model.is_empty() {
                    map.insert(
                        "requested_model".to_owned(),
                        Value::String(self.requested_model.clone()),
                    );
                }
                if !text.is_empty() {
                    map.insert(
                        "text".to_owned(),
                        Value::String(truncate_str(&text, self.text_budget)),
                    );
                    map.insert("full_length".to_owned(), Value::Number(text.len().into()));
                }
            }
            self.publish_event(store, "refusal", &payload).await;
        }
    }

    /// Publish a classified event to NATS with identity metadata.
    async fn publish_event(&self, store: &Store, event_type: &str, data: &Value) {
        let session_id = store.session_id.read().await.clone();
        let subject = format!("{}.messages.{}.{event_type}", self.prefix, self.actor);

        let mut obj = match data {
            Value::Object(map) => map.clone(),
            _ => return,
        };

        obj.insert("event_type".to_owned(), Value::String(event_type.to_owned()));
        obj.insert("session_id".to_owned(), Value::String(session_id));
        obj.insert("ts".to_owned(), Value::Number(epoch_ms().into()));
        // Always include actor so consumers can resolve the agent name.
        obj.entry("actor".to_owned()).or_insert_with(|| Value::String(self.actor.clone()));

        // Inject session metadata (agent type, labels, k8s).
        if let Value::Object(ref meta) = self.metadata {
            for (k, v) in meta {
                obj.entry(k.clone()).or_insert_with(|| v.clone());
            }
        }

        let payload = match serde_json::to_vec(&obj) {
            Ok(p) => p,
            Err(e) => {
                tracing::warn!("nats-forward: serialize failed for {subject}: {e}");
                return;
            }
        };
        // Final guard: the text budget already reserves envelope headroom, but
        // drop (don't publish) any message that would still exceed the server's
        // max_payload so we never hand the server a payload it will reject.
        if payload.len() > self.max_payload {
            tracing::warn!(
                "nats-forward: dropping {subject} — serialized {} bytes exceeds max_payload {}",
                payload.len(),
                self.max_payload
            );
            return;
        }
        if let Err(e) = self.client.publish(subject.clone(), payload.into()).await {
            tracing::warn!("nats-forward: publish to {subject} failed: {e}");
        }
    }
}

// ── Classification helpers ──────────────────────────────────────────────

/// Extract concatenated text content from an assistant message.
fn extract_text(json: &Value) -> Option<String> {
    if json.get("type").and_then(|v| v.as_str()) != Some("assistant") {
        return None;
    }
    let content = json.get("message")?.get("content")?.as_array()?;
    let texts: Vec<&str> = content
        .iter()
        .filter(|b| b.get("type").and_then(|v| v.as_str()) == Some("text"))
        .filter_map(|b| b.get("text").and_then(|v| v.as_str()))
        .collect();
    if texts.is_empty() {
        return None;
    }
    let joined = texts.join("\n");
    let trimmed = joined.trim();
    if trimmed.is_empty() {
        return None;
    }
    Some(trimmed.to_owned())
}

/// Extract concatenated thinking/reasoning content from an assistant message.
fn extract_thinking(json: &Value) -> Option<String> {
    if json.get("type").and_then(|v| v.as_str()) != Some("assistant") {
        return None;
    }
    let content = json.get("message")?.get("content")?.as_array()?;
    let thinking: Vec<&str> = content
        .iter()
        .filter(|b| b.get("type").and_then(|v| v.as_str()) == Some("thinking"))
        .filter_map(|b| b.get("thinking").and_then(|v| v.as_str()))
        .collect();
    if thinking.is_empty() {
        return None;
    }
    let joined = thinking.join("\n");
    let trimmed = joined.trim();
    if trimmed.is_empty() {
        return None;
    }
    Some(trimmed.to_owned())
}

/// One `tool_use` block extracted from an assistant message, flattened to the
/// two fields the transcript tool-turn contract needs: the tool `name` and a
/// compact args summary (the same [`extract_tool_label`] label the status bar
/// and Slack tool-cause render). `args_summary` is `None` when the tool exposes
/// no summarizable input (matches `extract_tool_label` returning `None`).
struct ToolUse {
    name: String,
    args_summary: Option<String>,
}

/// Extract every `tool_use` block from an assistant message, in emission order.
///
/// Returns an empty vec for a non-assistant entry or an assistant turn with no
/// tool_use blocks. `AskUserQuestion` is intentionally skipped: it is an
/// interactive prompt (it drives the Prompt state, not a tool cause), not a
/// tool call the transcript paints — mirroring [`parse_claude_state`]'s special
/// case. Each surviving block is summarised via [`extract_tool_label`] so the
/// forwarded args summary is byte-for-byte what the status bar / Slack
/// tool-cause show.
fn extract_tool_uses(json: &Value) -> Vec<ToolUse> {
    if json.get("type").and_then(|v| v.as_str()) != Some("assistant") {
        return Vec::new();
    }
    let Some(content) =
        json.get("message").and_then(|m| m.get("content")).and_then(|c| c.as_array())
    else {
        return Vec::new();
    };
    content
        .iter()
        .filter(|b| b.get("type").and_then(|v| v.as_str()) == Some("tool_use"))
        .filter_map(|b| {
            let name = b.get("name").and_then(|v| v.as_str()).filter(|s| !s.is_empty())?;
            // Skip interactive prompts — not a transcript tool call.
            if name == "AskUserQuestion" {
                return None;
            }
            Some(ToolUse {
                name: name.to_owned(),
                args_summary: extract_tool_label(name, b.get("input")),
            })
        })
        .collect()
}

/// Extract usage data from a log entry.
fn extract_usage(json: &Value) -> Option<MessageKind> {
    let usage = json.get("usage").or_else(|| json.get("message").and_then(|m| m.get("usage")))?;

    let input = usage.get("input_tokens").and_then(|v| v.as_u64()).unwrap_or(0);
    let output = usage.get("output_tokens").and_then(|v| v.as_u64()).unwrap_or(0);

    if input == 0 && output == 0 {
        return None;
    }

    // Model id served this turn. Claude Code writes it at `message.model`;
    // also accept a top-level `model` for non-nested log shapes.
    let model = json
        .get("message")
        .and_then(|m| m.get("model"))
        .or_else(|| json.get("model"))
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_owned();

    Some(MessageKind::Usage {
        input_tokens: input,
        output_tokens: output,
        cache_read: usage.get("cache_read_input_tokens").and_then(|v| v.as_u64()).unwrap_or(0),
        cache_write: usage.get("cache_creation_input_tokens").and_then(|v| v.as_u64()).unwrap_or(0),
        duration_ms: json
            .get("durationMs")
            .or_else(|| usage.get("durationMs"))
            .and_then(|v| v.as_u64())
            .unwrap_or(0),
        model,
    })
}

/// Extract a refusal signal from a log entry: an assistant message whose
/// `stop_reason` is Anthropic's `"refusal"` value (the model declined to
/// answer). Returns `None` for any other stop reason (`end_turn`, `tool_use`,
/// `max_tokens`, `stop_sequence`, …) or a non-assistant entry.
///
/// Claude Code writes `stop_reason` on the nested `message` object; a top-level
/// `stop_reason` is also accepted for non-nested log shapes. The served model is
/// read from `message.model` (or a top-level `model`), mirroring
/// [`extract_usage`], and the refusal text (if any) is pulled via
/// [`extract_text`].
fn extract_refusal(json: &Value) -> Option<MessageKind> {
    // Only assistant messages carry a stop_reason we care about. Guard on it so a
    // user/tool entry that happens to include the word never trips detection.
    if json.get("type").and_then(|v| v.as_str()) != Some("assistant") {
        return None;
    }

    let stop_reason = json
        .get("message")
        .and_then(|m| m.get("stop_reason"))
        .or_else(|| json.get("stop_reason"))
        .and_then(|v| v.as_str())?;

    if !is_refusal_stop_reason(stop_reason) {
        return None;
    }

    let model = json
        .get("message")
        .and_then(|m| m.get("model"))
        .or_else(|| json.get("model"))
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_owned();

    // A refusal often carries no text block; extract_text returns None then, and
    // we forward an empty snippet (the stop_reason alone is the signal).
    let text = extract_text(json).unwrap_or_default();

    Some(MessageKind::Refusal { model, text })
}

/// Truncate a string to at most `max` bytes, appending "..." if truncated.
/// Avoids splitting multi-byte UTF-8 characters.
fn truncate_str(s: &str, max: usize) -> String {
    if s.len() <= max {
        return s.to_owned();
    }
    // Find the last valid char boundary at or before max.
    let mut end = max;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    let mut result = s[..end].to_owned();
    result.push_str("...");
    result
}

/// Return current epoch millis.
fn epoch_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

#[cfg(test)]
#[path = "nats_forward_tests.rs"]
mod tests;
