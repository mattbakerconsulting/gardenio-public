// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::time::Duration;

use crate::driver::{NudgeEncoder, NudgeStep, QuestionAnswer, RespondEncoder};

use super::CodexMode;

/// Encodes nudges for Codex.
///
/// The PTY-less backends read their input through a line discipline, so a
/// nudge is one atomic write of `text\r` with no typing delay. The TUI needs
/// the same type-wait-Enter sequence as every other terminal agent.
pub struct CodexNudgeEncoder {
    /// `Some((base, per_byte))` delay before Enter, for the TUI; `None` for
    /// the PTY-less backends.
    typed_delays: Option<(Duration, Duration)>,
}

impl CodexNudgeEncoder {
    pub fn for_mode(mode: CodexMode, input_delay: Duration, per_byte: Duration) -> Self {
        Self { typed_delays: (mode == CodexMode::Tui).then_some((input_delay, per_byte)) }
    }
}

impl NudgeEncoder for CodexNudgeEncoder {
    fn encode(&self, message: &str) -> Vec<NudgeStep> {
        match self.typed_delays {
            None => {
                let mut bytes = message.as_bytes().to_vec();
                bytes.push(b'\r');
                vec![NudgeStep { bytes, delay_after: None }]
            }
            Some((base, per_byte)) => {
                let delay = crate::driver::compute_nudge_delay(base, per_byte, message.len());
                vec![
                    NudgeStep { bytes: message.as_bytes().to_vec(), delay_after: Some(delay) },
                    NudgeStep { bytes: b"\r".to_vec(), delay_after: None },
                ]
            }
        }
    }
}

/// Codex prompts never reach the operator: sessions run with
/// `approval_policy=never`, and the backend declines any approval or user-input
/// request itself (ADR 0004 §3), surfacing it as an error state instead of a
/// prompt. There is therefore nothing to encode.
pub struct CodexRespondEncoder;

impl RespondEncoder for CodexRespondEncoder {
    fn encode_permission(&self, _option: u32) -> Vec<NudgeStep> {
        vec![]
    }

    fn encode_plan(&self, _option: u32, _feedback: Option<&str>) -> Vec<NudgeStep> {
        vec![]
    }

    fn encode_question(
        &self,
        _answers: &[QuestionAnswer],
        _total_questions: usize,
    ) -> Vec<NudgeStep> {
        vec![]
    }

    fn encode_setup(&self, _option: u32) -> Vec<NudgeStep> {
        vec![]
    }
}

#[cfg(test)]
#[path = "encoding_tests.rs"]
mod tests;
