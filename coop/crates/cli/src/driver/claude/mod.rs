// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

pub mod encoding;
pub mod hooks;
pub mod parse;
pub mod prompt;
pub mod resume;
pub mod retire;
pub mod screen;
pub mod setup;
pub mod stream;

use std::path::{Path, PathBuf};

use crate::config::Config;

use super::hook_recv::HookReceiver;
use super::{Detector, DetectorSinks};
use encoding::{ClaudeNudgeEncoder, ClaudeRespondEncoder};
use stream::LogDetector;

/// Claude Code agent driver.
///
/// Provides encoding for nudge/respond actions and detection tiers
/// for monitoring Claude's agent state.
pub struct ClaudeDriver {
    pub nudge: ClaudeNudgeEncoder,
    pub respond: ClaudeRespondEncoder,
    pub detectors: Vec<Box<dyn Detector>>,
}

impl ClaudeDriver {
    /// Build a new driver from config and runtime paths.
    ///
    /// Constructs detectors based on available tiers:
    /// - Tier 1 (HookDetector): if `hook_pipe_path` is set
    /// - Tier 2 (LogDetector): if `session_log_path` is set
    /// - Tier 3 (StdoutDetector): if `sinks.stdout_rx` is provided
    pub fn new(
        config: &Config,
        hook_pipe_path: Option<&Path>,
        session_log_path: Option<PathBuf>,
        log_start_offset: u64,
        working_dir: Option<PathBuf>,
        sinks: DetectorSinks,
    ) -> anyhow::Result<Self> {
        let DetectorSinks {
            last_message,
            raw_hook_tx,
            raw_message_tx,
            stdout_rx,
            usage,
            resolved_log_path,
        } = sinks;
        let mut detectors: Vec<Box<dyn Detector>> = Vec::new();

        // Subscribed now, before the backend spawns Claude, so Tier 2 sees
        // the first hook (SessionStart) that names this Claude's transcript.
        let hook_rx = match (hook_pipe_path, &raw_hook_tx) {
            (Some(_), Some(tx)) => Some(tx.subscribe()),
            _ => None,
        };

        // Tier 1: Hook events (highest confidence). Shares the usage sink with
        // Tier 2 so it can record tool-call latency (ToolBefore→ToolAfter)
        // while Tier 2 records LLM-request latency from the session log.
        if let Some(pipe_path) = hook_pipe_path {
            let receiver = HookReceiver::new(pipe_path)?;
            detectors.push(Box::new(stream::new_hook_detector(
                receiver,
                raw_hook_tx,
                usage.clone(),
            )));
        }

        // Tier 2: Session log watching
        if let Some(log_path) = session_log_path {
            detectors.push(Box::new(LogDetector {
                log_path,
                start_offset: log_start_offset,
                poll_interval: config.log_poll(),
                last_message: last_message.clone(),
                raw_message_tx: raw_message_tx.clone(),
                usage,
                // Captured here, before the backend spawns Claude, so every
                // log already on disk belongs to an earlier incarnation.
                log_baseline: working_dir.map(|wd| resume::SessionLogBaseline::capture(&wd)),
                hook_rx,
                resolved_log_path,
            }));
        }

        // Tier 3: Structured stdout JSONL
        if let Some(stdout_rx) = stdout_rx {
            detectors.push(Box::new(stream::new_stdout_detector(
                stdout_rx,
                last_message,
                raw_message_tx,
            )));
        }

        // Sort by tier (lowest number = highest priority)
        detectors.sort_by_key(|d| d.tier());

        Ok(Self {
            nudge: ClaudeNudgeEncoder {
                input_delay: config.input_delay(),
                input_delay_per_byte: config.input_delay_per_byte(),
            },
            respond: ClaudeRespondEncoder { input_delay: config.input_delay() },
            detectors,
        })
    }
}
