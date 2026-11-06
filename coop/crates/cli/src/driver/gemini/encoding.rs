// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::time::Duration;

use crate::driver::{NudgeStep, QuestionAnswer, RespondEncoder};

pub use crate::driver::nudge::SafeNudgeEncoder as GeminiNudgeEncoder;

/// Encodes prompt responses for Gemini CLI's terminal input.
pub struct GeminiRespondEncoder {
    /// Delay between keystrokes in multi-step sequences.
    pub input_delay: Duration,
}

impl Default for GeminiRespondEncoder {
    fn default() -> Self {
        Self { input_delay: Duration::from_millis(200) }
    }
}

impl RespondEncoder for GeminiRespondEncoder {
    fn encode_permission(&self, option: u32) -> Vec<NudgeStep> {
        // Gemini permission: option 1 = accept ("1\r"), anything else = Escape to dismiss.
        let bytes = if option == 1 { b"1\r".to_vec() } else { b"\x1b".to_vec() };
        vec![NudgeStep { bytes, delay_after: None }]
    }

    fn encode_plan(&self, option: u32, feedback: Option<&str>) -> Vec<NudgeStep> {
        // Gemini plan: options 1-3 accept ("y\r"), option 4 rejects ("n\r") + feedback.
        if option <= 3 {
            return vec![NudgeStep { bytes: b"y\r".to_vec(), delay_after: None }];
        }

        let mut steps = vec![NudgeStep {
            bytes: b"n\r".to_vec(),
            delay_after: feedback.map(|_| self.input_delay),
        }];

        if let Some(text) = feedback {
            steps.push(NudgeStep { bytes: format!("{text}\r").into_bytes(), delay_after: None });
        }

        steps
    }

    fn encode_question(
        &self,
        answers: &[QuestionAnswer],
        _total_questions: usize,
    ) -> Vec<NudgeStep> {
        // Gemini uses simple single-question prompts; take the first answer.
        let answer = match answers.first() {
            Some(a) => a,
            None => return vec![],
        };

        if let Some(n) = answer.option {
            return vec![NudgeStep { bytes: format!("{n}\r").into_bytes(), delay_after: None }];
        }

        if let Some(ref text) = answer.text {
            return vec![NudgeStep { bytes: format!("{text}\r").into_bytes(), delay_after: None }];
        }

        vec![]
    }

    fn encode_setup(&self, option: u32) -> Vec<NudgeStep> {
        vec![
            NudgeStep {
                bytes: format!("{option}").into_bytes(),
                delay_after: Some(self.input_delay),
            },
            NudgeStep { bytes: b"\r".to_vec(), delay_after: None },
        ]
    }
}

#[cfg(test)]
#[path = "encoding_tests.rs"]
mod tests;
