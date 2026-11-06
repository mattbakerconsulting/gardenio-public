// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Generic hook-based detector shared by all agent drivers.
//!
//! Each agent provides a mapping function from [`HookEvent`] to
//! `(AgentState, cause)` pairs; the select loop is identical.
//!
//! As a side effect, when a [`UsageState`] sink is wired in, the detector
//! also measures per-tool-call wall-clock latency by pairing each
//! `ToolBefore` with its matching `ToolAfter` and recording the elapsed
//! time into the `gardenio_agent_tool_call_latency_seconds` histogram.

use std::collections::{HashMap, VecDeque};
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Instant;

use tokio::sync::{broadcast, mpsc};
use tokio_util::sync::CancellationToken;

use crate::driver::hook_recv::HookReceiver;
use crate::driver::{AgentState, Detector, DetectorEmission, HookEvent};
use crate::event::RawHookEvent;
use crate::usage::UsageState;

/// Tier 1 detector that maps hook events to agent states via a
/// caller-supplied closure.
pub struct HookDetector<F>
where
    F: Fn(HookEvent) -> Option<(AgentState, String)> + Send + 'static,
{
    pub receiver: HookReceiver,
    pub map_event: F,
    /// Optional sender for raw hook JSON broadcast.
    pub raw_hook_tx: Option<broadcast::Sender<RawHookEvent>>,
    /// Optional usage state for recording tool-call latency.
    pub usage: Option<Arc<UsageState>>,
}

/// Pairs `ToolBefore` start times with their matching `ToolAfter` to derive
/// per-tool-call latency.
///
/// Claude tags every tool call with a `tool_use_id` that is unique within the
/// session, so when both events carry one, pairing keys on that id — exact
/// regardless of how many same-named calls are in flight at once. When a
/// `tool_use_id` is absent (older agents, or a hook line that didn't carry
/// one), falls back to a per-name FIFO queue matched oldest-first — a
/// reasonable approximation, but one that mis-pairs overlapping same-named
/// calls (37% of PreToolUse events in fleet traffic, gd-Kl42VWWRd6).
#[derive(Default)]
struct ToolTimer {
    by_id: HashMap<String, Instant>,
    pending: HashMap<String, VecDeque<Instant>>,
}

impl ToolTimer {
    /// Record the start of a tool call.
    fn start(&mut self, tool: &str, tool_use_id: Option<&str>, now: Instant) {
        match tool_use_id {
            Some(id) => {
                self.by_id.insert(id.to_owned(), now);
            }
            None => {
                self.pending.entry(tool.to_owned()).or_default().push_back(now);
            }
        }
    }

    /// Pop the matching start time and return the elapsed seconds. Returns
    /// `None` if no matching start was recorded (e.g. a `ToolAfter` arriving
    /// after the detector started mid-session).
    fn finish(&mut self, tool: &str, tool_use_id: Option<&str>, now: Instant) -> Option<f64> {
        let start = match tool_use_id {
            Some(id) => self.by_id.remove(id),
            None => self.pending.get_mut(tool).and_then(VecDeque::pop_front),
        }?;
        Some(now.saturating_duration_since(start).as_secs_f64())
    }
}

impl<F> Detector for HookDetector<F>
where
    F: Fn(HookEvent) -> Option<(AgentState, String)> + Send + 'static,
{
    fn run(
        self: Box<Self>,
        state_tx: mpsc::Sender<DetectorEmission>,
        shutdown: CancellationToken,
    ) -> Pin<Box<dyn Future<Output = ()> + Send>> {
        Box::pin(async move {
            let mut receiver = self.receiver;
            let map_event = self.map_event;
            let raw_hook_tx = self.raw_hook_tx;
            let usage = self.usage;
            let mut tool_timer = ToolTimer::default();
            loop {
                tokio::select! {
                    _ = shutdown.cancelled() => break,
                    event = receiver.next_event() => {
                        match event {
                            Some((hook_event, raw_json)) => {
                                if let Some(ref tx) = raw_hook_tx {
                                    let _ = tx.send(RawHookEvent { json: raw_json });
                                }
                                // Side-channel: measure tool-call latency by
                                // pairing ToolBefore→ToolAfter, before the event
                                // is consumed by map_event below.
                                if usage.is_some() {
                                    match &hook_event {
                                        HookEvent::ToolBefore { tool, tool_use_id, .. } => {
                                            tool_timer.start(
                                                tool,
                                                tool_use_id.as_deref(),
                                                Instant::now(),
                                            );
                                        }
                                        HookEvent::ToolAfter {
                                            tool,
                                            tool_use_id,
                                            duration_ms,
                                            ..
                                        } => {
                                            // Always finish() to clear the pending
                                            // start, but prefer Claude's own
                                            // duration_ms when present — it excludes
                                            // coop's hook-dispatch overhead.
                                            let elapsed = tool_timer.finish(
                                                tool,
                                                tool_use_id.as_deref(),
                                                Instant::now(),
                                            );
                                            let secs = duration_ms
                                                .map(|ms| ms as f64 / 1000.0)
                                                .or(elapsed);
                                            if let Some(secs) = secs {
                                                if let Some(ref u) = usage {
                                                    u.observe_tool_call(secs).await;
                                                }
                                            }
                                        }
                                        _ => {}
                                    }
                                }
                                if let Some((state, cause)) = map_event(hook_event) {
                                    let _ = state_tx.send((state, cause, None)).await;
                                }
                            }
                            None => break,
                        }
                    }
                }
            }
        })
    }

    fn tier(&self) -> u8 {
        1
    }
}

#[cfg(test)]
#[path = "hook_detect_tests.rs"]
mod tests;
