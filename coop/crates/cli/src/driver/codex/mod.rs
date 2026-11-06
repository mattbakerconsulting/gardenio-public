// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! OpenAI Codex agent driver (ADR 0004, `docs/adr/0004-codex-harness.md`).
//!
//! coop drives Codex without a terminal UI. How depends on the command:
//!
//! | Command            | [`CodexMode`] | Backend                                   |
//! |--------------------|---------------|-------------------------------------------|
//! | `codex app-server` | `AppServer`   | [`crate::backend::codex::CodexAppServerBackend`] (JSON-RPC, primary) |
//! | `codex exec ...`   | `Exec`        | [`crate::backend::codex::CodexExecBackend`] (one process per turn)   |
//! | anything else      | `Tui`         | ordinary PTY, typed input, hooks only     |
//!
//! State detection: Tier 1 Codex hooks when `$CODEX_HOME/hooks.json` carries
//! coop's hook config (see [`hooks`]), Tier 3 protocol/JSONL parsing for the
//! two PTY-less modes ([`parse`]), Tier 4 process monitor for `Tui`.

pub mod auth;
pub mod detect;
pub mod encoding;
pub mod hooks;
pub mod parse;
pub mod rollout;
pub mod rpc;
pub mod setup;

use std::path::Path;

use crate::config::Config;

use super::hook_recv::HookReceiver;
use super::{Detector, DetectorSinks};
use encoding::{CodexNudgeEncoder, CodexRespondEncoder};

/// How coop drives the Codex process.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CodexMode {
    /// `codex app-server`: long-lived JSON-RPC over stdio.
    AppServer,
    /// `codex exec --json`: one process per turn, resumed by thread id.
    Exec,
    /// Anything else (the interactive TUI): a plain PTY.
    Tui,
}

/// Flags whose following argument is a value, not a subcommand.
const VALUE_FLAGS: &[&str] =
    &["-c", "--config", "--enable", "--disable", "-m", "--model", "-p", "--profile", "-C", "--cd"];

impl CodexMode {
    /// Derive the mode from the agent command line (`command[0]` is the binary).
    pub fn from_command(command: &[String]) -> Self {
        let mut args = command.iter().skip(1);
        while let Some(arg) = args.next() {
            if VALUE_FLAGS.contains(&arg.as_str()) {
                args.next();
                continue;
            }
            if arg.starts_with('-') {
                continue;
            }
            return match arg.as_str() {
                "app-server" => Self::AppServer,
                "exec" | "e" => Self::Exec,
                _ => Self::Tui,
            };
        }
        Self::Tui
    }

    /// True for the modes with no terminal UI, whose backend tees raw JSON to a
    /// Tier 3 detector.
    pub fn is_headless(self) -> bool {
        matches!(self, Self::AppServer | Self::Exec)
    }
}

/// Codex agent driver: encoders plus the detector tiers available for `mode`.
pub struct CodexDriver {
    pub nudge: CodexNudgeEncoder,
    pub respond: CodexRespondEncoder,
    pub detectors: Vec<Box<dyn Detector>>,
}

impl CodexDriver {
    /// Build a driver from config and runtime paths.
    ///
    /// - Tier 1 (`HookDetector`): if `hook_pipe_path` is set
    /// - Tier 3 (`StdoutDetector`): if `sinks.stdout_rx` is provided
    pub fn new(
        config: &Config,
        mode: CodexMode,
        hook_pipe_path: Option<&Path>,
        sinks: DetectorSinks,
    ) -> anyhow::Result<Self> {
        let DetectorSinks { last_message, raw_hook_tx, raw_message_tx, stdout_rx, usage, .. } =
            sinks;
        let mut detectors: Vec<Box<dyn Detector>> = Vec::new();

        if let Some(pipe_path) = hook_pipe_path {
            let receiver = HookReceiver::new(pipe_path)?;
            detectors.push(Box::new(detect::new_hook_detector(
                receiver,
                raw_hook_tx,
                usage.clone(),
            )));
        }

        if let Some(stdout_rx) = stdout_rx {
            detectors.push(Box::new(detect::new_stdout_detector(
                stdout_rx,
                last_message,
                raw_message_tx,
                usage,
            )));
        }

        detectors.sort_by_key(|d| d.tier());

        Ok(Self {
            nudge: CodexNudgeEncoder::for_mode(
                mode,
                config.input_delay(),
                config.input_delay_per_byte(),
            ),
            respond: CodexRespondEncoder,
            detectors,
        })
    }
}

#[cfg(test)]
#[path = "mod_tests.rs"]
mod tests;
