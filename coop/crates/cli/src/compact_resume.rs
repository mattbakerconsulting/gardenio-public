// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Post-compaction resume nudge.
//!
//! When Claude Code compacts its context window it finishes the compaction and
//! returns to an EMPTY prompt: there is no pending user turn left, so the agent
//! lands in [`AgentState::Idle`] and sits there. That is fine for an agent that
//! was already done, but it strands every agent that was mid-work — the exact
//! operator report behind ka-0000007525 ("agent went idle after compacting;
//! needs nudge"). It also strands our post-compaction context recovery: the
//! `PreCompact` hook enqueues `gb prime` output to `/tmp/inject-queue.jsonl`
//! and `drain-queue.sh` only drains it on the NEXT `PostToolUse` — an idle
//! agent never takes another tool call, so the prime block is never re-injected
//! either.
//!
//! Coop already observes the compaction: Claude Code fires the `SessionStart`
//! hook with `source=compact`, which is what triggers the transcript snapshot
//! (`transport::http::hooks`). This module hangs a one-shot resume nudge off
//! that same signal:
//!
//!   1. Arm on `SessionStart(source=compact)`, recording the current
//!      `bytes_written` as a baseline.
//!   2. Poll the agent state. Compaction itself renders as `Working`, so we
//!      wait for the agent to settle into `Idle` and STAY idle for `grace`
//!      (default 45s) — long enough that a compaction which auto-continues
//!      into the next turn never gets a spurious nudge.
//!   3. Deliver exactly one nudge, then disarm.
//!
//! It disarms without nudging when any of these happen first: any other input
//! reaches the PTY (an operator or `gb` already resumed the agent —
//! `bytes_written` moved off the baseline), the agent exits, the session shuts
//! down, or the window (default 10m) elapses.
//!
//! Knobs (env, read once per compaction via [`CompactResumeConfig::from_env`]):
//!
//! | Env | Default | Meaning |
//! |---|---|---|
//! | `COOP_COMPACT_RESUME` | `1` | `0`/`false`/`off` disables the resume nudge |
//! | `COOP_COMPACT_RESUME_GRACE_MS` | `45000` | idle dwell before nudging |
//! | `COOP_COMPACT_RESUME_WINDOW_MS` | `600000` | give up this long after compaction |
//! | `COOP_COMPACT_RESUME_POLL_MS` | `2000` | state poll interval |
//! | `COOP_COMPACT_RESUME_MESSAGE` | see [`DEFAULT_MESSAGE`] | nudge text |

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use tokio::time::Instant;

use crate::driver::AgentState;
use crate::transport::handler::handle_nudge;
use crate::transport::state::Store;

/// Default resume text typed into the agent after a compaction.
///
/// Single line on purpose — the nudge encoder types the message and presses
/// Enter, so an embedded newline would submit a half-written prompt. It also
/// must not start with `/` or `@`, which would open Claude Code's slash-command
/// or file-mention completion menu instead of being read as prose.
pub const DEFAULT_MESSAGE: &str = "Context was compacted and you went idle. Run `gb prime` to reload your workflow context, re-read your claimed bead, and continue the work you were doing before the compaction. If that work is genuinely finished, land it and close out (kd close / gb stop) rather than idling.";

/// Whether the resume watcher nudged, and if not, why it stood down.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ResumeOutcome {
    /// The watcher was never armed (`COOP_COMPACT_RESUME=0`).
    Disabled,
    /// A resume nudge was delivered.
    Nudged,
    /// `handle_nudge` refused or errored (agent left the nudgeable states).
    NudgeFailed,
    /// Other input reached the PTY first — something already resumed the agent.
    Interrupted,
    /// The agent exited before it settled into a nudgeable idle.
    Exited,
    /// The session shut down while waiting.
    ShuttingDown,
    /// The agent never sat idle long enough within the window.
    WindowElapsed,
}

/// Tunables for one post-compaction resume watch.
#[derive(Debug, Clone)]
pub struct CompactResumeConfig {
    /// Master switch. When `false`, [`arm`] is a no-op.
    pub enabled: bool,
    /// How long the agent must sit continuously idle before we nudge.
    pub grace: Duration,
    /// How long after the compaction we keep watching before giving up.
    pub window: Duration,
    /// Agent-state poll interval.
    pub poll: Duration,
    /// The message to type.
    pub message: String,
}

impl Default for CompactResumeConfig {
    fn default() -> Self {
        Self {
            enabled: true,
            grace: Duration::from_millis(45_000),
            window: Duration::from_millis(600_000),
            poll: Duration::from_millis(2_000),
            message: DEFAULT_MESSAGE.to_owned(),
        }
    }
}

impl CompactResumeConfig {
    /// Read the knobs from the environment, falling back to [`Default`] for
    /// anything unset or unparseable.
    pub fn from_env() -> Self {
        let d = Self::default();
        Self {
            enabled: env_bool("COOP_COMPACT_RESUME", d.enabled),
            grace: env_duration("COOP_COMPACT_RESUME_GRACE_MS", d.grace),
            window: env_duration("COOP_COMPACT_RESUME_WINDOW_MS", d.window),
            poll: env_duration("COOP_COMPACT_RESUME_POLL_MS", d.poll),
            message: std::env::var("COOP_COMPACT_RESUME_MESSAGE")
                .ok()
                .filter(|s| !s.trim().is_empty())
                .unwrap_or(d.message),
        }
    }
}

fn env_bool(key: &str, default: bool) -> bool {
    match std::env::var(key) {
        Ok(v) => !matches!(v.trim().to_ascii_lowercase().as_str(), "0" | "false" | "off" | "no"),
        Err(_) => default,
    }
}

fn env_duration(key: &str, default: Duration) -> Duration {
    std::env::var(key)
        .ok()
        .and_then(|v| v.trim().parse::<u64>().ok())
        .map(Duration::from_millis)
        .unwrap_or(default)
}

/// Arm the post-compaction resume watcher as a detached task.
///
/// Called from the `SessionStart` hook handler when `source=compact`. Returns
/// immediately; the watch runs in the background for at most `config.window`.
pub fn arm(store: Arc<Store>, config: CompactResumeConfig) {
    if !config.enabled {
        tracing::debug!("compact-resume: disabled, not arming");
        return;
    }
    tokio::spawn(async move {
        let outcome = watch(&store, &config).await;
        tracing::info!(?outcome, "compact-resume: watch finished");
    });
}

/// Run the resume watch to completion. Exposed (not private) so tests can drive
/// it deterministically instead of racing a detached task.
pub async fn watch(store: &Store, config: &CompactResumeConfig) -> ResumeOutcome {
    if !config.enabled {
        return ResumeOutcome::Disabled;
    }

    // Any PTY write from here on means somebody else already resumed the agent
    // (operator nudge, `gb` re-inject, raw keys) — stand down rather than
    // stacking a second prompt on top of theirs.
    let baseline_bytes = store.lifecycle.bytes_written.load(Ordering::Relaxed);
    let deadline = Instant::now() + config.window;
    let mut idle_since: Option<Instant> = None;

    loop {
        if store.lifecycle.bytes_written.load(Ordering::Relaxed) != baseline_bytes {
            return ResumeOutcome::Interrupted;
        }

        let agent = store.driver.agent_state.read().await;
        let is_exited = matches!(&*agent, AgentState::Exited { .. });
        let is_idle = matches!(&*agent, AgentState::Idle);
        drop(agent);

        if is_exited {
            return ResumeOutcome::Exited;
        }

        let now = Instant::now();
        if is_idle {
            let since = *idle_since.get_or_insert(now);
            if now.duration_since(since) >= config.grace {
                return deliver(store, config).await;
            }
        } else {
            // Compaction itself (and any turn that follows it) reads as
            // Working; only an UNBROKEN idle stretch counts toward the grace.
            idle_since = None;
        }

        if now >= deadline {
            return ResumeOutcome::WindowElapsed;
        }

        tokio::select! {
            _ = store.lifecycle.shutdown.cancelled() => return ResumeOutcome::ShuttingDown,
            _ = tokio::time::sleep(config.poll) => {}
        }
    }
}

async fn deliver(store: &Store, config: &CompactResumeConfig) -> ResumeOutcome {
    match handle_nudge(store, &config.message).await {
        Ok(outcome) if outcome.delivered => {
            tracing::info!(
                grace_ms = config.grace.as_millis() as u64,
                "compact-resume: agent idle after compaction, delivered resume nudge"
            );
            ResumeOutcome::Nudged
        }
        Ok(outcome) => {
            tracing::warn!(
                reason = outcome.reason.unwrap_or_default(),
                "compact-resume: nudge not delivered"
            );
            ResumeOutcome::NudgeFailed
        }
        Err(e) => {
            tracing::warn!(error = ?e, "compact-resume: nudge failed");
            ResumeOutcome::NudgeFailed
        }
    }
}

#[cfg(test)]
#[path = "compact_resume_tests.rs"]
mod tests;
