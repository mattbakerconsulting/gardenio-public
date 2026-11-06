// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

pub mod acp;
pub mod detect;
pub mod encoding;
pub mod hooks;
pub mod parse;
pub mod screen;
pub mod session;
pub mod setup;

use std::path::Path;

use crate::config::Config;

use super::hook_recv::HookReceiver;
use super::{Detector, DetectorSinks};
use encoding::{GeminiNudgeEncoder, GeminiRespondEncoder};

/// How coop drives the Gemini process (ADR 0005 §4, §11).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GeminiMode {
    /// `gemini --acp`: long-lived JSON-RPC over stdio, no terminal UI
    /// ([`crate::backend::gemini::GeminiAcpBackend`]).
    Acp,
    /// Anything else (the interactive TUI, or a headless `-p` run): a plain PTY.
    Tui,
}

impl GeminiMode {
    /// Derive the mode from the agent command line (`command[0]` is the binary).
    /// `--experimental-acp` is the pre-0.64 spelling of `--acp`.
    pub fn from_command(command: &[String]) -> Self {
        let acp = command.iter().skip(1).any(|a| a == "--acp" || a == "--experimental-acp");
        if acp {
            Self::Acp
        } else {
            Self::Tui
        }
    }

    /// True for the mode with no terminal UI, whose backend tees raw JSON to a
    /// Tier 3 detector.
    pub fn is_headless(self) -> bool {
        self == Self::Acp
    }
}

/// Gemini CLI agent driver.
///
/// Provides encoding for nudge/respond actions and detection tiers
/// for monitoring Gemini's agent state.
pub struct GeminiDriver {
    pub nudge: GeminiNudgeEncoder,
    pub respond: GeminiRespondEncoder,
    pub detectors: Vec<Box<dyn Detector>>,
}

impl GeminiDriver {
    /// Build a new driver from config and runtime paths.
    ///
    /// Constructs detectors based on available tiers:
    /// - Tier 1 (HookDetector): if `hook_pipe_path` is set
    /// - Tier 3 (StdoutDetector): if `sinks.stdout_rx` is provided; it carries the
    ///   last assistant message and per-turn usage
    pub fn new(
        config: &Config,
        mode: GeminiMode,
        hook_pipe_path: Option<&Path>,
        sinks: DetectorSinks,
    ) -> anyhow::Result<Self> {
        let DetectorSinks { last_message, raw_hook_tx, raw_message_tx, stdout_rx, usage, .. } =
            sinks;
        let mut detectors: Vec<Box<dyn Detector>> = Vec::new();

        // Tier 1: Hook events (highest confidence). Shares the usage sink so it
        // can record tool-call latency (ToolBefore→ToolAfter).
        if let Some(pipe_path) = hook_pipe_path {
            let receiver = HookReceiver::new(pipe_path)?;
            detectors.push(Box::new(detect::new_hook_detector(
                receiver,
                raw_hook_tx,
                usage.clone(),
            )));
        }

        // Tier 3: Structured stdout JSONL
        if let Some(stdout_rx) = stdout_rx {
            detectors.push(Box::new(detect::new_stdout_detector(
                stdout_rx,
                last_message,
                raw_message_tx,
                usage,
            )));
        }

        // Sort by tier (lowest number = highest priority)
        detectors.sort_by_key(|d| d.tier());

        // The ACP backend reads input through a line discipline, so a nudge is
        // `text\r` with no typing delay; the TUI needs type-wait-Enter.
        let (input_delay, input_delay_per_byte) = match mode {
            GeminiMode::Acp => (std::time::Duration::ZERO, std::time::Duration::ZERO),
            GeminiMode::Tui => (config.input_delay(), config.input_delay_per_byte()),
        };
        Ok(Self {
            nudge: GeminiNudgeEncoder { input_delay, input_delay_per_byte },
            respond: GeminiRespondEncoder { input_delay: config.input_delay() },
            detectors,
        })
    }
}

#[cfg(test)]
#[path = "mod_tests.rs"]
mod tests;
